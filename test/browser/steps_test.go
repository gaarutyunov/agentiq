//go:build browser

package browser

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/cucumber/godog"
	"github.com/stretchr/testify/assert"

	"github.com/gaarutyunov/agentiq/wasmpg"
)

// sqlstatePattern matches a five-character SQLSTATE as pgx renders it in an
// error string: `(SQLSTATE 42601)`.
var sqlstatePattern = regexp.MustCompile(`SQLSTATE\s*[0-9A-Z]{5}`)

// The DOM contract the demo page is expected to expose.
//
// These are selectors, not implementation: the page may render them however it
// likes as long as the text content means what the name says. They are
// constants in one place so that a change in demo/web is a one-line change
// here, and so that a page that predates the contract fails on a missing
// selector rather than on a confusing assertion about an empty string.
const (
	selStatus         = "#status"          // booting | ready | running | done | error
	selStart          = "#start"           // the button that enqueues a run
	selWorkflowID     = "#workflow-id"     // the workflow UUID
	selWorkflowStatus = "#workflow-status" // the value read back from dbos.workflow_status
	selSteps          = "#steps"           // the GRAPH_TABLE result, one child per step
	selError          = "#error"           // the last error surfaced through the shim
	selBadSQL         = "#run-bad-sql"     // runs a deliberately invalid statement (F20)
)

const (
	pageTimeout     = 60 * time.Second
	workflowTimeout = 3 * time.Minute
)

// suite is the browser scenario state.
type suite struct {
	target  string
	browser context.Context

	tab       context.Context
	cancelTab context.CancelFunc

	workflowID string
	lastError  string

	// probe holds the result of the inline-notification experiment.
	probe notificationProbe
}

func (s *suite) before(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
	s.tab, s.cancelTab = chromedp.NewContext(s.browser)
	s.workflowID = ""
	s.lastError = ""
	s.probe = notificationProbe{}
	return ctx, nil
}

func (s *suite) after(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
	if s.cancelTab != nil {
		s.cancelTab()
		s.cancelTab = nil
	}
	return ctx, nil
}

// run executes actions against the scenario's tab under a step-scoped timeout.
func (s *suite) run(actions ...chromedp.Action) error {
	ctx, cancel := context.WithTimeout(s.tab, pageTimeout)
	defer cancel()
	return chromedp.Run(ctx, actions...)
}

// --- Given -----------------------------------------------------------------

// theDeployedPage navigates to the target and waits for the runtime to boot.
//
// Booting is not instant: the page loads a 9.4 MB PGlite wasm, a 5.4 MB data
// image and a standard-Go wasm binary that the SPEC.md §18.3 gate allows up to
// 40 MiB. Waiting for #status to leave "booting" is the only honest readiness
// signal — a DOM-ready wait would pass before Postgres exists.
func (s *suite) theDeployedPage() error {
	return s.run(
		chromedp.Navigate(s.target),
		chromedp.WaitVisible(selStatus, chromedp.ByQuery),
		waitForText(selStatus, "ready"),
	)
}

// suppressNotifications replaces PGlite's onNotification with a no-op before
// the runtime subscribes, which is failure-matrix row F21's mechanism.
//
// It is installed as a page-load script so it wins the race against the wasm
// module: patching after boot would leave the already-registered callback in
// place and test nothing.
func (s *suite) suppressNotifications() error {
	return s.run(
		chromedp.Navigate(s.target),
		chromedp.Evaluate(suppressJS, nil),
		chromedp.Reload(),
		chromedp.WaitVisible(selStatus, chromedp.ByQuery),
		waitForText(selStatus, "ready"),
	)
}

