//go:build browser

package browser

// The JavaScript this suite injects, which is one script and only one.
//
// Everything the suite asks the page is answered by the page: demo/wasm/probe.go
// runs the LISTEN/NOTIFY experiment against the real bundle at boot and
// publishes the counts, and demo/wasm/ui.go publishes the rest of the runtime's
// state alongside them. Re-asking any of it from injected JavaScript would be a
// second, weaker measurement of something already measured — on a throwaway
// database, through PGlite's own wrappers rather than the transport the demo
// runs on.
//
// What cannot be asked of the page is what the page does when a capability is
// taken away from it. That is failure-matrix row F21, and it is this script.

// suppressJS removes PGlite's notification callback before the runtime
// subscribes to it.
//
// It is installed with Page.addScriptToEvaluateOnNewDocument, so it runs before
// any of the document's own scripts, and it patches the factory rather than the
// instance. Both halves of that are load-bearing. The wasm module registers its
// callback during startup (demo/wasm/pglite.go, subscribe), so a patch applied
// after boot leaves the registration in place and suppresses nothing; and a
// patch applied before a reload is discarded with the global object the reload
// replaces, after which boot.js reinstates an unpatched window.agentiq.
//
// The patch is a property setter because there is nothing to wrap until boot.js
// assigns window.agentiq, and this script runs first by design.
//
// `db.onNotification = () => () => {}` shadows the bundle's own method with one
// that accepts the registration, returns a plausible unsubscribe function and
// never calls anything. That is what a lost notification looks like from Go's
// side: the subscription succeeds, nothing ever arrives, and only DBOS's
// polling fallback advances the queue. It deliberately leaves the property a
// function, so the page still reports the bundle as having the callback and the
// scenario is testing delivery rather than absence.
const suppressJS = `
(() => {
  let bridge;
  Object.defineProperty(globalThis, "agentiq", {
    configurable: true,
    get: () => bridge,
    set: (value) => {
      if (value && typeof value.createPGlite === "function") {
        const original = value.createPGlite;
        value.createPGlite = async (...args) => {
          const db = await original.apply(value, args);
          db.onNotification = () => () => {};
          return db;
        };
      }
      bridge = value;
    },
  });
})()`
