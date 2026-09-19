//go:build windows

package app

import (
	"os"
	"time"

	"github.com/JorgeMuehlebach/repo-sync/internal/machinelock"
)

func lockContextStatusFile(path string, timeout time.Duration) (*os.File, func(), error) {
	return lockApplicationFile(path, timeout, "context status")
}

func lockRepositoryOperationFile(path string, timeout time.Duration) (*os.File, func(), error) {
	return lockApplicationFile(path, timeout, "repository operation")
}

func lockApplicationFile(path string, timeout time.Duration, purpose string) (*os.File, func(), error) {
	return machinelock.Acquire(path, timeout, purpose)
}
