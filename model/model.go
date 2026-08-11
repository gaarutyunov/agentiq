// Package model constructs the LLM AgentIQ talks to (SPEC.md §9.7).
//
// It is ADK's `model/openaimodel` pointed at OpenRouter, and nothing more. It
// may import ADK's model packages and `net/http`; it must not import DBOS or
// `generated/client` (SPEC.md §5), which is what lets it be constructed and
// tested without a workflow or a database. Making a generation *durable* is
// `dbosadk`'s job, not this package's — a model that checkpointed itself could
// not be used outside a workflow at all.
//
// # Two ways to get a key, and only one of them is a secret
//
// On the server the key comes from `OPENROUTER_API_KEY` (§10.1) and is the
// operator's.
//
// In the browser there is no key to ship, deliberately (D19, §9.7): the demo is
// a public GitHub Pages site, and a key in its build is a disclosed credential
// the moment it deploys. Each user signs in through OpenRouter's PKCE flow and
// funds their own inference. See pkce.go.
//
// # Why the browser half is portable Go
//
// SPEC.md §9.7 places `PKCEFlow` in this package "js/wasm only". Everything
// about PKCE that is worth testing is portable, though — the verifier, the
// S256 challenge, the authorize URL and the token exchange are crypto and one
// HTTP round trip — so the js/wasm-only part is reduced to what is genuinely
// browser-shaped: reading the redirect back and holding the key. Those are
// behind [KeyStore], which `demo/wasm` implements over `sessionStorage`.
//
// That keeps two things true at once. §17.2's `no-js-outside-wasmpg` depguard
// rule stays as written rather than growing a fifth exemption for a package
// that does not need `syscall/js`; and the whole flow is exercised by
// `go test ./model` against an `httptest` server, on a machine with no browser.
package model

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"
)

const (
	// BaseURL is OpenRouter's OpenAI-compatible endpoint (SPEC.md §9.7).
	BaseURL = "https://openrouter.ai/api/v1"

	// APIKeyEnv is where the server reads its key from (SPEC.md §10.1). There
	// is deliberately no flag and no config file entry: a key on a command line
	// is a key in the process list.
	APIKeyEnv = "OPENROUTER_API_KEY"
)

// ErrNoAPIKey is returned when no key is available.
var ErrNoAPIKey = errors.New("model: no OpenRouter API key")

// ErrNoModel is returned when Config names no model.
var ErrNoModel = errors.New("model: no model name")

// Config describes the model to construct.
type Config struct {
	// Model is the OpenRouter model identifier, e.g. "openai/gpt-5". It is
	// required: OpenRouter has no default and a request without one is
	// rejected by the API rather than by us.
	Model string

	// APIKey authenticates the call. On the server it comes from
	// [APIKeyFromEnv]; in the browser it is the user's own key from the PKCE
	// exchange.
	APIKey string

	// BaseURL overrides [BaseURL]. The only caller that sets it is a test
	// pointing at a stub endpoint — which is how failure-matrix rows F6 and F7
	// (HTTP 529, malformed JSON) are driven without a network.
	BaseURL string

	// HTTPClient overrides the default. Under js/wasm Go's default transport is
	// already `fetch`, so the browser needs nothing here.
	HTTPClient *http.Client
}

// New constructs the model (SPEC.md §9.7).
//
// The returned `model.LLM` is not durable. Wrap it with `dbosadk.NewModel`
// before handing it to a Runner inside workflow code, or the generation will be
// re-executed on every replay.
func New(cfg Config) (adkmodel.LLM, error) {
	if cfg.Model == "" {
		return nil, ErrNoModel
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("%w: set %s on the server, or complete the PKCE exchange in the browser", ErrNoAPIKey, APIKeyEnv)
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = BaseURL
	}

	// openaimodel.NewModel takes a context and ignores it — its parameter is
	// named `_`. That is why this constructor takes none: threading one through
	// would promise a cancellation that nothing observes. Background is passed
	// rather than nil so the call stays correct if a later ADK release starts
	// using it.
	llm, err := openaimodel.NewModel(context.Background(), cfg.Model, &openaimodel.ClientConfig{
		APIKey:     cfg.APIKey,
		BaseURL:    baseURL,
		HTTPClient: cfg.HTTPClient,
	})
	if err != nil {
		return nil, fmt.Errorf("model: construct %q against %s: %w", cfg.Model, baseURL, err)
	}
	return llm, nil
}

// APIKeyFromEnv reads the server's key (SPEC.md §10.1).
//
// The browser never calls it: a WASM page has no environment, so it would
// always return the empty string and the failure would read as "the key is
// missing" rather than "this path does not exist here".
func APIKeyFromEnv() (string, error) {
	key := os.Getenv(APIKeyEnv)
	if key == "" {
		return "", fmt.Errorf("%w: %s is not set", ErrNoAPIKey, APIKeyEnv)
	}
	return key, nil
}
