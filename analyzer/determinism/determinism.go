// Package determinism implements the AgentIQ determinism analyzer (SPEC.md
// §17.1) — the one architecture check with no off-the-shelf substitute.
// Temporal ships `workflowcheck`; DBOS ships nothing equivalent, and DBOS
// performs no automatic substitution and no import sandboxing (SPEC.md §9.1),
// so nothing at run time stops non-deterministic code from being replayed into
// a different outcome.
//
// # What it checks
//
// Entry points are the functions passed to `dbos.RegisterWorkflow`, plus their
// transitive call graph *within this module*. Inside that closure the analyzer
// diagnoses `time.Now`, `time.Since`, `math/rand`, `crypto/rand`, UUID
// generation, `os.Getenv`, `net/http`, `os` file operations, `range` over a
// map, bare `go` statements and bare `select`.
//
// # Why the escape hatch is the important half
//
// Any call reached through `dbos.RunAsStep`, `dbos.RunAsTransaction`,
// `dbos.Go` or `dbos.Select` terminates the walk. That is the entire point:
// non-determinism is not a bug, it is the job — it is only illegal in the
// workflow body, which replays. An analyzer that flagged every `time.Now` in
// the transitive closure of a workflow would flag the whole program and be
// switched off within a day, which is strictly worse than no analyzer at all.
//
// # Scoping the walk to the module
//
// The walk follows calls only into packages that share the module prefix of
// the package being analyzed (override with -module). Following calls into the
// standard library or third-party dependencies would be useless noise:
// `encoding/json` ranges over maps, `fmt` reaches `os` file operations, and
// almost everything eventually reaches `time`. The forbidden operations in
// those packages are recognised directly, by name, rather than by walking into
// them.
//
// # Where diagnostics land
//
// Cross-package reachability travels on analysis facts, which flow from
// dependency to dependent — the opposite direction from registration. So facts
// record *why* a function is non-deterministic for every function in the
// module, and diagnostics are emitted only when walking outward from a
// `dbos.RegisterWorkflow` call. A non-deterministic helper that no workflow
// reaches is not a finding.
//
// When the registered function lives in the package under analysis, each
// violation is reported at its own position, which is what makes the finding
// actionable. When it lives in another package, the position is not in this
// package's files and go/analysis cannot report there, so the diagnostic lands
// on the registration site carrying the call chain and the rendered position
// of the original violation.
//
// This package has no runtime dependencies (SPEC.md §17.1): it is loaded as a
// golangci-lint module plugin, so it must not drag DBOS, pgx or anything else
// from the application into the linter binary.
package determinism

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// dbosPkgPath is the import path of the DBOS Go SDK. Recognition also accepts
// any package *named* `dbos`, so that the analyzer's own test fixtures — and a
// future major-version bump of the SDK's import path — do not need it changed.
const dbosPkgPath = "github.com/dbos-inc/dbos-transact-golang/dbos"

// registerFunc is the entry-point marker: everything reachable from the
// function passed to it is workflow code. `workflow/` is the only package
// permitted to call it (SPEC.md §4.1), which is a rule about where
// registration lives, not one this analyzer enforces.
const registerFunc = "RegisterWorkflow"

// stepBoundaries are the calls that terminate the walk (SPEC.md §17.1). Every
// one of them takes the work to be done as a function value; DBOS runs that
// value outside the replayed body and checkpoints its result, so whatever it
// does is replayed from the checkpoint rather than re-executed.
var stepBoundaries = map[string]bool{
	"RunAsStep":        true,
	"RunAsTransaction": true,
	"Go":               true,
	"Select":           true,
}

