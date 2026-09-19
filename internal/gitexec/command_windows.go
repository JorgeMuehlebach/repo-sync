//go:build windows

package gitexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

type executableBinding struct{}

func cleanupProcessSnapshots() {}

func prepareVerifiedExecutable(_ context.Context, retained *os.File, path, _ string) (*executableBinding, error) {
	if retained == nil || path == "" {
		return nil, fmt.Errorf("verified executable is unavailable")
	}
	return &executableBinding{}, nil
}

// OpenCanonicalRegularRetained holds a non-shareable Windows handle through
// process completion, preventing ordinary writes, replacement, or deletion
// while CreateProcess opens this verified path.
func commandForVerifiedExecutable(ctx context.Context, binding *executableBinding, retained *os.File, path string, args ...string) (*exec.Cmd, error) {
	if binding == nil || retained == nil || path == "" {
		return nil, fmt.Errorf("verified executable is unavailable")
	}
	return exec.CommandContext(ctx, path, args...), nil
}
