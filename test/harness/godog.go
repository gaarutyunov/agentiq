package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cucumber/godog"
)

// FeaturesDir is the canonical scenario source (SPEC.md §14.3). Every suite
// points at this one directory: a suite with its own copy of a feature file is
// a second source of truth, which is the thing §14.3 forbids.
func FeaturesDir() (string, error) {
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "features")
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("harness: %s: %w", dir, err)
	}
	return dir, nil
}

// GodogOptions builds the options every AgentIQ suite runs under.
//
// Two things here are required rather than stylistic:
//
//   - TestingT makes each scenario a Go subtest, so a failure is attributed to
//     the scenario by name and `go test -run` can select one (SPEC.md §14.3).
//   - JUnit output is what CI reads. It is written next to the suite under
//     reports/, which is gitignored; junitName distinguishes the suites, since
//     two files with the same name overwrite each other in the artifact.
//
// tags selects scenarios by their feature-level tag — "@integration" or
// "@browser" — so the two suites read the same directory and take disjoint
// halves of it.
func GodogOptions(t *testing.T, tags, junitName string) (godog.Options, error) {
	t.Helper()

	features, err := FeaturesDir()
	if err != nil {
		return godog.Options{}, err
	}

	reports := filepath.Join("reports")
	if err := os.MkdirAll(reports, 0o750); err != nil {
		return godog.Options{}, fmt.Errorf("harness: create %s: %w", reports, err)
	}
	junit := filepath.Join(reports, junitName)

	return godog.Options{
		Format:   "pretty,junit:" + junit,
		Paths:    []string{features},
		Tags:     tags,
		TestingT: t,
		// Strict fails the suite on an undefined or pending step. Without it
		// a scenario nobody implemented reports as a skip, and a skipped
		// acceptance criterion reads exactly like a passing one in CI.
		Strict: true,
		// Scenarios share one database and one worker slot; running them
		// concurrently would make each one's kill the other one's flake.
		Concurrency: 1,
	}, nil
}
