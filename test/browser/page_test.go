//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// The page's contract, as demo/web/index.html declares it and demo/wasm/ui.go
// drives it.
//
// The page is rendered for a person and published for a machine, and this suite
// only talks to the second half: it presses two buttons and reads one JSON
// node. Nothing below asserts on rendered text — SPEC.md §14.4 puts the
// runtime's own account of itself in [selState] precisely so it does not have
// to. That is also why the other ids the page drives — #stage, #runs, #probe,
// #shape, #shim-error, #facts, #log, #error — are absent here rather than
// listed and unused: every one of them shows a value the state element already
// carries, and asserting on the rendering as well would be two contracts to
// keep current for one fact.
const (
	// selStart enqueues one workflow. It stays disabled until the runtime is
	// up, which is one more reason readiness is waited on rather than assumed.
	selStart = "#start"

	// selBadSQL runs the deliberately invalid statement behind failure-matrix
	// row F20 (demo/wasm/main.go, invalidStatement).
	selBadSQL = "#bad-sql"

	// selState is the <script type="application/json"> that demo/wasm/ui.go
	// republishes on every state change. It is not a rendered element, so it is
	// read through textContent and never through chromedp.Text, which wants a
	// visible node.
	selState = "#agentiq-state"
)

// pageContract is the version demo/web/boot.js declares on `window.agentiq`.
//
// boot.js bumps it when an element id is removed or renamed — adding one is not
// a break — so that a suite written against an older page fails here, on one
// legible assertion, instead of fifty selectors later on a confusing claim
// about an empty string. Bumping it there is meant to break this constant.
const pageContract = 1

// stateJS reads [selState] without requiring the node to be visible.
const stateJS = `document.getElementById("agentiq-state")?.textContent ?? ""`

// contractJS reads the version boot.js published, or 0 before it has run.
const contractJS = `window.agentiq?.contract ?? 0`

// pageState mirrors demo/wasm/ui.go's pageState.
//
// It is a mirror rather than an import because the original lives in a
// `js && wasm` package main that a host build cannot see. The JSON tags are the
// contract; a field the page stops publishing decodes as its zero value, so
// every assertion below checks the field it reads rather than trusting that it
// arrived.
//
// Only the parts this suite reads or reports are mirrored. The rest of the
// state — the pool counters, the execProtocol result shapes — is on the page
// for a person, and copying it here would be a second contract to keep current
// for no assertion's sake.
type pageState struct {
	Stage     string        `json:"stage"`
	Ready     bool          `json:"ready"`
	Error     string        `json:"error"`
	Runtime   runtimeFacts  `json:"runtime"`
	Probe     notifyProbe   `json:"notificationProbe"`
	Workflows []workflowRow `json:"workflows"`
	ShimError *shimError    `json:"shimError"`
	Log       []string      `json:"log"`
}

type runtimeFacts struct {
	PGliteVersion string `json:"pgliteVersion"`
	ServerVersion string `json:"serverVersion"`
	Launched      bool   `json:"dbosLaunched"`
	Recovered     int    `json:"workflowsFoundAtStartup"`
}

// notifyProbe mirrors demo/wasm/probe.go's notifyProbe: what LISTEN/NOTIFY did
// when the page ran it across two logical connections, before dbos.Launch.
//
// CallbackHits and InlineFrames are counted on the tap in demo/wasm/pglite.go —
// one in the onNotification subscription, one over the raw bytes execProtocol
// returned — so both are measured at the PGlite boundary and neither depends on
// the routing decision they are used to check.
type notifyProbe struct {
	Ran          bool   `json:"ran"`
	RouteInline  bool   `json:"routeInlineNotifications"`
	HasCallback  bool   `json:"pgliteExposesOnNotification"`
	Delivered    bool   `json:"deliveredToPgx"`
	CallbackHits int    `json:"onNotificationCallbacks"`
	InlineFrames int    `json:"inlineNotificationFrames"`
	Verdict      string `json:"verdict"`
	Err          string `json:"error"`
}

// workflowRow is one row of DBOS's workflow list, with the steps the property
// graph says it has. The two halves come from different reads, which is why
// StepsError exists: the traversal can fail while the row itself is fine.
type workflowRow struct {
	ID         string    `json:"workflowUuid"`
	Status     string    `json:"status"`
	Name       string    `json:"name"`
	Queue      string    `json:"queueName"`
	Steps      []stepRow `json:"steps"`
	StepsError string    `json:"stepsError"`
}

type stepRow struct {
	FunctionID   int32  `json:"functionId"`
	FunctionName string `json:"functionName"`
	Error        string `json:"error"`
}

// shimError is the F20 result: what a backend error looked like by the time it
// had crossed the transport.
type shimError struct {
	Statement string `json:"statement"`
	Message   string `json:"message"`
	// SQLState is populated only when the error decoded to a *pgconn.PgError,
	// which is the thing F20 actually asserts.
	SQLState  string `json:"sqlState"`
	IsPgError bool   `json:"isPgError"`
}

