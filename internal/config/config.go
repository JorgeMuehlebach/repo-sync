package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/JorgeMuehlebach/repo-sync/internal/atomicfile"
	"gopkg.in/yaml.v3"
)

const DefaultInterval = 5 * time.Minute

type Config struct {
	Interval     string   `yaml:"interval"`
	Repositories []string `yaml:"repositories"`
	SearchRoots  []string `yaml:"search_roots,omitempty"`
}

func Default() Config {
	return Config{Interval: DefaultInterval.String(), Repositories: []string{}}
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
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Save(path string, cfg Config) error {
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
	if err := update(&cfg); err != nil {
		return err
	}
	return Save(path, cfg)
}

func acquire(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
			_ = file.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("lock config: %w", err)
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > 10*time.Minute {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("config is busy; try again")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (c Config) Duration() (time.Duration, error) {
	d, err := time.ParseDuration(c.Interval)
	if err != nil {
		return 0, fmt.Errorf("invalid interval %q: %w", c.Interval, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("interval must be greater than zero")
	}
	return d, nil
}

func (c Config) Validate() error {
	if _, err := c.Duration(); err != nil {
		return err
	}
	for i, repo := range c.Repositories {
		if repo == "" {
			return fmt.Errorf("repositories[%d] cannot be empty", i)
		}
	}
	return nil
}
