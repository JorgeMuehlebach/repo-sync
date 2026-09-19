package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/JorgeMuehlebach/repo-sync/internal/atomicfile"
	"github.com/JorgeMuehlebach/repo-sync/internal/discovery"
	"github.com/JorgeMuehlebach/repo-sync/internal/machinelock"
	"gopkg.in/yaml.v3"
)

const (
	DefaultInterval = 5 * time.Minute
	MaxRepositories = 256
)

type Mode string

const (
	ModePublish Mode = "publish"
	ModeMirror  Mode = "mirror"
)

var repositoryIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,127}$`)
var dottedIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:[.-][a-z0-9]+)+$`)

// Repository accepts both the original scalar URL form and the structured v1
// form. Resolved checkout paths deliberately remain in machine-local state.
type Repository struct {
	ID       string `yaml:"id,omitempty"`
	URL      string `yaml:"url"`
	Mode     Mode   `yaml:"mode,omitempty"`
	SourceID string `yaml:"source_id,omitempty"`
	Revision uint64 `yaml:"revision,omitempty"`
	legacy   bool
}

func LegacyRepository(rawURL string) Repository {
	return Repository{URL: rawURL, Mode: ModePublish, legacy: true}
}

func StructuredRepository(id, rawURL string, mode Mode, sourceID ...string) Repository {
	repository := Repository{ID: id, URL: rawURL, Mode: mode, Revision: 1}
	if len(sourceID) > 0 {
		repository.SourceID = sourceID[0]
	}
	return repository
}

func (r Repository) EffectiveMode() Mode {
	if r.Mode == "" {
		return ModePublish
	}
	return r.Mode
}

func (r Repository) IsLegacy() bool { return r.legacy }

// EffectiveRevision gives pre-revision structured entries a deterministic
// initial generation while preserving legacy scalar readability.
func (r Repository) EffectiveRevision() uint64 {
	if r.Revision == 0 {
		return 1
	}
	return r.Revision
}

func (r Repository) StateKey() (string, error) {
	if r.ID != "" {
		return r.ID, nil
	}
	spec, err := discovery.ParseGitHubBranchURL(r.URL)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(spec.StateKey()))
	return fmt.Sprintf("legacy-%x", digest[:8]), nil
}

func (r Repository) LegacyStateKey() (string, error) {
	spec, err := discovery.ParseGitHubBranchURL(r.URL)
	if err != nil {
		return "", err
	}
	return spec.StateKey(), nil
}

func (r *Repository) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" || node.Value == "" {
			return fmt.Errorf("repository scalar must be a non-empty URL")
		}
		*r = LegacyRepository(node.Value)
		return nil
	case yaml.MappingNode:
		allowed := map[string]bool{"id": true, "url": true, "mode": true, "source_id": true, "revision": true}
		seen := make(map[string]bool)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index].Value
			if !allowed[key] {
				return fmt.Errorf("unknown repository field %q", key)
			}
			if seen[key] {
				return fmt.Errorf("duplicate repository field %q", key)
			}
			seen[key] = true
		}
		type plain Repository
		var decoded plain
		if err := node.Decode(&decoded); err != nil {
			return err
		}
		*r = Repository(decoded)
		return nil
	default:
		return fmt.Errorf("repository entry must be a URL or mapping")
	}
}

func (r Repository) MarshalYAML() (any, error) {
	if r.legacy {
		return r.URL, nil
	}
	type plain Repository
	return plain(r), nil
}

type Config struct {
	Interval     string       `yaml:"interval"`
	Repositories []Repository `yaml:"repositories"`
	SearchRoots  []string     `yaml:"search_roots,omitempty"`
}

func Default() Config {
	return Config{Interval: DefaultInterval.String(), Repositories: []Repository{}}
}

func DefaultDir() (string, error) {
	return defaultDir()
}

func copyLegacyFiles(legacyDir, canonicalDir string) error {
	for _, name := range []string{"config.yaml", "state.json"} {
		source := filepath.Join(legacyDir, name)
		data, err := os.ReadFile(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read legacy %s: %w", source, err)
		}
		if err := os.MkdirAll(canonicalDir, 0o755); err != nil {
			return fmt.Errorf("create config directory: %w", err)
		}
		destination := filepath.Join(canonicalDir, name)
		file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("create migrated %s: %w", destination, err)
		}
		if _, err := file.Write(data); err != nil {
			_ = file.Close()
			_ = os.Remove(destination)
			return fmt.Errorf("copy legacy %s: %w", source, err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			_ = os.Remove(destination)
			return fmt.Errorf("sync migrated %s: %w", destination, err)
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(destination)
			return fmt.Errorf("close migrated %s: %w", destination, err)
		}
	}
	return nil
}

