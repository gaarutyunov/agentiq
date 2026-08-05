// Package escape is package bad written correctly: the same non-deterministic
// operations, every one of them behind a step boundary.
//
// This file carries no `want` comments, which is the point. The false-positive
// direction decides whether anyone leaves the linter switched on — an analyzer
// that flagged the whole transitive closure of a workflow would be turned off
// within a day, and then the true positives in package bad would go unseen
// too.
package escape

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

func Run(ctx dbos.Context, in In) (Out, error) {
	// A function literal handed to RunAsStep is not workflow code: DBOS runs
	// it once and replays its checkpointed result.
	_, err := dbos.RunAsStep(ctx, func(context.Context) (string, error) {
		started := time.Now()
		_ = time.Since(started)
		_ = mrand.Intn(10)

		buf := make([]byte, 4)
		_, _ = crand.Read(buf)

		_ = uuid.NewString()
		_ = os.Getenv("AGENTIQ_DATABASE_URL")
		_, _ = os.ReadFile("/etc/agentiq.yaml")
		_, _ = http.Get("https://example.invalid/agent")

		labels := map[string]int{"a": 1}
		for k, v := range labels {
			_ = k
			_ = v
		}

		go func() {}()

		ch := make(chan int, 1)
		ch <- 1
		select {
		case <-ch:
		default:
		}

		return "SUCCESS", nil
	}, dbos.WithStepName("everything"))
	if err != nil {
		return Out{}, err
	}

	// A named function handed to a step boundary is exempt the same way, and
	// the walk must not follow it.
	if _, err := dbos.RunAsStep(ctx, readEnvironment); err != nil {
		return Out{}, err
	}

	if _, err := dbos.RunAsTransaction(ctx, dbos.NewDataSource(), func(context.Context, any) (string, error) {
		return os.Getenv("AGENTIQ_DATABASE_URL"), nil
	}); err != nil {
		return Out{}, err
	}

	// dbos.Go is the deterministic substitute for a bare `go` statement, and
	// dbos.Select for a bare `select`.
	first, err := dbos.Go(ctx, func(context.Context) (int, error) { return mrand.Intn(3), nil })
	if err != nil {
		return Out{}, err
	}
	second, err := dbos.Go(ctx, readClock)
	if err != nil {
		return Out{}, err
	}
	if _, err := dbos.Select(ctx, []<-chan dbos.StepOutcome[int]{first, second}); err != nil {
		return Out{}, err
	}

	// A durable sleep is not a clock read: DBOS records the wake-up time, so
	// a recovered workflow does not sleep again.
	if _, err := dbos.Sleep(ctx, 2*time.Second); err != nil {
		return Out{}, err
	}

	// Deterministic helpers in the same package are followed and found clean.
	return Out{Status: label(in)}, nil
}

func readEnvironment(context.Context) (string, error) {
	return os.Getenv("AGENTIQ_DATABASE_URL"), nil
}

func readClock(context.Context) (int, error) {
	return time.Now().Nanosecond(), nil
}

func label(in In) string {
	if in.Digest == "" {
		return "PENDING"
	}
	return "SUCCESS"
}
