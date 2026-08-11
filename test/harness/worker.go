package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// workerBinary builds ./cmd/agentiq once per test process.
//
// SPEC.md §20 M1 lists `cmd/agentiq/` as required "because F1 kills a child
// worker process; there is no other worker to kill". This is the code that
// kills it. An in-process worker would prove nothing: the guarantee under test
// is that state survives the death of the process holding it, and a goroutine
// cancelled politely is not a process that died.
var workerBinary struct {
	sync.Once
	path string
	err  error
}

// ModuleRoot is the repository root. It is exported because suites run with
// their own package directory as the working directory and still need to reach
// checked-in artifacts — `features/`, `generated/graph/`, `go.mod`.
func ModuleRoot() (string, error) { return moduleRoot() }

// moduleRoot is the repository root, derived from this file's own path so it
// does not depend on the working directory a test happens to run in.
func moduleRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("harness: cannot locate the harness source")
	}
	// .../test/harness/worker.go -> .../test/harness -> .../test -> ...
	return filepath.Dir(filepath.Dir(filepath.Dir(file))), nil
}

// BuildWorker compiles ./cmd/agentiq and returns the binary's path. Repeated
// calls in one test process return the same binary.
func BuildWorker() (string, error) {
	workerBinary.Do(func() {
		root, err := moduleRoot()
		if err != nil {
			workerBinary.err = err
			return
		}
		dir, err := os.MkdirTemp("", "agentiq-worker-")
		if err != nil {
			workerBinary.err = fmt.Errorf("harness: temp dir: %w", err)
			return
		}
		out := filepath.Join(dir, "agentiq")

		cmd := exec.Command("go", "build", "-o", out, "./cmd/agentiq")
		cmd.Dir = root
		if combined, err := cmd.CombinedOutput(); err != nil {
			workerBinary.err = fmt.Errorf("harness: build ./cmd/agentiq: %w\n%s", err, combined)
			return
		}
		workerBinary.path = out
	})
	return workerBinary.path, workerBinary.err
}

// Worker is a child `agentiq serve` process.
type Worker struct {
	// Addr is the address its HTTP surface listens on.
	Addr string

	cmd *exec.Cmd

	// done is closed once the process has been reaped, and reaping happens in
	// a goroutine started with the process rather than on demand.
	//
	// That is what makes a worker which dies during startup report as a worker
	// which died. `exec.Cmd.ProcessState` stays nil until something calls Wait,
	// so a readiness loop that polled it would see a live process for the whole
	// timeout: a `serve` that failed in the first second — a bad DATABASE_URL,
	// a migration that will not apply — used to surface 90 seconds later as
	// "worker did not become ready: timed out", which names the symptom of
	// every possible cause and the cause of none.
	done chan struct{}

	mu      sync.Mutex
	output  bytes.Buffer
	waitErr error
}

// StartWorker launches `agentiq serve` against dsn and blocks until it reports
// ready, or fails the test.
//
// extraEnv entries are `KEY=VALUE` and are appended after the harness's own, so
// a caller can override anything the harness sets.
func StartWorker(ctx context.Context, tb testingTB, dsn string, extraEnv ...string) *Worker {
	tb.Helper()

	bin, err := BuildWorker()
	if err != nil {
		tb.Fatalf("%v", err)
	}

	addr, err := freeAddr()
	if err != nil {
		tb.Fatalf("harness: pick a free port: %v", err)
	}

	w := &Worker{Addr: addr, done: make(chan struct{})}
	// The worker is not given the test's context: cancelling it must not be
	// what stops the process, because every scenario here is about how the
	// process dies. Cleanup kills it explicitly.
	cmd := exec.Command(bin, "serve", "--addr", addr) //nolint:gosec // bin is this package's own build output
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+dsn,
		// One workflow at a time. F1 and F5 both need to know which run the
		// worker was in the middle of when it was killed.
		"AGENTIQ_QUEUE_CONCURRENCY=1",
		// Pin the application version so the enqueuer and the worker agree.
		// See [AppVersion]: without it the run stays ENQUEUED and nothing says
		// why.
		AppVersionEnv+"="+AppVersion,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdout = &syncWriter{w: w}
	cmd.Stderr = &syncWriter{w: w}
	// A process group means a killed worker takes anything it spawned with it.
	cmd.SysProcAttr = sysProcAttr()

	if err := cmd.Start(); err != nil {
		tb.Fatalf("harness: start the worker: %v", err)
	}
	w.cmd = cmd
	go func() {
		err := cmd.Wait()
		w.mu.Lock()
		w.waitErr = err
		w.mu.Unlock()
		close(w.done)
	}()
	tb.Cleanup(func() { _ = w.Kill() })

	if err := w.waitReady(ctx, 90*time.Second); err != nil {
		tb.Fatalf("harness: worker did not become ready: %v\n--- worker output ---\n%s", err, w.Output())
	}
	return w
}

// waitReady polls the worker's readiness endpoint.
//
// It polls rather than reading a log line because the log format is not a
// contract and the endpoint is: cmd/agentiq serves /readyz only after
// dbos.Launch has returned, which is after the system-database migrations have
// run and the queue runner is up.
func (w *Worker) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := "http://" + w.Addr + "/readyz"
	client := &http.Client{Timeout: 2 * time.Second}

	for time.Now().Before(deadline) {
		if exited, err := w.exited(); exited {
			return fmt.Errorf("worker exited before becoming ready: %v", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return errors.New("timed out")
}

// Kill sends SIGKILL and reaps the process. This is failure-matrix row F1's
// mechanism, and it is SIGKILL rather than SIGTERM deliberately: SIGTERM lets
// cmd/agentiq drain, and a worker that drained is a worker that was not
// interrupted.
func (w *Worker) Kill() error {
	if w.cmd == nil || w.cmd.Process == nil {
		return nil
	}
	_ = w.cmd.Process.Kill()
	return w.wait()
}

// Stop asks the worker to shut down cleanly and waits for it.
func (w *Worker) Stop() error {
	if w.cmd == nil || w.cmd.Process == nil {
		return nil
	}
	if err := w.cmd.Process.Signal(os.Interrupt); err != nil {
		return w.Kill()
	}
	return w.wait()
}

// wait blocks until the reaper has collected the process, and returns what Wait
// returned.
//
// It never calls Wait itself: exactly one goroutine does, so a second caller
// cannot get the misleading "Wait was already called". The lock is taken only
// around the result, never around the wait, because stdout and stderr are
// written under the same lock and holding it for the life of the process would
// deadlock the child's first log line against its own reaper.
func (w *Worker) wait() error {
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.waitErr
}

// exited reports whether the process is already gone, without blocking.
func (w *Worker) exited() (bool, error) {
	select {
	case <-w.done:
		w.mu.Lock()
		defer w.mu.Unlock()
		return true, w.waitErr
	default:
		return false, nil
	}
}

// Output returns everything the worker has written to stdout and stderr. It is
// what a failing assertion should print: the reason a durable workflow did not
// resume is almost always in the worker's log, not in the database.
func (w *Worker) Output() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.output.String()
}

type syncWriter struct{ w *Worker }

func (s *syncWriter) Write(p []byte) (int, error) {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	return s.w.output.Write(p)
}

// freeAddr returns a loopback address nothing is listening on.
//
// There is an unavoidable race between closing the listener and the worker
// binding it. Binding :0 in the worker and reading the port back would close
// it, but cmd/agentiq prints no port and the readiness probe needs one, so the
// race is accepted and would surface as a loud "address already in use" rather
// than a silent misbehaviour.
func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := l.Addr().String()
	return addr, l.Close()
}
