// Package browser drives headless Chrome against the *deployed* demo
// (SPEC.md §14.4, §16 `browser-test`), in pure Go via chromedp.
//
// Pure Go is the point: keeping the browser assertions in Go avoids a Node
// toolchain in CI and lets them share fixtures with the server-side suites.
//
// The suite reads its target from AGENTIQ_PREVIEW_URL and skips when that is
// unset. There is no local server fallback on purpose. What M1 has to prove is
// that the runtime works where it is actually served — under
// `/pr-preview/pr-N/`, with relative asset paths, without COOP/COEP and
// therefore without SharedArrayBuffer (SPEC.md §3.3, §13.3) — and a locally
// served copy would satisfy none of those constraints while reporting green.
//
// The suite is behind `//go:build browser`; this file carries no tag so
// `go vet ./...` does not report the package as having no buildable files.
package browser
