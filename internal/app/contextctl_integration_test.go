//go:build contextctl_integration

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/JorgeMuehlebach/repo-sync/internal/config"
	"github.com/JorgeMuehlebach/repo-sync/internal/state"
)

func TestDependencyUpdateV21RegistryPassesRepoSyncPreflight(t *testing.T) {
	contextRootValue := os.Getenv("CONTEXT_SYSTEM_ROOT")
	if contextRootValue == "" {
		t.Skip("CONTEXT_SYSTEM_ROOT is required for the cross-tool integration test")
	}
	contextRoot := canonicalTestPath(t, contextRootValue)
	runtimeRoot := canonicalTestPath(t, t.TempDir())
	home := filepath.Join(runtimeRoot, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}

	contextctlPath := filepath.Join(runtimeRoot, "contextctl")
	if runtime.GOOS == "windows" {
		contextctlPath += ".exe"
	}
	build := exec.Command("go", "build", "-o", contextctlPath, "./cmd/contextctl")
	build.Dir = contextRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build contextctl: %v\n%s", err, output)
	}
	t.Setenv("HOME", home)
	if runtime.GOOS == "linux" {
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	}
	gitPath, gitDigest := appIntegrationExecutable(t, "git")

	sourceRoot := canonicalTestPath(t, t.TempDir())
	appIntegrationGit(t, gitPath, sourceRoot, "init", "--initial-branch=main")
	appIntegrationGit(t, gitPath, sourceRoot, "config", "user.name", "Context Integration")
	appIntegrationGit(t, gitPath, sourceRoot, "config", "user.email", "context@example.invalid")
	appIntegrationGit(t, gitPath, sourceRoot, "remote", "add", "origin", "https://github.com/example/context-integration.git")
	if err := os.MkdirAll(filepath.Join(sourceRoot, ".agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "contract_version: 2.1.0\nsource_id: example.integration.context\npublication:\n  mode: repo-sync\n  canonical_branch: main\n"
	if err := os.WriteFile(filepath.Join(sourceRoot, ".agents", "context-source.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "CONTEXT.md"), []byte("# Integration context\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	appIntegrationGit(t, gitPath, sourceRoot, "add", ".agents/context-source.yaml", "CONTEXT.md")
	appIntegrationGit(t, gitPath, sourceRoot, "commit", "--no-gpg-sign", "--no-verify", "-m", "integration fixture")

	pathStyle := "posix"
	if runtime.GOOS == "windows" {
		pathStyle = "windows"
	}
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	registryPath := filepath.Join(runtimeRoot, "machine-registry.json")
	appIntegrationJSON(t, registryPath, map[string]any{
		"schema_version": 2, "contract_version": "2.0.0", "registry_revision": 1,
		"machine": map[string]any{"id": "integration-machine-001", "profile": "generic-only", "os": platform},
		"registrations": []any{map[string]any{
			"source_id": "example.integration.context", "role": "writable", "enabled": true, "expose_global_skills": false,
			"root_path": map[string]any{"style": pathStyle, "value": sourceRoot},
			"git":       map[string]any{"remote_name": "origin", "remote_url": "https://github.com/example/context-integration.git", "branch": "main"},
			"repo_sync": map[string]any{"repository_id": "context-integration", "mode": "publisher"},
		}},
		"local_dependencies": []any{map[string]any{
			"id": "context-system.git", "path": map[string]any{"style": pathStyle, "value": gitPath}, "sha256": gitDigest, "kind": "executable",
		}},
		"connections": []any{},
	})
	trustPath := filepath.Join(runtimeRoot, "trusted-validator-state.json")
	appIntegrationJSON(t, trustPath, map[string]any{
		"schema_version": 2, "contract_version": "2.1.0", "state_revision": 1,
	})

	update := exec.Command(contextctlPath, "dependencies", "update", "--registry", registryPath, "--codex-executable", contextctlPath, "--format", "json")
	output, err := update.CombinedOutput()
	if err != nil {
		t.Fatalf("contextctl dependencies update: %v\n%s", err, output)
	}
	var updateReport struct {
		Command string `json:"command"`
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(output, &updateReport); err != nil || updateReport.Command != "dependencies-update" || updateReport.Verdict != "pass" {
		t.Fatalf("unexpected dependency update report: error=%v report=%#v", err, updateReport)
	}
	var updatedRegistry struct {
		ContractVersion  string `json:"contract_version"`
		RegistryRevision int    `json:"registry_revision"`
	}
	updatedBytes, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(updatedBytes, &updatedRegistry); err != nil || updatedRegistry.ContractVersion != "2.1.0" || updatedRegistry.RegistryRevision != 2 {
		t.Fatalf("unexpected updated registry: error=%v registry=%#v", err, updatedRegistry)
	}

	application, _ := testApplication(t)
	application.reconcileRuntime = nil
	entry := config.StructuredRepository("context-integration", "https://github.com/example/context-integration/tree/main", config.ModePublish, "example.integration.context")
	cfg := config.Config{Interval: "5m", Repositories: []config.Repository{entry}}
	if err := config.Save(application.configPath, cfg); err != nil {
		t.Fatal(err)
	}
	repositories, err := configuredRepositories(cfg, "")
	if err != nil || len(repositories) != 1 {
		t.Fatalf("configured repositories: error=%v repositories=%#v", err, repositories)
	}
	runtimeState, err := buildValidationRuntime(setupOptions{
		Contextctl: contextctlPath, Registry: registryPath, TrustState: trustPath,
	}, repositories[0], sourceRoot)
	if err != nil {
		t.Fatalf("build 2.1 validation runtime: %v", err)
	}
	machineState := state.New()
	machineState.Repositories[repositories[0].ID] = state.Repository{
		ID: repositories[0].ID, Mode: "publish", Path: sourceRoot, Revision: 1, ValidationRuntime: runtimeState,
	}
	if err := state.Save(application.statePath, machineState); err != nil {
		t.Fatal(err)
	}
	if err := application.preflightRepositories(context.Background(), repositories, machineState); err != nil {
		t.Fatalf("Repo Sync rejected dependency-update-created 2.1 registry: %v", err)
	}
}

func appIntegrationExecutable(t *testing.T, name string) (string, string) {
	t.Helper()
	value, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is unavailable", name)
	}
	value, err = filepath.EvalSymlinks(value)
	if err != nil {
		t.Fatal(err)
	}
	value, err = filepath.Abs(value)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(value)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	return filepath.Clean(value), hex.EncodeToString(digest[:])
}

func appIntegrationGit(t *testing.T, executable, directory string, args ...string) {
	t.Helper()
	command := exec.Command(executable, args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func appIntegrationJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
