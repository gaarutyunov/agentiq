//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"syscall/js"
	"time"
)

// maxLogLines bounds the on-page log. The page is a long-lived tab driving a
// polling loop; an unbounded list is a leak with a scrollbar.
const maxLogLines = 200

// ui drives the ui-kit page through DOM APIs (SPEC.md §13.4). The HTML in
// demo/web/index.html is static and declares the components; everything that
// changes is written from here, which is what keeps the demo a demo of the Go
// runtime rather than of a JavaScript application.
type ui struct {
	doc js.Value

	mu    sync.Mutex
	state pageState
	// funcs are the js.Func values handed to addEventListener. They are held so
	// they are not collected while JavaScript can still call them; the page
	// outlives every one of them, so none is ever released.
	funcs []js.Func
}

// pageState is the whole of what the page knows, and it is published verbatim
// into the `#agentiq-state` script element as JSON.
//
// That element is the contract with the browser test (SPEC.md §14.4): chromedp
// reads one node and gets the runtime's own account of what happened, instead
// of scraping rendered text that exists to be read by a person. It is also how
// a `--dump-dom` against a headless browser produces something worth reading.
type pageState struct {
	Stage     string        `json:"stage"`
	Ready     bool          `json:"ready"`
	Error     string        `json:"error,omitempty"`
	Runtime   runtimeFacts  `json:"runtime"`
	Probe     notifyProbe   `json:"notificationProbe"`
	PGlite    observations  `json:"pglite"`
	Workflows []workflowRow `json:"workflows"`
	// ShimError is the last error deliberately provoked through the transport
	// (failure-matrix row F20). It is nil until the page is asked to run
	// invalid SQL, and it is not the same thing as Error: Error means the
	// runtime did not start, this means the runtime reported a database error
	// correctly.
	ShimError *shimError `json:"shimError,omitempty"`
	Log       []string   `json:"log"`
}

// shimError is what an error looked like by the time it had crossed the shim.
type shimError struct {
	Statement string `json:"statement"`
	Message   string `json:"message"`
	// SQLState is populated only when the error decoded to a *pgconn.PgError,
	// which is what F20 actually asserts: the ErrorResponse frame survived the
	// transport with its SQLSTATE intact.
	SQLState  string `json:"sqlState,omitempty"`
	IsPgError bool   `json:"isPgError"`
}

// runtimeFacts is what the page reports about how it wired itself up. Every
// field is something that was a guess before the demo ran.
type runtimeFacts struct {
	PGliteVersion  string `json:"pgliteVersion"`
	ServerVersion  string `json:"serverVersion"`
	DataDir        string `json:"dataDir"`
	LogicalConns   int32  `json:"logicalConnectionBudget"`
	PoolMaxConns   int32  `json:"pgxpoolMaxConns"`
	UsableConns    int32  `json:"usableConcurrency"`
	GraphMigration string `json:"graphMigration"`
	Migrated       bool   `json:"dbosMigrated"`
	Launched       bool   `json:"dbosLaunched"`
	Recovered      int    `json:"workflowsFoundAtStartup"`
}

// workflowRow is one row of the property-graph traversal: a workflow with its
// steps, which is SPEC.md §16's M1 acceptance query.
type workflowRow struct {
	ID      string    `json:"workflowUuid"`
	Status  string    `json:"status"`
	Name    string    `json:"name"`
	Queue   string    `json:"queueName"`
	Created string    `json:"createdAt"`
	Steps   []stepRow `json:"steps"`
	// StepsError is set when the property-graph traversal for this workflow
	// failed. The row still exists — DBOS's list found it — but the graph could
	// not say what steps it has.
	StepsError string `json:"stepsError,omitempty"`
}

type stepRow struct {
	FunctionID   int32  `json:"functionId"`
	FunctionName string `json:"functionName"`
	Error        string `json:"error,omitempty"`
}

func newUI() *ui {
	u := &ui{doc: js.Global().Get("document")}
	u.state.Stage = "starting"
	return u
}

func (u *ui) el(id string) js.Value { return u.doc.Call("getElementById", id) }

// stage advances the headline status and records it in the log. The badge
// colour is the only place the page editorialises: amber while working, green
// when the runtime is up, red when it is not.
func (u *ui) stage(text string) {
	u.mu.Lock()
	u.state.Stage = text
	u.mu.Unlock()

	if b := u.el("stage"); !b.IsNull() {
		b.Set("textContent", text)
	}
	u.logf("%s", text)
}

func (u *ui) ready() {
	u.mu.Lock()
	u.state.Ready = true
	u.state.Stage = "ready"
	u.mu.Unlock()

	if b := u.el("stage"); !b.IsNull() {
		b.Set("textContent", "ready")
		b.Call("setAttribute", "color", "green")
	}
	u.enable("start", true)
	u.logf("ready")
	u.flush()
}

