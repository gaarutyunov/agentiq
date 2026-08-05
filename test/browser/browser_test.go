//go:build browser

package browser

import (
	"context"
	"os"
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

// TestBrowserRuntime runs features/browser_runtime.feature against the
// deployed demo.
func TestBrowserRuntime(t *testing.T) {
	target := os.Getenv(PreviewURLEnv)
	if target == "" {
		t.Skipf("%s is not set; nothing is deployed to test against. "+
			"Set it to the Pages URL or a PR preview URL (SPEC.md §13.2).", PreviewURLEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	// One browser for the suite. Each scenario gets its own tab, because the
	// reload scenario must not disturb the others and IndexedDB is per-origin,
	// not per-tab.
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	t.Cleanup(cancelAlloc)

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	t.Cleanup(cancelBrowser)

	// Fail early and clearly if Chrome is not installed, rather than as a
	// timeout on the first navigation.
	require.NoError(t, chromedp.Run(browserCtx), "start headless Chrome")

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
	sc.Step(`^a throwaway in-memory PGlite instance$`, s.aThrowawayPGlite)

	sc.Step(`^a workflow is started from the browser$`, s.aWorkflowIsStarted)
	sc.Step(`^invalid SQL is executed from the browser$`, s.invalidSQLIsExecuted)
	sc.Step(`^the tab is reloaded before the workflow completes$`, s.theTabIsReloaded)
	sc.Step(`^a notification is raised on a channel that instance is listening to$`, s.aNotificationIsRaised)

	sc.Step(`^dbos\.workflow_status contains one row with status SUCCESS$`, s.oneRowWithStatusSuccess)
	sc.Step(`^the property graph returns the workflow with its steps$`, s.theGraphReturnsSteps)
	sc.Step(`^the page reports a pgx error$`, s.thePageReportsAPgxError)
	sc.Step(`^the error carries a SQLSTATE$`, s.theErrorCarriesASQLSTATE)
	sc.Step(`^the workflow is still known after the reload$`, s.theWorkflowSurvivesTheReload)
	sc.Step(`^the onNotification callback receives it$`, s.theCallbackReceivesIt)
	sc.Step(`^wasmpg\.Config\.RouteInlineNotifications is correct for that behaviour$`, s.routeInlineIsCorrect)
}
