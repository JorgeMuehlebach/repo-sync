//go:build windows

package machinelock

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireDeniesLockPathDeletionAndReplacement(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "machine.lock")
	_, release, err := Acquire(path, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := os.Remove(path); err == nil {
		t.Fatal("live lock path was deleted")
	}
	replacement := filepath.Join(directory, "replacement.lock")
	if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err == nil {
		t.Fatal("live lock path was replaced")
	}
}
