//go:build browser

package browser

import (
	"context"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/cucumber/godog"
	"github.com/stretchr/testify/assert"

	"github.com/gaarutyunov/agentiq/wasmpg"
)

const (
	// bootTimeout bounds a cold load. Measured against the deployed preview on
	// an idle machine it is just under three minutes with an empty HTTP cache —
	// 15 MB of wasm and data images, then initdb, the dbos migrations, the
	// property graph and the notification probe — and about twelve seconds once
	// the browser has the assets. The suite shares one browser, so only the
	// first scenario pays the cold price.
	//
	// The bound is nearly three times the measurement because every part of
	// that is elastic: the download is somebody else's CDN, and instantiating
	// the wasm module and running initdb are CPU the runner shares with
	// whatever else is on it. A bound set close to the measured time turns a
	// busy machine into a red suite, which is the one failure mode a browser
	// test must not have — it is expensive to re-run and impossible to trust
	// once it has cried wolf.
	bootTimeout = 8 * time.Minute

	// workflowTimeout bounds a run from enqueue to a terminal status. The
	// workflow is two steps around a two-second durable sleep and finishes in
	// well under a minute; the slack is for a queue runner competing with the
	// refresh loop for one PGlite backend.
	workflowTimeout = 3 * time.Minute

	// evalTimeout bounds one round trip to the tab. It is generous because the
	// Go runtime and the page share a thread: an Evaluate issued while the wasm
	// module is doing something long waits for it.
	evalTimeout = 45 * time.Second

	// pollInterval is how often the page's state is re-read. The runtime
	// republishes it on every change, so this only decides how quickly the
	// suite notices.
	pollInterval = 500 * time.Millisecond
)

// sqlstatePattern is the shape of a SQLSTATE: five characters, digits and
// upper-case letters.
const sqlstatePattern = `^[0-9A-Z]{5}$`

// suite is the browser scenario state.
type suite struct {
	target  string
	browser context.Context

	tab       context.Context
	cancelTab context.CancelFunc

	// workflowID is the run this scenario started. Every scenario shares one
	// origin and therefore one IndexedDB database, so the page's workflow list
	// is cumulative and an assertion has to name the row it means.
	workflowID string
}

func (s *suite) before(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
	s.tab, s.cancelTab = chromedp.NewContext(s.browser)
	s.workflowID = ""
	return ctx, nil
}

func (s *suite) after(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
	if s.cancelTab != nil {
		s.cancelTab()
		s.cancelTab = nil
	}
	return ctx, nil
}

// --- Given -----------------------------------------------------------------

// theDeployedPage loads the target and waits for the runtime to boot.
func (s *suite) theDeployedPage() error { return s.open() }

// suppressNotifications removes PGlite's notification callback before the
// runtime subscribes, which is failure-matrix row F21's mechanism.
//
// It is installed with Page.addScriptToEvaluateOnNewDocument, which runs before
// any of the document's own scripts. That ordering is the whole point: the wasm
// module registers its callback during startup, and a patch applied afterwards
// would leave the registration in place and suppress nothing. Patching after
// load and then reloading does not work either — a reload builds a new global
// object, and boot.js reinstates an unpatched window.agentiq on it.
//
// The patch is a property setter rather than an assignment, because boot.js
// creates window.agentiq itself and there is nothing to wrap until it does.
func (s *suite) suppressNotifications() error {
	ctx, cancel := context.WithTimeout(s.tab, evalTimeout)
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(suppressJS).Do(c)
		return err
	})); err != nil {
		cancel()
		return fmt.Errorf("install the notification suppressor: %w", err)
	}
	cancel()

	if err := s.open(); err != nil {
		return err
	}

	// The suppression has to be real, or the scenario is a second copy of the
	// first one wearing its name. The page's own probe says whether it was: the
	// bundle still exposes onNotification — that is not what was removed — and
	// the callback never fired.
	st, err := s.readState()
	if err != nil {
		return err
	}
	if !st.Probe.Ran {
		return fmt.Errorf("the page's notification probe did not run, so the suppression cannot be "+
			"confirmed and this scenario would assert nothing: %s", st.Probe.Verdict)
	}
	if st.Probe.CallbackHits != 0 {
		return fmt.Errorf("onNotification fired %d time(s) with the suppressor installed; it did not "+
			"take, and the queue would be advancing on the fast path this scenario exists to remove",
			st.Probe.CallbackHits)
	}
	return nil
}

