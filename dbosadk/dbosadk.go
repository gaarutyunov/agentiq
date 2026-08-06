// Package dbosadk is the one place ADK and DBOS types meet (SPEC.md §5, §8.2).
//
// The shape follows Temporal's `contrib/googleadk` and SPEC.md §8.2/D1: the ADK
// Runner loop *is* the workflow, each model generation is a durable step, and
// each tool invocation is a durable step. Everything the loop does that is not
// deterministic has to come through here, because the determinism analyzer
// (SPEC.md §17.1) walks the transitive call graph from `RegisterWorkflow` and
// an ADK internal reaching `time.Now` is a diagnostic, not a runtime surprise.
//
// It may import ADK and DBOS. It must not import `generated/client` or
// `artifact` (SPEC.md §5), and depguard enforces that: what keeps this package
// reusable is that it knows nothing about AgentIQ's schema.
//
// # How the DBOS context reaches an ADK interface
//
// ADK's `model.LLM` and tool interfaces take a plain `context.Context`, and
// `dbos.RunAsStep` needs a `dbos.Context`. There is no seam to thread a second
// argument through — the Runner calls the model, not us.
//
// `dbos.Context` embeds `context.Context` (dbos/dbos.go: `type Client interface
// { context.Context; ... }`), so workflow code passes the DBOS context itself
// as the ADK context and the wrappers recover it with a type assertion. When
// the assertion fails the call is not inside workflow code, and that is
// reported as an error rather than silently degrading to a direct model call:
// a generation that ran outside a step is a generation that will be re-executed
// on replay, and it would look exactly like a working system until the first
// recovery.
package dbosadk

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"google.golang.org/adk/v2/model"
)

// Defaults from SPEC.md §9.2's step taxonomy. They are spec'd numbers, not
// library defaults to inherit: DBOS's own base interval is 100ms and its
// default retry count is zero.
const (
	// DefaultMaxRetries is §9.2's model-call policy. The spec says "5 retries"
	// in SPEC.md §20 M2 and "5 attempts" in its constraint list; this is the
	// former, which is the one that names the mechanism, and it maps onto
	// [dbos.WithStepMaxRetries] unchanged.
	DefaultMaxRetries = 5

	// DefaultBaseInterval is §9.2's base delay. DBOS multiplies it by its
	// backoff factor per retry, which defaults to 2 — the "exponential" §9.2
	// asks for.
	DefaultBaseInterval = time.Second
)

// ErrNotInWorkflow is returned when a wrapped model or tool is invoked with a
// context that is not a DBOS workflow context.
//
// It is deliberately fatal to the call. The alternative — falling back to the
// inner model — produces a system that works until the first replay and then
// re-issues a generation that was already paid for and already recorded.
var ErrNotInWorkflow = errors.New("dbosadk: not called from DBOS workflow code")

// stepConfig is the resolved policy for one wrapped call.
type stepConfig struct {
	name         string
	maxRetries   int
	baseInterval time.Duration
}

// StepOption configures how a wrapped model or tool call is checkpointed.
type StepOption func(*stepConfig)

// WithStepName overrides the step name DBOS records in
// `dbos.operation_outputs.function_name`.
//
// The name is what a durability assertion is written against: "step one is not
// re-executed" is a statement about a row in that table. Defaulting it to
// something derived from the wrapped model's own name keeps the checkpoint
// legible without the caller having to say so.
func WithStepName(name string) StepOption {
	return func(c *stepConfig) { c.name = name }
}

// WithMaxRetries overrides [DefaultMaxRetries].
func WithMaxRetries(n int) StepOption {
	return func(c *stepConfig) { c.maxRetries = n }
}

// WithBaseInterval overrides [DefaultBaseInterval].
func WithBaseInterval(d time.Duration) StepOption {
	return func(c *stepConfig) { c.baseInterval = d }
}

// resolve applies opts over the §9.2 defaults.
func resolve(defaultName string, opts []StepOption) stepConfig {
	c := stepConfig{
		name:         defaultName,
		maxRetries:   DefaultMaxRetries,
		baseInterval: DefaultBaseInterval,
	}
	for _, opt := range opts {
		opt(&c)
	}
	return c
}

// stepOptions renders the resolved policy as DBOS options.
func (c stepConfig) stepOptions() []dbos.StepOption {
	return []dbos.StepOption{
		dbos.WithStepName(c.name),
		dbos.WithStepMaxRetries(c.maxRetries),
		dbos.WithStepBaseInterval(c.baseInterval),
	}
}

// streamKeyPrefix namespaces AgentIQ's model streams inside `dbos.streams`,
// which is a table DBOS also uses for anything else the application streams.
const streamKeyPrefix = "agentiq/model/"

// StreamKey is the `dbos.streams` key partial model responses for one
// invocation are written to (SPEC.md §8.2, §9.4).
//
// It is a function rather than a convention written twice because both sides
// need it and they are in different packages: `dbosadk` writes, and M7's
// subscription reads. A key that agreed by coincidence would be a subscription
// that silently returns nothing.
func StreamKey(invocationID string) string { return streamKeyPrefix + invocationID }

