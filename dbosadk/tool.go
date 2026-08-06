package dbosadk

import (
	"context"
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// runnableTool is the half of an ADK tool that actually does something.
//
// ADK's `tool.Tool` is only `Name`/`Description`/`IsLongRunning`; the execution
// surface lives on an interface ADK declares unexported (`tool.runnableTool`),
// so it is restated here structurally. That is not a workaround: a tool
// implementation satisfies it or it does not, and a `tool.Tool` that does not
// is a declaration with no body — nothing to wrap in a step.
type runnableTool interface {
	Declaration() *genai.FunctionDeclaration
	ProcessRequest(ctx agent.Context, req *model.LLMRequest) error
	Run(ctx agent.Context, args any) (map[string]any, error)
}

// StepAsTool wraps t so each invocation of it is a durable step
// (SPEC.md §8.2, §9.2).
//
// It is defined in M2 and first exercised in M4, which is the milestone that
// gives the agent tools. Defining it now is not speculative: the whole reason
// `dbosadk` exists is that it is the only package where ADK and DBOS types
// meet, and a tool wrapper written later in some other package would be the
// second such place.
//
// A tool that carries no execution surface is returned unchanged. There is
// nothing to checkpoint, and wrapping it would produce a `tool.Tool` that no
// longer satisfies the interface ADK's Runner type-asserts for — which fails as
// "the model called a tool that cannot run", naming neither the tool nor this.
func StepAsTool(t tool.Tool, opts ...StepOption) tool.Tool {
	runnable, ok := t.(runnableTool)
	if !ok {
		return t
	}
	return &stepTool{Tool: t, runnable: runnable, opts: opts}
}

// stepTool embeds the tool for its declarative half and intercepts Run.
type stepTool struct {
	tool.Tool
	runnable runnableTool
	opts     []StepOption
}

func (t *stepTool) Declaration() *genai.FunctionDeclaration { return t.runnable.Declaration() }

// ProcessRequest is not a step. It shapes the request the model is about to
// receive — it is part of building the generation, and the generation is
// already checkpointed by [NewModel]. Checkpointing it separately would record
// a second output for one logical call.
func (t *stepTool) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	return t.runnable.ProcessRequest(ctx, req)
}

// Run executes the tool as a durable step.
//
// The step returns `map[string]any`, which is what ADK tools return and what
// DBOS serialises into the checkpoint. A tool whose result is not serialisable
// fails here rather than at replay, which is the earlier of the two.
func (t *stepTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	dctx, err := workflowContext(ctx)
	if err != nil {
		return nil, err
	}
	cfg := resolve("tool:"+t.Name(), t.opts)

	// The step body keeps the ADK context rather than the plain
	// context.Context DBOS hands it: an ADK tool reads the invocation, the
	// session and the event actions off it, and a bare context would strip all
	// three. Cancellation still reaches it — the ADK context wraps the same
	// workflow context DBOS derived its own from.
	out, err := dbos.RunAsStep(dctx, func(_ context.Context) (map[string]any, error) {
		return t.runnable.Run(ctx, args)
	}, cfg.stepOptions()...)
	if err != nil {
		return nil, fmt.Errorf("dbosadk: tool %q: %w", t.Name(), err)
	}
	return out, nil
}
