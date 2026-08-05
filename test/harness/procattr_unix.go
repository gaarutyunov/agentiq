//go:build !windows

package harness

import "syscall"

// sysProcAttr puts the worker in its own process group so a kill takes down
// anything it started. On Windows there is no equivalent worth the cgo-free
// complexity, and CI runs Linux (SPEC.md §18.1).
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
