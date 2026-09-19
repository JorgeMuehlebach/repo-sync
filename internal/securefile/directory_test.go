package securefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyDirectoryNoReparseRejectsAncestorLink(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realDirectory := filepath.Join(root, "real", "objects")
	if err := os.MkdirAll(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyDirectoryNoReparse(realDirectory); err != nil {
		t.Fatalf("ordinary directory was rejected: %v", err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(filepath.Join(root, "real"), link); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if _, err := VerifyDirectoryNoReparse(filepath.Join(link, "objects")); err == nil {
		t.Fatal("directory beneath a symlink/reparse ancestor was accepted")
	}
}
