//go:build browser

package browser

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/cucumber/godog"
	"github.com/stretchr/testify/require"

	"github.com/gaarutyunov/agentiq/test/harness"
)

// PreviewURLEnv names the deployed page under test: the Pages production URL,
// or a PR preview at https://<user>.github.io/agentiq/pr-preview/pr-<n>/
// (SPEC.md §13.2).
const PreviewURLEnv = "AGENTIQ_PREVIEW_URL"

// suiteTimeout bounds the whole feature.
//
// The first scenario loads 15 MB of wasm and data images over the network and
// takes about three minutes to reach ready on an idle machine; every scenario
// after it finds the assets in the browser's cache and takes about twelve
// seconds. Five scenarios therefore cost four or five minutes in total, and
// this is a ceiling rather than an estimate: it exists so that a suite which
// has gone wrong stops and reports, and it is set high enough that a busy
// runner is never the thing that trips it.
const suiteTimeout = 40 * time.Minute

// TestBrowserRuntime runs features/browser_runtime.feature against the
// deployed demo.
func TestBrowserRuntime(t *testing.T) {
	target := os.Getenv(PreviewURLEnv)
	if target == "" {
		t.Skipf("%s is not set; nothing is deployed to test against. "+
			"Set it to the Pages URL or a PR preview URL (SPEC.md §13.2).", PreviewURLEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	t.Cleanup(cancel)

	// Chrome's own stderr, captured.
	//
	// Without this the only thing a startup failure produces is chromedp's
	// internal 20-second wait for the DevTools websocket URL expiring:
	//
	//	browser_test.go:57: Received unexpected error: websocket url timeout reached
	//	Messages: start headless Chrome
	//
	// That message names the symptom of every possible cause and the cause of
	// none — Chrome missing, Chrome killed by the sandbox, Chrome out of shared
	// memory and Chrome crashing on a bad flag all produce exactly those words.
	// Chrome says which one it was on stderr and nothing was reading it.
	//
	// It is a buffer rather than a direct pipe to the test log because Chrome is
	// chatty on a healthy run too; the contents are only printed when the
	// handshake fails, which is the only time they are worth reading.
	var chromeOutput bytes.Buffer
	allocOpts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	allocOpts = append(allocOpts, chromedp.CombinedOutput(&chromeOutput))

	// One browser for the suite. Each scenario gets its own tab, because the
	// reload scenario must not disturb the others and IndexedDB is per-origin,
	// not per-tab.
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, allocOpts...)
	t.Cleanup(cancelAlloc)

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	t.Cleanup(cancelBrowser)

	// Fail early and clearly if Chrome cannot start, rather than as a timeout on
	// the first navigation — and say what Chrome said.
	if err := chromedp.Run(browserCtx); err != nil {
		out := strings.TrimSpace(chromeOutput.String())
		if out == "" {
			out = "(Chrome wrote nothing to stderr, which points at the binary " +
				"never being executed at all rather than at it failing to start)"
		}
		require.NoError(t, err, "start headless Chrome\n--- chrome stderr ---\n%s\n--- end ---", out)
	}

	s := &suite{target: target, browser: browserCtx}

	opts, err := harness.GodogOptions(t, "@browser", "browser-junit.xml")
	require.NoError(t, err)

	status := godog.TestSuite{
		Name:                "browser-runtime",
		ScenarioInitializer: s.initialize,
		Options:             &opts,
	}.Run()

	require.Zero(t, status, "godog exit status")
}

func (s *suite) initialize(sc *godog.ScenarioContext) {
	sc.Before(s.before)
	sc.After(s.after)

	sc.Step(`^the deployed demo page$`, s.theDeployedPage)
	sc.Step(`^the PGlite notification callback is suppressed$`, s.suppressNotifications)
	sc.Step(`^the runtime's notification probe has run$`, s.theProbeHasRun)

	sc.Step(`^a workflow is started from the browser$`, s.aWorkflowIsStarted)
	sc.Step(`^invalid SQL is executed from the browser$`, s.invalidSQLIsExecuted)
	sc.Step(`^the tab is reloaded before the workflow completes$`, s.theTabIsReloaded)

	sc.Step(`^dbos\.workflow_status contains one row with status SUCCESS$`, s.oneRowWithStatusSuccess)
	sc.Step(`^the property graph returns the workflow with its steps$`, s.theGraphReturnsSteps)
	sc.Step(`^the page reports a pgx error$`, s.thePageReportsAPgxError)
	sc.Step(`^the error carries a SQLSTATE$`, s.theErrorCarriesASQLSTATE)
	sc.Step(`^the workflow is still known after the reload$`, s.theWorkflowSurvivesTheReload)
	sc.Step(`^the onNotification callback received the notification$`, s.theCallbackReceivedTheNotification)
	sc.Step(`^wasmpg\.Config\.RouteInlineNotifications is correct for that behaviour$`, s.routeInlineIsCorrect)
}
