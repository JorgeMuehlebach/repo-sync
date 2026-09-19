package validation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type fakeExecutor struct {
	report     Report
	raw        []byte
	exitCode   int
	stderr     string
	seenArgs   []string
	executable string
}

func (f *fakeExecutor) Run(_ context.Context, executable string, args []string, stdout, stderr io.Writer) (int, error) {
	f.executable = executable
	f.seenArgs = append([]string(nil), args...)
	data := f.raw
	if data == nil {
		data, _ = json.Marshal(f.report)
	}
	_, _ = stdout.Write(data)
	_, _ = io.WriteString(stderr, f.stderr)
	return f.exitCode, nil
}

func validReport(sourceID, objectID string) Report {
	return Report{
		SchemaVersion:    ReportSchemaVersion,
		Protocol:         ReportProtocol,
		ContractVersion:  "2.0.0",
		Command:          "validate",
		GeneratedAt:      "2026-09-19T00:00:00Z",
		Verdict:          VerdictPass,
		Promotable:       true,
		Candidate:        Candidate{Kind: "git-tree", SourceID: sourceID, ObjectID: objectID},
		Checks:           []Check{{ID: "CTX-SCHEMA-PASS", Status: "pass", Paths: []string{}}},
		ValidatorResults: []ValidatorResult{{ContractID: "example.validator", ContractVersion: "1.0.0", Disposition: "pass", CheckIDs: []string{"CTX-SCHEMA-PASS"}}},
		LinkActions:      []LinkAction{},
	}
}

func testRequest(t *testing.T) Request {
	t.Helper()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(dir, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "contextctl")
	binaryContents := []byte("trusted binary")
	if err := os.WriteFile(binary, binaryContents, 0o700); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(dir, "registry.json")
	trust := filepath.Join(dir, "trust.json")
	stateContents := []byte("{}")
	for _, path := range []string{registry, trust} {
		if err := os.WriteFile(path, stateContents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Request{
		RepositoryID:   "repo",
		SourceRoot:     repository,
		RepositoryPath: repository,
		Mode:           ModeMirror,
		Tree:           strings.Repeat("a", 40),
		Runtime: Runtime{
			ContextctlPath:   binary,
			ContextctlDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(binaryContents)),
			RegistryPath:     registry,
			RegistryDigest:   fmt.Sprintf("sha256:%x", sha256.Sum256(stateContents)),
			TrustStatePath:   trust,
			TrustStateDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(stateContents)),
			SourceID:         "example.source",
		},
	}
}

func TestRunnerAcceptsExactPassingReport(t *testing.T) {
	request := testRequest(t)
	executor := &fakeExecutor{report: validReport(request.Runtime.SourceID, request.Tree)}
	report, err := (Runner{Executor: executor}).Validate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != VerdictPass || len(executor.seenArgs) != 15 {
		t.Fatalf("report/args = %#v %#v", report, executor.seenArgs)
	}
}

func TestRunnerRejectsIdentityAndExitMismatches(t *testing.T) {
	request := testRequest(t)
	tests := map[string]fakeExecutor{
		"source": {report: validReport("other.source", request.Tree)},
		"tree":   {report: validReport(request.Runtime.SourceID, strings.Repeat("b", 40))},
		"exit":   {report: validReport(request.Runtime.SourceID, request.Tree), exitCode: 20},
		"stderr": {report: validReport(request.Runtime.SourceID, request.Tree), stderr: "unexpected"},
	}
	for name, executor := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := (Runner{Executor: &executor}).Validate(context.Background(), request); err == nil {
				t.Fatal("Validate() returned no error")
			}
		})
	}
}

func TestNormalizeRejectsUnsafeFindingPath(t *testing.T) {
	request := testRequest(t)
	report := validReport(request.Runtime.SourceID, request.Tree)
	report.Verdict = VerdictFail
	report.Promotable = false
	report.Checks = []Check{{ID: "CTX-SCHEMA-FAIL", Status: "fail", Paths: []string{"../secret"}}}
	if err := Normalize(&report, request); err == nil {
		t.Fatal("Normalize() accepted an escaping path")
	}
}

