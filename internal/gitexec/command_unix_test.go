//go:build !windows

package gitexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSystemRunnerExecutesVerifiedDescriptorAcrossPathSwap(t *testing.T) {
	t.Cleanup(CleanupProcessSnapshots)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "git")
	marker := filepath.Join(root, "executed")
	oldScript := []byte("#!/bin/sh\nprintf old > \"$REPO_SYNC_GITEXEC_MARKER\"\nprintf old\n")
	newScript := []byte("#!/bin/sh\nprintf replacement > \"$REPO_SYNC_GITEXEC_MARKER\"\nprintf replacement\n")
	if err := os.WriteFile(executable, oldScript, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(oldScript)
	runner, err := NewSystemRunner(Dependency{path: executable, digest: hex.EncodeToString(digest[:])})
	if err != nil {
		t.Fatal(err)
	}
	runner.beforeVerifiedExec = func() error {
		if err := os.Rename(executable, filepath.Join(root, "verified-old")); err != nil {
			return err
		}
		return os.WriteFile(executable, newScript, 0o700)
	}
	t.Setenv("REPO_SYNC_GITEXEC_MARKER", marker)

	result := runner.Run(context.Background(), root, "--version")
	if result.Err != nil {
		t.Fatalf("verified descriptor did not execute: %v (%s)", result.Err, result.Output)
	}
	if result.Output != "old" {
		t.Fatalf("executed output = %q, want verified old executable", result.Output)
	}
	executed, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(executed) != "old" {
		t.Fatalf("executed marker = %q, replacement path won the race", executed)
	}

	result = runner.Run(context.Background(), root, "--version")
	if !errors.Is(result.Err, ErrDependencyUnavailable) {
		t.Fatalf("subsequent run error = %v, want dependency unavailable", result.Err)
	}
}

func TestCleanupProcessSnapshotsRemovesPrivateExecutable(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "git")
	contents := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(executable, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	runner, err := NewSystemRunner(Dependency{path: executable, digest: hex.EncodeToString(digest[:])})
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot := runner.binding.root
	CleanupProcessSnapshots()
	if _, err := os.Stat(snapshotRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot root still exists after process cleanup: %v", err)
	}
}
