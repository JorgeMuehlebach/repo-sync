package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/JorgeMuehlebach/repo-sync/internal/atomicfile"
	"github.com/JorgeMuehlebach/repo-sync/internal/background"
	"github.com/JorgeMuehlebach/repo-sync/internal/config"
	"github.com/JorgeMuehlebach/repo-sync/internal/discovery"
	"github.com/JorgeMuehlebach/repo-sync/internal/gitops"
	"github.com/JorgeMuehlebach/repo-sync/internal/notify"
	"github.com/JorgeMuehlebach/repo-sync/internal/state"
	"github.com/JorgeMuehlebach/repo-sync/internal/validation"
)

type fakeService struct {
	status       background.Status
	statusErr    error
	installErr   error
	startErr     error
	stopErr      error
	installCalls int
	startCalls   int
	stopCalls    int
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

func (f *fakeService) Stop() error {
	f.stopCalls++
	return f.stopErr
}
func (f *fakeService) Run() error { return nil }
func (f *fakeService) Status() (background.Status, error) {
	return f.status, f.statusErr
}

func testApplication(t *testing.T) (*Application, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	output := &bytes.Buffer{}
	application := &Application{
		version:    "test",
		in:         bytes.NewBuffer(nil),
		out:        output,
		errOut:     output,
		configDir:  dir,
		configPath: filepath.Join(dir, "config.yaml"),
		statePath:  filepath.Join(dir, "state.json"),
		logPath:    filepath.Join(dir, "repo-sync.log"),
		lockDir:    filepath.Join(dir, "locks"),
		homeDir:    dir,
		notifier:   quietNotifier{},
		now:        func() time.Time { return time.Date(2026, 9, 19, 1, 2, 3, 0, time.UTC) },
		reconcileRuntime: func(state.Repository, configuredRepository) error {
			return nil
		},
	}
	return application, output
}

func TestConfigAddListAndRemoveStructuredRepository(t *testing.T) {
	application, output := testApplication(t)
	branchURL := "https://github.com/owner/repo/tree/main"
	if err := application.Run([]string{"config", "add", branchURL, "--id", "bird-deter", "--mode", "mirror", "--source", "bird-deter.context"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(application.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 1 || cfg.Repositories[0].ID != "bird-deter" || cfg.Repositories[0].EffectiveMode() != config.ModeMirror {
		t.Fatalf("repositories = %#v", cfg.Repositories)
	}
	output.Reset()
	if err := application.Run([]string{"config", "list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "bird-deter  mirror") || !strings.Contains(output.String(), branchURL) {
		t.Fatalf("config list output = %q", output.String())
	}
	if err := application.Run([]string{"config", "remove", "bird-deter"}); err != nil {
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

func TestConfigAddRefusesNewLegacyScalar(t *testing.T) {
	application, _ := testApplication(t)
	err := application.Run([]string{"config", "add", "https://github.com/owner/repo/tree/main"})
	if err == nil || !strings.Contains(err.Error(), "--id") {
		t.Fatalf("config add error = %v", err)
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
		runAppGit(t, home, "init", clone)
		runAppGit(t, clone, "remote", "add", "origin", "git@github.com:owner/repo.git")
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
		notifier:   quietNotifier{},
	}
	branchURL := "https://github.com/owner/repo/tree/main"
	entry := config.StructuredRepository("repo-main", branchURL, config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	if err := application.setup(context.Background(), setupOptions{Registry: trustedGitRegistryPath(t)}); err != nil {
		t.Fatal(err)
	}
	machineState, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(second)
	if err != nil {
		t.Fatal(err)
	}
	if got := machineState.Repositories["repo-main"].Path; got != want {
		t.Fatalf("selected path = %q, want canonical %q", got, want)
	}
}

func TestTargetedSetupRejectsPathAlreadyBoundToPairedRepository(t *testing.T) {
	application, _ := testApplication(t)
	repositoryPath := canonicalTestPath(t, t.TempDir())
	runAppGit(t, repositoryPath, "init", "--initial-branch=main")
	runAppGit(t, repositoryPath, "remote", "add", "origin", "https://github.com/owner/repo.git")
	publisher := config.StructuredRepository("publisher", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	mirror := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/feature", config.ModePublish, "other.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{publisher, mirror}}); err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.Repositories["mirror"] = state.Repository{ID: "mirror", Mode: "publish", Path: repositoryPath}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	err := application.setup(context.Background(), setupOptions{RepositoryID: "publisher", Path: repositoryPath, Registry: trustedGitRegistryPath(t)})
	if err == nil || !strings.Contains(err.Error(), "share local path") {
		t.Fatalf("targeted setup error = %v", err)
	}
}

func TestRepositoryLockPreventsOverlap(t *testing.T) {
	application, _ := testApplication(t)
	release, err := application.acquireLock("owner/repo#main")
	if err != nil {
		t.Fatal(err)
	}
	lockEntries, err := os.ReadDir(application.lockDir)
	if err != nil || len(lockEntries) != 1 {
		t.Fatalf("lock entries = %v, %v", lockEntries, err)
	}
	lockPath := filepath.Join(application.lockDir, lockEntries[0].Name())
	if _, err := application.acquireLock("owner/repo#main"); err == nil {
		t.Fatal("live repository operation lock was not honored")
	}
	release()
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("persistent operation lock file is unavailable: %v", err)
	}
	releaseAgain, err := application.acquireLock("owner/repo#main")
	if err != nil {
		t.Fatalf("lock could not be reacquired: %v", err)
	}
	releaseAgain()
}

func TestRepositoryLockIsReleasedImmediatelyWhenOwnerProcessExits(t *testing.T) {
	application, _ := testApplication(t)
	command := exec.Command(os.Args[0], "-test.run=^TestRepositoryLockCrashHelper$")
	command.Env = append(os.Environ(), "REPO_SYNC_LOCK_HELPER_DIR="+application.lockDir)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("lock helper failed: %v: %s", err, output)
	}
	release, err := application.acquireLock("owner/repo#main")
	if err != nil {
		t.Fatalf("kernel lock survived owner process exit: %v", err)
	}
	release()
}

func TestRepositoryLockCrashHelper(t *testing.T) {
	lockDir := os.Getenv("REPO_SYNC_LOCK_HELPER_DIR")
	if lockDir == "" {
		t.Skip("subprocess helper")
	}
	application := &Application{lockDir: lockDir, now: time.Now}
	if _, err := application.acquireLock("owner/repo#main"); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestTargetedSyncTouchesOnlySelectedRepository(t *testing.T) {
	application, _ := testApplication(t)
	application.reconcileRuntime = func(state.Repository, configuredRepository) error { return nil }
	first := config.StructuredRepository("first", "https://github.com/owner/first/tree/main", config.ModePublish, "first.context")
	second := config.StructuredRepository("second", "https://github.com/owner/second/tree/main", config.ModePublish, "second.context")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{first, second}}); err != nil {
		t.Fatal(err)
	}
	runtimeState := validStatusRuntime("first.context")
	machineState := state.New()
	machineState.Repositories["first"] = state.Repository{ID: "first", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), ValidationRuntime: runtimeState}
	runtimeState = validStatusRuntime("second.context")
	machineState.Repositories["second"] = state.Repository{ID: "second", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), ValidationRuntime: runtimeState}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	engine := &recordingEngine{}
	application.publisher = engine
	application.mirror = engine
	if err := application.Run([]string{"sync", "--repository", "second"}); err != nil {
		t.Fatal(err)
	}
	if len(engine.targets) != 1 || engine.targets[0].ID != "second" {
		t.Fatalf("sync targets = %#v", engine.targets)
	}
}

func TestConfigRemoveWaitsAcrossFinalReconcileAndSideEffectBoundary(t *testing.T) {
	assertConfigRemoveSerialized(t, false)
}

func TestConfigRemoveWaitsAfterSideEffectBeforeStateCommit(t *testing.T) {
	assertConfigRemoveSerialized(t, true)
}

func assertConfigRemoveSerialized(t *testing.T, afterSideEffect bool) {
	t.Helper()
	application, _ := testApplication(t)
	application.reconcileRuntime = func(state.Repository, configuredRepository) error { return nil }
	entry := config.StructuredRepository("repository", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.Repositories["repository"] = state.Repository{ID: "repository", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), ValidationRuntime: validStatusRuntime("example.source")}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	boundary := make(chan struct{})
	proceed := make(chan struct{})
	if afterSideEffect {
		application.publisher = &recordingEngine{}
		application.afterEngineSync = func() {
			close(boundary)
			<-proceed
		}
	} else {
		application.publisher = &reconcileBoundaryEngine{boundary: boundary, proceed: proceed}
	}
	syncDone := make(chan error, 1)
	go func() { syncDone <- application.syncAll(context.Background(), false, "repository") }()
	select {
	case <-boundary:
	case <-time.After(2 * time.Second):
		t.Fatal("sync did not reach the tested removal boundary")
	}
	removeDone := make(chan error, 1)
	go func() { removeDone <- application.configCommand([]string{"remove", "repository"}) }()
	select {
	case err := <-removeDone:
		t.Fatalf("config removal bypassed the repository operation lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(proceed)
	if err := <-syncDone; err != nil {
		t.Fatalf("sync failed after releasing the boundary: %v", err)
	}
	if err := <-removeDone; err != nil {
		t.Fatalf("serialized config removal failed: %v", err)
	}
	cfg, err := config.Load(application.configPath)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 0 {
		t.Fatalf("repository remained configured: %#v", cfg.Repositories)
	}
	if _, ok := updated.Repositories["repository"]; ok {
		t.Fatal("sync resurrected state after serialized config removal")
	}
}

func TestStaleRepositoryStateGenerationIsRejected(t *testing.T) {
	application, _ := testApplication(t)
	entry := config.StructuredRepository("repository", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	repositories, err := configuredRepositories(config.Config{Interval: "5m", Repositories: []config.Repository{entry}}, "")
	if err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.Repositories["repository"] = state.Repository{ID: "repository", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), Revision: 4, ValidationRuntime: validStatusRuntime("example.source")}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	stale := machineState.Repositories["repository"]
	if err := state.Update(application.statePath, func(current *state.State) error {
		newer := current.Repositories["repository"]
		newer.Revision++
		newer.LastError = "newer writer"
		current.Repositories["repository"] = newer
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := application.persistRepositoryState(repositories[0], stale, true); !errors.Is(err, errRepositoryStateCAS) {
		t.Fatalf("stale state generation error = %v", err)
	}
	updated, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Repositories["repository"].Revision != 5 || updated.Repositories["repository"].LastError != "newer writer" {
		t.Fatalf("stale writer overwrote newer state: %#v", updated.Repositories["repository"])
	}
}

func TestOperationLockContentionIsDurableWithoutInvalidatingPinnedRevision(t *testing.T) {
	application, _ := testApplication(t)
	application.reconcileRuntime = func(state.Repository, configuredRepository) error { return nil }
	entry := config.StructuredRepository("repository", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.Repositories["repository"] = state.Repository{ID: "repository", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), Revision: 9, ValidationRuntime: validStatusRuntime("example.source")}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	engine := &recordingEngine{}
	notifier := &countingNotifier{delivery: notify.DeliverySent}
	application.publisher = engine
	application.notifier = notifier
	release, err := application.acquireLock("repository")
	if err != nil {
		t.Fatal(err)
	}
	if err := application.syncAll(context.Background(), false, "repository"); err == nil {
		release()
		t.Fatal("overlapping sync unexpectedly acquired the operation lock")
	}
	release()
	updated, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	repository := updated.Repositories["repository"]
	if repository.Revision != 9 || repository.Failure == nil || repository.Failure.Code != "REPO-OPERATION-LOCKED" {
		t.Fatalf("durable contention state = %#v", repository)
	}
	if notifier.calls != 1 || len(engine.targets) != 0 {
		t.Fatalf("contention calls: notifier=%d engine=%d", notifier.calls, len(engine.targets))
	}
}

func TestPinnedReconciliationRejectsConcurrentConfigAndStateChanges(t *testing.T) {
	application, _ := testApplication(t)
	entry := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	repositories, err := configuredRepositories(config.Config{Interval: "5m", Repositories: []config.Repository{entry}}, "")
	if err != nil {
		t.Fatal(err)
	}
	pinnedState := state.Repository{ID: "mirror", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), ValidationRuntime: validStatusRuntime("example.source")}
	machineState := state.New()
	machineState.Repositories["mirror"] = pinnedState
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	if err := application.reconcilePinnedTarget(repositories[0], pinnedState); err != nil {
		t.Fatalf("stable binding was rejected: %v", err)
	}
	changedEntry := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.changed")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{changedEntry}}); err != nil {
		t.Fatal(err)
	}
	if err := application.reconcilePinnedTarget(repositories[0], pinnedState); err == nil {
		t.Fatal("concurrent portable config change was accepted")
	}
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	machineState.Repositories["mirror"] = pinnedState
	changedState := machineState.Repositories["mirror"]
	changedState.ValidationRuntime.RegistryRevision++
	machineState.Repositories["mirror"] = changedState
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	if err := application.reconcilePinnedTarget(repositories[0], pinnedState); err == nil {
		t.Fatal("concurrent machine binding change was accepted")
	}
}

func TestStatusJSONIsSanitizedAndUsesFrozenFields(t *testing.T) {
	application, output := testApplication(t)
	entry := config.StructuredRepository("bird-deter", "https://github.com/owner/repo/tree/main", config.ModePublish, "bird-deter.context")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(t.TempDir(), "must-not-leak")
	digest := "sha256:" + strings.Repeat("a", 64)
	objectID := strings.Repeat("b", 40)
	machineState := state.New()
	machineState.Repositories["bird-deter"] = state.Repository{
		ID: "bird-deter", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()),
		AcceptedCommit: objectID, AcceptedTree: strings.Repeat("c", 40), CandidateCommit: objectID, CandidateTree: strings.Repeat("d", 40),
		ValidationRuntime: state.ValidationRuntime{
			Protocol: validation.ReportProtocol, SourceID: "bird-deter.context", ContextctlPath: secretPath,
			ContextctlDigest: digest, RegistryPath: secretPath, RegistryRevision: 1, RegistryDigest: digest,
			TrustStatePath: secretPath, TrustStateRevision: 0, TrustStateDigest: digest,
		},
		Validation: state.Validation{
			ContractVersion: "2.0.0", ObjectID: strings.Repeat("d", 40), Verdict: "pass", CheckIDs: []string{"CTX-SCHEMA-PASS"},
			ValidatorResults: []state.ValidatorResult{{ContractID: "example.validator", ContractVersion: "1.0.0", Disposition: "pass", CheckIDs: []string{"CTX-SCHEMA-PASS"}, SourceID: "bird-deter.context", SourceRole: "context-mirror"}},
		},
	}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	if err := application.Run([]string{"status", "--repository", "bird-deter", "--json"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), secretPath) || strings.Contains(output.String(), "accepted_tree") || strings.Contains(output.String(), "candidate_tree") {
		t.Fatalf("status leaked private or non-contract data: %s", output.String())
	}
	var report map[string]any
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	repositories := report["repositories"].([]any)
	repository := repositories[0].(map[string]any)
	if repository["repository_id"] != "bird-deter" || repository["accepted_commit"] != objectID {
		t.Fatalf("repository status = %#v", repository)
	}
	validator := repository["last_validation"].(map[string]any)["validator_results"].([]any)[0].(map[string]any)
	if _, ok := validator["source_id"]; ok {
		t.Fatalf("status exposed report-only validator source fields: %#v", validator)
	}
	if _, ok := validator["bundle_version"]; ok {
		t.Fatalf("status exposed non-contract validator bundle version: %#v", validator)
	}
	statusData, err := os.ReadFile(filepath.Join(application.configDir, "context-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(statusData, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted["schema_version"] != float64(contextStatusSchema) || persisted["protocol"] != contextStatusProtocol {
		t.Fatalf("persisted context status = %#v", persisted)
	}
	if info, err := os.Stat(filepath.Join(application.configDir, "context-status.json")); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		t.Fatalf("context status permissions = %v, %v", info, err)
	}
}

func TestContextStatusWithholdsRefreshForUnreconciledRepository(t *testing.T) {
	application, _ := testApplication(t)
	first := config.StructuredRepository("first", "https://github.com/owner/first/tree/main", config.ModePublish, "first.context")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{first}}); err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.Repositories["first"] = state.Repository{ID: "first", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), ValidationRuntime: validStatusRuntime("first.context")}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	if _, err := application.refreshContextStatus("running"); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(application.configDir, "context-status.json")
	before, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	second := config.StructuredRepository("second", "https://github.com/owner/second/tree/main", config.ModePublish, "second.context")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{first, second}}); err != nil {
		t.Fatal(err)
	}
	if _, err := application.refreshContextStatus("running"); err == nil || !strings.Contains(err.Error(), "second") {
		t.Fatalf("unreconciled refresh error = %v", err)
	}
	after, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("unreconciled repository replaced the last complete status snapshot")
	}
}

