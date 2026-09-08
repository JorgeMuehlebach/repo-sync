package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(base, "repo-sync"), nil
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
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