// theProbeHasRun asserts the page ran its notification probe and reports it.
//
// The probe is demo/wasm/probe.go, run at boot before dbos.Launch. Nothing has
// to be triggered here: the measurement is already in the state element by the
// time the page is ready.
func (s *suite) theProbeHasRun() error {
	st, err := s.readState()
	if err != nil {
		return err
	}
	if !st.Probe.Ran {
		return fmt.Errorf("the page's notification probe did not run (%s), so nothing below can say "+
			"what the real PGlite bundle does with a NOTIFY: %s", st.Probe.Err, st.Probe.Verdict)
	}
	return nil
}

// --- When ------------------------------------------------------------------

// aWorkflowIsStarted presses Start and waits for the run to appear.
//
// The new run is identified as the difference against the list before the
// click, not as the first row: the page keeps its database across scenarios and
// across page loads, which is the point of failure-matrix row F22.
func (s *suite) aWorkflowIsStarted() error {
	before, err := s.readState()
	if err != nil {
		return err
	}
	known := before.ids()

	if err := s.click(selStart); err != nil {
		return err
	}

	st, err := s.awaitState(workflowTimeout, "the enqueued workflow to appear in the page's list",
		func(p pageState) bool {
			for _, w := range p.Workflows {
				if !known[w.ID] {
					return true
				}
			}
			return false
		})
	if err != nil {
		return err
	}

	for _, w := range st.Workflows {
		if !known[w.ID] {
			s.workflowID = w.ID
			return nil
		}
	}
	return fmt.Errorf("the page listed a new workflow and then stopped listing it: %s", describe(st))
}

// invalidSQLIsExecuted presses Run invalid SQL and waits for the page to report
// what came back.
func (s *suite) invalidSQLIsExecuted() error {
	if err := s.click(selBadSQL); err != nil {
		return err
	}
	_, err := s.awaitState(workflowTimeout, "the page to report the result of the invalid statement",
		func(p pageState) bool { return p.ShimError != nil })
	return err
}

// theTabIsReloaded is failure-matrix row F22.
//
// The reload has to land while the run is still in flight, or the scenario
// proves only that a finished workflow is still listed afterwards. The
// workflow's two steps are separated by a two-second durable sleep and the
// reload is issued as soon as the run is enqueued, so the window is wide; the
// step checks it rather than assuming it, because a scenario that silently
// stopped testing what it is named after is worse than one that fails.
func (s *suite) theTabIsReloaded() error {
	st, err := s.readState()
	if err != nil {
		return err
	}
	row, ok := st.find(s.workflowID)
	if !ok {
		return fmt.Errorf("workflow %s is not in the page's list before the reload: %s",
			s.workflowID, describe(st))
	}
	if terminal(row.Status) {
		return fmt.Errorf("workflow %s already reached %s before the tab could be reloaded; this "+
			"scenario needs the reload to land mid-run and did not get its window",
			s.workflowID, row.Status)
	}

	return s.reopen()
}

// --- Then ------------------------------------------------------------------

