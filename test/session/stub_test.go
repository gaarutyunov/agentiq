//go:build integration

package session_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A stub OpenRouter endpoint (SPEC.md §14, §19 rows F6 and F7).
//
// OpenRouter is OpenAI-compatible and `model/` points ADK's `openaimodel` at it
// by BaseURL alone (§9.7), so a stub is an `httptest.Server` and `Config.BaseURL`
// — no network, no key, no rewriting of the model package to be testable.
//
// It counts requests, which is the only externally visible evidence that a step
// retried: DBOS's retry is invisible from inside the step, and `operation_outputs`
// records the outcome rather than the attempts.

// stub is an OpenRouter stand-in with a scripted response.
type stub struct {
	*httptest.Server
	requests atomic.Int64
}

// Requests is how many times the model actually called out.
func (s *stub) Requests() int { return int(s.requests.Load()) }

// newStub starts a stub that answers every request with respond.
func newStub(t *testing.T, respond http.HandlerFunc) *stub {
	t.Helper()
	s := &stub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		respond(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// streamingText answers with `deltas` text deltas followed by a completed
// event — the §15 partial-skip scenario's model.
//
// It counts *deltas*, not partial responses, and the two differ by one:
// measured against ADK v2.1.0, five deltas produce six partial `LLMResponse`s,
// because the completion event contributes one of its own before the final
// response. The caller chooses deltas and the scenario counts what reached the
// stream, so the translation stays at the call site where it can be seen.
//
// The wire format is the OpenAI Responses streaming API, which is what
// `openaimodel` speaks: `response.created`, then one
// `response.output_text.delta` per partial, then `response.completed`. ADK
// translates each delta into an `LLMResponse` with `Partial` set and the
// completed event into the final one — which is exactly the split
// `dbosadk.NewModel` acts on, partials to the stream and only the final one
// through the checkpoint (§9.4).
func streamingText(deltas int, text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// No `response.created`: a real endpoint sends it first, ADK translates
		// it into an `LLMResponse` of its own, and it is not something the §15
		// scenario is counting.
		var events []string
		for i := range deltas {
			events = append(events, fmt.Sprintf(
				`{"type":"response.output_text.delta","delta":%q}`, fmt.Sprintf("%s%d ", text, i)))
		}
		events = append(events,
			`{"type":"response.completed","response":{"id":"resp_stub","model":"stub-model","usage":{"total_tokens":7}}}`)

		flusher, _ := w.(http.Flusher)
		for _, e := range events {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", e)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// overloaded is failure-matrix row F6: HTTP 529, which is what OpenRouter
// returns when the upstream provider is saturated.
func overloaded() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		// 529 is not one of net/http's constants — it is not in any RFC. That
		// is precisely why the row exists: a status the client library has no
		// special handling for has to be retried by the step policy or not at
		// all.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded","type":"server_error"}}`))
	}
}

// malformed is failure-matrix row F7: a 200 whose body is not the JSON the
// client expects.
//
// It is a valid HTTP response with an invalid payload on purpose. A transport
// error and a parse error take different paths through the client, and the row
// is about the second: the step has to fail on a response it received.
func malformed() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("}{ not json ", 3)))
	}
}
