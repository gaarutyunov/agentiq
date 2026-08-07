//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"syscall/js"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/gaarutyunov/gopgql/exec"
	"github.com/jackc/pgx/v5/pgxpool"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/gaarutyunov/agentiq/generated/client"
	"github.com/gaarutyunov/agentiq/model"
	"github.com/gaarutyunov/agentiq/workflow"
)

// SPEC.md §20 M2's browser demo: OpenRouter PKCE sign-in, one chat turn, and
// the session and its events rendered from the property graph.
//
// # No subscription, on purpose
//
// The transcript is read by re-querying `Query.session` after the turn, not by
// subscribing to anything. §20's preamble says so for every milestone before
// M7 specifically so that it does not look like an omission: the write side of
// the DBOS stream is M2 work (`dbosadk.NewModel`), and the read side —
// `ClientReadStream`, `Subscription.runEvents`, retention, offset deduplication
// — is M7's. Adding a websocket here to make the page feel live would be
// building M7 badly.
//
// # The page ships no secret (D19, §9.7)
//
// There is no key in this binary and no backend that holds one. The user
// completes OpenRouter's PKCE flow, the key it issues is theirs, and it lives in
// `sessionStorage` — never `localStorage`, which survives the tab and would
// leave a credential behind on a shared machine — and is cleared on sign-out.
//
// A shipped key on a public GitHub Pages site is a credential incident, not a
// convenience, which is why the demo is signed-out until the user acts.

// keyStorageKey is where the PKCE-issued key is held.
//
// `sessionStorage`, and the name says which: a reader checking D19 should not
// have to find the call site to know it is not `localStorage`.
const keyStorageKey = "agentiq.openrouter.key"

// verifierStorageKey holds the PKCE verifier across the redirect to OpenRouter
// and back. It is not a credential — it is the proof that the code being
// exchanged belongs to the flow this tab started, which is the whole of what
// PKCE adds — and it is deleted as soon as the exchange succeeds or fails.
const verifierStorageKey = "agentiq.openrouter.verifier"

// chat is the M2 turn: sign-in state, the workflow deps that a signed-in page
// gets, and the transcript.
type chat struct {
	app *app

	// sessionID is stable for the tab, so a second turn continues the first
	// one's conversation rather than starting a new session.
	sessionID string
}

func sessionStorage() js.Value { return js.Global().Get("sessionStorage") }

// storedKey reads the PKCE-issued key, or "" when the tab is signed out.
func storedKey() string {
	v := sessionStorage().Call("getItem", keyStorageKey)
	if v.IsNull() || v.IsUndefined() {
		return ""
	}
	return v.String()
}

func storeKey(key string) { sessionStorage().Call("setItem", keyStorageKey, key) }

// clearKey is sign-out. It removes the key and the verifier together: a
// verifier left behind would be a half-finished flow that the next sign-in
// would try to complete with a code it never requested.
func clearKey() {
	sessionStorage().Call("removeItem", keyStorageKey)
	sessionStorage().Call("removeItem", verifierStorageKey)
}

// callbackURL is this page's own URL, with the query stripped.
//
// OpenRouter redirects back to it with `?code=...`, so the URL registered for
// the flow must not already carry one — a second round trip would otherwise
// send the first round trip's code back as part of the callback.
func callbackURL() string {
	loc := js.Global().Get("location")
	return loc.Get("origin").String() + loc.Get("pathname").String()
}

// wireChat installs the chat panel's handlers and finishes a PKCE flow that
// this page was redirected back into.
func (a *app) wireChat(ctx context.Context) {
	c := &chat{app: a, sessionID: chatSessionID}
	a.chat = c

	a.ui.onClick("sign-in", func() { c.signIn() })
	a.ui.onClick("sign-out", func() { c.signOut(ctx) })
	a.ui.onClick("chat-send", func() { go c.send(ctx) })

	// A redirect back from OpenRouter carries the code. Completing it here,
	// during boot, is what makes the sign-in survive the navigation: the page
	// that started the flow is gone.
	if code := queryParam("code", ""); code != "" {
		go c.completeSignIn(ctx, code)
		return
	}
	c.applyKeyState(ctx, storedKey())
}

