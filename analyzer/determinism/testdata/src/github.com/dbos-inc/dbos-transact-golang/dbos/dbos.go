// Package dbos is a stub of the DBOS Go SDK for analysistest fixtures.
//
// analysistest runs in GOPATH mode, so fixtures resolve their imports from
// testdata/src rather than from the module graph. Only the signatures the
// analyzer keys on are reproduced, and they match
// github.com/dbos-inc/dbos-transact-golang/dbos v1.0.0 — the analyzer's
// recognition of a step boundary depends on which argument is the function
// value, so a stub with a different argument order would test nothing.
package dbos

import (
	"context"
	"time"
)

type Context interface {
	context.Context
}

type (
	Workflow[P any, R any]     func(ctx Context, in P) (R, error)
	Step[R any]                func(ctx context.Context) (R, error)
	Txn[R any]                 func(ctx context.Context, tx any) (R, error)
	StepOutcome[R any]         struct{ Result R }
	WorkflowOption             func()
	StepOption                 func()
	WorkflowRegistrationOption func()
	DataSource                 struct{}
)

func RegisterWorkflow[P any, R any](ctx Context, fn Workflow[P, R], opts ...WorkflowRegistrationOption) {
}

func RunAsStep[R any](ctx Context, fn Step[R], opts ...StepOption) (R, error) {
	var zero R
	return zero, nil
}

func RunAsTransaction[R any](ctx Context, ds *DataSource, fn Txn[R], opts ...StepOption) (R, error) {
	var zero R
	return zero, nil
}

func Go[R any](ctx Context, fn Step[R], opts ...StepOption) (<-chan StepOutcome[R], error) {
	return nil, nil
}

func Select[R any](ctx Context, channels []<-chan StepOutcome[R]) (R, error) {
	var zero R
	return zero, nil
}

func Sleep(ctx Context, d time.Duration) (time.Duration, error) { return 0, nil }

func GetWorkflowID(ctx Context) (string, error) { return "", nil }

func WithStepName(name string) StepOption { return func() {} }

func NewDataSource() *DataSource { return &DataSource{} }
