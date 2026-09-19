package gitexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
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
	valid := dependencyRegistry(t, executable, hex.EncodeToString(digest[:]), "2.0.0", 1)
	if _, err := DependencyFromRegistry(valid); err != nil {
		t.Fatalf("valid dependency was rejected: %v", err)
	}
	for name, data := range map[string][]byte{
		"missing":    dependencyRegistry(t, executable, hex.EncodeToString(digest[:]), "2.0.0", 0),
		"duplicate":  dependencyRegistry(t, executable, hex.EncodeToString(digest[:]), "2.0.0", 2),
		"uppercase":  dependencyRegistry(t, executable, strings.ToUpper(hex.EncodeToString(digest[:])), "2.0.0", 1),
		"prerelease": dependencyRegistry(t, executable, hex.EncodeToString(digest[:]), "2.0.0-rc.1", 1),
		"build":      dependencyRegistry(t, executable, hex.EncodeToString(digest[:]), "2.0.0+build", 1),
		"future":     dependencyRegistry(t, executable, hex.EncodeToString(digest[:]), "2.1.0", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DependencyFromRegistry(data); err == nil {
				t.Fatal("incompatible registry dependency was accepted")
			}
		})
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
		"schema_version": 2, "contract_version": contract, "local_dependencies": dependencies,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
