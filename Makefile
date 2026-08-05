# AgentIQ verification (SPEC.md §16).
#
# `make verify` is the single command. CI runs the same recipes — one workflow
# step per target (§18.1) — so the gate a developer runs and the gate CI runs
# cannot drift apart.
#
# The target list is §16's, unchanged. Three recipes do more than §16 writes,
# and in each case the spec's version provably checks less than it reads as
# checking. Each deviation is argued at the target it affects:
#
#   lint       — §16's single `./bin/custom-gcl run ./...` leaves the js/wasm
#                depguard rule evaluating zero files, and never looks at
#                `analyzer/` at all.
#   test-unit  — `go test ./...` from the root does not reach `analyzer/`,
#                which is a second module.
#   demo       — `go build -o` does not create the output directory.
#
# `wasm-size` is a target here even though §16's `verify` does not list it:
# §18.1 runs the §18.3 size gate as its own step, so it has to be runnable
# without re-deriving the threshold in YAML. See the target.

.PHONY: verify generate-check lint lint-host lint-analyzer lint-wasm \
        test-unit test-integration test-drift demo browser-test wasm-size \
        custom-gcl

# The golangci-lint carrying the AgentIQ analyzers, built by `golangci-lint
# custom` from .custom-gcl.yml (§17.1). The stock golangci-lint-action does not
# load module plugins, so nothing but this binary runs `determinism`/`rawsql`.
CUSTOM_GCL := ./bin/custom-gcl

# Rebuild the plugin binary when the plugin sources or its build config change.
# testdata/ is excluded: those files are analyzer *inputs*, several of them
# deliberately uncompilable, and touching one does not change the binary.
ANALYZER_SOURCES := $(shell find analyzer -name '*.go' -not -path 'analyzer/testdata/*' 2>/dev/null)

# SPEC.md §18.3. Standard Go WASM with pgx and DBOS is large; the gate makes
# growth visible rather than enforcing a target. Exceeding it is a decision
# recorded in SPEC.md, not a silent regression.
WASM_MAX_BYTES := 41943040

verify: generate-check lint test-unit test-integration demo browser-test

# --- §16 targets ------------------------------------------------------------

# §17.3. gopgql generation is hermetic — stable ordering, no timestamps, no
# live database — so a non-empty diff means the checked-in output is stale, not
# that the generator is noisy. Do not soften this to a warning.
generate-check:
	go generate ./...
	git diff --exit-code

lint: lint-host lint-analyzer lint-wasm

# §16 writes this target as `go test ./... -short` alone. That never reaches
# `analyzer/`: .custom-gcl.yml's `path: ./analyzer` (§17.1) makes it a second
# Go module declaring the repository's own module path, and `go test
# ./analyzer/...` from the root fails outright. The determinism analyzer is the
# one check with no off-the-shelf substitute, so leaving its own tests unrun is
# not an option.
test-unit:
	go test ./... -short
	cd analyzer && go build ./... && go vet ./... && go test ./...

test-integration:
	go test ./test/... -tags=integration -timeout 20m

# §17.4, and a subset of test-integration above: `./test/...` already matches
# `./test/drift`. §18.1 runs it as a separate step and that is worth a target —
# drift is the one failure in this suite that means "update the read-only
# projection", not "fix the code".
test-drift:
	go test ./test/drift -tags=integration -timeout 20m

# `mkdir -p` is not in §16's recipe and is required: `go build -o dir/file`
# does not create `dir`, so a clean checkout fails on the first line.
demo:
	mkdir -p demo/dist
	GOOS=js GOARCH=wasm go build -o demo/dist/agentiq.wasm ./demo/wasm
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" demo/dist/
	cp -r demo/web/* demo/dist/

# `-v` is not in §16's recipe. The suite skips when AGENTIQ_PREVIEW_URL is
# unset — there is no deployed page to drive — and without `-v` a whole-suite
# skip prints as `ok`, which is indistinguishable from three passing scenarios.
# Failure-matrix rows F20, F21 and F22 live here; a run that executed none of
# them must say so.
browser-test:
	go test ./test/browser -tags=browser -v

# --- lint passes ------------------------------------------------------------

lint-host: $(CUSTOM_GCL)
	$(CUSTOM_GCL) run ./...

# `./bin/custom-gcl run ./...` from the root does not lint `analyzer/` for the
# same reason `go test ./...` does not test it: separate module.
lint-analyzer: $(CUSTOM_GCL)
	cd analyzer && ../bin/custom-gcl run ./...

# §17.2's `no-js-outside-wasmpg` rule evaluates *nothing* on a host run. Any
# file importing `syscall/js` must carry a `js && wasm` build constraint to
# compile at all, and golangci-lint skips build-constrained files — so the rule
# reports zero violations because it examined zero files, not because the tree
# is clean. This pass is what gives it something to bite on.
#
# `test/harness` is excluded because it drives Docker through testcontainers,
# whose transitive dependencies do not compile for js/wasm; every other package
# in the module typechecks under these env vars. The remaining `test/`
# packages are behind build tags and contribute only their doc.go here.
#
# `go list -f '{{.Dir}}'`, not import paths: golangci-lint resolves its
# arguments as filesystem paths, so an import path is looked up relative to the
# working directory and reported as "directory not found".
lint-wasm: $(CUSTOM_GCL)
	GOOS=js GOARCH=wasm $(CUSTOM_GCL) run \
	  $$(GOOS=js GOARCH=wasm go list -f '{{.Dir}}' ./... | grep -v '/test/harness$$')

# --- §18.3 WASM size gate ---------------------------------------------------

# §18.1 writes this as `test "$(stat -c%s demo/dist/agentiq.wasm)" -lt
# 41943040`. `stat -c` is GNU-only, so that line works on the CI runner and
# fails on a developer's macOS with a usage error rather than a size report.
# `wc -c` is portable, and reporting the measured size is the point of a gate
# that exists to make growth visible.
wasm-size:
	@size=$$(wc -c < demo/dist/agentiq.wasm | tr -d '[:space:]'); \
	printf 'demo/dist/agentiq.wasm: %s bytes (gate %s, SPEC.md §18.3)\n' "$$size" "$(WASM_MAX_BYTES)"; \
	if [ "$$size" -ge "$(WASM_MAX_BYTES)" ]; then \
	  printf 'FAIL: over the 40 MiB gate. Exceeding it is a decision recorded in SPEC.md §18.3, not a silent regression.\n' >&2; \
	  exit 1; \
	fi

# --- tooling ----------------------------------------------------------------

custom-gcl: $(CUSTOM_GCL)

$(CUSTOM_GCL): .custom-gcl.yml analyzer/go.mod analyzer/go.sum $(ANALYZER_SOURCES)
	golangci-lint custom
