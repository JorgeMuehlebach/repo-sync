//go:build !windows

package gitops

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/JorgeMuehlebach/repo-sync/internal/securefile"
)

type systemMirrorPointerBackend struct{}

func (systemMirrorPointerBackend) Resolve(exposed string) (string, error) {
	if err := requireCanonicalMirrorPointerPath(exposed); err != nil {
		return "", err
	}
	info, err := os.Lstat(exposed)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("mirror pointer is not a symbolic link")
	}
	target, err := os.Readlink(exposed)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(exposed), target)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", err
	}
	target = filepath.Clean(target)
	targetInfo, err := os.Stat(target)
	if err != nil || !targetInfo.IsDir() {
		return "", fmt.Errorf("mirror pointer target is not a directory")
	}
	return target, nil
}

func (backend systemMirrorPointerBackend) Create(exposed, target string) error {
	if err := requireCanonicalMirrorPointerPath(exposed); err != nil {
		return err
	}
	if err := requireCanonicalMirrorGenerationPath(target); err != nil {
		return err
	}
	if err := os.Symlink(target, exposed); err != nil {
		return err
	}
	if err := syncMirrorDirectory(filepath.Dir(exposed)); err != nil {
		_ = os.Remove(exposed)
		return err
	}
	resolved, err := backend.Resolve(exposed)
	if err != nil || !sameMirrorPath(resolved, target) {
		_ = os.Remove(exposed)
		return fmt.Errorf("created mirror pointer has the wrong target")
	}
	return nil
}

func (backend systemMirrorPointerBackend) Replace(exposed, expected, next string) error {
	if err := requireCanonicalMirrorPointerPath(exposed); err != nil {
		return &mirrorAtomicPointerError{err: err}
	}
	if err := requireCanonicalMirrorGenerationPath(expected); err != nil {
		return &mirrorAtomicPointerError{err: err}
	}
	if err := requireCanonicalMirrorGenerationPath(next); err != nil {
		return &mirrorAtomicPointerError{err: err}
	}
	resolved, err := backend.Resolve(exposed)
	if err != nil || !sameMirrorPath(resolved, expected) {
		return &mirrorAtomicPointerError{err: fmt.Errorf("mirror pointer changed before replacement")}
	}
	before, err := os.Lstat(exposed)
	if err != nil || before.Mode()&os.ModeSymlink == 0 {
		return &mirrorAtomicPointerError{err: fmt.Errorf("mirror pointer identity is invalid")}
	}
	id, err := newMirrorTransactionID()
	if err != nil {
		return &mirrorAtomicPointerError{err: err}
	}
	temporary := filepath.Join(filepath.Dir(exposed), ".repo-sync-pointer-"+id)
	if err := os.Symlink(next, temporary); err != nil {
		return &mirrorAtomicPointerError{err: err}
	}
	defer os.Remove(temporary)
	after, err := os.Lstat(exposed)
	if err != nil || !os.SameFile(before, after) {
		return &mirrorAtomicPointerError{err: fmt.Errorf("mirror pointer changed during replacement")}
	}
	resolved, err = backend.Resolve(exposed)
	if err != nil || !sameMirrorPath(resolved, expected) {
		return &mirrorAtomicPointerError{err: fmt.Errorf("mirror pointer target changed during replacement")}
	}
	if err := os.Rename(temporary, exposed); err != nil {
		return &mirrorAtomicPointerError{err: err}
	}
	if err := syncMirrorDirectory(filepath.Dir(exposed)); err != nil {
		return &mirrorAtomicPointerError{err: err}
	}
	resolved, err = backend.Resolve(exposed)
	if err != nil || !sameMirrorPath(resolved, next) {
		return &mirrorAtomicPointerError{err: fmt.Errorf("atomic replacement did not expose the next generation")}
	}
	return nil
}

func requireCanonicalMirrorPointerPath(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil || filepath.Clean(path) != absolute || filepath.Base(path) == "." {
		return fmt.Errorf("mirror pointer path is not absolute and canonical")
	}
	if _, err := securefile.VerifyDirectoryNoReparse(filepath.Dir(path)); err != nil {
		return fmt.Errorf("mirror pointer parent is not a directory")
	}
	return nil
}

func requireCanonicalMirrorGenerationPath(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil || filepath.Clean(path) != absolute {
		return fmt.Errorf("mirror generation path is not absolute and canonical")
	}
	if _, err := securefile.VerifyDirectoryNoReparse(path); err != nil {
		return fmt.Errorf("mirror generation is not a directory")
	}
	return nil
}

func syncMirrorDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func mirrorPathCaseInsensitive() bool { return false }

func normalizeMirrorPathForComparison(path string) string { return filepath.Clean(path) }

func isMirrorPointer(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}