// Analyzer is the go/analysis pass. It is exported for the golangci-lint
// module plugin in the parent package and for `singlechecker` use.
var Analyzer = &analysis.Analyzer{
	Name: "determinism",
	Doc: "check that DBOS workflow code is deterministic\n\n" +
		"Walks outward from every function passed to dbos.RegisterWorkflow and reports\n" +
		"non-deterministic operations reachable without crossing a dbos.RunAsStep,\n" +
		"dbos.RunAsTransaction, dbos.Go or dbos.Select boundary.",
	URL:       "https://github.com/gaarutyunov/agentiq/blob/main/SPEC.md#171-determinism-analyzer-analyzerdeterminism",
	Run:       run,
	FactTypes: []analysis.Fact{new(nonDeterministic)},
}

// modulePrefix overrides the inferred module prefix. golangci-lint passes
// plugin settings through the analyzer's flag set, so this is also how a
// consumer with an unusual import path scopes the walk.
var modulePrefix string

func init() {
	Analyzer.Flags.StringVar(&modulePrefix, "module", "",
		"module path prefix bounding the call-graph walk (default: inferred from the package under analysis)")
}

// reason is one non-deterministic operation, together with the chain of calls
// that reaches it. Chain is empty when the operation sits directly in the
// function the reason is attached to.
//
// Facts are gob-encoded across package boundaries, so every field is exported
// and every type is concrete. Where is a *rendered* position rather than a
// token.Pos because a token.Pos is meaningless in another package's FileSet.
type reason struct {
	// Tag is a short stable name for the operation ("time.Now", "map-range").
	// It is what the fact renders, so that reading a fact — in a test
	// expectation or in `-fact` debug output — does not mean reading a
	// paragraph per violation.
	Tag   string
	Msg   string
	Where string
	Chain []string
}

// String renders a reason for a diagnostic message: the operation, the path
// that reaches it, and where it actually is.
func (r reason) String() string {
	var b strings.Builder
	b.WriteString(r.Msg)
	if len(r.Chain) > 0 {
		b.WriteString(" (via ")
		b.WriteString(strings.Join(r.Chain, " -> "))
		b.WriteString(")")
	}
	if r.Where != "" {
		b.WriteString(" at ")
		b.WriteString(r.Where)
	}
	return b.String()
}

// nonDeterministic is the fact exported for every function in the module that
// can reach a non-deterministic operation without crossing a step boundary.
// Its absence is the useful signal: a function with no fact is safe to call
// from workflow code.
type nonDeterministic struct {
	Reasons []reason
}

func (*nonDeterministic) AFact() {}

func (f *nonDeterministic) String() string {
	seen := map[string]bool{}
	tags := make([]string, 0, len(f.Reasons))
	for _, r := range f.Reasons {
		if seen[r.Tag] {
			continue
		}
		seen[r.Tag] = true
		tags = append(tags, r.Tag)
	}
	sort.Strings(tags)
	return "nondeterministic: " + strings.Join(tags, ", ")
}

func run(pass *analysis.Pass) (any, error) {
	w := &walker{
		pass:   pass,
		prefix: prefixFor(pass.Pkg.Path()),
		bodies: map[*types.Func]*ast.FuncDecl{},
	}

	// Index the package's function declarations so the call graph can be
	// walked without a second pass over the AST.
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			if fn, ok := pass.TypesInfo.Defs[fd.Name].(*types.Func); ok {
				w.bodies[fn] = fd
			}
		}
	}

	w.exportFacts()
	w.reportFromRegistrations()

	return nil, nil
}

// prefixFor infers the module prefix bounding the walk from the import path of
// the package under analysis. A first segment containing a dot is a host, so
// the module is the first three segments (`github.com/owner/repo`); otherwise
// — a GOPATH-style path, which is what analysistest fixtures use — the first
// segment is the module.
//
// This is a heuristic, and a repository whose module path is not three
// segments under a host (a `gopkg.in/...` module, a vanity path of a different
// depth) must set -module explicitly. Getting it wrong is not silent: too
// narrow a prefix under-reports, too wide a prefix reports noise from
// dependencies.
func prefixFor(pkgPath string) string {
	if modulePrefix != "" {
		return modulePrefix
	}
	parts := strings.Split(pkgPath, "/")
	if strings.Contains(parts[0], ".") {
		if len(parts) >= 3 {
			return strings.Join(parts[:3], "/")
		}
		return pkgPath
	}
	return parts[0]
}

