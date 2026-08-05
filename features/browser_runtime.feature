# SPEC.md §14.3: this file is canonical. The Gherkin in SPEC.md §15 is an
# illustrative extract of it, not its source.
#
# Everything here runs against the *deployed* page (SPEC.md §14.4) — the Pages
# production URL or a PR preview under /pr-preview/pr-N/. There is no local
# server: what M1 has to prove is that the real runtime works where it is
# actually served, and the constraints that make the browser hard (no
# SharedArrayBuffer, relative asset paths, IndexedDB persistence) are
# properties of the deployment, not of the code.

@browser
Feature: Browser runtime
  The same Go — real DBOS, real pgx, the real generated client — compiled to
  js/wasm and talking to a real PostgreSQL 19 engine through wasmpg's net.Conn
  shim over PGlite's execProtocol. Nothing here is a mock; if a scenario passes,
  the shim, the synthesised handshake, the multiplexer and the notification
  routing all work against the real bundle.

  Background:
    Given the deployed demo page

  # SPEC.md §15 Feature: Browser runtime, verbatim in substance. SPEC.md §16's
  # M1 acceptance adds the second half: the GRAPH_TABLE query must answer in
  # the browser build, not only the server build, so the step count is asserted
  # rather than just the status.
  Scenario: The demo runs the real runtime against real Postgres
    When a workflow is started from the browser
    Then dbos.workflow_status contains one row with status SUCCESS
    And the property graph returns the workflow with its steps

  # SPEC.md §19 F20. The point is not that invalid SQL fails — it is that the
  # backend's ErrorResponse frame survives the trip back through execProtocol,
  # the multiplexer and the shim's Read, and arrives at the caller as a pgx
  # error carrying the SQLSTATE. A shim that swallowed the frame would surface
  # this as a hang or an EOF instead.
  Scenario: A backend error mid-transaction surfaces as a pgx error
    When invalid SQL is executed from the browser
    Then the page reports a pgx error
    And the error carries a SQLSTATE

  # SPEC.md §19 F21. DBOS uses LISTEN/NOTIFY to wake its queue runner, and
  # falls back to polling when a notification never arrives. Suppressing
  # onNotification removes the fast path; the queue must still advance, because
  # a demo that only works when every notification is delivered is a demo that
  # stalls the first time one is not.
  Scenario: Notifications are suppressed and the queue still advances
    Given the PGlite notification callback is suppressed
    When a workflow is started from the browser
    Then dbos.workflow_status contains one row with status SUCCESS

  # SPEC.md §19 F22. Persistence is IndexedDB, not OPFS: GitHub Pages cannot
  # set COOP/COEP, so there is no SharedArrayBuffer (SPEC.md §3.3, §12.6,
  # §13.3). The reload has to find the same database, which is also why
  # demo.GraphMigrations applies Down-then-Up on every boot.
  Scenario: The tab is reloaded mid-run and the workflow resumes
    When a workflow is started from the browser
    And the tab is reloaded before the workflow completes
    Then the workflow is still known after the reload
    And dbos.workflow_status contains one row with status SUCCESS

  # Not a failure-matrix row: the open question wasmpg.Config.RouteInlineNotifications
  # exists for. wasmpg drops NotificationResponse frames found inside an
  # execProtocol result, on the assumption that PGlite also dispatches them to
  # onNotification. If that assumption is wrong, notifications are lost silently
  # and only F21's polling fallback keeps the demo alive — a demo that works for
  # the wrong reason.
  #
  # The probe runs against a second, throwaway in-memory PGlite so it cannot
  # disturb the demo's own database. It is an assertion, not a diagnostic: it
  # fails when the shipped default contradicts what the real bundle does.
  Scenario: Inline notification routing matches the shipped default
    Given a throwaway in-memory PGlite instance
    When a notification is raised on a channel that instance is listening to
    Then the onNotification callback receives it
    And wasmpg.Config.RouteInlineNotifications is correct for that behaviour
