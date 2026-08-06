package determinism_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/gaarutyunov/agentiq/determinism"
)

// TestDiagnostics runs the analyzer over every fixture. The `want` comments in
// testdata carry the expectations; a package listed here with no `want`
// comment anywhere in it is asserting the analyzer stays silent.
func TestDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		pkg  string
		what string
	}{
		{"bad", "every forbidden operation, directly in the workflow body, is diagnosed"},
		{"escape", "the same operations behind dbos.RunAsStep / RunAsTransaction / Go / Select are not"},
		{"chained", "a violation two calls deep in the same package is diagnosed at its own line"},
		{"crosspkg", "a violation in another package of the module is diagnosed at the call site"},
		{"crossreg", "a workflow registered from another package is diagnosed at the registration"},
		{"funclit", "an inline workflow with explicit type arguments is walked"},
		{"unregistered", "non-deterministic code no workflow reaches is not a finding"},
	} {
		t.Run(tc.pkg, func(t *testing.T) {
			t.Log(tc.what)
			analysistest.Run(t, analysistest.TestData(), determinism.Analyzer, tc.pkg)
		})
	}
}

// TestAnalyzerShape guards the two properties golangci-lint depends on: the
// analyzer must export facts (cross-package reachability is carried on them)
// and it must expose the -module flag the plugin wrapper sets from
// .golangci.yml.
func TestAnalyzerShape(t *testing.T) {
	require.Equal(t, "determinism", determinism.Analyzer.Name)
	assert.NotEmpty(t, determinism.Analyzer.Doc)
	assert.Len(t, determinism.Analyzer.FactTypes, 1,
		"cross-package reachability travels on a fact; removing it silently narrows the analyzer to one package")

	flag := determinism.Analyzer.Flags.Lookup("module")
	require.NotNil(t, flag, "the plugin wrapper sets -module from linters.settings.custom.determinism.settings")
	assert.Equal(t, "", flag.DefValue, "an empty default means the module prefix is inferred")
}
