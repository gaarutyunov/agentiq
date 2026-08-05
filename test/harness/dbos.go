package harness

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/gaarutyunov/agentiq/workflow"
)

// AgentRunWorkflowName is the name DBOS dispatches workflow.AgentRun by.
//
// DBOS resolves a registered workflow's name from runtime.FuncForPC on the
// function value (dbos/workflow.go, resolveWorkflowFunctionName), so the name
// is derived the same way here rather than written out. A literal would be a
// second source of truth that a package rename silently breaks: the enqueue
// would succeed, the worker would never claim the row, and the scenario would
// fail as a timeout with nothing pointing at the cause.
var AgentRunWorkflowName = runtime.FuncForPC(reflect.ValueOf(workflow.AgentRun).Pointer()).Name()

// AppVersion pins the DBOS application version on both sides of a run.
//
// DBOS's queue dequeue is filtered by application version:
//
//	WHERE queue_name = $1 AND status = 'ENQUEUED' AND application_version = $3
//
// (dbos/internal/sysdb/system_database.go, DequeueWorkflows). An executor
// computes its own version by hashing the workflows it registered, so a client
// that registers nothing — which is exactly what an external enqueuer is —
// computes a different one, and the run sits at ENQUEUED forever while the
// worker logs nothing at all. It is not a timeout, a permissions problem or a
// queue-name mismatch, and it looks like all three.
//
// Both sides are pinned to a literal instead: the worker through DBOS__APPVERSION
// (which overrides Config.ApplicationVersion) and the enqueue through
// [dbos.WithEnqueueApplicationVersion].
//
// The same trap sits under SPEC.md §7.4's SQL enqueue path.
// `dbos.enqueue_workflow` takes `app_version` defaulting to NULL, and a NULL is
// only dequeued when the executor happens to be the *latest registered*
// version — so a production `startAgentRun` inherits this behaviour rather than
// escaping it.
const AppVersion = "agentiq-integration"

// AppVersionEnv is the environment variable DBOS reads the application version
// from. It is DBOS's own name, with the double underscore.
const AppVersionEnv = "DBOS__APPVERSION"

// NewDBOSClient opens a DBOS client against dsn and registers its shutdown.
//
// The client is how the suite enqueues and observes. It is not a worker: it
// runs no workflow and claims no queue row, so it can watch a run without
// becoming the thing that executes it.
//
// SPEC.md §7.4 names this library as what `agentiq admin` uses for cancel,
// resume and fork — the operations with no SQL function. It is also the only
// working enqueue path: see [Enqueue].
func NewDBOSClient(ctx context.Context, tb testingTB, dsn string) dbos.Client {
	tb.Helper()

	client, err := dbos.NewClient(ctx, dbos.ClientConfig{
		DatabaseURL: dsn,
		// SPEC.md §10.1: the application database and the DBOS system
		// database are the same database. There is no second URL.
		DatabaseSchema: "dbos",
	})
	if err != nil {
		tb.Fatalf("harness: open a DBOS client: %v", err)
	}
	tb.Cleanup(func() { _ = client.Shutdown(client, 10*time.Second) })
	return client
}

// Enqueue starts a run and returns its workflow ID.
//
// # Why this does not go through the generated client
//
// SPEC.md §7.4 maps `Mutation.startAgentRun(agentDigest, userId, input, queue,
// deduplicationId, priority)` onto `dbos.enqueue_workflow` with named-argument
// notation, and gopgql generates exactly that:
//
//	SELECT dbos.enqueue_workflow(agent_digest => $1, user_id => $2, input => $3,
//	                             queue => $4, deduplication_id => $5, priority => $6)
//
// `dbos.enqueue_workflow` in DBOS Go v1.0.0 — the pinned version — has none of
// `agent_digest`, `user_id`, `input` or `queue`. Its parameters are
// workflow_name, queue_name, positional_args, named_args, class_name,
// config_name, workflow_id, app_version, timeout_ms, deadline_epoch_ms,
// deduplication_id, priority, queue_partition_key, authenticated_user,
// authenticated_roles, delay_until_epoch_ms
// (dbos/internal/sysdb/migrations/38_update_enqueue_workflow.sql). Four of the
// six named arguments do not exist, so the generated statement fails with
// "function dbos.enqueue_workflow(...) does not exist" — the SDL is describing
// a function signature that is not the one DBOS ships.
//
// Enqueueing through the DBOS client library is the same architectural path —
// every externally triggered run still passes through a DBOS queue, there is
// still no unqueued start — with the argument names the database actually has.
func Enqueue(client dbos.Client, in workflow.AgentRunInput, opts ...dbos.EnqueueOption) (string, error) {
	// Prepended, so a caller can still override it.
	opts = append([]dbos.EnqueueOption{dbos.WithEnqueueApplicationVersion(AppVersion)}, opts...)

	handle, err := dbos.Enqueue[workflow.AgentRunOutput](
		client, workflow.DefaultQueueName, AgentRunWorkflowName, in, opts...)
	if err != nil {
		return "", fmt.Errorf("harness: enqueue: %w", err)
	}
	return handle.GetWorkflowID(), nil
}

