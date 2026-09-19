package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/JorgeMuehlebach/repo-sync/internal/gitexec"
)

func TestFindSkipsWindowsAppData(t *testing.T) {
	home := t.TempDir()
	visible := filepath.Join(home, "projects", "repo")
	hidden := filepath.Join(home, "AppData", "repo")
	for _, repo := range []string{visible, hidden} {
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("git", "init", repo)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, output)
		}
		command = exec.Command("git", "-C", repo, "remote", "add", "origin", "https://github.com/owner/repo.git")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git remote add: %v\n%s", err, output)
		}
	}
	matches, err := Find(context.Background(), trustedDiscoveryRunner(t), []string{home}, map[string]bool{"owner/repo": true})
	if err != nil {
		t.Fatal(err)
	}
	if got := matches["owner/repo"]; len(got) != 1 || got[0] != visible {
		t.Fatalf("matches = %#v, want only %s", got, visible)
	}
}

func trustedDiscoveryRunner(t *testing.T) gitexec.SystemRunner {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	style := "posix"
	if runtime.GOOS == "windows" {
		style = "windows"
	}
	registry, err := json.Marshal(map[string]any{
		"schema_version": 2, "contract_version": "2.0.0",
		"local_dependencies": []any{map[string]any{
			"id": "context-system.git", "path": map[string]any{"style": style, "value": path},
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
