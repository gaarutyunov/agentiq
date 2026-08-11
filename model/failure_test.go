package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Failure-matrix rows F6 and F7 (SPEC.md §19), at the layer this package owns.
//
// The acceptance names the test mechanism for both rows as "stub OpenRouter
// endpoint", and that is what these are. What they assert is the half of each
// row that is a property of the model call: a 529 and a malformed body both
// have to surface as an *error* from `GenerateContent`, because
// `dbosadk.NewModel` turns exactly that into a failed step, and a step that
// does not fail is a step DBOS does not retry.
//
// The other half of each row — that the step is then retried five times at base
// 1s and that the final failure lands in the session as an event with an
// errorCode — needs a DBOS context and the `agentiq.event` table, and is in
// features/session_persistence.feature behind @needs-gopgql.
//
// Between them the two halves are the row. Splitting them this way means the
// part that can be checked on any machine is checked on every `go test ./...`,
// rather than the whole row waiting on a release.

// stubOpenRouter serves one canned reply to whatever the model asks for.
//
// It answers every path rather than only `/chat/completions`: which endpoint
// ADK's `openaimodel` calls is ADK's business, and a stub that 404'd an
// unexpected path would fail these tests for a reason that has nothing to do
// with F6 or F7.
func stubOpenRouter(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// generate runs one non-streaming generation against the stub and returns the
// first error the sequence yields.
func generate(t *testing.T, llm adkmodel.LLM) error {
	t.Helper()
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{{
			Role:  "user",
			Parts: []*genai.Part{{Text: "hello"}},
		}},
	}
	for resp, err := range llm.GenerateContent(context.Background(), req, false) {
		if err != nil {
			return err
		}
		_ = resp
	}
	return nil
}

// TestF6ModelReturns529 — failure matrix row F6.
//
// 529 is OpenRouter's "the upstream provider is overloaded". It is a
// *transient* failure, which is the whole reason §9.2 gives the model step five
// attempts at base 1s rather than failing the turn on the first one — so what
// matters here is that it arrives as an error at all. A client that swallowed
// it into an empty response would produce a step that succeeded with no
// content, and the turn would proceed on nothing.
func TestF6ModelReturns529(t *testing.T) {
	srv := stubOpenRouter(t, 529, `{"error":{"message":"provider overloaded","type":"server_error"}}`)

	llm, err := New(Config{Model: "openai/gpt-5", APIKey: "test", BaseURL: srv.URL})
	require.NoError(t, err)

	err = generate(t, llm)
	require.Error(t, err, "a 529 must surface as an error, or dbosadk.NewModel has nothing to retry")
	assert.Contains(t, err.Error(), "529", "the status has to reach the error, or the retry decision is blind")
}

// TestF7ModelReturnsMalformedJSON — failure matrix row F7.
//
// A 200 with a body that is not the expected document is the harder of the two
// rows: the transport succeeded, so anything that checked only the status code
// would report success and hand the turn a zero-valued response.
func TestF7ModelReturnsMalformedJSON(t *testing.T) {
	srv := stubOpenRouter(t, http.StatusOK, `{"choices": [ this is not json`)

	llm, err := New(Config{Model: "openai/gpt-5", APIKey: "test", BaseURL: srv.URL})
	require.NoError(t, err)

	err = generate(t, llm)
	require.Error(t, err, "a malformed body must surface as an error, not as an empty response")
}

// TestAValidResponseIsNotAnError is what stops the two tests above from passing
// against a client that failed on everything.
//
// Without it, a `New` that returned a model erroring unconditionally would
// satisfy both F6 and F7 while being useless.
func TestAValidResponseIsNotAnError(t *testing.T) {
	// This is an OpenAI *Responses* API document, not a chat-completions one.
	// ADK's `openaimodel` talks to `/responses`, which the first version of this
	// test got wrong — it sent a `choices` array and the client answered
	// "openai: response included no output items". That failure is the reason
	// this test is here: F6 and F7 both assert that something errors, and
	// without a case that must *not* error they would have passed just as
	// happily against a stub the client could never parse.
	srv := stubOpenRouter(t, http.StatusOK, `{
	  "id": "resp_1",
	  "object": "response",
	  "model": "openai/gpt-5",
	  "status": "completed",
	  "output": [{
	    "type": "message",
	    "id": "msg_1",
	    "role": "assistant",
	    "status": "completed",
	    "content": [{"type": "output_text", "text": "hello back", "annotations": []}]
	  }]
	}`)

	llm, err := New(Config{Model: "openai/gpt-5", APIKey: "test", BaseURL: srv.URL})
	require.NoError(t, err)

	assert.NoError(t, generate(t, llm))
}
