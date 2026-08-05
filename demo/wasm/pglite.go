//go:build js && wasm

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"syscall/js"

	"github.com/gaarutyunov/agentiq/wasmpg"
)

// backend is the PGlite side of the transport, with a tap on it.
//
// # Why this is not just wasmpg.NewFromPGlite
//
// [wasmpg.NewFromPGlite] does exactly what the two methods below do, in fewer
// lines, and it is what a caller with nothing to prove should use. This file
// exists because two things about the real PGlite bundle were never verifiable
// on a machine with no browser, and both are recorded in wasmpg's own comments
// as open:
//
//  1. **The shape of `execProtocol`'s result.** It has moved across PGlite
//     releases; `wasmpg` accepts three plausible shapes and errors on a fourth.
//     Which one 0.5.4-pg19.1 actually returns is a fact, and a fact nobody has
//     written down is a fact that gets guessed at again next milestone.
//
//  2. **Whether a NotificationResponse that arrives inline inside an
//     `execProtocol` result is *also* dispatched to `onNotification`.**
//     [wasmpg.Config.RouteInlineNotifications] defaults to false — drop the
//     inline copy — on the assumption that PGlite dispatches it. If that
//     assumption is wrong, every notification is lost silently, DBOS falls back
//     to polling, and the only symptom is a queue that runs slowly. See
//     [notifyProbe].
//
// Both counters are cheap: an integer per round trip, and a byte scan that
// walks message headers without copying. Neither is on a hot path — the hot
// path is a 9 MB WASM Postgres.
//
// When the answers are recorded in SPEC.md, this collapses back to
// [wasmpg.NewFromPGlite] and the probe goes with it.
type backend struct {
	pglite js.Value

	mu           sync.Mutex
	roundTrips   int
	resultShapes map[string]int
	inlineFrames int
	callbackHits int
}

func newBackend(pglite js.Value) (*backend, error) {
	if pglite.IsUndefined() || pglite.IsNull() {
		return nil, fmt.Errorf("PGlite instance is undefined")
	}
	if pglite.Get("execProtocol").Type() != js.TypeFunction {
		return nil, fmt.Errorf("PGlite instance has no execProtocol; check that the vendored bundle is the PG19 fork")
	}
	return &backend{pglite: pglite, resultShapes: map[string]int{}}, nil
}

// execOptions is the second argument to `execProtocol`.
//
// `throwOnError: false` is the whole of it, and it is what makes a backend error
// reach pgx as a backend error.
//
// PGlite's default is to reject the promise when the statement produced an
// ErrorResponse. The bytes are not lost when it does — `execProtocolRaw` has
// already returned them — but the rejection discards the result object and
// hands the caller a JavaScript Error instead, whose message is the backend's
// text and nothing else. That arrives here as a Go error out of [backend.exec],
// which the multiplexer returns from submit and [wasmpg.Conn.Write] returns to
// pgx, which reports it as `write failed: …`. pgx never sees an ErrorResponse
// to decode, so there is no *pgconn.PgError and no SQLSTATE — which is
// failure-matrix row F20 failing, and it failed exactly that way against the
// deployed preview.
//
// With the flag off, `execProtocol` returns normally and `data` carries the
// frames the backend really sent: the ErrorResponse with its true SQLSTATE, and
// the ReadyForQuery that answers pgx's Sync. Both matter. Synthesising an
// ErrorResponse here instead would have to invent a SQLSTATE, and omitting the
// ReadyForQuery would leave pgx waiting for one; letting the real frames
// through does neither.
//
// It also repairs the multiplexer's transaction tracking, which keys on the
// ReadyForQuery status byte: a rejected exec produced no ReadyForQuery at all,
// so a statement that failed inside a transaction left the session's status
// unobserved.
var execOptions = map[string]any{"throwOnError": false}

// exec is the [wasmpg.ExecProtocol] the multiplexer runs on.
//
// It is `execProtocol` and never `execProtocolRaw` (D7, SPEC.md §12.2): the Raw
// variants bypass the wrappers that manage notification listeners, which is the
// machinery DBOS depends on for queue dispatch.
func (b *backend) exec(ctx context.Context, msg []byte) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("execProtocol panicked: %v", r)
		}
	}()

	buf := js.Global().Get("Uint8Array").New(len(msg))
	js.CopyBytesToJS(buf, msg)

	result, err := await(ctx, b.pglite.Call("execProtocol", buf, execOptions))
	if err != nil {
		return nil, err
	}

	raw, shape, err := decodeExecProtocolResult(result)
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	b.roundTrips++
	b.resultShapes[shape]++
	b.inlineFrames += countFrames(raw, beNotificationResponse)
	b.mu.Unlock()

	return raw, nil
}