type walker struct {
	pass   *analysis.Pass
	prefix string
	bodies map[*types.Func]*ast.FuncDecl
}

// inModule reports whether a call into pkg should be followed. Nil means a
// builtin or an unresolved object; neither is followable.
func (w *walker) inModule(pkg *types.Package) bool {
	if pkg == nil {
		return false
	}
	p := pkg.Path()
	return p == w.prefix || strings.HasPrefix(p, w.prefix+"/")
}

func (w *walker) position(pos token.Pos) string {
	return w.pass.Fset.Position(pos).String()
}

// ---------------------------------------------------------------------------
// Fact computation
// ---------------------------------------------------------------------------

// exportFacts computes, for every function declared in this package, the set
// of non-deterministic operations it can reach without crossing a step
// boundary, and exports it as a fact.
//
// The computation is a fixed point rather than a single pass because the
// package's call graph may contain cycles, and because a caller declared
// before its callee must still see the callee's reasons.
func (w *walker) exportFacts() {
	direct := map[*types.Func][]reason{}
	callees := map[*types.Func][]*types.Func{}

	for fn, fd := range w.bodies {
		rs, cs := w.scan(fd.Body)
		direct[fn] = rs
		callees[fn] = cs
	}

	// resolved[fn] accumulates direct reasons plus reasons imported from
	// callees, with the callee prepended to each chain.
	resolved := map[*types.Func][]reason{}
	for fn, rs := range direct {
		resolved[fn] = append([]reason(nil), rs...)
	}

	for changed := true; changed; {
		changed = false
		for fn, cs := range callees {
			for _, callee := range cs {
				for _, r := range w.reasonsFor(callee, resolved) {
					merged := reason{
						Tag:   r.Tag,
						Msg:   r.Msg,
						Where: r.Where,
						Chain: append([]string{name(callee)}, r.Chain...),
					}
					if addReason(resolved, fn, merged) {
						changed = true
					}
				}
			}
		}
	}

	for fn, rs := range resolved {
		if len(rs) == 0 {
			continue
		}
		// Only exported functions need a fact: an unexported one cannot be
		// called from another package, so nothing outside this pass could ever
		// import the fact. Within the package the fixed point above is the
		// source of truth, and the reporting walk reads the AST directly.
		if !fn.Exported() {
			continue
		}
		sortReasons(rs)
		w.pass.ExportObjectFact(fn, &nonDeterministic{Reasons: rs})
	}
}

// reasonsFor returns a callee's reasons, preferring the in-progress fixed
// point for functions in this package and falling back to the imported fact
// for functions elsewhere in the module.
func (w *walker) reasonsFor(callee *types.Func, resolved map[*types.Func][]reason) []reason {
	if rs, ok := resolved[callee]; ok {
		return rs
	}
	var fact nonDeterministic
	if w.pass.ImportObjectFact(callee, &fact) {
		return fact.Reasons
	}
	return nil
}

// addReason appends r to fn's reasons if an equivalent one is not already
// there, and reports whether it did. Deduplication by (message, position) is
// what terminates the fixed point through a recursive call cycle: the chain
// grows but the origin does not, so a second arrival at the same origin adds
// nothing.
func addReason(resolved map[*types.Func][]reason, fn *types.Func, r reason) bool {
	for _, existing := range resolved[fn] {
		if existing.Msg == r.Msg && existing.Where == r.Where {
			return false
		}
	}
	resolved[fn] = append(resolved[fn], r)
	return true
}

func sortReasons(rs []reason) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].Where != rs[j].Where {
			return rs[i].Where < rs[j].Where
		}
		return rs[i].Msg < rs[j].Msg
	})
}

func name(fn *types.Func) string {
	if fn.Pkg() == nil {
		return fn.Name()
	}
	return fn.Pkg().Name() + "." + fn.Name()
}

