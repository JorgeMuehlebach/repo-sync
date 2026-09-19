//go:build contextctl_integration

package validation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const integrationSourceID = "example.integration.context"

type integrationFixture struct {
	request        Request
	gitPath        string
	gitEnvironment []string
	registry       []byte
	trustState     []byte
	status         string
}

type transformingExecutor struct {
	delegate         Executor
	transform        func([]byte) ([]byte, error)
	calls            int
	executable       string
	executableDigest string
}

func (e *transformingExecutor) Run(ctx context.Context, executable string, args []string, stdout, stderr io.Writer) (int, error) {
	e.calls++
	e.executable = executable
	executableBytes, err := os.ReadFile(executable)
	if err != nil {
		return -1, err
	}
	e.executableDigest = digestIntegrationBytes(executableBytes)
	var captured bytes.Buffer
	exitCode, runErr := e.delegate.Run(ctx, executable, args, &captured, stderr)
	transformed, transformErr := e.transform(captured.Bytes())
	if transformErr != nil {
		return exitCode, transformErr
	}
	if _, err := stdout.Write(transformed); err != nil && runErr == nil {
		return exitCode, err
	}
	return exitCode, runErr
}

func TestContextctlCLIThroughRunner(t *testing.T) {
	contextSystemRoot := canonicalContextSystemRoot(t)
	runtimeRoot := canonicalTempDir(t)
	contextctlPath := buildContextctl(t, contextSystemRoot, runtimeRoot)
	fixture := newIntegrationFixture(t, runtimeRoot, contextctlPath)

	t.Run("canonical v2 pass", func(t *testing.T) {
		report, err := (Runner{Executor: SystemExecutor{}, Timeout: 30 * time.Second}).Validate(context.Background(), fixture.request)
		if err != nil {
			t.Fatalf("Validate() failed: %v", err)
		}
		if report.SchemaVersion != ReportSchemaVersion || report.Protocol != ReportProtocol || report.ContractVersion != "2.0.0" || report.Command != "validate" {
			t.Fatalf("unexpected report identity: %#v", report)
		}
		if report.Verdict != VerdictPass || !report.Promotable || report.Candidate.Kind != "git-tree" || report.Candidate.SourceID != integrationSourceID || report.Candidate.ObjectID != fixture.request.Tree {
			t.Fatalf("unexpected passing report: %#v", report)
		}
		if len(report.LinkActions) != 0 {
			t.Fatalf("validation proposed link actions: %#v", report.LinkActions)
		}
		wantValidator := "llm-config.check-posix"
		if runtime.GOOS == "windows" {
			wantValidator = "llm-config.check-windows"
		}
		found := false
		for _, result := range report.ValidatorResults {
			if result.ContractID == wantValidator && result.ContractVersion == "1.0.0" && result.Disposition == "pass" && result.SourceID == integrationSourceID && result.SourceRole == "writable" && containsIntegrationString(result.CheckIDs, "CTX-VALIDATOR-BUILTIN-PASS") {
				found = true
			}
		}
		if !found {
			t.Fatalf("passing built-in validator result not found: %#v", report.ValidatorResults)
		}
		fixture.assertUnchanged(t)
	})

	t.Run("recognizable v1 fails closed", func(t *testing.T) {
		executor := &transformingExecutor{delegate: SystemExecutor{}, transform: func(data []byte) ([]byte, error) {
			var value map[string]any
			if err := json.Unmarshal(data, &value); err != nil {
				return nil, fmt.Errorf("decode canonical report: %w", err)
			}
			value["schema_version"] = 1
			value["protocol"] = "contextctl.report.v1"
			value["contract_version"] = "1.0.0"
			return json.Marshal(value)
		}}
		if _, err := (Runner{Executor: executor, Timeout: 30 * time.Second}).Validate(context.Background(), fixture.request); err == nil || ErrorCode(err) != CodeMigrationNeeded {
			t.Fatalf("v1 report error = %v, code = %s", err, ErrorCode(err))
		}
		assertSnapshotContextctlExecution(t, executor, contextctlPath)
		fixture.assertUnchanged(t)
	})

	t.Run("malformed protocol fails closed", func(t *testing.T) {
		executor := &transformingExecutor{delegate: SystemExecutor{}, transform: func(data []byte) ([]byte, error) {
			if len(data) < 2 {
				return nil, fmt.Errorf("canonical report is unexpectedly short")
			}
			return append([]byte(nil), data[:len(data)-1]...), nil
		}}
		if _, err := (Runner{Executor: executor, Timeout: 30 * time.Second}).Validate(context.Background(), fixture.request); err == nil || ErrorCode(err) != CodeUnsupported {
			t.Fatalf("malformed report error = %v, code = %s", err, ErrorCode(err))
		}
		assertSnapshotContextctlExecution(t, executor, contextctlPath)
		fixture.assertUnchanged(t)
	})
}

