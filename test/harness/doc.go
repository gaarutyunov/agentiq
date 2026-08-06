// Package harness is the shared fixture layer for AgentIQ's integration and
// browser suites (SPEC.md §14).
//
// It is deliberately not behind a build tag. The suites that use it are — they
// carry `//go:build integration` or `//go:build browser` so `go test ./...
// -short` runs none of them (SPEC.md §16) — but the fixtures themselves are
// compiled, vetted and gofmt-checked on every build. A fixture that only
// compiles under a tag is a fixture that rots under the tag nobody runs
// locally.
//
// # Why this package speaks SQL
//
// SPEC.md §17.2's depguard rules confine `github.com/jackc/pgx/v5` and
// `database/sql` to `generated/` and `wasmpg/`. This package needs both, and
// `.golangci.yml` exempts `test/` for three reasons that are not negotiable
// away:
//
//  1. SPEC.md §17.4 requires introspecting `dbos.*` and comparing the column
//     set against a fixture. The generated client reads what the SDL declares;
//     the drift check exists precisely to see what the *database* has, which no
//     generated read model can report.
//  2. The property graph (`generated/graph/`) is DDL somebody has to execute
//     before a `GRAPH_TABLE` query can answer. gopgql's only pool constructor
//     is [exec.OpenReadOnly], which sets `default_transaction_read_only=on` by
//     design, so the DDL cannot run through it.
//  3. testcontainers' Snapshot/Restore needs a registered `database/sql`
//     driver — see [RequireSQLDriver] for what happens when it does not have
//     one.
//
// Reads that the generated client *can* serve go through the generated client
// (see [Graph]), because SPEC.md §21's "no hand-written SQL" is about the read
// model, and a test that hand-wrote the GRAPH_TABLE query would not be testing
// the thing M1 ships.
package harness
