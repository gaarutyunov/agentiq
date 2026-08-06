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
	"fmt"
	"time"

	"encoding/json"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
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
}

// Deps is everything AgentRun's steps need from the outside world. It is
// passed to Register once at startup and captured by the registered workflow,
// so nothing inside workflow code has to reach for a global.
//
// At M1 it carries only the queue configuration. From M2 on it grows the
// generated client, the ADK model and the epos resolver; each of those is
// reached from inside a step, never from workflow code directly.
type Deps struct {
	// QueueName is the DBOS queue runs are enqueued on. Empty means
	// DefaultQueueName.
	QueueName string

	// WorkerConcurrency caps how many workflows this process runs at once.
	// Zero means DBOS's own default. It is the `AGENTIQ_QUEUE_CONCURRENCY`
	// knob of SPEC.md §10.1.
	WorkerConcurrency int
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
// At M1 there is no agent: the two steps stand in for the resolution and
// session bookkeeping that M3 and M2 fill in, and exist so that the durable
// execution guarantees have something to be proven against. What M1 proves is
// that a worker killed between them resumes without re-executing the first —
// which is a property of the step boundaries, not of what the steps do.
func AgentRun(ctx dbos.Context, in AgentRunInput) (AgentRunOutput, error) {
	// The session ID has to be stable across a replay, so it is either the
	// caller's or the workflow's own ID. Generating one here would be a UUID
	// call in workflow code: forbidden, and it would hand a recovered
	// workflow a different session than the one it started.
	sessionID := in.SessionID
	if sessionID == "" {
		id, err := dbos.GetWorkflowID(ctx)
		if err != nil {
			return AgentRunOutput{}, fmt.Errorf("workflow id: %w", err)
		}
		sessionID = id
	}

	digest, err := dbos.RunAsStep(ctx, func(context.Context) (string, error) {
		return resolveAgent(in.AgentDigest)
	}, dbos.WithStepName("resolveAgent"))
	if err != nil {
		return AgentRunOutput{}, fmt.Errorf("resolve agent: %w", err)
	}

	// Durable, checkpointed and not re-slept on recovery.
	if _, err := dbos.Sleep(ctx, checkpointDelay); err != nil {
		return AgentRunOutput{}, fmt.Errorf("sleep: %w", err)
	}

	status, err := dbos.RunAsStep(ctx, func(context.Context) (string, error) {
		return completeRun(digest, sessionID)
	}, dbos.WithStepName("completeRun"))
	if err != nil {
		return AgentRunOutput{}, fmt.Errorf("complete run: %w", err)
	}

	return AgentRunOutput{SessionID: sessionID, Status: status}, nil
}

// resolveAgent stands in for artifact.Resolve (SPEC.md §8.4), which resolves
// the digest-pinned agent closure through the epos API. That is M3 work and
// epos does not ship the public Go API it needs until then (SPEC.md §3.2), so
// M1 asserts the digest is present and returns it.
//
// It is a step because resolution is I/O: a registry pull. Resolution is
// idempotent and cacheable — the same digest always yields the same closure —
// which is what makes it safe to retry.
func resolveAgent(digest string) (string, error) {
	if digest == "" {
		return "", fmt.Errorf("agent digest is empty")
	}
	return digest, nil
}

// completeRun stands in for the session write that M2 does inside
// dbos.RunAsTransaction (SPEC.md §8.3), so that the event rows and the step
// checkpoint commit atomically. M1 owns no tables, so there is nothing to
// write and the step only reports the terminal status.
func completeRun(digest, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("session id is empty")
	}
	_ = digest
	return "SUCCESS", nil
}