// oneRowWithStatusSuccess waits for this scenario's run to reach SUCCESS.
//
// "one row" is the row for the workflow this scenario started. The page's list
// is DBOS's own, over a database that survives every reload and every scenario,
// so an assertion about "the" row has to name which one it means.
func (s *suite) oneRowWithStatusSuccess() error {
	if s.workflowID == "" {
		return fmt.Errorf("no workflow was started, so there is no row to assert on")
	}

	st, err := s.awaitState(workflowTimeout, fmt.Sprintf("workflow %s to reach SUCCESS", s.workflowID),
		func(p pageState) bool {
			row, ok := p.find(s.workflowID)
			return ok && terminal(row.Status)
		})
	if err != nil {
		return err
	}

	row, ok := st.find(s.workflowID)
	if !ok {
		return fmt.Errorf("workflow %s vanished from the page's list: %s", s.workflowID, describe(st))
	}

	a := &assertions{}
	assert.Equal(a, "SUCCESS", row.Status,
		"dbos.workflow_status has workflow %s at %s in the browser build\n%s",
		s.workflowID, row.Status, describe(st))
	return a.err()
}

// theGraphReturnsSteps is the GRAPH_TABLE half of SPEC.md §16's M1 acceptance:
// the property graph has to answer in the browser build, not only the server
// build.
//
// The steps come from the generated traversal over dbos.*, which is a different
// read from the workflow list above and can fail on its own — demo/wasm/main.go
// records that on the row as stepsError rather than losing the row with it. So
// both are asserted: an empty step list with an error explains itself, an empty
// step list without one is the graph saying this workflow has no steps.
func (s *suite) theGraphReturnsSteps() error {
	st, err := s.readState()
	if err != nil {
		return err
	}
	row, ok := st.find(s.workflowID)
	if !ok {
		return fmt.Errorf("workflow %s is not in the page's list: %s", s.workflowID, describe(st))
	}

	a := &assertions{}
	assert.Empty(a, row.StepsError,
		"the property-graph traversal for workflow %s failed, so the graph could not be asked what "+
			"steps it has\n%s", s.workflowID, describe(st))
	assert.ElementsMatch(a, []string{"resolveAgent", "completeRun"}, row.stepNames(),
		"the property graph returned %d step(s) for workflow %s; workflow.AgentRun has resolveAgent "+
			"and completeRun\n%s", len(row.Steps), s.workflowID, describe(st))
	return a.err()
}

// thePageReportsAPgxError and theErrorCarriesASQLSTATE are failure-matrix row
// F20, split the way the feature file splits it.
//
// The row is about the transport, not about invalid SQL failing. What has to
// survive execProtocol, the multiplexer and the shim's Read is the backend's
// ErrorResponse frame, arriving at pgx intact enough to decode into a
// *pgconn.PgError. An error that reaches the caller as a plain string — as a
// write that failed, or an EOF, or a hang — is the failure this row exists to
// catch, and the page reports which of the two happened as isPgError.
func (s *suite) thePageReportsAPgxError() error {
	st, err := s.readState()
	if err != nil {
		return err
	}
	if st.ShimError == nil {
		return fmt.Errorf("the page reported nothing for the invalid statement: %s", describe(st))
	}

	a := &assertions{}
	assert.NotEmpty(a, st.ShimError.Message, "the page reported an empty error for %q",
		st.ShimError.Statement)
	assert.True(a, st.ShimError.IsPgError,
		"the error from %q did not reach pgx as a *pgconn.PgError; it arrived as %q, so the "+
			"backend's ErrorResponse frame did not survive the shim intact",
		st.ShimError.Statement, st.ShimError.Message)
	return a.err()
}

// theErrorCarriesASQLSTATE distinguishes "the shim returned an error" from "the
// shim returned the backend's error". A SQLSTATE can only have come from the
// ErrorResponse frame PGlite produced.
func (s *suite) theErrorCarriesASQLSTATE() error {
	st, err := s.readState()
	if err != nil {
		return err
	}
	if st.ShimError == nil {
		return fmt.Errorf("the page reported nothing for the invalid statement: %s", describe(st))
	}

	a := &assertions{}
	assert.Regexp(a, sqlstatePattern, st.ShimError.SQLState,
		"the error from %q carries no SQLSTATE (%q); it did not come from the backend through the "+
			"shim\n%s", st.ShimError.Statement, st.ShimError.Message, describe(st))
	return a.err()
}

