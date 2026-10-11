package validation

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func validMaintenanceReport(t *testing.T, command string) MaintenanceReport {
	t.Helper()
	root := t.TempDir()
	report := MaintenanceReport{
		SchemaVersion:    ReportSchemaVersion,
		Protocol:         ReportProtocol,
		ContractVersion:  "2.1.0",
		Command:          command,
		GeneratedAt:      "2026-10-11T00:00:00Z",
		Verdict:          VerdictPass,
		Checks:           []Check{{ID: "CTX-LINK-PLAN-VALID", Status: "pass", Paths: []string{}}},
		ValidatorResults: []ValidatorResult{},
		LinkActions: []LinkAction{{
			Action: "noop", Ownership: "managed", SkillName: "context-system", SourceID: "example.context",
			LinkPath: filepath.Join(root, "managed", "context-system"), TargetPath: filepath.Join(root, "mirror", "context-system"),
		}},
	}
	if !filepath.IsAbs(report.LinkActions[0].LinkPath) || !filepath.IsAbs(report.LinkActions[0].TargetPath) {
		t.Fatalf("maintenance report fixture paths must be absolute: %#v", report.LinkActions[0])
	}
	return report
}

func maintenanceRequest(t *testing.T) MaintenanceRequest {
	t.Helper()
	request := testRequest(t)
	return MaintenanceRequest{
		Runtime:         request.Runtime,
		RepositoryPaths: []string{request.RepositoryPath},
		StatusPath:      filepath.Join(filepath.Dir(request.RepositoryPath), "context-status.json"),
	}
}

func TestMaintenanceRunnerUsesFixedAddOnlyInterfaceAndSuppressesChildOutput(t *testing.T) {
	request := maintenanceRequest(t)
	report := validMaintenanceReport(t, "links-apply")
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakeExecutor{raw: raw}
	got, err := (MaintenanceRunner{Executor: executor}).ApplyManagedSkillLinks(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{
		"links", "apply", "--add-only", "--format", "json",
		"--registry", executor.seenArgs[6], "--trust-state", executor.seenArgs[8],
		"--repo-sync-status", request.StatusPath,
	}
	if got.Verdict != VerdictPass || !reflect.DeepEqual(executor.seenArgs, wantArgs) {
		t.Fatalf("report/args = %#v %#v, want args %#v", got, executor.seenArgs, wantArgs)
	}
	if executor.executable == request.Runtime.ContextctlPath {
		t.Fatal("maintenance executed the mutable installed pathname instead of a verified snapshot")
	}
}

func TestMaintenanceRunnerRejectsMalformedUnsafeAndExitMismatchedReports(t *testing.T) {
	request := maintenanceRequest(t)
	unsafe := validMaintenanceReport(t, "links-apply")
	unsafe.LinkActions[0].Action = "remove"
	unsafeRaw, _ := json.Marshal(unsafe)
	heldWithAction := validMaintenanceReport(t, "links-apply")
	heldWithAction.Verdict = VerdictHold
	heldWithAction.Checks[0].Status = "hold"
	heldWithActionRaw, _ := json.Marshal(heldWithAction)
	missingTarget := validMaintenanceReport(t, "links-apply")
	missingTarget.LinkActions[0].TargetPath = ""
	missingTargetRaw, _ := json.Marshal(missingTarget)
	mismatch := validMaintenanceReport(t, "links-apply")
	mismatchRaw, _ := json.Marshal(mismatch)
	for name, test := range map[string]struct {
		executor *fakeExecutor
		code     string
	}{
		"malformed":      {executor: &fakeExecutor{raw: []byte(`{"schema_version":`)}, code: CodeUnsupported},
		"unsafe":         {executor: &fakeExecutor{raw: unsafeRaw}, code: CodeMaintenanceUnsafe},
		"held actions":   {executor: &fakeExecutor{raw: heldWithActionRaw, exitCode: 21}, code: CodeMaintenanceUnsafe},
		"missing target": {executor: &fakeExecutor{raw: missingTargetRaw}, code: CodeUnsupported},
		"stderr":         {executor: &fakeExecutor{raw: mismatchRaw, stderr: "secret child output"}, code: CodeUnsupported},
		"exit mismatch":  {executor: &fakeExecutor{raw: mismatchRaw, exitCode: 21}, code: CodeUnsupported},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (MaintenanceRunner{Executor: test.executor}).ApplyManagedSkillLinks(context.Background(), request)
			if err == nil || ErrorCode(err) != test.code {
				t.Fatalf("error = %v, code = %s, want %s", err, ErrorCode(err), test.code)
			}
		})
	}
}

func TestMaintenanceRunnerAcceptsNoopPlanAndCurrentMinor(t *testing.T) {
	request := maintenanceRequest(t)
	report := validMaintenanceReport(t, "links-plan")
	raw, _ := json.Marshal(report)
	executor := &fakeExecutor{raw: raw}
	if _, err := (MaintenanceRunner{Executor: executor}).PlanManagedSkillLinks(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(executor.seenArgs) < 2 || executor.seenArgs[0] != "links" || executor.seenArgs[1] != "plan" {
		t.Fatalf("plan args = %#v", executor.seenArgs)
	}
}
