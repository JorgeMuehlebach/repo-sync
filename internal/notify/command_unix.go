//go:build !windows

package notify

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func platformLookPath(name string) (string, error) { return exec.LookPath(name) }

func platformRunCommand(ctx context.Context, executable string, args ...string) error {
	command := exec.Command(executable, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		return err
	case <-ctx.Done():
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			_ = command.Process.Kill()
		}
		select {
		case <-done:
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return ctx.Err()
		}
	}
}
