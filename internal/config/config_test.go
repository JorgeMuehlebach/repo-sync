package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo-sync", "config.yaml")
	want := Config{
		Interval:     "5m",
		Repositories: []string{"https://github.com/example/docs/tree/main"},
		SearchRoots:  []string{"/work"},
	}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestConcurrentUpdatesPreserveRepositories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	const count = 12
	errorsFound := make(chan error, count)
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			err := Update(path, func(cfg *Config) error {
				cfg.Repositories = append(cfg.Repositories, "repo-"+strconv.Itoa(index))
				return nil
			})
			if err != nil {
				errorsFound <- err
			}
		}(i)
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != count {
		t.Fatalf("repository count = %d, want %d", len(cfg.Repositories), count)
	}
}

func TestEnsureCreatesDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	got, err := Ensure(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Interval != DefaultInterval.String() {
		t.Fatalf("interval = %q, want %q", got.Interval, DefaultInterval)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default config was not created: %v", err)
	}
}

func TestDurationRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"nope", "0s", "-1m"} {
		cfg := Config{Interval: value}
		if _, err := cfg.Duration(); err == nil {
			t.Fatalf("Duration(%q) returned no error", value)
		}
	}
	if got, err := (Config{Interval: "5m"}).Duration(); err != nil || got != 5*time.Minute {
		t.Fatalf("Duration(5m) = %v, %v", got, err)
	}
}

func TestCopyLegacyFilesPreservesExistingAndLeavesLegacy(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "legacy")
	canonical := filepath.Join(root, "canonical")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "config.yaml"), []byte("legacy config"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "state.json"), []byte("legacy state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "repo-sync.log"), []byte("do not copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "config.yaml"), []byte("canonical config"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := copyLegacyFiles(legacy, canonical); err != nil {
		t.Fatal(err)
	}

	configData, err := os.ReadFile(filepath.Join(canonical, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(configData) != "canonical config" {
		t.Fatalf("canonical config was overwritten: %q", configData)
	}
	stateData, err := os.ReadFile(filepath.Join(canonical, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stateData) != "legacy state" {
		t.Fatalf("migrated state = %q", stateData)
	}
	if _, err := os.Stat(filepath.Join(canonical, "repo-sync.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("log was migrated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "state.json")); err != nil {
		t.Fatalf("legacy state was removed: %v", err)
	}
}
