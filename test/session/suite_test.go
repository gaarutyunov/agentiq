//go:build integration

package session_test

import (
	"context"
	"testing"

	"github.com/gaarutyunov/gopgql/exec"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/sessiontestsuite"

	"github.com/gaarutyunov/agentiq/session"
)

// SPEC.md §14.1, §20 M2 DoD ("conformance suite green"), §21.
//
// ADK's own conformance suite, run against `postgres:19beta2` through the
// generated client. It is the check that says AgentIQ's session service is an
// ADK session service and not merely a store that resembles one — and it is
// where D5 is asserted from the outside: `partial_events_are_not_persisted`
// appends a partial event and requires the session to come back empty.
//
// It runs against a real database rather than a fake because everything M2
// stakes on the storage — normalized rows (D3), `json` and never `jsonb`
// (§7.2 rule 1), `part_index` on read (rule 2), NULL against empty (rule 3) —
// is invisible to a fake. A suite green against an in-memory map would be green
// against a schema that stored nothing correctly.
func TestSessionServiceConformance(t *testing.T) {
	// The DBOS context is not used directly: the suite calls the service from
	// outside a workflow. `setup` still builds it, because it is what creates
	// the DataSource `session.New` is given.
	ctx, pool, _ := setup(t)

	// A handle over the same pool the DataSource was built on.
	//
	// The suite calls the service from an ordinary test goroutine, not from
	// workflow code, and `dbos.RunAsTransaction` needs a workflow context — DBOS
	// hands out transactions, not connections, and `*dbos.DataSource` exposes no
	// way to reach its pool. SPEC.md §8.3's `New(ds)` is exactly the workflow
	// path; this is the other one, and `session.WithHandle` is what makes the
	// same service serve both. (`exec.Handle`, not the pool: §5 keeps pgx out of
	// `session/`.)
	svc := session.New(ds, session.WithHandle(exec.Pgx(pool)))

	sessiontestsuite.RunServiceTests(t, sessiontestsuite.SuiteOptions{
		// `agentiq.session`'s natural key is `(app_name, user_id, adk_id)` and
		// `Mutation.createSession` takes the ADK id as an argument, so a
		// caller-chosen id is stored as given.
		SupportsUserProvidedSessionID: true,
		// `agentiq.event.adk_id` is ADK's own `Event.ID`, stored verbatim; the
		// surrogate uuid gopgql owns is a separate column and never surfaces.
		// So an event read back carries the id the caller gave it.
		ProvidesServerAssignedEventID: false,
		AppName:                       conformanceAppName,
	}, func(t *testing.T) adksession.Service {
		truncate(ctx, t, pool)
		return svc
	})
}

const conformanceAppName = "agentiq-conformance"

// truncate empties the AgentIQ tables between the suite's subtests.
//
// The suite hands each subtest a service from `setup` and expects it to see
// only what that subtest wrote — `List` in particular counts sessions and would
// count an earlier subtest's. A fresh container per subtest would be the other
// way to get that, at about fifteen container starts and several minutes.
//
// `TRUNCATE` and not `DELETE`: it is a single statement over the seven tables
// with no per-row work, and `test/` is exempt from the no-hand-written-SQL rule
// precisely so a fixture can say something the read model has no reason to.
func truncate(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `TRUNCATE
		agentiq.state_delta,
		agentiq.artifact_delta,
		agentiq.actions,
		agentiq.part,
		agentiq.event,
		agentiq.session_state,
		agentiq.session`)
	require.NoError(t, err)
}