// signIn starts the PKCE flow: generate a verifier and a challenge, keep the
// verifier for the return trip, and navigate to OpenRouter.
//
// No client registration and no backend (D19). The verifier never leaves the
// tab; only its SHA-256 challenge does, which is what lets OpenRouter check on
// the way back that the code is being redeemed by whoever asked for it.
func (c *chat) signIn() {
	p, err := model.NewPKCE()
	if err != nil {
		c.app.ui.logf("sign-in failed to start: %v", err)
		return
	}
	sessionStorage().Call("setItem", verifierStorageKey, p.Verifier)
	js.Global().Get("location").Set("href", p.AuthorizeURL(callbackURL()))
}

// completeSignIn exchanges the code the redirect carried for a user-controlled
// key.
func (c *chat) completeSignIn(ctx context.Context, code string) {
	v := sessionStorage().Call("getItem", verifierStorageKey)
	if v.IsNull() || v.IsUndefined() || v.String() == "" {
		c.app.ui.logf("the redirect carried a code but this tab has no verifier; " +
			"start the sign-in again")
		c.applyKeyState(ctx, "")
		return
	}

	p := &model.PKCE{Verifier: v.String()}
	key, err := p.Exchange(ctx, code)
	// The verifier is spent either way. A verifier kept after a failed exchange
	// is one that a later attempt would reuse against a different code.
	sessionStorage().Call("removeItem", verifierStorageKey)
	if err != nil {
		c.app.ui.logf("the OpenRouter code exchange failed: %v", err)
		c.applyKeyState(ctx, "")
		return
	}

	storeKey(key)
	// Drop `?code=` from the address bar. It is spent, and leaving it there
	// means a reload tries to redeem it a second time.
	js.Global().Get("history").Call("replaceState", js.Null(), "", callbackURL())
	c.app.ui.logf("signed in to OpenRouter; the key is in sessionStorage for this tab only")
	c.applyKeyState(ctx, key)
}

// signOut clears the key and puts the page back into its signed-out state.
func (c *chat) signOut(ctx context.Context) {
	clearKey()
	c.app.ui.logf("signed out; the OpenRouter key is cleared")
	c.applyKeyState(ctx, "")
}

// applyKeyState re-registers the workflow for the key the tab now holds, and
// enables or disables the panel to match.
//
// Re-registration rather than a key read inside the turn: `Deps.NewModel` is
// captured once and a workflow that reached for `sessionStorage` mid-run would
// be reading the browser from inside workflow code — the same objection §9.1
// makes to `os.Getenv` on the server, and the analyzer would catch it.
func (c *chat) applyKeyState(ctx context.Context, key string) {
	signedIn := key != ""

	c.app.ui.enable("sign-in", !signedIn)
	c.app.ui.enable("sign-out", signedIn)
	c.app.ui.enable("chat-input", signedIn)
	c.app.ui.enable("chat-send", signedIn)

	if !signedIn {
		c.app.ui.setChat("not signed in", "info", false)
		// Back to the durability-only deps, so the rest of the page — the
		// workflow rows, the reload scenario — keeps working signed out.
		workflow.SetDeps(workflow.Deps{})
		return
	}

	// SetDeps and not Register. The workflow and its queue were registered at
	// boot, before Launch; registering a second time hangs the page at
	// "launching the DBOS queue worker" and the runtime never reports ready,
	// which is a boot failure and not a sign-in failure. See workflow.SetDeps.
	workflow.SetDeps(workflow.Deps{
		AppName:    appName,
		DataSource: c.app.dataSource,
		Handle:     exec.Pgx(c.app.pool),
		NewModel: func(modelID string) (adkmodel.LLM, error) {
			return model.New(model.Config{Model: modelID, APIKey: key})
		},
	})
	c.app.ui.setChat("signed in — send a message to run one turn", "success", true)
	go c.renderTranscript(ctx)
}

// send runs one turn and then reads the session back.
func (c *chat) send(ctx context.Context) {
	text := strings.TrimSpace(c.app.ui.inputValue("chat-input"))
	if text == "" {
		return
	}
	c.app.ui.enable("chat-send", false)
	defer c.app.ui.enable("chat-send", true)
	c.app.ui.setInputValue("chat-input", "")
	c.app.ui.setChatStatus("running one turn…", "info")

	msg, err := json.Marshal(genai.Content{
		Role:  "user",
		Parts: []*genai.Part{{Text: text}},
	})
	if err != nil {
		c.app.ui.logf("encoding the message failed: %v", err)
		return
	}

	h, err := workflow.Enqueue(c.app.dbosCtx, workflow.AgentRunInput{
		AgentDigest: fixtureAgentDigest,
		AppName:     appName,
		UserID:      chatUserID,
		SessionID:   c.sessionID,
		Message:     msg,
	})
	if err != nil {
		c.app.ui.logf("enqueue failed: %v", err)
		c.app.ui.setChatStatus(fmt.Sprintf("the turn could not be enqueued: %v", err), "danger")
		return
	}

	out, err := h.GetResult()
	if err != nil {
		c.app.ui.logf("the turn failed: %v", err)
		c.app.ui.setChatStatus(fmt.Sprintf("the turn failed: %v", err), "danger")
		c.renderTranscript(ctx)
		return
	}
	if out.ErrorCode != "" {
		c.app.ui.setChatStatus("the turn ended with "+out.ErrorCode+
			" — the reason is the last event below", "danger")
	} else {
		c.app.ui.setChatStatus("turn complete — the transcript below is read back from the graph", "success")
	}
	c.renderTranscript(ctx)
	go c.app.refresh(ctx)
}

