package model

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// The OpenRouter PKCE endpoints (SPEC.md §9.7, D19).
//
// AuthPath is a page the user is sent to; KeysPath is the exchange. Neither is
// under [BaseURL] — the authorize page is on the site, not the API — so they
// are absolute and separate.
const (
	AuthURL = "https://openrouter.ai/auth"

	// KeysPath is appended to the API base, so a test can point the exchange at
	// a stub by setting [PKCE.BaseURL] while leaving the real authorize URL
	// alone.
	KeysPath = "/auth/keys"

	// ChallengeMethod is S256. OpenRouter also accepts a plain challenge; a
	// plain challenge is the verifier itself, sent in the clear in the
	// redirect, which defeats the point of the exchange.
	ChallengeMethod = "S256"

	// verifierBytes is the entropy behind the verifier. RFC 7636 allows 43 to
	// 128 characters after base64url encoding; 32 bytes lands at 43, the
	// minimum length and the maximum entropy that fits it.
	verifierBytes = 32
)

// ErrNoCode is returned when the redirect carried no authorization code.
var ErrNoCode = errors.New("model: the OpenRouter redirect carried no code")

// KeyStore holds the user's OpenRouter key between page loads.
//
// It is an interface because *where* the key is held is the one genuinely
// browser-shaped part of the flow, and because the requirement on it is a
// prohibition rather than a behaviour: the key goes in `sessionStorage` and
// never in `localStorage` (D19, §9.7). `localStorage` outlives the tab, so a
// key put there is a credential left on a shared machine after the user walked
// away; `sessionStorage` dies with the tab.
//
// `demo/wasm` supplies the only implementation that ships, which is what makes
// that prohibition checkable by reading one file rather than by grepping for
// the wrong API.
type KeyStore interface {
	// Key returns the stored key, and whether there was one.
	Key() (string, bool)
	// SetKey stores the key.
	SetKey(key string) error
	// Clear removes it. This is sign-out, and it must leave nothing behind.
	Clear() error
}

// PKCE is one authorization attempt (SPEC.md §9.7, D19).
//
// The verifier is generated once, kept for the length of the redirect, and sent
// exactly once — to the exchange, never to the authorize page. The value that
// crosses the redirect is the challenge, which cannot be reversed into it.
//
// There is no client registration and no backend in this flow. That is the
// whole reason it can run on a static GitHub Pages site: the user authorizes
// AgentIQ against their own OpenRouter account, OpenRouter mints a key scoped
// to them, and the key never touches a server AgentIQ operates.
type PKCE struct {
	// Verifier is the secret half. It must not be logged, put in a URL that is
	// navigated to, or stored anywhere that outlives the exchange.
	Verifier string

	// Challenge is the public half — the base64url-encoded SHA-256 of the
	// verifier.
	Challenge string

	// BaseURL overrides [BaseURL] for the exchange only. Tests set it; nothing
	// else does.
	BaseURL string

	// HTTPClient overrides the default client for the exchange.
	HTTPClient *http.Client
}

// NewPKCE generates a verifier and its challenge.
func NewPKCE() (*PKCE, error) {
	raw := make([]byte, verifierBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("model: generate a PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))

	return &PKCE{
		Verifier: verifier,
		// The challenge hashes the *encoded* verifier, not the raw bytes:
		// RFC 7636 defines it as S256(ASCII(code_verifier)), and the verifier
		// is the encoded string. Hashing the raw bytes instead produces a
		// challenge the server cannot reproduce, and the failure arrives at the
		// exchange as a flat "invalid grant" with nothing pointing here.
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// AuthorizeURL is where the browser sends the user.
//
// callbackURL is the page OpenRouter redirects back to with `?code=`. It is the
// demo's own URL, which is why it is a parameter: under `/pr-preview/pr-N/` the
// page is not at the site root (SPEC.md §13.3), and a hardcoded callback would
// send every preview's users to production.
func (p *PKCE) AuthorizeURL(callbackURL string) string {
	q := url.Values{
		"callback_url":          {callbackURL},
		"code_challenge":        {p.Challenge},
		"code_challenge_method": {ChallengeMethod},
	}
	return AuthURL + "?" + q.Encode()
}

// CodeFromRedirect pulls the authorization code out of the URL OpenRouter
// redirected back to.
//
// It takes the whole URL rather than a parsed query so the caller does not have
// to decide which of `location.search` and `location.hash` the code arrives in
// — it is the query, and stating that here once is better than each caller
// guessing.
func CodeFromRedirect(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("model: parse the redirect URL: %w", err)
	}
	code := u.Query().Get("code")
	if code == "" {
		return "", ErrNoCode
	}
	return code, nil
}

// keysRequest is the exchange body. OpenRouter's `POST /api/v1/auth/keys` takes
// the code and the verifier and returns a key.
type keysRequest struct {
	Code                string `json:"code"`
	CodeVerifier        string `json:"code_verifier"`
	CodeChallengeMethod string `json:"code_challenge_method"`
}

type keysResponse struct {
	Key string `json:"key"`
}

// Exchange trades the authorization code for a user-controlled OpenRouter key.
//
// The returned key belongs to the user, not to AgentIQ: it is billed to their
// account and revocable from their dashboard. Store it with a [KeyStore] and
// clear it on sign-out.
func (p *PKCE) Exchange(ctx context.Context, code string) (string, error) {
	if code == "" {
		return "", ErrNoCode
	}
	if p.Verifier == "" {
		// A verifier that did not survive the redirect is the common failure
		// here — the page reloaded and nothing kept it — and without this the
		// symptom is an opaque rejection from OpenRouter.
		return "", errors.New("model: no PKCE verifier; it must survive the redirect")
	}

	base := p.BaseURL
	if base == "" {
		base = BaseURL
	}

	body, err := json.Marshal(keysRequest{
		Code:                code,
		CodeVerifier:        p.Verifier,
		CodeChallengeMethod: ChallengeMethod,
	})
	if err != nil {
		return "", fmt.Errorf("model: encode the exchange request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+KeysPath, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("model: build the exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("model: exchange the authorization code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read before checking the status: OpenRouter puts the reason in the body,
	// and a bare status code turns every misconfiguration into "401".
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("model: read the exchange response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("model: exchange the authorization code: %s: %s", resp.Status, bytes.TrimSpace(raw))
	}

	var out keysResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("model: decode the exchange response: %w", err)
	}
	if out.Key == "" {
		return "", errors.New("model: the exchange returned no key")
	}
	return out.Key, nil
}
