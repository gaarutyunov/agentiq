package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/gaarutyunov/gopgql/exec"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkplugin "google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/gaarutyunov/agentiq/dbosadk"
	"github.com/gaarutyunov/agentiq/generated/client"
	"github.com/gaarutyunov/agentiq/session"
)

// maxTurnSteps bounds the Runner loop.
//
// The loop runs until `turnComplete` (SPEC.md §20 M2), and a model that never
// sets it would otherwise spin forever inside a workflow that DBOS keeps alive
// across restarts — a runaway that survives the process it started in. M2 has
// no tools, so a turn is one generation and this is a ceiling by two orders of
// magnitude rather than a budget anything approaches.
const maxTurnSteps = 100

// roleUser is genai's role for a user turn. It is a literal because `genai`
// exports no constant for it and ADK writes the same string.
const roleUser = "user"

// agentAuthor is the `author` on an event AgentIQ writes itself rather than
// receives from the Runner.
const agentAuthor = "agentiq"

// turnFailedCode is the `errorCode` of the event a failed turn leaves behind.
// §19 names no code for rows F6 and F7 — only that one is present — so this is
// AgentIQ's own, and it says what happened rather than echoing a provider's
// status.
const turnFailedCode = "TURN_FAILED"

// resolvedAgent is the agent closure a run executes against (SPEC.md §6.3).
//
// It crosses a step checkpoint, so it is a flat struct of strings: the digest
// transitively fixes every one of these values, which is the whole point of
// identifying an agent by it, and re-resolving on replay must produce the same
// closure rather than whatever the registry holds today.
type resolvedAgent struct {
	Digest      string `json:"digest"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Model       string `json:"model"`
	Instruction string `json:"instruction"`
}

// resolveAgentClosure reads one agent row by digest.
//
// In M2 the row is seeded by the fixture migration (D24, SPEC.md §20 M2
// Configuration); from M3 it is written by the epos projection into the same
// table. Neither is reached from here: this reads `agentiq.agent`, and which
// of the two wrote the row is not something a run can tell — which is what
// makes M2's seeding a first implementation rather than scaffolding.
func resolveAgentClosure(ctx context.Context, h exec.Handle, digest string) (resolvedAgent, error) {
	if digest == "" {
		return resolvedAgent{}, fmt.Errorf("workflow: the agent digest is empty")
	}
	rows, err := client.New().Agent(ctx, h, client.AgentInput{Digest: digest})
	if err != nil {
		return resolvedAgent{}, fmt.Errorf("workflow: resolve agent %s: %w", digest, err)
	}
	if len(rows) == 0 {
		return resolvedAgent{}, fmt.Errorf("workflow: no agent with digest %s", digest)
	}
	row := rows[0]
	out := resolvedAgent{
		Digest:      row.Digest,
		Name:        row.Name,
		Model:       row.Model,
		Instruction: row.Instruction,
	}
	if row.Description != nil {
		out.Description = *row.Description
	}
	return out, nil
}

// buildRunner assembles the ADK Runner for one run.
//
// Every non-deterministic thing it wires is wrapped: the model by
// [dbosadk.NewModel], so each generation is a step at §9.2's policy (5 retries,
// base 1s, exponential); the session service by `dbos.RunAsTransaction`, so each
// append is a transaction at §9.2's other policy (3 retries). The plugin is the
// check that this actually happened — it refuses a model that is not wrapped
// (§8.2), which is the one failure no static analysis of the workflow can see,
// because which model an agent holds is a value and not a call.
func (d Deps) buildRunner(ra resolvedAgent, workflowUUID string) (*runner.Runner, adksession.Service, error) {
	llm, err := d.model(ra.Model)
	if err != nil {
		return nil, nil, err
	}

	root, err := llmagent.New(llmagent.Config{
		Name:        ra.Name,
		Description: ra.Description,
		Instruction: ra.Instruction,
		// The wrapper, not the model. An unwrapped model here is a generation
		// re-executed and re-billed on every replay, in a system that looks
		// healthy until its first recovery.
		Model: dbosadk.NewModel(llm),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("workflow: build agent %q: %w", ra.Name, err)
	}

	// The plugin's own turn-end close is off, and the workflow closes the
	// stream itself. Both would close it, and the second call fails with
	// "stream ... is already closed" — which surfaced here as a turn that had
	// completed successfully reporting an error. dbosadk.Config documents this
	// as the case for turning it off: a workflow that closes the stream itself
	// wants the error rather than the plugin's best-effort attempt.
	off := false
	durability, err := dbosadk.NewPlugin(dbosadk.Config{CloseStreamAtTurnEnd: &off})
	if err != nil {
		return nil, nil, fmt.Errorf("workflow: build the durability plugin: %w", err)
	}

	svc := session.New(d.DataSource,
		session.WithHandle(d.Handle),
		session.WithAgentDigest(ra.Digest),
		session.WithWorkflowUUID(workflowUUID),
	)

	r, err := runner.New(runner.Config{
		AppName:        d.appName(),
		Agent:          root,
		SessionService: svc,
		PluginConfig:   runner.PluginConfig{Plugins: []*adkplugin.Plugin{durability}},
		// The Runner's own Get-then-Create. It is what removes the need for
		// `agentiq.create_session` to be an upsert — and the conformance suite
		// requires it not to be.
		AutoCreateSession: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("workflow: build the runner: %w", err)
	}
	return r, svc, nil
}

// runTurn drives the ADK Runner loop as deterministic workflow code (D1, §8.2).
//
// The loop *is* the workflow, mirroring Temporal's `contrib/googleadk`: each
// model call is a step and each append is a transaction, and what runs between
// them is ordinary Go that a replay re-executes. That is why it may not read a
// clock, draw a random number or make an HTTP call of its own — the §17.1
// analyzer walks outward from `RegisterWorkflow` to enforce it.
//
// It ends on `turnComplete`, which is §20 M2's condition, and reports the last
// event's error code when the turn ended in one. `maxTurnSteps` is the
// backstop.
func runTurn(ctx dbos.Context, r *runner.Runner, svc adksession.Service, in AgentRunInput, sessionID string) (AgentRunOutput, error) {
	msg, err := userMessage(in.Message)
	if err != nil {
		return AgentRunOutput{}, err
	}

	out := AgentRunOutput{SessionID: sessionID, Status: "SUCCESS"}
	var invocationID string
	steps := 0

	// Streaming is requested, and §20 M2's "no streaming" is about the
	// subscription rather than the model. The §15 partial-skip scenario ends
	// "And the DBOS stream contains five values", which there is nothing to
	// count unless the model is asked for partials — M7 adds the read side.
	cfg := adkagent.RunConfig{StreamingMode: adkagent.StreamingModeSSE}

	// The context the Runner is given carries the workflow, so that every
	// callback ADK makes — the wrapped model, the session service — can find it
	// again. A bare `ctx` does not survive: ADK wraps it in its own invocation
	// context, which is a `context.Context` and nothing more, and every wrapped
	// call then reports that it is not in a workflow.
	runCtx := dbosadk.WithWorkflowContext(ctx, ctx)

	for event, err := range r.Run(runCtx, in.UserID, sessionID, msg, cfg) {
		if err != nil {
			// §19 F6 and F7: the run ends with the failure recorded *as an
			// event*, not as a workflow that vanished. Returning the error here
			// would leave a session that says nothing about what happened.
			return recordTurnFailure(ctx, svc, in, sessionID, invocationID, err)
		}
		if event == nil {
			continue
		}
		if invocationID == "" {
			invocationID = event.InvocationID
		}
		if event.ErrorCode != "" {
			// Not a Go error: §19 rows F6 and F7 both end with the failure
			// recorded *as an event*, which the Runner has already appended by
			// the time this sees it. Returning an error here would roll the run
			// back to a state in which nothing says why it failed.
			out.Status = "ERROR"
			out.ErrorCode = event.ErrorCode
		}
		if event.TurnComplete {
			break
		}
		if steps++; steps >= maxTurnSteps {
			return AgentRunOutput{}, fmt.Errorf(
				"workflow: the turn produced %d events without turnComplete", steps)
		}
	}

	// §9.4: the stream is closed at turn end and not by the model, because a
	// turn may generate more than once and a stream closed by the first
	// generation would drop the partials of the second.
	if invocationID != "" {
		if err := dbosadk.CloseStream(ctx, invocationID); err != nil {
			return AgentRunOutput{}, err
		}
	}

	return out, nil
}

// recordTurnFailure appends the error event a failed turn leaves behind
// (SPEC.md §19, rows F6 and F7).
//
// The append is the whole point: by the time this runs the model step has
// exhausted §9.2's five attempts, and a workflow that returned the error would
// leave the run's own session with no record of why it stopped. So the failure
// becomes an event with an `errorCode`, exactly like a failure the model
// reported itself, and the workflow completes with `Status: "ERROR"`.
//
// A failure to append the failure *is* returned. At that point nothing has
// recorded anything and there is no better place for it to surface.
func recordTurnFailure(ctx dbos.Context, svc adksession.Service, in AgentRunInput, sessionID, invocationID string, cause error) (AgentRunOutput, error) {
	if invocationID == "" {
		invocationID = sessionID
	}

	// The event id is derived from the workflow's own ids rather than drawn,
	// so a replay that reaches here again appends the same event — and
	// `agentiq.append_event`'s natural key turns the second attempt into a
	// no-op instead of a duplicate.
	event := &adksession.Event{
		ID:           "error-" + invocationID,
		InvocationID: invocationID,
		Author:       agentAuthor,
	}
	event.ErrorCode = turnFailedCode
	event.ErrorMessage = cause.Error()
	event.TurnComplete = true

	sess := &failedSession{appName: registeredDeps.appName(), userID: in.UserID, id: sessionID}
	if err := svc.AppendEvent(ctx, sess, event); err != nil {
		return AgentRunOutput{}, fmt.Errorf("workflow: record the turn failure %v: %w", cause, err)
	}

	return AgentRunOutput{
		SessionID: sessionID,
		Status:    "ERROR",
		ErrorCode: turnFailedCode,
	}, nil
}

// failedSession is the minimum `session.Session` an append needs: the natural
// key. The service resolves the row from it and reads nothing else, so
// reloading the whole conversation to record that it failed would be a query
// for values nobody looks at.
type failedSession struct {
	appName string
	userID  string
	id      string
}

func (s *failedSession) ID() string                { return s.id }
func (s *failedSession) AppName() string           { return s.appName }
func (s *failedSession) UserID() string            { return s.userID }
func (s *failedSession) State() adksession.State   { return nil }
func (s *failedSession) Events() adksession.Events { return nil }
func (s *failedSession) LastUpdateTime() time.Time { return time.Time{} }

// userMessage decodes the workflow input's message.
//
// The input is `json.RawMessage` because it is serialized into
// `dbos.workflow_status.inputs` and replayed verbatim (§8.1), so its shape is a
// compatibility surface. An absent message is a turn with no user content,
// which ADK accepts.
func userMessage(raw json.RawMessage) (*genai.Content, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var content genai.Content
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil, fmt.Errorf("workflow: decode the user message: %w", err)
	}
	if content.Role == "" {
		// ADK routes a turn's content by role and the model provider rejects an
		// empty one, so a message that did not name itself is the user's — the
		// only role a workflow input can carry.
		content.Role = roleUser
	}
	return &content, nil
}
