//go:build browser

package browser

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The browser's first launch on a cold machine costs about twenty-three
// seconds; every launch after it costs about two hundred milliseconds.
//
// Measured on the CI runner, first launch against second, with a no-network
// control to rule out anything network-shaped:
//
//	chrome #1: 22878 ms      chrome #2: 231 ms
//	unshare(net): 3 ms       (identical before and after container churn)
//
// chromedp waits twenty seconds for the DevTools websocket URL and then gives
// up, so the *first* launch loses that race almost every time and every launch
// after it wins comfortably. That is the whole of the browser suite's flake:
// it failed about three runs in four, always at exactly 20.04 s, and the two
// obvious explanations are both refuted by those numbers — duplicate CI runs
// contending for the runner (it reproduces on a single run) and Docker's
// container churn serialising network-namespace setup (`unshare(net)` is 2-3 ms
// before and after).
//
// So the twenty-three seconds are real work, done once per machine: paging a
// two-hundred-megabyte binary and its libraries in from cold, and on Ubuntu
// also whatever the snap wrapper does the first time it is asked for a browser.
// None of it is the suite's work, and the suite should not be charged for it.
//
// warmBrowser pays that cost before the timed handshake, in a launch with no
// deadline riding on it. Raising chromedp's timeout or retrying the handshake
// would wait the same cost out rather than move it, and would re-break on any
// runner slower than this one; warming makes the handshake's own budget honest.
//
// It lives in the test rather than in ci.yml on purpose. SPEC.md §16 and §18.1
// exist so the gate a developer runs and the gate CI runs cannot drift, and a
// warm-up that only CI performs would be exactly that drift — a developer
// running `make verify` on a freshly booted machine would meet the flake CI no
// longer has.
const warmupBudget = 4 * time.Minute

// browserLocations is chromedp's own search order (allocate.go, findExecPath),
// duplicated because that function is unexported.
//
// The duplication cannot drift into a mismatch, which is the reason it is safe:
// whatever this finds is passed back to chromedp as chromedp.ExecPath, so
// chromedp never consults its own list and the binary warmed is by construction
// the binary launched. Warming a different browser than the one under test is
// the one way this fix could look like it worked while changing nothing — on
// ubuntu-latest the resolved binary is /usr/bin/chromium, not google-chrome,
// and warming the latter would warm a browser no test ever starts.
var browserLocations = map[string][]string{
	"darwin": {
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	},
	"windows": {
		"chrome",
		"chrome.exe",
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
	},
	"": {
		"headless_shell",
		"headless-shell",
		"chromium",
		"chromium-browser",
		"google-chrome",
		"google-chrome-stable",
		"google-chrome-beta",
		"google-chrome-unstable",
		"/usr/bin/google-chrome",
		"/usr/local/bin/chrome",
		"/snap/bin/chromium",
		"chrome",
	},
}

// findBrowser resolves the browser executable, or returns "" when none of the
// candidates is on PATH. An empty result is not a failure here: chromedp falls
// back to "google-chrome" so that it can report a useful error, and letting it
// do that produces a better message than anything this could invent.
func findBrowser() string {
	locations, ok := browserLocations[runtime.GOOS]
	if !ok {
		locations = browserLocations[""]
	}
	for _, path := range locations {
		if found, err := exec.LookPath(path); err == nil {
			return found
		}
	}
	return ""
}

// devToolsBanner is the line Chrome writes to stderr once the DevTools endpoint
// is listening — the exact event chromedp waits twenty seconds for.
const devToolsBanner = "DevTools listening on"

// warmBrowser performs, once and without a deadline riding on it, the same
// launch chromedp is about to time: start the browser with a debugging port and
// wait for it to announce the DevTools websocket.
//
// Doing precisely that rather than something cheaper is the point. A lighter
// probe — `--version`, or reading the binary into /dev/null — pages in only part
// of what a launch needs and leaves some of the cost still to be paid, and
// `--dump-dom` is a different code path that on some builds does not terminate
// when its output is a pipe. Waiting for this one line warms every stage the
// handshake depends on and nothing else, and the elapsed time it reports is
// directly comparable with the twenty-second budget it exists to protect.
//
// The browser is killed as soon as the line appears: the suite starts its own,
// and two browsers sharing a runner is the contention this is trying to remove.
//
// Failure is logged, not fatal. A browser that cannot start at all is the
// business of the handshake that follows, which reports Chrome's own stderr
// (see TestBrowserRuntime); failing here would replace that diagnosis with a
// worse one from a launch nothing depends on.
func warmBrowser(t *testing.T, path string) {
	t.Helper()

	if path == "" {
		t.Log("browser warm-up skipped: no browser executable found on PATH; " +
			"chromedp will report the failure with Chrome's own stderr")
		return
	}

	// Its own profile directory, discarded with the test: the warm-up must not
	// leave state that the suite's browser then inherits.
	profile, err := os.MkdirTemp("", "agentiq-browser-warmup-")
	if err != nil {
		t.Logf("browser warm-up skipped: temp profile: %v", err)
		return
	}
	defer func() { _ = os.RemoveAll(profile) }()

	ctx, cancel := context.WithTimeout(context.Background(), warmupBudget)
	defer cancel()

	cmd := exec.CommandContext(ctx, path,
		"--headless",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--user-data-dir="+filepath.Clean(profile),
		// Port 0 asks the OS for a free one, so the warm-up cannot collide with
		// anything else on the runner.
		"--remote-debugging-port=0",
		"about:blank",
	)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Logf("browser warm-up skipped: stderr pipe: %v", err)
		return
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Logf("browser warm-up skipped: start %s: %v", path, err)
		return
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	var seen bytes.Buffer
	scanner := bufio.NewScanner(io.TeeReader(stderr, &seen))
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), devToolsBanner) {
			// Always reported, because the number is the evidence. A first
			// launch of twenty-odd seconds followed by a fast handshake is the
			// fix working; a warm-up that is already fast means the machine was
			// warm and the suite would have passed regardless.
			t.Logf("browser warm-up: %s announced DevTools after %s",
				path, time.Since(start).Round(time.Millisecond))
			return
		}
	}

	t.Logf("browser warm-up did not reach %q in %s (not fatal — the handshake "+
		"below reports the real diagnosis)\n--- browser stderr ---\n%s\n--- end ---",
		devToolsBanner, time.Since(start).Round(time.Millisecond),
		strings.TrimSpace(seen.String()))
}
