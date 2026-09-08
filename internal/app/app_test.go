package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JorgeMuehlebach/repo-sync/internal/background"
	"github.com/JorgeMuehlebach/repo-sync/internal/config"
	"github.com/JorgeMuehlebach/repo-sync/internal/discovery"
	"github.com/JorgeMuehlebach/repo-sync/internal/gitops"
	"github.com/JorgeMuehlebach/repo-sync/internal/state"
)

type fakeService struct {
	status       background.Status
	statusErr    error
	installErr   error
	startErr     error
	installCalls int
	startCalls   int
}

type recordingGitRunner struct {
	root  string
	calls []string
}

func (r *recordingGitRunner) Run(_ context.Context, _ string, args ...string) gitops.Result {
	call := strings.Join(args, " ")
	r.calls = append(r.calls, call)
	switch {
	case call == "rev-parse --show-toplevel":
		return gitops.Result{Output: r.root}
	case call == "config --get remote.origin.url":
		return gitops.Result{Output: "git@github.com:owner/repo.git"}
	case call == "config --get user.name":
		return gitops.Result{Output: "Test User"}
	case call == "config --get user.email":
		return gitops.Result{Output: "test@example.com"}
	case call == "branch --show-current":
		return gitops.Result{Output: "main"}
	case call == "status --porcelain=v1":
		return gitops.Result{Output: " M README.md"}
	case call == "status --porcelain=v1 --ignored=matching":
		return gitops.Result{Output: "!! local.env"}
	case call == "ls-remote --exit-code --heads origin refs/heads/main":
		return gitops.Result{Output: "abc refs/heads/main"}
	case strings.HasPrefix(call, "rev-parse --git-path "):
		return gitops.Result{Output: filepath.Join(r.root, ".git", args[len(args)-1])}
	default:
		return gitops.Result{}
	}
}

func (f *fakeService) Install() error {
	f.installCalls++
	return f.installErr
}

func (f *fakeService) Uninstall() error { return nil }

func (f *fakeService) Start() error {
	f.startCalls++
	return f.startErr
}

func (f *fakeService) Stop() error { return nil }

func (f *fakeService) Run() error { return nil }

func (f *fakeService) Status() (background.Status, error) { return f.status, f.statusErr }

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

func TestBootstrapLocalPathIsExplicitAndIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "checkout")
	command := exec.Command("git", "init", "-b", "feature/sync", root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	command = exec.Command("git", "-C", root, "remote", "add", "origin", "git@github.com:Owner/Repo.git")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, output)
	}
	dir := filepath.Join(t.TempDir(), "config")
	application := &Application{
		version:    "test",
		in:         bytes.NewBuffer(nil),
		out:        &bytes.Buffer{},
		errOut:     &bytes.Buffer{},
		configDir:  dir,
		configPath: filepath.Join(dir, "config.yaml"),
		statePath:  filepath.Join(dir, "state.json"),
		logPath:    filepath.Join(dir, "repo-sync.log"),
		lockDir:    filepath.Join(dir, "locks"),
		homeDir:    t.TempDir(),
		syncer:     gitops.NewSyncer(),
	}
	for range 2 {
		if err := application.Run([]string{"bootstrap", root}); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load(application.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 1 || cfg.Repositories[0] != "https://github.com/owner/repo/tree/feature/sync" {
		t.Fatalf("repositories = %#v", cfg.Repositories)
	}
	spec, err := discovery.ParseGitHubBranchURL(cfg.Repositories[0])
	if err != nil {
		t.Fatal(err)
	}
	machineState, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	got := machineState.Repositories[spec.StateKey()].Path
	gotInfo, gotErr := os.Stat(got)
	wantInfo, wantErr := os.Stat(root)
	if gotErr != nil || wantErr != nil || !os.SameFile(gotInfo, wantInfo) {
		t.Fatalf("path = %q, want the same checkout as %q", got, root)
	}
}

func TestBootstrapURLAdoptsDeterministicCheckout(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "repo")
	command := exec.Command("git", "init", "-b", "main", root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	command = exec.Command("git", "-C", root, "remote", "add", "origin", "https://github.com/owner/repo.git")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, output)
	}
	dir := filepath.Join(home, "config")
	application := &Application{
		version:    "test",
		in:         bytes.NewBuffer(nil),
		out:        &bytes.Buffer{},
		errOut:     &bytes.Buffer{},
		configDir:  dir,
		configPath: filepath.Join(dir, "config.yaml"),
		statePath:  filepath.Join(dir, "state.json"),
		logPath:    filepath.Join(dir, "repo-sync.log"),
		lockDir:    filepath.Join(dir, "locks"),
		homeDir:    home,
		syncer:     gitops.NewSyncer(),
	}
	branchURL := "https://github.com/owner/repo/tree/main"
	if err := application.Run([]string{"bootstrap", branchURL}); err != nil {
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
	if got := machineState.Repositories[spec.StateKey()].Path; got != filepath.Clean(root) {
		t.Fatalf("path = %q, want %q", got, root)
	}
}

