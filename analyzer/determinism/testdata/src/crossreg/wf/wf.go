// Package wf holds a workflow function that is registered from somewhere else.
// It registers nothing itself, so it gets no diagnostics — only a fact.
package wf

import (
	"os"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

type In struct{ Digest string }

type Out struct{ Status string }

func Run(ctx dbos.Context, in In) (Out, error) { // want Run:"nondeterministic: os.Getenv"
	if os.Getenv("AGENTIQ_DATABASE_URL") == "" {
		return Out{Status: "PENDING"}, nil
	}
	return Out{Status: "SUCCESS"}, nil
}
