# AgentIQ Engineering Specification

## 1. Vision

AgentIQ is a durable agentic workflow runtime.

The GraphQL SDL is the canonical domain model. Everything else is
generated from it.

    GraphQL SDL
        ├── gopgql → PostgreSQL schema + GraphQL + MCP
        ├── Go codegen → typed client
        ├── Browser demo
        └── Documentation

Technology stack:

-   Go (no CGO)
-   PostgreSQL 19
-   sql/pgq
-   gopgql
-   DBOS
-   Google ADK
-   Testcontainers-Go
-   golangci-lint
-   custom go/analysis analyzers
-   TinyGo + WASM
-   React
-   gaarutyunov/ui-kit
-   React Flow
-   ELK.js

Deployment:

-   GitHub Pages (branch mode)
-   GitHub Actions PR Preview
-   Browser demo built from production packages.

# Milestone 1 --- GraphQL Platform

## Goal

Deliver a releasable vertical slice.

## Repository layout

    schema/
    generated/
    runtime/
    workflow/
    execution/
    session/
    dbos/
    adk/
    demo/
    verify/
    test/

## Public SDL additions

-   Queries
-   Mutations
-   Subscriptions
-   SessionItem types

## Main packages

-   runtime
-   workflow
-   execution
-   generated/graphql
-   demo

## Main interfaces

``` go
type Runtime interface {
    ExecuteWorkflow(...)
    ResumeWorkflow(...)
}

type EventSink interface {
    Append(SessionItem)
}
```

## Sequence

    GraphQL mutation
          ↓
    Generated client
          ↓
    Runtime
          ↓
    SessionItems
          ↓
    GraphQL mutation

## Gherkin

``` gherkin
Feature: GraphQL Platform

Scenario: Happy path
 Given clean PostgreSQL
 When scenario executes
 Then workflow completes

Scenario: Restart
 Given running workflow
 When interruption occurs
 Then expected behaviour is observed
```

## Integration tests

-   PostgreSQL 19 (Testcontainers)
-   real gopgql
-   generated client
-   no mocks for persistence
-   browser fixtures mirror server fixtures

## Verification

-   make verify
-   go generate produces no diff
-   integration tests
-   browser demo
-   architecture lint
-   GitHub Pages preview

## Custom analyzers

-   forbid SQL
-   forbid pgx/sqlx
-   forbid ORM
-   layering rules
-   generated code current
-   runtime cannot import implementation packages
-   persistence only through generated client

## GitHub Actions

-   golangci-lint
-   architecture analyzer
-   go generate verification
-   integration tests
-   browser build
-   GitHub Pages branch deployment
-   PR Preview deployment

## Browser demo

Runs entirely in WASM:

    React
     ↓
    gaarutyunov/ui-kit
     ↓
    Generated GraphQL client
     ↓
    gopgql
     ↓
    pglite(sql/pgq)
     ↓
    AgentIQ runtime

No mocks.

Each milestone ships an executable scenario.

# Milestone 2 --- Workflow Engine

## Goal

Deliver a releasable vertical slice.

## Repository layout

    schema/
    generated/
    runtime/
    workflow/
    execution/
    session/
    dbos/
    adk/
    demo/
    verify/
    test/

## Public SDL additions

-   Queries
-   Mutations
-   Subscriptions
-   SessionItem types

## Main packages

-   runtime
-   workflow
-   execution
-   generated/graphql
-   demo

## Main interfaces

``` go
type Runtime interface {
    ExecuteWorkflow(...)
    ResumeWorkflow(...)
}

type EventSink interface {
    Append(SessionItem)
}
```

## Sequence

    GraphQL mutation
          ↓
    Generated client
          ↓
    Runtime
          ↓
    SessionItems
          ↓
    GraphQL mutation

## Gherkin

``` gherkin
Feature: Workflow Engine

Scenario: Happy path
 Given clean PostgreSQL
 When scenario executes
 Then workflow completes

Scenario: Restart
 Given running workflow
 When interruption occurs
 Then expected behaviour is observed
```

## Integration tests

-   PostgreSQL 19 (Testcontainers)
-   real gopgql
-   generated client
-   no mocks for persistence
-   browser fixtures mirror server fixtures

## Verification

-   make verify
-   go generate produces no diff
-   integration tests
-   browser demo
-   architecture lint
-   GitHub Pages preview

## Custom analyzers

-   forbid SQL
-   forbid pgx/sqlx
-   forbid ORM
-   layering rules
-   generated code current
-   runtime cannot import implementation packages
-   persistence only through generated client

## GitHub Actions

-   golangci-lint
-   architecture analyzer
-   go generate verification
-   integration tests
-   browser build
-   GitHub Pages branch deployment
-   PR Preview deployment

