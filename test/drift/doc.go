// Package drift is the DBOS schema-drift check (SPEC.md §17.4).
//
// It introspects `dbos.*` in a running PostgreSQL 19 container and compares the
// column set against a checked-in fixture. A mismatch fails the build naming
// the pinned DBOS version, so the read-only projection in `schema/dbos.graphql`
// is updated deliberately rather than discovered in production.
//
// The check exists because DBOS owns these tables and adds columns to them:
// `attributes`, `is_debounced` and `delay_until_epoch_ms` are recent examples.
// SPEC.md §3.1 and §17.4 both describe DBOS as pre-1.0; this repository pins
// **v1.0.0**, so the fixture is cut against v1.0.0. Being 1.0 does not retire
// the check — the projection is generated from an SDL that was written by hand
// against one schema version, and nothing but this test notices when the two
// stop agreeing.
//
// The suite is behind `//go:build integration`; this file carries no tag so
// `go vet ./...` does not report the package as having no buildable files.
package drift