// ErrWorkflowNotFound is returned when no workflow with the given ID exists.
var ErrWorkflowNotFound = errors.New("harness: workflow not found")

// Status reads one workflow's status row.
//
// LoadOutput is on because it also controls whether `Error` is populated.
// Without it a failed workflow reads back with a nil Error and a status of
// ERROR, which looks exactly like "DBOS recorded the failure but lost the
// reason" — failure-matrix row F4's assertion would fail against a database
// that has the error stored correctly.
func Status(client dbos.Client, id string) (dbos.WorkflowStatus, error) {
	rows, err := dbos.ListWorkflows(client,
		dbos.WithFilterWorkflowIDs(id),
		dbos.WithFilterLoadOutput(true),
		dbos.WithFilterLoadInput(false))
	if err != nil {
		return dbos.WorkflowStatus{}, fmt.Errorf("harness: list workflows: %w", err)
	}
	if len(rows) == 0 {
		return dbos.WorkflowStatus{}, ErrWorkflowNotFound
	}
	if len(rows) > 1 {
		return dbos.WorkflowStatus{}, fmt.Errorf("harness: %d rows for workflow %s, want 1", len(rows), id)
	}
	return rows[0], nil
}

// Steps reads a workflow's recorded step checkpoints, in `function_id` order.
//
// These rows are `dbos.operation_outputs` — the checkpoints the durability
// guarantee is made of. "Step one is not re-executed" is a statement about
// this table and nothing else.
func Steps(client dbos.Client, id string) ([]dbos.StepInfo, error) {
	steps, err := dbos.GetWorkflowSteps(client, id, dbos.WithStepsLoadOutput(true))
	if err != nil {
		return nil, fmt.Errorf("harness: get workflow steps: %w", err)
	}
	return steps, nil
}

// FindStep returns the checkpoint for a named step, and whether it exists.
func FindStep(steps []dbos.StepInfo, name string) (dbos.StepInfo, bool) {
	for _, s := range steps {
		if s.StepName == name {
			return s, true
		}
	}
	return dbos.StepInfo{}, false
}

// WaitForStatus blocks until the workflow reaches one of want, or the timeout
// expires.
//
// It polls. DBOS notifies on completion, but a test that waited on the
// notification would be a test of the notification, and F21 exists precisely
// because a notification can go missing.
func WaitForStatus(ctx context.Context, client dbos.Client, id string, timeout time.Duration, want ...dbos.WorkflowStatusType) (dbos.WorkflowStatus, error) {
	var last dbos.WorkflowStatus
	err := poll(ctx, timeout, func() (bool, error) {
		st, err := Status(client, id)
		if errors.Is(err, ErrWorkflowNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		last = st
		for _, w := range want {
			if st.Status == w {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return last, fmt.Errorf("harness: workflow %s did not reach %v (last status %q): %w",
			id, want, last.Status, err)
	}
	return last, nil
}

// WaitForStep blocks until a named step has a checkpoint, and returns it.
func WaitForStep(ctx context.Context, client dbos.Client, id, name string, timeout time.Duration) (dbos.StepInfo, error) {
	var found dbos.StepInfo
	err := poll(ctx, timeout, func() (bool, error) {
		steps, err := Steps(client, id)
		if err != nil {
			// The workflow may not have a status row yet, which is not an
			// error worth aborting on this early.
			return false, nil //nolint:nilerr // absence is "not yet", not a failure
		}
		s, ok := FindStep(steps, name)
		if !ok {
			return false, nil
		}
		found = s
		return true, nil
	})
	if err != nil {
		return dbos.StepInfo{}, fmt.Errorf("harness: step %q of workflow %s never checkpointed: %w", name, id, err)
	}
	return found, nil
}

// poll runs cond until it reports done, errors, or the timeout expires.
func poll(ctx context.Context, timeout time.Duration, cond func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		done, err := cond()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}
