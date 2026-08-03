# AgentIQ — Engineering Specification

**Status:** Implementation contract, v1.0
**Scope:** Complete. No architectural decision is deferred to implementation.

-----

## 1. Vision

AgentIQ is a **durable agentic workflow runtime**. It executes ADK agents as
DBOS workflows, so every model call, tool call, and human approval is a
checkpointed step that survives process crashes, worker restarts, and database
restarts.

### 1.1 Goals

- Agent executions are durable and replayable. A killed worker resumes from the
  last completed step, not from the beginning.
- Agent definitions are **immutable, content-addressed OCI artifacts**. An
  execution pins a digest, so replay reconstructs a byte-identical agent.
- All state — execution and domain — is queryable through **one GraphQL API**
  generated from SDL, backed by a PostgreSQL 19 property graph.
- The browser demo runs the **real runtime against real PostgreSQL**. No mocks,
  no fixtures, no simulated persistence.

### 1.2 Non-goals

- AgentIQ does not serve MCP. It is an MCP **consumer** (§6.7).
- AgentIQ does not implement agent memory. That is a sibling project (§1.4).
- AgentIQ does not schedule cron workflows or provide analytics dashboards.
- AgentIQ does not host MCP servers. The mcp-anything proxy does (§9.6).

### 1.3 Architectural principles

1. **Durability is not a feature; it is the execution model.** Every
   non-deterministic operation is a DBOS step. This is enforced by a custom
   analyzer, not by review (§17).
1. **The database is one database.** DBOS system tables, AgentIQ domain tables,
   and the property graph share one PostgreSQL instance so that a step
   checkpoint and a domain write commit in the same transaction.
1. **Definitions are artifacts; state is rows.** Anything authored is an OCI
   artifact pinned by digest. Anything observed is a row in Postgres.
1. **Composition is by reference.** An agent inlines nothing. Instructions,
   model configuration, skills, tools and sub-agents are all descriptors in an
   OCI image index.
1. **The browser runs production code.** The WASM build differs from the server
   build in one place only: the Postgres transport (§12).

### 1.4 Relationship to sibling projects

|Project            |Relationship                                                                                                                                                                             |
|-------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
|`gopgql`           |Generates the PostgreSQL 19 schema, property graph, and typed client from SDL. **Blocking dependency** for M1 (§3.2).                                                                    |
|`mcp-anything`     |Fronts all tool execution over Streamable HTTP. **Blocking dependency** for M4 (§9.6).                                                                                                   |
|`epos`             |Owns the **agent artifact format** and all packaging: `agent`, `instruction`, `model`, `tool` and `skill` kinds, the CLI, and the local OCI store. **Blocking dependency** for M3 (§3.2).|
|*(memory, unnamed)*|A Spectron-class provenance-first memory layer, built on AgentIQ’s event/part graph. AgentIQ exposes ADK’s `memory.Service` as a seam with a no-op default.                              |

-----

## 2. Architecture Overview

### 2.1 System responsibilities

```
┌──────────────────────────────────────────────────────────────────┐
│ AUTHORING                                                        │
│   agentiq CLI ──pack/push──▶ OCI registry                        │
│                              (agent index + referenced artifacts)│
└──────────────────────────────────────────────────────────────────┘
                                   │ pull by digest (oras-go)
                                   ▼
┌──────────────────────────────────────────────────────────────────┐
│ AGENTIQ RUNTIME (single Go module)                               │
│                                                                  │
│   ┌────────────┐   RunAsStep    ┌──────────────────────────┐     │
│   │  dbosadk   │───────────────▶│ OpenRouter (model call)  │     │
│   │ integration│   RunAsStep    ├──────────────────────────┤     │
│   │            │───────────────▶│ mcp-anything (tool call) │     │
│   │  ADK agent │   Recv         └──────────────────────────┘     │
│   │  loop runs │◀──── human approval (dbos.send_message)         │
│   │  inside a  │                                                 │
│   │  DBOS      │ RunAsTransaction ──▶ session/event/part tables  │
│   │  workflow  │ WriteStream      ──▶ dbos.streams               │
│   └────────────┘                                                 │
└──────────────────────────────────────────────────────────────────┘
                                   │
                                   ▼
┌──────────────────────────────────────────────────────────────────┐
│ POSTGRESQL 19 (one database)                                     │
│   dbos.*            — DBOS-owned, DBOS-migrated, read-only to us  │
│   agentiq.*         — AgentIQ-owned, goose-migrated via gopgql    │
│   PROPERTY GRAPH    — read-only view spanning both                │
└──────────────────────────────────────────────────────────────────┘
                                   ▲
                                   │ generated typed client
┌──────────────────────────────────────────────────────────────────┐
│ GRAPHQL API (generated from SDL by gopgql)                       │
│   Query        — graph reads                                     │
│   Mutation     — mapped to dbos PL/pgSQL functions               │
│   Subscription — reads dbos.streams (M7)                         │
└──────────────────────────────────────────────────────────────────┘
```

### 2.2 Ownership

|Subsystem                |Owns                                                        |Must not touch        |
|-------------------------|------------------------------------------------------------|----------------------|
|`dbos` (vendored library)|`dbos.*` schema, checkpoints, queues, streams, notifications|AgentIQ domain tables |
|`generated/`             |All SQL, all pgx usage on the domain side                   |Business logic        |
|`workflow/`              |Workflow registration, step boundaries, determinism         |Direct DB access      |
|`session/`               |ADK `SessionService` implementation                         |Workflow orchestration|
|`artifact/`              |OCI pull, index resolution, projection into rows            |Execution             |
|`wasmpg/`                |The browser Postgres transport                              |Everything else       |

### 2.3 Execution flow (single agent turn)

```
GraphQL mutation startAgentRun(agentDigest, input)
  │
  ├─▶ dbos.enqueue_workflow('AgentRun', queue, args...)  [SQL function]
  │      returns workflow_uuid
  ▼
DBOS queue worker picks up workflow
  │
  ├─ RunAsStep: resolve agent closure from digest (cached; idempotent)
  ├─ RunAsTransaction: create session row
  │
  └─ ADK Runner loop (deterministic workflow code):
       │
       ├─ RunAsStep ──▶ OpenRouter chat/completions
       │     └─ WriteStream(partials) ──▶ dbos.streams
       ├─ RunAsTransaction ──▶ append final event + parts + actions + state
       ├─ RunAsStep ──▶ mcp-anything POST /mcp (tool call)
       ├─ RunAsTransaction ──▶ append tool-response event
       │
       ├─ [if tool requires confirmation]
       │     RunAsTransaction ──▶ append event{interrupted, requested_input}
       │     dbos.Recv(topic="approval", 168h) ◀── dbos.send_message
       │
       └─ loop until turnComplete
  │
  └─ CloseStream; workflow_status → SUCCESS
```

### 2.4 Data flow

```
SDL (canonical)
  ├─ gopgql ─▶ goose migrations (agentiq.* tables)
  ├─ gopgql ─▶ CREATE PROPERTY GRAPH (spans dbos.* + agentiq.*)
  ├─ gopgql ─▶ typed Go client (Tx-scoped)
  └─ gopgql ─▶ GraphQL server + schema document
```

The SDL is the **domain contract**. It does not claim ownership of where
execution history physically lives; `dbos.*` is exposed through it as a
read-only projection.

-----

## 3. Technology Stack

### 3.1 Selected technologies

|Technology                             |Purpose                                                |Alternatives considered                               |Reason selected                                                                                                        |
|---------------------------------------|-------------------------------------------------------|------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------|
|Go (no cgo)                            |Runtime, CLI, WASM build                               |Rust, TypeScript                                      |Cross-compilation is a hard project constraint; ADK and DBOS both have Go SDKs                                         |
|DBOS Transact Go v0.17+                |Durable execution                                      |Temporal, Restate, Cadence, river, hand-rolled        |Library not platform; Postgres-only, no cluster; DataSources give exactly-once domain writes                           |
|ADK Go v2 (`google.golang.org/adk/v2`) |Agent loop, session model, tools                       |langchaingo, eino, genkit, native                     |Typed `session.Event` model, pluggable `SessionService`, `model/openaimodel` with `BaseURL`, official conformance suite|
|PostgreSQL 19                          |Storage + property graph                               |PG18 + Apache AGE, Neo4j                              |SQL/PGQ `GRAPH_TABLE`/`MATCH` in core; single engine for relational + graph                                            |
|gopgql                                 |SDL → schema, graph, client                            |gqlgen alone, sqlc, hand-written                      |Owns the SDL→SQL/PGQ compilation; already the ecosystem’s schema-first tool                                            |
|epos                                   |Agent artifact format, packing, resolution, local store|Bespoke format in AgentIQ, KitOps ModelKit, AGNTCY ADS|One packaging system across the ecosystem; already owns the skill kind, the store and the CLI                          |
|OpenRouter                             |Model gateway                                          |Direct provider APIs, self-hosted proxy               |Browser-usable via PKCE with no backend; one API for all models; OpenAI-compatible so `openaimodel` works unmodified   |
|mcp-anything                           |Tool execution                                         |agentgateway, MetaMCP, mcp-proxy                      |Already ours; js/lua/bash runtimes let script tools run without containers                                             |
|PGlite (PG19 fork)                     |Browser Postgres                                       |modernc.org/sqlite, remote Postgres                   |Only path to real Postgres semantics + SQL/PGQ in-browser. **Delivered and verified**; consumed as a pinned build      |
|Standard Go `js/wasm`                  |Browser build                                          |TinyGo                                                |pgx, DBOS, ADK and `encoding/json` are reflection-heavy; TinyGo cannot compile them                                    |
|`gaarutyunov/ui-kit`                   |Docs + demo frontend                                   |React component libraries                             |Zero-dependency Web Components, buildless, MIT, with a `./react` export                                                |
|godog                                  |Executable Gherkin                                     |testify only                                          |Feature files are canonical test sources; `go test` integration; JUnit output                                          |
|testcontainers-go                      |Integration infrastructure                             |docker-compose in CI                                  |Real `postgres:19beta2` and real `zot` per test run                                                                    |
|golangci-lint v2 + custom module plugin|Architecture enforcement                               |Review, go-arch-lint                                  |DBOS has no `workflowcheck` equivalent; determinism must be machine-checked                                            |

