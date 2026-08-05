// Package bad registers a workflow whose body performs every operation the
// determinism contract forbids (SPEC.md §9.1). Every one of them must be
// diagnosed; package escape is the same list written correctly.
package bad

import (
	"context"
	crand "crypto/rand"
	mrand "math/rand"
	"net/http"
	"os"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"
)

type In struct{ Digest string }

type Out struct{ Status string }

func Register(ctx dbos.Context) {
	dbos.RegisterWorkflow(ctx, Run)
}

// The fact records what Run can reach, by short tag, for any package that
// calls it. It is what package crossreg reads across a package boundary.
func Run(ctx dbos.Context, in In) (Out, error) { // want Run:"nondeterministic: crypto/rand, go-stmt, map-range, math/rand, net/http, os.Getenv, os.ReadFile, select-stmt, time.Now, time.Since, uuid"
	started := time.Now()   // want "`time.Now` in workflow code"
	_ = time.Since(started) // want "`time.Since` in workflow code"

	_ = mrand.Intn(10) // want "`math/rand` in workflow code"

	buf := make([]byte, 4)
	_, _ = crand.Read(buf) // want "`crypto/rand` in workflow code"

	_ = uuid.NewString() // want "UUID generation in workflow code"

	// Parsing a UUID is deterministic and must not be diagnosed.
	_, _ = uuid.Parse(in.Digest)

	_ = os.Getenv("AGENTIQ_DATABASE_URL")   // want "`os.Getenv` in workflow code"
	_, _ = os.ReadFile("/etc/agentiq.yaml") // want "`os.ReadFile` in workflow code"

	_, _ = http.Get("https://example.invalid/agent") // want "`net/http` in workflow code"

	labels := map[string]int{"a": 1, "b": 2}
	total := 0
	for k, v := range labels { // want "`range` over a map in workflow code"
		_ = k
		total += v
	}
	_ = total

	// A range with neither key nor value cannot observe iteration order, so
	// it is deterministic and must not be diagnosed.
	count := 0
	for range labels {
		count++
	}
	_ = count

	go sideEffect() // want "bare `go` statement in workflow code"

	ch := make(chan int, 1)
	ch <- 1
	select { // want "bare `select` in workflow code"
	case <-ch:
	default:
	}

	// The step boundary exempts the function value it is handed, not the
	// expression that builds it: this time.Now() runs in the replayed body.
	_, _ = dbos.RunAsStep(ctx, stepStamped(time.Now())) // want "`time.Now` in workflow code"

	return Out{Status: "SUCCESS"}, nil
}

func sideEffect() {}

func stepStamped(t time.Time) dbos.Step[string] {
	return func(context.Context) (string, error) { return t.String(), nil }
}
