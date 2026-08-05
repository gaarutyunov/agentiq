// Package uuid is a stub of github.com/google/uuid for analysistest fixtures.
// It carries both a generator and a parser so the fixtures can show that the
// analyzer distinguishes them: Parse is deterministic, New is not.
package uuid

type UUID [16]byte

func New() UUID { return UUID{} }

func NewString() string { return "" }

func Parse(s string) (UUID, error) { return UUID{}, nil }

func (u UUID) String() string { return "" }
