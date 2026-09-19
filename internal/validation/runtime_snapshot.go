package validation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/JorgeMuehlebach/repo-sync/internal/securefile"
)

type preparedRuntime struct {
	root       string
	rootInfo   os.FileInfo
	contextctl runtimeArtifact
	registry   runtimeArtifact
	trustState runtimeArtifact
}

type runtimeArtifact struct {
	path       string
	digest     string
	identity   os.FileInfo
	retained   *os.File
	executable bool
}

func prepareRuntime(ctx context.Context, request Request) (preparedRuntime, error) {
	parent := filepath.Dir(request.Runtime.RegistryPath)
	if _, err := securefile.VerifyDirectoryNoReparse(parent); err != nil {
		return preparedRuntime{}, &ProtocolError{Code: CodeUnavailable}
	}
	root, err := os.MkdirTemp(parent, ".repo-sync-validation-*")
	if err != nil {
		return preparedRuntime{}, &ProtocolError{Code: CodeUnavailable}
	}
	canonicalRoot, canonicalErr := filepath.EvalSymlinks(root)
	if canonicalErr == nil {
		canonicalRoot, canonicalErr = filepath.Abs(canonicalRoot)
	}
	if canonicalErr != nil || filepath.Clean(canonicalRoot) != canonicalRoot || pathsOverlap(canonicalRoot, request.SourceRoot) || pathsOverlap(canonicalRoot, request.RepositoryPath) || os.Chmod(canonicalRoot, 0o700) != nil {
		_ = os.RemoveAll(root)
		return preparedRuntime{}, &ProtocolError{Code: CodeUnavailable}
	}
	prepared := preparedRuntime{root: canonicalRoot}
	fail := func(code string) (preparedRuntime, error) {
		prepared.close()
		return preparedRuntime{}, &ProtocolError{Code: code}
	}
	contextctlName := "contextctl"
	if runtime.GOOS == "windows" {
		contextctlName += ".exe"
	}
	prepared.contextctl, err = createRuntimeSnapshot(ctx, request.Runtime.ContextctlPath, filepath.Join(canonicalRoot, contextctlName), request.Runtime.ContextctlDigest, maxContextctlBytes, true, request.SourceRoot, request.RepositoryPath)
	if err != nil {
		return fail(CodeUnavailable)
	}
	prepared.registry, err = createRuntimeSnapshot(ctx, request.Runtime.RegistryPath, filepath.Join(canonicalRoot, "registry.json"), request.Runtime.RegistryDigest, maxRuntimeJSONBytes, false, request.SourceRoot, request.RepositoryPath)
	if err != nil {
		return fail(CodeUnavailable)
	}
	prepared.trustState, err = createRuntimeSnapshot(ctx, request.Runtime.TrustStatePath, filepath.Join(canonicalRoot, "trust-state.json"), request.Runtime.TrustStateDigest, maxRuntimeJSONBytes, false, request.SourceRoot, request.RepositoryPath)
	if err != nil {
		return fail(CodeTrustTamper)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(canonicalRoot, 0o500); err != nil {
			return fail(CodeUnavailable)
		}
	}
	prepared.rootInfo, err = os.Lstat(canonicalRoot)
	if err != nil || prepared.rootInfo.Mode()&os.ModeSymlink != 0 || !prepared.rootInfo.IsDir() || (runtime.GOOS != "windows" && prepared.rootInfo.Mode().Perm()&0o222 != 0) {
		return fail(CodeUnavailable)
	}
	return prepared, nil
}

func pathsOverlap(left, right string) bool {
	left, leftErr := filepath.EvalSymlinks(left)
	if leftErr == nil {
		left, leftErr = filepath.Abs(left)
	}
	right, rightErr := filepath.EvalSymlinks(right)
	if rightErr == nil {
		right, rightErr = filepath.Abs(right)
	}
	if leftErr != nil || rightErr != nil {
		return true
	}
	equal := left == right
	if runtime.GOOS == "windows" {
		equal = strings.EqualFold(left, right)
	}
	return equal || within(left, right) || within(right, left)
}