// theWorkflowSurvivesTheReload asserts the reloaded page found the same run in
// the same IndexedDB-persisted database.
func (s *suite) theWorkflowSurvivesTheReload() error {
	st, err := s.awaitState(workflowTimeout, fmt.Sprintf("workflow %s to reappear after the reload", s.workflowID),
		func(p pageState) bool {
			_, ok := p.find(s.workflowID)
			return ok
		})
	if err != nil {
		return fmt.Errorf("%w\nPGlite state did not persist in IndexedDB across the reload", err)
	}

	// Found *at startup*, not merely by a later refresh: demo/wasm/main.go
	// records the list it read between dbos.Launch and the first render, so a
	// non-zero count is the reloaded runtime opening the same IndexedDB
	// database rather than a fresh one that filled up afterwards.
	a := &assertions{}
	assert.NotZero(a, st.Runtime.Recovered,
		"the reloaded page found no workflows at startup, so it did not come back to the database "+
			"workflow %s was enqueued in\n%s", s.workflowID, describe(st))
	return a.err()
}

// theCallbackReceivedTheNotification reads the page's own probe.
//
// The measurement is demo/wasm/probe.go's: LISTEN on one logical connection,
// NOTIFY on another, both before dbos.Launch takes a connection for its
// listener. The two counters come off the tap in demo/wasm/pglite.go — one in
// the onNotification subscription, one over the raw bytes execProtocol returned
// — so neither is downstream of the routing decision the next step checks.
func (s *suite) theCallbackReceivedTheNotification() error {
	st, err := s.readState()
	if err != nil {
		return err
	}
	p := st.Probe

	a := &assertions{}
	assert.Empty(a, p.Err, "the probe did not complete: %s", p.Verdict)
	assert.True(a, p.HasCallback,
		"the vendored PGlite bundle exposes no onNotification at all, so the shim has no "+
			"notification ingress and DBOS is on its polling fallback: %s", p.Verdict)
	assert.NotZero(a, p.CallbackHits,
		"onNotification did not fire (an inline NotificationResponse appeared %d time(s)): %s",
		p.InlineFrames, p.Verdict)
	assert.True(a, p.Delivered,
		"the notification never reached pgx on the listening connection, so DBOS's queue dispatch "+
			"is running on its polling fallback: %s", p.Verdict)
	return a.err()
}

// routeInlineIsCorrect turns the probe into a guard on the shipped default.
//
// The answer is known: the real bundle fires onNotification *and* leaves a
// NotificationResponse inline, so dropping the inline copy — which is what
// wasmpg.Config's zero value does — drops a duplicate. This asserts that is
// still true, so a PGlite bundle that stops dispatching to onNotification fails
// here instead of degrading the demo to polling in silence.
//
// The page's own routeInlineNotifications is checked first. It is settable from
// the URL (demo/wasm/main.go, queryParam), and a page running with the flag on
// would make everything above an observation about a configuration nobody
// ships.
func (s *suite) routeInlineIsCorrect() error {
	st, err := s.readState()
	if err != nil {
		return err
	}
	p := st.Probe

	var cfg wasmpg.Config
	// The default is right unless the notification arrives inline only: then
	// dropping the inline copy drops the only copy there is.
	mustRouteInline := p.InlineFrames > 0 && p.CallbackHits == 0

	a := &assertions{}
	assert.Equal(a, cfg.RouteInlineNotifications, p.RouteInline,
		"the deployed page ran with RouteInlineNotifications=%t and wasmpg ships %t, so the probe "+
			"measured a configuration this assertion is not about", p.RouteInline, cfg.RouteInlineNotifications)
	assert.Equal(a, mustRouteInline, cfg.RouteInlineNotifications,
		"the real PGlite bundle delivered the notification through onNotification %d time(s) and "+
			"inline %d time(s); wasmpg.Config's zero value routes inline frames=%t. %s",
		p.CallbackHits, p.InlineFrames, cfg.RouteInlineNotifications, p.Verdict)
	return a.err()
}
