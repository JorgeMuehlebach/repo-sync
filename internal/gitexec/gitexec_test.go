package gitexec

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
)

func TestDependencyFromRegistryRequiresExactlyOneCanonicalV2Git(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "git")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	if err := os.WriteFile(executable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("fixture"))
	for _, contract := range []string{"2.0.0", "2.0.9", "2.1.0", "2.1.9"} {
		if _, err := DependencyFromRegistry(dependencyRegistry(t, executable, hex.EncodeToString(digest[:]), contract, 1)); err != nil {
			t.Fatalf("valid contract %s was rejected: %v", contract, err)
		}
	}
	for name, data := range map[string][]byte{
		"missing":      dependencyRegistry(t, executable, hex.EncodeToString(digest[:]), "2.1.0", 0),
		"duplicate":    dependencyRegistry(t, executable, hex.EncodeToString(digest[:]), "2.1.0", 2),
		"uppercase":    dependencyRegistry(t, executable, strings.ToUpper(hex.EncodeToString(digest[:])), "2.1.0", 1),
		"prerelease":   dependencyRegistry(t, executable, hex.EncodeToString(digest[:]), "2.1.0-rc.1", 1),
		"build":        dependencyRegistry(t, executable, hex.EncodeToString(digest[:]), "2.1.0+build", 1),
		"future":       dependencyRegistry(t, executable, hex.EncodeToString(digest[:]), "2.2.0", 1),
		"bad contract": dependencyRegistry(t, executable, hex.EncodeToString(digest[:]), "invalid", 1),
		"bad schema":   dependencyRegistrySchema(t, executable, hex.EncodeToString(digest[:]), "2.1.0", 1, 3),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DependencyFromRegistry(data); err == nil {
				t.Fatal("incompatible registry dependency was accepted")
			}
		})
	}
}

func TestSystemRunnerExecutesPinnedGitFromV21Registry(t *testing.T) {
	executable, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is unavailable")
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	runner, err := NewSystemRunnerFromRegistry(dependencyRegistry(t, executable, hex.EncodeToString(digest[:]), "2.1.0", 1))
	if err != nil {
		t.Fatalf("create runner from 2.1 registry: %v", err)
	}
	result := runner.Run(context.Background(), t.TempDir(), "--version")
	if result.Err != nil || result.ExitCode != 0 || !strings.HasPrefix(result.Output, "git version ") {
		t.Fatalf("pinned Git result = %#v", result)
	}
}

func TestNewSystemRunnerHonorsCanceledHashContext(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "git")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	contents := []byte("fixture")
	if err := os.WriteFile(executable, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSystemRunnerFromRegistryContext(ctx, dependencyRegistry(t, executable, hex.EncodeToString(digest[:]), "2.0.0", 1)); err == nil {
		t.Fatal("canceled dependency hashing unexpectedly succeeded")
	}
}

func dependencyRegistry(t *testing.T, executable, digest, contract string, count int) []byte {
	return dependencyRegistrySchema(t, executable, digest, contract, count, 2)
}

func dependencyRegistrySchema(t *testing.T, executable, digest, contract string, count, schema int) []byte {
	t.Helper()
	style := "posix"
	if runtime.GOOS == "windows" {
		style = "windows"
	}
	dependencies := make([]any, 0, count)
	for range count {
		dependencies = append(dependencies, map[string]any{
			"id": DependencyID, "path": map[string]any{"style": style, "value": executable},
			"sha256": digest, "kind": "executable",
		})
	}
	data, err := json.Marshal(map[string]any{
		"schema_version": schema, "contract_version": contract, "local_dependencies": dependencies,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
