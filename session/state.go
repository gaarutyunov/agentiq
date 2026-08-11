package session

import (
	"strings"

	adksession "google.golang.org/adk/v2/session"
)

// The three scopes an `agentiq.state_delta` / `agentiq.session_state` row can
// carry (SPEC.md §6.4). There is deliberately no `temp` scope: `temp:` keys are
// not persisted at all, so a row carrying that scope could never be written and
// a constant for it would suggest otherwise.
const (
	ScopeApp     = "app"
	ScopeUser    = "user"
	ScopeSession = "session"
)

// ScopeOf projects an ADK state key onto its §6.4 scope and the key as stored.
//
// The prefix is stripped, because the scope column already carries it and
// storing `app:foo` under `scope = 'app'` would say the same thing twice — and
// then disagree with itself the first time one of the two is written wrong.
// [Key] is the inverse and restores the prefix, which is what makes the key
// come back byte-identical inside `EventActions.StateDelta`.
//
// persist is false for exactly one input: a `temp:` key. That is SPEC.md §7.2
// rule 4, the one intentional deviation from round-trip fidelity — the key is
// dropped on write and absent on read.
//
// The prefixes are ADK's own constants rather than string literals: they are
// part of ADK's contract with the agent author, and a copy here would keep
// compiling after ADK changed one.
func ScopeOf(key string) (scope, stored string, persist bool) {
	switch {
	case strings.HasPrefix(key, adksession.KeyPrefixTemp):
		return "", "", false
	case strings.HasPrefix(key, adksession.KeyPrefixApp):
		return ScopeApp, strings.TrimPrefix(key, adksession.KeyPrefixApp), true
	case strings.HasPrefix(key, adksession.KeyPrefixUser):
		return ScopeUser, strings.TrimPrefix(key, adksession.KeyPrefixUser), true
	default:
		return ScopeSession, key, true
	}
}

// Key is the inverse of [ScopeOf]: it rebuilds the ADK-visible state key from a
// stored scope and key.
//
// An unknown scope is returned unprefixed rather than rejected. The scope
// column is written by [ScopeOf] and constrained to three values, so an
// unknown one means the row was written by something else — and a session that
// silently loses a key is a better failure here than one that refuses to load
// at all, because the caller can still see every key that is present.
func Key(scope, stored string) string {
	switch scope {
	case ScopeApp:
		return adksession.KeyPrefixApp + stored
	case ScopeUser:
		return adksession.KeyPrefixUser + stored
	default:
		return stored
	}
}
