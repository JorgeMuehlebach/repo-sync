package config

import (
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