// ---------------------------------------------------------------------------
// Scanning a function body
// ---------------------------------------------------------------------------

// found is one non-deterministic operation located in a body, before it is
// turned into a reason.
type found struct {
	pos token.Pos
	tag string
	msg string
}

// scan walks a body and returns the non-deterministic operations directly in
// it, plus the module-internal functions it calls. Subtrees behind a step
// boundary are not walked, and function values handed to a step boundary are
// neither walked nor followed.
func (w *walker) scan(body *ast.BlockStmt) ([]reason, []*types.Func) {
	var founds []found
	var calls []*types.Func

	w.inspect(body, func(f found) { founds = append(founds, f) }, func(fn *types.Func) { calls = append(calls, fn) })

	reasons := make([]reason, 0, len(founds))
	for _, f := range founds {
		reasons = append(reasons, reason{Tag: f.tag, Msg: f.msg, Where: w.position(f.pos)})
	}
	return reasons, calls
}

// inspect is the single AST walk shared by fact computation and reporting.
// It is written by hand rather than with ast.Inspect because a step boundary
// has to prune only *part* of a call expression: the function value is
// exempt, but the other arguments are still workflow code.
func (w *walker) inspect(n ast.Node, onFound func(found), onCall func(*types.Func)) {
	ast.Inspect(n, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.GoStmt:
			onFound(found{node.Go, "go-stmt", "bare `go` statement in workflow code: a goroutine started here is not " +
				"checkpointed and is not recreated on replay; use dbos.Go, which assigns the spawned work a " +
				"deterministic step ID"})
			return true

		case *ast.SelectStmt:
			onFound(found{node.Select, "select-stmt", "bare `select` in workflow code: which case fires depends on run-time " +
				"scheduling and is not recorded, so a replay can take a different branch; use dbos.Select, " +
				"which checkpoints the channel that was chosen"})
			return true

		case *ast.RangeStmt:
			w.checkRange(node, onFound)
			return true

		case *ast.CallExpr:
			return w.checkCall(node, onFound, onCall)
		}
		return true
	})
}

// checkRange flags iteration over a map. This is the rule people forget,
// because the code looks pure: nothing in `for k, v := range m` reads a clock
// or a random source.
//
// A range with no key and no value — `for range m` — is only a count, so the
// order it visits entries in cannot be observed, and it is not flagged.
func (w *walker) checkRange(rng *ast.RangeStmt, onFound func(found)) {
	if !used(rng.Key) && !used(rng.Value) {
		return
	}
	t := w.pass.TypesInfo.TypeOf(rng.X)
	if t == nil {
		return
	}
	if _, ok := types.Unalias(t).Underlying().(*types.Map); !ok {
		return
	}
	onFound(found{rng.Range, "map-range", "`range` over a map in workflow code: Go randomises map iteration order on every " +
		"run, so a replay visits the same entries in a different order and issues the workflow's steps in a " +
		"different sequence — DBOS then matches a checkpoint to the wrong step. Collect the keys, sort them, " +
		"and range over the sorted slice"})
}

func used(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name != "_"
}

// checkCall handles a call expression and reports whether the walk should
// descend into it.
func (w *walker) checkCall(call *ast.CallExpr, onFound func(found), onCall func(*types.Func)) bool {
	fn := w.calleeOf(call)
	if fn == nil {
		return true
	}

	// RegisterWorkflow is treated like a step boundary here for the same
	// reason: the function value handed to it is not run by the caller. An
	// inline workflow literal is workflow code, and it is walked from
	// reportEntry — folding it into the registering function's own reasons
	// would say `Register` reads the clock, which it does not.
	if isDBOS(fn) && (stepBoundaries[fn.Name()] || fn.Name() == registerFunc) {
		// The boundary exempts only the function value it is handed.
		// `dbos.RunAsStep(ctx, mkStep(time.Now()))` still evaluates
		// time.Now() inside the replayed body.
		for _, arg := range call.Args {
			if w.isFunctionValue(arg) {
				continue
			}
			w.inspect(arg, onFound, onCall)
		}
		return false
	}

	if tag, msg, bad := forbidden(fn); bad {
		onFound(found{call.Lparen, tag, msg})
		return true
	}

	if w.inModule(fn.Pkg()) {
		onCall(fn)
	}
	return true
}

