package wasmpg

import "strings"

// listenKind is what an observed statement asks the routing table to do.
type listenKind int

const (
	listenRegister listenKind = iota + 1
	listenUnregister
	listenUnregisterAll // UNLISTEN *
)

// listenOp is one observed LISTEN or UNLISTEN. Channel is the folded
// identifier — unquoted names are lower-cased the way PostgreSQL folds them,
// quoted names keep their case — so it compares equal to the channel PGlite
// reports through onNotification.
type listenOp struct {
	kind    listenKind
	channel string
}

// observeListen finds every LISTEN and UNLISTEN in a statement string.
//
// The multiplexer cannot ask the backend who is listening: `LISTEN` is
// session-global against a single PGlite session, so the backend's own view
// cannot distinguish the logical connections sharing it (SPEC.md §12.4).
// Watching the traffic go past is the only source of that mapping, which is
// why this is a parser rather than a query.
func observeListen(sql string) []listenOp {
	if sql == "" {
		return nil
	}
	// Cheap rejection first: the overwhelming majority of statements are
	// neither, and this runs on every Query and Parse.
	if !strings.Contains(strings.ToLower(sql), "listen") {
		return nil
	}

	var ops []listenOp
	for _, stmt := range splitStatements(sql) {
		if op, ok := parseListenOp(stmt); ok {
			ops = append(ops, op)
		}
	}
	return ops
}

// parseListenOp matches a single statement against LISTEN/UNLISTEN. It reports
// false for anything else, including a statement that merely mentions the word.
func parseListenOp(stmt string) (listenOp, bool) {
	s := skipLeading(stmt)

	var kind listenKind
	switch {
	case hasKeyword(s, "unlisten"):
		kind = listenUnregister
		s = s[len("unlisten"):]
	case hasKeyword(s, "listen"):
		kind = listenRegister
		s = s[len("listen"):]
	default:
		return listenOp{}, false
	}

	s = skipLeading(s)
	if strings.HasPrefix(s, "*") {
		if kind != listenUnregister {
			return listenOp{}, false // `LISTEN *` is not valid SQL
		}
		return listenOp{kind: listenUnregisterAll}, true
	}

	name, ok := parseIdentifier(s)
	if !ok {
		return listenOp{}, false
	}
	return listenOp{kind: kind, channel: name}, true
}

// hasKeyword reports whether s starts with kw, case-insensitively, followed by
// something that cannot continue an identifier. Without the boundary check,
// `LISTENER_STATE` would parse as a LISTEN.
func hasKeyword(s, kw string) bool {
	if len(s) < len(kw) || !strings.EqualFold(s[:len(kw)], kw) {
		return false
	}
	if len(s) == len(kw) {
		return true
	}
	return !isIdentByte(s[len(kw)])
}

// parseIdentifier reads a channel name: a double-quoted identifier keeps its
// case and collapses doubled quotes, an unquoted one is folded to lower case,
// exactly as the backend would.
func parseIdentifier(s string) (string, bool) {
	if strings.HasPrefix(s, `"`) {
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] != '"' {
				b.WriteByte(s[i])
				continue
			}
			if i+1 < len(s) && s[i+1] == '"' {
				b.WriteByte('"')
				i++
				continue
			}
			return b.String(), b.Len() > 0
		}
		return "", false // unterminated
	}

	end := 0
	for end < len(s) && isIdentByte(s[end]) {
		end++
	}
	if end == 0 {
		return "", false
	}
	return strings.ToLower(s[:end]), true
}

// isIdentByte covers the bytes PostgreSQL accepts in an unquoted identifier.
// Bytes above 0x7f are accepted wholesale: they can only be part of a
// multi-byte character, which the backend treats as a letter.
func isIdentByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_' || c == '$' || c >= 0x80:
		return true
	default:
		return false
	}
}

// skipLeading drops whitespace and comments from the front of s, so that a
// statement's first real token can be matched.
func skipLeading(s string) string {
	for {
		trimmed := strings.TrimLeft(s, " \t\r\n\f\v")
		switch {
		case strings.HasPrefix(trimmed, "--"):
			if i := strings.IndexByte(trimmed, '\n'); i >= 0 {
				s = trimmed[i+1:]
				continue
			}
			return ""
		case strings.HasPrefix(trimmed, "/*"):
			if i := strings.Index(trimmed[2:], "*/"); i >= 0 {
				s = trimmed[2+i+2:]
				continue
			}
			return ""
		default:
			return trimmed
		}
	}
}

// splitStatements cuts a query string on top-level semicolons.
//
// It has to skip string literals, quoted identifiers, dollar-quoted bodies and
// comments, because a semicolon inside any of them is not a statement
// boundary. Getting that wrong would let `SELECT 'x; LISTEN y'` register a
// channel nobody asked for, and a bogus registration means notifications
// delivered to a connection that is not expecting them.
func splitStatements(sql string) []string {
	var out []string
	start, i := 0, 0
	for i < len(sql) {
		switch sql[i] {
		case '\'', '"':
			i = skipQuoted(sql, i)
		case '$':
			i = skipDollarQuoted(sql, i)
		case '-':
			if strings.HasPrefix(sql[i:], "--") {
				if j := strings.IndexByte(sql[i:], '\n'); j >= 0 {
					i += j + 1
				} else {
					i = len(sql)
				}
			} else {
				i++
			}
		case '/':
			if strings.HasPrefix(sql[i:], "/*") {
				if j := strings.Index(sql[i+2:], "*/"); j >= 0 {
					i += 2 + j + 2
				} else {
					i = len(sql)
				}
			} else {
				i++
			}
		case ';':
			out = append(out, sql[start:i])
			i++
			start = i
		default:
			i++
		}
	}
	return append(out, sql[start:])
}

// skipQuoted advances past a single-quoted literal or double-quoted
// identifier starting at i, honouring the doubled-quote escape. Backslash
// escapes are not honoured because standard_conforming_strings is on.
func skipQuoted(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] != q {
			continue
		}
		if j+1 < len(s) && s[j+1] == q {
			j++
			continue
		}
		return j + 1
	}
	return len(s)
}

// skipDollarQuoted advances past a $tag$…$tag$ body starting at i. If i is not
// the start of a valid opening tag — a bare `$1` placeholder, say — it advances
// by one byte.
func skipDollarQuoted(s string, i int) int {
	// A tag is `$`, an optional identifier that may not start with a digit,
	// then `$`. Anything else — a `$1` placeholder, a lone `$` — is not one.
	end := i + 1
	if end < len(s) && s[end] >= '0' && s[end] <= '9' {
		return i + 1
	}
	// `$` itself cannot appear in the tag, or `$$a$$` would scan its own
	// closing delimiter as part of the opening one.
	for end < len(s) && s[end] != '$' && isIdentByte(s[end]) {
		end++
	}
	if end >= len(s) || s[end] != '$' {
		return i + 1
	}
	tag := s[i : end+1]
	if j := strings.Index(s[end+1:], tag); j >= 0 {
		return end + 1 + j + len(tag)
	}
	return len(s)
}