// invocationIDer is how an invocation ID is recovered from an ADK context.
//
// ADK's `agent.InvocationContext` carries `InvocationID() string` and is itself
// a `context.Context`, so the Runner hands it to the model as the ctx. The
// interface is declared structurally rather than importing the ADK one so this
// package does not depend on which of ADK's several context wrappers the
// Runner happened to pass — they all carry the method.
type invocationIDer interface {
	InvocationID() string
}

// invocationID recovers the ADK invocation ID from ctx, if it carries one.
func invocationID(ctx context.Context) string {
	if inv, ok := ctx.(invocationIDer); ok {
		return inv.InvocationID()
	}
	return ""
}

// workflowContext recovers the DBOS context from an ADK context.
func workflowContext(ctx context.Context) (dbos.Context, error) {
	dctx, ok := ctx.(dbos.Context)
	if !ok {
		return nil, fmt.Errorf("%w: got %T, which is not a dbos.Context; "+
			"the ADK Runner loop must be invoked with the workflow's own context (SPEC.md §8.2, D1)",
			ErrNotInWorkflow, ctx)
	}
	return dctx, nil
}

// NewModel wraps inner so every generation is a durable step (SPEC.md §8.2).
//
// # What crosses the checkpoint, and what does not
//
// A generation yields a sequence: zero or more partial responses and then one
// final response. Only the final one is returned from the step and therefore
// only it is checkpointed (§9.4). The partials go to a DBOS stream keyed by
// [StreamKey], because a checkpoint is replayed — a partial recorded in
// `dbos.operation_outputs` would be re-delivered on every recovery, and the
// §15 partial-skip scenario asserts the opposite: five values in the stream,
// exactly one event in the session.
//
// The stream is written but never closed here. Closing it is a turn-level act,
// not a generation-level one — a turn may generate more than once — so
// [CloseStream] is called by the workflow at turn end (§9.4).
//
// # Streaming is requested regardless of the caller's flag
//
// The `stream` argument is passed through untouched. A caller that asks for
// non-streaming generation gets a sequence of exactly one final response and
// therefore an empty DBOS stream, which is correct: there were no partials to
// record.
func NewModel(inner model.LLM, opts ...StepOption) model.LLM {
	return &stepModel{inner: inner, opts: opts}
}

type stepModel struct {
	inner model.LLM
	opts  []StepOption
}

// Name reports the wrapped model's name unchanged. The wrapper is a durability
// concern; it is not a different model, and a name that said so would end up in
// `LLMRequest.Model` and in the provider's API call.
func (m *stepModel) Name() string { return m.inner.Name() }

// GenerateContent runs one generation as a durable step and yields its final
// response.
func (m *stepModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	dctx, err := workflowContext(ctx)
	if err != nil {
		return errorSeq(err)
	}

	cfg := resolve("model:"+m.inner.Name(), m.opts)
	key := StreamKey(invocationID(ctx))
	// An invocation with no ID has an unaddressable stream: nothing could
	// subscribe to it, so writing partials there would cost round trips to
	// produce rows no reader can name. The generation is still a step.
	writePartials := invocationID(ctx) != ""

	final, err := dbos.RunAsStep(dctx, func(stepCtx context.Context) (*model.LLMResponse, error) {
		var last *model.LLMResponse
		for resp, err := range m.inner.GenerateContent(stepCtx, req, stream) {
			if err != nil {
				return nil, err
			}
			if resp == nil {
				continue
			}
			if resp.Partial {
				if writePartials {
					if err := dbos.WriteStream(dctx, key, resp); err != nil {
						return nil, fmt.Errorf("dbosadk: write a partial response to stream %q: %w", key, err)
					}
				}
				continue
			}
			last = resp
		}
		if last == nil {
			// Every partial arrived and no final one did. Returning the last
			// partial instead would put a partial through the checkpoint, which
			// is the one thing §9.4 forbids.
			return nil, errors.New("dbosadk: the model produced no final response")
		}
		return last, nil
	}, cfg.stepOptions()...)
	if err != nil {
		return errorSeq(err)
	}
	return singleSeq(final)
}

// CloseStream closes the partial-response stream for one invocation
// (SPEC.md §9.4).
//
// It is the workflow's call, at turn end, not the model's: a turn can generate
// more than once, and a stream closed by the first generation would drop the
// partials of the second.
func CloseStream(ctx context.Context, invocationID string) error {
	dctx, err := workflowContext(ctx)
	if err != nil {
		return err
	}
	if invocationID == "" {
		return nil
	}
	if err := dbos.CloseStream(dctx, StreamKey(invocationID)); err != nil {
		return fmt.Errorf("dbosadk: close stream for invocation %q: %w", invocationID, err)
	}
	return nil
}

// errorSeq is a one-element sequence carrying err, which is how a
// `model.LLM` reports a failure it detected before generating anything.
func errorSeq(err error) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(nil, err)
	}
}

// singleSeq is a one-element sequence carrying resp.
func singleSeq(resp *model.LLMResponse) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(resp, nil)
	}
}
