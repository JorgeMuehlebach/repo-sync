//go:build !windows

package config

import (
	"fmt"
	"os"
	"path/filepath"
)

func defaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(base, "repo-sync"), nil
}