func TestNormalizeAcceptsCurrentContractPatchAndRejectsIncompatibleVersions(t *testing.T) {
	request := testRequest(t)
	for _, version := range []string{"2.0.0", "2.0.9"} {
		report := validReport(request.Runtime.SourceID, request.Tree)
		report.ContractVersion = version
		if err := Normalize(&report, request); err != nil {
			t.Fatalf("Normalize(%s): %v", version, err)
		}
	}
	for _, version := range []string{"1.2.0", "2.1.0", "3.0.0", "2.0.0-rc.1", "2.0.0+build"} {
		report := validReport(request.Runtime.SourceID, request.Tree)
		report.ContractVersion = version
		if err := Normalize(&report, request); err == nil {
			t.Fatalf("Normalize(%s) accepted an unsupported contract", version)
		}
	}
	legacy := validReport(request.Runtime.SourceID, request.Tree)
	legacy.SchemaVersion = 1
	legacy.Protocol = "contextctl.report.v1"
	legacy.ContractVersion = "1.2.0"
	if err := Normalize(&legacy, request); err == nil || ErrorCode(err) != CodeMigrationNeeded {
		t.Fatalf("legacy contract error = %v, code = %s", err, ErrorCode(err))
	}
}

func TestRunnerRejectsTrailingBytesMissingFieldsAndDuplicateKeys(t *testing.T) {
	request := testRequest(t)
	valid, err := json.Marshal(validReport(request.Runtime.SourceID, request.Tree))
	if err != nil {
		t.Fatal(err)
	}
	missingPromotable := map[string]any{}
	if err := json.Unmarshal(valid, &missingPromotable); err != nil {
		t.Fatal(err)
	}
	delete(missingPromotable, "promotable")
	missing, _ := json.Marshal(missingPromotable)
	tests := map[string][]byte{
		"trailing newline":     append(append([]byte(nil), valid...), '\n'),
		"missing promotable":   missing,
		"duplicate top key":    bytes.Replace(valid, []byte(`"schema_version":2`), []byte(`"schema_version":2,"schema_version":2`), 1),
		"duplicate nested key": bytes.Replace(valid, []byte(`"id":"CTX-SCHEMA-PASS"`), []byte(`"id":"CTX-SCHEMA-PASS","id":"CTX-SCHEMA-PASS"`), 1),
		"null result array":    bytes.Replace(valid, []byte(`"validator_results":[`), []byte(`"validator_results":null,"ignored":[`), 1),
		"legacy bundle field":  bytes.Replace(valid, []byte(`"contract_version":"1.0.0"`), []byte(`"contract_version":"1.0.0","bundle_version":"1.0.0"`), 1),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			executor := &fakeExecutor{raw: raw}
			if _, err := (Runner{Executor: executor}).Validate(context.Background(), request); err == nil || ErrorCode(err) != CodeUnsupported {
				t.Fatal("Validate() accepted malformed protocol JSON")
			}
		})
	}
}

func TestRunnerClassifiesContractFamiliesExactly(t *testing.T) {
	request := testRequest(t)
	base, err := json.Marshal(validReport(request.Runtime.SourceID, request.Tree))
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(fields map[string]any) []byte {
		var value map[string]any
		if err := json.Unmarshal(base, &value); err != nil {
			t.Fatal(err)
		}
		for key, field := range fields {
			value[key] = field
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for name, test := range map[string]struct {
		raw  []byte
		code string
	}{
		"recognizable v1 identity": {raw: mutate(map[string]any{"schema_version": 1, "protocol": "contextctl.report.v1", "contract_version": "1.9.0"}), code: CodeMigrationNeeded},
		"v1 contract on v2":        {raw: mutate(map[string]any{"contract_version": "1.9.0"}), code: CodeUnsupported},
		"future schema":            {raw: mutate(map[string]any{"schema_version": 3}), code: CodeUnsupported},
		"future protocol":          {raw: mutate(map[string]any{"protocol": "contextctl.report.v3"}), code: CodeUnsupported},
		"future contract":          {raw: mutate(map[string]any{"contract_version": "2.1.0"}), code: CodeUnsupported},
		"prerelease contract":      {raw: mutate(map[string]any{"contract_version": "2.0.0-rc.1"}), code: CodeUnsupported},
		"build contract":           {raw: mutate(map[string]any{"contract_version": "2.0.0+build"}), code: CodeUnsupported},
		"malformed":                {raw: []byte(`{"schema_version":`), code: CodeUnsupported},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (Runner{Executor: &fakeExecutor{raw: test.raw}}).Validate(context.Background(), request)
			if err == nil || ErrorCode(err) != test.code {
				t.Fatalf("error = %v, code = %s, want %s", err, ErrorCode(err), test.code)
			}
		})
	}
}

