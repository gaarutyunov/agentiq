//go:build integration

package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/gaarutyunov/gopgql/exec"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	adkmodel "google.golang.org/adk/v2/model"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/gaarutyunov/agentiq/dbosadk"
	"github.com/gaarutyunov/agentiq/migrate"
	"github.com/gaarutyunov/agentiq/model"
	"github.com/gaarutyunov/agentiq/session"
	"github.com/gaarutyunov/agentiq/test/harness"
	"github.com/gaarutyunov/agentiq/workflow"
)

// SPEC.md §15, Feature: Session persistence — every scenario, against a real
// `postgres:19beta2` and a real `workflow.AgentRun`.
//
// The model is a stub OpenRouter endpoint (stub_test.go) rather than a fake
// `model.LLM`, because three of the five scenarios are about what happens
// between the HTTP response and the database: partials that must reach the DBOS
// stream and not the event table (§9.4, D5), and two failure-matrix rows whose
// mechanism §19 names as "stub endpoint". A fake `model.LLM` would skip the
// code those scenarios are about.
//
// The fixture agent (migrate/sql/0004, D24) is the agent that runs. Its model
// id is whatever the row says; only the BaseURL is redirected.
const fixtureDigest = "sha256:0f0e4d9a7b1c2e3f4a5b6c7d8e9f0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b"

func TestSessionPersistence(t *testing.T) {
	s := &featureSuite{t: t}

	opts, err := harness.GodogOptions(t, "@session", "session-junit.xml")
	require.NoError(t, err)

	status := godog.TestSuite{
		Name:                "session-persistence",
		ScenarioInitializer: s.initialize,
		Options:             &opts,
	}.Run()

	require.Zero(t, status, "godog exit status")
}

type featureSuite struct {
	t *testing.T

	ctx  context.Context
	pool *pgxpool.Pool
	dctx dbos.Context

	stub    *stub
	svc     adksession.Service
	handle  exec.Handle
	started time.Time

	// out is what the run returned, and runErr what it failed with. A scenario
	// asserting on a failed turn needs both: §19's rows end with the workflow
	// *completing* and the failure recorded as an event.
	out    workflow.AgentRunOutput
	runErr error

	sessionID     string
	event         *adksession.Event
	reloaded      *adksession.Event
	reloadedState map[string]any
}

func (s *featureSuite) initialize(sc *godog.ScenarioContext) {
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		*s = featureSuite{t: s.t}
		return ctx, nil
	})

	sc.Given(`^a running AgentIQ worker$`, s.aRunningWorker)
	sc.Given(`^an agent seeded by the fixture migration$`, s.anAgentSeededByTheFixture)

	sc.Given(`^the model produces five partial responses and one final response$`, s.fivePartialsAndAFinal)
	sc.Given(`^a stub OpenRouter endpoint that returns HTTP 529$`, s.anOverloadedEndpoint)
	sc.Given(`^a stub OpenRouter endpoint that returns malformed JSON$`, s.aMalformedEndpoint)
	sc.Given(`^an event with text, a thought signature and a function call$`, s.aRichEvent)
	sc.Given(`^an event whose state delta sets "app:theme", "user:name", "turns" and "temp:scratch"$`, s.aStatefulEvent)

	sc.When(`^the agent runs one turn$`, s.theAgentRunsOneTurn)
	sc.When(`^the event is appended and the session is reloaded$`, s.theEventIsAppendedAndReloaded)

	sc.Then(`^the session contains exactly one event$`, s.theSessionContainsExactlyOneEvent)
	sc.Then(`^the DBOS stream contains five values$`, s.theStreamContainsFiveValues)
	sc.Then(`^the reloaded event marshals identically to the original$`, s.theReloadedEventIsByteIdentical)
	sc.Then(`^the session state contains "app:theme", "user:name" and "turns"$`, s.theStateContainsTheThree)
	sc.Then(`^the session state does not contain "temp:scratch"$`, s.theStateHasNoTempKey)
	sc.Then(`^no state row carries the scope "temp"$`, s.noRowCarriesTheTempScope)
	sc.Then(`^the model step is retried with exponential backoff$`, s.theModelStepWasRetried)
	sc.Then(`^the final failure surfaces as an event with an errorCode$`, s.theFailureIsAnEventWithAnErrorCode)
	sc.Then(`^the model step errors and is retried$`, s.theModelStepWasRetried)
	sc.Then(`^the session records the error event$`, s.theFailureIsAnEventWithAnErrorCode)
}

