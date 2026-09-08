//go:build windows

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func defaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find user home directory: %w", err)
	}
	canonical := filepath.Join(home, ".config", "repo-sync")
	legacyBase, err := os.UserConfigDir()
	if err == nil {
		legacy := filepath.Join(legacyBase, "repo-sync")
		if !samePath(legacy, canonical) {
			if err := copyLegacyFiles(legacy, canonical); err != nil {
				return "", err
			}
		}
	}
	return canonical, nil
}

func samePath(left, right string) bool {
	left, leftErr := filepath.Abs(left)
	right, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}