func TestSyncDryRunDoesNotMutateRepositoryOrState(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	dir := t.TempDir()
	branchURL := "https://github.com/owner/repo/tree/main"
	configPath := filepath.Join(dir, "config.yaml")
	statePath := filepath.Join(dir, "state.json")
	if err := config.Save(configPath, config.Config{Interval: "5m", Repositories: []string{branchURL}}); err != nil {
		t.Fatal(err)
	}
	spec, err := discovery.ParseGitHubBranchURL(branchURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Save(statePath, state.State{Repositories: map[string]state.Repository{
		spec.StateKey(): {Path: root, LastError: "preserve me"},
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingGitRunner{root: root}
	var output bytes.Buffer
	application := &Application{
		out:        &output,
		errOut:     &output,
		configDir:  dir,
		configPath: configPath,
		statePath:  statePath,
		logPath:    filepath.Join(dir, "repo-sync.log"),
		lockDir:    filepath.Join(dir, "locks"),
		syncer:     gitops.Syncer{Git: runner},
	}
	if err := application.Run([]string{"sync", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("state changed during dry run\nbefore: %s\nafter: %s", before, after)
	}
	for _, call := range runner.calls {
		for _, mutating := range []string{"add ", "commit ", "fetch ", "rebase ", "push "} {
			if strings.HasPrefix(call, mutating) {
				t.Fatalf("dry run executed %q", call)
			}
		}
	}
	if _, err := os.Stat(application.lockDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run created lock directory: %v", err)
	}
	if _, err := os.Stat(application.logPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run created a log: %v", err)
	}
	if !strings.Contains(output.String(), "M README.md") {
		t.Fatalf("dry-run output = %q", output.String())
	}
}

func TestDoctorIsReadOnlyAndChecksRemote(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	dir := t.TempDir()
	branchURL := "https://github.com/owner/repo/tree/main"
	configPath := filepath.Join(dir, "config.yaml")
	statePath := filepath.Join(dir, "state.json")
	if err := config.Save(configPath, config.Config{Interval: "5m", Repositories: []string{branchURL}}); err != nil {
		t.Fatal(err)
	}
	spec, err := discovery.ParseGitHubBranchURL(branchURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Save(statePath, state.State{Repositories: map[string]state.Repository{
		spec.StateKey(): {Path: root},
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingGitRunner{root: root}
	service := &fakeService{status: background.StatusStopped}
	var output bytes.Buffer
	application := &Application{
		out:        &output,
		errOut:     &output,
		configDir:  dir,
		configPath: configPath,
		statePath:  statePath,
		logPath:    filepath.Join(dir, "repo-sync.log"),
		lockDir:    filepath.Join(dir, "locks"),
		syncer:     gitops.Syncer{Git: runner},
		serviceFactory: func() (background.Controller, error) {
			return service, nil
		},
	}
	if err := application.Run([]string{"doctor"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("state changed during doctor\nbefore: %s\nafter: %s", before, after)
	}
	if !strings.Contains(output.String(), "PASS owner/repo (main) remote access") || !strings.Contains(output.String(), "WARN owner/repo (main) has 1 ignored path") {
		t.Fatalf("doctor output = %q", output.String())
	}
	if _, err := os.Stat(application.lockDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor created lock directory: %v", err)
	}
	if _, err := os.Stat(application.logPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor created a log: %v", err)
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

func TestStartDoesNotEnableWhenServiceInstallFails(t *testing.T) {
	service := &fakeService{statusErr: background.ErrNotInstalled, installErr: errors.New("access denied")}
	application := newStartTestApplication(t, service)
	err := application.start()
	if err == nil || !strings.Contains(err.Error(), "install service: access denied") {
		t.Fatalf("start() error = %v", err)
	}
	if service.startCalls != 0 {
		t.Fatalf("Start() calls = %d, want 0", service.startCalls)
	}
	assertEnabled(t, application.statePath, false)
}

func TestStartRollsBackEnabledWhenServiceStartFails(t *testing.T) {
	service := &fakeService{status: background.StatusStopped, startErr: errors.New("could not run task")}
	application := newStartTestApplication(t, service)
	err := application.start()
	if err == nil || !strings.Contains(err.Error(), "start service: could not run task") {
		t.Fatalf("start() error = %v", err)
	}
	if service.startCalls != 1 {
		t.Fatalf("Start() calls = %d, want 1", service.startCalls)
	}
	assertEnabled(t, application.statePath, false)
}

func TestStartEnablesAfterServiceIsReady(t *testing.T) {
	service := &fakeService{status: background.StatusStopped}
	application := newStartTestApplication(t, service)
	if err := application.start(); err != nil {
		t.Fatal(err)
	}
	if service.startCalls != 1 {
		t.Fatalf("Start() calls = %d, want 1", service.startCalls)
	}
	assertEnabled(t, application.statePath, true)
}

func newStartTestApplication(t *testing.T, service background.Controller) *Application {
	t.Helper()
	dir := t.TempDir()
	branchURL := "https://github.com/owner/repo/tree/main"
	configPath := filepath.Join(dir, "config.yaml")
	statePath := filepath.Join(dir, "state.json")
	if err := config.Save(configPath, config.Config{Interval: "5m", Repositories: []string{branchURL}}); err != nil {
		t.Fatal(err)
	}
	spec, err := discovery.ParseGitHubBranchURL(branchURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Save(statePath, state.State{Repositories: map[string]state.Repository{
		spec.StateKey(): {Path: filepath.Join(dir, "repo")},
	}}); err != nil {
		t.Fatal(err)
	}
	return &Application{
		version:    "test",
		in:         bytes.NewBuffer(nil),
		out:        &bytes.Buffer{},
		errOut:     &bytes.Buffer{},
		configDir:  dir,
		configPath: configPath,
		statePath:  statePath,
		logPath:    filepath.Join(dir, "repo-sync.log"),
		lockDir:    filepath.Join(dir, "locks"),
		homeDir:    dir,
		serviceFactory: func() (background.Controller, error) {
			return service, nil
		},
	}
}

func assertEnabled(t *testing.T, path string, want bool) {
	t.Helper()
	machineState, err := state.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if machineState.Enabled != want {
		t.Fatalf("Enabled = %t, want %t", machineState.Enabled, want)
	}
}
