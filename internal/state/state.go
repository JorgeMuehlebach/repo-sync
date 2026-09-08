package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/JorgeMuehlebach/repo-sync/internal/atomicfile"
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
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	data = append(data, '\n')
	return atomicfile.Write(path, data, 0o600)
}

func Update(path string, update func(*State) error) error {
	release, err := acquire(path + ".lock")
	if err != nil {
		return err
	}
	defer release()
	current, err := Load(path)
	if err != nil {
		return err
	}
	if err := update(&current); err != nil {
		return err
	}
	return Save(path, current)
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
			return nil, fmt.Errorf("lock state: %w", err)
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > 10*time.Minute {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("state is busy; try again")
		}
		time.Sleep(25 * time.Millisecond)
	}
}
