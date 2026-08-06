package harness

import (
	"sync"

	"github.com/cucumber/godog"
)

// ScenarioTB adapts godog's per-scenario [godog.TestingT] to the small
// testing.TB surface the fixtures take.
//
// godog.T(ctx) hands a step a TestingT that fails the right subtest, but it has
// no Helper and no Cleanup. Passing the suite-level *testing.T to a fixture
// instead would work for construction and be wrong for teardown: its Cleanup
// runs when the whole suite ends, so a worker started in scenario one would
// still be holding the database when scenario two restores it.
//
// [ScenarioTB.Close] drains the stack in reverse, and the suite calls it from
// godog's After hook.
type ScenarioTB struct {
	T godog.TestingT

	mu      sync.Mutex
	cleanup []func()
}

// NewScenarioTB wraps a godog TestingT.
func NewScenarioTB(t godog.TestingT) *ScenarioTB { return &ScenarioTB{T: t} }

// Helper is a no-op: godog attributes a failure to the step, not to a line.
func (s *ScenarioTB) Helper() {}

// Cleanup pushes fn onto the scenario's teardown stack.
func (s *ScenarioTB) Cleanup(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanup = append(s.cleanup, fn)
}

// Logf writes to the scenario's log.
func (s *ScenarioTB) Logf(format string, args ...any) { s.T.Logf(format, args...) }

// Fatalf fails the scenario and halts the step.
func (s *ScenarioTB) Fatalf(format string, args ...any) { s.T.Fatalf(format, args...) }

// Close runs the teardown stack in reverse and empties it.
func (s *ScenarioTB) Close() {
	s.mu.Lock()
	fns := s.cleanup
	s.cleanup = nil
	s.mu.Unlock()

	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}
