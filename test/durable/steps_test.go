//go:build integration

package durable

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cucumber/godog"
	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gaarutyunov/agentiq/test/harness"
	"github.com/gaarutyunov/agentiq/workflow"
)

// Step names as workflow.AgentRun registers them (dbos.WithStepName).
const (
	stepResolveAgent = "resolveAgent"
	stepCompleteRun  = "completeRun"
)

// Timeouts. They are generous: a scenario that has to start a container, build
// nothing and kill a process is dominated by Docker, and a tight bound here
// turns a slow CI runner into a false failure about durability.
const (
	stepTimeout     = 60 * time.Second
	terminalTimeout = 2 * time.Minute
)

// suite is the scenario state. Everything except pg is per-scenario and is
// reset by before/after.
type suite struct {
	pg  *harness.Postgres
	ctx context.Context

	tb     *harness.ScenarioTB
	worker *harness.Worker
	client dbos.Client
	pool   *pgxpool.Pool

	workflowID string

	// stepOne is the resolveAgent checkpoint as it stood before the
	// interruption. "Not re-executed" is the claim that this row is byte-for-
	// byte the same afterwards.
	stepOne     dbos.StepInfo
	haveStepOne bool
}

func (s *suite) before(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
	s.tb = harness.NewScenarioTB(godog.T(ctx))
	s.worker = nil
	s.client = nil
	s.pool = nil
	s.workflowID = ""
	s.stepOne = dbos.StepInfo{}
	s.haveStepOne = false

	// Reset before the scenario rather than after it, so a failed scenario
	// leaves its database behind for inspection.
	if err := s.pg.Restore(s.ctx); err != nil {
		return ctx, err
	}
	return ctx, nil
}

func (s *suite) after(ctx context.Context, _ *godog.Scenario, err error) (context.Context, error) {
	if err != nil && s.worker != nil {
		godog.Logf(ctx, "worker output:\n%s", s.worker.Output())
	}
	// Everything holding a connection has to let go before the next
	// scenario's Restore: PostgreSQL will not drop a database with sessions
	// attached, and the failure would be reported against the wrong scenario.
	if s.worker != nil {
		_ = s.worker.Kill()
	}
	if s.tb != nil {
		s.tb.Close()
	}
	return ctx, nil
}

// observer opens (once per scenario) the client the suite watches through. It
// is not a worker: it registers nothing and claims no queue row.
func (s *suite) observer() dbos.Client {
	if s.client == nil {
		s.client = harness.NewDBOSClient(s.ctx, s.tb, s.pg.ProxiedDSN)
	}
	return s.client
}

func (s *suite) sqlPool() *pgxpool.Pool {
	if s.pool == nil {
		s.pool = harness.OpenPool(s.ctx, s.tb, s.pg.ProxiedDSN)
	}
	return s.pool
}

// --- Background ------------------------------------------------------------

// aCleanDatabase is satisfied by the snapshot restore in before(). The step
// exists because the feature file says so and the feature file is canonical
// (SPEC.md §14.3); implementing it as a no-op with a comment is honest, and
// deleting it from the feature to avoid an empty step definition would not be.
func (s *suite) aCleanDatabase() error { return nil }

// aRegisteredWorkflow asserts the registration DBOS will dispatch by name.
//
// It is not a no-op: an enqueue names a workflow, and if `workflow/` renamed
// or moved AgentRun, the enqueue would still succeed and the run would sit in
// the queue forever. Failing here names the cause.
func (s *suite) aRegisteredWorkflow() error {
	if harness.AgentRunWorkflowName == "" {
		return fmt.Errorf("workflow name did not resolve")
	}
	assert.Equal(s.tb.T, "github.com/gaarutyunov/agentiq/workflow.AgentRun", harness.AgentRunWorkflowName,
		"DBOS dispatches by fully qualified function name; a move or rename breaks enqueue silently")
	return nil
}

// --- Given -----------------------------------------------------------------

func (s *suite) aRunningWorker() error {
	s.worker = harness.StartWorker(s.ctx, s.tb, s.pg.ProxiedDSN)
	return nil
}

// aRunningWorkerThroughProxy is the same worker: every worker in this suite
// already connects through toxiproxy, for the reason TestDurableExecution
// explains. The step exists so the F3 scenario states its precondition rather
// than relying on a fixture decision made elsewhere, and it fails loudly if
// that decision is ever reversed.
func (s *suite) aRunningWorkerThroughProxy() error {
	if s.pg.ProxiedDSN == "" {
		return fmt.Errorf("the fixture has no proxy; F3 cannot partition anything")
	}
	return s.aRunningWorker()
}

