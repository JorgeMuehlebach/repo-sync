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
	if !IsMirrorPointer(pointer) {
		t.Fatal("created Windows junction was not recognized as a stable mirror pointer")
	}
	if IsMirrorPointer(first) {
		t.Fatal("ordinary Windows directory was recognized as a stable mirror pointer")
	}
	resolved, err := ResolveMirrorPointer(pointer)
	if err != nil || !sameMirrorPath(resolved, first) {
		t.Fatalf("resolved created junction = %q, %v; want %q", resolved, err, first)
	}
	before, beforeErr := windowsMirrorPointerInfo(pointer)
	if err := backend.Replace(pointer, first, second); err != nil {
		resolved, resolveErr := backend.Resolve(pointer)
		after, afterErr := windowsMirrorPointerInfo(pointer)
		t.Fatalf("FileRenameInfoEx junction replacement is unavailable: %v (resolved %q, want %q, resolve error %v, identity unchanged %v, identity errors %v/%v)", err, resolved, second, resolveErr, beforeErr == nil && afterErr == nil && os.SameFile(before, after), beforeErr, afterErr)
	}
	resolved, err = backend.Resolve(pointer)
	if err != nil || !sameMirrorPath(resolved, second) {
		t.Fatalf("resolved pointer = %q, %v", resolved, err)
	}
	if info, err := os.Stat(first); err != nil || !info.IsDir() {
		t.Fatalf("replaced pointer damaged baseline generation: %#v, %v", info, err)
	}
}