## Browser demo

Runs entirely in WASM:

    React
     ↓
    gaarutyunov/ui-kit
     ↓
    Generated GraphQL client
     ↓
    gopgql
     ↓
    pglite(sql/pgq)
     ↓
    AgentIQ runtime

No mocks.

Each milestone ships an executable scenario.

# Milestone 3 --- DBOS Durability

## Goal

Deliver a releasable vertical slice.

## Repository layout

    schema/
    generated/
    runtime/
    workflow/
    execution/
    session/
    dbos/
    adk/
    demo/
    verify/
    test/

## Public SDL additions

-   Queries
-   Mutations
-   Subscriptions
-   SessionItem types

## Main packages

-   runtime
-   workflow
-   execution
-   generated/graphql
-   demo

## Main interfaces

``` go
type Runtime interface {
    ExecuteWorkflow(...)
    ResumeWorkflow(...)
}

type EventSink interface {
    Append(SessionItem)
}
```

## Sequence

    GraphQL mutation
          ↓
    Generated client
          ↓
    Runtime
          ↓
    SessionItems
          ↓
    GraphQL mutation

## Gherkin

``` gherkin
Feature: DBOS Durability

Scenario: Happy path
 Given clean PostgreSQL
 When scenario executes
 Then workflow completes

Scenario: Restart
 Given running workflow
 When interruption occurs
 Then expected behaviour is observed
```

## Integration tests

-   PostgreSQL 19 (Testcontainers)
-   real gopgql
-   generated client
-   no mocks for persistence
-   browser fixtures mirror server fixtures

## Verification

-   make verify
-   go generate produces no diff
-   integration tests
-   browser demo
-   architecture lint
-   GitHub Pages preview

## Custom analyzers

-   forbid SQL
-   forbid pgx/sqlx
-   forbid ORM
-   layering rules
-   generated code current
-   runtime cannot import implementation packages
-   persistence only through generated client

## GitHub Actions

-   golangci-lint
-   architecture analyzer
-   go generate verification
-   integration tests
-   browser build
-   GitHub Pages branch deployment
-   PR Preview deployment

## Browser demo

Runs entirely in WASM:

    React
     ↓
    gaarutyunov/ui-kit
     ↓
    Generated GraphQL client
     ↓
    gopgql
     ↓
    pglite(sql/pgq)
     ↓
    AgentIQ runtime

No mocks.

Each milestone ships an executable scenario.

# Milestone 4 --- Streaming

## Goal

Deliver a releasable vertical slice.

## Repository layout

    schema/
    generated/
    runtime/
    workflow/
    execution/
    session/
    dbos/
    adk/
    demo/
    verify/
    test/

## Public SDL additions

-   Queries
-   Mutations
-   Subscriptions
-   SessionItem types

## Main packages

-   runtime
-   workflow
-   execution
-   generated/graphql
-   demo

## Main interfaces

``` go
type Runtime interface {
    ExecuteWorkflow(...)
    ResumeWorkflow(...)
}

type EventSink interface {
    Append(SessionItem)
}
```

## Sequence

    GraphQL mutation
          ↓
    Generated client
          ↓
    Runtime
          ↓
    SessionItems
          ↓
    GraphQL mutation

## Gherkin

``` gherkin
Feature: Streaming

Scenario: Happy path
 Given clean PostgreSQL
 When scenario executes
 Then workflow completes

Scenario: Restart
 Given running workflow
 When interruption occurs
 Then expected behaviour is observed
```

## Integration tests

-   PostgreSQL 19 (Testcontainers)
-   real gopgql
-   generated client
-   no mocks for persistence
-   browser fixtures mirror server fixtures

## Verification

-   make verify
-   go generate produces no diff
-   integration tests
-   browser demo
-   architecture lint
-   GitHub Pages preview

## Custom analyzers

-   forbid SQL
-   forbid pgx/sqlx
-   forbid ORM
-   layering rules
-   generated code current
-   runtime cannot import implementation packages
-   persistence only through generated client

## GitHub Actions

-   golangci-lint
-   architecture analyzer
-   go generate verification
-   integration tests
-   browser build
-   GitHub Pages branch deployment
-   PR Preview deployment

## Browser demo

Runs entirely in WASM:

    React
     ↓
    gaarutyunov/ui-kit
     ↓
    Generated GraphQL client
     ↓
    gopgql
     ↓
    pglite(sql/pgq)
     ↓
    AgentIQ runtime

No mocks.

Each milestone ships an executable scenario.

# Milestone 5 --- Human Approval

## Goal

Deliver a releasable vertical slice.

## Repository layout

    schema/
    generated/
    runtime/
    workflow/
    execution/
    session/
    dbos/
    adk/
    demo/
    verify/
    test/

