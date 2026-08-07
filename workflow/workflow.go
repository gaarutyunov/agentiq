// Package workflow registers every DBOS workflow AgentIQ runs, defines the
// step boundaries, and owns determinism (SPEC.md §4.1, §5, §9).
//
// This is the only place `dbos.RegisterWorkflow` is called. The determinism
// analyzer (SPEC.md §17.1) walks outward from the functions passed to it, so a
// registration made anywhere else is a registration nothing checks.
//
// Inside workflow code — `AgentRun` and everything reachable from it that is
// not behind a step boundary — `time.Now`, `math/rand`, `crypto/rand`, UUID
// generation, `os.Getenv`, `net/http`, file I/O, map iteration, bare `go` and
// bare `select` are forbidden. DBOS performs no automatic substitution and no
// import sandboxing (SPEC.md §9.1): nothing stops non-deterministic code at
// run time, which is why the analyzer exists.
package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/gaarutyunov/gopgql/exec"
	adkmodel "google.golang.org/adk/v2/model"
)

// DefaultQueueName is the queue every externally triggered run passes through.
// It matches the `queue: String = "agent"` default of `Mutation.startAgentRun`
// in schema/dbos.graphql (SPEC.md §7.4): DBOS exposes enqueue, not direct
// start, so there is no unqueued execution path reachable from the API.
const DefaultQueueName = "agent"

// checkpointDelay is an artificial delay between the two steps of AgentRun,
// long enough that a worker killed mid-workflow is reliably killed *between*
// checkpoints rather than in the middle of one. It is what makes the M1
// failure-matrix row F1 ("os.Process.Kill on a child worker") a repeatable
// test rather than a race.
//
// It is a durable sleep: `dbos.Sleep` records its wake-up time in the system
// database, so it survives a restart and is not re-slept on recovery. A
// `time.Sleep` here would be both non-deterministic and lost on replay.
const checkpointDelay = 2 * time.Second

// AgentRunInput is the durable workflow input (SPEC.md §8.1). It is serialized
// into `dbos.workflow_status.inputs` and replayed verbatim on recovery, so its
// shape is a compatibility surface from M1 onward: a field removed or retyped
// changes how an in-flight workflow deserializes after a deploy.
//
// AgentDigest pins the entire agent closure (SPEC.md §6.3) — one digest, one
// resolved set of instructions, model, skills, tools and sub-agents. Nothing
// about an agent is configured at run time (SPEC.md §10.2).
type AgentRunInput struct {
	AgentDigest string          `json:"agentDigest"`
	AppName     string          `json:"appName"`
	UserID      string          `json:"userId"`
	SessionID   string          `json:"sessionId"`
	Message     json.RawMessage `json:"message"`
}

// AgentRunOutput is the durable workflow result (SPEC.md §8.1). It is
// serialized into `dbos.workflow_status.output`.
type AgentRunOutput struct {
	SessionID string `json:"sessionId"`
	Status    string `json:"status"`

	// ErrorCode is the `errorCode` of the event the turn ended on, when it
	// ended on one. Failure-matrix rows F6 and F7 both surface a failed model
	// call as an event carrying one (SPEC.md §19), so a run that failed that
	// way completes with a result rather than erroring — the reason is in the
	// session, and a workflow that returned an error would leave nothing that
	// says which failure it was.
	ErrorCode string `json:"errorCode,omitempty"`
}

// Deps is everything AgentRun's steps need from the outside world. It is
// passed to Register once at startup and captured by the registered workflow,
// so nothing inside workflow code has to reach for a global.
//
// At M1 it carried only the queue configuration. M2 adds what one turn needs:
// the DataSource `dbos.RunAsTransaction` opens transactions on, a read handle,
// and a way to build the ADK model the resolved agent names.
type Deps struct {
	// QueueName is the DBOS queue runs are enqueued on. Empty means
	// DefaultQueueName.
	QueueName string

	// WorkerConcurrency caps how many workflows this process runs at once.
	// Zero means DBOS's own default. It is the `AGENTIQ_QUEUE_CONCURRENCY`
	// knob of SPEC.md §10.1.
	WorkerConcurrency int

	// AppName is ADK's app name, the first component of a session's natural
	// key (SPEC.md §6.1). Empty means DefaultAppName.
	AppName string

	// DataSource is what `dbos.RunAsTransaction` opens its transaction on, and
	// therefore what makes an append commit with its checkpoint (D2). It is the
	// argument SPEC.md §8.3 gives `session.New`.
	DataSource *dbos.DataSource

	// Handle is the read handle: the sessions, events and agent rows a turn
	// reads, none of which is a transaction to join. It is an `exec.Handle` and
	// not a pool so that nothing here has to name a driver.
	Handle exec.Handle

	// NewModel builds the ADK model for one model id (SPEC.md §9.7). It is a
	// function rather than a `model.LLM` because the id comes from the resolved
	// agent closure and is therefore not known until the run has started —
	// which is D24's point: nothing about an agent is configured at run time,
	// including which model it uses.
	//
	// The model it returns is the *unwrapped* one. Making a generation durable
	// is `dbosadk`'s job (SPEC.md §5), and a model that checkpointed itself
	// could not be constructed outside a workflow at all.
	NewModel func(modelID string) (adkmodel.LLM, error)
}

