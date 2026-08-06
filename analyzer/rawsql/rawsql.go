// Package rawsql flags raw SQL string literals (SPEC.md §17.2, §21: "no
// hand-written SQL").
//
// # Why this is not a forbidigo pattern
//
// SPEC.md §17.2 assigns this rule to forbidigo. forbidigo cannot carry it:
// its visitor handles only `*ast.SelectorExpr` and `*ast.Ident` and returns
// early for every other node, so a `*ast.BasicLit` is never matched against
// any pattern. A forbidigo pattern written for SQL text would sit in
// .golangci.yml matching nothing, which is worse than no rule — it reads as
// enforcement and enforces nothing.
//
// .golangci.yml still carries forbidigo patterns for the *identifiers* through
// which SQL reaches a database (`.Exec`, `.Query`, `.QueryRow`); the two rules
// are complementary. depguard is the third layer, and the strongest one: it
// keeps pgx and database/sql out of every package but `generated/` and
// `wasmpg/` in the first place.
//
// # What counts as SQL
//
// A string literal whose first non-blank word begins a SQL statement. Matching
// on the leading keyword rather than anywhere in the text is deliberate: an
// error message reading "failed to select the agent" is not SQL, and an
// analyzer that says it is gets switched off.
//
// This package has no runtime dependencies: it is loaded as a golangci-lint
// module plugin.
package rawsql

import (
	"go/ast"
	"go/token"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// sqlStatement matches the opening of a SQL statement. GRAPH_TABLE and CREATE
// PROPERTY GRAPH are in the list because SQL/PGQ is the query surface AgentIQ
// generates (SPEC.md §6.5) and is exactly the kind of query someone reaches
// for a string literal to write.
var sqlStatement = regexp.MustCompile(`(?is)^\s*(?:--[^\n]*\n\s*)*(` +
	`select\s|` +
	`insert\s+into\s|` +
	`update\s+[a-z_."][\w."]*\s+set\s|` +
	`delete\s+from\s|` +
	`with\s+[a-z_][\w]*\s+as\s*\(|` +
	`create\s+(?:or\s+replace\s+)?(?:unique\s+)?(?:table|index|view|schema|type|function|property\s+graph|materialized\s+view)\s|` +
	`alter\s+(?:table|schema|type)\s|` +
	`drop\s+(?:table|index|view|schema|type|function|property\s+graph)\s|` +
	`truncate\s+table\s|` +
	`graph_table\s*\(|` +
	`copy\s+[a-z_."][\w."]*\s+(?:from|to)\s` +
	`)`)

var Analyzer = &analysis.Analyzer{
	Name: "rawsql",
	Doc: "check for raw SQL string literals\n\n" +
		"Persistence goes through the generated client (SPEC.md §21: no hand-written SQL).\n" +
		"Scope this linter to the packages that are allowed no SQL by excluding generated/\n" +
		"in .golangci.yml, the same way the depguard rules do.",
	URL: "https://github.com/gaarutyunov/agentiq/blob/main/SPEC.md#172-depguard-rules",
	Run: run,
}

func run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			// Struct tags and import paths are string literals too. They are
			// not special-cased: anchoring the pattern at the start of the
			// statement is what keeps them — and error messages containing
			// the word "select" — out of the results.
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if !sqlStatement.MatchString(text) {
				return true
			}
			pass.Reportf(lit.Pos(), "raw SQL string literal: %q. Persistence goes through the generated "+
				"client, and DDL through the generated migrations — both are produced from the SDL in "+
				"schema/ by `go generate ./...` (SPEC.md §11, §21). Hand-written SQL here is not covered by "+
				"the generated-code currency check, so it drifts from the schema silently",
				summarize(text))
			return true
		})
	}
	return nil, nil
}

// summarize shortens a statement for the diagnostic: the first line, capped,
// is enough to identify which literal is meant without printing a page of SQL
// into the linter output.
func summarize(text string) string {
	s := strings.TrimSpace(text)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " ..."
	}
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}