// aThrowawayPGlite spins up a second, in-memory PGlite for the probe, so the
// experiment cannot touch the demo's own IndexedDB-backed database.
func (s *suite) aThrowawayPGlite() error {
	var ok bool
	if err := s.run(evaluateAsync(probeSetupJS, &ok)); err != nil {
		return fmt.Errorf("create a throwaway PGlite: %w", err)
	}
	if !ok {
		return fmt.Errorf(
			"window.agentiq.createPGlite is not exposed on the deployed page; the inline-notification " +
				"probe cannot run. It needs the PGlite factory reachable from JavaScript (demo/wasm/js.go " +
				"already calls window.agentiq.createPGlite, so exposing it costs nothing)")
	}
	return nil
}

// --- When ------------------------------------------------------------------

func (s *suite) aWorkflowIsStarted() error {
	if err := s.run(
		chromedp.Click(selStart, chromedp.ByQuery),
		waitForNonEmptyText(selWorkflowID),
		chromedp.Text(selWorkflowID, &s.workflowID, chromedp.ByQuery, chromedp.NodeVisible),
	); err != nil {
		return err
	}
	if strings.TrimSpace(s.workflowID) == "" {
		return fmt.Errorf("the page reported an empty workflow id")
	}
	return nil
}

func (s *suite) invalidSQLIsExecuted() error {
	return s.run(
		chromedp.Click(selBadSQL, chromedp.ByQuery),
		waitForNonEmptyText(selError),
	)
}

// theTabIsReloaded is failure-matrix row F22. The reload has to happen while
// the workflow is still running, which is what workflow.checkpointDelay's
// durable sleep makes reliable.
func (s *suite) theTabIsReloaded() error {
	return s.run(
		chromedp.Reload(),
		chromedp.WaitVisible(selStatus, chromedp.ByQuery),
		waitForText(selStatus, "ready"),
	)
}

func (s *suite) aNotificationIsRaised() error {
	return s.run(evaluateAsync(probeRunJS, &s.probe))
}

// --- Then ------------------------------------------------------------------

func (s *suite) oneRowWithStatusSuccess(ctx context.Context) error {
	tabCtx, cancel := context.WithTimeout(s.tab, workflowTimeout)
	defer cancel()

	var status string
	if err := chromedp.Run(tabCtx,
		waitForText(selWorkflowStatus, "SUCCESS"),
		chromedp.Text(selWorkflowStatus, &status, chromedp.ByQuery, chromedp.NodeVisible),
	); err != nil {
		return fmt.Errorf("dbos.workflow_status never reached SUCCESS in the browser: %w", err)
	}
	assert.Equal(godog.T(ctx), "SUCCESS", strings.TrimSpace(status))
	return nil
}

// theGraphReturnsSteps asserts the GRAPH_TABLE half of SPEC.md §16's M1
// acceptance: the property graph has to answer in the browser build, not only
// in the server build.
func (s *suite) theGraphReturnsSteps() error {
	var count int
	if err := s.run(
		chromedp.WaitVisible(selSteps, chromedp.ByQuery),
		chromedp.Evaluate(fmt.Sprintf(`document.querySelectorAll("%s > *").length`, selSteps), &count),
	); err != nil {
		return err
	}
	if count < 2 {
		return fmt.Errorf("the property graph returned %d steps; AgentRun has resolveAgent and completeRun", count)
	}
	return nil
}

func (s *suite) thePageReportsAPgxError() error {
	var text string
	if err := s.run(chromedp.Text(selError, &text, chromedp.ByQuery, chromedp.NodeVisible)); err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("the page reported no error for invalid SQL; the backend's ErrorResponse frame did not survive the shim")
	}
	s.lastError = text
	return nil
}

// theErrorCarriesASQLSTATE is the assertion that distinguishes "the shim
// returned an error" from "the shim returned *the backend's* error". A
// SQLSTATE can only have come from the ErrorResponse frame PGlite produced.
func (s *suite) theErrorCarriesASQLSTATE() error {
	if !sqlstatePattern.MatchString(s.lastError) {
		return fmt.Errorf("no SQLSTATE in %q; the error did not come from the backend through the shim", s.lastError)
	}
	return nil
}

