package config

import (
	"os"
	"path/filepath"
	"reflect"
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