func createRuntimeSnapshot(ctx context.Context, sourcePath, snapshotPath, expectedDigest string, maximumBytes int64, executable bool, repositoryPaths ...string) (runtimeArtifact, error) {
	if err := validateRuntimeSourcePath(sourcePath, repositoryPaths...); err != nil {
		return runtimeArtifact{}, err
	}
	before, err := os.Lstat(sourcePath)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > maximumBytes || (executable && runtime.GOOS != "windows" && before.Mode()&0o111 == 0) {
		return runtimeArtifact{}, fmt.Errorf("runtime source is not a bounded regular file")
	}
	source, err := securefile.OpenCanonicalRegularRetained(sourcePath)
	if err != nil {
		return runtimeArtifact{}, err
	}
	defer source.Close()
	opened, err := source.Stat()
	if err != nil || !os.SameFile(before, opened) || opened.Size() != before.Size() || !opened.ModTime().Equal(before.ModTime()) {
		return runtimeArtifact{}, fmt.Errorf("runtime source changed while opening")
	}
	destination, err := os.OpenFile(snapshotPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return runtimeArtifact{}, err
	}
	removeSnapshot := true
	defer func() {
		_ = destination.Close()
		if removeSnapshot {
			_ = os.Chmod(snapshotPath, 0o600)
			_ = os.Remove(snapshotPath)
		}
	}()
	hash := sha256.New()
	written, err := copyRuntimeBytes(ctx, io.MultiWriter(destination, hash), source, maximumBytes)
	if err != nil || written != opened.Size() || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return runtimeArtifact{}, fmt.Errorf("runtime source digest changed")
	}
	if err := destination.Sync(); err != nil {
		return runtimeArtifact{}, err
	}
	mode := os.FileMode(0o400)
	if executable {
		mode = 0o500
	}
	if err := destination.Chmod(mode); err != nil {
		return runtimeArtifact{}, err
	}
	if err := destination.Close(); err != nil {
		return runtimeArtifact{}, err
	}
	after, err := os.Lstat(sourcePath)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return runtimeArtifact{}, fmt.Errorf("runtime source changed while snapshotting")
	}
	retained, err := securefile.OpenCanonicalRegularRetained(snapshotPath)
	if err != nil {
		return runtimeArtifact{}, err
	}
	identity, err := retained.Stat()
	if err != nil || !identity.Mode().IsRegular() || identity.Size() != written || (executable && runtime.GOOS != "windows" && identity.Mode()&0o111 == 0) {
		_ = retained.Close()
		return runtimeArtifact{}, fmt.Errorf("runtime snapshot identity is invalid")
	}
	artifact := runtimeArtifact{path: snapshotPath, digest: expectedDigest, identity: identity, retained: retained, executable: executable}
	if err := artifact.verify(ctx); err != nil {
		_ = retained.Close()
		return runtimeArtifact{}, err
	}
	removeSnapshot = false
	return artifact, nil
}

func validateRuntimeSourcePath(path string, repositoryPaths ...string) error {
	absolute, err := filepath.Abs(path)
	if err != nil || filepath.Clean(path) != absolute {
		return fmt.Errorf("runtime path is not canonical")
	}
	for _, repositoryPath := range repositoryPaths {
		canonical, err := filepath.EvalSymlinks(repositoryPath)
		if err != nil {
			return err
		}
		canonical, err = filepath.Abs(canonical)
		if err != nil || absolute == canonical || within(absolute, canonical) {
			return fmt.Errorf("runtime path is inside candidate repository")
		}
	}
	return nil
}

func copyRuntimeBytes(ctx context.Context, destination io.Writer, source io.Reader, maximumBytes int64) (int64, error) {
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			total += int64(count)
			if total > maximumBytes {
				return total, fmt.Errorf("runtime artifact exceeds size limit")
			}
			written, writeErr := destination.Write(buffer[:count])
			if writeErr != nil {
				return total, writeErr
			}
			if written != count {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func (p preparedRuntime) verify(ctx context.Context) error {
	rootInfo, err := os.Lstat(p.root)
	if err != nil || p.rootInfo == nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() || !os.SameFile(p.rootInfo, rootInfo) || (runtime.GOOS != "windows" && rootInfo.Mode().Perm()&0o222 != 0) {
		return &ProtocolError{Code: CodeUnavailable}
	}
	if err := p.contextctl.verify(ctx); err != nil {
		return &ProtocolError{Code: CodeUnavailable}
	}
	if err := p.registry.verify(ctx); err != nil {
		return &ProtocolError{Code: CodeUnavailable}
	}
	if err := p.trustState.verify(ctx); err != nil {
		return &ProtocolError{Code: CodeTrustTamper}
	}
	return nil
}

func (a runtimeArtifact) verify(ctx context.Context) error {
	if a.retained == nil || a.identity == nil {
		return fmt.Errorf("runtime snapshot is unavailable")
	}
	live, err := os.Lstat(a.path)
	if err != nil || live.Mode()&os.ModeSymlink != 0 || !live.Mode().IsRegular() || !os.SameFile(a.identity, live) || live.Size() != a.identity.Size() || !live.ModTime().Equal(a.identity.ModTime()) || (a.executable && runtime.GOOS != "windows" && live.Mode()&0o111 == 0) {
		return fmt.Errorf("runtime snapshot identity changed")
	}
	if _, err := a.retained.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	read, err := copyRuntimeBytes(ctx, hash, a.retained, a.identity.Size())
	if err != nil || read != a.identity.Size() || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != a.digest {
		return fmt.Errorf("runtime snapshot digest changed")
	}
	return nil
}

func (p *preparedRuntime) close() {
	for _, artifact := range []*runtimeArtifact{&p.contextctl, &p.registry, &p.trustState} {
		if artifact.retained != nil {
			_ = artifact.retained.Close()
			artifact.retained = nil
		}
	}
	if p.root != "" {
		_ = os.Chmod(p.root, 0o700)
	}
	for _, artifact := range []*runtimeArtifact{&p.contextctl, &p.registry, &p.trustState} {
		if artifact.path != "" {
			_ = os.Chmod(artifact.path, 0o600)
		}
	}
	if p.root != "" {
		_ = os.RemoveAll(p.root)
	}
}