func TestContextStatusRejectsTimestampRegressionAndRecoversDeadWriterLock(t *testing.T) {
	application, _ := testApplication(t)
	entry := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.Repositories["mirror"] = state.Repository{ID: "mirror", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), ValidationRuntime: validStatusRuntime("example.source")}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(application.lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(application.lockDir, "context-status.lock"), []byte("2147483647\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := application.refreshContextStatus("running"); err != nil {
		t.Fatalf("dead cross-process writer lock was not recovered: %v", err)
	}
	statusPath := filepath.Join(application.configDir, "context-status.json")
	data, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	var report contextStatus
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	report.GeneratedAt = application.now().Add(time.Hour).Format(time.RFC3339Nano)
	future, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusPath, future, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := application.refreshContextStatus("running"); err == nil || !strings.Contains(err.Error(), "backwards") {
		t.Fatalf("timestamp regression error = %v", err)
	}
	unchanged, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(future, unchanged) {
		t.Fatal("timestamp regression replaced the newer status snapshot")
	}
}

func TestContextStatusMigratesLegacyStatusWithoutTrustingItsTimestamp(t *testing.T) {
	application, _ := testApplication(t)
	entry := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.Repositories["mirror"] = state.Repository{ID: "mirror", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), ValidationRuntime: validStatusRuntime("example.source")}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"schema_version":1,"protocol":"repo-sync.context-status.v1","generated_at":"2099-01-01T00:00:00Z"}`)
	if err := os.WriteFile(filepath.Join(application.configDir, "context-status.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := application.refreshContextStatus("running")
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != contextStatusSchema || report.Protocol != contextStatusProtocol {
		t.Fatalf("legacy status was not replaced by v2: %#v", report)
	}
}

func TestContextStatusSerializesWritersAcrossApplicationInstances(t *testing.T) {
	application, _ := testApplication(t)
	entry := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.Repositories["mirror"] = state.Repository{ID: "mirror", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), ValidationRuntime: validStatusRuntime("example.source")}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(application.lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, release, err := lockContextStatusFile(filepath.Join(application.lockDir, "context-status.lock"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, refreshErr := application.refreshContextStatus("running")
		result <- refreshErr
	}()
	select {
	case err := <-result:
		release()
		t.Fatalf("second writer bypassed the cross-process lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiting status writer did not acquire the released lock")
	}
}

func TestContextStatusFrozenSchemaRejectsUnknownAndDuplicateFields(t *testing.T) {
	application, _ := testApplication(t)
	entry := config.StructuredRepository("repository", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.Repositories["repository"] = state.Repository{ID: "repository", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), ValidationRuntime: validStatusRuntime("example.source")}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	if _, err := application.refreshContextStatus("running"); err != nil {
		t.Fatal(err)
	}
	valid, err := os.ReadFile(filepath.Join(application.configDir, "context-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var unknown map[string]any
	if err := json.Unmarshal(valid, &unknown); err != nil {
		t.Fatal(err)
	}
	unknown["unexpected"] = true
	unknownData, err := json.Marshal(unknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateContextStatusSchema(unknownData); err == nil {
		t.Fatal("frozen status schema accepted an unknown field")
	}
	duplicate := bytes.Replace(valid, []byte(`"schema_version": 2,`), []byte(`"schema_version": 2, "schema_version": 2,`), 1)
	if err := validateContextStatusSchema(duplicate); err == nil {
		t.Fatal("strict status decoder accepted a duplicate field")
	}
}

func TestStatusCompletionBarrierFailureIsDurableAndDoesNotAdvanceSuccess(t *testing.T) {
	application, _ := testApplication(t)
	application.reconcileRuntime = func(state.Repository, configuredRepository) error { return nil }
	entry := config.StructuredRepository("repository", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	previousSuccess := application.now().Add(-time.Hour)
	machineState := state.New()
	machineState.Repositories["repository"] = state.Repository{
		ID: "repository", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()),
		ValidationRuntime: validStatusRuntime("example.source"), LastSuccess: previousSuccess, LastSync: previousSuccess,
	}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	engine := &recordingEngine{}
	notifier := &countingNotifier{delivery: notify.DeliverySent}
	application.publisher = engine
	application.notifier = notifier
	application.writeContextStatus = func(path string, data []byte, mode os.FileMode) error {
		corrupted := bytes.Replace(data, []byte(contextStatusProtocol), []byte("repo-sync.context-status.invalid"), 1)
		return atomicfile.Write(path, corrupted, mode)
	}
	if err := application.syncAll(context.Background(), false, "repository"); err == nil {
		t.Fatal("sync crossed a corrupted accepted-status completion barrier")
	}
	updated, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	repository := updated.Repositories["repository"]
	if repository.Failure == nil || repository.Failure.Code != "REPO-CONTEXT-STATUS" {
		t.Fatalf("completion barrier failure was not durable: %#v", repository)
	}
	if !repository.LastSuccess.Equal(previousSuccess) || !repository.LastSync.Equal(previousSuccess) {
		t.Fatalf("failed completion advanced success timestamps: %#v", repository)
	}
	if notifier.calls != 1 || repository.Notification.Status != string(notify.DeliverySent) {
		t.Fatalf("completion failure notification = %#v, calls=%d", repository.Notification, notifier.calls)
	}
	if len(engine.targets) != 1 {
		t.Fatalf("engine calls = %d, want 1", len(engine.targets))
	}
}

func TestOrdinaryStatusWriterCannotExposePendingCompletion(t *testing.T) {
	application, _ := testApplication(t)
	application.reconcileRuntime = func(state.Repository, configuredRepository) error { return nil }
	entry := config.StructuredRepository("repository", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	baselineCommit := strings.Repeat("e", 40)
	machineState := state.New()
	machineState.Repositories["repository"] = state.Repository{ID: "repository", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), ValidationRuntime: validStatusRuntime("example.source"), AcceptedCommit: baselineCommit, AcceptedTree: strings.Repeat("f", 40)}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	if _, err := application.refreshContextStatus("running"); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(application.configDir, "context-status.json")
	before, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	proceed := make(chan struct{})
	application.afterPendingCompletion = func() error {
		close(entered)
		<-proceed
		return nil
	}
	application.publisher = &recordingEngine{}
	syncDone := make(chan error, 1)
	go func() { syncDone <- application.syncAll(context.Background(), false, "repository") }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("sync did not persist its pending completion")
	}
	if _, err := application.refreshContextStatus("running"); err == nil {
		t.Fatal("ordinary status writer exposed a pending completion")
	} else {
		var pending *pendingCompletionError
		if !errors.As(err, &pending) {
			t.Fatalf("ordinary writer error = %v", err)
		}
	}
	after, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("ordinary writer replaced the last accepted status while completion was pending")
	}
	close(proceed)
	if err := <-syncDone; err != nil {
		t.Fatal(err)
	}
}

func TestPublisherRecoversCrashBeforeStatusWrite(t *testing.T) {
	application, _ := testApplication(t)
	application.reconcileRuntime = func(state.Repository, configuredRepository) error { return nil }
	entry := config.StructuredRepository("repository", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.Repositories["repository"] = state.Repository{ID: "repository", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), ValidationRuntime: validStatusRuntime("example.source")}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	if _, err := application.refreshContextStatus("running"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(application.configDir, "context-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	engine := &recordingEngine{}
	application.publisher = engine
	application.afterPendingCompletion = func() error { return errors.New("simulated crash before status write") }
	if err := application.syncAll(context.Background(), false, "repository"); err == nil {
		t.Fatal("simulated pre-status crash unexpectedly completed")
	}
	crashed, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if crashed.Repositories["repository"].PendingCompletion == nil || !crashed.Repositories["repository"].LastSuccess.IsZero() {
		t.Fatalf("pre-status crash state = %#v", crashed.Repositories["repository"])
	}
	afterCrash, err := os.ReadFile(filepath.Join(application.configDir, "context-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, afterCrash) {
		t.Fatal("pre-status crash changed accepted status")
	}
	application.afterPendingCompletion = nil
	if err := application.syncAll(context.Background(), false, "repository"); err != nil {
		t.Fatalf("pending publisher completion did not recover: %v", err)
	}
	updated, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Repositories["repository"].PendingCompletion != nil || updated.Repositories["repository"].AcceptedCommit != successfulTestOutcome().AcceptedCommit || len(engine.targets) != 2 {
		t.Fatalf("recovered publisher state/calls = %#v / %d", updated.Repositories["repository"], len(engine.targets))
	}
}

func TestPublisherRecoversAcceptedStatusAfterCrashBeforePrivateFinalize(t *testing.T) {
	application, _ := testApplication(t)
	application.reconcileRuntime = func(state.Repository, configuredRepository) error { return nil }
	entry := config.StructuredRepository("repository", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.Repositories["repository"] = state.Repository{ID: "repository", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), ValidationRuntime: validStatusRuntime("example.source")}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	engine := &recordingEngine{}
	application.publisher = engine
	application.afterStatusBarrier = func() error { return errors.New("simulated crash after accepted status") }
	if err := application.syncAll(context.Background(), false, "repository"); err == nil {
		t.Fatal("simulated post-status crash unexpectedly completed")
	}
	crashed, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if crashed.Repositories["repository"].PendingCompletion == nil || !crashed.Repositories["repository"].LastSuccess.IsZero() {
		t.Fatalf("post-status crash state = %#v", crashed.Repositories["repository"])
	}
	status, err := readPersistedContextStatus(filepath.Join(application.configDir, "context-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if item, ok := exactStatusRepository(status, "repository"); !ok || item.AcceptedCommit != successfulTestOutcome().AcceptedCommit {
		t.Fatalf("accepted status was not durable at crash checkpoint: %#v", item)
	}
	application.afterStatusBarrier = nil
	engine.onSync = func() {
		current, loadErr := state.Load(application.statePath)
		if loadErr != nil {
			t.Errorf("load recovered state: %v", loadErr)
			return
		}
		if current.Repositories["repository"].PendingCompletion != nil || current.Repositories["repository"].AcceptedCommit != successfulTestOutcome().AcceptedCommit {
			t.Errorf("accepted status did not repair private state before the next engine call: %#v", current.Repositories["repository"])
		}
	}
	if err := application.syncAll(context.Background(), false, "repository"); err != nil {
		t.Fatalf("post-status pending completion did not recover: %v", err)
	}
}

func TestMirrorPromotionFinalizesOnlyAfterReopenedAcceptedStatus(t *testing.T) {
	application, _ := testApplication(t)
	application.reconcileRuntime = func(state.Repository, configuredRepository) error { return nil }
	entry := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/main", config.ModeMirror, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.Repositories["mirror"] = state.Repository{ID: "mirror", Mode: "mirror", Path: stableMirrorPointerTo(t, canonicalTestPath(t, t.TempDir())), ValidationRuntime: validStatusRuntime("example.source")}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	outcome := successfulTestOutcome()
	mirror := &transactionalMirrorFake{syncResult: gitops.MirrorSyncResult{Outcome: outcome, TransactionID: "transaction"}}
	mirror.onFinalize = func(finalized gitops.Outcome) *gitops.OperationError {
		persisted, err := state.Load(application.statePath)
		if err != nil {
			t.Errorf("load state at finalize: %v", err)
			return nil
		}
		repository := persisted.Repositories["mirror"]
		if repository.AcceptedCommit != finalized.AcceptedCommit || repository.AcceptedTree != finalized.AcceptedTree || repository.Failure != nil {
			t.Errorf("private state was not accepted before finalize: %#v", repository)
		}
		status, err := readPersistedContextStatus(filepath.Join(application.configDir, "context-status.json"))
		if err != nil {
			t.Errorf("reopen status at finalize: %v", err)
			return nil
		}
		item, ok := exactStatusRepository(status, "mirror")
		if !ok || item.AcceptedCommit != finalized.AcceptedCommit || item.CandidateCommit != finalized.CandidateCommit || item.LastValidation == nil || item.LastValidation.TreeObjectID != finalized.AcceptedTree {
			t.Errorf("shared status did not cross the exact completion barrier: %#v", item)
		}
		return nil
	}
	application.mirror = mirror
	if err := application.syncAll(context.Background(), false, "mirror"); err != nil {
		t.Fatal(err)
	}
	if mirror.finalizeCalls != 1 || mirror.rollbackCalls != 0 || mirror.acknowledgeCalls != 0 {
		t.Fatalf("mirror transaction calls: finalize=%d rollback=%d acknowledge=%d", mirror.finalizeCalls, mirror.rollbackCalls, mirror.acknowledgeCalls)
	}
}

func TestMirrorCrashAfterAcceptedStatusPreservesJournalAndFinalizesOnRecovery(t *testing.T) {
	application, _ := testApplication(t)
	application.reconcileRuntime = func(state.Repository, configuredRepository) error { return nil }
	entry := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/main", config.ModeMirror, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	baselineCommit := strings.Repeat("e", 40)
	baselineTree := strings.Repeat("f", 40)
	path := stableMirrorPointerTo(t, canonicalTestPath(t, t.TempDir()))
	machineState := state.New()
	machineState.Repositories["mirror"] = state.Repository{
		ID: "mirror", Mode: "mirror", Path: path, ValidationRuntime: validStatusRuntime("example.source"),
		AcceptedCommit: baselineCommit, AcceptedTree: baselineTree,
	}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	outcome := successfulTestOutcome()
	mirror := &transactionalMirrorFake{syncResult: gitops.MirrorSyncResult{Outcome: outcome, TransactionID: "transaction"}}
	application.mirror = mirror
	application.afterStatusBarrier = func() error { return errors.New("simulated crash after mirror accepted status") }
	if err := application.syncAll(context.Background(), false, "mirror"); err == nil {
		t.Fatal("simulated post-status mirror crash unexpectedly completed")
	}
	crashed, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if crashed.Repositories["mirror"].PendingCompletion == nil {
		t.Fatalf("accepted mirror completion was not retained for recovery: %#v", crashed.Repositories["mirror"])
	}
	status, err := readPersistedContextStatus(filepath.Join(application.configDir, "context-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if item, ok := exactStatusRepository(status, "mirror"); !ok || item.AcceptedCommit != outcome.AcceptedCommit || item.CandidateCommit != outcome.CandidateCommit {
		t.Fatalf("accepted mirror status was not durable at crash checkpoint: %#v", item)
	}
	if mirror.rollbackCalls != 0 || mirror.acknowledgeCalls != 0 || mirror.finalizeCalls != 0 {
		t.Fatalf("accepted transaction was changed after crash: finalize=%d rollback=%d acknowledge=%d", mirror.finalizeCalls, mirror.rollbackCalls, mirror.acknowledgeCalls)
	}

	application.afterStatusBarrier = nil
	mirror.recovery = gitops.MirrorRecovery{
		Pending: true, TransactionID: "transaction", ActiveGeneration: "candidate",
		BaselineCommit: baselineCommit, BaselineTree: baselineTree,
		CandidateCommit: outcome.CandidateCommit, CandidateTree: outcome.CandidateTree,
		BaselineWasAccepted: true,
	}
	mirror.syncResult = gitops.MirrorSyncResult{Outcome: outcome}
	if err := application.syncAll(context.Background(), false, "mirror"); err != nil {
		t.Fatalf("accepted mirror transaction did not recover: %v", err)
	}
	updated, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	repository := updated.Repositories["mirror"]
	if repository.PendingCompletion != nil || repository.AcceptedCommit != outcome.AcceptedCommit || repository.AcceptedTree != outcome.AcceptedTree || repository.Failure != nil {
		t.Fatalf("recovered mirror state = %#v", repository)
	}
	if mirror.finalizeCalls != 1 || mirror.rollbackCalls != 0 || mirror.acknowledgeCalls != 0 || mirror.syncCalls != 2 {
		t.Fatalf("recovered mirror transaction calls: finalize=%d rollback=%d acknowledge=%d sync=%d", mirror.finalizeCalls, mirror.rollbackCalls, mirror.acknowledgeCalls, mirror.syncCalls)
	}
}

func TestMirrorStatusBarrierFailureRollsBackAndRetainsRecoveryJournal(t *testing.T) {
	application, _ := testApplication(t)
	application.reconcileRuntime = func(state.Repository, configuredRepository) error { return nil }
	entry := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/main", config.ModeMirror, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	baselineCommit := strings.Repeat("e", 40)
	baselineTree := strings.Repeat("f", 40)
	machineState := state.New()
	machineState.Repositories["mirror"] = state.Repository{
		ID: "mirror", Mode: "mirror", Path: stableMirrorPointerTo(t, canonicalTestPath(t, t.TempDir())), ValidationRuntime: validStatusRuntime("example.source"),
		AcceptedCommit: baselineCommit, AcceptedTree: baselineTree,
	}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	outcome := successfulTestOutcome()
	mirror := &transactionalMirrorFake{
		syncResult:      gitops.MirrorSyncResult{Outcome: outcome, TransactionID: "transaction"},
		rollbackOutcome: gitops.Outcome{AcceptedCommit: baselineCommit, AcceptedTree: baselineTree, CandidateCommit: outcome.CandidateCommit, CandidateTree: outcome.CandidateTree},
	}
	application.mirror = mirror
	application.writeContextStatus = func(path string, data []byte, mode os.FileMode) error {
		corrupted := bytes.Replace(data, []byte(contextStatusProtocol), []byte("repo-sync.context-status.invalid"), 1)
		return atomicfile.Write(path, corrupted, mode)
	}
	if err := application.syncAll(context.Background(), false, "mirror"); err == nil {
		t.Fatal("mirror sync crossed a corrupted shared-status barrier")
	}
	updated, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	repository := updated.Repositories["mirror"]
	if repository.AcceptedCommit != baselineCommit || repository.AcceptedTree != baselineTree || repository.Failure == nil || repository.Failure.Code != "REPO-MIRROR-RECOVERY-ROLLBACK" {
		t.Fatalf("rollback state = %#v", repository)
	}
	if mirror.rollbackCalls != 1 || mirror.acknowledgeCalls != 0 {
		t.Fatalf("failed rollback completion discarded recovery evidence: rollback=%d acknowledge=%d", mirror.rollbackCalls, mirror.acknowledgeCalls)
	}
}

func TestMirrorRecoveryFinalizesCandidateProvenByExistingStatus(t *testing.T) {
	application, _ := testApplication(t)
	application.reconcileRuntime = func(state.Repository, configuredRepository) error { return nil }
	entry := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/main", config.ModeMirror, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	path := stableMirrorPointerTo(t, canonicalTestPath(t, t.TempDir()))
	outcome := successfulTestOutcome()
	candidateState := state.New()
	candidateState.Repositories["mirror"] = state.Repository{
		ID: "mirror", Mode: "mirror", Path: path, ValidationRuntime: validStatusRuntime("example.source"),
		AcceptedCommit: outcome.AcceptedCommit, AcceptedTree: outcome.AcceptedTree, CandidateCommit: outcome.CandidateCommit, CandidateTree: outcome.CandidateTree,
		LastAttempt: application.now(), LastSuccess: application.now(), LastSync: application.now(), Validation: validationState(outcome.Validation, outcome.CandidateTree, application.now()),
	}
	if err := state.Save(application.statePath, candidateState); err != nil {
		t.Fatal(err)
	}
	if _, err := application.refreshContextStatus("running"); err != nil {
		t.Fatal(err)
	}
	baselineCommit := strings.Repeat("e", 40)
	baselineTree := strings.Repeat("f", 40)
	lagging := candidateState
	lagging.Repositories = map[string]state.Repository{"mirror": {
		ID: "mirror", Mode: "mirror", Path: path, ValidationRuntime: validStatusRuntime("example.source"),
		AcceptedCommit: baselineCommit, AcceptedTree: baselineTree,
		PendingCompletion: &state.PendingCompletion{
			AcceptedCommit: outcome.AcceptedCommit, AcceptedTree: outcome.AcceptedTree,
			CandidateCommit: outcome.CandidateCommit, CandidateTree: outcome.CandidateTree,
			Validation: validationState(outcome.Validation, outcome.CandidateTree, application.now()), CompletedAt: application.now(),
		},
	}}
	if err := state.Save(application.statePath, lagging); err != nil {
		t.Fatal(err)
	}
	mirror := &transactionalMirrorFake{
		recovery:   gitops.MirrorRecovery{Pending: true, TransactionID: "recovery", ActiveGeneration: "candidate", BaselineCommit: baselineCommit, BaselineTree: baselineTree, CandidateCommit: outcome.CandidateCommit, CandidateTree: outcome.CandidateTree, BaselineWasAccepted: true},
		syncResult: gitops.MirrorSyncResult{Outcome: outcome},
	}
	application.mirror = mirror
	if err := application.syncAll(context.Background(), false, "mirror"); err != nil {
		t.Fatal(err)
	}
	updated, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	repository := updated.Repositories["mirror"]
	if repository.AcceptedCommit != outcome.AcceptedCommit || repository.AcceptedTree != outcome.AcceptedTree || repository.Failure != nil {
		t.Fatalf("authoritative candidate status did not repair private state: %#v", repository)
	}
	if mirror.finalizeCalls != 1 || mirror.rollbackCalls != 0 || mirror.syncCalls != 1 {
		t.Fatalf("candidate recovery calls: finalize=%d rollback=%d sync=%d", mirror.finalizeCalls, mirror.rollbackCalls, mirror.syncCalls)
	}
}

func TestMirrorRecoveryRollsBackWhenExistingStatusDoesNotAcceptCandidate(t *testing.T) {
	application, _ := testApplication(t)
	application.reconcileRuntime = func(state.Repository, configuredRepository) error { return nil }
	entry := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/main", config.ModeMirror, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	baselineCommit := strings.Repeat("e", 40)
	baselineTree := strings.Repeat("f", 40)
	candidateCommit := strings.Repeat("b", 40)
	candidateTree := strings.Repeat("a", 40)
	machineState := state.New()
	machineState.Repositories["mirror"] = state.Repository{
		ID: "mirror", Mode: "mirror", Path: stableMirrorPointerTo(t, canonicalTestPath(t, t.TempDir())), ValidationRuntime: validStatusRuntime("example.source"),
		AcceptedCommit: baselineCommit, AcceptedTree: baselineTree,
	}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	if _, err := application.refreshContextStatus("running"); err != nil {
		t.Fatal(err)
	}
	mirror := &transactionalMirrorFake{
		recovery:        gitops.MirrorRecovery{Pending: true, TransactionID: "recovery", ActiveGeneration: "candidate", BaselineCommit: baselineCommit, BaselineTree: baselineTree, CandidateCommit: candidateCommit, CandidateTree: candidateTree, BaselineWasAccepted: true},
		rollbackOutcome: gitops.Outcome{AcceptedCommit: baselineCommit, AcceptedTree: baselineTree, CandidateCommit: candidateCommit, CandidateTree: candidateTree},
	}
	application.mirror = mirror
	if err := application.syncAll(context.Background(), false, "mirror"); err == nil {
		t.Fatal("recovery rollback was not surfaced")
	}
	updated, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	repository := updated.Repositories["mirror"]
	if repository.AcceptedCommit != baselineCommit || repository.AcceptedTree != baselineTree || repository.Failure == nil || repository.Failure.Code != "REPO-MIRROR-RECOVERY-ROLLBACK" {
		t.Fatalf("recovered baseline state = %#v", repository)
	}
	if mirror.rollbackCalls != 1 || mirror.acknowledgeCalls != 1 || mirror.syncCalls != 0 {
		t.Fatalf("rollback recovery calls: rollback=%d acknowledge=%d sync=%d", mirror.rollbackCalls, mirror.acknowledgeCalls, mirror.syncCalls)
	}
}

func TestReconcileValidationSourceRequiresExactRegistryAndManifestIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mirror pointer fixture uses a POSIX symbolic link")
	}
	generationPath := t.TempDir()
	var err error
	generationPath, err = canonicalRepositoryRoot(generationPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(generationPath, ".agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "contract_version: 2.0.0\nsource_id: example.source\npublication:\n  mode: repo-sync\n  canonical_branch: main\n"
	if err := os.WriteFile(filepath.Join(generationPath, ".agents", "context-source.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	repositoryPath := stableMirrorPointerTo(t, generationPath)
	spec, err := discovery.ParseGitHubBranchURL("https://github.com/owner/repo/tree/main")
	if err != nil {
		t.Fatal(err)
	}
	repository := configuredRepository{
		Config: config.StructuredRepository("mirror", spec.OriginalURL, config.ModeMirror, "example.source"),
		Spec:   spec, ID: "mirror",
	}
	style := "posix"
	if runtime.GOOS == "windows" {
		style = "windows"
	}
	validRegistration := func() registryRegistration {
		return registryRegistration{
			SourceID: "example.source", Role: "context-mirror", Enabled: true,
			RootPath: registryLocalPath{Style: style, Value: repositoryPath},
			Git:      &registryGit{RemoteName: "origin", RemoteURL: "https://github.com/owner/repo.git", Branch: "main"},
			RepoSync: &registryRepoSync{RepositoryID: "mirror", Mode: "mirror"},
		}
	}
	encode := func(t *testing.T, registration registryRegistration) []byte {
		t.Helper()
		data, err := json.Marshal(registrySnapshot{SchemaVersion: contextRegistrySchema, ContractVersion: "2.0.0", RegistryRevision: 1, Registrations: []registryRegistration{registration}})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if err := reconcileValidationSource(encode(t, validRegistration()), repository, repositoryPath); err != nil {
		t.Fatalf("valid registration was rejected: %v", err)
	}
	legacyRegistry, err := json.Marshal(registrySnapshot{SchemaVersion: 1, ContractVersion: "2.0.0", RegistryRevision: 1, Registrations: []registryRegistration{validRegistration()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileValidationSource(legacyRegistry, repository, repositoryPath); err == nil {
		t.Fatal("v1 registry handshake was accepted")
	}
	manifestPath := filepath.Join(generationPath, ".agents", "context-source.yaml")
	manualReview := "contract_version: 2.0.0\nsource_id: example.source\npublication:\n  mode: manual-review\n  canonical_branch: main\n"
	if err := os.WriteFile(manifestPath, []byte(manualReview), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reconcileValidationSource(encode(t, validRegistration()), repository, repositoryPath); err != nil {
		t.Fatalf("manual-review mirror was rejected: %v", err)
	}

	tests := map[string]func(*registryRegistration){
		"disabled":      func(value *registryRegistration) { value.Enabled = false },
		"role":          func(value *registryRegistration) { value.Role = "development" },
		"repository id": func(value *registryRegistration) { value.RepoSync.RepositoryID = "other" },
		"mode":          func(value *registryRegistration) { value.RepoSync.Mode = "publisher" },
		"remote name":   func(value *registryRegistration) { value.Git.RemoteName = "upstream" },
		"remote URL":    func(value *registryRegistration) { value.Git.RemoteURL = "https://github.com/owner/other.git" },
		"branch":        func(value *registryRegistration) { value.Git.Branch = "feature" },
		"root":          func(value *registryRegistration) { value.RootPath.Value = filepath.Dir(repositoryPath) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			registration := validRegistration()
			mutate(&registration)
			if err := reconcileValidationSource(encode(t, registration), repository, repositoryPath); err == nil {
				t.Fatal("mismatched registration was accepted")
			}
		})
	}

	publisher := repository
	publisher.ID = "publisher"
	publisher.Config = config.StructuredRepository("publisher", spec.OriginalURL, config.ModePublish, "example.source")
	registration := validRegistration()
	registration.Role = "writable"
	registration.RepoSync.RepositoryID = "publisher"
	registration.RepoSync.Mode = "publisher"
	registration.RootPath.Value = generationPath
	if err := reconcileValidationSource(encode(t, registration), publisher, generationPath); err == nil {
		t.Fatal("publisher accepted manual-review publication")
	}
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reconcileValidationSource(encode(t, registration), publisher, generationPath); err != nil {
		t.Fatalf("valid publisher registration was rejected: %v", err)
	}
}

func TestReconcileValidationSourceRejectsSymlinkRootAndManifestDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliably available on Windows test hosts")
	}
	parent := canonicalTestPath(t, t.TempDir())
	generationPath := filepath.Join(parent, "repository")
	if err := os.MkdirAll(filepath.Join(generationPath, ".agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	var err error
	generationPath, err = canonicalRepositoryRoot(generationPath)
	if err != nil {
		t.Fatal(err)
	}
	repositoryPath := stableMirrorPointerTo(t, generationPath)
	manifestPath := filepath.Join(generationPath, ".agents", "context-source.yaml")
	if err := os.WriteFile(manifestPath, []byte("contract_version: 2.0.0\nsource_id: example.source\npublication:\n  mode: manual\n  canonical_branch: main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := discovery.ParseGitHubBranchURL("https://github.com/owner/repo/tree/main")
	if err != nil {
		t.Fatal(err)
	}
	repository := configuredRepository{Config: config.StructuredRepository("mirror", spec.OriginalURL, config.ModeMirror, "example.source"), Spec: spec, ID: "mirror"}
	registration := registryRegistration{
		SourceID: "example.source", Role: "context-mirror", Enabled: true,
		RootPath: registryLocalPath{Style: "posix", Value: repositoryPath},
		Git:      &registryGit{RemoteName: "origin", RemoteURL: "https://github.com/owner/repo.git", Branch: "main"},
		RepoSync: &registryRepoSync{RepositoryID: "mirror", Mode: "mirror"},
	}
	data, err := json.Marshal(registrySnapshot{SchemaVersion: contextRegistrySchema, ContractVersion: "2.0.0", RegistryRevision: 1, Registrations: []registryRegistration{registration}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileValidationSource(data, repository, repositoryPath); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("manifest drift error = %v", err)
	}
	if err := os.WriteFile(manifestPath, []byte("contract_version: 2.0.0\nsource_id: example.source\npublication:\n  mode: repo-sync\n  canonical_branch: main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registration.RootPath.Value = generationPath
	data, err = json.Marshal(registrySnapshot{SchemaVersion: contextRegistrySchema, ContractVersion: "2.0.0", RegistryRevision: 1, Registrations: []registryRegistration{registration}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileValidationSource(data, repository, repositoryPath); err == nil {
		t.Fatal("real mirror directory registry root was accepted")
	}
}

func TestStableRuntimeFileRejectsOversizedArtifact(t *testing.T) {
	directory := t.TempDir()
	repositoryPath := filepath.Join(directory, "repository")
	if err := os.Mkdir(repositoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(directory, "registry.json")
	file, err := os.OpenFile(artifact, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxRuntimeJSONBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := stableRuntimeFile(artifact, repositoryPath, true, maxRuntimeJSONBytes); err == nil {
		t.Fatal("oversized runtime artifact was accepted")
	}
}

func TestContextStatusRejectsLegacyValidationRuntime(t *testing.T) {
	entry := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	repositories, err := configuredRepositories(config.Config{Interval: "5m", Repositories: []config.Repository{entry}}, "")
	if err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	runtimeState := validStatusRuntime("example.source")
	runtimeState.Protocol = "contextctl.report.v1"
	machineState.Repositories["mirror"] = state.Repository{ID: "mirror", Mode: "mirror", Path: t.TempDir(), ValidationRuntime: runtimeState}
	if _, err := buildContextStatus(config.Config{Interval: "5m", Repositories: []config.Repository{entry}}, machineState, repositories, "running", time.Now(), nil); err == nil {
		t.Fatal("v1 validation runtime was accepted into the v2 context-status handshake")
	}
}

func TestStartRefusesLegacyRepositoryUntilMigrated(t *testing.T) {
	application, _ := testApplication(t)
	entry := config.LegacyRepository("https://github.com/owner/repo/tree/main")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	legacyKey, err := entry.LegacyStateKey()
	if err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.Repositories[legacyKey] = state.Repository{Path: t.TempDir()}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	err = application.start()
	if err == nil || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("start error = %v", err)
	}
}

func TestEnableServiceDoesNotEnableWhenInstallFails(t *testing.T) {
	service := &fakeService{statusErr: background.ErrNotInstalled, installErr: errors.New("access denied")}
	application := serviceTestApplication(t, service)
	err := application.enableService()
	if err == nil || !strings.Contains(err.Error(), "install service: access denied") {
		t.Fatalf("enableService() error = %v", err)
	}
	if service.startCalls != 0 {
		t.Fatalf("Start() calls = %d, want 0", service.startCalls)
	}
	assertEnabled(t, application.statePath, false)
}

func TestEnableServiceRollsBackEnabledWhenStartFails(t *testing.T) {
	service := &fakeService{status: background.StatusStopped, startErr: errors.New("could not run task")}
	application := serviceTestApplication(t, service)
	err := application.enableService()
	if err == nil || !strings.Contains(err.Error(), "start service: could not run task") {
		t.Fatalf("enableService() error = %v", err)
	}
	if service.startCalls != 1 {
		t.Fatalf("Start() calls = %d, want 1", service.startCalls)
	}
	assertEnabled(t, application.statePath, false)
}

func TestEnableServiceEnablesAfterServiceIsReady(t *testing.T) {
	service := &fakeService{status: background.StatusStopped}
	application := serviceTestApplication(t, service)
	if err := application.enableService(); err != nil {
		t.Fatal(err)
	}
	if service.startCalls != 1 {
		t.Fatalf("Start() calls = %d, want 1", service.startCalls)
	}
	assertEnabled(t, application.statePath, true)
}

func TestEnableServiceRestartsLingeringDisabledService(t *testing.T) {
	service := &fakeService{status: background.StatusRunning}
	application := serviceTestApplication(t, service)
	if err := application.enableService(); err != nil {
		t.Fatal(err)
	}
	if service.stopCalls != 1 || service.startCalls != 1 {
		t.Fatalf("Stop()/Start() calls = %d/%d, want 1/1", service.stopCalls, service.startCalls)
	}
	assertEnabled(t, application.statePath, true)
}

func TestEnableServiceLeavesDisabledWhenLingeringServiceCannotStop(t *testing.T) {
	service := &fakeService{status: background.StatusRunning, stopErr: errors.New("still stopping")}
	application := serviceTestApplication(t, service)
	err := application.enableService()
	if err == nil || !strings.Contains(err.Error(), "stop disabled service: still stopping") {
		t.Fatalf("enableService() error = %v", err)
	}
	if service.startCalls != 0 {
		t.Fatalf("Start() calls = %d, want 0", service.startCalls)
	}
	assertEnabled(t, application.statePath, false)
}

func serviceTestApplication(t *testing.T, service background.Controller) *Application {
	t.Helper()
	application, _ := testApplication(t)
	if err := state.Save(application.statePath, state.New()); err != nil {
		t.Fatal(err)
	}
	application.serviceFactory = func() (background.Controller, error) { return service, nil }
	return application
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

func TestEarlySyncFailureDurablyUpdatesLastAttempt(t *testing.T) {
	application, _ := testApplication(t)
	entry := config.StructuredRepository("missing", "https://github.com/owner/repo/tree/main", config.ModeMirror, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	if err := application.syncAll(context.Background(), false, "missing"); err == nil {
		t.Fatal("sync unexpectedly succeeded")
	}
	machineState, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	entryState := machineState.Repositories["missing"]
	if entryState.LastAttempt.IsZero() || !entryState.LastAttempt.Equal(application.now()) || entryState.Failure == nil {
		t.Fatalf("early failure state = %#v", entryState)
	}
}

func TestFailureNotificationIsDeduplicatedAndUnavailableDeliveryIsDurable(t *testing.T) {
	application, _ := testApplication(t)
	application.reconcileRuntime = func(state.Repository, configuredRepository) error { return nil }
	entry := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.Repositories["mirror"] = state.Repository{
		ID: "mirror", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()),
		ValidationRuntime: validStatusRuntime("example.source"),
	}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	engine := staticFailureEngine{}
	notifier := &countingNotifier{delivery: notify.DeliveryUnavailable}
	application.publisher = engine
	application.mirror = engine
	application.notifier = notifier
	for attempt := 0; attempt < 2; attempt++ {
		if err := application.syncAll(context.Background(), false, "mirror"); err == nil {
			t.Fatal("failing engine unexpectedly succeeded")
		}
	}
	if notifier.calls != 1 {
		t.Fatalf("notification calls = %d, want 1", notifier.calls)
	}
	machineState, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	repository := machineState.Repositories["mirror"]
	if repository.Notification.Status != string(notify.DeliveryUnavailable) || repository.Notification.FailureKey == "" || repository.Failure == nil {
		t.Fatalf("durable failure notification state = %#v", repository)
	}
}

func TestStalePendingNotificationIsRetriedWithNewVersion(t *testing.T) {
	application, _ := testApplication(t)
	entry := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	outcome, failure := staticFailureEngine{}.Sync(context.Background(), gitops.Target{})
	fingerprint := failureFingerprint("mirror", failure, outcome.CandidateCommit, outcome.CandidateTree)
	machineState := state.New()
	machineState.Repositories["mirror"] = state.Repository{
		ID: "mirror", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), ValidationRuntime: validStatusRuntime("example.source"),
		Notification: state.Notification{FailureKey: fingerprint, Status: "pending", AttemptedAt: application.now().Add(-notificationPendingRetry), Version: 7},
	}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	notifier := &countingNotifier{delivery: notify.DeliveryUnavailable}
	application.publisher = staticFailureEngine{}
	application.mirror = staticFailureEngine{}
	application.notifier = notifier
	if err := application.syncAll(context.Background(), false, "mirror"); err == nil {
		t.Fatal("failing engine unexpectedly succeeded")
	}
	updated, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	notification := updated.Repositories["mirror"].Notification
	if notifier.calls != 1 || notification.Version != 8 || notification.Status != string(notify.DeliveryUnavailable) {
		t.Fatalf("retry state/calls = %#v %d", notification, notifier.calls)
	}
}

func TestNotificationCompletionCASPreservesNewerPendingFailure(t *testing.T) {
	application, _ := testApplication(t)
	entry := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/main", config.ModePublish, "example.source")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.Repositories["mirror"] = state.Repository{ID: "mirror", Mode: "publish", Path: canonicalTestPath(t, t.TempDir()), ValidationRuntime: validStatusRuntime("example.source")}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	notifier := &countingNotifier{delivery: notify.DeliverySent}
	notifier.onNotify = func() {
		if err := state.Update(application.statePath, func(current *state.State) error {
			repository := current.Repositories["mirror"]
			repository.Notification = state.Notification{FailureKey: "newer-failure", Status: "pending", AttemptedAt: application.now(), Version: repository.Notification.Version + 1}
			current.Repositories["mirror"] = repository
			return nil
		}); err != nil {
			t.Errorf("inject newer notification: %v", err)
		}
	}
	application.publisher = staticFailureEngine{}
	application.mirror = staticFailureEngine{}
	application.notifier = notifier
	if err := application.syncAll(context.Background(), false, "mirror"); err == nil {
		t.Fatal("failing engine unexpectedly succeeded")
	}
	updated, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	notification := updated.Repositories["mirror"].Notification
	if notification.FailureKey != "newer-failure" || notification.Status != "pending" || notification.Version != 2 {
		t.Fatalf("newer notification was overwritten: %#v", notification)
	}
}

type recordingEngine struct {
	targets []gitops.Target
	onSync  func()
}

func (e *recordingEngine) Sync(_ context.Context, target gitops.Target) (gitops.Outcome, *gitops.OperationError) {
	if e.onSync != nil {
		e.onSync()
	}
	e.targets = append(e.targets, target)
	return successfulTestOutcome(), nil
}

type reconcileBoundaryEngine struct {
	boundary chan struct{}
	proceed  chan struct{}
}

func (e *reconcileBoundaryEngine) Sync(ctx context.Context, target gitops.Target) (gitops.Outcome, *gitops.OperationError) {
	if target.Reconcile == nil {
		return gitops.Outcome{}, &gitops.OperationError{Code: "REPO-TEST-RECONCILE", Phase: "publish", Summary: "reconcile callback is unavailable"}
	}
	if err := target.Reconcile(ctx); err != nil {
		return gitops.Outcome{}, &gitops.OperationError{Code: "REPO-TEST-RECONCILE", Phase: "publish", Summary: err.Error()}
	}
	close(e.boundary)
	select {
	case <-ctx.Done():
		return gitops.Outcome{}, &gitops.OperationError{Code: "REPO-TEST-CANCELLED", Phase: "publish", Summary: "test operation was cancelled"}
	case <-e.proceed:
	}
	return successfulTestOutcome(), nil
}

func successfulTestOutcome() gitops.Outcome {
	tree := strings.Repeat("a", 40)
	commit := strings.Repeat("b", 40)
	return gitops.Outcome{
		AcceptedCommit: commit, AcceptedTree: tree, CandidateCommit: commit, CandidateTree: tree,
		Validation: validation.Report{
			SchemaVersion: validation.ReportSchemaVersion, Protocol: validation.ReportProtocol, ContractVersion: "2.0.0", Command: "validate",
			GeneratedAt: "2026-09-19T00:00:00Z", Verdict: validation.VerdictPass, Promotable: true,
			Checks:           []validation.Check{{ID: "CTX-SCHEMA-PASS", Status: "pass", Paths: []string{}}},
			ValidatorResults: []validation.ValidatorResult{{ContractID: "example.validator", ContractVersion: "1.0.0", Disposition: "pass", CheckIDs: []string{"CTX-SCHEMA-PASS"}}},
		},
	}
}

type staticFailureEngine struct{}

func (staticFailureEngine) Sync(context.Context, gitops.Target) (gitops.Outcome, *gitops.OperationError) {
	tree := strings.Repeat("c", 40)
	commit := strings.Repeat("d", 40)
	return gitops.Outcome{CandidateCommit: commit, CandidateTree: tree}, &gitops.OperationError{
		Code: "REPO-VALIDATION-FAILED", Phase: "validation", Summary: "candidate context validation failed",
		Findings: []validation.Finding{{CheckID: "CTX-SCHEMA-FAIL", Path: "skills/example/SKILL.md"}},
	}
}

type transactionalMirrorFake struct {
	syncResult         gitops.MirrorSyncResult
	syncFailure        *gitops.OperationError
	recovery           gitops.MirrorRecovery
	recoveryFailure    *gitops.OperationError
	rollbackOutcome    gitops.Outcome
	rollbackFailure    *gitops.OperationError
	acknowledgeFailure *gitops.OperationError
	onFinalize         func(gitops.Outcome) *gitops.OperationError
	syncCalls          int
	finalizeCalls      int
	rollbackCalls      int
	acknowledgeCalls   int
}

func (m *transactionalMirrorFake) Sync(ctx context.Context, target gitops.Target) (gitops.Outcome, *gitops.OperationError) {
	result, failure := m.SyncTransaction(ctx, target)
	return result.Outcome, failure
}

func (m *transactionalMirrorFake) SyncTransaction(context.Context, gitops.Target) (gitops.MirrorSyncResult, *gitops.OperationError) {
	m.syncCalls++
	return m.syncResult, m.syncFailure
}

func (m *transactionalMirrorFake) InspectRecovery(context.Context, gitops.Target) (gitops.MirrorRecovery, *gitops.OperationError) {
	return m.recovery, m.recoveryFailure
}

func (m *transactionalMirrorFake) Finalize(_ context.Context, _ gitops.Target, outcome gitops.Outcome, _ string) *gitops.OperationError {
	m.finalizeCalls++
	if m.onFinalize != nil {
		return m.onFinalize(outcome)
	}
	return nil
}

func (m *transactionalMirrorFake) Rollback(context.Context, gitops.Target, gitops.Outcome, string) (gitops.Outcome, *gitops.OperationError) {
	m.rollbackCalls++
	return m.rollbackOutcome, m.rollbackFailure
}

func (m *transactionalMirrorFake) AcknowledgeRollback(context.Context, gitops.Target, string) *gitops.OperationError {
	m.acknowledgeCalls++
	return m.acknowledgeFailure
}

type countingNotifier struct {
	delivery notify.Delivery
	calls    int
	onNotify func()
}

func (n *countingNotifier) Notify(context.Context, notify.Message) notify.Delivery {
	n.calls++
	if n.onNotify != nil {
		n.onNotify()
	}
	return n.delivery
}

type quietNotifier struct{}

func (quietNotifier) Notify(context.Context, notify.Message) notify.Delivery {
	return notify.DeliverySent
}

func validStatusRuntime(sourceID string) state.ValidationRuntime {
	digest := "sha256:" + strings.Repeat("a", 64)
	return state.ValidationRuntime{
		Protocol: validation.ReportProtocol, SourceID: sourceID, ContextctlDigest: digest,
		RegistryRevision: 1, RegistryDigest: digest, TrustStateRevision: 0, TrustStateDigest: digest,
	}
}

func canonicalTestPath(t *testing.T, path string) string {
	t.Helper()
	canonical, err := canonicalRepositoryRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func stableMirrorPointerTo(t *testing.T, target string) string {
	t.Helper()
	parent := canonicalTestPath(t, t.TempDir())
	path := filepath.Join(parent, "mirror-current")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("create stable mirror test pointer: %v", err)
	}
	return path
}

func runAppGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func trustedGitRegistryPath(t *testing.T) string {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitPath, err = filepath.EvalSymlinks(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	gitPath, err = filepath.Abs(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	style := "posix"
	if runtime.GOOS == "windows" {
		style = "windows"
	}
	registry, err := json.Marshal(map[string]any{
		"schema_version": 2, "contract_version": "2.0.0", "registry_revision": 1,
		"local_dependencies": []any{map[string]any{
			"id": "context-system.git", "kind": "executable",
			"path": map[string]any{"style": style, "value": gitPath}, "sha256": hex.EncodeToString(digest[:]),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	directory := canonicalTestPath(t, t.TempDir())
	path := filepath.Join(directory, "registry.json")
	if err := os.WriteFile(path, registry, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
