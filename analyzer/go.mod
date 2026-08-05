// The module path is deliberately the same as the repository module's:
// .custom-gcl.yml (SPEC.md §17.1) declares the plugin as module
// `github.com/gaarutyunov/agentiq` at `path: ./analyzer`, and golangci-lint
// resolves that with a filesystem replace, which requires the go.mod at that
// path to declare exactly that module path. See analyzer/plugin.go.
module github.com/gaarutyunov/agentiq

go 1.25.0

require (
	github.com/golangci/plugin-module-register v0.1.2
	golang.org/x/tools v0.48.0
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/stretchr/testify v1.11.1
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