// isFunctionValue reports whether an argument denotes the work a step boundary
// will run: a literal, or an identifier or selector of function type.
func (w *walker) isFunctionValue(arg ast.Expr) bool {
	if _, ok := arg.(*ast.FuncLit); ok {
		return true
	}
	switch arg.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		t := w.pass.TypesInfo.TypeOf(arg)
		if t == nil {
			return false
		}
		_, ok := types.Unalias(t).Underlying().(*types.Signature)
		return ok
	}
	return false
}

// calleeOf resolves a call to the function it invokes, for plain calls, method
// calls and instantiated generic calls alike. It returns nil for builtins,
// conversions and calls through a function value, none of which name a
// function this analyzer can reason about.
func (w *walker) calleeOf(call *ast.CallExpr) *types.Func {
	fun := ast.Unparen(call.Fun)
	if idx, ok := fun.(*ast.IndexExpr); ok {
		fun = ast.Unparen(idx.X)
	}
	if idx, ok := fun.(*ast.IndexListExpr); ok {
		fun = ast.Unparen(idx.X)
	}

	var id *ast.Ident
	switch e := fun.(type) {
	case *ast.Ident:
		id = e
	case *ast.SelectorExpr:
		id = e.Sel
	default:
		return nil
	}

	fn, _ := w.pass.TypesInfo.Uses[id].(*types.Func)
	if fn == nil {
		return nil
	}
	// Generic instantiation yields a distinct *types.Func; facts and the body
	// index are keyed on the generic origin.
	return fn.Origin()
}

func isDBOS(fn *types.Func) bool {
	pkg := fn.Pkg()
	if pkg == nil {
		return false
	}
	return pkg.Path() == dbosPkgPath || pkg.Name() == "dbos"
}

// ---------------------------------------------------------------------------
// The forbidden set
// ---------------------------------------------------------------------------

// uuidPkgs are the UUID libraries whose generators are recognised. Generation
// draws from a random source or from the clock; parsing and name-based
// derivation (NewMD5, NewSHA1) do not, and are deliberately absent from
// uuidGenerators below.
var uuidPkgs = map[string]bool{
	"github.com/google/uuid":    true,
	"github.com/gofrs/uuid":     true,
	"github.com/gofrs/uuid/v3":  true,
	"github.com/gofrs/uuid/v4":  true,
	"github.com/gofrs/uuid/v5":  true,
	"github.com/satori/go.uuid": true,
}

var uuidGenerators = map[string]bool{
	"New": true, "NewString": true, "NewRandom": true, "NewRandomFromReader": true,
	"NewUUID": true, "NewV1": true, "NewV4": true, "NewV6": true, "NewV7": true,
	"NewGen": true, "NewDCEGroup": true, "NewDCEPerson": true, "NewDCESecurity": true,
}

// timeFuncs are the `time` entry points that read the wall clock or schedule
// against it. SPEC.md §17.1 names `time.Now` and `time.Since`; the rest follow
// from the same clause of §9.1 that names `dbos.Sleep` as the required
// substitute for delays, and from the fact that a timer created during a
// replay fires on a different schedule than the one that was checkpointed.
var timeFuncs = map[string]bool{
	"Now": true, "Since": true, "Until": true,
	"Sleep": true, "After": true, "Tick": true,
	"NewTimer": true, "NewTicker": true, "AfterFunc": true,
}