func canonicalContextSystemRoot(t *testing.T) string {
	t.Helper()
	value := os.Getenv("CONTEXT_SYSTEM_ROOT")
	if value == "" {
		t.Fatal("CONTEXT_SYSTEM_ROOT is required for the contextctl integration gate")
	}
	root, err := filepath.Abs(value)
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve CONTEXT_SYSTEM_ROOT: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		t.Fatalf("CONTEXT_SYSTEM_ROOT is not a directory: %v", err)
	}
	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !bytes.Contains(module, []byte("module github.com/JorgeMuehlebach/llm-config/context-system")) {
		t.Fatalf("CONTEXT_SYSTEM_ROOT is not the context-system module: %v", err)
	}
	return filepath.Clean(root)
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(root)
}

func buildContextctl(t *testing.T, contextSystemRoot, runtimeRoot string) string {
	t.Helper()
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("go executable is unavailable")
	}
	goPath, err = filepath.EvalSymlinks(goPath)
	if err != nil {
		t.Fatalf("resolve go executable: %v", err)
	}
	binaryName := "contextctl"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binaryPath := filepath.Join(runtimeRoot, binaryName)
	goCache := filepath.Join(runtimeRoot, "go-cache")
	goTemp := filepath.Join(runtimeRoot, "go-tmp")
	for _, directory := range []string{goCache, goTemp} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command(goPath, "build", "-trimpath", "-mod=readonly", "-o", binaryPath, "./cmd/contextctl")
	command.Dir = contextSystemRoot
	command.Env = integrationEnvironment(os.Environ(), map[string]string{
		"GOCACHE":             goCache,
		"GOENV":               "off",
		"GOFLAGS":             "",
		"GONOSUMDB":           "*",
		"GOPROXY":             "off",
		"GOSUMDB":             "off",
		"GOTOOLCHAIN":         "local",
		"GOTMPDIR":            goTemp,
		"GOVCS":               "*:off",
		"GIT_TERMINAL_PROMPT": "0",
	})
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build contextctl without network access: %v: %s", err, output)
	}
	canonical, err := filepath.EvalSymlinks(binaryPath)
	if err != nil {
		t.Fatalf("resolve built contextctl: %v", err)
	}
	return filepath.Clean(canonical)
}

