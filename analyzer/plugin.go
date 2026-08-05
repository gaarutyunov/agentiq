// Package analyzer is the golangci-lint module-plugin entry point for the
// AgentIQ architecture analyzers (SPEC.md §17.1).
//
// # Why this is its own Go module
//
// `.custom-gcl.yml` declares the plugin as module `github.com/gaarutyunov/agentiq`
// at `path: ./analyzer`. golangci-lint turns that into
// `go mod edit -replace github.com/gaarutyunov/agentiq=<abs>/analyzer` and then
// imports the package at the module root — so `./analyzer` has to *be* a
// module root declaring that path, with an importable package in it. It cannot
// be an ordinary directory of the repository module: the repository root has
// no Go files, so the generated import would not resolve.
//
// The practical consequences, all deliberate:
//
//   - `go build ./...` and `go test ./...` at the repository root do not cover
//     this directory. Build and test it from inside `analyzer/`.
//   - Nothing in the repository module may import this module, and nothing
//     here may import the repository module. That is the same boundary
//     SPEC.md §17.1 states as "no runtime dependencies in this package": the
//     linter binary must not carry DBOS, pgx or the generated client.
package analyzer

import (
	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"

	"github.com/gaarutyunov/agentiq/determinism"
	"github.com/gaarutyunov/agentiq/rawsql"
)

func init() {
	register.Plugin("determinism", newDeterminism)
	register.Plugin("rawsql", newRawSQL)
}

// Settings are the plugin's `linters.settings.custom.determinism.settings`
// block in .golangci.yml.
type Settings struct {
	// Module bounds the call-graph walk to one module path prefix. Empty
	// means the analyzer infers it from the package under analysis, which is
	// correct for `github.com/owner/repo`-shaped paths.
	Module string `json:"module"`
}

func newDeterminism(raw any) (register.LinterPlugin, error) {
	s, err := register.DecodeSettings[Settings](raw)
	if err != nil {
		return nil, err
	}
	return &determinismPlugin{settings: s}, nil
}

type determinismPlugin struct {
	settings Settings
}

func (p *determinismPlugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	if p.settings.Module != "" {
		if err := determinism.Analyzer.Flags.Set("module", p.settings.Module); err != nil {
			return nil, err
		}
	}
	return []*analysis.Analyzer{determinism.Analyzer}, nil
}

// GetLoadMode is LoadModeTypesInfo because every rule here resolves a callee to
// a *types.Func: the syntax alone cannot tell `rand.Int` in `math/rand` from an
// identically named method on a local type, and the map-range rule needs the
// type of the ranged expression.
func (p *determinismPlugin) GetLoadMode() string {
	return register.LoadModeTypesInfo
}

func newRawSQL(any) (register.LinterPlugin, error) {
	return &rawSQLPlugin{}, nil
}

type rawSQLPlugin struct{}

func (p *rawSQLPlugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{rawsql.Analyzer}, nil
}

// GetLoadMode is LoadModeSyntax because the rule is about the text of a string
// literal; nothing here needs a type.
func (p *rawSQLPlugin) GetLoadMode() string {
	return register.LoadModeSyntax
}