func DefaultPath() (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

func Ensure(path string) (Config, error) {
	cfg, err := Load(path)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Config{}, err
	}
	cfg = Default()
	if err := Save(path, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Default()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("multiple YAML documents are not supported")
		}
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Save(path string, cfg Config) error {
	for index := range cfg.Repositories {
		if !cfg.Repositories[index].legacy && cfg.Repositories[index].Revision == 0 {
			cfg.Repositories[index].Revision = 1
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := atomicfile.Write(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func Update(path string, update func(*Config) error) error {
	release, err := acquire(path + ".lock")
	if err != nil {
		return err
	}
	defer release()
	cfg, err := Ensure(path)
	if err != nil {
		return err
	}
	before := append([]Repository(nil), cfg.Repositories...)
	if err := update(&cfg); err != nil {
		return err
	}
	if err := updateRepositoryRevisions(before, cfg.Repositories); err != nil {
		return err
	}
	return Save(path, cfg)
}

func updateRepositoryRevisions(before, after []Repository) error {
	previous := make(map[string]Repository, len(before))
	for _, repository := range before {
		id, err := repository.StateKey()
		if err != nil {
			return err
		}
		previous[id] = repository
	}
	for index := range after {
		if after[index].legacy {
			continue
		}
		id, err := after[index].StateKey()
		if err != nil {
			return err
		}
		old, exists := previous[id]
		if !exists || old.legacy {
			after[index].Revision = 1
			continue
		}
		oldRevision := old.EffectiveRevision()
		old.Revision = 0
		candidate := after[index]
		candidate.Revision = 0
		if old == candidate {
			after[index].Revision = oldRevision
			continue
		}
		if oldRevision == ^uint64(0) {
			return fmt.Errorf("repository %q revision exhausted", id)
		}
		after[index].Revision = oldRevision + 1
	}
	return nil
}

func acquire(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	_, release, err := machinelock.Acquire(path, 5*time.Second, "config")
	return release, err
}

func (c Config) Duration() (time.Duration, error) {
	d, err := time.ParseDuration(c.Interval)
	if err != nil {
		return 0, fmt.Errorf("invalid interval %q: %w", c.Interval, err)
	}
	if d < time.Second || d > 24*time.Hour || d%time.Second != 0 {
		return 0, fmt.Errorf("interval must be a whole number of seconds between 1s and 24h")
	}
	return d, nil
}

func (c Config) Validate() error {
	if _, err := c.Duration(); err != nil {
		return err
	}
	if len(c.Repositories) > MaxRepositories {
		return fmt.Errorf("repositories cannot contain more than %d entries", MaxRepositories)
	}
	ids := make(map[string]bool)
	branchModes := make(map[string]bool)
	sourceModes := make(map[string]bool)
	sourceBranches := make(map[string]string)
	branchSources := make(map[string]string)
	for i, repo := range c.Repositories {
		if repo.URL == "" {
			return fmt.Errorf("repositories[%d].url cannot be empty", i)
		}
		mode := repo.EffectiveMode()
		if mode != ModePublish && mode != ModeMirror {
			return fmt.Errorf("repositories[%d].mode must be publish or mirror", i)
		}
		if !repo.legacy {
			if !repositoryIDPattern.MatchString(repo.ID) {
				return fmt.Errorf("repositories[%d].id is invalid", i)
			}
			if repo.Mode == "" {
				return fmt.Errorf("repositories[%d].mode is required", i)
			}
			if len(repo.SourceID) > 160 || !dottedIDPattern.MatchString(repo.SourceID) {
				return fmt.Errorf("repositories[%d].source_id is invalid", i)
			}
		}
		spec, err := discovery.ParseGitHubBranchURL(repo.URL)
		if err != nil {
			return fmt.Errorf("repositories[%d]: %w", i, err)
		}
		if !validGitBranch(spec.Branch) {
			return fmt.Errorf("repositories[%d]: branch is invalid", i)
		}
		if mode == ModeMirror && spec.Branch != "main" {
			return fmt.Errorf("repositories[%d]: mirror mode requires branch main", i)
		}
		id, err := repo.StateKey()
		if err != nil {
			return fmt.Errorf("repositories[%d]: %w", i, err)
		}
		if ids[id] {
			return fmt.Errorf("duplicate repository id %q", id)
		}
		ids[id] = true
		branchMode := spec.StateKey() + "\x00" + string(mode)
		if branchModes[branchMode] {
			return fmt.Errorf("duplicate repository branch and mode %q (%s)", spec.StateKey(), mode)
		}
		branchModes[branchMode] = true
		if repo.SourceID == "" {
			continue
		}
		sourceMode := repo.SourceID + "\x00" + string(mode)
		if sourceModes[sourceMode] {
			return fmt.Errorf("duplicate source and mode %q (%s)", repo.SourceID, mode)
		}
		sourceModes[sourceMode] = true
		if branch, ok := sourceBranches[repo.SourceID]; ok && branch != spec.StateKey() {
			return fmt.Errorf("source %q is configured for multiple repository branches", repo.SourceID)
		}
		sourceBranches[repo.SourceID] = spec.StateKey()
		if source, ok := branchSources[spec.StateKey()]; ok && source != repo.SourceID {
			return fmt.Errorf("repository branch %q is configured for multiple sources", spec.StateKey())
		}
		branchSources[spec.StateKey()] = repo.SourceID
	}
	return nil
}

func validGitBranch(value string) bool {
	if value == "" || len(value) > 255 || !utf8.ValidString(value) || value == "@" || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "-") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") || strings.Contains(value, "..") || strings.Contains(value, "//") || strings.Contains(value, "@{") || strings.ContainsRune(value, '\\') {
		return false
	}
	for _, character := range value {
		if character <= 0x20 || character == 0x7f || strings.ContainsRune("~^:?*[", character) {
			return false
		}
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(strings.ToLower(component), ".lock") {
			return false
		}
	}
	return true
}
