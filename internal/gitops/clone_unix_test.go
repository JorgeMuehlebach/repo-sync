//go:build !windows

package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/JorgeMuehlebach/repo-sync/internal/discovery"
)

func TestSecureCloneFailsBeforeChangedPinnedGitExecutes(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pinned := filepath.Join(root, "pinned-git")
	marker := filepath.Join(root, "changed-ran")
	writeCloneExecutable(t, pinned, "#!/bin/sh\nexit 0\n")
	runner := cloneTestRunner(t, pinned)
	writeCloneExecutable(t, pinned, "#!/bin/sh\nprintf changed > \"$CHANGED_GIT_MARKER\"\n")
	t.Setenv("CHANGED_GIT_MARKER", marker)
	spec, err := discovery.ParseGitHubBranchURL("https://github.com/owner/repo/tree/main")
	if err != nil {
		t.Fatal(err)
	}
	if err := SecureClone(context.Background(), runner, root, filepath.Join(root, "clone"), spec); err == nil {
		t.Fatal("SecureClone accepted a changed pinned Git executable")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("changed pinned Git executed: %v", err)
	}
}

func cloneTestRunner(t *testing.T, executable string) SystemRunner {
	t.Helper()
	contents, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	registry, err := json.Marshal(map[string]any{
		"schema_version": 2, "contract_version": "2.0.0",
		"local_dependencies": []any{map[string]any{
			"id": "context-system.git", "path": map[string]any{"style": "posix", "value": executable},
			"sha256": hex.EncodeToString(digest[:]), "kind": "executable",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewSystemRunnerFromRegistry(registry)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func writeCloneExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