// stepNames lists a row's steps in the order the graph returned them.
func (w workflowRow) stepNames() []string {
	out := make([]string, 0, len(w.Steps))
	for _, s := range w.Steps {
		out = append(out, s.FunctionName)
	}
	return out
}

// find returns the row for id.
func (p pageState) find(id string) (workflowRow, bool) {
	for _, w := range p.Workflows {
		if w.ID == id {
			return w, true
		}
	}
	return workflowRow{}, false
}

// ids is the set of workflows the page already knows about.
//
// Every scenario shares one origin and therefore one IndexedDB database, so the
// list is cumulative across the suite. Taking the difference across a click is
// what makes "the workflow this scenario started" a well-defined thing.
func (p pageState) ids() map[string]bool {
	out := make(map[string]bool, len(p.Workflows))
	for _, w := range p.Workflows {
		out[w.ID] = true
	}
	return out
}

// terminal reports whether a DBOS status can still change. It is used to fail a
// wait early with the status the workflow actually reached, rather than after a
// three-minute timeout that says only that SUCCESS never arrived.
//
// The list is DBOS Go v1.0.0's enum minus the statuses that are still in
// flight; RETRIES_EXCEEDED is deliberately absent, because that is a DBOS
// Python/TypeScript status (see features/durable_execution.feature).
func terminal(status string) bool {
	switch status {
	case "SUCCESS", "ERROR", "CANCELLED", "MAX_RECOVERY_ATTEMPTS_EXCEEDED":
		return true
	default:
		return false
	}
}

// --- driving the tab --------------------------------------------------------

// load waits for the load event, and nothing may be asked of the tab until it
// returns.
//
// That is not chromedp's preference, it is the page's. boot.js instantiates the
// Go binary and calls go.run() from a module script, and the Go runtime's start
// up holds the JavaScript main thread for as long as it takes — measured
// against the deployed preview, about two and a quarter minutes on a cold HTTP
// cache and a few seconds on a warm one. Runtime.evaluate runs on that same
// thread, so a poll issued in the meantime does not observe a page that is
// still booting: it does not answer at all, and every read times out until the
// thread comes back. An earlier version of this suite polled through the
// navigation and produced exactly that — five minutes of `context deadline
// exceeded` against a page that was working perfectly well.
//
// So the load event is waited on, and only then does anything read the state.
// It is not a readiness signal — when the last byte lands the runtime has still
// to run initdb, the dbos migrations, the property graph and the notification
// probe — which is why [suite.settle] follows it.
func (s *suite) load(what string, action chromedp.Action) error {
	ctx, cancel := context.WithTimeout(s.tab, bootTimeout)
	defer cancel()
	if err := chromedp.Run(ctx, action); err != nil {
		return fmt.Errorf("%s %s: %w", what, s.target, err)
	}
	return nil
}

// open navigates and returns once the runtime says it is up.
func (s *suite) open() error {
	if err := s.load("navigate to", chromedp.Navigate(s.target)); err != nil {
		return err
	}
	return s.settle()
}

// reopen reloads the tab and returns once the runtime says it is up again.
func (s *suite) reopen() error {
	if err := s.load("reload", chromedp.Reload()); err != nil {
		return err
	}
	return s.settle()
}

// settle waits for boot.js to publish its contract and for the Go runtime to
// report itself ready. It is what both a navigation and a reload wait on.
func (s *suite) settle() error {
	if err := s.awaitContract(); err != nil {
		return err
	}
	st, err := s.awaitState(bootTimeout, "the runtime to report itself ready",
		func(p pageState) bool { return p.Ready || p.Stage == "failed" })
	if err != nil {
		return err
	}
	if !st.Ready {
		return fmt.Errorf("the page reached stage %q instead of ready: %s", st.Stage, describe(st))
	}
	return nil
}

// awaitContract waits for window.agentiq and checks the version it declares.
func (s *suite) awaitContract() error {
	deadline := time.Now().Add(bootTimeout)
	for {
		var got int
		ctx, cancel := context.WithTimeout(s.tab, evalTimeout)
		err := chromedp.Run(ctx, chromedp.Evaluate(contractJS, &got))
		cancel()

		switch {
		case err == nil && got == pageContract:
			return nil
		case err == nil && got != 0:
			return fmt.Errorf(
				"the deployed page declares window.agentiq.contract=%d and this suite is written "+
					"against %d; an element id was removed or renamed (demo/web/boot.js) and the "+
					"selectors here have to be retargeted before any assertion below means anything",
				got, pageContract)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("waited %s for demo/web/boot.js to publish window.agentiq at %s "+
				"(last read: %v)", bootTimeout, s.target, err)
		}
		if err := s.pause(); err != nil {
			return err
		}
	}
}

// readState reads the page's account of itself.
func (s *suite) readState() (pageState, error) {
	ctx, cancel := context.WithTimeout(s.tab, evalTimeout)
	defer cancel()

	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(stateJS, &raw)); err != nil {
		return pageState{}, fmt.Errorf("read %s: %w", selState, err)
	}
	if strings.TrimSpace(raw) == "" {
		return pageState{}, fmt.Errorf("%s is missing from the page; it is the whole of the "+
			"browser test's contract (SPEC.md §14.4)", selState)
	}

	var st pageState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return pageState{}, fmt.Errorf("decode %s: %w (it held %s)", selState, err, clip(raw))
	}
	return st, nil
}

