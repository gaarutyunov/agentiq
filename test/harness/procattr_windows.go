//go:build windows

package harness

import "syscall"

// sysProcAttr is a no-op on Windows.
func sysProcAttr() *syscall.SysProcAttr { return nil }