## Public SDL additions

-   Queries
-   Mutations
-   Subscriptions
-   SessionItem types

## Main packages

-   runtime
-   workflow
-   execution
-   generated/graphql
-   demo

## Main interfaces

``` go
type Runtime interface {
    ExecuteWorkflow(...)
    ResumeWorkflow(...)
}

type EventSink interface {
    Append(SessionItem)
}
```

## Sequence

    GraphQL mutation
          ↓
    Generated client
          ↓
    Runtime
          ↓
    SessionItems
          ↓
    GraphQL mutation

## Gherkin

``` gherkin
Feature: Human Approval

Scenario: Happy path
 Given clean PostgreSQL
 When scenario executes
 Then workflow completes

Scenario: Restart
 Given running workflow
 When interruption occurs
 Then expected behaviour is observed
```

## Integration tests

-   PostgreSQL 19 (Testcontainers)
-   real gopgql
-   generated client
-   no mocks for persistence
-   browser fixtures mirror server fixtures

## Verification

-   make verify
-   go generate produces no diff
-   integration tests
-   browser demo
-   architecture lint
-   GitHub Pages preview

## Custom analyzers

-   forbid SQL
-   forbid pgx/sqlx
-   forbid ORM
-   layering rules
-   generated code current
-   runtime cannot import implementation packages
-   persistence only through generated client

## GitHub Actions

-   golangci-lint
-   architecture analyzer
-   go generate verification
-   integration tests
-   browser build
-   GitHub Pages branch deployment
-   PR Preview deployment

## Browser demo

Runs entirely in WASM:

    React
     ↓
    gaarutyunov/ui-kit
     ↓
    Generated GraphQL client
     ↓
    gopgql
     ↓
    pglite(sql/pgq)
     ↓
    AgentIQ runtime

No mocks.

Each milestone ships an executable scenario.

# Milestone 6 --- Tool Providers

## Goal

Deliver a releasable vertical slice.

## Repository layout

    schema/
    generated/
    runtime/
    workflow/
    execution/
    session/
    dbos/
    adk/
    demo/
    verify/
    test/

## Public SDL additions

-   Queries
-   Mutations
-   Subscriptions
-   SessionItem types

## Main packages

-   runtime
-   workflow
-   execution
-   generated/graphql
-   demo

## Main interfaces

``` go
type Runtime interface {
    ExecuteWorkflow(...)
    ResumeWorkflow(...)
}

type EventSink interface {
    Append(SessionItem)
}
```

## Sequence

    GraphQL mutation
          ↓
    Generated client
          ↓
    Runtime
          ↓
    SessionItems
          ↓
    GraphQL mutation

## Gherkin

``` gherkin
Feature: Tool Providers

Scenario: Happy path
 Given clean PostgreSQL
 When scenario executes
 Then workflow completes

Scenario: Restart
 Given running workflow
 When interruption occurs
 Then expected behaviour is observed
```

## Integration tests

-   PostgreSQL 19 (Testcontainers)
-   real gopgql
-   generated client
-   no mocks for persistence
-   browser fixtures mirror server fixtures

## Verification

-   make verify
-   go generate produces no diff
-   integration tests
-   browser demo
-   architecture lint
-   GitHub Pages preview

## Custom analyzers

-   forbid SQL
-   forbid pgx/sqlx
-   forbid ORM
-   layering rules
-   generated code current
-   runtime cannot import implementation packages
-   persistence only through generated client

## GitHub Actions

-   golangci-lint
-   architecture analyzer
-   go generate verification
-   integration tests
-   browser build
-   GitHub Pages branch deployment
-   PR Preview deployment

## Browser demo

Runs entirely in WASM:

    React
     ↓
    gaarutyunov/ui-kit
     ↓
    Generated GraphQL client
     ↓
    gopgql
     ↓
    pglite(sql/pgq)
     ↓
    AgentIQ runtime

No mocks.

Each milestone ships an executable scenario.

# Milestone 7 --- Scheduler

## Goal

Deliver a releasable vertical slice.

## Repository layout

    schema/
    generated/
    runtime/
    workflow/
    execution/
    session/
    dbos/
    adk/
    demo/
    verify/
    test/

## Public SDL additions

-   Queries
-   Mutations
-   Subscriptions
-   SessionItem types

## Main packages

-   runtime
-   workflow
-   execution
-   generated/graphql
-   demo

## Main interfaces

``` go
type Runtime interface {
    ExecuteWorkflow(...)
    ResumeWorkflow(...)
}

type EventSink interface {
    Append(SessionItem)
}
```

## Sequence

    GraphQL mutation
          ↓
    Generated client
          ↓
    Runtime
          ↓
    SessionItems
          ↓
    GraphQL mutation