// fail puts the error where a person will see it and where the browser test can
// read it, and leaves the page in a state that says so rather than one that
// merely stops updating.
func (u *ui) fail(err error) {
	u.mu.Lock()
	u.state.Error = err.Error()
	u.state.Ready = false
	u.state.Stage = "failed"
	u.mu.Unlock()

	if b := u.el("stage"); !b.IsNull() {
		b.Set("textContent", "failed")
		b.Call("setAttribute", "color", "red")
	}
	if a := u.el("error"); !a.IsNull() {
		a.Set("textContent", err.Error())
		a.Call("removeAttribute", "hidden")
	}
	u.logf("error: %v", err)
	u.flush()
}

func (u *ui) logf(format string, args ...any) {
	line := fmt.Sprintf("%s  %s", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))

	u.mu.Lock()
	u.state.Log = append(u.state.Log, line)
	if len(u.state.Log) > maxLogLines {
		u.state.Log = u.state.Log[len(u.state.Log)-maxLogLines:]
	}
	u.mu.Unlock()

	box := u.el("log")
	if box.IsNull() {
		return
	}
	row := u.doc.Call("createElement", "div")
	row.Set("className", "log-line")
	row.Set("textContent", line)
	box.Call("appendChild", row)
	for box.Get("childElementCount").Int() > maxLogLines {
		box.Call("removeChild", box.Get("firstElementChild"))
	}
	box.Set("scrollTop", box.Get("scrollHeight"))

	// Publish. Without this the rendered log and the JSON state disagree, and
	// the JSON is what the browser test reads — so a page that is working
	// perfectly well looks, through that node, like a page that stopped at
	// whatever the last flush happened to be. That is not a hypothetical: it
	// cost an afternoon.
	u.flush()
}

func (u *ui) setRuntime(f runtimeFacts) {
	u.mu.Lock()
	u.state.Runtime = f
	u.mu.Unlock()

	u.renderFacts([][2]string{
		{"PGlite", f.PGliteVersion},
		{"server_version", f.ServerVersion},
		{"data directory", f.DataDir},
		{"logical connection budget", fmt.Sprint(f.LogicalConns)},
		{"pgxpool MaxConns", fmt.Sprint(f.PoolMaxConns)},
		{"usable concurrency", fmt.Sprintf("%d (budget − DBOS's listener)", f.UsableConns)},
		{"graph migration", f.GraphMigration},
	})
	u.flush()
}

func (u *ui) setProbe(p notifyProbe) {
	u.mu.Lock()
	u.state.Probe = p
	u.mu.Unlock()

	box := u.el("probe")
	if !box.IsNull() {
		box.Set("textContent", p.Verdict)
		tone := "info"
		switch {
		case p.Err != "" || !p.Ran:
			tone = "warning"
		case p.CallbackHits == 0 && p.InlineFrames > 0:
			tone = "danger"
		case p.Delivered:
			tone = "success"
		}
		box.Call("setAttribute", "tone", tone)
	}
	u.logf("notification probe: %s", p.Verdict)
	u.flush()
}

// setShimError publishes the F20 result to the page and to the state element.
func (u *ui) setShimError(e shimError) {
	u.mu.Lock()
	u.state.ShimError = &e
	u.mu.Unlock()

	if box := u.el("shim-error"); !box.IsNull() {
		text := e.Message
		if e.SQLState != "" {
			text = fmt.Sprintf("SQLSTATE %s: %s", e.SQLState, e.Message)
		}
		box.Set("textContent", text)
		box.Call("setAttribute", "tone", map[bool]string{true: "success", false: "warning"}[e.IsPgError])
		box.Call("removeAttribute", "hidden")
	}
	u.logf("invalid SQL through the shim: pgError=%t sqlstate=%q %s", e.IsPgError, e.SQLState, e.Message)
}

func (u *ui) setPGlite(o observations) {
	u.mu.Lock()
	u.state.PGlite = o
	u.mu.Unlock()

	shapes := make([]string, 0, len(o.ResultShapes))
	for k, v := range o.ResultShapes {
		shapes = append(shapes, fmt.Sprintf("%s ×%d", k, v))
	}
	sortStrings(shapes)

	if box := u.el("shape"); !box.IsNull() {
		box.Set("textContent", strings.Join(shapes, ", "))
	}
	u.flush()
}

// renderFacts writes a definition list. It builds nodes rather than assigning
// innerHTML: every value here is data, and one of them is a version string that
// came out of JavaScript.
func (u *ui) renderFacts(pairs [][2]string) {
	box := u.el("facts")
	if box.IsNull() {
		return
	}
	box.Set("textContent", "")
	for _, p := range pairs {
		dt := u.doc.Call("createElement", "dt")
		dt.Set("textContent", p[0])
		dd := u.doc.Call("createElement", "dd")
		dd.Set("textContent", p[1])
		box.Call("appendChild", dt)
		box.Call("appendChild", dd)
	}
}