func TestReportHeaderCrossProductClassification(t *testing.T) {
	request := testRequest(t)
	for _, schema := range []int{1, 2, 3} {
		for _, protocol := range []string{"contextctl.report.v1", "contextctl.report.v2", "contextctl.report.v3"} {
			for _, contract := range []string{"1.9.9", "2.0.9", "3.0.0", "2.0.0-rc.1", "2.0.0+build", "01.0.0", "invalid"} {
				name := fmt.Sprintf("schema-%d/%s/%s", schema, protocol, contract)
				t.Run(name, func(t *testing.T) {
					report := validReport(request.Runtime.SourceID, request.Tree)
					report.SchemaVersion = schema
					report.Protocol = protocol
					report.ContractVersion = contract
					raw, err := json.Marshal(report)
					if err != nil {
						t.Fatal(err)
					}
					_, err = decodeReport(raw)
					switch {
					case schema == 2 && protocol == ReportProtocol && contract == "2.0.9":
						if err != nil {
							t.Fatalf("current stable header was rejected: %v", err)
						}
					case schema == 1 && protocol == "contextctl.report.v1" && contract == "1.9.9":
						if err == nil || ErrorCode(err) != CodeMigrationNeeded {
							t.Fatalf("legacy header error = %v, code = %s", err, ErrorCode(err))
						}
					default:
						if err == nil || ErrorCode(err) != CodeUnsupported {
							t.Fatalf("mixed/future header error = %v, code = %s", err, ErrorCode(err))
						}
					}
				})
			}
		}
	}
}

func TestMalformedV1ReportsAreUnsupportedRatherThanMigrationRequired(t *testing.T) {
	request := testRequest(t)
	legacy := validReport(request.Runtime.SourceID, request.Tree)
	legacy.SchemaVersion = 1
	legacy.Protocol = "contextctl.report.v1"
	legacy.ContractVersion = "1.0.0"
	valid, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(change func(map[string]any)) []byte {
		t.Helper()
		var value map[string]any
		if err := json.Unmarshal(valid, &value); err != nil {
			t.Fatal(err)
		}
		change(value)
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	tests := map[string][]byte{
		"missing required field": mutate(func(value map[string]any) { delete(value, "checks") }),
		"unknown top field":      mutate(func(value map[string]any) { value["unknown"] = true }),
		"wrong command":          mutate(func(value map[string]any) { value["command"] = "status" }),
		"impossible verdict":     mutate(func(value map[string]any) { value["promotable"] = false }),
		"no checks":              mutate(func(value map[string]any) { value["checks"] = []any{} }),
		"invalid candidate": mutate(func(value map[string]any) {
			value["candidate"] = map[string]any{"kind": "git-tree", "source_id": "invalid", "object_id": request.Tree}
		}),
		"duplicate key":        bytes.Replace(valid, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1),
		"unknown nested field": bytes.Replace(valid, []byte(`"kind":"git-tree"`), []byte(`"kind":"git-tree","unknown":true`), 1),
		"trailing bytes":       append(append([]byte(nil), valid...), '\n'),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := decodeReport(raw)
			if err == nil || ErrorCode(err) != CodeUnsupported {
				t.Fatalf("decodeReport() error = %v, code = %s; want %s", err, ErrorCode(err), CodeUnsupported)
			}
		})
	}
	if _, err := decodeReport(valid); err == nil || ErrorCode(err) != CodeMigrationNeeded {
		t.Fatalf("valid frozen v1 report error = %v, code = %s", err, ErrorCode(err))
	}
}

func TestNormalizeRejectsV1ReportAsMigrationRequired(t *testing.T) {
	request := testRequest(t)
	report := validReport(request.Runtime.SourceID, request.Tree)
	report.SchemaVersion = 1
	report.Protocol = "contextctl.report.v1"
	report.ContractVersion = "1.0.0"
	err := Normalize(&report, request)
	if err == nil || ErrorCode(err) != CodeMigrationNeeded {
		t.Fatalf("Normalize(v1) error = %v, code = %s", err, ErrorCode(err))
	}
}

func TestNormalizeRejectsSemanticallyImpossibleV1AsUnsupported(t *testing.T) {
	request := testRequest(t)
	report := validReport(request.Runtime.SourceID, request.Tree)
	report.SchemaVersion = 1
	report.Protocol = "contextctl.report.v1"
	report.ContractVersion = "1.0.0"
	report.Promotable = false
	err := Normalize(&report, request)
	if err == nil || ErrorCode(err) != CodeUnsupported {
		t.Fatalf("Normalize(malformed v1) error = %v, code = %s", err, ErrorCode(err))
	}
}

