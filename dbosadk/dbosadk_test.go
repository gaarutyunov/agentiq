package dbosadk

import (
	"context"
	"iter"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/v2/model"
)

// stubLLM is a model.LLM that yields a fixed sequence. It is a stub rather than
// a generated mock because the thing under test is what the wrapper does with
// the *sequence*, and a sequence is data — there is no call protocol to verify.
type stubLLM struct {
	name  string
	seq   []*model.LLMResponse
	calls int
}

func (s *stubLLM) Name() string { return s.name }

func (s *stubLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	s.calls++
	return func(yield func(*model.LLMResponse, error) bool) {
		for _, r := range s.seq {
			if !yield(r, nil) {
				return
			}
		}
	}
}

// TestStreamKeyIsNamespacedAndStable covers the contract between the writer
// here and M7's reader. A key the two sides derive differently is a
// subscription that silently returns nothing.
func TestStreamKeyIsNamespacedAndStable(t *testing.T) {
	assert.Equal(t, "agentiq/model/inv-1", StreamKey("inv-1"))
	assert.NotEqual(t, StreamKey("a"), StreamKey("b"))

	// Namespaced, because `dbos.streams` is shared with anything else the
	// application streams.
	assert.Contains(t, StreamKey("inv-1"), "agentiq/")
}

// TestDefaultsAreTheSpecdPolicy pins SPEC.md §9.2's numbers. They are not
// library defaults — DBOS's own base interval is 100ms and its default retry
// count is zero — so inheriting instead of setting them would be silently
// wrong.
func TestDefaultsAreTheSpecdPolicy(t *testing.T) {
	assert.Equal(t, 5, DefaultMaxRetries)
	assert.Equal(t, time.Second, DefaultBaseInterval)

	cfg := resolve("model:x", nil)
	assert.Equal(t, "model:x", cfg.name)
	assert.Equal(t, DefaultMaxRetries, cfg.maxRetries)
	assert.Equal(t, DefaultBaseInterval, cfg.baseInterval)
}

func TestOptionsOverrideTheDefaults(t *testing.T) {
	cfg := resolve("model:x", []StepOption{
		WithStepName("generate"),
		WithMaxRetries(2),
		WithBaseInterval(250 * time.Millisecond),
	})
	assert.Equal(t, "generate", cfg.name)
	assert.Equal(t, 2, cfg.maxRetries)
	assert.Equal(t, 250*time.Millisecond, cfg.baseInterval)
}

// TestGenerateOutsideAWorkflowFails is the assertion that keeps the wrapper
// honest. Falling back to the inner model when there is no workflow context
// would produce a system that works right up until the first replay, and then
// re-issues a generation that was already paid for and already recorded.
func TestGenerateOutsideAWorkflowFails(t *testing.T) {
	inner := &stubLLM{name: "openai/gpt-5", seq: []*model.LLMResponse{{TurnComplete: true}}}
	wrapped := NewModel(inner)

	var gotErr error
	for _, err := range wrapped.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		gotErr = err
	}

	require.ErrorIs(t, gotErr, ErrNotInWorkflow)
	assert.Contains(t, gotErr.Error(), "dbos.Context")
	assert.Zero(t, inner.calls, "the inner model must not be reached outside a workflow")
}

// TestNameIsPassedThrough matters because the name ends up in
// `LLMRequest.Model` and therefore in the provider's API call. A wrapper that
// renamed the model would request a model that does not exist.
func TestNameIsPassedThrough(t *testing.T) {
	wrapped := NewModel(&stubLLM{name: "openai/gpt-5"})
	assert.Equal(t, "openai/gpt-5", wrapped.Name())
}

func TestCloseStreamOutsideAWorkflowFails(t *testing.T) {
	err := CloseStream(context.Background(), "inv-1")
	require.ErrorIs(t, err, ErrNotInWorkflow)
}

// invocationCtx is an ADK-shaped context: a context.Context that also carries
// an invocation ID, which is how the Runner passes one to a model.
type invocationCtx struct {
	context.Context
	id string
}

func (c invocationCtx) InvocationID() string { return c.id }

func TestInvocationIDIsRecoveredStructurally(t *testing.T) {
	// Declared structurally so the wrapper does not depend on which of ADK's
	// several context wrappers the Runner happened to pass.
	assert.Equal(t, "inv-9", invocationID(invocationCtx{Context: context.Background(), id: "inv-9"}))

	// A context with no invocation is not an error: it is an unaddressable
	// stream, and the generation is still a step.
	assert.Empty(t, invocationID(context.Background()))
}

// --- StepAsTool -------------------------------------------------------------

// declarativeTool has ADK's `tool.Tool` surface and nothing else: a declaration
// with no body.
type declarativeTool struct{ name string }

func (t *declarativeTool) Name() string        { return t.name }
func (t *declarativeTool) Description() string { return "" }
func (t *declarativeTool) IsLongRunning() bool { return false }

// TestStepAsToolLeavesANonRunnableToolAlone. Wrapping one would produce a
// tool.Tool that no longer satisfies the interface ADK's Runner type-asserts
// for, which fails as "the model called a tool that cannot run" — naming
// neither the tool nor the wrapper.
func TestStepAsToolLeavesANonRunnableToolAlone(t *testing.T) {
	original := &declarativeTool{name: "noop"}
	assert.Same(t, original, StepAsTool(original))
}
