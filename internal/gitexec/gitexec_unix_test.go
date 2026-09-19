//go:build !windows

package gitexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestSystemRunnerNeverUsesInheritedPATH(t *testing.T) {
	root := canonicalTestDirectory(t)
	pinned := filepath.Join(root, "pinned-git")
	pinnedMarker := filepath.Join(root, "pinned-called")
	hostileDirectory := filepath.Join(root, "hostile")
	if err := os.Mkdir(hostileDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	hostileMarker := filepath.Join(root, "hostile-called")
	writeExecutable(t, pinned, "#!/bin/sh\nprintf pinned > \"$PINNED_MARKER\"\n")
	writeExecutable(t, filepath.Join(hostileDirectory, "git"), "#!/bin/sh\nprintf hostile > \"$HOSTILE_MARKER\"\n")
	runner := runnerForExecutable(t, pinned)
	t.Setenv("PATH", hostileDirectory)
	t.Setenv("PINNED_MARKER", pinnedMarker)
	t.Setenv("HOSTILE_MARKER", hostileMarker)
	result := runner.Run(context.Background(), root, "--version")
	if result.Err != nil {
		t.Fatalf("pinned Git failed: %v", result.Err)
	}
	if _, err := os.Stat(pinnedMarker); err != nil {
		t.Fatalf("pinned executable did not run: %v", err)
	}
	if _, err := os.Stat(hostileMarker); !os.IsNotExist(err) {
		t.Fatalf("hostile PATH Git ran: %v", err)
	}
}

func TestSystemRunnerRejectsChangedPinnedBinaryBeforeExecution(t *testing.T) {
	root := canonicalTestDirectory(t)
	pinned := filepath.Join(root, "pinned-git")
	marker := filepath.Join(root, "changed-called")
	writeExecutable(t, pinned, "#!/bin/sh\nexit 0\n")
	runner := runnerForExecutable(t, pinned)
	writeExecutable(t, pinned, "#!/bin/sh\nprintf changed > \"$CHANGED_MARKER\"\n")
	t.Setenv("CHANGED_MARKER", marker)
	result := runner.Run(context.Background(), root, "--version")
	if result.Err == nil {
		t.Fatal("changed pinned Git executable was run")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("changed pinned Git executed: %v", err)
	}
}

func runnerForExecutable(t *testing.T, path string) SystemRunner {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	runner, err := NewSystemRunnerFromRegistry(dependencyRegistry(t, path, hex.EncodeToString(digest[:]), "2.0.0", 1))
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}

func canonicalTestDirectory(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(root)
}
