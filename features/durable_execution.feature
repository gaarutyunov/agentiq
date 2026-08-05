# SPEC.md §14.3: this file is canonical. The Gherkin in SPEC.md §15 is an
# illustrative extract of it, not its source. It is hand-authored, never
# generated, and never duplicated into a Go string literal — the step
# definitions in test/durable bind to these sentences, they do not restate
# them.
#
# Where a scenario below reads differently from SPEC.md §15 or §19, the
# difference is deliberate and is called out in a comment above the scenario.
# A feature file that asserts behaviour the pinned runtime does not have is a
# test that can only ever be skipped.

@integration
Feature: Durable workflow execution
  AgentIQ's durability is DBOS's durability. M1 owns no tables and runs no
  agent, so what these scenarios prove is that the step boundaries in
  workflow.AgentRun survive the three things that kill a worker in production:
  the process dying, the database restarting, and the network between them
  going away.

  Background:
    Given a clean PostgreSQL 19 database
    And a registered workflow with two steps separated by a durable sleep

  # SPEC.md §19 F1. SPEC.md §15 says "three steps"; workflow.AgentRun has two
  # steps — resolveAgent and completeRun — separated by a dbos.Sleep. Two steps
  # and a checkpointed sleep is what the property needs: one step to complete
  # before the kill, one to complete after it, and a window wide enough that
  # the kill lands between checkpoints rather than inside one.
  Scenario: Worker killed mid-workflow resumes from the last checkpoint
    Given a running worker process
    When a run is enqueued
    And the workflow completes step one
    And the worker process is killed
    And a new worker starts
    Then step one is not re-executed
    And the workflow completes successfully

  # SPEC.md §19 F2.
  Scenario: PostgreSQL restarts under a running workflow
    Given a running worker process
    When a run is enqueued
    And the workflow completes step one
    And the database container is stopped and restarted
    Then the workflow resumes without duplicating completed steps
    And the workflow completes successfully

  # SPEC.md §19 F3. "Retries; workflow eventually resumes" — the worker is
  # never killed here. It keeps running with its connections cut, which is what
  # separates a partition from a crash: nothing recovers the workflow, the same
  # process picks it back up once the packets flow again.
  Scenario: The network to the database is partitioned and healed
    Given a running worker process connected through a network proxy
    When a run is enqueued
    And the workflow completes step one
    And the connection to the database is cut for 5 seconds
    Then the workflow completes successfully
    And step one is not re-executed

  # SPEC.md §19 F4, corrected against the pinned runtime.
  #
  # §19 expects `workflow_status` → RETRIES_EXCEEDED. DBOS Go v1.0.0 has no
  # such status: its enum is PENDING, ENQUEUED, DELAYED, SUCCESS, ERROR,
  # CANCELLED, MAX_RECOVERY_ATTEMPTS_EXCEEDED (internal/models/workflow_status.go).
  # RETRIES_EXCEEDED is a DBOS Python/TypeScript status. A step whose error is
  # permanent — whether or not it had retries left — lands the workflow in
  # ERROR with the error recorded, and that is what is asserted.
  Scenario: A step returns a permanent error
    Given a running worker process
    When a run with no agent digest is enqueued
    Then the workflow status becomes ERROR
    And the workflow error is recorded
    And no step completed successfully

  # SPEC.md §19 F5.
  #
  # The kill loop is compressed rather than run 100 times. DBOS dead-letters a
  # workflow when its recovery_attempts exceeds the registered maximum plus one
  # (internal/sysdb/system_database.go), and the registered maximum is the
  # default 100. Driving that with 100 real worker restarts costs more than the
  # 20-minute integration budget allows and proves nothing the counter does not
  # already prove, so the counter is seeded and one real kill supplies the
  # recovery that trips it. §19 F13 compresses a timeout for the same reason.
  Scenario: Recovery attempts are exhausted
    Given a running worker process
    When a run is enqueued
    And the workflow completes step one
    And the recovery attempt counter is seeded to its limit
    And the worker process is killed
    And a new worker starts
    Then the workflow status becomes MAX_RECOVERY_ATTEMPTS_EXCEEDED
    And the workflow is no longer on a queue

  # SPEC.md §16's M1 acceptance, server half: "a GRAPH_TABLE query over dbos.*
  # returns a workflow with its steps in both the server build and the browser
  # build". The browser half is in browser_runtime.feature. The query is the
  # generated client's, compiled from schema/operations/workflow_with_steps.graphql
  # — no hand-written SQL (SPEC.md §21).
  #
  # @blocked-gopgql: this scenario CANNOT pass with the pinned gopgql v0.2.1,
  # and the reason is upstream, not here. gopgql's shaper canonicalises every
  # `Int` to a json.Number (shape/canonical.go, normaliseInt) while the client
  # it generates decodes with a gopgqlAsInt64 that accepts only int/int16/
  # int32/int64. Every Int field in every generated client is therefore
  # undecodable, and this traversal has four of them — including
  # `Step.functionId`, which is a key column and cannot be typed around:
  #
  #   gopgql: workflow_status[0].createdAt: cannot read json.Number as int64
  #
  # The scenario stays here because it *is* the acceptance criterion, and
  # deleting it would make the milestone look complete. It is excluded from the
  # suite's tag filter until a gopgql release fixes the decoder; the exclusion
  # is one word in test/durable/durable_test.go and nothing else has to change.
  @blocked-gopgql
  Scenario: The property graph returns the workflow with its steps
    Given a running worker process
    And the generated property graph is applied
    When a run is enqueued
    And the workflow completes successfully
    Then the property graph returns the workflow with steps resolveAgent and completeRun
    And the property graph reports the workflow status as SUCCESS
