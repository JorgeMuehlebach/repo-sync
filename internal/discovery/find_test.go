package discovery

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
	matches, err := Find(context.Background(), []string{home}, map[string]bool{"owner/repo": true})
	if err != nil {
		t.Fatal(err)
	}
	if got := matches["owner/repo"]; len(got) != 1 || got[0] != visible {
		t.Fatalf("matches = %#v, want only %s", got, visible)
	}
}
