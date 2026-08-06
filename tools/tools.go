//go:build tools

// Package tools pins the versions of the code generators, so that the version
// `go generate ./...` runs is the version recorded in go.mod rather than
// whatever happens to be on the developer's PATH (SPEC.md §11, rule 4).
//
// The build tag keeps the generator out of every real build; the blank import
// is what makes `go mod tidy` keep the requirement.
package tools

import (
	_ "github.com/gaarutyunov/gopgql/cmd/gopgql"
)
