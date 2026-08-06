//go:build integration

package session_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/gaarutyunov/gopgql/exec"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/gaarutyunov/agentiq/generated/client"
	"github.com/gaarutyunov/agentiq/session"
	"github.com/gaarutyunov/agentiq/test/harness"
)

// D2, against a real `postgres:19beta2`.
//
// The property under test is not "the rows arrive" — a five-statement Go loop
// would manage that. It is that the append runs **inside the transaction DBOS
// opened**, so the event rows and the step checkpoint commit together or not at
// all. TestAFailedTransactionRollsBackTheAppend is the half that proves it: an
// append that survived its transaction's rollback would be an append on a
// connection of its own, and exactly-once would be a claim rather than a
// property.
//
// This is the capability gopgql v0.3.0 delivered. `exec.Handle` is defined over
// gopgql's own Cursor/Tag types now, so `dbos.Tx` satisfies it directly and the
// generated `AppendEvent` takes the handle `dbos.RunAsTransaction` hands in.

const appName = "agentiq-test"

// setup brings up Postgres, applies AgentIQ's migrations, and returns a DBOS
// context that can actually *run* a workflow.
//
// `harness.NewDBOSClient` deliberately cannot: it is an observer, so it can
// watch a run without claiming its queue row. `dbos.RunAsTransaction` needs a
// real workflow context, so this builds one the way `cmd/agentiq` does.
//
// The registered workflow is a single indirection — `runTx` calls whatever the
// test put in `body`. DBOS registers a workflow by the function's name, so a
// closure per test would either collide or register nothing; one named
// workflow with a swappable body is the shape that survives that.
func setup(t *testing.T) (context.Context, *pgxpool.Pool, dbos.Context) {
	t.Helper()
	ctx := context.Background()

	pg := harness.StartPostgres(ctx, t)
	pool := harness.OpenPool(ctx, t, pg.DSN)

	dctx, err := dbos.NewContext(ctx, dbos.Config{
		AppName:        "agentiq-session-test",
		DatabaseURL:    pg.DSN,
		DatabaseSchema: "dbos",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = dbos.Shutdown(dctx, 10*time.Second) })

	// After NewContext (which runs `dbos migrate`) and before Launch, exactly
	// as cmd/agentiq orders it: the property graph projects `dbos.*`, and
	// Launch starts recovering workflows that append into `agentiq.*`.
	require.NoError(t, harness.ApplyMigrations(ctx, pool))

	// The DataSource is what `dbos.RunAsTransaction` opens its transaction on,
	// and it is the argument SPEC.md §8.3 gives `session.New(ds *dbos.DataSource)`
	// — the signature makes sense once you see that the session service does not
	// own a pool, it owns the thing DBOS hands transactions out of.
	var err2 error
	ds, err2 = dbos.NewDataSource(dctx, pool)
	require.NoError(t, err2)

	dbos.RegisterWorkflow(dctx, runTx)
	require.NoError(t, dbos.Launch(dctx))

	return ctx, pool, dctx
}

// ds is the DataSource the registered workflow transacts on. Package-level for
// the same reason `body` is: one workflow registration, one test at a time.
var ds *dbos.DataSource

// body is what the currently-running test wants runTx to do. Tests here run one
// workflow at a time, sequentially, so a package-level hand-off is honest about
// what it is rather than a synchronisation problem in disguise.
var body func(dbos.Context) (string, error)

func runTx(ctx dbos.Context, _ string) (string, error) { return body(ctx) }

// runWorkflow runs fn as workflow code and fails the test if it errors.
func runWorkflow(t *testing.T, dctx dbos.Context, fn func(dbos.Context) (string, error)) string {
	t.Helper()
	out, err := runWorkflowExpectingError(t, dctx, fn)
	require.NoError(t, err)
	return out
}

func runWorkflowExpectingError(t *testing.T, dctx dbos.Context, fn func(dbos.Context) (string, error)) (string, error) {
	t.Helper()
	body = fn
	handle, err := dbos.RunWorkflow(dctx, runTx, "")
	require.NoError(t, err, "starting the workflow must succeed; the test is about what it does")
	return handle.GetResult()
}

