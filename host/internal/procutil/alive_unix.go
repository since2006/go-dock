//go:build !windows

package procutil

import (
	"errors"
	"syscall"
)

// Alive reports whether a process with this PID exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
