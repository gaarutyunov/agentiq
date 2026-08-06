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
// one. These cover the step bodies, which are ordinary functions.

func TestResolveAgent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		digest  string
		want    string
		wantErr bool
	}{
		{
			name:   "returns the digest it was given",
			digest: "sha256:b5bb9d8014a0f9b1d61e21e796d78dccdf1352f23cd32812f4850b878ae4944c",
			want:   "sha256:b5bb9d8014a0f9b1d61e21e796d78dccdf1352f23cd32812f4850b878ae4944c",
		},
		{
			name:    "an empty digest pins nothing and is an error",
			digest:  "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveAgent(tt.digest)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCompleteRun(t *testing.T) {
	t.Parallel()

	t.Run("reports the terminal status", func(t *testing.T) {
		t.Parallel()

		got, err := completeRun("sha256:abc", "session-1")
		require.NoError(t, err)
		assert.Equal(t, "SUCCESS", got)
	})

	t.Run("an empty session id is an error", func(t *testing.T) {
		t.Parallel()

		_, err := completeRun("sha256:abc", "")
		require.Error(t, err)
	})
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
