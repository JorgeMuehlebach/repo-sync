package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/JorgeMuehlebach/repo-sync/internal/background"
	"github.com/JorgeMuehlebach/repo-sync/internal/config"
	"github.com/JorgeMuehlebach/repo-sync/internal/notify"
	"github.com/JorgeMuehlebach/repo-sync/internal/state"
	"github.com/JorgeMuehlebach/repo-sync/internal/validation"
)

type fakeMaintainer struct {
	planReport  validation.MaintenanceReport
	planErr     error
	applyReport validation.MaintenanceReport
	applyErr    error
	planCalls   int
	applyCalls  int
	requests    []validation.MaintenanceRequest
}

func (f *fakeMaintainer) PlanManagedSkillLinks(_ context.Context, request validation.MaintenanceRequest) (validation.MaintenanceReport, error) {
	f.planCalls++
	f.requests = append(f.requests, request)
	return f.planReport, f.planErr
}

func (f *fakeMaintainer) ApplyManagedSkillLinks(_ context.Context, request validation.MaintenanceRequest) (validation.MaintenanceReport, error) {
	f.applyCalls++
	f.requests = append(f.requests, request)
	return f.applyReport, f.applyErr
}

func appMaintenanceReport(verdict validation.Verdict) validation.MaintenanceReport {
	status := "pass"
	check := "CTX-LINK-PLAN-VALID"
	if verdict == validation.VerdictHold {
		status = "hold"
		check = "CTX-LINK-AUTOMATIC-REVIEW-REQUIRED"
	}
	return validation.MaintenanceReport{
		SchemaVersion: validation.ReportSchemaVersion, Protocol: validation.ReportProtocol,
		ContractVersion: "2.1.0", Command: "links-apply", GeneratedAt: "2026-10-11T00:00:00Z",
		Verdict: verdict, Checks: []validation.Check{{ID: check, Status: status, Paths: []string{}}},
		ValidatorResults: []validation.ValidatorResult{}, LinkActions: []validation.LinkAction{},
	}
}

func prepareMaintenanceApplication(t *testing.T, enabled bool) (*Application, *fakeMaintainer) {
	t.Helper()
	application, _ := testApplication(t)
	application.serviceFactory = func() (background.Controller, error) { return &fakeService{status: background.StatusStopped}, nil }
	entry := config.StructuredRepository("mirror", "https://github.com/owner/repo/tree/main", config.ModeMirror, "example.context")
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: []config.Repository{entry}}); err != nil {
		t.Fatal(err)
	}
	machineState := state.New()
	machineState.ManagedSkillLinks = enabled
	runtimeState := validStatusRuntime("example.context")
	mirrorPath := stableMirrorPointerTo(t, canonicalTestPath(t, t.TempDir()))
	machineState.Repositories["mirror"] = state.Repository{
		ID: "mirror", Mode: "mirror", Path: mirrorPath, Revision: 1,
		ValidationRuntime: runtimeState, AcceptedCommit: strings.Repeat("a", 40), AcceptedTree: strings.Repeat("b", 40), CandidateCommit: strings.Repeat("a", 40), CandidateTree: strings.Repeat("b", 40),
		Validation: state.Validation{
			ContractVersion: "2.1.0", ObjectID: strings.Repeat("b", 40), Verdict: "pass", CheckIDs: []string{"CTX-SCHEMA-PASS"}, CheckedAt: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC),
			ValidatorResults: []state.ValidatorResult{{ContractID: "example.validator", ContractVersion: "1.0.0", Disposition: "pass", CheckIDs: []string{"CTX-SCHEMA-PASS"}, SourceID: "example.context", SourceRole: "context-mirror"}},
		},
	}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	maintainer := &fakeMaintainer{planReport: appMaintenanceReport(validation.VerdictPass), applyReport: appMaintenanceReport(validation.VerdictPass)}
	maintainer.planReport.Command = "links-plan"
	application.maintainer = maintainer
	return application, maintainer
}

func TestManagedSkillMaintenanceIsOptInAndHealthyRunsAreQuiet(t *testing.T) {
	application, maintainer := prepareMaintenanceApplication(t, false)
	application.runManagedSkillMaintenance(context.Background())
	if maintainer.applyCalls != 0 {
		t.Fatalf("disabled maintenance calls = %d", maintainer.applyCalls)
	}

	machineState, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	machineState.ManagedSkillLinks = true
	machineState.Maintenance.Failure = &state.Failure{Code: maintenanceCodeHeld, Phase: "maintenance", Summary: "old", OccurredAt: time.Now()}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	application.runManagedSkillMaintenance(context.Background())
	if maintainer.applyCalls != 1 {
		t.Fatalf("enabled maintenance calls = %d", maintainer.applyCalls)
	}
	machineState, err = state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if machineState.Maintenance.Failure != nil || machineState.Maintenance.LastSuccess.IsZero() {
		t.Fatalf("maintenance state = %#v", machineState.Maintenance)
	}
}

func TestManagedSkillMaintenanceFailureNotificationIsDeduplicated(t *testing.T) {
	application, maintainer := prepareMaintenanceApplication(t, true)
	maintainer.applyErr = &validation.ProtocolError{Code: validation.CodeUnsupported}
	notifier := &countingNotifier{delivery: notify.DeliverySent}
	application.notifier = notifier
	application.runManagedSkillMaintenance(context.Background())
	application.runManagedSkillMaintenance(context.Background())
	if notifier.calls != 1 {
		t.Fatalf("notification calls = %d, want 1", notifier.calls)
	}
	machineState, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if machineState.Maintenance.Failure == nil || machineState.Maintenance.Failure.Code != maintenanceCodeProtocol {
		t.Fatalf("maintenance failure = %#v", machineState.Maintenance.Failure)
	}
}