// seedSession inserts the parent session row and returns its surrogate uuid.
//
// It is raw SQL because `test/` is exempt from the no-sql-outside-generated
// rule and because M2 has no `create_session` function yet — the fixture is
// scaffolding for this test, not a shape the application uses.
func seedSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, adkID string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO agentiq.session (adk_id, app_name, user_id, agent_digest, created_at_ts, last_update_at)
		VALUES ($1, $2, 'u1', 'sha256:test', now(), now())
		RETURNING id::text`, adkID, appName).Scan(&id)
	require.NoError(t, err)
	return id
}

// richEvent is the §15 event: text, a thought signature and a function call,
// plus the state deltas that exercise §6.4's scoping and §7.2 rule 4.
func richEvent(id string) *adksession.Event {
	e := &adksession.Event{
		ID:           id,
		Timestamp:    time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
		InvocationID: "inv-1",
		Author:       "root",
		Actions: adksession.EventActions{
			StateDelta: map[string]any{
				"app:theme":    "dark",
				"user:name":    "ada",
				"turns":        float64(3),
				"temp:scratch": "discard me",
			},
			ArtifactDelta: map[string]int64{"report.md": 2},
		},
	}
	e.Content = &genai.Content{
		Role: "model",
		Parts: []*genai.Part{
			{Text: "Let me look that up."},
			{Text: "thinking", Thought: true, ThoughtSignature: []byte{0x00, 0x01, 0xfe}},
			{FunctionCall: &genai.FunctionCall{
				ID: "call-1", Name: "search",
				Args: map[string]any{"query": "postgres 19"},
			}},
		},
	}
	e.TurnComplete = true
	return e
}

func appendInTx(ctx dbos.Context, sessionID string, e *adksession.Event) (string, error) {
	docs, err := session.EncodeForAppend(sessionID, e)
	if err != nil {
		return "", err
	}
	return dbos.RunAsTransaction(ctx, ds, func(_ context.Context, tx dbos.Tx) (string, error) {
		// exec.Portable is the whole point: `dbos.Tx` is driver-agnostic, and
		// since gopgql v0.3.0 so is `exec.Handle`, so the generated method runs
		// on DBOS's own transaction rather than opening a connection of its own.
		return client.New().AppendEvent(ctx, exec.Portable(tx), client.AppendEventInput{
			SessionId:      sessionID,
			Event:          docs.Event,
			Parts:          docs.Parts,
			Actions:        docs.Actions,
			StateDeltas:    docs.StateDeltas,
			ArtifactDeltas: docs.ArtifactDeltas,
		})
	})
}

// TestAppendEventWritesEveryTableInOneTransaction is the positive case: the
// event, its three parts, its actions row, its three state deltas and its
// artifact delta all land, and the `temp:` key does not.
func TestAppendEventWritesEveryTableInOneTransaction(t *testing.T) {
	ctx, pool, dctx := setup(t)
	sessionID := seedSession(ctx, t, pool, "s1")

	eventID := runWorkflow(t, dctx, func(wctx dbos.Context) (string, error) {
		return appendInTx(wctx, sessionID, richEvent("event1"))
	})
	require.NotEmpty(t, eventID)

	rows, err := client.New().Event(ctx, exec.Pgx(pool), client.EventInput{
		SessionId: sessionID, AdkId: "event1",
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	got := rows[0]

	assert.Equal(t, 1, got.Sequence, "sequence is allocated by the function, starting at 1")
	assert.Equal(t, "root", got.Author)
	assert.True(t, got.TurnComplete)
	require.NotNil(t, got.ContentRole)
	assert.Equal(t, "model", *got.ContentRole)

	require.Len(t, got.Parts, 3, "D3: parts are rows, not a blob")
	assert.Equal(t, 0, got.Parts[0].PartIndex)
	assert.Equal(t, 1, got.Parts[1].PartIndex)
	assert.Equal(t, 2, got.Parts[2].PartIndex, "§7.2 rule 2: part_index is slice position")
	require.NotNil(t, got.Parts[1].Thought)
	assert.True(t, *got.Parts[1].Thought)
	require.NotNil(t, got.Parts[2].FunctionCallName)
	assert.Equal(t, "search", *got.Parts[2].FunctionCallName)

	require.Len(t, got.Actions, 1)
	deltas := got.Actions[0].StateDeltas
	require.Len(t, deltas, 3, "§7.2 rule 4: app:, user: and the bare key are stored; temp: is not")

	scopes := map[string]string{}
	for _, d := range deltas {
		scopes[d.Key] = d.Scope
		assert.NotEqual(t, "temp", d.Scope, "no row may carry scope 'temp'")
	}
	assert.Equal(t, map[string]string{"theme": "app", "name": "user", "turns": "session"}, scopes,
		"§6.4: the prefix becomes the scope and the key is stored without it")

	require.Len(t, got.Actions[0].ArtifactDeltas, 1)
	assert.Equal(t, "report.md", got.Actions[0].ArtifactDeltas[0].Filename)

	// The session_state projection, which the same call maintains.
	var stateCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM agentiq.session_state WHERE session_id = $1`, sessionID).Scan(&stateCount))
	assert.Equal(t, 3, stateCount, "state deltas project onto session_state")
}

