// Package chained proves the walk is transitive: the workflow body itself is
// clean, and the violation is two calls deep. The diagnostic lands on the
// offending line, not on the workflow, because that is the line that has to
// change.
package chained

import (
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

type In struct{ Digest string }

type Out struct{ Status string }

var epoch = time.Unix(0, 0)

func Register(ctx dbos.Context) {
	dbos.RegisterWorkflow(ctx, Run)
}

func Run(ctx dbos.Context, in In) (Out, error) { // want Run:"nondeterministic: time.Since"
	if age() > 0 {
		return Out{Status: "SUCCESS"}, nil
	}
	return Out{Status: "PENDING"}, nil
}

func age() time.Duration {
	return drift()
}

func drift() time.Duration {
	return time.Since(epoch) // want "`time.Since` in workflow code.*\\(via chained.age -> chained.drift\\)"
}

// recurse is reachable from the workflow through age only if someone adds a
// call; it exists to prove the fixed point terminates through a cycle rather
// than to produce a diagnostic.
func recurse(n int) int {
	if n <= 0 {
		return 0
	}
	return recurse(n - 1)
}