### 3.2 Blocking upstream dependencies

**gopgql** must ship before M1:

1. **`@function` mutation directive.** Maps a GraphQL mutation field to a
   PL/pgSQL function call with typed argument mapping. Required because
   `dbos.enqueue_workflow` and `dbos.send_message` are the command surface.
1. **Tx-scoped execution.** Generated client operations must accept a
   caller-supplied transaction handle so they run inside `RunAsTransaction`.
1. **Read-only exposure of an externally-owned schema.** gopgql must generate
   the property-graph definition and read model over `dbos.*` without emitting
   DDL for those tables.
1. **Rename hints.** `dbos.streams.offset` is a reserved word.

The **PGlite PG19 fork** is already delivered and verified. It is a consumed
artifact, not a prerequisite to build.

**epos** must ship before M3:

1. **New artifact kinds** — `agent`, `instruction`, `model`, `tool` — alongside
   the existing `skill` kind, under the `vnd.epos.*` namespace.
1. **Reference composition** as a first-class peer to `Skillfile` merge
   composition: an OCI image index whose `manifests[]` are annotated descriptors,
   with nothing inlined and no merging.
1. **A public Go API** (`pkg/`) exposing resolution and closure types. AgentIQ
   calls `Resolve` inside a DBOS step; shelling out to the CLI is not viable
   inside workflow code.
1. **Recursive resolution** with cycle detection by digest and a depth limit.

Epos remains a CLI, a local store and a proxy. It does not gain an API server,
a database or a web UI.

**mcp-anything** must ship before M4:

1. Resolve MCP server and script-tool descriptors from an agent index by digest.
1. Expose the resolved closure over Streamable HTTP with CORS.
1. Dispatch on `artifactType`: container image vs script artifact.

### 3.3 Verified constraints

- SQL/PGQ in PG19 supports fixed-depth pattern matching. **Variable-length
  paths are not supported.** Recursive sub-agent traversal uses a recursive CTE
  (§6.5).
- PostgreSQL 19 is in beta (Beta 1 June 2026, Beta 2 July 2026). GA is
  projected, not committed. CI pins `postgres:19beta2`.
- DBOS Go is pre-1.0. The pinned version is recorded in `go.mod` and a CI check
  detects `dbos.*` schema drift (§17.4).
- GitHub Pages cannot set COOP/COEP headers, so `SharedArrayBuffer` is
  unavailable and PGlite’s OPFS-with-SAB persistence mode cannot be used (§13.3).

-----

## 4. Repository Layout

Single Go module. Packages are grouped by concern. There is no hexagonal
structure, no `internal/domain`, and no layer directories.

```
agentiq/
├── go.mod                        module github.com/gaarutyunov/agentiq
├── SPEC.md                       this document
├── EPOS-REQUIREMENTS.md          artifact format requirements on epos (§7.6)
├── schema/
│   ├── agentiq.graphql           canonical SDL — the domain contract
│   └── dbos.graphql              read-only SDL over the dbos.* schema
├── generated/                    gopgql output. NEVER hand-edited.
│   ├── migrations/               goose migrations for agentiq.*
│   ├── graph/                    CREATE PROPERTY GRAPH DDL
│   ├── client/                   typed Tx-scoped Go client
│   └── gql/                      GraphQL server + schema document
├── workflow/                     DBOS workflow registration + step boundaries
├── dbosadk/                      the DBOS↔ADK integration (§8.2)
├── session/                      ADK SessionService over the generated client
├── artifact/                     epos client: resolve closure, project to rows
├── proxy/                        mcp-anything Streamable HTTP client
├── model/                        OpenRouter wiring + PKCE
├── wasmpg/                       browser Postgres transport (js/wasm only)
├── demo/                         ui-kit frontend + WASM entrypoint
│   ├── wasm/main.go              //go:build js && wasm
│   └── web/                      ui-kit web components, index.html
├── cmd/
│   └── agentiq/                  server binary
│                                 (packing and publishing is `epos`, not ours)
├── analyzer/                     custom go/analysis determinism analyzer
├── features/                     canonical Gherkin feature files
├── test/                         godog runners, testcontainers fixtures
├── .custom-gcl.yml               golangci-lint custom build descriptor
├── .golangci.yml                 linter config incl. depguard rules
├── Makefile                      make verify
└── .github/workflows/
    ├── ci.yml                    lint, generate-check, tests
    ├── deploy.yml                Pages branch deployment
    └── pr-preview.yml            PR preview deployment
```

### 4.1 Directory ownership

|Directory   |Owner          |Rule                                                                 |
|------------|---------------|---------------------------------------------------------------------|
|`schema/`   |Domain contract|Only source of truth for the data model                              |
|`generated/`|gopgql         |Regenerated; drift fails CI                                          |
|`workflow/` |Execution      |Only place `RegisterWorkflow` is called; determinism analyzer applies|
|`dbosadk/`  |Integration    |Only place ADK and DBOS types meet                                   |
|`session/`  |Persistence    |Only implementation of `session.Service`                             |
|`artifact/` |Supply chain   |Only place the epos API is imported                                  |
|`wasmpg/`   |Transport      |Only place `syscall/js` is imported                                  |
|`analyzer/` |Enforcement    |No runtime dependencies                                              |

-----

## 5. Package Structure

For every package: responsibility, public interface, allowed and forbidden
dependencies. Forbidden dependencies are enforced by `depguard` (§17.2).

### `workflow`

- **Responsibility.** Registers DBOS workflows; defines step boundaries; owns
  determinism.
- **Public.** `Register(ctx dbos.Context) error`, `AgentRun(ctx, AgentRunInput) (AgentRunOutput, error)`
- **May import.** `dbosadk`, `session`, `artifact`, `proxy`, `model`, `generated/client`
- **Must not import.** `pgx`, `database/sql`, `net/http`, `time` (for `Now`), `math/rand`, `os`

### `dbosadk`

- **Responsibility.** Wraps ADK abstractions as DBOS steps. Mirrors the design
  of Temporal’s `contrib/googleadk`.
- **Public.** `NewModel(inner model.LLM, opts ...StepOption) model.LLM`,
  `StepAsTool(t tool.Tool, opts ...StepOption) tool.Tool`,
  `NewPlugin(cfg Config) adk.Plugin`
- **May import.** ADK, DBOS
- **Must not import.** `generated/client`, `artifact`

### `session`

- **Responsibility.** Implements `session.Service` over the generated client,
  writing through `dbos.RunAsTransaction`.
- **Public.** `New(ds *dbos.DataSource) session.Service`
- **May import.** ADK, DBOS, `generated/client`
- **Must not import.** `workflow`, `artifact`, `proxy`

### `artifact`

- **Responsibility.** Resolves an agent closure through the epos API and
  projects it into `agentiq.*` rows. Resolution itself — pull, index walk, cycle
  detection, depth limiting — belongs to epos.
- **Public.** `Resolve(ctx, ref string) (epos.Closure, error)`,
  `Project(ctx, tx client.Tx, c epos.Closure) error`
- **May import.** `github.com/gaarutyunov/epos/pkg/...`, `generated/client`
- **Must not import.** `oras-go` directly, ADK, `workflow`

### `proxy`

- **Responsibility.** Streamable HTTP client for mcp-anything; converts MCP
  tools into ADK tools.
- **Public.** `Connect(ctx, endpoint string, closure artifact.Closure) ([]tool.Tool, error)`
- **May import.** MCP Go SDK, ADK, `net/http`
- **Must not import.** DBOS (wrapping as steps is `dbosadk`’s job)

### `model`

- **Responsibility.** Constructs the OpenRouter-backed ADK model; owns PKCE in
  the browser.
- **Public.** `New(cfg Config) (model.LLM, error)`, `PKCEFlow` (js/wasm only)
- **May import.** ADK `model/openaimodel`, `net/http`
- **Must not import.** DBOS, `generated/client`

### `wasmpg`

- **Responsibility.** Implements `net.Conn` over PGlite’s `execProtocol`.
- **Build tag.** `//go:build js && wasm`
- **Public.** `Dialer(pglite js.Value) func(ctx, network, addr string) (net.Conn, error)`
- **May import.** `syscall/js`, `net`
- **Must not import.** anything else in the module

### `generated/client`

- **Responsibility.** All SQL. All pgx usage.
- **Public.** Generated. `Tx` interface, typed query/mutation methods.
- **Hand-editing is forbidden and detected by CI.**

-----

## 6. Domain Model

### 6.1 Entities

