package model

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRejectsMissingInputs(t *testing.T) {
	_, err := New(Config{APIKey: "sk-test"})
	require.ErrorIs(t, err, ErrNoModel)

	_, err = New(Config{Model: "openai/gpt-5"})
	require.ErrorIs(t, err, ErrNoAPIKey)
	// The message has to say which of the two paths the caller is on, because
	// the fix is completely different: an environment variable on the server,
	// a sign-in in the browser.
	assert.Contains(t, err.Error(), APIKeyEnv)
}

func TestNewSucceeds(t *testing.T) {
	llm, err := New(Config{Model: "openai/gpt-5", APIKey: "sk-test"})
	require.NoError(t, err)
	require.NotNil(t, llm)
	assert.Equal(t, "openai/gpt-5", llm.Name())
}

// TestAPIKeyFromEnv covers SPEC.md §10.1's server path. t.Setenv is used rather
// than os.Setenv so the variable is restored and the test can run in any order.
func TestAPIKeyFromEnv(t *testing.T) {
	t.Setenv(APIKeyEnv, "")
	_, err := APIKeyFromEnv()
	require.ErrorIs(t, err, ErrNoAPIKey)

	t.Setenv(APIKeyEnv, "sk-or-v1-test")
	key, err := APIKeyFromEnv()
	require.NoError(t, err)
	assert.Equal(t, "sk-or-v1-test", key)
}

// TestBaseURLIsOpenRouter pins SPEC.md §9.7's endpoint. It is a constant rather
// than an assertion about behaviour because pointing the model at the wrong
// OpenAI-compatible host is a mistake that works — it just bills someone else's
// account and fails on a model name OpenRouter has and they do not.
func TestBaseURLIsOpenRouter(t *testing.T) {
	assert.Equal(t, "https://openrouter.ai/api/v1", BaseURL)
}

// --- PKCE -------------------------------------------------------------------

func TestNewPKCEProducesAnS256Challenge(t *testing.T) {
	p, err := NewPKCE()
	require.NoError(t, err)

	// RFC 7636 puts the verifier between 43 and 128 characters.
	assert.GreaterOrEqual(t, len(p.Verifier), 43)
	assert.LessOrEqual(t, len(p.Verifier), 128)

	// The challenge hashes the encoded verifier, not the bytes behind it.
	// Getting that wrong produces a challenge OpenRouter cannot reproduce and
	// an exchange that fails with nothing pointing at the cause.
	sum := sha256.Sum256([]byte(p.Verifier))
	assert.Equal(t, base64.RawURLEncoding.EncodeToString(sum[:]), p.Challenge)

	// base64url, so it survives a query string unescaped.
	assert.NotContains(t, p.Verifier, "+")
	assert.NotContains(t, p.Verifier, "/")
	assert.NotContains(t, p.Verifier, "=")
}

func TestNewPKCEIsNotDeterministic(t *testing.T) {
	a, err := NewPKCE()
	require.NoError(t, err)
	b, err := NewPKCE()
	require.NoError(t, err)
	assert.NotEqual(t, a.Verifier, b.Verifier, "two attempts must not share a verifier")
}

func TestAuthorizeURLCarriesTheChallengeAndNotTheVerifier(t *testing.T) {
	p, err := NewPKCE()
	require.NoError(t, err)

	raw := p.AuthorizeURL("https://agentiq.example/pr-preview/pr-7/")
	u, err := url.Parse(raw)
	require.NoError(t, err)

	assert.Equal(t, "https://openrouter.ai/auth", u.Scheme+"://"+u.Host+u.Path)
	q := u.Query()
	assert.Equal(t, p.Challenge, q.Get("code_challenge"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.Equal(t, "https://agentiq.example/pr-preview/pr-7/", q.Get("callback_url"))

	// The whole point of PKCE. If the verifier crosses the redirect, anything
	// that can read the URL can complete the exchange.
	assert.NotContains(t, raw, p.Verifier)
}

func TestCodeFromRedirect(t *testing.T) {
	code, err := CodeFromRedirect("https://agentiq.example/?code=abc123&other=1")
	require.NoError(t, err)
	assert.Equal(t, "abc123", code)

	_, err = CodeFromRedirect("https://agentiq.example/")
	require.ErrorIs(t, err, ErrNoCode)
}

func TestExchangeSendsTheVerifierAndReturnsTheKey(t *testing.T) {
	var got keysRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, KeysPath, r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		_ = json.NewEncoder(w).Encode(keysResponse{Key: "sk-or-v1-user"})
	}))
	defer srv.Close()

	p, err := NewPKCE()
	require.NoError(t, err)
	p.BaseURL = srv.URL

	key, err := p.Exchange(context.Background(), "the-code")
	require.NoError(t, err)
	assert.Equal(t, "sk-or-v1-user", key)

	assert.Equal(t, "the-code", got.Code)
	assert.Equal(t, p.Verifier, got.CodeVerifier)
	assert.Equal(t, "S256", got.CodeChallengeMethod)
}

func TestExchangeRejectsMissingInputs(t *testing.T) {
	p, err := NewPKCE()
	require.NoError(t, err)

	_, err = p.Exchange(context.Background(), "")
	require.ErrorIs(t, err, ErrNoCode)

	// A verifier that did not survive the redirect is the common failure, and
	// it must not be reported as an authorization problem.
	_, err = (&PKCE{}).Exchange(context.Background(), "the-code")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verifier")
}

func TestExchangeSurfacesTheServerReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"code expired"}`))
	}))
	defer srv.Close()

	p, err := NewPKCE()
	require.NoError(t, err)
	p.BaseURL = srv.URL

	_, err = p.Exchange(context.Background(), "the-code")
	require.Error(t, err)
	// A bare status turns every misconfiguration into "401".
	assert.Contains(t, err.Error(), "code expired")
}

func TestExchangeRejectsAnEmptyKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p, err := NewPKCE()
	require.NoError(t, err)
	p.BaseURL = srv.URL

	_, err = p.Exchange(context.Background(), "the-code")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no key")
}