// theGraphIsApplied applies everything AgentIQ owns. The property graph is the
// last thing the sequence does, and the only part of it this scenario then
// reads, which is why the step reads as it does in the feature file.
//
// It applies twice, and that is an assertion rather than belt and braces: two
// of the three programs that run this sequence re-run it routinely — the
// browser on every tab reload (failure-matrix row F22) and the server on every
// restart — and neither has goose to tell it what has already been applied. A
// second application that failed would be a demo that works exactly once.
func (s *suite) theGraphIsApplied() error {
	if err := harness.ApplyMigrations(s.ctx, s.sqlPool()); err != nil {
		return err
	}
	if err := harness.ApplyMigrations(s.ctx, s.sqlPool()); err != nil {
		return fmt.Errorf("re-applying must be a no-op (the browser reloads, the server restarts): %w", err)
	}
	return nil
}

// --- When ------------------------------------------------------------------

func (s *suite) aRunIsEnqueued() error { return s.enqueue("sha256:m1-placeholder-digest") }

// aRunWithNoDigestIsEnqueued drives failure-matrix row F4. workflow.resolveAgent
// rejects an empty digest, which is the only permanent step error M1's
// workflow can be made to produce without changing production code.
func (s *suite) aRunWithNoDigestIsEnqueued() error { return s.enqueue("") }

func (s *suite) enqueue(digest string) error {
	id, err := harness.Enqueue(s.observer(), workflow.AgentRunInput{
		AgentDigest: digest,
		AppName:     "agentiq",
		UserID:      "integration",
		Message:     json.RawMessage(`{"text":"m1"}`),
	})
	if err != nil {
		return err
	}
	s.workflowID = id
	return nil
}

func (s *suite) theWorkflowCompletesStepOne() error {
	step, err := harness.WaitForStep(s.ctx, s.observer(), s.workflowID, stepResolveAgent, stepTimeout)
	if err != nil {
		return err
	}
	s.stepOne = step
	s.haveStepOne = true

	// The kill has to land before completeRun. workflow.checkpointDelay is a
	// two-second dbos.Sleep placed between the steps for exactly this, so
	// arriving here means the window is open.
	if _, ok := harness.FindStep(mustSteps(s), stepCompleteRun); ok {
		return fmt.Errorf("step two already completed; the interruption window closed before the scenario could use it")
	}
	return nil
}

func (s *suite) theWorkerIsKilled() error {
	if s.worker == nil {
		return fmt.Errorf("no worker to kill")
	}
	err := s.worker.Kill()
	s.worker = nil
	// A SIGKILLed process reports "signal: killed"; that is the success case.
	if err != nil && err.Error() != "signal: killed" {
		return fmt.Errorf("kill the worker: %w", err)
	}
	return nil
}

func (s *suite) aNewWorkerStarts() error { return s.aRunningWorker() }

func (s *suite) theDatabaseIsBounced() error { return s.pg.Bounce(s.ctx) }

func (s *suite) theConnectionIsCut(seconds int) error {
	return s.pg.Partition(s.ctx, time.Duration(seconds)*time.Second)
}

// theRecoveryCounterIsSeeded compresses failure-matrix row F5's kill loop. See
// harness.SeedRecoveryAttempts for why.
func (s *suite) theRecoveryCounterIsSeeded() error {
	return harness.SeedRecoveryAttempts(s.ctx, s.sqlPool(), s.workflowID,
		harness.DefaultMaxRecoveryAttempts+1)
}

// --- Then ------------------------------------------------------------------

// stepOneIsNotReExecuted is the assertion the whole of failure-matrix row F1
// reduces to. A re-executed step would produce a new checkpoint with a new
// completion time; an identical CompletedAt is the checkpoint that was written
// once and read back on recovery.
func (s *suite) stepOneIsNotReExecuted() error {
	if !s.haveStepOne {
		return fmt.Errorf("step one was never observed before the interruption")
	}
	steps, err := harness.Steps(s.observer(), s.workflowID)
	if err != nil {
		return err
	}
	after, ok := harness.FindStep(steps, stepResolveAgent)
	if !ok {
		return fmt.Errorf("step %q vanished from the checkpoint table", stepResolveAgent)
	}

	assert.Equal(s.tb.T, s.stepOne.StepID, after.StepID, "step one changed function_id across recovery")
	assert.True(s.tb.T, s.stepOne.CompletedAt.Equal(after.CompletedAt),
		"step one was re-executed: completed_at moved from %s to %s",
		s.stepOne.CompletedAt, after.CompletedAt)
	assert.Equal(s.tb.T, s.stepOne.Output, after.Output, "step one produced a different output on recovery")
	assert.Equal(s.tb.T, 1, countSteps(steps, stepResolveAgent), "step one is checkpointed more than once")
	return nil
}

