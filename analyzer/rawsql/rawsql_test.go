package rawsql_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/gaarutyunov/agentiq/rawsql"
)

func TestRawSQL(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), rawsql.Analyzer, "sqlish")
}

func TestAnalyzerShape(t *testing.T) {
	require.Equal(t, "rawsql", rawsql.Analyzer.Name)
	assert.NotEmpty(t, rawsql.Analyzer.Doc)
	assert.Empty(t, rawsql.Analyzer.FactTypes,
		"the rule is per-literal; nothing needs to cross a package boundary")
}
