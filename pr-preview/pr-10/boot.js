// boot.js is the only JavaScript in the demo that does anything, and it does
// three things: enter the PGlite ES module graph, load the Go binary, and hand
// the two to each other. Everything after that is Go driving the DOM
// (SPEC.md §13.4).
//
// It exists because `import` is a JavaScript syntactic form. Go can call a
// function through syscall/js; it cannot execute an import. So the module graph
// is entered here, once, and what Go gets back is a factory it calls itself —
// which is what SPEC.md §20 M1's "instantiates PGlite" means in practice.
//
// Every path below is relative (`./`). That is not a style preference: the
// demo is served from `/pr-preview/pr-N/` on PR previews, and one absolute path
// breaks every one of them (SPEC.md §13.3).

import { PGlite } from "./vendor/pglite/index.js";

// The pinned build. `gaarutyunov/pglite` release `pglite-v0.5.4-pg19.1`, a
// PostgreSQL 19beta2 fork, marked pre-release and pinned by tag — see
// vendor/pglite/VENDOR.md. Reported on the page so a stale vendored bundle is
// visible rather than inferred.
const PGLITE_VERSION = "0.5.4-pg19.1";

globalThis.agentiq = {
  // contract is the version of the page's machine-readable surface: the
  // `window.agentiq` functions below and the element IDs the Go side drives
  // (#stage, #start, #reload, #bad-sql, #runs, #probe, #shape, #shim-error,
  // #facts, #log, #error, and #agentiq-state which carries all of it as JSON).
  //
  // It exists so a browser test can fail loudly on a page that predates the
  // contract instead of silently asserting on elements that are not there.
  // Bump it when an ID is removed or renamed; adding one is not a break.
  contract: 1,

  pgliteVersion: PGLITE_VERSION,

  // createPGlite is what demo/wasm/main.go calls. It returns a promise, which
  // the Go side awaits on a goroutine without blocking the event loop
  // (SPEC.md §12.5).
  //
  // `PGlite.create` rather than `new PGlite`: the constructor returns before
  // the database is ready, and Go's first execProtocol would race initdb.
  createPGlite(dataDir) {
    return PGlite.create({
      dataDir,
      // The demo's own logging goes through the Go side's page log; PGlite's
      // internal chatter would drown it.
      debug: 0,
    });
  },
};

// Load the Go runtime shim and the binary. wasm_exec.js is copied out of GOROOT
// by the `demo` make target (SPEC.md §16), so it always matches the toolchain
// that produced agentiq.wasm — a mismatched pair fails in ways that look like a
// corrupt binary.
const failure = (message) => {
  const stage = document.getElementById("stage");
  if (stage) {
    stage.textContent = "failed";
    stage.setAttribute("color", "red");
  }
  const alert = document.getElementById("error");
  if (alert) {
    alert.textContent = message;
    alert.removeAttribute("hidden");
  }
  // Also publish it into the state element, so a browser test that finds the
  // page dead learns why from the same node it reads everything else from.
  const state = document.getElementById("agentiq-state");
  if (state) {
    state.textContent = JSON.stringify({ stage: "failed", ready: false, error: message }, null, 1);
  }
};

try {
  await import("./wasm_exec.js");

  const go = new Go();
  // instantiateStreaming needs the server to send application/wasm. GitHub
  // Pages does; a plain `python3 -m http.server` does not, so fall back rather
  // than making local inspection harder than it needs to be.
  let result;
  try {
    result = await WebAssembly.instantiateStreaming(fetch("./agentiq.wasm"), go.importObject);
  } catch {
    const bytes = await (await fetch("./agentiq.wasm")).arrayBuffer();
    result = await WebAssembly.instantiate(bytes, go.importObject);
  }
  go.run(result.instance);
} catch (err) {
  failure(`could not start the Go runtime: ${err && err.message ? err.message : err}`);
  throw err;
}
