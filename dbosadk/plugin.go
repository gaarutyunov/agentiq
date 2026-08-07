package dbosadk

import (
	"errors"
	"fmt"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/tool"
)

// DefaultPluginName is the name the plugin registers under. ADK requires a
// non-empty one and reports it in its own errors, so it names the concern
// rather than the package.
const DefaultPluginName = "agentiq-durability"

// Config configures [NewPlugin].
type Config struct {
	// Name overrides [DefaultPluginName].
	Name string

	// CloseStreamAtTurnEnd closes the invocation's partial-response stream when
	// the run finishes (SPEC.md §9.4). It defaults to on, because a stream that
	// is never closed leaves M7's reader waiting for values that will not come.
	//
	// Turning it off is for a workflow that calls [CloseStream] itself and
	// wants the error rather than the best-effort close described below.
	CloseStreamAtTurnEnd *bool
}

// ErrModelNotWrapped is returned when the Runner is about to call a model that
// is not wrapped by [NewModel].
var ErrModelNotWrapped = errors.New("dbosadk: the model is not wrapped by dbosadk.NewModel")

// ErrToolNotWrapped is returned when the Runner is about to call a tool that is
// not wrapped by [StepAsTool].
var ErrToolNotWrapped = errors.New("dbosadk: the tool is not wrapped by dbosadk.StepAsTool")

// NewPlugin installs the ADK-side hooks that keep a Runner loop running as
// workflow code (SPEC.md §8.2).
//
// SPEC.md §8.2 gives the plugin two jobs, and they are two halves of one thing:
// route every model and tool invocation through the step wrappers, and reject
// direct use of non-deterministic ADK facilities inside workflow code.
//
// # Why this is a plugin and not a lint rule
//
// The determinism analyzer (SPEC.md §17.1) walks the transitive call graph from
// `RegisterWorkflow`, and it stops at the module boundary — `walker.inModule`
// follows a call only into a package under AgentIQ's own import path. So it
// covers `workflow`, `session` and this package, and it does **not** see inside
// ADK. That is a deliberate limit rather than an oversight: a walk into every
// dependency would report on code nobody here can change, and the module's own
// code is where a violation can actually be fixed.
//
// Which leaves two things outside its reach, and they are this plugin's:
//
//   - What ADK itself does between the step boundaries. The analyzer cannot
//     read it, so the wrappers have to be the seam — everything ADK does that
//     matters for replay is a model call or a tool call, and both go through
//     `dbosadk`.
//   - An agent *assembled* with an unwrapped model. Which `model.LLM` an
//     `llmagent` holds is a value, not a call, so no static walk from the
//     workflow function could see it even within the module. The failure that
//     produces is the expensive kind: a generation re-executed and re-billed on
//     every replay, in a system that looks entirely healthy until its first
//     recovery.
//
// So the analyzer covers the module's code and the plugin covers the wiring and
// the boundary. Together they are §9.1's determinism contract; neither is
// sufficient alone.
//
// # It fails the run rather than repairing it
//
// A `BeforeModelCallback` may return an `*model.LLMResponse` to short-circuit
// the call, and this one could have wrapped the model on the fly instead of
// refusing. It does not: by the time the Runner is calling the model, the agent
// is already built, and silently substituting a different model would make the
// agent that ran differ from the agent that was configured — which is exactly
// what D24's digest-pinned closure exists to prevent.
func NewPlugin(cfg Config) (*plugin.Plugin, error) {
	name := cfg.Name
	if name == "" {
		name = DefaultPluginName
	}

	closeAtTurnEnd := true
	if cfg.CloseStreamAtTurnEnd != nil {
		closeAtTurnEnd = *cfg.CloseStreamAtTurnEnd
	}

	pluginCfg := plugin.Config{
		Name: name,

		// Both callbacks return (nil, err) on rejection: a nil response with a
		// non-nil error is ADK's "this call fails", where a non-nil response
		// would be "this call is answered without the model".
		BeforeModelCallback: func(ctx agent.Context, _ *model.LLMRequest) (*model.LLMResponse, error) {
			if _, err := workflowContext(ctx); err != nil {
				return nil, err
			}
			return nil, nil
		},

		BeforeToolCallback: func(ctx agent.Context, t tool.Tool, _ map[string]any) (map[string]any, error) {
			if _, err := workflowContext(ctx); err != nil {
				return nil, err
			}
			if _, ok := t.(*stepTool); !ok {
				// A tool with no execution surface is legitimately unwrapped —
				// StepAsTool returns those unchanged because there is nothing to
				// checkpoint — so it is not an error to see one here.
				if _, runnable := t.(runnableTool); runnable {
					return nil, fmt.Errorf("%w: %q would run outside a step and be re-executed on replay",
						ErrToolNotWrapped, t.Name())
				}
			}
			return nil, nil
		},
	}

	if closeAtTurnEnd {
		pluginCfg.AfterRunCallback = func(ctx agent.InvocationContext) {
			// Best effort, and it has to be: ADK's AfterRunCallback returns
			// nothing, so there is nowhere to report a failure to. That is
			// acceptable here and would not be for the write path — the stream
			// is the read-side convenience M7 subscribes to, while the event
			// rows and the checkpoint are the truth, and they were committed by
			// the transaction long before this runs.
			//
			// A workflow that needs the error calls CloseStream itself and sets
			// Config.CloseStreamAtTurnEnd to false.
			_ = CloseStream(ctx, ctx.InvocationID())
		}
	}

	p, err := plugin.New(pluginCfg)
	if err != nil {
		return nil, fmt.Errorf("dbosadk: build the %q plugin: %w", name, err)
	}
	return p, nil
}

// ModelIsWrapped reports whether llm is a model [NewModel] produced.
//
// It exists because the check is worth making at assembly time, where the fix
// is one line in the code that built the agent, rather than at the first
// generation, where it is a failed run. The plugin makes the same check late;
// this one lets a caller make it early.
func ModelIsWrapped(llm model.LLM) bool {
	_, ok := llm.(*stepModel)
	return ok
}

// assertPluginCallbackTypes pins the ADK callback signatures this file is
// written against. ADK is a v2 module under active development, and a callback
// whose signature changed would otherwise fail as an unassignable field deep in
// a composite literal rather than here.
var (
	_ llmagent.BeforeModelCallback = func(agent.Context, *model.LLMRequest) (*model.LLMResponse, error) { return nil, nil }
	_ llmagent.BeforeToolCallback  = func(agent.Context, tool.Tool, map[string]any) (map[string]any, error) { return nil, nil }
	_ plugin.AfterRunCallback      = func(agent.InvocationContext) {}
	_                              = dbos.CloseStream
)