// DefaultAppName is the ADK app name AgentIQ runs under when Deps names none.
const DefaultAppName = "agentiq"

// DurabilityProbeDigest is a reserved agent digest that resolves to no agent.
//
// A run carrying it executes the two-step shape M1 shipped — resolve, durable
// sleep, complete — and touches no agent row, no model and no session. That is
// what failure-matrix rows F1 to F5 need and all they need: each one kills a
// worker between two checkpoints and asserts the recovered run does not
// re-execute the first, which is a property of where the checkpoints are and
// not of what happens between them.
//
// It exists because the alternative is worse in both directions. Registering a
// second workflow would break the M1 guarantee that the browser and the server
// dispatch the same function name, which test/durable asserts on purpose.
// Running the rows against a real agent would make offline durability tests
// need an OpenRouter key and would replace the deliberate two-second gap
// between checkpoints with whatever a model happened to take.
//
// It is a reserved identifier, like a reserved user name: well-formed, so it
// travels through everything that parses a digest, and pointing at nothing, so
// it cannot collide with an agent anybody publishes.
const DurabilityProbeDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

func (d Deps) appName() string {
	if d.AppName == "" {
		return DefaultAppName
	}
	return d.AppName
}

// model builds the ADK model for one model id, and refuses rather than
// improvising when Deps carries no constructor.
//
// A default that reached for `model.New` here would put an `OPENROUTER_API_KEY`
// read inside workflow code — SPEC.md §5 keeps `model/` out of DBOS precisely
// so that the environment is read where a process is configured and not where a
// turn is replayed.
func (d Deps) model(modelID string) (adkmodel.LLM, error) {
	if d.NewModel == nil {
		return nil, fmt.Errorf("workflow: Deps.NewModel is nil; a run cannot build the model %q", modelID)
	}
	llm, err := d.NewModel(modelID)
	if err != nil {
		return nil, fmt.Errorf("workflow: build the model %q: %w", modelID, err)
	}
	return llm, nil
}

// registered is the queue Register created. It is package-level because
// Register's signature is fixed by SPEC.md §8.1 and returns only an error,
// while enqueueing needs the dbos.Queue value itself. Registration happens
// once, at startup, before dbos.Launch and before anything can enqueue.
//
// The primary enqueue path does not read it at all: `Mutation.startAgentRun`
// calls `dbos.enqueue_workflow` in SQL and never enters this process
// (SPEC.md §7.4). Enqueue below is the server-side and test-side path.
var registered dbos.Queue

// registeredDeps is what Register was given, for the same reason: SPEC.md §8.1
// fixes `AgentRun`'s signature at `(dbos.Context, AgentRunInput)`, and DBOS
// registers a workflow by the function itself, so a closure over the deps
// cannot be what is registered. It is written once at startup, before
// `dbos.Launch` and before anything can enqueue.
var registeredDeps Deps

// Register wires every workflow into the DBOS context. It is called once at
// startup, before dbos.Launch, and it is the only place in the module where
// dbos.RegisterWorkflow appears (SPEC.md §4.1, §8.1).
func Register(ctx dbos.Context, deps Deps) error {
	name := deps.QueueName
	if name == "" {
		name = DefaultQueueName
	}

	opts := []dbos.QueueOption{dbos.WithPriorityEnabled()}
	if deps.WorkerConcurrency > 0 {
		opts = append(opts, dbos.WithWorkerConcurrency(deps.WorkerConcurrency))
	}

	queue, err := dbos.RegisterQueue(ctx, name, opts...)
	if err != nil {
		return fmt.Errorf("register queue %q: %w", name, err)
	}
	registered = queue
	registeredDeps = deps

	dbos.RegisterWorkflow(ctx, AgentRun)

	return nil
}

// Enqueue starts a run on the registered queue. Every externally triggered run
// passes through a queue — there is no unqueued execution path (SPEC.md §7.4)
// — so this never calls dbos.RunWorkflow without dbos.WithQueue.
func Enqueue(ctx dbos.Context, in AgentRunInput, opts ...dbos.WorkflowOption) (dbos.WorkflowHandle[AgentRunOutput], error) {
	opts = append([]dbos.WorkflowOption{dbos.WithQueue(registered)}, opts...)
	return dbos.RunWorkflow(ctx, AgentRun, in, opts...)
}