func TestNormalizeRejectsDuplicatesAndPassWithSkippedWork(t *testing.T) {
	request := testRequest(t)
	t.Run("duplicate check", func(t *testing.T) {
		report := validReport(request.Runtime.SourceID, request.Tree)
		report.Checks = append(report.Checks, report.Checks[0])
		if err := Normalize(&report, request); err == nil {
			t.Fatal("duplicate check was accepted")
		}
	})
	t.Run("duplicate validator", func(t *testing.T) {
		report := validReport(request.Runtime.SourceID, request.Tree)
		report.ValidatorResults = append(report.ValidatorResults, report.ValidatorResults[0])
		if err := Normalize(&report, request); err == nil {
			t.Fatal("duplicate validator identity was accepted")
		}
	})
	t.Run("pass with skip", func(t *testing.T) {
		report := validReport(request.Runtime.SourceID, request.Tree)
		report.Checks[0].Status = "skip"
		report.ValidatorResults[0].Disposition = "skip"
		if err := Normalize(&report, request); err == nil {
			t.Fatal("pass report with skipped required work was accepted")
		}
	})
	t.Run("pass without validator", func(t *testing.T) {
		report := validReport(request.Runtime.SourceID, request.Tree)
		report.ValidatorResults = []ValidatorResult{}
		if err := Normalize(&report, request); err == nil {
			t.Fatal("pass report without an applicable validator was accepted")
		}
	})
	t.Run("validator prerelease version", func(t *testing.T) {
		report := validReport(request.Runtime.SourceID, request.Tree)
		report.ValidatorResults[0].ContractVersion = "1.0.0-rc.1"
		if err := Normalize(&report, request); err == nil || ErrorCode(err) != CodeUnsupported {
			t.Fatalf("prerelease validator version error = %v", err)
		}
	})
}

func TestNormalizeValidatesOptionalValidatorSourceIdentity(t *testing.T) {
	request := testRequest(t)
	report := validReport(request.Runtime.SourceID, request.Tree)
	report.ValidatorResults[0].SourceID = request.Runtime.SourceID
	report.ValidatorResults[0].SourceRole = "context-mirror"
	if err := Normalize(&report, request); err != nil {
		t.Fatalf("valid validator source identity was rejected: %v", err)
	}

	for name, mutate := range map[string]func(*ValidatorResult){
		"wrong source": func(result *ValidatorResult) { result.SourceID = "other.source" },
		"bad role":     func(result *ValidatorResult) { result.SourceRole = "administrator" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := validReport(request.Runtime.SourceID, request.Tree)
			mutate(&invalid.ValidatorResults[0])
			if err := Normalize(&invalid, request); err == nil {
				t.Fatal("invalid validator source identity was accepted")
			}
		})
	}
}

