// Command agentiq is the AgentIQ server binary (SPEC.md §4).
//
// It registers every workflow, launches the DBOS queue worker, and serves the
// API. Packing and publishing agent artifacts is `epos`, not this binary
// (SPEC.md §4, §13.5); `cancel`, `resume` and `fork` have no SQL function and
// become `agentiq admin` subcommands using the DBOS client library directly
// (SPEC.md §7.4).
//
// M1 needs a server binary because failure-matrix row F1 kills a child worker
// process to prove the workflow resumes — there is no other worker to kill.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/spf13/cobra"
	adkmodel "google.golang.org/adk/v2/model"

	"github.com/gaarutyunov/agentiq/migrate"
	"github.com/gaarutyunov/agentiq/model"
	"github.com/gaarutyunov/agentiq/workflow"
)

// shutdownTimeout bounds how long DBOS is given to drain in-flight work.
const shutdownTimeout = 5 * time.Second

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "agentiq:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "agentiq",
		Short:         "AgentIQ server",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newServeCmd())
	return root
}

func newServeCmd() *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Register workflows, run the queue worker, and serve the API",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context(), addr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", envOr("AGENTIQ_ADDR", ":8080"), "address the HTTP server listens on")

	return cmd
}

func serve(parent context.Context, addr string) error {
	// SPEC.md §10.1: the application database and the DBOS system database
	// must be the same database. Separate databases forfeit the
	// single-transaction guarantee RunAsTransaction provides, which is the
	// whole reason DataSources were chosen. There is deliberately no second
	// URL to configure.
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is not set")
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbosCtx, err := dbos.NewContext(ctx, dbos.Config{
		AppName:        "agentiq",
		DatabaseURL:    databaseURL,
		DatabaseSchema: envOr("DBOS_SYSTEM_SCHEMA", "dbos"),
	})
	if err != nil {
		return fmt.Errorf("dbos context: %w", err)
	}
	defer func() { _ = dbos.Shutdown(dbosCtx, shutdownTimeout) }()

	// After NewContext, before Launch. NewContext is what runs `dbos migrate`,
	// and the property graph applied below projects `dbos.*` — applying it
	// first fails with "relation does not exist". Launch, in turn, starts
	// recovering workflows, and a recovered run appends session events into
	// tables that have to be there already.
	//
	// This call is new in M2 and its absence was a real gap rather than a
	// simplification: the server created no `agentiq` schema, applied no table
	// history and applied no property graph, and only the browser demo — which
	// did all three in a copy of the sequence — made that look like it worked.
	// See migrate/migrate.go.
	//
	// M2 keeps the pool rather than discarding it: a turn appends events inside
	// `dbos.RunAsTransaction` and reads sessions and agent rows outside one, so
	// the server needs both handles for its lifetime. `migrate.Open` is what
	// keeps pgx out of this file — see migrate/runtime.go.
	rt, err := migrate.Open(ctx, dbosCtx, databaseURL)
	if err != nil {
		return err
	}
	defer rt.Close()

	concurrency, err := envInt("AGENTIQ_QUEUE_CONCURRENCY")
	if err != nil {
		return err
	}

	// SPEC.md §10.1: the server reads its OpenRouter key from the environment,
	// once, here. Reading it inside the workflow would be `os.Getenv` in
	// workflow code — forbidden by §9.1 and caught by the §17.1 analyzer — and
	// it would mean a replay depended on the environment of whichever executor
	// recovered it.
	//
	// An absent key is not fatal at startup. A worker with no key can still
	// recover and complete the durable-execution rows (F1-F5), which need no
	// model; the failure belongs at the first generation, where it can say
	// which run wanted a model and could not have one.
	apiKey, keyErr := model.APIKeyFromEnv()

	if err := workflow.Register(dbosCtx, workflow.Deps{
		WorkerConcurrency: concurrency,
		DataSource:        rt.DataSource,
		Handle:            rt.Handle,
		NewModel: func(modelID string) (adkmodel.LLM, error) {
			if keyErr != nil {
				return nil, keyErr
			}
			return model.New(model.Config{Model: modelID, APIKey: apiKey})
		},
	}); err != nil {
		return fmt.Errorf("register workflows: %w", err)
	}

	// Launch starts the queue runner and recovers workflows this executor
	// left in flight. Everything has to be registered before it.
	if err := dbos.Launch(dbosCtx); err != nil {
		return fmt.Errorf("launch: %w", err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// newHandler serves the process's liveness surface.
//
// The GraphQL API SPEC.md §4 places at `generated/gql/` is not served here and
// is not hand-written here either. gopgql v0.2.1 generates a typed Go *client*
// (`generated/client/`) and a property graph, but no GraphQL server, and
// SPEC.md §21 forbids hand-written GraphQL resolvers — so the API surface
// waits on a gopgql server generator rather than on a resolver layer written
// in this repository. Nothing below issues SQL or resolves a field.
func newHandler() http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	}
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /readyz", ok)
	return mux
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envInt reads an optional integer environment variable. Absent or empty is
// zero, which every caller reads as "DBOS's own default".
func envInt(key string) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