## Gherkin

``` gherkin
Feature: Scheduler

Scenario: Happy path
 Given clean PostgreSQL
 When scenario executes
 Then workflow completes

Scenario: Restart
 Given running workflow
 When interruption occurs
 Then expected behaviour is observed
```

## Integration tests

-   PostgreSQL 19 (Testcontainers)
-   real gopgql
-   generated client
-   no mocks for persistence
-   browser fixtures mirror server fixtures

## Verification

-   make verify
-   go generate produces no diff
-   integration tests
-   browser demo
-   architecture lint
-   GitHub Pages preview

## Custom analyzers

-   forbid SQL
-   forbid pgx/sqlx
-   forbid ORM
-   layering rules
-   generated code current
-   runtime cannot import implementation packages
-   persistence only through generated client

## GitHub Actions

-   golangci-lint
-   architecture analyzer
-   go generate verification
-   integration tests
-   browser build
-   GitHub Pages branch deployment
-   PR Preview deployment

## Browser demo

Runs entirely in WASM:

    React
     ↓
    gaarutyunov/ui-kit
     ↓
    Generated GraphQL client
     ↓
    gopgql
     ↓
    pglite(sql/pgq)
     ↓
    AgentIQ runtime

No mocks.

Each milestone ships an executable scenario.

# Milestone 8 --- Analytics

## Goal

Deliver a releasable vertical slice.

## Repository layout

    schema/
    generated/
    runtime/
    workflow/
    execution/
    session/
    dbos/
    adk/
    demo/
    verify/
    test/

## Public SDL additions

-   Queries
-   Mutations
-   Subscriptions
-   SessionItem types

## Main packages

-   runtime
-   workflow
-   execution
-   generated/graphql
-   demo

## Main interfaces

``` go
type Runtime interface {
    ExecuteWorkflow(...)
    ResumeWorkflow(...)
}

type EventSink interface {
    Append(SessionItem)
}
```

## Sequence

    GraphQL mutation
          ↓
    Generated client
          ↓
    Runtime
          ↓
    SessionItems
          ↓
    GraphQL mutation

## Gherkin

``` gherkin
Feature: Analytics

Scenario: Happy path
 Given clean PostgreSQL
 When scenario executes
 Then workflow completes

Scenario: Restart
 Given running workflow
 When interruption occurs
 Then expected behaviour is observed
```

## Integration tests

-   PostgreSQL 19 (Testcontainers)
-   real gopgql
-   generated client
-   no mocks for persistence
-   browser fixtures mirror server fixtures

## Verification

-   make verify
-   go generate produces no diff
-   integration tests
-   browser demo
-   architecture lint
-   GitHub Pages preview

## Custom analyzers

-   forbid SQL
-   forbid pgx/sqlx
-   forbid ORM
-   layering rules
-   generated code current
-   runtime cannot import implementation packages
-   persistence only through generated client

## GitHub Actions

-   golangci-lint
-   architecture analyzer
-   go generate verification
-   integration tests
-   browser build
-   GitHub Pages branch deployment
-   PR Preview deployment

## Browser demo

Runs entirely in WASM:

    React
     ↓
    gaarutyunov/ui-kit
     ↓
    Generated GraphQL client
     ↓
    gopgql
     ↓
    pglite(sql/pgq)
     ↓
    AgentIQ runtime

No mocks.

Each milestone ships an executable scenario.

# Failure Matrix

Every durability feature must include executable scenarios:

-   LLM returns 529
-   Tool panics
-   Worker process killed
-   PostgreSQL restart
-   Network timeout
-   Approval waits for days
-   Parallel branch failure
-   Child workflow failure
-   Retry exhausted

Each scenario verifies:

-   SessionItem ordering
-   Final workflow state
-   DBOS recovery
-   GraphQL query results
-   Browser replay

# Architecture Definition of Done

Every milestone MUST satisfy:

-   Zero architecture lint violations.
-   Zero generated-code drift.
-   No SQL outside generated code.
-   No persistence except generated GraphQL client.
-   Integration tests use PostgreSQL 19 via Testcontainers.
-   Browser demo uses real gopgql and pglite(sql/pgq).
-   GitHub Pages branch deployment succeeds.
-   PR preview succeeds.
-   Gherkin scenarios execute automatically.
-   Public SDL is the only domain model.

# Long-term Code Structure

    schema/
    generated/
    runtime/
    workflow/
    execution/
    session/
    dbos/
    adk/
    tools/
    demo/
    verify/
    contracts/
    .github/

Contracts are executable and versioned:

-   api
-   architecture
-   execution
-   durability
-   compatibility
-   performance
-   browser-demo
