//go:build js && wasm

package wasmpg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall/js"
)

// Dialer returns a pgconn DialFunc backed by a PGlite instance. pgx never
// touches the network: the returned net.Conn marshals wire-protocol bytes
// through PGlite's execProtocol (SPEC.md §8.6, §12).
//
// The pool it is installed on must also have `MaxConns` set to
// [DefaultLogicalConns] — the budget counts DBOS's notification listener, and
// a pool allowed to open more connections than the multiplexer will hand out
// fails at dial time instead of silently starving. [NewFromPGlite] returns the
// [Multiplexer] itself when the caller wants [Multiplexer.MaxConns] rather than
// the constant.
func Dialer(pglite js.Value) func(ctx context.Context, network, addr string) (net.Conn, error) {
	m, err := NewFromPGlite(pglite, Config{})
	if err != nil {
		return func(context.Context, string, string) (net.Conn, error) { return nil, err }
	}
	return m.DialContext
}

// NewFromPGlite builds a multiplexer over a live PGlite instance and
// subscribes to its notifications.
func NewFromPGlite(pglite js.Value, cfg Config) (*Multiplexer, error) {
	if pglite.IsUndefined() || pglite.IsNull() {
		return nil, errors.New("wasmpg: PGlite instance is undefined")
	}
	if pglite.Get("execProtocol").Type() != js.TypeFunction {
		return nil, errors.New("wasmpg: PGlite instance has no execProtocol; check that the vendored bundle is the PG19 fork")
	}

	cfg.Exec = execProtocol(pglite)
	m, err := New(cfg)
	if err != nil {
		return nil, err
	}
	m.subscribe(pglite)
	return m, nil
}

// execProtocol wraps PGlite's execProtocol as an [ExecProtocol].
//
// It is execProtocol and not execProtocolRaw (D7, SPEC.md §12.2). The Raw
// variants bypass the notification-listener wrappers DBOS depends on.
func execProtocol(pglite js.Value) ExecProtocol {
	return func(ctx context.Context, msg []byte) (out []byte, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("wasmpg: execProtocol panicked: %v", r)
			}
		}()

		buf := js.Global().Get("Uint8Array").New(len(msg))
		js.CopyBytesToJS(buf, msg)

		result, err := await(ctx, pglite.Call("execProtocol", buf))
		if err != nil {
			return nil, err
		}
		return decodeExecProtocolResult(result)
	}
}

// subscribe wires PGlite's onNotification callback into
// [Multiplexer.Notify], which is the multiplexer's only notification ingress.
//
// PGlite reports no originating PID, so the frames the multiplexer builds
// carry its own synthetic one; nothing in DBOS reads it.
func (m *Multiplexer) subscribe(pglite js.Value) {
	if pglite.Get("onNotification").Type() != js.TypeFunction {
		// Older bundles without the callback still work: DBOS falls back to
		// polling its queues, which is failure-matrix row F21's whole point.
		return
	}

	cb := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) < 1 || args[0].Type() != js.TypeString {
			return nil
		}
		payload := ""
		if len(args) > 1 && args[1].Type() == js.TypeString {
			payload = args[1].String()
		}
		m.Notify(args[0].String(), payload)
		return nil
	})

	// onNotification returns an unsubscribe function in the versions that
	// have one; calling it is best-effort, releasing the callback is not.
	unsubscribe := pglite.Call("onNotification", cb)
	m.addCleanup(func() {
		if unsubscribe.Type() == js.TypeFunction {
			unsubscribe.Invoke()
		}
		cb.Release()
	})
}

// await blocks the calling goroutine on a JavaScript promise, without blocking
// the JavaScript event loop (SPEC.md §12.5). The promise's settlement runs on
// the event loop and hands the result over a channel; the Go scheduler resumes
// this goroutine from there, so every syscall/js call still happens on the Go
// scheduler's thread.
//
// A value that is not a promise is returned as-is, which keeps this working if
// a future PGlite makes execProtocol synchronous.
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
		// A promise cannot be cancelled. Hand the callbacks to a goroutine
		// that releases them once it settles, rather than leaking them or
		// releasing them while JavaScript may still call them.
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

// rejection turns a rejected promise's value into a Go error, preferring the
// Error's message over its stringification.
func rejection(args []js.Value) error {
	v := firstArg(args)
	if v.IsUndefined() || v.IsNull() {
		return errors.New("wasmpg: PGlite rejected the call with no reason")
	}
	if msg := v.Get("message"); msg.Type() == js.TypeString {
		return fmt.Errorf("wasmpg: %s", msg.String())
	}
	return fmt.Errorf("wasmpg: %s", js.Global().Get("String").Invoke(v).String())
}

// decodeExecProtocolResult extracts the raw backend bytes from whatever
// execProtocol returned.
//
// The shape has moved across PGlite releases: an array of
// [BackendMessage, Uint8Array] tuples in the versions SPEC.md §12.2 describes,
// and a { messages, data } object in others. The pinned PG19 fork bundle was
// not available to test against here — there is no browser on the build host —
// so all three shapes are accepted rather than one being guessed at. An
// unrecognised shape is an error, never silently empty bytes, because empty
// bytes would present as a hung query.
func decodeExecProtocolResult(v js.Value) ([]byte, error) {
	if v.IsUndefined() || v.IsNull() {
		return nil, nil
	}

	if isUint8Array(v) {
		return copyBytes(v), nil
	}

	if v.Type() == js.TypeObject {
		if data := v.Get("data"); isUint8Array(data) {
			return copyBytes(data), nil
		}
	}

	if isArray(v) {
		var out []byte
		for i := 0; i < v.Length(); i++ {
			elem := v.Index(i)
			switch {
			case isUint8Array(elem):
				out = append(out, copyBytes(elem)...)
			case isArray(elem) && elem.Length() >= 2 && isUint8Array(elem.Index(1)):
				out = append(out, copyBytes(elem.Index(1))...)
			}
		}
		return out, nil
	}

	return nil, fmt.Errorf("wasmpg: unrecognised execProtocol result of type %s", v.Type())
}

func isUint8Array(v js.Value) bool {
	return v.Type() == js.TypeObject && v.InstanceOf(js.Global().Get("Uint8Array"))
}

func isArray(v js.Value) bool {
	return v.Type() == js.TypeObject && js.Global().Get("Array").Call("isArray", v).Bool()
}

func copyBytes(v js.Value) []byte {
	n := v.Get("length").Int()
	if n == 0 {
		return nil
	}
	b := make([]byte, n)
	js.CopyBytesToGo(b, v)
	return b
}
