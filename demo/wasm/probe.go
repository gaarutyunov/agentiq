//go:build js && wasm

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// probeChannel is the LISTEN channel the notification probe uses. It is
// deliberately not a channel DBOS uses, so the probe cannot consume a
// notification the runtime was waiting for.
const probeChannel = "agentiq_notify_probe"

// probeTimeout bounds the wait for a notification that may never come — which
// is the interesting outcome, not a fault.
const probeTimeout = 10 * time.Second

// notifyProbe is the answer to the one question SPEC.md §12.4 left open and no
// unit test can reach: does a NotificationResponse arrive through PGlite's
// `onNotification` callback, inline inside the `execProtocol` result of the
// statement that caused it, or both?
//
// It matters because [wasmpg.Config.RouteInlineNotifications] defaults to
// *dropping* the inline copy, on the assumption that the callback will deliver
// it. If the callback never fires, that default loses every notification
// silently: DBOS's queue dispatch degrades to polling and the only symptom is
// slowness (failure-matrix row F21 by accident rather than by test).
//
// The procedure is the smallest one that separates the cases:
//
//	conn A: LISTEN <probeChannel>          -- registers in the routing table
//	conn A: WaitForNotification            -- parks, holding no backend lock
//	conn B: NOTIFY <probeChannel>, 'ping'  -- one execProtocol round trip
//
// and then reads three numbers off the tap in [backend]: whether pgx delivered
// the notification, how many times `onNotification` fired, and how many
// NotificationResponse frames appeared inside `execProtocol` results.
type notifyProbe struct {
	// Ran is false if the probe could not be attempted at all.
	Ran bool `json:"ran"`
	// RouteInline is the [wasmpg.Config.RouteInlineNotifications] the page ran
	// with, because the delivered/not-delivered outcome depends on it.
	RouteInline bool `json:"routeInlineNotifications"`
	// HasCallback reports whether the bundle exposes `onNotification` at all.
	HasCallback bool `json:"pgliteExposesOnNotification"`
	// Delivered is whether pgx surfaced the notification on the listening
	// connection — the end-to-end result DBOS depends on.
	Delivered bool `json:"deliveredToPgx"`
	// CallbackHits and InlineFrames are the deltas across the probe.
	CallbackHits int `json:"onNotificationCallbacks"`
	InlineFrames int `json:"inlineNotificationFrames"`
	// Verdict is the plain-language conclusion. It is the point of the probe.
	Verdict string `json:"verdict"`
	// Err is set when the probe could not complete; Verdict then says so.
	Err string `json:"error,omitempty"`
}

// runNotifyProbe executes the probe against a live pool.
//
// It must run *before* dbos.Launch: the listener DBOS parks never gives its
// connection back, and the probe wants two of a budget of four
// (SPEC.md §12.4). It releases both before returning.
//
// The two statements below are the only hand-written SQL in the browser build.
// SPEC.md §21's rule is about persistence — the domain's reads and writes come
// from the SDL through the generated client — and this is diagnostics against
// the transport, the same exemption test/drift has for introspecting `dbos.*`
// (SPEC.md §17.4). Neither statement touches a table.
func runNotifyProbe(ctx context.Context, pool *pgxpool.Pool, be *backend, routeInline bool) notifyProbe {
	p := notifyProbe{
		RouteInline: routeInline,
		HasCallback: be.hasOnNotification(),
	}

	before := be.observe()

	listener, err := pool.Acquire(ctx)
	if err != nil {
		p.Err = fmt.Sprintf("acquire the listening connection: %v", err)
		p.Verdict = "not run: no connection to listen on"
		return p
	}
	defer listener.Release()

	if _, err := listener.Exec(ctx, "LISTEN "+probeChannel); err != nil {
		p.Err = fmt.Sprintf("LISTEN: %v", err)
		p.Verdict = "not run: LISTEN failed"
		return p
	}
	// UNLISTEN on the way out so the routing table does not keep an entry for
	// a channel nothing will notify again.
	defer func() {
		unlisten, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
		defer cancel()
		_, _ = listener.Exec(unlisten, "UNLISTEN "+probeChannel)
	}()

	p.Ran = true

	waitCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	got := make(chan error, 1)
	go func() {
		_, err := listener.Conn().WaitForNotification(waitCtx)
		got <- err
	}()

	// Give the LISTEN's round trip a moment to land before notifying. The
	// routing table is updated when the statement is written rather than when
	// the backend confirms it, so this is belt and braces — but a probe that
	// races its own setup would report a false negative on the one question it
	// exists to answer.
	time.Sleep(100 * time.Millisecond)

	notifier, err := pool.Acquire(ctx)
	if err != nil {
		p.Err = fmt.Sprintf("acquire the notifying connection: %v", err)
		p.Verdict = "not run: no second connection to notify from"
		return p
	}
	defer notifier.Release()

	if _, err := notifier.Exec(ctx, "NOTIFY "+probeChannel+", 'ping'"); err != nil {
		p.Err = fmt.Sprintf("NOTIFY: %v", err)
		p.Verdict = "not run: NOTIFY failed"
		return p
	}

	switch err := <-got; {
	case err == nil:
		p.Delivered = true
	case waitCtx.Err() != nil:
		p.Delivered = false
	default:
		p.Err = fmt.Sprintf("WaitForNotification: %v", err)
	}

	after := be.observe()
	p.CallbackHits = after.CallbackHits - before.CallbackHits
	p.InlineFrames = after.InlineFrames - before.InlineFrames
	p.Verdict = verdict(p)
	return p
}

// verdict turns the three numbers into the sentence someone reading the page —
// or the browser test's DOM dump — actually needs.
func verdict(p notifyProbe) string {
	switch {
	case !p.HasCallback:
		return "PGlite exposes no onNotification: RouteInlineNotifications must be true, " +
			"or DBOS runs on its polling fallback (F21)"

	case p.CallbackHits > 0 && p.InlineFrames == 0:
		return "onNotification fired and no NotificationResponse appeared inline: " +
			"wasmpg's default (RouteInlineNotifications=false) is correct, and dropping the inline copy drops nothing"

	case p.CallbackHits > 0 && p.InlineFrames > 0:
		return "onNotification fired AND a NotificationResponse appeared inline: both paths carry it, " +
			"so wasmpg's default (RouteInlineNotifications=false) is correct — enabling it would duplicate, which DBOS tolerates"

	case p.CallbackHits == 0 && p.InlineFrames > 0:
		return "onNotification did NOT fire and the NotificationResponse arrived inline only: " +
			"wasmpg's default is WRONG and RouteInlineNotifications must be true, or every notification is lost silently"

	case p.Delivered:
		// Delivery with neither counter moving would mean the notification
		// reached pgx by a route this probe does not model. Worth saying so
		// rather than picking one of the branches above.
		return "the notification reached pgx, but neither onNotification nor an inline frame was observed: " +
			"the transport is delivering by a path this probe does not model"

	default:
		return "no notification arrived by either path within the timeout: " +
			"LISTEN/NOTIFY does not cross the shim at all, and DBOS is on its polling fallback (F21)"
	}
}