// awaitState polls until want is satisfied, and on failure says what the page
// was doing instead.
//
// It polls rather than using chromedp.Poll because the predicate is written in
// Go over the decoded state: a JavaScript expression could answer "is it
// SUCCESS yet" but not report the step error, the stage and the tail of the
// runtime's log when the answer stays no.
func (s *suite) awaitState(timeout time.Duration, what string, want func(pageState) bool) (pageState, error) {
	deadline := time.Now().Add(timeout)
	var (
		last    pageState
		lastErr error
		seen    bool
	)
	for {
		st, err := s.readState()
		if err == nil {
			last, lastErr, seen = st, nil, true
			if want(st) {
				return st, nil
			}
		} else {
			lastErr = err
		}

		if time.Now().After(deadline) {
			break
		}
		if err := s.pause(); err != nil {
			return last, fmt.Errorf("waiting for %s: %w", what, err)
		}
	}

	if !seen {
		return last, fmt.Errorf("waited %s for %s; the page never published a readable state: %v",
			timeout, what, lastErr)
	}
	return last, fmt.Errorf("waited %s for %s; %s", timeout, what, describe(last))
}

// pause waits one polling interval, or gives up if the tab has gone.
func (s *suite) pause() error {
	select {
	case <-time.After(pollInterval):
		return nil
	case <-s.tab.Done():
		return fmt.Errorf("the tab closed: %w", s.tab.Err())
	}
}

// click presses one of the page's buttons.
//
// The buttons are ga-button custom elements and the click handlers are
// registered on the host (demo/wasm/ui.go, onClick), so a real mouse click on
// the element is what the page is built to receive — and it is also what a
// person does, which is the thing the scenario claims works.
func (s *suite) click(sel string) error {
	ctx, cancel := context.WithTimeout(s.tab, evalTimeout)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Click(sel, chromedp.ByQuery, chromedp.NodeVisible)); err != nil {
		return fmt.Errorf("click %s: %w", sel, err)
	}
	return nil
}

// --- reporting --------------------------------------------------------------

// logTail is how much of the runtime's own log a failure carries. The page
// keeps 200 lines; the last few are the ones that say why.
const logTail = 15

// describe renders the state as the paragraph a failure needs: what stage the
// runtime reached, what it thinks of its workflows, and the tail of its log,
// which is where DBOS and the transport say what went wrong.
func describe(st pageState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "the page reports stage=%q ready=%t dbosLaunched=%t recoveredAtStartup=%d",
		st.Stage, st.Ready, st.Runtime.Launched, st.Runtime.Recovered)
	if st.Error != "" {
		fmt.Fprintf(&b, " error=%q", st.Error)
	}
	if st.Runtime.PGliteVersion != "" {
		fmt.Fprintf(&b, " pglite=%s server=%s", st.Runtime.PGliteVersion, st.Runtime.ServerVersion)
	}

	if len(st.Workflows) == 0 {
		b.WriteString("\nworkflows: none")
	} else {
		b.WriteString("\nworkflows:")
		for _, w := range st.Workflows {
			fmt.Fprintf(&b, "\n  %s %s queue=%q steps=%v", w.ID, w.Status, w.Queue, w.stepNames())
			if w.StepsError != "" {
				fmt.Fprintf(&b, " stepsError=%q", w.StepsError)
			}
		}
	}
	if st.ShimError != nil {
		fmt.Fprintf(&b, "\nshimError: isPgError=%t sqlState=%q %s",
			st.ShimError.IsPgError, st.ShimError.SQLState, st.ShimError.Message)
	}

	if n := len(st.Log); n > 0 {
		from := max(n-logTail, 0)
		fmt.Fprintf(&b, "\nthe last %d of the runtime's %d log lines:\n  %s",
			n-from, n, strings.Join(st.Log[from:], "\n  "))
	}
	return b.String()
}

// clip shortens a blob for a failure message.
func clip(s string) string {
	const limit = 300
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q… (%d bytes)", s[:limit], len(s))
}

// --- assertions -------------------------------------------------------------

// assertions collects testify failures so that a step can return them.
//
// godog attributes a failure to a scenario and a step from the error the step
// returns, and that error is what reaches the JUnit report CI reads. Testify
// reports through a TestingT instead, so handing it godog's own *testing.T
// fails the Go subtest while leaving the step — and therefore the scenario —
// green: the report would show a passing scenario inside a failing test.
//
// This bridges the two. Only assert.* is used against it, never require.*:
// require calls FailNow, which has nothing to stop here. A step that must stop
// early returns an error of its own.
type assertions struct{ msgs []string }

func (a *assertions) Errorf(format string, args ...any) {
	a.msgs = append(a.msgs, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (a *assertions) err() error {
	if len(a.msgs) == 0 {
		return nil
	}
	return errors.New(strings.Join(a.msgs, "\n"))
}
