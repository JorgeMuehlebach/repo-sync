//go:build !windows

package processstate

import (
	"errors"
	"syscall"
)

// Alive reports whether pid still names a process. Permission errors mean the
// process exists; any other unexpected result is returned so callers can fail
// closed instead of evicting a lock they do not understand.
func Alive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}