|Entity        |Owner               |Lifecycle                                                                            |Identity                      |
|--------------|--------------------|-------------------------------------------------------------------------------------|------------------------------|
|`Workflow`    |DBOS                |PENDING → ENQUEUED → PENDING → SUCCESS/ERROR/CANCELLED/MAX_RECOVERY_ATTEMPTS_EXCEEDED|`workflow_uuid`               |
|`Step`        |DBOS                |created → completed/errored                                                          |`(workflow_uuid, function_id)`|
|`StreamValue` |DBOS                |appended; closed                                                                     |`(workflow_uuid, key, offset)`|
|`Notification`|DBOS                |sent → consumed                                                                      |`(destination_uuid, topic)`   |
|`Session`     |AgentIQ             |created → active → (never deleted)                                                   |`(app_name, user_id, id)`     |
|`Event`       |AgentIQ             |append-only                                                                          |`(session_id, id)`            |
|`Part`        |AgentIQ             |append-only, ordered                                                                 |`(event_id, part_index)`      |
|`Actions`     |AgentIQ             |append-only, 0..1 per event                                                          |`event_id`                    |
|`StateDelta`  |AgentIQ             |append-only                                                                          |`(event_id, key)`             |
|`SessionState`|AgentIQ             |projected from deltas                                                                |`(session_id, scope, key)`    |
|`Agent`       |Registry (projected)|immutable per digest                                                                 |`digest`                      |
|`Instruction` |Registry (projected)|immutable per digest                                                                 |`digest`                      |
|`ModelConfig` |Registry (projected)|immutable per digest                                                                 |`digest`                      |
|`Skill`       |Registry (projected)|immutable per digest                                                                 |`digest`                      |
|`Tool`        |Registry (projected)|immutable per digest                                                                 |`digest`                      |

### 6.2 Relationships

```
Agent ──HAS_INSTRUCTION──▶ Instruction
Agent ──USES_MODEL───────▶ ModelConfig
Agent ──USES_SKILL───────▶ Skill
Agent ──USES_TOOL────────▶ Tool
Agent ──HAS_SUB_AGENT────▶ Agent            (recursive; see §6.5)

Workflow ──HAS_STEP──────▶ Step
Step ────SPAWNED─────────▶ Workflow          (child workflows)
Workflow ──PARENT────────▶ Workflow
Workflow ──EMITTED───────▶ StreamValue
Workflow ──RAN_AGENT─────▶ Agent             (by pinned digest)
Workflow ──HAS_SESSION───▶ Session

Session ──HAS_EVENT──────▶ Event
Event ────HAS_PART───────▶ Part
Event ────HAS_ACTIONS────▶ Actions
Actions ──SETS───────────▶ StateDelta
Event ────PRODUCED_BY────▶ Step
```

`Event ──PRODUCED_BY──▶ Step` is the provenance edge. It is the substrate the
sibling memory project reads: every derived fact can trace to the exact event,
part, and step that produced it.

### 6.3 Agent immutability

An agent is identified by the digest of its OCI image index. That digest
transitively fixes the instruction, model config, skills, tools and sub-agents,
because each is a descriptor containing its own digest.

`agentiq.workflow_agent(workflow_uuid, agent_digest)` pins the digest at enqueue
time. Replay re-reads the same projected rows. Editing an agent produces a new
digest and does not affect in-flight executions — including executions suspended
for days awaiting approval.

### 6.4 State scoping

ADK state keys are prefixed. AgentIQ persists two scopes and discards one:

|Prefix  |Scope           |Persisted             |
|--------|----------------|----------------------|
|`app:`  |application-wide|yes, `scope='app'`    |
|`user:` |per user        |yes, `scope='user'`   |
|*(none)*|per session     |yes, `scope='session'`|
|`temp:` |per invocation  |**no**                |

### 6.5 Sub-agent traversal

`Agent -[HAS_SUB_AGENT]-> Agent` is recursive with unbounded depth. PG19
SQL/PGQ does not support variable-length paths, so:

- **Fixed-depth queries** (direct sub-agents, grandchildren) use `GRAPH_TABLE`.
- **Transitive closure** (all descendants of an agent) uses a recursive CTE
  exposed as a generated query, with `MAX_DEPTH = 16`.

Resolution at pull time detects cycles by digest and fails with
`ErrCycleDetected`. Depth beyond 16 fails with `ErrDepthExceeded`.

-----

## 7. Data Contracts

### 7.1 Canonical SDL — session and event model

```graphql
directive @table(name: String!, schema: String) on OBJECT
directive @column(name: String!) on FIELD_DEFINITION
directive @readonly on OBJECT
directive @vertex(key: [String!]!) on OBJECT
directive @edge(from: String!, to: String!, label: String!) on FIELD_DEFINITION
directive @function(schema: String!, name: String!) on FIELD_DEFINITION

scalar JSON      # maps to Postgres `json` — preserves key order for round-trip
scalar JSONB     # maps to Postgres `jsonb` — GIN-indexable, key order NOT preserved
scalar Bytes     # bytea
scalar DateTime  # timestamptz(6)

type Session @table(name: "session", schema: "agentiq") @vertex(key: ["id"]) {
  id            ID!
  appName       String!  @column(name: "app_name")
  userId        String!  @column(name: "user_id")
  workflowUuid  String   @column(name: "workflow_uuid")
  agentDigest   String!  @column(name: "agent_digest")
  createdAt     DateTime!
  lastUpdateAt  DateTime!
  events        [Event!]! @edge(from: "id", to: "session_id", label: "HAS_EVENT")
  state         [SessionState!]! @edge(from: "id", to: "session_id", label: "HAS_STATE")
}

"""
Mirrors google.golang.org/adk/v2/session.Event, which embeds model.LLMResponse.
Partial events are never stored (§9.4).
"""
type Event @table(name: "event", schema: "agentiq") @vertex(key: ["id"]) {
  id                 ID!
  sessionId          ID!      @column(name: "session_id")
  sequence           Int!     # monotonic per session; the ordering guarantee
  invocationId       String!  @column(name: "invocation_id")
  author             String!
  branch             String   # dotted agent path, e.g. "root.researcher"
  timestamp          DateTime!
  turnComplete       Boolean! @column(name: "turn_complete")
  interrupted        Boolean!
  isolationScope     String   @column(name: "isolation_scope")
  errorCode          String   @column(name: "error_code")
  errorMessage       String   @column(name: "error_message")
  contentRole        String   @column(name: "content_role")
  longRunningToolIds [String!] @column(name: "long_running_tool_ids")
  requestedInput     JSON     @column(name: "requested_input")
  routes             JSON
  nodeInfo           JSON     @column(name: "node_info")
  groundingMetadata  JSON     @column(name: "grounding_metadata")
  usageMetadata      JSON     @column(name: "usage_metadata")
  citationMetadata   JSON     @column(name: "citation_metadata")
  customMetadata     JSON     @column(name: "custom_metadata")

  stepFunctionId     Int      @column(name: "step_function_id")   # provenance
  workflowUuid       String   @column(name: "workflow_uuid")      # provenance

  parts   [Part!]!  @edge(from: "id", to: "event_id", label: "HAS_PART")
  actions Actions   @edge(from: "id", to: "event_id", label: "HAS_ACTIONS")
}

"""
Mirrors genai.Part. NOT a discriminated union: several fields may be set on the
same part (e.g. text + thought + thoughtSignature). One wide table; nested
structs are flattened into prefixed columns (§7.2).
"""
type Part @table(name: "part", schema: "agentiq") @vertex(key: ["event_id", "part_index"]) {
  eventId                   ID!     @column(name: "event_id")
  partIndex                 Int!    @column(name: "part_index")

  text                      String
  thought                   Boolean
  thoughtSignature          Bytes   @column(name: "thought_signature")

  functionCallId            String  @column(name: "function_call_id")
  functionCallName          String  @column(name: "function_call_name")
  functionCallArgs          JSON    @column(name: "function_call_args")

  functionResponseId        String  @column(name: "function_response_id")
  functionResponseName      String  @column(name: "function_response_name")
  functionResponseResponse  JSON    @column(name: "function_response_response")

  inlineDataMimeType        String  @column(name: "inline_data_mime_type")
  inlineDataBytes           Bytes   @column(name: "inline_data_bytes")
  inlineDataDisplayName     String  @column(name: "inline_data_display_name")

  fileDataMimeType          String  @column(name: "file_data_mime_type")
  fileDataUri               String  @column(name: "file_data_uri")
  fileDataDisplayName       String  @column(name: "file_data_display_name")

  executableCodeLanguage    String  @column(name: "executable_code_language")
  executableCodeCode        String  @column(name: "executable_code_code")

  codeExecutionOutcome      String  @column(name: "code_execution_outcome")
  codeExecutionOutput       String  @column(name: "code_execution_output")

  videoMetadataStartOffset  String  @column(name: "video_metadata_start_offset")
  videoMetadataEndOffset    String  @column(name: "video_metadata_end_offset")
  videoMetadataFps          Float   @column(name: "video_metadata_fps")

  audioTranscription        JSON    @column(name: "audio_transcription")
  mediaResolution           String  @column(name: "media_resolution")
  toolCall                  JSON    @column(name: "tool_call")
  toolResponse              JSON    @column(name: "tool_response")
  partMetadata              JSON    @column(name: "part_metadata")
}

type Actions @table(name: "actions", schema: "agentiq") @vertex(key: ["event_id"]) {
  eventId          ID!  @column(name: "event_id")
  skipSummarization Boolean @column(name: "skip_summarization")
  transferToAgent  String  @column(name: "transfer_to_agent")
  escalate         Boolean
  requestedAuthConfigs JSON @column(name: "requested_auth_configs")
  stateDeltas   [StateDelta!]! @edge(from: "event_id", to: "event_id", label: "SETS")
  artifactDeltas [ArtifactDelta!]! @edge(from: "event_id", to: "event_id", label: "PRODUCES")
}

type StateDelta @table(name: "state_delta", schema: "agentiq") @vertex(key: ["event_id", "key"]) {
  eventId ID!    @column(name: "event_id")
  scope   String!         # app | user | session
  key     String!
  value   JSON!
}

type ArtifactDelta @table(name: "artifact_delta", schema: "agentiq") @vertex(key: ["event_id", "filename"]) {
  eventId  ID!    @column(name: "event_id")
  filename String!
  version  Int!
}

type SessionState @table(name: "session_state", schema: "agentiq") @vertex(key: ["session_id", "scope", "key"]) {
  sessionId ID!     @column(name: "session_id")
  scope     String!
  key       String!
  value     JSON!
  updatedAt DateTime! @column(name: "updated_at")
}
```

