package config

import (
	"fmt"
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
		Repositories: []Repository{StructuredRepository("example-docs", "https://github.com/example/docs/tree/main", ModePublish, "example.docs")},
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

func TestLoadAcceptsLegacyScalarAndStructuredRepositories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte(`interval: 5m
repositories:
  - https://github.com/example/legacy/tree/main
  - id: example-mirror
    url: https://github.com/example/mirror/tree/main
    mode: mirror
    source_id: example.mirror
`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 2 {
		t.Fatalf("repository count = %d, want 2", len(cfg.Repositories))
	}
	if !cfg.Repositories[0].IsLegacy() || cfg.Repositories[0].EffectiveMode() != ModePublish {
		t.Fatalf("legacy repository = %#v", cfg.Repositories[0])
	}
	if cfg.Repositories[1].ID != "example-mirror" || cfg.Repositories[1].EffectiveMode() != ModeMirror {
		t.Fatalf("structured repository = %#v", cfg.Repositories[1])
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	roundTrip, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
		if !reflect.DeepEqual(roundTrip, []byte("interval: 5m\nrepositories:\n    - https://github.com/example/legacy/tree/main\n    - id: example-mirror\n      url: https://github.com/example/mirror/tree/main\n      mode: mirror\n      source_id: example.mirror\n      revision: 1\n")) {
		t.Fatalf("round-trip config =\n%s", roundTrip)
	}
}

func TestUpdateIncrementsOnlyChangedStructuredRepositoryRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	first := StructuredRepository("first", "https://github.com/example/first/tree/main", ModePublish, "example.first")
	second := StructuredRepository("second", "https://github.com/example/second/tree/main", ModePublish, "example.second")
	first.Revision = 7
	second.Revision = 11
	if err := Save(path, Config{Interval: "5m", Repositories: []Repository{first, second}}); err != nil {
		t.Fatal(err)
	}
	if err := Update(path, func(cfg *Config) error {
		cfg.Repositories[0].SourceID = "example.changed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Repositories[0].Revision; got != 8 {
		t.Fatalf("changed repository revision = %d, want 8", got)
	}
	if got := cfg.Repositories[1].Revision; got != 11 {
		t.Fatalf("unchanged repository revision = %d, want 11", got)
	}
}

func TestValidateRejectsUnsafeRepositoryDefinitions(t *testing.T) {
	tests := map[string]Config{
		"mirror branch": {Interval: "5m", Repositories: []Repository{StructuredRepository("mirror", "https://github.com/example/repo/tree/feature", ModeMirror, "example.mirror")}},
		"duplicate id": {Interval: "5m", Repositories: []Repository{
			StructuredRepository("same", "https://github.com/example/one/tree/main", ModePublish, "example.one"),
			StructuredRepository("same", "https://github.com/example/two/tree/main", ModePublish, "example.two"),
		}},
		"duplicate branch": {Interval: "5m", Repositories: []Repository{
			StructuredRepository("one", "https://github.com/example/repo/tree/main", ModePublish, "example.one"),
			StructuredRepository("two", "https://github.com/example/repo/tree/main", ModePublish, "example.two"),
		}},
		"duplicate source mode": {Interval: "5m", Repositories: []Repository{
			StructuredRepository("one", "https://github.com/example/one/tree/main", ModePublish, "example.source"),
			StructuredRepository("two", "https://github.com/example/two/tree/main", ModePublish, "example.source"),
		}},
		"source split across branches": {Interval: "5m", Repositories: []Repository{
			StructuredRepository("publisher", "https://github.com/example/one/tree/main", ModePublish, "example.source"),
			StructuredRepository("mirror", "https://github.com/example/two/tree/main", ModeMirror, "example.source"),
		}},
		"branch split across sources": {Interval: "5m", Repositories: []Repository{
			StructuredRepository("publisher", "https://github.com/example/repo/tree/main", ModePublish, "example.publisher"),
			StructuredRepository("mirror", "https://github.com/example/repo/tree/main", ModeMirror, "example.mirror"),
		}},
		"invalid id":     {Interval: "5m", Repositories: []Repository{StructuredRepository("Bad ID", "https://github.com/example/repo/tree/main", ModePublish, "example.source")}},
		"invalid branch": {Interval: "5m", Repositories: []Repository{StructuredRepository("bad-branch", "https://github.com/example/repo/tree/feature%20name", ModePublish, "example.source")}},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() returned no error")
			}
		})
	}
}

func TestValidateAllowsOnePublisherAndMirrorForSameSourceBranch(t *testing.T) {
	cfg := Config{Interval: "5m", Repositories: []Repository{
		StructuredRepository("publisher", "https://github.com/example/repo/tree/main", ModePublish, "example.source"),
		StructuredRepository("mirror", "https://github.com/example/repo/tree/main", ModeMirror, "example.source"),
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
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
				cfg.Repositories = append(cfg.Repositories, LegacyRepository("https://github.com/example/repo-"+strconv.Itoa(index)+"/tree/main"))
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
	for _, value := range []string{"nope", "0s", "-1m", "500ms", "24h1s"} {
		cfg := Config{Interval: value}
		if _, err := cfg.Duration(); err == nil {
			t.Fatalf("Duration(%q) returned no error", value)
		}
	}
	if got, err := (Config{Interval: "5m"}).Duration(); err != nil || got != 5*time.Minute {
		t.Fatalf("Duration(5m) = %v, %v", got, err)
	}
}

func TestLoadRejectsUnknownDuplicateAndMultipleDocuments(t *testing.T) {
	tests := map[string]string{
		"unknown top field":          "interval: 5m\nrepositories: []\nunexpected: true\n",
		"duplicate repository field": "interval: 5m\nrepositories:\n  - id: first\n    id: second\n    url: https://github.com/example/repo/tree/main\n    mode: publish\n    source_id: example.source\n",
		"multiple documents":         "interval: 5m\nrepositories: []\n---\ninterval: 10m\nrepositories: []\n",
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load() accepted ambiguous configuration")
			}
		})
	}
}

func TestValidateCapsRepositoryCount(t *testing.T) {
	cfg := Config{Interval: "5m"}
	for index := 0; index <= MaxRepositories; index++ {
		cfg.Repositories = append(cfg.Repositories, StructuredRepository(
			fmt.Sprintf("repo-%03d", index),
			fmt.Sprintf("https://github.com/example/repo-%03d/tree/main", index),
			ModePublish,
			fmt.Sprintf("example.source-%03d", index),
		))
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted more repositories than the status contract")
	}
}
