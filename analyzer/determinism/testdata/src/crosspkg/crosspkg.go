// Package crosspkg registers a workflow that reaches a non-deterministic
// helper in another package of the same module. The violation is in
// crosspkg/helper, where this pass may not report, so the diagnostic lands on
// the call site that reaches it and names the position of the original.
package crosspkg

import (
	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"crosspkg/helper"
)

type In struct{ Digest string }

type Out struct{ Status string }

func Register(ctx dbos.Context) {
	dbos.RegisterWorkflow(ctx, Run)
}

func Run(ctx dbos.Context, in In) (Out, error) { // want Run:"nondeterministic: time.Now"
	// Deterministic: followed across the package boundary and found clean.
	status := helper.Label(in.Digest)

	if helper.Stamp() > 0 { // want "`time.Now` in workflow code.*\\(via helper.Stamp\\).*helper.go:11"
		return Out{Status: status}, nil
	}
	return Out{Status: "PENDING"}, nil
}