// renderWorkflows fills the ga-table. Each slotted child is one row, with one
// element per declared column — that is the contract ga-table's grid layout
// expects, and the column list is declared on the element in index.html.
func (u *ui) renderWorkflows(rows []workflowRow) {
	u.mu.Lock()
	u.state.Workflows = rows
	u.mu.Unlock()

	table := u.el("runs")
	if table.IsNull() {
		return
	}
	table.Set("textContent", "")

	if len(rows) == 0 {
		empty := u.doc.Call("createElement", "div")
		empty.Set("className", "empty")
		empty.Call("appendChild", u.cell("no workflows yet — start one"))
		empty.Call("appendChild", u.cell(""))
		empty.Call("appendChild", u.cell(""))
		empty.Call("appendChild", u.cell(""))
		table.Call("appendChild", empty)
		u.flush()
		return
	}

	for _, r := range rows {
		row := u.doc.Call("createElement", "div")
		row.Call("appendChild", u.cell(shortID(r.ID)))

		badge := u.doc.Call("createElement", "ga-badge")
		badge.Set("textContent", r.Status)
		badge.Call("setAttribute", "color", statusColor(r.Status))
		badge.Call("setAttribute", "size", "sm")
		wrap := u.doc.Call("createElement", "span")
		wrap.Call("appendChild", badge)
		row.Call("appendChild", wrap)

		row.Call("appendChild", u.cell(r.Name))
		if r.StepsError != "" {
			row.Call("appendChild", u.cell("graph traversal failed"))
		} else {
			row.Call("appendChild", u.cell(describeSteps(r.Steps)))
		}
		table.Call("appendChild", row)
	}
	u.flush()
}

func (u *ui) cell(text string) js.Value {
	span := u.doc.Call("createElement", "span")
	span.Set("textContent", text)
	return span
}

// onClick registers a handler that runs its body on a fresh goroutine, so that
// nothing a click starts runs on the JavaScript event loop's turn
// (SPEC.md §12.5).
func (u *ui) onClick(id string, fn func()) {
	el := u.el(id)
	if el.IsNull() {
		return
	}
	cb := js.FuncOf(func(js.Value, []js.Value) any {
		go fn()
		return nil
	})
	u.mu.Lock()
	u.funcs = append(u.funcs, cb)
	u.mu.Unlock()
	el.Call("addEventListener", "click", cb)
}

func (u *ui) enable(id string, on bool) {
	el := u.el(id)
	if el.IsNull() {
		return
	}
	if on {
		el.Call("removeAttribute", "disabled")
	} else {
		el.Call("setAttribute", "disabled", "")
	}
}

// flush publishes the state. It is the last thing every mutator does, because a
// state element that lags the page is worse than no state element.
func (u *ui) flush() {
	u.mu.Lock()
	blob, err := json.MarshalIndent(u.state, "", " ")
	u.mu.Unlock()
	if err != nil {
		return
	}
	if el := u.el("agentiq-state"); !el.IsNull() {
		el.Set("textContent", string(blob))
	}
}

// slogHandler forwards DBOS's own logging onto the page.
//
// DBOS logs the things that make the demo legible — that it migrated, that it
// recovered N workflows, that a queue picked one up — and under js/wasm its
// default handler writes to stdout, which is the browser console and therefore
// invisible to `--dump-dom` and awkward for chromedp. Sending it through the
// page log puts it where both a person and the browser test can read it.
type slogHandler struct {
	ui    *ui
	level slog.Level
	attrs []slog.Attr
}

func newSlogHandler(u *ui, level slog.Level) *slogHandler {
	return &slogHandler{ui: u, level: level}
}

func (h *slogHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *slogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	fmt.Fprintf(&b, "dbos %s %s", strings.ToLower(r.Level.String()), r.Message)
	for _, a := range h.attrs {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
	}
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	h.ui.logf("%s", b.String())
	return nil
}

func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := *h
	out.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &out
}

// WithGroup is a no-op: the page log is one line per record and a group would
// only add a prefix nobody reads.
func (h *slogHandler) WithGroup(string) slog.Handler { return h }

func statusColor(status string) string {
	switch status {
	case "SUCCESS":
		return "green"
	case "PENDING", "ENQUEUED":
		return "amber"
	case "ERROR", "CANCELLED", "MAX_RECOVERY_ATTEMPTS_EXCEEDED", "RETRIES_EXCEEDED":
		return "red"
	default:
		return "blue"
	}
}

// shortID trims a workflow UUID to something a table column can hold without
// the full value becoming unreadable. The whole value stays in the JSON state.
func shortID(id string) string {
	if len(id) <= 13 {
		return id
	}
	return id[:8] + "…" + id[len(id)-4:]
}

func describeSteps(steps []stepRow) string {
	if len(steps) == 0 {
		return "—"
	}
	names := make([]string, 0, len(steps))
	for _, s := range steps {
		if s.Error != "" {
			names = append(names, s.FunctionName+" (error)")
			continue
		}
		names = append(names, s.FunctionName)
	}
	return strings.Join(names, " → ")
}

// sortStrings is an insertion sort over a handful of strings. Importing `sort`
// for at most five elements is not worth the binary — and the binary is gated
// at 40 MiB (SPEC.md §18.3).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