// ---------------------------------------------------------------------------
// Background
// ---------------------------------------------------------------------------

// aRunningWorker brings up Postgres, applies AgentIQ's migrations and registers
// the real workflow in this process.
//
// In-process rather than the subprocess `test/harness` starts for the durable
// rows: those kill a worker, which needs a process to kill, while these assert
// what one completed turn left in the database.
func (s *featureSuite) aRunningWorker() error {
	s.ctx = context.Background()
	pg := harness.StartPostgres(s.ctx, s.t)
	s.pool = harness.OpenPool(s.ctx, s.t, pg.DSN)

	dctx, err := dbos.NewContext(s.ctx, dbos.Config{
		AppName:        "agentiq-session-feature",
		DatabaseURL:    pg.DSN,
		DatabaseSchema: "dbos",
	})
	if err != nil {
		return err
	}
	s.t.Cleanup(func() { _ = dbos.Shutdown(dctx, 10*time.Second) })
	s.dctx = dctx

	// After NewContext (which runs `dbos migrate`) and before Launch, exactly
	// as cmd/agentiq orders it.
	rt, err := migrate.Open(s.ctx, dctx, pg.DSN)
	if err != nil {
		return err
	}
	s.handle = rt.Handle

	// The stub defaults to a well-behaved model, so a scenario that says
	// nothing about the endpoint still gets one turn that works.
	if s.stub == nil {
		s.stub = newStub(s.t, streamingText(1, "ok"))
	}

	if err := workflow.Register(dctx, workflow.Deps{
		AppName:    featureAppName,
		DataSource: rt.DataSource,
		Handle:     rt.Handle,
		NewModel: func(modelID string) (adkmodel.LLM, error) {
			// Only the BaseURL is redirected. The model id is the fixture
			// row's, and the key is a placeholder the stub never checks —
			// `model.New` requires one because a real OpenRouter call without
			// one is rejected by the API rather than by us.
			return model.New(model.Config{
				Model: modelID, APIKey: "stub-key", BaseURL: s.stub.URL,
			})
		},
	}); err != nil {
		return err
	}
	if err := dbos.Launch(dctx); err != nil {
		return err
	}

	s.svc = session.New(rt.DataSource,
		session.WithHandle(rt.Handle),
		session.WithAgentDigest(fixtureDigest),
	)
	return nil
}

const featureAppName = "agentiq-feature"