func (s *suite) theWorkflowCompletesSuccessfully() error {
	st, err := harness.WaitForStatus(s.ctx, s.observer(), s.workflowID, terminalTimeout,
		dbos.WorkflowStatusSuccess, dbos.WorkflowStatusError,
		dbos.WorkflowStatusCancelled, dbos.WorkflowStatusMaxRecoveryAttemptsExceeded)
	if err != nil {
		return err
	}
	if st.Status != dbos.WorkflowStatusSuccess {
		return fmt.Errorf("workflow %s reached %s, want SUCCESS (error: %v)", s.workflowID, st.Status, st.Error)
	}
	return nil
}

func (s *suite) theWorkflowResumesWithoutDuplicates() error {
	if err := s.theWorkflowCompletesSuccessfully(); err != nil {
		return err
	}
	steps, err := harness.Steps(s.observer(), s.workflowID)
	if err != nil {
		return err
	}
	for _, name := range []string{stepResolveAgent, stepCompleteRun} {
		assert.Equal(s.tb.T, 1, countSteps(steps, name), "step %q is checkpointed more than once", name)
	}
	return s.stepOneIsNotReExecuted()
}

func (s *suite) theWorkflowStatusBecomesError() error {
	_, err := harness.WaitForStatus(s.ctx, s.observer(), s.workflowID, terminalTimeout, dbos.WorkflowStatusError)
	return err
}

func (s *suite) theWorkflowErrorIsRecorded() error {
	st, err := harness.Status(s.observer(), s.workflowID)
	if err != nil {
		return err
	}
	require.Error(s.tb.T, st.Error, "workflow_status.error is empty for a failed workflow")
	assert.Contains(s.tb.T, st.Error.Error(), "digest",
		"the recorded error should name what actually failed")
	return nil
}

// noStepCompletedSuccessfully asserts that a failed workflow leaves no
// checkpoint claiming success. It is the half of failure-matrix row F4 that
// matters for durability: a recorded success would be replayed on recovery.
func (s *suite) noStepCompletedSuccessfully() error {
	steps, err := harness.Steps(s.observer(), s.workflowID)
	if err != nil {
		return err
	}
	for _, step := range steps {
		assert.Error(s.tb.T, step.Error, "step %q was checkpointed without an error on a failed workflow", step.StepName)
	}
	assert.Equal(s.tb.T, 0, countSteps(steps, stepCompleteRun), "the second step ran despite the first failing")
	return nil
}

func (s *suite) theWorkflowIsDeadLettered() error {
	_, err := harness.WaitForStatus(s.ctx, s.observer(), s.workflowID, terminalTimeout,
		dbos.WorkflowStatusMaxRecoveryAttemptsExceeded)
	return err
}

// theWorkflowIsNoLongerQueued asserts the dead-letter side effect: DBOS clears
// queue_name so a dead-lettered workflow cannot be dequeued again.
func (s *suite) theWorkflowIsNoLongerQueued() error {
	st, err := harness.Status(s.observer(), s.workflowID)
	if err != nil {
		return err
	}
	assert.Empty(s.tb.T, st.QueueName, "a dead-lettered workflow is still on a queue")
	return nil
}

// theGraphReturnsSteps runs the generated GRAPH_TABLE traversal — SPEC.md
// §16's M1 acceptance, server half.
func (s *suite) theGraphReturnsSteps(first, second string) error {
	rows, err := harness.GraphWorkflow(s.ctx, s.sqlPool(), s.workflowID)
	if err != nil {
		return err
	}
	require.Len(s.tb.T, rows, 1, "the graph returned %d workflows for one workflow_uuid", len(rows))

	names := make([]string, 0, len(rows[0].Steps))
	for _, step := range rows[0].Steps {
		if step.FunctionName != nil {
			names = append(names, *step.FunctionName)
		}
	}
	// Named rather than counted: dbos.Sleep is itself a checkpoint, so the
	// row count is DBOS's business and the step names are AgentRun's.
	assert.Contains(s.tb.T, names, first)
	assert.Contains(s.tb.T, names, second)
	return nil
}

func (s *suite) theGraphReportsStatus(want string) error {
	rows, err := harness.GraphWorkflow(s.ctx, s.sqlPool(), s.workflowID)
	if err != nil {
		return err
	}
	require.Len(s.tb.T, rows, 1)
	assert.Equal(s.tb.T, want, rows[0].Status)
	assert.Equal(s.tb.T, s.workflowID, rows[0].WorkflowUuid)
	return nil
}

// --- helpers ---------------------------------------------------------------

func countSteps(steps []dbos.StepInfo, name string) int {
	n := 0
	for _, s := range steps {
		if s.StepName == name {
			n++
		}
	}
	return n
}

// mustSteps reads the checkpoints, treating a read failure as "none yet". It is
// only used where absence and failure lead to the same conclusion.
func mustSteps(s *suite) []dbos.StepInfo {
	steps, err := harness.Steps(s.observer(), s.workflowID)
	if err != nil {
		return nil
	}
	return steps
}
