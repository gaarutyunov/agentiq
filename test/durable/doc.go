// Package durable runs features/durable_execution.feature against a real
// PostgreSQL 19 container and a real child worker process (SPEC.md §14, §19).
//
// Everything in it is behind `//go:build integration`, so `go test ./...
// -short` compiles the package and runs nothing (SPEC.md §16). This file
// carries no build tag so that the package always has a Go file: a directory
// whose every file is excluded by a tag makes `go vet ./...` fail with "build
// constraints exclude all Go files", which is a confusing way to learn that
// the suite is tag-gated.
package durable
