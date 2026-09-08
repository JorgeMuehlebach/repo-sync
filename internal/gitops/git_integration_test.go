package gitops

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JorgeMuehlebach/repo-sync/internal/discovery"
)

func TestSyncWithSystemGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	runGit(t, root, "init", "--bare", "--initial-branch=main", bare)
	runGit(t, root, "init", "--initial-branch=main", work)
	runGit(t, work, "config", "user.name", "Repo Sync Test")
	runGit(t, work, "config", "user.email", "repo-sync@example.invalid")
	runGit(t, work, "config", "protocol.file.allow", "always")
	localURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(bare)}).String()
	runGit(t, work, "config", "url."+localURL+".insteadOf", "https://github.com/owner/repo.git")
	runGit(t, work, "remote", "add", "origin", "https://github.com/owner/repo.git")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-m", "initial")
	runGit(t, work, "push", "-u", "origin", "main")

	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := discovery.RepositorySpec{Key: "owner/repo", Branch: "main"}
	if err := NewSyncer().Sync(context.Background(), work, spec); err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, work, "status", "--porcelain=v1"); got != "" {
		t.Fatalf("working tree is dirty: %s", got)
	}
	if got := runGit(t, bare, "show", "main:README.md"); got != "updated" {
		t.Fatalf("remote README = %q, want updated", got)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
