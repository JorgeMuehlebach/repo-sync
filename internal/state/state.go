package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Repository struct {
	Path      string    `json:"path"`
	LastSync  time.Time `json:"last_sync,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

type State struct {
	Enabled      bool                  `json:"enabled"`
	Repositories map[string]Repository `json:"repositories"`
}

func New() State {
	return State{Repositories: make(map[string]Repository)}
}

func Load(path string) (State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return State{}, err
	}
	result := New()
	if err := json.Unmarshal(data, &result); err != nil {
		return State{}, fmt.Errorf("parse state %s: %w", path, err)
	}
	if result.Repositories == nil {
		result.Repositories = make(map[string]Repository)
	}
	return result, nil
}

func Save(path string, value State) error {
	if value.Repositories == nil {
		value.Repositories = make(map[string]Repository)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		// Windows does not replace an existing destination with os.Rename.
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(tmpPath, path); err != nil {
			return fmt.Errorf("replace state %s: %w", path, err)
		}
	}
	return nil
}
