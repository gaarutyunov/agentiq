//go:build browser

package browser

// The JavaScript this suite injects.
//
// It is JavaScript rather than Go because the question it answers is about
// JavaScript: what the *real* PGlite bundle does with a NOTIFY. Asking it from
// inside the wasm module would route the answer through the very shim under
// test, so the probe talks to PGlite directly and the shim's behaviour is
// compared against the result afterwards.

// suppressJS removes PGlite's notification callback for failure-matrix row F21.
//
// It is installed before the runtime boots by patching the factory rather than
// the instance: the wasm module registers its callback during startup, and a
// patch applied afterwards would leave that registration in place.
const suppressJS = `
(() => {
  const bridge = window.agentiq;
  if (!bridge || typeof bridge.createPGlite !== "function") {
    throw new Error("window.agentiq.createPGlite is missing");
  }
  const original = bridge.createPGlite;
  bridge.createPGlite = async (...args) => {
    const db = await original(...args);
    // Accept the registration and never invoke the callback. This is what a
    // lost notification looks like from Go's side: the subscription succeeds,
    // nothing ever arrives, and only DBOS's polling fallback advances the
    // queue.
    db.onNotification = () => () => {};
    return db;
  };
  window.__agentiqNotificationsSuppressed = true;
})()`

// probeSetupJS creates a throwaway in-memory PGlite and parks it on window.
//
// `memory://` rather than `idb://`: the probe must not touch the demo's own
// database, and an in-memory instance disappears with the tab.
const probeSetupJS = `
(async () => {
  const bridge = window.agentiq;
  if (!bridge || typeof bridge.createPGlite !== "function") return false;
  try {
    window.__agentiqProbeDB = await bridge.createPGlite("memory://agentiq-probe");
    return typeof window.__agentiqProbeDB.execProtocol === "function";
  } catch (e) {
    window.__agentiqProbeError = String(e);
    return false;
  }
})()`

// probeRunJS answers the open question wasmpg.Config.RouteInlineNotifications
// exists for.
//
// wasmpg drops NotificationResponse frames found inside an execProtocol result
// on the assumption that PGlite also dispatches them to onNotification. This
// runs LISTEN and NOTIFY through execProtocol — the same entry point the shim
// uses, not PGlite's higher-level query API, because the higher-level API is
// exactly the wrapper the shim bypasses — and reports which of the two paths
// actually delivered.
//
// The inline check looks for a NotificationResponse: message type 'A' (0x41),
// a four-byte length, a four-byte PID, then the NUL-terminated channel name.
// Matching the framing rather than searching for the channel string avoids a
// false positive on a CommandComplete that happens to quote the statement.
const probeRunJS = `
(async () => {
  const out = {callbackFired: false, inlineFound: false, error: ""};
  const db = window.__agentiqProbeDB;
  if (!db) {
    out.error = window.__agentiqProbeError || "no probe instance; run the setup step first";
    return out;
  }

  const channel = "agentiq_probe";
  const encoder = new TextEncoder();

  // A simple Query message: 'Q', int32 length (self-inclusive), NUL-terminated
  // SQL. The shim's own frontend framing, so the backend sees what it would
  // see in production.
  const query = (sql) => {
    const body = encoder.encode(sql + "\0");
    const buf = new Uint8Array(5 + body.length);
    buf[0] = 0x51;
    new DataView(buf.buffer).setUint32(1, 4 + body.length, false);
    buf.set(body, 5);
    return db.execProtocol(buf);
  };

  // Flatten whatever shape execProtocol returns. PGlite has shipped a raw
  // Uint8Array, a {data} envelope and an array of [message, bytes] pairs
  // across versions; wasmpg's decodeExecProtocolResult accepts all three and
  // so does this.
  const bytes = (result) => {
    if (!result) return new Uint8Array(0);
    if (result instanceof Uint8Array) return result;
    if (result.data instanceof Uint8Array) return result.data;
    if (Array.isArray(result)) {
      const parts = [];
      for (const item of result) {
        if (item instanceof Uint8Array) parts.push(item);
        else if (Array.isArray(item) && item[1] instanceof Uint8Array) parts.push(item[1]);
      }
      const total = parts.reduce((n, p) => n + p.length, 0);
      const flat = new Uint8Array(total);
      let at = 0;
      for (const p of parts) { flat.set(p, at); at += p.length; }
      return flat;
    }
    return new Uint8Array(0);
  };

  // Scan for a NotificationResponse frame naming our channel.
  const hasInlineNotification = (buf) => {
    const want = encoder.encode(channel);
    const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
    let i = 0;
    while (i + 5 <= buf.length) {
      const type = buf[i];
      const len = view.getUint32(i + 1, false);
      if (len < 4 || i + 1 + len > buf.length) break;
      if (type === 0x41) {                      // 'A' NotificationResponse
        const nameAt = i + 5 + 4;               // header + int32 PID
        let matches = nameAt + want.length < buf.length;
        for (let j = 0; matches && j < want.length; j++) {
          if (buf[nameAt + j] !== want[j]) matches = false;
        }
        if (matches && buf[nameAt + want.length] === 0) return true;
      }
      i += 1 + len;
    }
    return false;
  };

  try {
    if (typeof db.onNotification === "function") {
      db.onNotification((ch) => { if (ch === channel) out.callbackFired = true; });
    } else {
      out.error = "this PGlite bundle exposes no onNotification; the shim has no notification ingress at all";
      return out;
    }

    await query("LISTEN " + channel);
    const result = await query("NOTIFY " + channel + ", 'probe'");
    out.inlineFound = hasInlineNotification(bytes(result));

    // onNotification is dispatched asynchronously; give the event loop a few
    // turns before concluding it never fired.
    for (let i = 0; i < 20 && !out.callbackFired; i++) {
      await new Promise((r) => setTimeout(r, 25));
    }
  } catch (e) {
    out.error = String(e);
  }
  return out;
})()`
