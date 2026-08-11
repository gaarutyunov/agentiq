// Package session persists ADK sessions, events and parts into the
// AgentIQ-owned `agentiq.*` tables (SPEC.md §5, §8.3).
//
// It is the only implementation of ADK's `session.Service` in this module. It
// may import ADK, DBOS and `generated/client`; it must not import `workflow`,
// `artifact` or `proxy` (SPEC.md §5), and depguard enforces that. In
// particular it must not import `pgx`: every statement goes through the
// generated client, which is what keeps the SQL generated rather than written.
//
// # The package is split into a mapping half and an execution half
//
// The mapping half — [encodeEvent], [decodeEvent], [ScopeOf] and the row types
// — is pure. It turns a `session.Event` into the rows of the seven §7.1 tables
// and back, and it is where SPEC.md §7.2's conformance property actually
// lives:
//
//	for any session.Event E, json.Marshal(load(store(E))) == json.Marshal(E)
//
// That property is a statement about the *mapping*. The database's only job is
// to give back the values it was handed, which is exactly what §7.2 rule 1
// buys by insisting on `json` and not `jsonb`. So the corpus can be — and is —
// run without a database, and the same corpus runs against Postgres through
// the execution half.
//
// The execution half — [New] and the `session.Service` it returns — runs those
// rows through `generated/client` inside `dbos.RunAsTransaction`, so that the
// event rows and the step checkpoint commit atomically (D2). service.go is the
// ADK contract and load.go turns the generated result types back into the row
// types the pure decoder reads; there is deliberately no second decoder written
// against the generated shapes, because it would be a second opinion about the
// mapping the corpus test is pinning.
//
// # What is deliberately not stored
//
// Partial events (`Event.Partial == true`) are never written (D5, §9.4).
// `sessiontestsuite` asserts zero stored events for them, so this is a
// conformance requirement and not a preference.
//
// `temp:` state keys are dropped on write and absent on read (§6.4, §7.2
// rule 4). It is the one sanctioned deviation from round-trip fidelity and it
// has its own test rather than being left implicit.
package session
