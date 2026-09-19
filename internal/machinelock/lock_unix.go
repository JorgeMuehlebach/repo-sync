//go:build !windows

package machinelock

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Acquire holds a kernel-backed exclusive lock until release. The lock file is
// persistent; process death closes the descriptor and releases the lock
// immediately without stale-PID recovery.
func Acquire(path string, timeout time.Duration, purpose string) (*os.File, func(), error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s lock: %w", purpose, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, nil, fmt.Errorf("open %s lock", purpose)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%s lock is not a regular file", purpose)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, func() {
				_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, nil, fmt.Errorf("lock %s: %w", purpose, err)
		}
		if timeout <= 0 || time.Now().After(deadline) {
			_ = file.Close()
			return nil, nil, fmt.Errorf("%s is busy; try again", purpose)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
