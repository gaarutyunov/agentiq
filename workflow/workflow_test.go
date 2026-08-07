package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AgentRun itself is not exercised here. It takes a dbos.Context, which is an
// interface, so testing it needs a generated mock of that interface plus the
// step-replay behaviour to assert against — that belongs with the durable
// execution suite in test/ (SPEC.md §14), against a real database, because
// what is worth asserting is that a killed worker does not re-execute step
// one. These cover the pieces around it that are ordinary functions.

func TestUserMessage(t *testing.T) {
	t.Parallel()

	t.Run("an absent message is a turn with no user content", func(t *testing.T) {
		t.Parallel()

		got, err := userMessage(nil)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("a message that names no role is the user's", func(t *testing.T) {
		t.Parallel()

		got, err := userMessage([]byte(`{"parts":[{"text":"hello"}]}`))
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "user", got.Role,
			"the provider rejects an empty role, and a workflow input can carry no other")
		require.Len(t, got.Parts, 1)
		assert.Equal(t, "hello", got.Parts[0].Text)
	})

	t.Run("a role the caller gave is kept", func(t *testing.T) {
		t.Parallel()

		got, err := userMessage([]byte(`{"role":"model","parts":[{"text":"hi"}]}`))
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "model", got.Role)
	})

	t.Run("a message that is not a genai.Content is an error", func(t *testing.T) {
		t.Parallel()

		_, err := userMessage([]byte(`["not", "an", "object"]`))
		require.Error(t, err)
	})
}

// Deps.model refuses rather than improvising, because the alternative would put
// an OPENROUTER_API_KEY read inside workflow code (SPEC.md §5).
func TestDepsModelWithoutAConstructor(t *testing.T) {
	t.Parallel()

	_, err := Deps{}.model("openai/gpt-4o-mini")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NewModel is nil")
}

func TestDepsAppName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, DefaultAppName, Deps{}.appName())
	assert.Equal(t, "other", Deps{AppName: "other"}.appName())
}

// The queue name is a contract with schema/dbos.graphql: it is the `queue:
// String = "agent"` default of Mutation.startAgentRun (SPEC.md §7.4). A run
// enqueued by the API onto a queue no worker listens to never starts, and
// nothing reports it — so the two are asserted equal rather than left to
// agree by habit.
func TestDefaultQueueNameMatchesTheSchemaDefault(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "agent", DefaultQueueName)
}