func newIntegrationFixture(t *testing.T, runtimeRoot, contextctlPath string) integrationFixture {
	t.Helper()
	gitPath, gitDigest := discoverIntegrationGit(t)
	sourceRoot := canonicalTempDir(t)
	hooksRoot := filepath.Join(sourceRoot, "empty-hooks")
	if err := os.Mkdir(hooksRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	globalConfig := filepath.Join(runtimeRoot, "empty-gitconfig")
	if err := os.WriteFile(globalConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	gitEnvironment := integrationEnvironment(os.Environ(), map[string]string{
		"GCM_INTERACTIVE":     "never",
		"GIT_CONFIG_GLOBAL":   globalConfig,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_NO_LAZY_FETCH":   "1",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_OPTIONAL_LOCKS":  "0",
	})
	runIntegrationGit(t, gitPath, gitEnvironment, "init", "-b", "main", sourceRoot)
	if err := os.Remove(hooksRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sourceRoot, ".agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"schema_version":   2,
		"contract_version": "2.0.0",
		"source_id":        integrationSourceID,
		"authority": map[string]any{
			"kind":     "generic",
			"domain":   "integration",
			"owns":     []string{"Disposable context integration fixture"},
			"excludes": []string{"Live machine and service state"},
		},
		"context_map":               "CONTEXT.md",
		"skills":                    []any{},
		"allowed_machine_profiles":  []string{"generic-only"},
		"allowed_source_roles":      []string{"writable", "context-mirror"},
		"global_skill_source_roles": []any{},
		"validators": []map[string]any{
			{"contract_id": "llm-config.check-posix", "contract_version": "1.0.0", "platforms": []string{"macos", "linux"}},
			{"contract_id": "llm-config.check-windows", "contract_version": "1.0.0", "platforms": []string{"windows"}},
		},
		"publication": map[string]any{
			"mode":             "repo-sync",
			"canonical_branch": "main",
			"writable_roles":   []string{"writable"},
		},
		"harness_adapters":            []any{},
		"external_skill_dependencies": []any{},
	}
	writeIntegrationJSON(t, filepath.Join(sourceRoot, ".agents", "context-source.yaml"), manifest)
	if err := os.WriteFile(filepath.Join(sourceRoot, "CONTEXT.md"), []byte("# Disposable integration context\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitArgs := []string{"-c", "core.hooksPath=" + hooksRoot, "-C", sourceRoot}
	runIntegrationGit(t, gitPath, gitEnvironment, append(gitArgs, "add", "--", ".agents/context-source.yaml", "CONTEXT.md")...)
	runIntegrationGit(t, gitPath, gitEnvironment, append(gitArgs, "-c", "user.name=Context Integration", "-c", "user.email=context@example.invalid", "commit", "--no-gpg-sign", "--no-verify", "-m", "integration fixture")...)
	tree := strings.TrimSpace(runIntegrationGit(t, gitPath, gitEnvironment, append(gitArgs, "rev-parse", "HEAD^{tree}")...))
	commit := strings.TrimSpace(runIntegrationGit(t, gitPath, gitEnvironment, append(gitArgs, "rev-parse", "HEAD")...))
	status := runIntegrationGit(t, gitPath, gitEnvironment, append(gitArgs, "status", "--porcelain=v1")...)
	if status != "" {
		t.Fatalf("fixture repository is dirty: %q", status)
	}

	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	pathStyle := "posix"
	if runtime.GOOS == "windows" {
		pathStyle = "windows"
	}
	registryPath := filepath.Join(runtimeRoot, "machine-registry.json")
	registryValue := map[string]any{
		"schema_version":    2,
		"contract_version":  "2.0.0",
		"registry_revision": 1,
		"machine": map[string]any{
			"id": "integration-machine-001", "profile": "generic-only", "os": platform,
		},
		"registrations": []map[string]any{{
			"source_id": integrationSourceID, "role": "writable", "enabled": true, "expose_global_skills": false,
			"root_path": map[string]any{"style": pathStyle, "value": sourceRoot},
			"git": map[string]any{
				"remote_name": "origin", "remote_url": "https://example.invalid/context-integration.git", "branch": "main", "commit": commit,
			},
			"repo_sync": map[string]any{"repository_id": "context-integration", "mode": "publisher"},
		}},
		"local_dependencies": []map[string]any{{
			"id": "context-system.git", "path": map[string]any{"style": pathStyle, "value": gitPath}, "sha256": gitDigest, "version": "integration", "kind": "executable",
		}},
		"connections": []any{},
	}
	writeIntegrationJSON(t, registryPath, registryValue)
	trustRoot := filepath.Join(runtimeRoot, "trust-root")
	if err := os.Mkdir(trustRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	trustStatePath := filepath.Join(runtimeRoot, "trusted-validator-state.json")
	trustStateValue := map[string]any{
		"schema_version":   2,
		"contract_version": "2.0.0",
		"state_revision":   1,
		"machine_id":       "integration-machine-001",
		"trust_root":       map[string]any{"style": pathStyle, "value": trustRoot},
		"approvals":        []any{},
	}
	writeIntegrationJSON(t, trustStatePath, trustStateValue)
	registry := readIntegrationFile(t, registryPath)
	trustState := readIntegrationFile(t, trustStatePath)

	return integrationFixture{
		request: Request{
			RepositoryID:   "context-integration",
			SourceRoot:     sourceRoot,
			RepositoryPath: sourceRoot,
			Mode:           ModePublish,
			Tree:           tree,
			Commit:         commit,
			Runtime: Runtime{
				ContextctlPath:   contextctlPath,
				ContextctlDigest: "sha256:" + digestIntegrationFile(t, contextctlPath),
				RegistryPath:     registryPath,
				RegistryDigest:   "sha256:" + digestIntegrationBytes(registry),
				TrustStatePath:   trustStatePath,
				TrustStateDigest: "sha256:" + digestIntegrationBytes(trustState),
				SourceID:         integrationSourceID,
			},
		},
		gitPath:        gitPath,
		gitEnvironment: gitEnvironment,
		registry:       registry,
		trustState:     trustState,
		status:         status,
	}
}

func (f integrationFixture) assertUnchanged(t *testing.T) {
	t.Helper()
	if current := readIntegrationFile(t, f.request.Runtime.RegistryPath); !bytes.Equal(current, f.registry) {
		t.Fatal("contextctl changed the machine registry")
	}
	if current := readIntegrationFile(t, f.request.Runtime.TrustStatePath); !bytes.Equal(current, f.trustState) {
		t.Fatal("contextctl changed trusted-validator state")
	}
	gitArgs := []string{"-c", "core.hooksPath=" + filepath.Join(f.request.SourceRoot, "empty-hooks"), "-C", f.request.RepositoryPath}
	if tree := strings.TrimSpace(runIntegrationGit(t, f.gitPath, f.gitEnvironment, append(gitArgs, "rev-parse", "HEAD^{tree}")...)); tree != f.request.Tree {
		t.Fatalf("candidate tree changed: got %q, want %q", tree, f.request.Tree)
	}
	if status := runIntegrationGit(t, f.gitPath, f.gitEnvironment, append(gitArgs, "status", "--porcelain=v1")...); status != f.status {
		t.Fatalf("candidate worktree changed: got %q, want %q", status, f.status)
	}
}

func discoverIntegrationGit(t *testing.T) (string, string) {
	t.Helper()
	value, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("git executable is unavailable")
	}
	value, err = filepath.Abs(value)
	if err != nil {
		t.Fatal(err)
	}
	value, err = filepath.EvalSymlinks(value)
	if err != nil {
		t.Fatalf("resolve git executable: %v", err)
	}
	info, err := os.Lstat(value)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode()&0o111 == 0) {
		t.Fatalf("git executable is not a canonical regular executable: %v", err)
	}
	return filepath.Clean(value), digestIntegrationFile(t, value)
}

func runIntegrationGit(t *testing.T, gitPath string, environment []string, args ...string) string {
	t.Helper()
	command := exec.Command(gitPath, args...)
	command.Env = environment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("local git %v failed: %v: %s", args, err, output)
	}
	return string(output)
}

func integrationEnvironment(base []string, overrides map[string]string) []string {
	blocked := make(map[string]bool, len(overrides))
	for key := range overrides {
		blocked[strings.ToUpper(key)] = true
	}
	result := make([]string, 0, len(base)+len(overrides))
	for _, value := range base {
		name, _, ok := strings.Cut(value, "=")
		upper := strings.ToUpper(name)
		if !ok || blocked[upper] || strings.HasPrefix(upper, "GIT_") || strings.HasPrefix(upper, "GCM_") {
			continue
		}
		result = append(result, value)
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

func writeIntegrationJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readIntegrationFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func digestIntegrationFile(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func digestIntegrationBytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func assertSnapshotContextctlExecution(t *testing.T, executor *transformingExecutor, source string) {
	t.Helper()
	if executor.calls != 1 || executor.executable == source || executor.executableDigest != digestIntegrationFile(t, source) {
		t.Fatalf("contextctl executions = %d at source=%t digest=%q", executor.calls, executor.executable == source, executor.executableDigest)
	}
}

func containsIntegrationString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