// osFuncs are the `os` file operations and environment reads. The filesystem
// and the environment of a recovering worker are not the ones the workflow
// started on — recovery routinely happens in a different process, and under
// §12 it can happen in a browser tab where neither exists at all.
var osFuncs = map[string]bool{
	"Getenv": true, "LookupEnv": true, "Environ": true, "ExpandEnv": true,

	"Open": true, "OpenFile": true, "Create": true, "CreateTemp": true,
	"ReadFile": true, "WriteFile": true, "ReadDir": true, "Remove": true,
	"RemoveAll": true, "Mkdir": true, "MkdirAll": true, "MkdirTemp": true,
	"Rename": true, "Stat": true, "Lstat": true, "Truncate": true,
	"Symlink": true, "Link": true, "Readlink": true, "Chmod": true, "Chown": true,
	"Getwd": true, "TempDir": true, "UserHomeDir": true, "UserCacheDir": true,
	"UserConfigDir": true, "Hostname": true, "Getpid": true,
}

// forbidden classifies a callee. The returned message says why the call is a
// problem in replayed code, not merely that it is on a list.
func forbidden(fn *types.Func) (tag, msg string, bad bool) {
	pkg := fn.Pkg()
	if pkg == nil {
		return "", "", false
	}
	path := pkg.Path()

	// Methods carry the receiver's package, so `(*os.File).Write` and
	// `(*http.Client).Do` are classified by the package below alongside the
	// package-level functions.
	recv := receiverName(fn)

	switch {
	case path == "time" && recv == "" && timeFuncs[fn.Name()]:
		return "time." + fn.Name(),
			"`time." + fn.Name() + "` in workflow code: the workflow body is re-executed on recovery, so a " +
				"wall-clock read returns a different value on the replay than it did on the original run; use " +
				"dbos.Sleep for delays and read the clock inside dbos.RunAsStep, which checkpoints the value", true

	case path == "math/rand" || path == "math/rand/v2":
		return "math/rand",
			"`math/rand` in workflow code: a replay reseeds and produces a different sequence, so the " +
				"workflow takes a different path than the checkpoints it is replaying; draw the value inside " +
				"dbos.RunAsStep", true

	case path == "crypto/rand":
		return "crypto/rand",
			"`crypto/rand` in workflow code: every replay draws different bytes, so nothing derived from " +
				"them survives recovery; draw them inside dbos.RunAsStep", true

	case uuidPkgs[path] && uuidGenerators[fn.Name()]:
		return "uuid",
			"UUID generation in workflow code: a new UUID on every replay means the recovered workflow " +
				"disagrees with its own checkpoints about which entity it created; generate it inside " +
				"dbos.RunAsStep, or derive it from the workflow ID", true

	case path == "os" && osFuncs[fn.Name()] && recv == "":
		return "os." + fn.Name(),
			"`os." + fn.Name() + "` in workflow code: the environment and filesystem of the process that " +
				"recovers a workflow are not the ones it started in — and under SPEC.md §12 the recovering " +
				"process may be a browser tab, where neither exists; do it inside dbos.RunAsStep", true

	case path == "os" && recv == "File":
		return "os.File",
			"file I/O in workflow code: `(*os.File)." + fn.Name() + "` touches state outside the " +
				"checkpoint, which a replay cannot reproduce; do it inside dbos.RunAsStep", true

	case path == "net/http":
		return "net/http",
			"`net/http` in workflow code: an HTTP call is I/O with an outcome the checkpoint does not " +
				"record, so a replay re-issues it — at best duplicating a side effect, at worst getting a " +
				"different answer; make the call inside dbos.RunAsStep, which checkpoints the result and owns " +
				"the retry policy", true
	}

	return "", "", false
}

// receiverName returns the bare type name of fn's receiver, or "" for a
// package-level function.
func receiverName(fn *types.Func) string {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return ""
	}
	t := sig.Recv().Type()
	if ptr, ok := types.Unalias(t).(*types.Pointer); ok {
		t = ptr.Elem()
	}
	if named, ok := types.Unalias(t).(*types.Named); ok {
		return named.Obj().Name()
	}
	return ""
}

// ---------------------------------------------------------------------------
// Reporting
// ---------------------------------------------------------------------------