func TestRunnerClassifiesRuntimeIdentityDriftWithoutExecuting(t *testing.T) {
	request := testRequest(t)
	executor := &fakeExecutor{report: validReport(request.Runtime.SourceID, request.Tree)}
	if err := os.WriteFile(request.Runtime.RegistryPath, []byte(`{"changed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (Runner{Executor: executor}).Validate(context.Background(), request)
	if ErrorCode(err) != CodeUnavailable || len(executor.seenArgs) != 0 {
		t.Fatalf("registry drift error/call = %v %#v", err, executor.seenArgs)
	}

	request = testRequest(t)
	executor = &fakeExecutor{report: validReport(request.Runtime.SourceID, request.Tree)}
	if err := os.WriteFile(request.Runtime.TrustStatePath, []byte(`{"changed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = (Runner{Executor: executor}).Validate(context.Background(), request)
	if ErrorCode(err) != CodeTrustTamper || len(executor.seenArgs) != 0 {
		t.Fatalf("trust drift error/call = %v %#v", err, executor.seenArgs)
	}
}

func TestRunnerRejectsOversizedRuntimeArtifactsWithoutExecuting(t *testing.T) {
	request := testRequest(t)
	executor := &fakeExecutor{report: validReport(request.Runtime.SourceID, request.Tree)}
	if err := os.Truncate(request.Runtime.RegistryPath, maxRuntimeJSONBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := (Runner{Executor: executor}).Validate(context.Background(), request); ErrorCode(err) != CodeUnavailable {
		t.Fatalf("oversized registry error = %v", err)
	}
	if len(executor.seenArgs) != 0 {
		t.Fatalf("executor ran for oversized registry: %#v", executor.seenArgs)
	}
}

func TestRunnerUsesVerifiedPrivateSnapshotsAcrossBoundarySwap(t *testing.T) {
	request := testRequest(t)
	trustedExecutable, err := os.ReadFile(request.Runtime.ContextctlPath)
	if err != nil {
		t.Fatal(err)
	}
	trustedRegistry, err := os.ReadFile(request.Runtime.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	trustedTrust, err := os.ReadFile(request.Runtime.TrustStatePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", request.SourceRoot)
	executor := &snapshotInspectingExecutor{
		request: request,
		trusted: map[string][]byte{"contextctl": trustedExecutable, "registry": trustedRegistry, "trust": trustedTrust},
		report:  validReport(request.Runtime.SourceID, request.Tree),
	}
	if _, err := (Runner{Executor: executor}).Validate(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !executor.called {
		t.Fatal("snapshot executor was not called")
	}
}

type snapshotInspectingExecutor struct {
	request Request
	trusted map[string][]byte
	report  Report
	called  bool
}

func (e *snapshotInspectingExecutor) Run(_ context.Context, executable string, args []string, stdout, _ io.Writer) (int, error) {
	e.called = true
	registry := argumentValue(args, "--registry")
	trust := argumentValue(args, "--trust-state")
	if executable == e.request.Runtime.ContextctlPath || registry == e.request.Runtime.RegistryPath || trust == e.request.Runtime.TrustStatePath {
		return -1, fmt.Errorf("child received mutable runtime source paths")
	}
	snapshotRoot := filepath.Dir(executable)
	if filepath.Dir(registry) != snapshotRoot || filepath.Dir(trust) != snapshotRoot {
		return -1, fmt.Errorf("runtime snapshots do not share one private root")
	}
	if rootInfo, err := os.Stat(snapshotRoot); err != nil || !rootInfo.IsDir() || (runtime.GOOS != "windows" && rootInfo.Mode().Perm()&0o222 != 0) {
		return -1, fmt.Errorf("runtime snapshot root is writable")
	}
	for _, path := range []string{executable, registry, trust} {
		if pathsOverlap(path, e.request.SourceRoot) || pathsOverlap(path, e.request.RepositoryPath) {
			return -1, fmt.Errorf("runtime snapshot overlaps candidate repository")
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0o222 != 0 {
			return -1, fmt.Errorf("runtime snapshot is writable")
		}
		if runtime.GOOS != "windows" {
			if writable, openErr := os.OpenFile(path, os.O_WRONLY, 0); openErr == nil {
				_ = writable.Close()
				return -1, fmt.Errorf("runtime snapshot opened for writing")
			}
			replacement := path + ".replacement"
			if renameErr := os.Rename(path, replacement); renameErr == nil {
				_ = os.Rename(replacement, path)
				return -1, fmt.Errorf("runtime snapshot was replaceable")
			}
		}
	}
	for _, source := range []struct {
		path        string
		replacement []byte
		mode        os.FileMode
	}{
		{e.request.Runtime.ContextctlPath, []byte("hostile executable"), 0o700},
		{e.request.Runtime.RegistryPath, []byte(`{"hostile":true}`), 0o600},
		{e.request.Runtime.TrustStatePath, []byte(`{"hostile":true}`), 0o600},
	} {
		backup := source.path + ".verified-backup"
		if err := os.Rename(source.path, backup); err != nil {
			return -1, err
		}
		if err := os.WriteFile(source.path, source.replacement, source.mode); err != nil {
			_ = os.Rename(backup, source.path)
			return -1, err
		}
		defer func(path, backup string) {
			_ = os.Remove(path)
			_ = os.Rename(backup, path)
		}(source.path, backup)
	}
	for path, want := range map[string][]byte{executable: e.trusted["contextctl"], registry: e.trusted["registry"], trust: e.trusted["trust"]} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			return -1, fmt.Errorf("child did not receive exact verified snapshot")
		}
	}
	data, err := json.Marshal(e.report)
	if err != nil {
		return -1, err
	}
	_, err = stdout.Write(data)
	return 0, err
}

func argumentValue(args []string, name string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}
