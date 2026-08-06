# `migrate/tables/` — a generated copy of `generated/migrations/`

Written by `go generate ./...` (see `tools/generate.go`), never by hand.

`//go:embed` cannot reference a parent directory and `migrate/` does not sit
above `generated/`, so gopgql's `CREATE TABLE` history for the seven
`agentiq.*` types of SPEC.md §7.1 is copied here to be embedded. Because the
copy is a generated artifact, SPEC.md §17.3's `go generate ./... && git diff
--exit-code` gate fails the moment it drifts from what gopgql wrote — a copy
nothing checks would be a fork, a copy the drift gate checks is a build step.

## It holds no `.sql` today, and that is a known gopgql defect

`generated/migrations/` is empty: gopgql v0.2.2 skips the `CREATE TABLE` of any
table it owns that some `@relationship` also maps an edge onto, and exits 0
while doing it, which for SPEC.md §7.1's foreign-key edges means 0 of 7 tables.
The mechanism is written up in `generated/migrations/README.md` and filed as
**gaarutyunov/gopgql#53**.

Nothing here changes when that is fixed. `go generate ./...` fills this
directory and `migrate.Apply` picks the files up in order.

This file is also what makes the directory embeddable while it is empty of
`.sql`: `//go:embed` fails to compile on a pattern that matches nothing, and a
directory holding only `.gitkeep` matches nothing because embed skips names
beginning with a dot.