### 7.2 Round-trip fidelity

The `SessionService` must satisfy `session/sessiontestsuite`. In addition,
AgentIQ defines a conformance property:

> For any `session.Event` E, `json.Marshal(load(store(E))) == json.Marshal(E)`.

Rules that make this hold:

1. Columns typed `JSON` map to Postgres `json`, **not** `jsonb`. `jsonb`
   normalizes key order and whitespace, breaking byte equality.
1. `part_index` is written from slice position and read back in `ORDER BY part_index`.
1. Absent vs zero is distinguished by nullability: a Go `nil` pointer field maps
   to SQL `NULL`; a set-but-empty value maps to a non-null empty value.
1. `temp:` prefixed state keys are dropped on write and are absent on read. This
   is the one intentional deviation and is asserted explicitly by a test.

### 7.3 Read-only projection of `dbos.*`

```graphql
type Workflow @table(name: "workflow_status", schema: "dbos") @readonly
              @vertex(key: ["workflow_uuid"]) {
  workflowUuid     ID!     @column(name: "workflow_uuid")
  status           String!
  name             String!
  queueName        String  @column(name: "queue_name")
  createdAt        DateTime! @column(name: "created_at")
  updatedAt        DateTime! @column(name: "updated_at")
  recoveryAttempts Int     @column(name: "recovery_attempts")
  attributes       JSONB
  inputs           JSON
  output           JSON
  error            JSON

  steps    [Step!]!   @edge(from: "workflow_uuid", to: "workflow_uuid", label: "HAS_STEP")
  parent   Workflow   @edge(from: "parent_workflow_id", to: "workflow_uuid", label: "PARENT")
  streams  [StreamValue!]! @edge(from: "workflow_uuid", to: "workflow_uuid", label: "EMITTED")
  session  Session    @edge(from: "workflow_uuid", to: "workflow_uuid", label: "HAS_SESSION")
}

type Step @table(name: "operation_outputs", schema: "dbos") @readonly
          @vertex(key: ["workflow_uuid", "function_id"]) {
  workflowUuid   ID!    @column(name: "workflow_uuid")
  functionId     Int!   @column(name: "function_id")
  functionName   String @column(name: "function_name")
  output         JSON
  error          JSON
  childWorkflow  Workflow @edge(from: "child_workflow_id", to: "workflow_uuid", label: "SPAWNED")
}

type StreamValue @table(name: "streams", schema: "dbos") @readonly
                 @vertex(key: ["workflow_uuid", "key", "offset"]) {
  workflowUuid ID!    @column(name: "workflow_uuid")
  key          String!
  seq          Int!   @column(name: "offset")   # rename hint: `offset` is reserved
  value        JSON
  functionId   Int    @column(name: "function_id")
}
```

gopgql emits **no DDL** for `@readonly` types. DBOS owns those tables and
migrates them with `dbos migrate`.

### 7.4 Mutations

Every mutation maps to a DBOS PL/pgSQL function. There are no hand-written
resolvers.

```graphql
type Mutation {
  startAgentRun(
    agentDigest: String!
    userId: String!
    input: JSON!
    queue: String = "agent"
    deduplicationId: String
    priority: Int
  ): String! @function(schema: "dbos", name: "enqueue_workflow")

  approve(
    workflowUuid: String!
    decision: JSON!
    idempotencyKey: String!
  ): Boolean! @function(schema: "dbos", name: "send_message")
}
```

**Architectural consequence.** DBOS exposes *enqueue*, not direct start. Every
externally triggered AgentIQ run therefore passes through a DBOS queue, which
supplies concurrency limits, rate limiting, deduplication and priority. There is
no unqueued execution path reachable from the API.

`cancel`, `resume` and `fork` have no SQL function. They are **not** in the
GraphQL API; they are `agentiq admin` subcommands on the server binary, using
the DBOS client library directly.
This is a documented boundary, not an omission.

### 7.5 Subscriptions (M7)

```graphql
type Subscription {
  runEvents(workflowUuid: String!, key: String! = "events", afterOffset: Int = -1): StreamValue!
}
```

Backed by `dbos.ClientReadStream`. `afterOffset` makes reconnection exact.
`CloseStream` terminates the subscription with GraphQL `complete`.
Stream writes are **at-least-once**, so consumers deduplicate by `offset`.

### 7.6 Agent artifact contract

The format is **owned by epos**, not by AgentIQ. `EPOS-REQUIREMENTS.md` states
the requirements AgentIQ places on it; the normative specification lives in the
epos repository. Summary of the contract AgentIQ depends on:

- An agent is an **OCI image index** with `artifactType`
  `application/vnd.epos.agent.index.v1+json`.
- `manifests[]` contains exactly one descriptor annotated
  `dev.epos.role=definition`, and zero or more annotated
  `instruction`, `model`, `skill`, `tool`, `subagent`.
- Every document uses a `TypeMeta`/`ObjectMeta`/`Spec`/`Status` envelope.
- References are digests. There is no lock file: the index *is* the lock.
- Tags are resolved to digests once, before execution, and never re-resolved
  during a run.

-----

## 8. Main Interfaces

### 8.1 Workflow

```go
package workflow

// AgentRunInput is the durable workflow input. It is serialized into
// dbos.workflow_status.inputs and replayed verbatim on recovery.
// AgentDigest pins the entire agent closure (§6.3).
type AgentRunInput struct {
    AgentDigest string          `json:"agentDigest"`
    AppName     string          `json:"appName"`
    UserID      string          `json:"userId"`
    SessionID   string          `json:"sessionId"`
    Message     json.RawMessage `json:"message"`
}

type AgentRunOutput struct {
    SessionID string `json:"sessionId"`
    Status    string `json:"status"`
}

// Register wires every workflow into the DBOS context. Called once at startup,
// before Launch.
func Register(ctx dbos.Context, deps Deps) error

// AgentRun is deterministic workflow code. Every non-deterministic operation
// inside it is a step. Enforced by analyzer/determinism.
func AgentRun(ctx dbos.Context, in AgentRunInput) (AgentRunOutput, error)
```

### 8.2 DBOS↔ADK integration

Mirrors Temporal’s `contrib/googleadk`: the agent loop is workflow code, the
model call is a step, and each tool is a step.

```go
package dbosadk

// NewModel wraps an ADK model so each generation is a durable step with its own
// retry policy. Partial responses are written to a DBOS stream, never returned
// through the checkpoint.
func NewModel(inner model.LLM, opts ...StepOption) model.LLM

// StepAsTool wraps an ADK tool so each invocation is a durable step.
// Equivalent to Temporal's ActivityAsTool.
func StepAsTool(t tool.Tool, opts ...StepOption) tool.Tool

// StepOption configures the underlying dbos.RunAsStep call.
type StepOption func(*stepConfig)

func WithMaxRetries(n int) StepOption
func WithBaseInterval(d time.Duration) StepOption
func WithStepName(name string) StepOption

// Plugin installs the ADK-side hooks: it routes every model and tool
// invocation through the step wrappers and rejects direct use of
// non-deterministic ADK facilities inside workflow code.
func NewPlugin(cfg Config) adk.Plugin

// StreamKey returns the DBOS stream key for an invocation's partial events.
func StreamKey(invocationID string) string
```

### 8.3 Session persistence

```go
package session

// New returns an ADK session.Service backed by PostgreSQL through the
// generated client. Every AppendEvent runs inside dbos.RunAsTransaction, so
// the event rows and the step checkpoint commit atomically.
//
// Partial events (Event.Partial == true) are dropped, matching every other
// ADK implementation and asserted by sessiontestsuite.
func New(ds *dbos.DataSource) session.Service
```

### 8.4 Artifact resolution

```go
package artifact

// epos.Closure is a fully resolved, digest-pinned agent, produced by the epos
// resolver. AgentIQ does not define it; it is reproduced here as the contract
// AgentIQ consumes.
//
//   type Closure struct {
//       Digest      digest.Digest
//       Definition  AgentDefinition
//       Instruction *InstructionDoc
//       Model       *ModelDoc
//       Skills      []SkillDoc
//       Tools       []ToolRef      // descriptors; the proxy executes them
//       SubAgents   []Closure      // recursive; depth <= epos.MaxDepth
//   }
//
// epos.MaxDepth is 16. epos.ErrCycleDetected and epos.ErrDepthExceeded are
// returned by resolution.

// Resolve delegates to the epos resolver. Idempotent and cacheable: the same
// digest always yields the same Closure, so it is safe as a retried DBOS step.
func Resolve(ctx context.Context, ref string) (epos.Closure, error)

// Project writes a Closure into agentiq.* rows inside the caller's
// transaction. Keyed by digest, so re-projection is a no-op. This is the one
// part AgentIQ owns: epos knows nothing about the property graph.
func Project(ctx context.Context, tx client.Tx, c epos.Closure) error
```

### 8.5 Tool execution

```go
package proxy

// Connect resolves the closure's tool descriptors against a running
// mcp-anything instance and returns them as ADK tools. The returned tools are
// NOT yet durable; workflow code wraps each with dbosadk.StepAsTool.
func Connect(ctx context.Context, endpoint string, c epos.Closure) ([]tool.Tool, error)
```

### 8.6 Browser Postgres transport

```go
//go:build js && wasm

package wasmpg

// Dialer returns a pgconn DialFunc backed by a PGlite instance. pgx never
// touches the network: the returned net.Conn marshals wire-protocol bytes
// through PGlite's execProtocol (§12).
func Dialer(pglite js.Value) func(ctx context.Context, network, addr string) (net.Conn, error)

// Multiplexer serializes N logical connections onto PGlite's single backend
// session and routes asynchronous NotificationResponse frames.
type Multiplexer struct{ /* ... */ }
```