// renderTranscript reads the session from the property graph and draws it.
//
// From the graph, not from what `send` had in hand: the point of D3 is that an
// event is queryable rows rather than a blob, and a page that rendered the
// value it just sent would be demonstrating nothing about the storage.
func (c *chat) renderTranscript(ctx context.Context) {
	rows, err := client.New().Session(ctx, exec.Pgx(c.app.pool), client.SessionInput{
		AppName: appName, UserId: chatUserID, AdkId: c.sessionID,
	})
	if err != nil {
		c.app.ui.logf("reading the session back failed: %v", err)
		return
	}
	if len(rows) == 0 {
		c.app.ui.renderEvents(nil)
		return
	}

	events := make([]eventRow, 0, len(rows[0].Events))
	for _, e := range rows[0].Events {
		events = append(events, eventRow{
			Sequence: e.Sequence,
			Author:   e.Author,
			Parts:    describeParts(e),
		})
	}
	c.app.ui.renderEvents(events)
}

// eventRow is one row of the transcript table.
type eventRow struct {
	Sequence int64
	Author   string
	Parts    string
}

// describeParts renders an event's parts the way the schema stores them: one
// wide row per part (D4), several fields of which may be set at once.
func describeParts(e client.SessionSessionEvents) string {
	if e.ErrorCode != nil && *e.ErrorCode != "" {
		msg := ""
		if e.ErrorMessage != nil {
			msg = ": " + *e.ErrorMessage
		}
		return "error " + *e.ErrorCode + msg
	}

	var out []string
	// Ordered by part_index, which is what §7.2 rule 2 makes the read-back
	// order. The traversal orders by key columns and Part's key is a surrogate
	// uuid, so without this the parts arrive in no order at all.
	parts := make([]client.SessionSessionEventsParts, len(e.Parts))
	copy(parts, e.Parts)
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j].PartIndex < parts[j-1].PartIndex; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}

	for _, p := range parts {
		switch {
		case p.Thought != nil && *p.Thought:
			out = append(out, "(thought)")
		case p.FunctionCallName != nil && *p.FunctionCallName != "":
			out = append(out, "call "+*p.FunctionCallName)
		case p.Text != nil && *p.Text != "":
			out = append(out, *p.Text)
		}
	}
	if len(out) == 0 {
		return "—"
	}
	return strings.Join(out, " ")
}

// chatSessionID is the conversation this tab continues.
//
// Fixed rather than generated, so a reload lands back in the same conversation
// — which is the point of persisting it. A second tab shares it too, and that
// is honest: they share one PGlite database, so they were always going to.
const chatSessionID = "browser-session"

// chatUserID is the user the browser turns run as. There is no sign-in beyond
// OpenRouter's, so there is one user per tab and naming it is more honest than
// inventing an identity the page does not have.
const chatUserID = "browser"

// fixtureAgentDigest is the agent seeded by migrate/sql/0004 (D24). It is a
// literal rather than a lookup because the migration is what puts the row
// there, and a page that searched for "some agent" would run whichever one it
// found.
const fixtureAgentDigest = "sha256:0f0e4d9a7b1c2e3f4a5b6c7d8e9f0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b"

// dataSourceFor builds the DBOS DataSource a chat turn transacts on (D2).
//
// It is built at boot and not at sign-in because `dbos.NewDataSource` creates
// its own `transaction_completion` table — migration work, which belongs with
// the rest of the migration rather than at the first message.
func dataSourceFor(dctx dbos.Context, pool *pgxpool.Pool) (*dbos.DataSource, error) {
	ds, err := dbos.NewDataSource(dctx, pool)
	if err != nil {
		return nil, fmt.Errorf("data source: %w", err)
	}
	return ds, nil
}
