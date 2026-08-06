// Package crossreg registers a workflow that is declared in another package.
// Nothing about the violation is at a position this pass may report, so the
// registration site carries it — with the chain and the rendered position of
// the original, which is the only thing that makes such a diagnostic
// actionable.
package crossreg

import (
	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"crossreg/wf"
)

func Register(ctx dbos.Context) {
	dbos.RegisterWorkflow(ctx, wf.Run) // want "wf.Run is registered as a workflow but is not deterministic: `os.Getenv` in workflow code"
}
