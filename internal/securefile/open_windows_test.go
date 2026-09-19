//go:build windows

package securefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRegularBeneathRejectsReparsePointAncestorByHandle(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "nested")); err != nil {
		t.Skipf("Windows directory symlink creation is unavailable: %v", err)
	}
	file, err := OpenRegularBeneath(root, "nested/secret")
	if file != nil {
		_ = file.Close()
	}
	if err == nil {
		t.Fatal("reparse-point ancestor was accepted")
	}
}