// anAgentSeededByTheFixture asserts D24's row is there.
//
// It is a Given and it does no seeding: the row is written by
// migrate/sql/0004, which `aRunningWorker` already applied. A step that seeded
// it here would be testing its own fixture rather than the migration §20 M2
// requires.
func (s *featureSuite) anAgentSeededByTheFixture() error {
	var count int
	if err := s.pool.QueryRow(s.ctx,
		`SELECT count(*) FROM agentiq.agent WHERE digest = $1`, fixtureDigest).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("the fixture migration seeded %d agent rows for %s, want 1", count, fixtureDigest)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Given
// ---------------------------------------------------------------------------

// fivePartialsAndAFinal scripts the model of §15's partial-skip scenario.
//
// Four deltas, not five: ADK's completion event contributes a partial of its
// own before the final response, so four text deltas are five partial
// `LLMResponse`s — which is what the scenario says the model produces and what
// the stream then holds. Measured, against ADK v2.1.0; see streamingText.
func (s *featureSuite) fivePartialsAndAFinal() error {
	s.stub = newStub(s.t, streamingText(4, "chunk"))
	return nil
}

func (s *featureSuite) anOverloadedEndpoint() error {
	s.stub = newStub(s.t, overloaded())
	return nil
}

func (s *featureSuite) aMalformedEndpoint() error {
	s.stub = newStub(s.t, malformed())
	return nil
}

func (s *featureSuite) aRichEvent() error {
	s.event = richEvent("feature-roundtrip")
	// The §15 corpus event is text plus a thought signature plus a function
	// call, which `richEvent` already carries; the state deltas it also carries
	// belong to the scenario below and would make this one assert two things.
	s.event.Actions = adksession.EventActions{}
	return nil
}

func (s *featureSuite) aStatefulEvent() error {
	s.event = &adksession.Event{
		ID:           "feature-state",
		Timestamp:    time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
		InvocationID: "inv-state",
		Author:       "root",
		Actions: adksession.EventActions{
			StateDelta: map[string]any{
				"app:theme":    "dark",
				"user:name":    "ada",
				"turns":        float64(3),
				"temp:scratch": "discard me",
			},
		},
	}
	s.event.TurnComplete = true
	return nil
}

// ---------------------------------------------------------------------------
// When
// ---------------------------------------------------------------------------

func (s *featureSuite) theAgentRunsOneTurn() error {
	msg, err := json.Marshal(genai.Content{
		Role:  "user",
		Parts: []*genai.Part{{Text: "hello"}},
	})
	if err != nil {
		return err
	}

	s.sessionID = "feature-session"
	s.started = time.Now()
	handle, err := workflow.Enqueue(s.dctx, workflow.AgentRunInput{
		AgentDigest: fixtureDigest,
		AppName:     featureAppName,
		UserID:      "u1",
		SessionID:   s.sessionID,
		Message:     msg,
	})
	if err != nil {
		return err
	}
	s.out, s.runErr = handle.GetResult()
	return nil
}

func (s *featureSuite) theEventIsAppendedAndReloaded() error {
	s.sessionID = "feature-session"

	created, err := s.svc.Create(s.ctx, &adksession.CreateRequest{
		AppName: featureAppName, UserID: "u1", SessionID: s.sessionID,
	})
	if err != nil {
		return err
	}
	if err := s.svc.AppendEvent(s.ctx, created.Session, s.event); err != nil {
		return err
	}

	got, err := s.svc.Get(s.ctx, &adksession.GetRequest{
		AppName: featureAppName, UserID: "u1", SessionID: s.sessionID,
	})
	if err != nil {
		return err
	}
	if got.Session.Events().Len() != 1 {
		return fmt.Errorf("reloaded %d events, want 1", got.Session.Events().Len())
	}
	s.reloaded = got.Session.Events().At(0)
	s.reloadedState = map[string]any{}
	for k, v := range got.Session.State().All() {
		s.reloadedState[k] = v
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then
// ---------------------------------------------------------------------------

func (s *featureSuite) theSessionContainsExactlyOneEvent() error {
	if s.runErr != nil {
		return fmt.Errorf("the turn failed: %w", s.runErr)
	}
	// The user's own message is excluded, and that is the scenario read
	// precisely rather than loosened. ADK's Runner appends the user turn as an
	// event of its own before the agent runs, so a session that received a
	// message always holds at least one event that has nothing to do with the
	// model. What §15 counts is what the *generation* left behind: five
	// partials and one final response, of which exactly one is stored (D5).
	var count int
	if err := s.pool.QueryRow(s.ctx, `
		SELECT count(*) FROM agentiq.event e
		  JOIN agentiq.session ss ON ss.id = e.session_id
		 WHERE ss.adk_id = $1 AND e.author <> 'user'`, s.sessionID).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("the generation left %d events, want exactly 1 (D5: partials are never stored)", count)
	}
	return nil
}

// theStreamContainsFiveValues counts the partials in `dbos.streams`.
//
// This is the assertion that makes `dbos.WriteStream` M2 work rather than M7's:
// §20 calls M2 "no streaming" and puts `Subscription.runEvents` in M7, but M7
// adds the *read* side. There is nothing for it to read unless the write
// happened here.
func (s *featureSuite) theStreamContainsFiveValues() error {
	rows, err := s.pool.Query(s.ctx,
		`SELECT key, count(*) FROM dbos.streams GROUP BY key`)
	if err != nil {
		return err
	}
	defer rows.Close()

	total := 0
	keys := map[string]int{}
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return err
		}
		keys[key] = n
		if len(key) >= len(streamPrefix) && key[:len(streamPrefix)] == streamPrefix {
			total += n
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if total != 5 {
		return fmt.Errorf("the model streams hold %d values, want 5 (keys: %v)", total, keys)
	}
	return nil
}

// streamPrefix is dbosadk's namespace inside `dbos.streams`, taken from the
// function that writes it rather than copied — a prefix that agreed by
// coincidence would be a count of the wrong rows.
var streamPrefix = dbosadk.StreamKey("")

func (s *featureSuite) theReloadedEventIsByteIdentical() error {
	want, err := json.Marshal(s.event)
	if err != nil {
		return err
	}
	got, err := json.Marshal(s.reloaded)
	if err != nil {
		return err
	}
	if string(want) != string(got) {
		return fmt.Errorf("§7.2: the reloaded event is not byte-identical\nwant %s\n got %s", want, got)
	}
	return nil
}

func (s *featureSuite) theStateContainsTheThree() error {
	for _, key := range []string{"app:theme", "user:name", "turns"} {
		if _, ok := s.reloadedState[key]; !ok {
			return fmt.Errorf("the session state has no %q (got %v)", key, s.reloadedState)
		}
	}
	return nil
}

func (s *featureSuite) theStateHasNoTempKey() error {
	if _, ok := s.reloadedState["temp:scratch"]; ok {
		return errors.New("§7.2 rule 4: temp: state must be absent on read")
	}
	return nil
}

func (s *featureSuite) noRowCarriesTheTempScope() error {
	for _, table := range []string{"agentiq.state_delta", "agentiq.session_state"} {
		var count int
		if err := s.pool.QueryRow(s.ctx,
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE scope = 'temp'`, table)).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("%s holds %d rows with scope 'temp'", table, count)
		}
	}
	return nil
}

// theModelStepWasRetried reads the retry off the stub's request count.
//
// DBOS's retry is invisible from inside the step and `operation_outputs`
// records the outcome rather than the attempts, so the number of times the
// model actually called out is the only external evidence. §9.2's policy is
// five attempts at base 1s exponential; the elapsed time is asserted too,
// because a retry with no backoff would produce the same count.
func (s *featureSuite) theModelStepWasRetried() error {
	if n := s.stub.Requests(); n < 2 {
		return fmt.Errorf("the stub was called %d times; the step did not retry "+
			"(run status %q, errorCode %q, err %v, recorded: %s)",
			n, s.out.Status, s.out.ErrorCode, s.runErr, s.recordedFailure())
	}
	if elapsed := time.Since(s.started); elapsed < time.Second {
		return fmt.Errorf("the turn took %s; §9.2's base interval is 1s, so a retried step cannot be faster", elapsed)
	}
	return nil
}

// recordedFailure reads back the message of the error event a failed turn left,
// so a step that fails for the wrong reason says which one.
func (s *featureSuite) recordedFailure() string {
	var message string
	err := s.pool.QueryRow(s.ctx, `
		SELECT coalesce(e.error_message, '')
		  FROM agentiq.event e
		  JOIN agentiq.session ss ON ss.id = e.session_id
		 WHERE ss.adk_id = $1 AND e.error_code IS NOT NULL
		 ORDER BY e.sequence DESC LIMIT 1`, s.sessionID).Scan(&message)
	if err != nil {
		return "(no error event: " + err.Error() + ")"
	}
	return message
}

func (s *featureSuite) theFailureIsAnEventWithAnErrorCode() error {
	if s.runErr != nil {
		return fmt.Errorf("the run errored instead of recording the failure: %w", s.runErr)
	}
	if s.out.ErrorCode == "" {
		return fmt.Errorf("the run reported status %q with no error code", s.out.Status)
	}

	var code, message string
	err := s.pool.QueryRow(s.ctx, `
		SELECT e.error_code, coalesce(e.error_message, '')
		  FROM agentiq.event e
		  JOIN agentiq.session ss ON ss.id = e.session_id
		 WHERE ss.adk_id = $1 AND e.error_code IS NOT NULL
		 ORDER BY e.sequence DESC LIMIT 1`, s.sessionID).Scan(&code, &message)
	if err != nil {
		return fmt.Errorf("no event carries an errorCode: %w", err)
	}
	if code == "" {
		return errors.New("the event's errorCode is empty")
	}
	return nil
}