// subscribe wires PGlite's onNotification into the multiplexer, which is its
// only notification ingress, and counts the callbacks on the way past.
//
// A bundle without the callback is not an error: DBOS falls back to polling its
// queues, which is what failure-matrix row F21 asserts still works.
func (b *backend) subscribe(m *wasmpg.Multiplexer) (release func()) {
	if b.pglite.Get("onNotification").Type() != js.TypeFunction {
		return func() {}
	}

	cb := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) < 1 || args[0].Type() != js.TypeString {
			return nil
		}
		payload := ""
		if len(args) > 1 && args[1].Type() == js.TypeString {
			payload = args[1].String()
		}

		b.mu.Lock()
		b.callbackHits++
		b.mu.Unlock()

		m.Notify(args[0].String(), payload)
		return nil
	})

	// onNotification returns an unsubscribe function in the releases that have
	// one. Calling it is best-effort; releasing the js.Func is not.
	unsubscribe := b.pglite.Call("onNotification", cb)
	return func() {
		if unsubscribe.Type() == js.TypeFunction {
			unsubscribe.Invoke()
		}
		cb.Release()
	}
}

// hasOnNotification reports whether the vendored bundle exposes the callback at
// all. Reported on the page, because "notifications never arrive" and "there is
// no callback to arrive on" are different bugs.
func (b *backend) hasOnNotification() bool {
	return b.pglite.Get("onNotification").Type() == js.TypeFunction
}

// observations is a snapshot of the tap. Deltas between two snapshots are what
// [notifyProbe] reasons about.
type observations struct {
	RoundTrips   int            `json:"roundTrips"`
	ResultShapes map[string]int `json:"resultShapes"`
	InlineFrames int            `json:"inlineNotificationFrames"`
	CallbackHits int            `json:"onNotificationCallbacks"`
}

func (b *backend) observe() observations {
	b.mu.Lock()
	defer b.mu.Unlock()

	shapes := make(map[string]int, len(b.resultShapes))
	for k, v := range b.resultShapes {
		shapes[k] = v
	}
	return observations{
		RoundTrips:   b.roundTrips,
		ResultShapes: shapes,
		InlineFrames: b.inlineFrames,
		CallbackHits: b.callbackHits,
	}
}

// The three result shapes `execProtocol` has had across PGlite releases. The
// names are what gets reported on the page and in the browser test's DOM dump,
// so they are stable strings rather than an enum nobody outside Go can read.
const (
	shapeUint8Array = "uint8array"        // the whole result as one Uint8Array
	shapeDataObject = "object.data"       // { messages, data }
	shapeTupleArray = "array-of-tuples"   // [ [BackendMessage, Uint8Array], … ]
	shapeByteArrays = "array-of-uint8"    // [ Uint8Array, … ]
	shapeEmpty      = "null-or-undefined" // nothing to decode
)

// decodeExecProtocolResult extracts the raw backend bytes, and reports which
// shape they came in.
//
// It mirrors wasmpg's own decoder deliberately: if the two ever disagree, the
// page says so, which is more useful than a demo that works while the transport
// silently does something else. An unrecognised shape is an error and never
// empty bytes — empty bytes present as a hung query.
func decodeExecProtocolResult(v js.Value) ([]byte, string, error) {
	if v.IsUndefined() || v.IsNull() {
		return nil, shapeEmpty, nil
	}

	if isUint8Array(v) {
		return copyBytes(v), shapeUint8Array, nil
	}

	if v.Type() == js.TypeObject {
		if data := v.Get("data"); isUint8Array(data) {
			return copyBytes(data), shapeDataObject, nil
		}
	}

	if isArray(v) {
		var (
			out    []byte
			tuples bool
		)
		for i := 0; i < v.Length(); i++ {
			elem := v.Index(i)
			switch {
			case isUint8Array(elem):
				out = append(out, copyBytes(elem)...)
			case isArray(elem) && elem.Length() >= 2 && isUint8Array(elem.Index(1)):
				out = append(out, copyBytes(elem.Index(1))...)
				tuples = true
			}
		}
		if tuples {
			return out, shapeTupleArray, nil
		}
		return out, shapeByteArrays, nil
	}

	return nil, "", fmt.Errorf("unrecognised execProtocol result of type %s", v.Type())
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

// beNotificationResponse is the backend message type for an asynchronous
// notification (SPEC.md §12.4).
const beNotificationResponse = 'A'

// countFrames counts backend messages of one type in a raw protocol stream.
//
// A backend message is a type byte followed by a big-endian int32 length that
// counts itself but not the type byte. A malformed or truncated tail stops the
// walk rather than guessing: the caller only wants a count, and an over-count
// from misreading a payload as a header would be worse than a short one.
func countFrames(buf []byte, typ byte) int {
	n, i := 0, 0
	for i+5 <= len(buf) {
		length := int(binary.BigEndian.Uint32(buf[i+1 : i+5]))
		if length < 4 || i+1+length > len(buf) {
			return n
		}
		if buf[i] == typ {
			n++
		}
		i += 1 + length
	}
	return n
}
