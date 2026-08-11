// Command copysql mirrors the `.sql` files of one directory into another.
//
// It exists because //go:embed cannot reference a parent directory: `migrate/`
// has to embed gopgql's output, gopgql writes it under `generated/`, and
// neither `migrate/` nor anything else that could embed it sits above
// `generated/`. The copies are generated artifacts, so SPEC.md §17.3's
// `go generate ./... && git diff --exit-code` gate is what keeps them honest.
//
// # Why a program rather than a line of shell
//
// The obvious `sh -c "rm -f dst/*.sql && cp src/*.sql dst/"` breaks on the case
// this repository is in right now: `generated/migrations/` holds no `.sql` at
// all (gaarutyunov/gopgql#53), and an unmatched glob makes `cp` fail, which
// fails `go generate ./...` for a directory that is legitimately empty.
// Suppressing that with `|| true` would suppress a real copy failure with it.
// A program can tell "nothing to copy" from "the copy did not work", and it
// also runs on a developer's Windows checkout, where `sh` is not a given.
//
// The mirror is exact: every `.sql` already in the destination is removed
// first, so a migration deleted upstream does not survive as a stale embedded
// copy that the drift gate cannot see.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	src := flag.String("src", "", "directory to copy .sql files from")
	dst := flag.String("dst", "", "directory to mirror them into")
	flag.Parse()

	if *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "copysql: both -src and -dst are required")
		os.Exit(2)
	}
	if err := run(*src, *dst); err != nil {
		fmt.Fprintln(os.Stderr, "copysql:", err)
		os.Exit(1)
	}
}

func run(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}

	stale, err := filepath.Glob(filepath.Join(dst, "*.sql"))
	if err != nil {
		return fmt.Errorf("list %s: %w", dst, err)
	}
	for _, f := range stale {
		if err := os.Remove(f); err != nil {
			return fmt.Errorf("remove %s: %w", f, err)
		}
	}

	sources, err := filepath.Glob(filepath.Join(src, "*.sql"))
	if err != nil {
		return fmt.Errorf("list %s: %w", src, err)
	}
	for _, f := range sources {
		body, err := os.ReadFile(f) //nolint:gosec // a path this command was pointed at by a go:generate directive
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		out := filepath.Join(dst, filepath.Base(f))
		if err := os.WriteFile(out, body, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", out, err)
		}
	}
	return nil
}
