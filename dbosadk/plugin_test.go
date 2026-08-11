package dbosadk

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// The two context mocks these tests need are generated, not hand-rolled:
//
//	go tool mockgen -destination=dbosadk/mocks_test.go -package=dbosadk \
//	  -mock_names Context=MockAgentContext,InvocationContext=MockInvocationContext \
//	  google.golang.org/adk/v2/agent Context,InvocationContext
//	go tool mockgen -destination=dbosadk/mocks_dbos_test.go -package=dbosadk \
//	  -mock_names Context=MockDBOSContext \
//	  github.com/dbos-inc/dbos-transact-golang/dbos Context
//
// `agent.Context` has around twenty methods across three embedded interfaces
// and `dbos.Context` more. A hand-written double for either would keep
// compiling after ADK or DBOS added a method and would then be asserting
// against an interface that no longer exists.

// workflowContextDouble is what workflow code actually hands the Runner: one
// value that is both a `dbos.Context` and an `agent.Context`. That is not a
// test convenience — it is the mechanism the package doc describes, and it is
// why `workflowContext` can recover the DBOS context by type assertion from an
// interface that knows nothing about DBOS.
//
// An embedded interface promotes its whole method set at depth one, so both
// halves offer `context.Context`'s four methods at the same depth and Go calls
// the selector ambiguous. They are resolved explicitly onto the DBOS half,
// which is the half carrying the workflow's real deadline and cancellation.
type workflowContextDouble struct {
	agent.Context
	*MockDBOSContext
}

func (c workflowContextDouble) Deadline() (time.Time, bool) { return c.MockDBOSContext.Deadline() }
func (c workflowContextDouble) Done() <-chan struct{}       { return c.MockDBOSContext.Done() }
func (c workflowContextDouble) Err() error                  { return c.MockDBOSContext.Err() }
func (c workflowContextDouble) Value(key any) any           { return c.MockDBOSContext.Value(key) }

// hostContext is an agent.Context that is *not* a dbos.Context — the shape a
// Runner has when it was started outside workflow code.
func hostContext(t *testing.T) agent.Context {
	t.Helper()
	ctx := NewMockAgentContext(gomock.NewController(t))
	ctx.EXPECT().InvocationID().Return("inv-host").AnyTimes()
	// A host context carries no workflow, which is what this double is for.
	// The lookup is asked for because a type assertion does not survive the
	// Runner's own context wrappers — see dbosadk.WithWorkflowContext.
	ctx.EXPECT().Value(gomock.Any()).Return(nil).AnyTimes()
	return ctx
}

func workflowContextFor(t *testing.T) agent.Context {
	t.Helper()
	ctrl := gomock.NewController(t)

	d := NewMockDBOSContext(ctrl)
	d.EXPECT().Deadline().Return(time.Time{}, false).AnyTimes()
	d.EXPECT().Done().Return(nil).AnyTimes()
	d.EXPECT().Err().Return(nil).AnyTimes()
	d.EXPECT().Value(gomock.Any()).Return(nil).AnyTimes()

	a := NewMockAgentContext(ctrl)
	a.EXPECT().InvocationID().Return("inv-wf").AnyTimes()

	return workflowContextDouble{Context: a, MockDBOSContext: d}
}

// runnableStubTool has the execution surface StepAsTool looks for, and is
// therefore a tool that must be wrapped.
type runnableStubTool struct{ declarativeTool }

func (t *runnableStubTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: t.name}
}

func (t *runnableStubTool) ProcessRequest(agent.Context, *model.LLMRequest) error { return nil }

func (t *runnableStubTool) Run(agent.Context, any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

func TestNewPluginNamesItself(t *testing.T) {
	p, err := NewPlugin(Config{})
	require.NoError(t, err)
	assert.Equal(t, DefaultPluginName, p.Name())

	named, err := NewPlugin(Config{Name: "custom"})
	require.NoError(t, err)
	assert.Equal(t, "custom", named.Name())
}

// TestThePluginRefusesAModelCallOutsideWorkflowCode is half of SPEC.md §8.2's
// "rejects direct use of non-deterministic ADK facilities inside workflow
// code": a Runner whose context is not the workflow's cannot checkpoint
// anything, so its generations would be re-executed on every replay.
func TestThePluginRefusesAModelCallOutsideWorkflowCode(t *testing.T) {
	p, err := NewPlugin(Config{})
	require.NoError(t, err)

	resp, err := p.BeforeModelCallback()(hostContext(t), &model.LLMRequest{})
	require.ErrorIs(t, err, ErrNotInWorkflow)
	assert.Nil(t, resp, "a non-nil response would answer the call instead of failing it")
}

func TestThePluginRefusesAToolCallOutsideWorkflowCode(t *testing.T) {
	p, err := NewPlugin(Config{})
	require.NoError(t, err)

	out, err := p.BeforeToolCallback()(hostContext(t), &runnableStubTool{}, nil)
	require.ErrorIs(t, err, ErrNotInWorkflow)
	assert.Nil(t, out)
}

// TestThePluginRefusesAnUnwrappedTool is the other half: the context *is* the
// workflow's, but the agent was assembled with a tool that never went through
// StepAsTool.
//
// This is the case the determinism analyzer cannot reach. Which tool an agent
// holds is a value, not a call, so no static walk from `RegisterWorkflow` can
// see it — the plugin is the only thing that can.
func TestThePluginRefusesAnUnwrappedTool(t *testing.T) {
	p, err := NewPlugin(Config{})
	require.NoError(t, err)

	out, err := p.BeforeToolCallback()(workflowContextFor(t), &runnableStubTool{}, nil)
	require.ErrorIs(t, err, ErrToolNotWrapped)
	assert.Contains(t, err.Error(), "re-executed on replay")
	assert.Nil(t, out)
}

// TestThePluginAcceptsAWrappedTool is the positive case, and it is what stops
// the two tests above from passing on a plugin that rejected everything.
func TestThePluginAcceptsAWrappedTool(t *testing.T) {
	p, err := NewPlugin(Config{})
	require.NoError(t, err)

	wrapped := StepAsTool(&runnableStubTool{declarativeTool{name: "search"}})
	out, err := p.BeforeToolCallback()(workflowContextFor(t), wrapped, nil)
	require.NoError(t, err)
	assert.Nil(t, out, "a nil result lets the Runner run the tool")
}

// TestThePluginLeavesADeclarationOnlyToolAlone pins the exemption StepAsTool
// relies on: a tool with no execution surface has nothing to checkpoint, so
// StepAsTool returns it unchanged — and the plugin must not then reject it for
// being unchanged.
func TestThePluginLeavesADeclarationOnlyToolAlone(t *testing.T) {
	p, err := NewPlugin(Config{})
	require.NoError(t, err)

	declarative := &declarativeTool{name: "noop"}
	require.Same(t, declarative, StepAsTool(declarative), "precondition: StepAsTool passes it through")

	out, err := p.BeforeToolCallback()(workflowContextFor(t), declarative, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestModelIsWrapped(t *testing.T) {
	inner := &stubLLM{name: "openai/gpt-5"}
	assert.False(t, ModelIsWrapped(inner))
	assert.True(t, ModelIsWrapped(NewModel(inner)))
}