-----

## 9. Execution Model

### 9.1 Determinism contract

DBOS replays the workflow function on recovery, invoking the same steps with
the same inputs in the same order. Inside any function reachable from
`RegisterWorkflow`:

**Forbidden.** `time.Now`, `time.Since`, `math/rand`, `crypto/rand`, UUID
generation, `os.Getenv`, map iteration, bare `go` statements, bare `select`,
`net/http`, file I/O.

**Required substitutes.** `dbos.Sleep` for delays, `dbos.Go` and `dbos.Select`
for concurrency (they assign deterministic step IDs and checkpoint which
channel was selected), `dbos.RunAsStep` for everything else.

Unlike Temporal, DBOS Go performs **no automatic substitution and no import
sandboxing**. AgentIQ therefore owns this boundary and enforces it with a
custom analyzer (§17.1).

### 9.2 Step taxonomy

|Operation           |Mechanism               |Retries                |Notes                                            |
|--------------------|------------------------|-----------------------|-------------------------------------------------|
|Model generation    |`RunAsStep`             |5, base 1s, exponential|Streams partials as a side effect                |
|Tool invocation     |`RunAsStep`             |3, base 500ms          |Per-tool override from the artifact              |
|Artifact resolution |`RunAsStep`             |3                      |Idempotent by digest                             |
|Session/event append|`RunAsTransaction`      |3                      |Exactly-once; commits with the checkpoint        |
|Human approval wait |`dbos.Recv`             |n/a                    |168h default timeout                             |
|Sub-agent invocation|child workflow via queue|inherits               |Recorded in `operation_outputs.child_workflow_id`|

### 9.3 Concurrency

Parallel sub-agents use `dbos.Go` and are gathered with `dbos.Select`. Fan-out
across a queue provides global concurrency limits and rate limiting. Bare
goroutines in workflow code are a lint error.

### 9.4 Streaming

ADK emits partial events during generation. Every ADK session implementation
skips them on append, and `sessiontestsuite` asserts zero stored events for
`Partial=true`. AgentIQ follows that contract:

```
model chunk ──▶ dbos.WriteStream(key=StreamKey(invocationID), value=chunk)
                       │
                       └──▶ dbos.streams (offset-ordered, durable)
final event ──▶ RunAsTransaction ──▶ agentiq.event + part + actions + state_delta
turn end    ──▶ dbos.CloseStream
```

Consequences recorded explicitly:

- Stream writes from a step are **at-least-once**. A retried step can duplicate
  values, in order. Readers deduplicate by `offset`.
- `dbos.streams` grows without bound. Retention: rows are deleted for workflows
  in a terminal state older than 30 days, by a scheduled maintenance job outside
  the workflow path.
- DBOS changes `workflow_status` itself and does not write to a stream. AgentIQ
  therefore emits its own progress values at each phase boundary; status
  transitions are not observable through subscriptions for free.

### 9.5 Human approval

```
ADK signals a confirmation requirement
  │
  ├─ RunAsTransaction: append Event{interrupted: true, requestedInput: {...}}
  │
  ├─ dbos.Recv(ctx, topic="approval:"+invocationID, 168*time.Hour)
  │     ▲
  │     └── GraphQL: approve(workflowUuid, decision, idempotencyKey)
  │              └─▶ dbos.send_message(...)   [SQL function]
  │
  ├─ on decision: resume via ADK's synthetic confirmation function-call
  ├─ on timeout:  append Event{errorCode: "APPROVAL_TIMEOUT"}; workflow → ERROR
  └─ continue loop
```

`idempotencyKey` prevents double-approval. Pending approvals are a plain graph
query (`Event.interrupted = true` with no subsequent resolution event), so no
subscription is required before M7.

### 9.6 Tool execution

The runtime never spawns processes or containers. It speaks Streamable HTTP to
mcp-anything, in both server and browser builds.

```
workflow ──RunAsStep──▶ proxy client ──POST /mcp──▶ mcp-anything
                                                      │
                                          resolves descriptor by artifactType:
                                            container image  ──▶ spawn/route
                                            script artifact  ──▶ js | lua | bash
```

Credentials live in the proxy, never in the artifact and never in the runtime.
Stdio targets are spawned by the proxy and inherit its trust boundary — this is
documented as a deliberate security property of the deployment, not a defect.

### 9.7 Model calls

The one path that bypasses the proxy. `model/openaimodel` is configured with
`BaseURL: "https://openrouter.ai/api/v1"`.

- **Server.** API key from `OPENROUTER_API_KEY`.
- **Browser.** OAuth PKCE. The page generates a verifier and challenge,
  redirects to OpenRouter, and exchanges the code at
  `POST /api/v1/auth/keys` for a user-controlled key. No client registration and
  no backend. The key is held in `sessionStorage`, never `localStorage`, and is
  cleared on sign-out; the demo ships no secret and each user funds their own
  inference.

-----

## 10. Configuration Model

### 10.1 Runtime configuration

|Variable                   |Purpose                                           |Required   |
|---------------------------|--------------------------------------------------|-----------|
|`DATABASE_URL`             |Application + DBOS system database (same database)|yes        |
|`DBOS_SYSTEM_SCHEMA`       |Defaults to `dbos`                                |no         |
|`OPENROUTER_API_KEY`       |Server-side model access                          |server only|
|`MCP_PROXY_ENDPOINT`       |mcp-anything Streamable HTTP URL                  |M4+        |
|`OCI_REGISTRY`             |Registry the runtime resolves agent digests from  |M3+        |
|`AGENTIQ_QUEUE_CONCURRENCY`|Global worker concurrency                         |no         |

The application database and the DBOS system database **must be the same
database**. Separate databases would forfeit the single-transaction guarantee
that `RunAsTransaction` provides, which is the reason DataSources were chosen.

### 10.2 Agent configuration

Nothing about an agent is configured at runtime. Every configurable property is
a field in a digest-pinned artifact document (§7.6, `EPOS-REQUIREMENTS.md`):
instructions, model and its parameters, retry policies per tool, sub-agent
composition, skills.

The only runtime input is `AgentRunInput` (§8.1).

-----

## 11. Code Generation

|Input                   |Generator|Output                                                        |Regeneration       |
|------------------------|---------|--------------------------------------------------------------|-------------------|
|`schema/agentiq.graphql`|gopgql   |`generated/migrations/`, `generated/client/`, `generated/gql/`|`go generate ./...`|
|`schema/dbos.graphql`   |gopgql   |`generated/graph/` (property graph DDL), read-only client     |`go generate ./...`|

Rules:

1. Generated code is never hand-edited.
1. `go generate ./... && git diff --exit-code` runs in CI. A non-empty diff
   fails the build.
1. Generation is **hermetic**: gopgql compiles SDL without a live database.
   No generator may depend on running infrastructure.
1. Generator versions are pinned in `go.mod` and in `tools/tools.go`.

-----

## 12. Browser Runtime

The browser build runs the production runtime — real DBOS, real ADK, real pgx,
real generated client — against a real PostgreSQL 19 engine. It differs from the
server build in exactly one place: the transport beneath pgx.

### 12.1 The problem

`dbos.NewDataSource` requires `*pgxpool.Pool`. pgx requires a `net.Conn`. A
browser WASM sandbox cannot open TCP sockets. PGlite is a single-connection,
single-user-mode Postgres compiled with Emscripten that skips the normal startup
and authentication handshake and operates outside any TCP connection.

### 12.2 The solution

```
        Go (js/wasm)                          JavaScript
┌────────────────────────────┐      ┌────────────────────────────┐
│ dbos ──▶ pgxpool ──▶ pgconn│      │  PGlite (PG19 fork)        │
│                    DialFunc│      │                            │
│                       │    │      │  execProtocol(msg)         │
│                       ▼    │      │    ──▶ [ {parsed, raw}, …] │
│  wasmpg.Conn (net.Conn)    │◀────▶│                            │
│    Write ─▶ execProtocol   │ js   │  onNotification(cb)        │
│    Read  ◀─ raw chunks     │      │  listen / unlisten         │
│                            │      │                            │
│  Multiplexer               │      └────────────────────────────┘
│    - handshake synthesis   │
│    - N logical conns → 1   │
│    - notification routing  │
└────────────────────────────┘
```

**Why `execProtocol` and not `execProtocolRaw`.** `execProtocol` returns one
tuple per wire-protocol result message, each including that message’s raw
`Uint8Array`, and is safe to use alongside PGlite’s other APIs because it
handles errors, transactions **and notifications**. The `Raw` variants
explicitly bypass the wrappers that manage notification listeners — which is
exactly the machinery DBOS depends on for queues and stream reads.

### 12.3 Handshake synthesis

Single-user mode skips startup and authentication, but pgx sends `SSLRequest`
and `StartupMessage` and expects `AuthenticationOk`, `ParameterStatus`,
`BackendKeyData` and `ReadyForQuery`. The multiplexer answers these locally:

|pgx sends       |Multiplexer responds                                                                                                                                                             |
|----------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
|`SSLRequest`    |`'N'` (SSL unsupported)                                                                                                                                                          |
|`StartupMessage`|`AuthenticationOk`, `ParameterStatus` (`server_version`, `client_encoding`, `DateStyle`, `TimeZone`, `integer_datetimes`), `BackendKeyData` (synthetic PID), `ReadyForQuery('I')`|
|`Terminate`     |close logical connection; backend stays alive                                                                                                                                    |

### 12.4 Multiplexing and notifications

- Protocol messages are serialized: one in flight against the backend at a time,
  with a FIFO queue per logical connection.
- `LISTEN` registrations are **session-global**, not per-connection. The
  multiplexer maintains a channel → logical-connection routing table populated by
  observing `LISTEN`/`UNLISTEN` traffic.
