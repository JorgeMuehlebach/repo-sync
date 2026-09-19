//go:build windows

package gitops

import (
	"os"
	"path/filepath"
	"testing"
)

// This is intentionally a runtime contract test, not a mocked API test. A
// Windows release is unsupported unless FileRenameInfoEx can atomically replace
// a real directory junction on the deployment filesystem.
func TestWindowsMirrorPointerAtomicReplacementRuntime(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	pointer := filepath.Join(root, "current")
	for _, path := range []string{first, second} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	backend := systemMirrorPointerBackend{}
	if err := backend.Create(pointer, first); err != nil {
		t.Fatalf("create junction: %v", err)
	}
	if err := backend.Replace(pointer, first, second); err != nil {
		t.Fatalf("FileRenameInfoEx junction replacement is unavailable: %v", err)
	}
	resolved, err := backend.Resolve(pointer)
	if err != nil || !sameMirrorPath(resolved, second) {
		t.Fatalf("resolved pointer = %q, %v", resolved, err)
	}
	if info, err := os.Stat(first); err != nil || !info.IsDir() {
		t.Fatalf("replaced pointer damaged baseline generation: %#v, %v", info, err)
	}
}
