//go:build !windows

package validation

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"syscall"
	"time"
)

func runIsolatedProcess(ctx context.Context, executable string, args, environment []string, stdout, stderr io.Writer) (int, error) {
	command := exec.Command(executable, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Env = environment
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var err error
	select {
	case err = <-done:
		// The validated executable has exited, but a descendant may still be
		// running in its process group. Do not let it outlive validation.
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	case <-ctx.Done():
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
			_ = command.Process.Kill()
		}
		select {
		case err = <-done:
			if err == nil {
				err = ctx.Err()
			}
		case <-time.After(5 * time.Second):
			return -1, ctx.Err()
		}
	}
	if err == nil {
		return 0, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode(), err
	}
	return -1, err
}