// reportFromRegistrations finds every dbos.RegisterWorkflow call in this
// package and reports what the registered function can reach.
func (w *walker) reportFromRegistrations() {
	for _, file := range w.pass.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn := w.calleeOf(call)
			if fn == nil || !isDBOS(fn) || fn.Name() != registerFunc {
				return true
			}
			w.reportEntry(call)
			return true
		})
	}
}

// reportEntry reports the violations reachable from the workflow argument of a
// RegisterWorkflow call.
func (w *walker) reportEntry(call *ast.CallExpr) {
	for _, arg := range call.Args {
		if !w.isFunctionValue(arg) {
			continue
		}

		if lit, ok := arg.(*ast.FuncLit); ok {
			w.reportBody(lit.Body, nil, map[*types.Func]bool{})
			continue
		}

		fn := w.functionOf(arg)
		if fn == nil {
			continue
		}
		if fd, ok := w.bodies[fn]; ok {
			w.reportBody(fd.Body, nil, map[*types.Func]bool{fn: true})
			continue
		}

		// The workflow lives in another package: its violations are not at
		// positions this pass may report, so the registration site carries
		// them, with the position spelled out in the message.
		var fact nonDeterministic
		if !w.pass.ImportObjectFact(fn, &fact) {
			continue
		}
		for _, r := range fact.Reasons {
			w.pass.Reportf(call.Lparen, "%s is registered as a workflow but is not deterministic: %s",
				name(fn), reason{Tag: r.Tag, Msg: r.Msg, Where: r.Where, Chain: append([]string{name(fn)}, r.Chain...)})
		}
	}
}

// functionOf resolves an identifier or selector argument to the function it
// names.
func (w *walker) functionOf(arg ast.Expr) *types.Func {
	var id *ast.Ident
	switch e := ast.Unparen(arg).(type) {
	case *ast.Ident:
		id = e
	case *ast.SelectorExpr:
		id = e.Sel
	default:
		return nil
	}
	fn, _ := w.pass.TypesInfo.Uses[id].(*types.Func)
	if fn == nil {
		return nil
	}
	return fn.Origin()
}

// reportBody walks a workflow body in this package, reporting each violation
// at its own position and descending into same-package callees. chain records
// how the walk arrived here so a violation two helpers deep says so.
func (w *walker) reportBody(body *ast.BlockStmt, chain []string, seen map[*types.Func]bool) {
	w.inspect(body,
		func(f found) {
			w.pass.Reportf(f.pos, "%s", reason{Tag: f.tag, Msg: f.msg, Chain: chain})
		},
		func(fn *types.Func) {
			if seen[fn] {
				return
			}
			next := append(append([]string(nil), chain...), name(fn))

			if fd, ok := w.bodies[fn]; ok {
				seen[fn] = true
				w.reportBody(fd.Body, next, seen)
				return
			}

			// Another package in the module. Its violations are reported at
			// the call site, which is in this package's files.
			var fact nonDeterministic
			if !w.pass.ImportObjectFact(fn, &fact) {
				return
			}
			seen[fn] = true
			for _, r := range fact.Reasons {
				w.pass.Reportf(callSiteOf(fn, body), "%s", reason{
					Tag:   r.Tag,
					Msg:   r.Msg,
					Where: r.Where,
					Chain: append(append([]string(nil), next...), r.Chain...),
				})
			}
		})
}

// callSiteOf finds where body calls fn, so a cross-package violation is
// reported on the line that reaches it rather than at the top of the function.
func callSiteOf(fn *types.Func, body *ast.BlockStmt) token.Pos {
	pos := body.Pos()
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		var id *ast.Ident
		switch e := ast.Unparen(call.Fun).(type) {
		case *ast.Ident:
			id = e
		case *ast.SelectorExpr:
			id = e.Sel
		default:
			return true
		}
		if id.Name == fn.Name() {
			pos = call.Lparen
			return false
		}
		return true
	})
	return pos
}
