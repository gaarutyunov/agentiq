//go:build integration

package durable

import (
	"context"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/stretchr/testify/require"

	"github.com/gaarutyunov/agentiq/test/harness"
)

// TestDurableExecution runs the canonical feature file.
//
// The suite owns one PostgreSQL container for its whole run and resets to a
// snapshot between scenarios. Starting a container per scenario would be
// cleaner and would cost roughly a minute of the twenty the integration budget
// allows (SPEC.md §18.1), most of it initdb.
func TestDurableExecution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	t.Cleanup(cancel)

	// WithProxy is not only for failure-matrix row F3.
	//
	// Every worker connects through the proxy, because a container's
	// *published* port changes across a stop/start: after F2 bounces the
	// database, a worker holding the direct DSN can never reconnect, and the
	// scenario fails for a reason that has nothing to do with durability. The
	// proxy's listening port is stable, so the DSN survives the restart the
	// same way a real deployment's hostname does.
	pg := harness.StartPostgres(ctx, t, harness.WithProxy())

	// Create `dbos.*` once, then snapshot. "A clean database" for a system
	// whose schema is owned by DBOS (SPEC.md §2.2) means clean *and* migrated:
	// restoring to a database without `dbos.*` would make every scenario pay
	// for the migrations again.
	client := harness.NewDBOSClient(ctx, t, pg.ProxiedDSN)
	require.NoError(t, client.Shutdown(client, 30*time.Second), "shut down the migrating client")
	require.NoError(t, pg.Snapshot(ctx), "snapshot the migrated database")

	s := &suite{pg: pg, ctx: ctx}

	opts, err := harness.GodogOptions(t, "@integration", "durable-junit.xml")
	require.NoError(t, err)

	status := godog.TestSuite{
		Name:                "durable-execution",
		ScenarioInitializer: s.initialize,
		Options:             &opts,
	}.Run()

	// godog reports through TestingT as well, but a non-zero exit status with
	// no failed subtest means the run itself broke — an unparsable feature
	// file, an unwritable report — and that has to fail the test too.
	require.Zero(t, status, "godog exit status")
}

func (s *suite) initialize(sc *godog.ScenarioContext) {
	sc.Before(s.before)
	sc.After(s.after)

	// Background
	sc.Step(`^a clean PostgreSQL 19 database$`, s.aCleanDatabase)
	sc.Step(`^a registered workflow with two steps separated by a durable sleep$`, s.aRegisteredWorkflow)

	// Given
	sc.Step(`^a running worker process$`, s.aRunningWorker)
	sc.Step(`^a running worker process connected through a network proxy$`, s.aRunningWorkerThroughProxy)
	sc.Step(`^the generated property graph is applied$`, s.theGraphIsApplied)

	// When
	sc.Step(`^a run is enqueued$`, s.aRunIsEnqueued)
	sc.Step(`^a run with no agent digest is enqueued$`, s.aRunWithNoDigestIsEnqueued)
	sc.Step(`^the workflow completes step one$`, s.theWorkflowCompletesStepOne)
	sc.Step(`^the worker process is killed$`, s.theWorkerIsKilled)
	sc.Step(`^a new worker starts$`, s.aNewWorkerStarts)
	sc.Step(`^the database container is stopped and restarted$`, s.theDatabaseIsBounced)
	sc.Step(`^the connection to the database is cut for (\d+) seconds?$`, s.theConnectionIsCut)
	sc.Step(`^the recovery attempt counter is seeded to its limit$`, s.theRecoveryCounterIsSeeded)

	// Then
	sc.Step(`^step one is not re-executed$`, s.stepOneIsNotReExecuted)
	sc.Step(`^the workflow completes successfully$`, s.theWorkflowCompletesSuccessfully)
	sc.Step(`^the workflow resumes without duplicating completed steps$`, s.theWorkflowResumesWithoutDuplicates)
	sc.Step(`^the workflow status becomes ERROR$`, s.theWorkflowStatusBecomesError)
	sc.Step(`^the workflow error is recorded$`, s.theWorkflowErrorIsRecorded)
	sc.Step(`^no step completed successfully$`, s.noStepCompletedSuccessfully)
	sc.Step(`^the workflow status becomes MAX_RECOVERY_ATTEMPTS_EXCEEDED$`, s.theWorkflowIsDeadLettered)
	sc.Step(`^the workflow is no longer on a queue$`, s.theWorkflowIsNoLongerQueued)
	sc.Step(`^the property graph returns the workflow with steps (\w+) and (\w+)$`, s.theGraphReturnsSteps)
	sc.Step(`^the property graph reports the workflow status as (\w+)$`, s.theGraphReportsStatus)
}