func (s *suite) theWorkflowSurvivesTheReload() error {
	var after string
	if err := s.run(
		waitForNonEmptyText(selWorkflowID),
		chromedp.Text(selWorkflowID, &after, chromedp.ByQuery, chromedp.NodeVisible),
	); err != nil {
		return err
	}
	if strings.TrimSpace(after) != strings.TrimSpace(s.workflowID) {
		return fmt.Errorf("after the reload the page knows workflow %q, not %q: PGlite state did not persist in IndexedDB",
			strings.TrimSpace(after), strings.TrimSpace(s.workflowID))
	}
	return nil
}

func (s *suite) theCallbackReceivesIt() error {
	if s.probe.Error != "" {
		return fmt.Errorf("the probe failed: %s", s.probe.Error)
	}
	if !s.probe.CallbackFired && !s.probe.InlineFound {
		return fmt.Errorf(
			"the notification arrived by neither path — the probe itself is broken, so it proves nothing " +
				"about wasmpg.Config.RouteInlineNotifications")
	}
	if !s.probe.CallbackFired {
		return fmt.Errorf(
			"PGlite did NOT dispatch the notification to onNotification (inline frame present: %t). "+
				"wasmpg drops inline NotificationResponse frames, so on this bundle every notification is "+
				"lost and only the polling fallback keeps the queue moving. Set "+
				"wasmpg.Config.RouteInlineNotifications = true", s.probe.InlineFound)
	}
	return nil
}

// routeInlineIsCorrect turns the probe into an assertion about the shipped
// default rather than a diagnostic somebody has to read.
//
// wasmpg.Config.RouteInlineNotifications defaults to false, which is correct
// exactly when onNotification fires: routing the inline copy as well would
// deliver a duplicate, which DBOS tolerates but does not need. It is wrong
// when the notification only ever appears inline, which is the case the whole
// scenario exists to rule out.
func (s *suite) routeInlineIsCorrect(ctx context.Context) error {
	shouldRouteInline := s.probe.InlineFound && !s.probe.CallbackFired

	var cfg wasmpg.Config
	assert.Equal(godog.T(ctx), shouldRouteInline, cfg.RouteInlineNotifications,
		"the real PGlite bundle delivers notifications via onNotification=%t, inline=%t; "+
			"wasmpg.Config's zero value routes inline frames=%t",
		s.probe.CallbackFired, s.probe.InlineFound, cfg.RouteInlineNotifications)
	return nil
}

// --- helpers ---------------------------------------------------------------

// notificationProbe is what probeRunJS returns.
type notificationProbe struct {
	// CallbackFired reports whether PGlite invoked the onNotification
	// callback registered on the throwaway instance.
	CallbackFired bool `json:"callbackFired"`

	// InlineFound reports whether the execProtocol result of the NOTIFY
	// carried a NotificationResponse ('A') frame for the channel.
	InlineFound bool `json:"inlineFound"`

	// Error is a message from the probe itself, not from the database.
	Error string `json:"error"`
}

// waitForNonEmptyText waits until a selector has non-whitespace text content.
//
// chromedp has WaitVisible but no "wait until it says something": an element
// rendered empty and filled in later is visible from the first paint, so a
// visibility wait would read the placeholder.
func waitForNonEmptyText(sel string) chromedp.Action {
	return chromedp.Poll(
		fmt.Sprintf(`(document.querySelector(%q)?.textContent ?? "").trim().length > 0`, sel),
		nil,
		chromedp.WithPollingInterval(200*time.Millisecond),
	)
}

func waitForText(sel, want string) chromedp.Action {
	return chromedp.Poll(
		fmt.Sprintf(`(document.querySelector(%q)?.textContent ?? "").trim() === %q`, sel, want),
		nil,
		chromedp.WithPollingInterval(200*time.Millisecond),
	)
}

// evaluateAsync runs an async IIFE and awaits its promise.
func evaluateAsync(js string, out any) chromedp.Action {
	return chromedp.Evaluate(js, out, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithAwaitPromise(true)
	})
}
