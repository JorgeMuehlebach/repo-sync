//go:build !windows

package gitexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

var processSnapshots = struct {
	sync.Mutex
	roots map[string]struct{}
}{roots: make(map[string]struct{})}

type executableBinding struct {
	path     string
	root     string
	identity os.FileInfo
}

// prepareVerifiedExecutable makes one runner-lifetime snapshot from the
// retained, verified descriptor. Each invocation still re-verifies the system
// dependency, but executes these immutable known-good bytes. Reusing the
// private snapshot also avoids macOS code-signing and filesystem overhead on
// every short Git subprocess.
func prepareVerifiedExecutable(ctx context.Context, retained *os.File, _ string, expectedDigest string) (*executableBinding, error) {
	if retained == nil {
		return nil, fmt.Errorf("verified executable is unavailable")
	}
	root, err := os.MkdirTemp("", "repo-sync-git-executable-*")
	if err != nil {
		return nil, err
	}
	valid := false
	defer func() {
		if !valid {
			_ = os.RemoveAll(root)
		}
	}()
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(root, "git")
	if err := copyVerifiedExecutable(ctx, retained, path, expectedDigest); err != nil {
		return nil, err
	}
	identity, err := os.Lstat(path)
	if err != nil || !identity.Mode().IsRegular() || identity.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("private executable snapshot is invalid")
	}
	binding := &executableBinding{path: path, root: root, identity: identity}
	processSnapshots.Lock()
	processSnapshots.roots[root] = struct{}{}
	processSnapshots.Unlock()
	runtime.SetFinalizer(binding, releaseExecutableBinding)
	valid = true
	return binding, nil
}

func releaseExecutableBinding(binding *executableBinding) {
	if binding == nil || binding.root == "" {
		return
	}
	processSnapshots.Lock()
	delete(processSnapshots.roots, binding.root)
	processSnapshots.Unlock()
	_ = os.RemoveAll(binding.root)
}

func cleanupProcessSnapshots() {
	processSnapshots.Lock()
	roots := make([]string, 0, len(processSnapshots.roots))
	for root := range processSnapshots.roots {
		roots = append(roots, root)
	}
	clear(processSnapshots.roots)
	processSnapshots.Unlock()
	for _, root := range roots {
		_ = os.RemoveAll(root)
	}
}

func copyVerifiedExecutable(ctx context.Context, retained *os.File, destination, expectedDigest string) error {
	if _, err := retained.Seek(0, io.SeekStart); err != nil {
		return err
	}
	snapshot, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = snapshot.Close()
		}
	}()
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	var copied int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, readErr := retained.Read(buffer)
		if count > 0 {
			copied += int64(count)
			if copied > maxExecutableSize {
				return fmt.Errorf("verified executable exceeds size limit")
			}
			if _, err := snapshot.Write(buffer[:count]); err != nil {
				return err
			}
			_, _ = hash.Write(buffer[:count])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if copied == 0 || hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return fmt.Errorf("verified executable changed while snapshotting")
	}
	// This file is consumed immediately and is not durable state. Close makes
	// all writes visible to exec; fsync would add latency without improving its
	// identity or crash semantics.
	if err := snapshot.Close(); err != nil {
		return err
	}
	closed = true
	return nil
}

func commandForVerifiedExecutable(ctx context.Context, binding *executableBinding, _ *os.File, _ string, args ...string) (*exec.Cmd, error) {
	if binding == nil || binding.path == "" || binding.identity == nil {
		return nil, fmt.Errorf("verified executable is unavailable")
	}
	live, err := os.Lstat(binding.path)
	if err != nil || !live.Mode().IsRegular() || !os.SameFile(binding.identity, live) || live.Size() != binding.identity.Size() || !live.ModTime().Equal(binding.identity.ModTime()) {
		return nil, fmt.Errorf("private executable snapshot changed")
	}
	return exec.CommandContext(ctx, binding.path, args...), nil
}