- Notifications arriving via PGlite’s `onNotification` are encoded as
  wire-format `NotificationResponse` (`'A'`: PID, channel, payload) and injected
  into the read buffer of every logical connection registered for that channel.
  pgx surfaces them through its `OnNotification` callback.
- `pgxpool` is configured with `MaxConns` matching the multiplexer’s logical
  connection budget; DBOS’s listener connection is one of them.

### 12.5 Goroutine and event-loop discipline

`Read` blocks a goroutine on a JS promise via a `js.FuncOf` callback and a
channel. The JS event loop is never blocked. All `syscall/js` calls occur on the
Go scheduler’s thread; `wasmpg` is the only package permitted to import
`syscall/js`.

### 12.6 Known constraints

|Constraint                       |Consequence                                                                                                                 |
|---------------------------------|----------------------------------------------------------------------------------------------------------------------------|
|GitHub Pages cannot set COOP/COEP|No `SharedArrayBuffer`; PGlite uses IndexedDB persistence, not OPFS-with-SAB                                                |
|PGlite is single-backend         |True concurrent transactions are serialized; throughput is not representative of production                                 |
|Standard Go WASM binary size     |Measured and gated in CI (§18.3); TinyGo is not an option because pgx, DBOS, ADK and `encoding/json` require full reflection|
|PG19 engine                      |Satisfied: the PGlite PG19 fork is delivered and verified. Pinned by version in the demo build                              |

-----

## 13. Deployment

### 13.1 GitHub Pages — branch mode

The demo is published to the `gh-pages` branch. Artifact-based Pages deployment
is **not** used, because PR previews require branch mode.

`.github/workflows/deploy.yml`

```yaml
name: Deploy demo
on:
  push:
    branches: [main]
permissions:
  contents: write
concurrency:
  group: pages-deploy
  cancel-in-progress: false
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.25' }
      - name: Build WASM demo
        run: make demo
      - name: Deploy to gh-pages
        uses: peaceiris/actions-gh-pages@v4
        with:
          github_token: ${{ secrets.GITHUB_TOKEN }}
          publish_dir: ./demo/dist
          clean-exclude: pr-preview
          force_orphan: false
```

`clean-exclude: pr-preview` is mandatory: without it, a production deploy wipes
every live PR preview.

### 13.2 PR previews

`.github/workflows/pr-preview.yml`

```yaml
name: PR preview
on:
  pull_request:
    types: [opened, reopened, synchronize, closed]
permissions:
  contents: write
  pull-requests: write
concurrency:
  group: preview-${{ github.ref }}
  cancel-in-progress: true
jobs:
  preview:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.25' }
      - name: Build WASM demo
        if: github.event.action != 'closed'
        run: make demo
      - uses: rossjrw/pr-preview-action@v1
        with:
          source-dir: ./demo/dist
          preview-branch: gh-pages
          umbrella-dir: pr-preview
          action: auto
```

`action: auto` deploys on open/reopen/synchronize and removes the preview on
close. Every milestone’s demo is reachable at
`https://<user>.github.io/agentiq/pr-preview/pr-<n>/`.

### 13.3 Pages constraints affecting the build

- Relative asset paths only. The demo’s HTML references `./` paths so previews
  served from `/pr-preview/pr-N/` resolve correctly.
- `.nojekyll` is added automatically by `peaceiris/actions-gh-pages@v4`.
- The PGlite WASM bundle and the Go WASM binary are committed build outputs on
  `gh-pages`; they are never stored via Git LFS.
- No custom headers, therefore no COOP/COEP, therefore no `SharedArrayBuffer`
  (§12.6).

### 13.4 Frontend

`gaarutyunov/ui-kit` — zero-dependency Web Components, MIT, buildless. The demo
loads `wasm_exec.js`, instantiates the Go binary, and the Go side drives ui-kit
components through DOM APIs. No bundler is required, which keeps the deployed
tree inspectable.

### 13.5 Release

`agentiq` is released with goreleaser on tag push: `linux/darwin/windows` ×
`amd64/arm64`, pure Go, no cgo. AgentIQ ships one binary; the authoring CLI is
`epos`.

-----

## 14. Testing Strategy

### 14.1 Levels

|Level            |Tool                                              |Target                                                                   |
|-----------------|--------------------------------------------------|-------------------------------------------------------------------------|
|Unit             |`go test`                                         |Pure functions: artifact resolution, part flattening, wire-frame encoding|
|Conformance      |`session/sessiontestsuite`                        |The `SessionService` implementation                                      |
|Integration      |godog + testcontainers-go                         |Real `postgres:19beta2`, real `zot`                                      |
|Failure injection|testcontainers pause/stop, process kill, toxiproxy|The failure matrix (§19)                                                 |
|Browser E2E      |chromedp                                          |The deployed Pages demo, pure Go                                         |
|Codegen          |`git diff --exit-code`                            |Generated artifacts current                                              |

### 14.2 Integration infrastructure

```go
pg, err := postgres.Run(ctx, "postgres:19beta2",
    postgres.WithDatabase("agentiq"),
    testcontainers.WithWaitStrategy(postgres.BasicWaitStrategies()),
)
```

`BasicWaitStrategies()` waits for the readiness log line **twice**, because
PostgreSQL restarts once after initdb. Omitting this is the primary source of
flaky startup on macOS and Windows runners.

`zot` runs in a container for artifact tests: pure Go, Apache-2.0, supports the
Referrers API and `_catalog`.

### 14.3 Feature files are canonical

`features/*.feature` are hand-authored and are the single source of scenario
truth. They are never generated, never duplicated into Go string literals, and
never paraphrased in this document beyond illustration. godog runs them through
`go test` with `Options.TestingT` and emits JUnit output for CI.

### 14.4 Browser E2E

`chromedp` drives headless Chrome against the deployed preview URL. Keeping this
in Go avoids a Node toolchain in CI and lets browser assertions share fixtures
with the server-side integration tests.

-----

## 15. Gherkin Scenarios

Illustrative extracts. The authoritative files live in `features/`.

```gherkin
Feature: Durable workflow execution
  Scenario: Worker killed mid-workflow resumes from the last checkpoint
    Given a clean PostgreSQL 19 database
    And a registered workflow with three steps
    When the workflow completes step one
    And the worker process is killed
    And a new worker starts
    Then step one is not re-executed
    And the workflow completes successfully

  Scenario: PostgreSQL restarts under a running workflow
    Given a running workflow suspended between steps
    When the database container is stopped and restarted
    Then the workflow resumes without duplicating completed steps
```

```gherkin
Feature: Session persistence
  Scenario: Partial events are never stored
    Given an agent producing a streamed response
    When the model emits five partial events and one final event
    Then the session contains exactly one event
    And the DBOS stream contains five values

  Scenario: Event round-trip is byte-identical
    Given an event with text, a thought signature and a function call
    When the event is stored and reloaded
    Then the reloaded event marshals identically to the original
```

```gherkin
Feature: Agent artifacts
  Scenario: A pinned digest survives a registry update
    Given an agent published at digest D
    And a workflow suspended awaiting approval, pinned to D
    When a new agent version is published to the same tag
    And the approval is granted
    Then the workflow resumes using the closure of D

  Scenario: Sub-agent cycles are rejected
    Given agent A referencing agent B
    And agent B referencing agent A
    When agent A is resolved
    Then resolution fails with ErrCycleDetected
```

```gherkin
Feature: Browser runtime
  Scenario: The demo runs the real runtime against real Postgres
    Given the deployed demo page
    When a workflow is started from the browser
    Then dbos.workflow_status contains one row with status SUCCESS
    And the property graph returns the workflow with its steps
```

-----

## 16. Verification

`make verify` is the single command. CI runs exactly this.

```makefile
verify: generate-check lint test-unit test-integration demo browser-test

generate-check:
	go generate ./...
	git diff --exit-code

lint:
	./bin/custom-gcl run ./...

test-unit:
	go test ./... -short

test-integration:
	go test ./test/... -tags=integration -timeout 20m

demo:
	GOOS=js GOARCH=wasm go build -o demo/dist/agentiq.wasm ./demo/wasm
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" demo/dist/
	cp -r demo/web/* demo/dist/

browser-test:
	go test ./test/browser -tags=browser
```

Acceptance criteria per milestone are stated in §20. Verification is never
manual.

-----

## 17. Architecture Enforcement

Two mechanisms. There is deliberately **no layering linter**: with a single
module and packages grouped by concern, layering rules would have nothing
meaningful to enforce.

### 17.1 Determinism analyzer (`analyzer/determinism`)

The one check with no off-the-shelf substitute. Temporal ships `workflowcheck`;
DBOS ships nothing equivalent.

- **Entry points.** Functions passed to `dbos.RegisterWorkflow`, plus their
  transitive call graph within this module.
- **Diagnostics.** Calls to `time.Now`, `time.Since`, `math/rand`,
  `crypto/rand`, UUID generation, `os.Getenv`, `net/http`, `os` file
  operations; `range` over a map; bare `go` statements; bare `select`.
- **Escape hatch.** Any call reached through `dbos.RunAsStep`,
  `dbos.RunAsTransaction`, `dbos.Go` or `dbos.Select` terminates the walk.
- **Packaging.** golangci-lint module plugin, built via `golangci-lint custom`
  driven by `.custom-gcl.yml`.

```yaml
# .custom-gcl.yml
version: v2.6.0
name: custom-gcl
destination: ./bin
plugins:
  - module: github.com/gaarutyunov/agentiq
    path: ./analyzer
```

Because the stock `golangci-lint-action` does not run custom module linters, CI
builds `custom-gcl` explicitly (§18.1).

### 17.2 depguard rules

