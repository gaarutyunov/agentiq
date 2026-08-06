//go:build js && wasm

package main

import (
	"context"
	"errors"
	"fmt"
	"syscall/js"
)

// bridgeName is the global object demo/web/boot.js installs. Go cannot execute
// a bare `import` — dynamic import is a JavaScript syntactic form, not a
// callable Go can reach through syscall/js — so the ES module graph is entered
// once, by three lines of JavaScript, which then hand back a factory. Every
// decision after that point is Go's.
const bridgeName = "agentiq"

// bridge returns the boot shim, or an error naming what is missing. It is
// worth the check: a page served without boot.js produces a `js.Value` of type
// undefined, and calling a method on that panics with a message that mentions
// neither the file nor the reason.
func bridge() (js.Value, error) {
	b := js.Global().Get(bridgeName)
	if b.IsUndefined() || b.IsNull() {
		return js.Undefined(), fmt.Errorf("window.%s is missing: demo/web/boot.js did not load", bridgeName)
	}
	return b, nil
}

// createPGlite instantiates the vendored PGlite PG19 fork over the given data
// directory and waits for it to finish booting.
//
// `idb://` is IndexedDB persistence. It is not a preference: GitHub Pages
// cannot set COOP/COEP, so there is no SharedArrayBuffer and PGlite's
// OPFS-with-SAB mode is unavailable (SPEC.md §3.3, §12.6, §13.3). IndexedDB is
// also what makes failure-matrix row F22 — reload the tab, the workflow resumes
// — mean anything: the database has to be the same database after the reload.
func createPGlite(ctx context.Context, dataDir string) (js.Value, error) {
	b, err := bridge()
	if err != nil {
		return js.Undefined(), err
	}
	if b.Get("createPGlite").Type() != js.TypeFunction {
		return js.Undefined(), fmt.Errorf("window.%s.createPGlite is not a function", bridgeName)
	}
	return await(ctx, b.Call("createPGlite", dataDir))
}

// await blocks the calling goroutine on a JavaScript promise without blocking
// the JavaScript event loop (SPEC.md §12.5).
//
// It duplicates wasmpg's unexported helper of the same name rather than
// exporting that one. wasmpg is the transport; widening its API to a general
// promise utility would invite the rest of the module to import it, and
// SPEC.md §4.1 keeps it a package nothing else depends on.
func await(ctx context.Context, promise js.Value) (js.Value, error) {
	if promise.Type() != js.TypeObject || promise.Get("then").Type() != js.TypeFunction {
		return promise, nil
	}

	type settled struct {
		value js.Value
		err   error
	}
	ch := make(chan settled, 1)

	var onOK, onErr js.Func
	release := func() {
		onOK.Release()
		onErr.Release()
	}
	onOK = js.FuncOf(func(_ js.Value, args []js.Value) any {
		ch <- settled{value: firstArg(args)}
		return nil
	})
	onErr = js.FuncOf(func(_ js.Value, args []js.Value) any {
		ch <- settled{err: rejection(args)}
		return nil
	})
	promise.Call("then", onOK, onErr)

	select {
	case s := <-ch:
		release()
		return s.value, s.err
	case <-ctx.Done():
		// A promise cannot be cancelled, so the callbacks are handed to a
		// goroutine that releases them once it settles. Releasing them here
		// would free a js.Func JavaScript may still call.
		go func() {
			<-ch
			release()
		}()
		return js.Undefined(), ctx.Err()
	}
}

func firstArg(args []js.Value) js.Value {
	if len(args) == 0 {
		return js.Undefined()
	}
	return args[0]
}

// rejection turns a rejected promise's value into a Go error, preferring an
// Error's message over its stringification.
func rejection(args []js.Value) error {
	v := firstArg(args)
	if v.IsUndefined() || v.IsNull() {
		return errors.New("the promise was rejected with no reason")
	}
	if msg := v.Get("message"); msg.Type() == js.TypeString {
		return errors.New(msg.String())
	}
	return errors.New(js.Global().Get("String").Invoke(v).String())
}