// AgentRun is deterministic workflow code (SPEC.md §8.1). Every
// non-deterministic operation inside it is a step, which the determinism
// analyzer enforces (SPEC.md §17.1).
//
// From M2 the ADK Runner loop is what this function *is* (D1, §8.2), mirroring
// Temporal's `contrib/googleadk`: the loop is the workflow, the model call is a
// step and — from M4 — each tool is a step. The pieces that make that true are
// not here, which is the point:
//
//   - `dbosadk.NewModel` makes each generation a step at §9.2's policy, and
//     `dbosadk.NewPlugin` refuses to run a model that is not wrapped.
//   - `session.New` makes each `AppendEvent` a `dbos.RunAsTransaction`, so the
//     event rows and the step checkpoint commit together (D2).
//   - The §17.1 analyzer walks outward from `RegisterWorkflow` through
//     everything this reaches, so an ADK internal that reads a clock is caught
//     here rather than at the first recovery.
//
// What is left in this function is ordinary Go that a replay re-executes.
//
// # When it goes back to M1's stub
//
// Two inputs take the two-step M1 shape instead: a process with no
// `Deps.DataSource`, which has no transaction to append in and no agent row to
// resolve; and [DurabilityProbeDigest], which is what failure-matrix rows F1 to
// F5 enqueue. See that constant for why the rows about step boundaries should
// not need an agent, a model or a key.
func AgentRun(ctx dbos.Context, in AgentRunInput) (AgentRunOutput, error) {
	// The session ID has to be stable across a replay, so it is either the
	// caller's or the workflow's own ID. Generating one here would be a UUID
	// call in workflow code: forbidden, and it would hand a recovered
	// workflow a different session than the one it started.
	sessionID := in.SessionID
	workflowUUID, err := dbos.GetWorkflowID(ctx)
	if err != nil {
		return AgentRunOutput{}, fmt.Errorf("workflow id: %w", err)
	}
	if sessionID == "" {
		sessionID = workflowUUID
	}

	deps := registeredDeps
	if deps.DataSource == nil || in.AgentDigest == DurabilityProbeDigest {
		return durabilityOnlyRun(ctx, in, sessionID)
	}

	// Resolution is a step because it is I/O — in M3 a registry pull — and
	// because a replay must run against the closure the first attempt resolved
	// rather than whatever the table holds now. The digest makes the two the
	// same in the ordinary case; the checkpoint makes them the same always.
	resolved, err := dbos.RunAsStep(ctx, func(stepCtx context.Context) (resolvedAgent, error) {
		return resolveAgentClosure(stepCtx, deps.Handle, in.AgentDigest)
	}, dbos.WithStepName("resolveAgent"))
	if err != nil {
		return AgentRunOutput{}, fmt.Errorf("resolve agent: %w", err)
	}

	r, svc, err := deps.buildRunner(resolved, workflowUUID)
	if err != nil {
		return AgentRunOutput{}, err
	}

	return runTurn(ctx, r, svc, in, sessionID)
}

// durabilityOnlyRun is M1's two-step workflow, kept as the path a process with
// no DataSource takes.
//
// Its steps do nothing an application would want. What they are for is the
// step *boundary*: failure-matrix row F1 kills a worker between them and
// asserts that the recovered workflow does not re-execute the first, which is a
// property of where the checkpoints are and not of what happens between them.
// A test of that should not need an agent, a model or a database.
func durabilityOnlyRun(ctx dbos.Context, in AgentRunInput, sessionID string) (AgentRunOutput, error) {
	digest, err := dbos.RunAsStep(ctx, func(context.Context) (string, error) {
		if in.AgentDigest == "" {
			return "", fmt.Errorf("agent digest is empty")
		}
		return in.AgentDigest, nil
	}, dbos.WithStepName("resolveAgent"))
	if err != nil {
		return AgentRunOutput{}, fmt.Errorf("resolve agent: %w", err)
	}

	// Durable, checkpointed and not re-slept on recovery.
	if _, err := dbos.Sleep(ctx, checkpointDelay); err != nil {
		return AgentRunOutput{}, fmt.Errorf("sleep: %w", err)
	}

	status, err := dbos.RunAsStep(ctx, func(context.Context) (string, error) {
		if sessionID == "" {
			return "", fmt.Errorf("session id is empty")
		}
		_ = digest
		return "SUCCESS", nil
	}, dbos.WithStepName("completeRun"))
	if err != nil {
		return AgentRunOutput{}, fmt.Errorf("complete run: %w", err)
	}

	return AgentRunOutput{SessionID: sessionID, Status: status}, nil
}