// TestAFailedTransactionRollsBackTheAppend is the half that proves the append
// ran in DBOS's transaction rather than on a connection of its own.
//
// It reads its own write back *inside* the transaction before failing, so the
// after-rollback count means what it says: the rows existed and then did not.
func TestAFailedTransactionRollsBackTheAppend(t *testing.T) {
	ctx, pool, dctx := setup(t)
	sessionID := seedSession(ctx, t, pool, "s2")

	sentinel := errors.New("deliberate failure after the append")
	_, err := runWorkflowExpectingError(t, dctx, func(wctx dbos.Context) (string, error) {
		docs, err := session.EncodeForAppend(sessionID, richEvent("event-rollback"))
		if err != nil {
			return "", err
		}
		_, err = dbos.RunAsTransaction(wctx, ds, func(_ context.Context, tx dbos.Tx) (string, error) {
			h := exec.Portable(tx)
			id, err := client.New().AppendEvent(wctx, h, client.AppendEventInput{
				SessionId: sessionID, Event: docs.Event, Parts: docs.Parts,
				Actions: docs.Actions, StateDeltas: docs.StateDeltas,
				ArtifactDeltas: docs.ArtifactDeltas,
			})
			if err != nil {
				return "", err
			}
			// Visible inside the transaction that wrote it.
			seen, err := client.New().Event(wctx, h, client.EventInput{
				SessionId: sessionID, AdkId: "event-rollback",
			})
			if err != nil {
				return "", err
			}
			if len(seen) != 1 {
				return "", errors.New("the append was not visible inside its own transaction")
			}
			_ = id
			return "", sentinel
		})
		return "", err
	})
	require.Error(t, err)

	var count int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM agentiq.event WHERE session_id = $1`, sessionID).Scan(&count))
	assert.Zero(t, count, "the append must not survive its transaction's rollback")

	var parts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM agentiq.part`).Scan(&parts))
	assert.Zero(t, parts, "and neither may its parts")
}

// TestAppendingTheSameEventTwiceIsIdempotent is the other half of exactly-once.
//
// `RunAsTransaction` commits the checkpoint with the write, so a retry after a
// successful commit should not reach the function at all — but "should not" is
// a statement about DBOS, and D2 is a property M2 is asked to deliver rather
// than to assume.
func TestAppendingTheSameEventTwiceIsIdempotent(t *testing.T) {
	ctx, pool, dctx := setup(t)
	sessionID := seedSession(ctx, t, pool, "s3")

	first := runWorkflow(t, dctx, func(wctx dbos.Context) (string, error) {
		return appendInTx(wctx, sessionID, richEvent("event-twice"))
	})
	second := runWorkflow(t, dctx, func(wctx dbos.Context) (string, error) {
		return appendInTx(wctx, sessionID, richEvent("event-twice"))
	})

	assert.Equal(t, first, second, "the second append returns the first one's id")

	var events, parts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM agentiq.event WHERE session_id = $1`, sessionID).Scan(&events))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM agentiq.part`).Scan(&parts))
	assert.Equal(t, 1, events, "one event, not two")
	assert.Equal(t, 3, parts, "and its three parts once, not six")
}

// TestSequenceIsMonotonicPerSession pins the §7.1 open question's answer: the
// database allocates it, inside the caller's transaction.
func TestSequenceIsMonotonicPerSession(t *testing.T) {
	ctx, pool, dctx := setup(t)
	sessionID := seedSession(ctx, t, pool, "s4")

	for _, id := range []string{"e1", "e2", "e3"} {
		runWorkflow(t, dctx, func(wctx dbos.Context) (string, error) {
			return appendInTx(wctx, sessionID, richEvent(id))
		})
	}

	rows, err := pool.Query(ctx,
		`SELECT sequence FROM agentiq.event WHERE session_id = $1 ORDER BY sequence`, sessionID)
	require.NoError(t, err)
	defer rows.Close()

	var seqs []int
	for rows.Next() {
		var s int
		require.NoError(t, rows.Scan(&s))
		seqs = append(seqs, s)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []int{1, 2, 3}, seqs)
}

// TestAPartialEventIsRefusedBeforeItReachesTheDatabase is D5 at the write
// boundary. The scenario in features/session_persistence.feature counts the
// stored events; this names the mechanism that keeps the count at zero.
func TestAPartialEventIsRefusedBeforeItReachesTheDatabase(t *testing.T) {
	e := richEvent("partial")
	e.Partial = true

	_, err := session.EncodeForAppend("00000000-0000-0000-0000-000000000001", e)
	require.ErrorIs(t, err, session.ErrPartialEvent)
}