```yaml
linters-settings:
  depguard:
    rules:
      no-sql-outside-generated:
        files: ["$all", "!**/generated/**", "!**/wasmpg/**"]
        deny:
          - pkg: "github.com/jackc/pgx/v5"
            desc: "pgx is confined to generated/ and wasmpg/"
          - pkg: "database/sql"
            desc: "persistence goes through the generated client"
      no-oci-outside-artifact:
        files: ["$all", "!**/artifact/**"]
        deny:
          - pkg: "oras.land/oras-go/v2"
            desc: "OCI access goes through the epos API, in artifact/ only"
          - pkg: "github.com/gaarutyunov/epos"
            desc: "the epos API is confined to artifact/"
      no-js-outside-wasmpg:
        files: ["$all", "!**/wasmpg/**", "!**/demo/wasm/**"]
        deny:
          - pkg: "syscall/js"
            desc: "JS interop is confined to wasmpg/"
```

Raw SQL string literals outside `generated/` are additionally flagged by a
`forbidigo` pattern.

### 17.3 Generated-code currency

`go generate ./... && git diff --exit-code`. Determinism of the generators is a
requirement on gopgql (§3.2): stable ordering, no timestamps in output, no
dependence on a live database.

### 17.4 DBOS schema drift

A CI job introspects `dbos.*` in the running test container and compares the
column set against a checked-in fixture. A mismatch fails the build with the
pinned DBOS version named in the message. DBOS is pre-1.0 and adds columns
(`attributes`, `is_debounced`, `delay_until_epoch_ms` are recent examples); the
read-only projection must be updated deliberately, not discovered in production.

-----

## 18. Continuous Integration

### 18.1 `ci.yml`

```yaml
name: CI
on: [push, pull_request]
jobs:
  verify:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.25' }
      - name: Install golangci-lint
        run: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.0
      - name: Build custom linter
        run: golangci-lint custom
      - name: Generated code is current
        run: go generate ./... && git diff --exit-code
      - name: Lint
        run: ./bin/custom-gcl run ./...
      - name: Unit tests
        run: go test ./... -short
      - name: Integration tests
        run: go test ./test/... -tags=integration -timeout 20m
      - name: DBOS schema drift
        run: go test ./test/drift -tags=integration
      - name: WASM build
        run: make demo
      - name: WASM size gate
        run: test "$(stat -c%s demo/dist/agentiq.wasm)" -lt 41943040
      - name: Browser tests
        run: go test ./test/browser -tags=browser
```

### 18.2 Job matrix per milestone

Every milestone runs the full job set. Milestones do not add or remove jobs;
they add scenarios to existing ones. The exception is `browser-test`, which
exists from M1 because M1 is the browser milestone.

### 18.3 WASM size gate

The binary is measured on every build and gated at 40 MiB. Standard Go WASM with
pgx, DBOS and ADK is large; the gate exists to make growth visible rather than
to enforce a target. Exceeding it is a deliberate decision recorded in this
document, not a silent regression.

-----

## 19. Failure Matrix

Every row has an automated test. Rows marked *(M1)* are testable from the first
milestone; the rest activate as their subsystem lands.

|#  |Failure                                       |Expected behaviour                                                            |Test mechanism                       |
|---|----------------------------------------------|------------------------------------------------------------------------------|-------------------------------------|
|F1 |Worker process killed mid-workflow *(M1)*     |Completed steps are not re-executed; workflow completes                       |`os.Process.Kill` on a child worker  |
|F2 |PostgreSQL restarted mid-workflow *(M1)*      |Workflow resumes; no duplicate step effects                                   |testcontainers stop/start            |
|F3 |Network partition to the database *(M1)*      |Retries; workflow eventually resumes                                          |toxiproxy                            |
|F4 |Retries exhausted on a step                   |`workflow_status` → `RETRIES_EXCEEDED`; error recorded                        |Step returning permanent error       |
|F5 |Max recovery attempts exceeded                |`MAX_RECOVERY_ATTEMPTS_EXCEEDED`; workflow dead-lettered                      |Repeated kill loop                   |
|F6 |Model returns HTTP 529                        |Step retries with backoff; final failure surfaces as an event with `errorCode`|Stub OpenRouter endpoint             |
|F7 |Model returns malformed JSON                  |Step errors; retried; session records the error event                         |Stub endpoint                        |
|F8 |Tool panics                                   |Step recovers, records error, retries per policy                              |Script tool that exits non-zero      |
|F9 |mcp-anything unreachable                      |Step retries; workflow surfaces a tool error event                            |Proxy container stopped              |
|F10|Registry unreachable at resolve time          |Resolution step retries; enqueue fails cleanly if unresolvable                |zot container stopped                |
|F11|Sub-agent cycle in artifact                   |`ErrCycleDetected` before any execution                                       |Crafted index                        |
|F12|Sub-agent depth exceeded                      |`ErrDepthExceeded`                                                            |Crafted 17-deep chain                |
|F13|Approval never arrives                        |`Recv` times out at 168h; event with `APPROVAL_TIMEOUT`; workflow → ERROR     |Compressed timeout in test           |
|F14|Duplicate approval submitted                  |Second `send_message` is a no-op via idempotency key                          |Two identical mutations              |
|F15|Agent updated while a workflow is suspended   |Resumed workflow uses the pinned digest, not the new tag                      |Publish over the tag mid-suspension  |
|F16|Stream value duplicated by step retry         |Consumer deduplicates by offset; UI shows each token once                     |Forced step retry after `WriteStream`|
|F17|Subscription client disconnects and reconnects|Resumes exactly at `afterOffset`; no gap, no repeat                           |Drop and re-establish                |
|F18|Parallel sub-agent branch fails               |Sibling branches complete; parent records the failure                         |One child returns error              |
|F19|Child workflow fails                          |Parent’s step records the error; `child_workflow_id` traceable                |Failing child                        |
|F20|PGlite backend error mid-transaction          |Error frame surfaces through the shim as a pgx error                          |Invalid SQL in browser test          |
|F21|Notification lost in the multiplexer          |Polling fallback still advances the queue                                     |Suppress `onNotification`            |
|F22|Browser tab closed mid-run                    |Nothing is lost; PGlite state persists in IndexedDB and resumes               |chromedp reload                      |

-----

## 20. Milestones

Every milestone produces a releasable system, includes a browser demo built from
production code, and deploys to GitHub Pages with a PR preview. No milestone
depends on a future milestone.

Subscriptions do not exist until M7. Milestones M1–M6 observe progress by
re-querying the graph. This is stated in each milestone rather than left to look
like an omission.

-----

### M1 — Browser Path and Durable Execution

**Goal.** Prove the hardest thing first: the real runtime, in a browser, against
real PostgreSQL 19, with durable execution. No agent.

**Dependencies.** gopgql release with §3.2 items 1–4. The PGlite PG19 fork is
already delivered and is consumed as a pinned build.

**Residual risk.** The engine is proven. What M1 proves is the *Go side*: the
`net.Conn` shim, handshake synthesis, the multiplexer, and notification routing
(§12). Nothing comparable exists in Go, so these are the milestone’s real
unknowns.

**Repository changes.** `schema/dbos.graphql`; `generated/`; `workflow/`;
`wasmpg/`; `demo/`; `analyzer/`; `test/`; all three workflows in
`.github/workflows/`.

**Public API.** `Query.workflow`, `Query.workflows`, `Query.step`;
`Mutation.startAgentRun` mapped to `dbos.enqueue_workflow` (running a trivial
two-step workflow, not an agent).

**Domain model.** `Workflow`, `Step` — read-only projections of `dbos.*`. No
AgentIQ-owned tables yet.

**Execution model.** One registered workflow with two steps and an artificial
delay. Enqueued through a DBOS queue.

**Interfaces.** `workflow.Register`, `wasmpg.Dialer`, `wasmpg.Multiplexer`.

**Browser demo.** ui-kit page: start a workflow, watch it appear in the graph,
kill and reload the tab, observe resumption. PGlite PG19 with `CREATE PROPERTY GRAPH` over `dbos.*`.

**Scenarios.** F1, F2, F3, F20, F21, F22.

**Verification.** `make verify`. Acceptance: `dbos migrate` succeeds against
PGlite through the shim; a `GRAPH_TABLE` query over `dbos.*` returns a workflow
with its steps in both server and browser builds.

**Definition of done.** Zero lint violations; zero generated drift; browser demo
deployed and passing; PR preview live.

-----

### M2 — Single Agent with Session Persistence

**Goal.** The thinnest agent slice. One agent, persisted sessions, no tools, no
streaming, no artifacts.

**Repository changes.** `schema/agentiq.graphql`; `dbosadk/`; `session/`;
`model/`.

**Public API.** `Query.session`, `Query.sessions`, `Query.event`, `Query.part`;
`Mutation.startAgentRun` now runs a real ADK agent.

**Domain model.** `Session`, `Event`, `Part`, `Actions`, `StateDelta`,
`ArtifactDelta`, `SessionState` — the full normalized schema from §7.1. This
schema is permanent; only its ingestion path changes later.

**Configuration.** Agent rows are **seeded directly** by a fixture migration.
The OCI pull lands in M3 and writes these same rows. Nothing built here is
discarded.

**Execution model.** ADK Runner inside the workflow; model call via
`dbosadk.NewModel` as a step; `AppendEvent` via `RunAsTransaction`.

**Browser demo.** OpenRouter PKCE sign-in, one chat turn, session and events
rendered from the graph.

**Scenarios.** F6, F7, plus the round-trip and partial-skip scenarios in §15.

**Verification.** `session/sessiontestsuite` passes. The byte-identical
round-trip property (§7.2) holds for a corpus of crafted events.

**Definition of done.** As M1, plus conformance suite green.

-----

### M3 — Agent Artifacts

**Goal.** Agents become immutable, content-addressed OCI artifacts.

**Dependencies.** epos release with §3.2 epos items 1–4.