func TestManagedSkillMaintenanceDefersQuietlyDuringRepositoryOperation(t *testing.T) {
	application, maintainer := prepareMaintenanceApplication(t, true)
	release, err := application.acquireLock("mirror")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	application.runManagedSkillMaintenance(context.Background())
	if maintainer.applyCalls != 0 {
		t.Fatalf("maintenance calls = %d, want 0", maintainer.applyCalls)
	}
	machineState, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if machineState.Maintenance.Failure != nil || !machineState.Maintenance.LastAttempt.IsZero() {
		t.Fatalf("deferred maintenance changed health: %#v", machineState.Maintenance)
	}
}

func TestMaintenanceEnableRequiresPassingAddOnlyPlan(t *testing.T) {
	application, maintainer := prepareMaintenanceApplication(t, false)
	if err := application.Run([]string{"maintenance", "enable"}); err != nil {
		t.Fatal(err)
	}
	machineState, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !machineState.ManagedSkillLinks || maintainer.planCalls != 1 {
		t.Fatalf("enabled/calls = %t/%d", machineState.ManagedSkillLinks, maintainer.planCalls)
	}

	if err := application.Run([]string{"maintenance", "disable"}); err != nil {
		t.Fatal(err)
	}
	maintainer.planReport = appMaintenanceReport(validation.VerdictHold)
	maintainer.planReport.Command = "links-plan"
	if err := application.Run([]string{"maintenance", "enable"}); err == nil {
		t.Fatal("maintenance enable accepted a held add-only plan")
	}
	machineState, _ = state.Load(application.statePath)
	if machineState.ManagedSkillLinks {
		t.Fatal("held preflight changed enrollment state")
	}
}

func TestRefreshValidationStagesAllRepositoriesAtomicallyAndIsIdempotent(t *testing.T) {
	application, _ := testApplication(t)
	application.serviceFactory = func() (background.Controller, error) { return &fakeService{status: background.StatusStopped}, nil }
	contextctlPath := filepath.Join(canonicalTestPath(t, t.TempDir()), "contextctl")
	if err := os.WriteFile(contextctlPath, []byte("contextctl 2.1"), 0o700); err != nil {
		t.Fatal(err)
	}
	trustPath := filepath.Join(filepath.Dir(contextctlPath), "trust.json")
	if err := os.WriteFile(trustPath, []byte(`{"schema_version":2,"contract_version":"2.1.0","state_revision":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	style := "posix"
	if runtime.GOOS == "windows" {
		style = "windows"
	}
	entries := []config.Repository{
		config.StructuredRepository("first", "https://github.com/owner/first/tree/main", config.ModePublish, "example.first"),
		config.StructuredRepository("second", "https://github.com/owner/second/tree/main", config.ModePublish, "example.second"),
	}
	registrations := make([]registryRegistration, 0, len(entries))
	machineState := state.New()
	for _, entry := range entries {
		root := canonicalTestPath(t, t.TempDir())
		if err := os.MkdirAll(filepath.Join(root, ".agents"), 0o700); err != nil {
			t.Fatal(err)
		}
		manifest := "contract_version: 2.1.0\nsource_id: " + entry.SourceID + "\npublication:\n  mode: repo-sync\n  canonical_branch: main\n"
		if err := os.WriteFile(filepath.Join(root, ".agents", "context-source.yaml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		registrations = append(registrations, registryRegistration{
			SourceID: entry.SourceID, Role: "writable", Enabled: true, RootPath: registryLocalPath{Style: style, Value: root},
			Git:      &registryGit{RemoteName: "origin", RemoteURL: "https://github.com/owner/" + entry.ID + ".git", Branch: "main"},
			RepoSync: &registryRepoSync{RepositoryID: entry.ID, Mode: "publisher"},
		})
		machineState.Repositories[entry.ID] = state.Repository{ID: entry.ID, Mode: "publish", Path: root, Revision: 7}
	}
	registryData, err := json.Marshal(registrySnapshot{SchemaVersion: 2, ContractVersion: "2.1.0", RegistryRevision: 3, Registrations: registrations})
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(filepath.Dir(contextctlPath), "registry.json")
	if err := os.WriteFile(registryPath, registryData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(application.configPath, config.Config{Interval: "5m", Repositories: entries}); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	options := setupOptions{RefreshValidation: true, Contextctl: contextctlPath, Registry: registryPath, TrustState: trustPath}
	if err := application.setup(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	first, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.setup(context.Background(), setupOptions{RefreshValidation: true}); err != nil {
		t.Fatal(err)
	}
	second, err := state.Load(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("idempotent refresh changed state\nfirst=%#v\nsecond=%#v", first, second)
	}

	before, err := os.ReadFile(application.statePath)
	if err != nil {
		t.Fatal(err)
	}
	brokenTrust := filepath.Join(filepath.Dir(contextctlPath), "broken-trust.json")
	if err := os.WriteFile(brokenTrust, []byte(`{"contract_version":"9.0.0","state_revision":4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err = application.setup(context.Background(), setupOptions{RefreshValidation: true, TrustState: brokenTrust})
	if err == nil {
		t.Fatal("batch refresh accepted an invalid staged dependency")
	}
	after, readErr := os.ReadFile(application.statePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed batch refresh changed machine state")
	}
}

func TestRefreshValidationRequiresStoppedService(t *testing.T) {
	application, _ := testApplication(t)
	application.serviceFactory = func() (background.Controller, error) { return &fakeService{status: background.StatusRunning}, nil }
	err := application.setup(context.Background(), setupOptions{RefreshValidation: true})
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("refresh error = %v", err)
	}
}
