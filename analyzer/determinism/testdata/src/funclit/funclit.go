// Package funclit registers a workflow written inline, with explicit type
// arguments. Both are shapes the call-site resolver has to handle: the entry
// point is an *ast.FuncLit rather than a named function, and RegisterWorkflow
// appears as an index expression rather than a bare selector.
package funclit

import (
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

type In struct{ Digest string }

type Out struct{ Status string }

func Register(ctx dbos.Context) {
	dbos.RegisterWorkflow[In, Out](ctx, func(ctx dbos.Context, in In) (Out, error) {
		_ = time.Now() // want "`time.Now` in workflow code"
		return Out{Status: "SUCCESS"}, nil
	})
}
