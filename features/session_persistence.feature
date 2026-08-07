# SPEC.md §15, Feature: Session persistence.
#
# The two scenarios below are M2's acceptance, verbatim from §15. Both are
# statements about what is in the database after a turn, which is the point:
# events are normalized rows and not blobs (D3), so "one event in the session"
# and "five values in the stream" are counts a query can produce.
#
# The step definitions live in test/session (feature_test.go), which runs the
# real `workflow.AgentRun` against a real `postgres:19beta2` and a stub
# OpenRouter endpoint.

Feature: Session persistence
  AgentIQ stores an ADK session as normalized rows in the tables it owns, so
  that the property graph and the sibling memory project can query the parts of
  an event rather than parse a blob.

  Background:
    Given a running AgentIQ worker
    And an agent seeded by the fixture migration

  @session
  Scenario: Partial events are never stored
    # D5 and §9.4. Every other ADK implementation drops partial events on
    # append, and `sessiontestsuite` asserts it, so this is a conformance
    # requirement rather than a design preference.
    #
    # The stream assertion is why `dbos.WriteStream` is M2 work and not M7's.
    # §20 calls M2 "no streaming" and puts `Subscription.runEvents` in M7 — but
    # M7 adds the *read* side. The write side has to exist for this scenario to
    # have five values to count.
    Given the model produces five partial responses and one final response
    When the agent runs one turn
    Then the session contains exactly one event
    And the DBOS stream contains five values

  @session
  Scenario: Event round-trip is byte-identical
    # §7.2's conformance property, at the level of a whole turn:
    #
    #   for any session.Event E, json.Marshal(load(store(E))) == json.Marshal(E)
    #
    # The mapping half of this property is already proven without a database by
    # session.TestEventRoundTripIsByteIdentical, over the crafted corpus §15
    # requires — text, a thought signature and a function call. This scenario is
    # the same corpus through the real tables, which is what makes the column
    # types part of the assertion: a `jsonb` column, a lost `part_index` or a
    # dropped NULL all fail here.
    Given an event with text, a thought signature and a function call
    When the event is appended and the session is reloaded
    Then the reloaded event marshals identically to the original

  @session
  Scenario: temp: state is dropped on write and absent on read
    # §7.2 rule 4, the one intentional deviation from round-trip fidelity. The
    # M2 acceptance asks for it to be asserted explicitly rather than left
    # implicit in the scenario above, because a deviation nothing names is
    # indistinguishable from a bug.
    Given an event whose state delta sets "app:theme", "user:name", "turns" and "temp:scratch"
    When the event is appended and the session is reloaded
    Then the session state contains "app:theme", "user:name" and "turns"
    And the session state does not contain "temp:scratch"
    And no state row carries the scope "temp"

  @session
  Scenario: The model returns HTTP 529
    # Failure matrix row F6. The step retry policy is §9.2's spec'd numbers —
    # 5 attempts at base 1s, exponential — not DBOS's defaults, which are zero
    # retries at 100ms.
    Given a stub OpenRouter endpoint that returns HTTP 529
    When the agent runs one turn
    Then the model step is retried with exponential backoff
    And the final failure surfaces as an event with an errorCode

  @session
  Scenario: The model returns malformed JSON
    # Failure matrix row F7.
    Given a stub OpenRouter endpoint that returns malformed JSON
    When the agent runs one turn
    Then the model step errors and is retried
    And the session records the error event