**Repository changes.** `artifact/`; `EPOS-REQUIREMENTS.md`. No CLI is added —
packing and publishing agents is `epos`.

**Public API.** `Query.agent`, `Query.agents`, `Query.instruction`,
`Query.modelConfig`, `Query.skill`, `Query.tool`; `Agent.subAgents`;
`Query.agentDescendants` (recursive CTE, §6.5).

**Domain model.** `Agent`, `Instruction`, `ModelConfig`, `Skill`, `Tool`, all
keyed by digest; `HAS_SUB_AGENT` edges.

**Execution model.** `AgentRunInput.AgentDigest` pins the closure. Resolution
through the epos API is a step; projection into the graph is a transaction.

**Browser demo.** Pull an agent from a public CORS-enabled registry over
`fetch`, project it into PGlite, run it. The demo displays the digest.

**Scenarios.** F10, F11, F12, F15.

**Verification.** zot in testcontainers; publish, pull, project, execute.
Re-projection of the same digest is a no-op.

**Definition of done.** As M2, plus `epos pack && epos push` produces an index
whose closure `oras copy --recursive` transfers intact, and AgentIQ resolves and
executes it by digest.

-----

### M4 — Tools via mcp-anything

**Goal.** Agents call tools.

**Dependencies.** mcp-anything with §3.2 proxy items 1–3.

**Repository changes.** `proxy/`.

**Public API.** `Tool` vertices carry `artifactType`; `Event.parts` now include
function calls and responses.

**Execution model.** `proxy.Connect` resolves descriptors; each tool is wrapped
with `dbosadk.StepAsTool`; per-tool retry policy comes from the artifact.

**Browser demo.** An agent calling a script tool through a hosted proxy;
function-call and function-response parts rendered from the graph.

**Scenarios.** F8, F9.

**Definition of done.** As M3, plus tool calls visible as `Part` rows with
`function_call_name` populated and queryable by graph pattern.

-----

### M5 — Human Approval

**Goal.** Workflows suspend for human decisions and survive restarts while
suspended.

**Public API.** `Mutation.approve` mapped to `dbos.send_message`;
`Query.pendingApprovals` (events with `interrupted = true` and no resolution).

**Execution model.** §9.5.

**Browser demo.** An agent requesting confirmation; the page polls
`pendingApprovals`, the user approves, the workflow resumes. Polling is
deliberate — subscriptions arrive in M7.

**Scenarios.** F13, F14.

**Definition of done.** As M4, plus a workflow suspended across a full worker
restart resumes on approval.

-----

### M6 — Multi-Agent and A2A

**Goal.** Agents delegate to sub-agents, locally and over A2A.

**Repository changes.** Sub-agent invocation in `workflow/`; A2A client in
`proxy/`.

**Public API.** `Workflow.children`; `Step.spawnedWorkflow`;
`Query.agentDescendants` exercised in anger.

**Execution model.** Sub-agents run as child workflows enqueued on a queue.
Parallel branches use `dbos.Go` and `dbos.Select`. `Event.branch` carries the
dotted agent path.

**Browser demo.** A root agent delegating to two sub-agents in parallel; the
workflow tree rendered from `HAS_STEP` and `SPAWNED` edges.

**Scenarios.** F18, F19.

**Definition of done.** As M5, plus parent/child traversal correct in the graph
and `ErrDepthExceeded` enforced at runtime as well as at resolution.

-----

### M7 — Streaming

**Goal.** Token-level streaming, end to end, in both modes.

**Public API.** `Subscription.runEvents` (§7.5).

**Execution model.** §9.4. `WriteStream` per chunk; `ClientReadStream` on the
read side; graphql-ws transport server-side, direct `syscall/js` callback in the
browser.

**Browser demo.** Live token rendering with no server; reconnect mid-stream and
resume exactly at `afterOffset`.

**Scenarios.** F16, F17.

**Definition of done.** As M6, plus stream retention job in place and duplicate
values provably suppressed by offset deduplication.

-----

## 21. Architecture Definition of Done

Repository-wide, evaluated at every milestone:

- Zero architecture lint violations, including the determinism analyzer.
- Zero generated-code drift.
- No `pgx` or `database/sql` import outside `generated/` and `wasmpg/`.
- No `syscall/js` import outside `wasmpg/` and `demo/wasm/`.
- No `oras-go` or epos import outside `artifact/`.
- No hand-written SQL anywhere.
- No hand-written GraphQL resolvers.
- All integration tests pass against `postgres:19beta2` via testcontainers.
- `session/sessiontestsuite` passes.
- Event round-trip is byte-identical (§7.2).
- The browser demo runs the production runtime against a real PostgreSQL 19
  engine, with no mocks and no fixtures.
- GitHub Pages branch deployment succeeds; PR preview succeeds.
- Every failure-matrix row has a passing automated test.
- Every artifact reference is digest-pinned; no tag resolution at runtime.
- `dbos.*` schema drift check passes against the pinned DBOS version.

-----

## 22. Decision Ledger

|#  |Decision                                                                                                |Rationale                                                                                                                                                                                                           |
|---|--------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
|D1 |DBOS owns orchestration; AgentIQ ships a DBOS↔ADK integration mirroring Temporal’s `contrib/googleadk`  |Postgres-only, no cluster; the Temporal plugin proves the granularity (loop as workflow, model and tools as steps)                                                                                                  |
|D2 |ADK sessions and events live in AgentIQ-owned tables written through DBOS DataSources                   |`RunAsTransaction` commits the event row and the step checkpoint together, giving exactly-once appends                                                                                                              |
|D3 |Events are normalized, not blobs                                                                        |Divergence from ADK Python, ADK Go and kagent, taken deliberately: the property graph and the sibling memory project need queryable parts                                                                           |
|D4 |`Part` is one wide table with nested structs as prefixed columns                                        |`genai.Part` is not a union; NULLs cost ~2 bytes; one heap fetch per part; TOAST handles large payloads; a 0..1 satellite is not a useful graph edge                                                                |
|D5 |Partial events go to `dbos.streams`, never the event table                                              |Matches ADK’s own contract, asserted by `sessiontestsuite`                                                                                                                                                          |
|D6 |Subscriptions read DBOS streams in both modes                                                           |One implementation; offset ordering makes reconnection exact                                                                                                                                                        |
|D7 |Browser runs a Go `net.Conn` shim over PGlite’s `execProtocol` with a multiplexer and real notifications|`execProtocol` preserves notification handling that the `Raw` variants bypass; pgx needs a `net.Conn`, not TCP                                                                                                      |
|D8 |M1 ships the browser path with a bare workflow                                                          |Highest-risk component first, with the smallest possible domain                                                                                                                                                     |
|D9 |Mutations map to `dbos.enqueue_workflow` and `dbos.send_message`                                        |The command surface becomes SQL, so no hand-written resolver layer exists                                                                                                                                           |
|D10|gopgql ships the required features first; M1 blocks on a tagged release                                 |Avoids forking or vendoring; keeps the capability where it belongs                                                                                                                                                  |
|D11|Single module, packages by concern                                                                      |Matches the CodiQ precedent; build tags separate the WASM transport                                                                                                                                                 |
|D12|Determinism analyzer plus depguard; no layering linter                                                  |DBOS has no `workflowcheck`; layering rules have nothing to bite on in a single module                                                                                                                              |
|D13|Agents are data, not code                                                                               |Enables the agent graph and browser-side editing                                                                                                                                                                    |
|D14|OCI is the source of truth; Postgres is a digest-keyed projection                                       |A digest is a stronger pin than a row version, and agent rows become read-only in the SDL                                                                                                                           |
|D15|A new runtime-agnostic agent artifact type, **owned by epos**                                           |ADS/OASF is discovery-oriented and docker-agent is a convention, so a new type was needed — but epos already owns OCI packaging, the store and the CLI, so a second packaging system next to it would be duplication|
|D16|Dependencies as an OCI image index with annotated descriptors; no lock file                             |The index is spec-defined, registry-visible, GC-safe, and already content-addressed                                                                                                                                 |
|D17|Tools are always references; scripts are just another artifact type                                     |One resolution path; script tools deduplicate across agents                                                                                                                                                         |
|D18|mcp-anything fronts all tools; the runtime speaks Streamable HTTP                                       |Uniform in both modes; credentials stay out of the artifact and the runtime                                                                                                                                         |
|D19|OpenRouter with PKCE in the browser                                                                     |`openaimodel` works unmodified; the demo ships no secret                                                                                                                                                            |
|D20|ADK HITL primitives with `dbos.Recv` as the durable wait                                                |ADK owns the semantics, DBOS owns the durability                                                                                                                                                                    |
|D21|AgentIQ consumes MCP, never serves it                                                                   |It is a workflow engine; serving MCP would reintroduce the shared-credential risk                                                                                                                                   |
|D22|A2A in scope; memory a sibling project; cron and analytics out                                          |Memory is a Spectron-class problem deserving its own design                                                                                                                                                         |
|D23|Milestone order M1–M7                                                                                   |Riskiest infrastructure first, external dependencies sequenced behind their consumers                                                                                                                               |
|D24|M2 seeds agent rows; the OCI pull lands in M3                                                           |The schema is permanent from M2, so no shim is discarded                                                                                                                                                            |
|D25|Instructions, model config, skills and sub-agents are all referenced artifacts                          |agentregistry independently deprecated inline model config in favour of refs                                                                                                                                        |
|D26|`TypeMeta`/`ObjectMeta`/`Spec`/`Status` envelope                                                        |Familiar, extensible, validated per kind                                                                                                                                                                            |
|D27|The artifact format lives in epos, which gains `agent`, `instruction`, `model` and `tool` kinds         |Reverses the earlier placement in the AgentIQ repo. Epos stays a CLI, store and proxy — it does not become a registry service like agentregistry                                                                    |
