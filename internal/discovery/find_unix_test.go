//go:build !windows

package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/JorgeMuehlebach/repo-sync/internal/gitexec"
)

func TestDiscoveryNeverUsesHostilePATH(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := trustedDiscoveryRunner(t)
	if result := runner.Run(context.Background(), repository, "init", "--initial-branch=main"); result.Err != nil {
		t.Fatal(result.Err)
	}
	if result := runner.Run(context.Background(), repository, "remote", "add", "origin", "https://github.com/owner/repo.git"); result.Err != nil {
		t.Fatal(result.Err)
	}
	hostile := filepath.Join(root, "hostile")
	if err := os.Mkdir(hostile, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "hostile-ran")
	writeDiscoveryExecutable(t, filepath.Join(hostile, "git"), "#!/bin/sh\nprintf hostile > \"$HOSTILE_GIT_MARKER\"\n")
	t.Setenv("PATH", hostile)
	t.Setenv("HOSTILE_GIT_MARKER", marker)
	matches, err := Find(context.Background(), runner, []string{root}, map[string]bool{"owner/repo": true})
	if err != nil || len(matches["owner/repo"]) != 1 {
		t.Fatalf("Find() = %#v, %v", matches, err)
	}
	spec, err := ParseGitHubBranchURL("https://github.com/owner/repo/tree/main")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(context.Background(), runner, repository, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("hostile PATH Git executed: %v", err)
	}
}

func TestDiscoveryFailsClosedWhenPinnedGitChanges(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	pinned := filepath.Join(root, "pinned-git")
	marker := filepath.Join(root, "changed-ran")
	writeDiscoveryExecutable(t, pinned, "#!/bin/sh\nexit 0\n")
	runner := discoveryRunnerForExecutable(t, pinned)
	writeDiscoveryExecutable(t, pinned, "#!/bin/sh\nprintf changed > \"$CHANGED_GIT_MARKER\"\n")
	t.Setenv("CHANGED_GIT_MARKER", marker)
	if _, err := Find(context.Background(), runner, []string{root}, map[string]bool{"owner/repo": true}); !errors.Is(err, gitexec.ErrDependencyUnavailable) {
		t.Fatalf("Find() error = %v", err)
	}
	spec, err := ParseGitHubBranchURL("https://github.com/owner/repo/tree/main")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(context.Background(), runner, repository, spec); !errors.Is(err, gitexec.ErrDependencyUnavailable) {
		t.Fatalf("ValidatePath() error = %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("changed pinned Git executed: %v", err)
	}
}

func discoveryRunnerForExecutable(t *testing.T, path string) gitexec.SystemRunner {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	registry, err := json.Marshal(map[string]any{
		"schema_version": 2, "contract_version": "2.0.0",
		"local_dependencies": []any{map[string]any{
			"id": "context-system.git", "path": map[string]any{"style": "posix", "value": path},
			"sha256": hex.EncodeToString(digest[:]), "kind": "executable",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := gitexec.NewSystemRunnerFromRegistry(registry)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func writeDiscoveryExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
