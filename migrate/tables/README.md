# `migrate/tables/` — a generated copy of `generated/migrations/`

Written by `go generate ./...` (see `tools/generate.go`), never by hand.

`//go:embed` cannot reference a parent directory and `migrate/` does not sit
above `generated/`, so gopgql's `CREATE TABLE` history for the seven
`agentiq.*` types of SPEC.md §7.1 is copied here to be embedded. Because the
copy is a generated artifact, SPEC.md §17.3's `go generate ./... && git diff
--exit-code` gate fails the moment it drifts from what gopgql wrote — a copy
nothing checks would be a fork, a copy the drift gate checks is a build step.

## It holds gopgql's `CREATE TABLE` history for the seven §7.1 types

`0001_agentiq_tables.sql` creates all seven. It was empty until gopgql v0.3.0
fixed gaarutyunov/gopgql#53 defect A — the mechanism is written up in
`generated/migrations/README.md`.

`migrate.Apply` embeds this tree and applies it before `migrate/graph/`, which
is the order the property graph needs: `generated/graph/0003` names these tables
and cannot be created before they exist.
