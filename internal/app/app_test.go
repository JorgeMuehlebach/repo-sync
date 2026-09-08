package app

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/JorgeMuehlebach/repo-sync/internal/config"
	"github.com/JorgeMuehlebach/repo-sync/internal/discovery"
	"github.com/JorgeMuehlebach/repo-sync/internal/state"
)

func TestConfigAddListAndRemove(t *testing.T) {
	dir := t.TempDir()
	var output bytes.Buffer
	application := &Application{
		version:    "test",
		in:         bytes.NewBuffer(nil),
		out:        &output,
		errOut:     &output,
		configDir:  dir,
		configPath: filepath.Join(dir, "config.yaml"),
		statePath:  filepath.Join(dir, "state.json"),
		lockDir:    filepath.Join(dir, "locks"),
		homeDir:    dir,
	}
	branchURL := "https://github.com/owner/repo/tree/main"
	if err := application.Run([]string{"config", "add", branchURL}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(application.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 1 || cfg.Repositories[0] != branchURL {
		t.Fatalf("repositories = %#v", cfg.Repositories)
	}
	output.Reset()
	if err := application.Run([]string{"config", "list"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte(branchURL)) {
		t.Fatalf("config list output = %q", output.String())
	}
	if err := application.Run([]string{"config", "remove", branchURL}); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(application.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 0 {
		t.Fatalf("repositories after remove = %#v", cfg.Repositories)
	}
}

func TestSetupSelectsOneOfMultipleClones(t *testing.T) {
	home := t.TempDir()
	first := filepath.Join(home, "clone-a")
	second := filepath.Join(home, "clone-b")
	for _, clone := range []string{first, second} {
		if err := os.MkdirAll(clone, 0o755); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("git", "init", clone)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, output)
		}
		command = exec.Command("git", "-C", clone, "remote", "add", "origin", "git@github.com:owner/repo.git")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git remote add: %v\n%s", err, output)
		}
	}
	dir := filepath.Join(home, "config")
	application := &Application{
		version:    "test",
		in:         bytes.NewBufferString("2\n"),
		out:        &bytes.Buffer{},
		errOut:     &bytes.Buffer{},
		configDir:  dir,
		configPath: filepath.Join(dir, "config.yaml"),
		statePath:  filepath.Join(dir, "state.json"),
		lockDir:    filepath.Join(dir, "locks"),
		homeDir:    home,
	}
	branchURL := "https://github.com/owner/repo/tree/main"
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []string{branchURL}}); err != nil {
		t.Fatal(err)
	}
	if err := application.setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	spec, err := discovery.ParseGitHubBranchURL(branchURL)
	if err != nil {
		t.Fatal(err)
	}
	machineState, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := machineState.Repositories[spec.StateKey()].Path; got != second {
		t.Fatalf("selected path = %q, want %q", got, second)
	}
}

func TestRepositoryLockPreventsOverlap(t *testing.T) {
	dir := t.TempDir()
	application := &Application{lockDir: filepath.Join(dir, "locks")}
	release, err := application.acquireLock("owner/repo#main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.acquireLock("owner/repo#main"); err == nil {
		t.Fatal("second lock acquisition succeeded")
	}
	release()
	releaseAgain, err := application.acquireLock("owner/repo#main")
	if err != nil {
		t.Fatalf("lock could not be reacquired: %v", err)
	}
	releaseAgain()
}
