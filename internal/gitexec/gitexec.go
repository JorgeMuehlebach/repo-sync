package gitexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/JorgeMuehlebach/repo-sync/internal/securefile"
	"github.com/JorgeMuehlebach/repo-sync/internal/strictjson"
)

const (
	DependencyID      = "context-system.git"
	maxExecutableSize = int64(512 << 20)
)

var (
	digestPattern            = regexp.MustCompile(`^[0-9a-f]{64}$`)
	stableSemverPattern      = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	ErrDependencyUnavailable = errors.New("trusted Git dependency is unavailable")
)

// CleanupProcessSnapshots removes runner-private executable snapshots created
// by this process. The command entry point calls it only after all work has
// stopped; finalizers remain a fallback for shorter-lived runners.
func CleanupProcessSnapshots() {
	cleanupProcessSnapshots()
}

type Dependency struct {
	path   string
	digest string
}

type registryDocument struct {
	SchemaVersion     int               `json:"schema_version"`
	ContractVersion   string            `json:"contract_version"`
	LocalDependencies []json.RawMessage `json:"local_dependencies"`
}

type registryDependency struct {
	ID      string `json:"id"`
	Path    path   `json:"path"`
	SHA256  string `json:"sha256"`
	Version string `json:"version,omitempty"`
	Kind    string `json:"kind,omitempty"`
}

type path struct {
	Style string `json:"style"`
	Value string `json:"value"`
}

// DependencyFromRegistry resolves the one canonical v2 Git executable record.
// It intentionally does not consult PATH or accept an ad-hoc executable name.
func DependencyFromRegistry(data []byte) (Dependency, error) {
	var document registryDocument
	if err := strictjson.Decode(data, &document, false); err != nil {
		return Dependency{}, errors.New("Git dependency registry is invalid")
	}
	if document.SchemaVersion != 2 || !stableSemverPattern.MatchString(document.ContractVersion) || !strings.HasPrefix(document.ContractVersion, "2.0.") {
		return Dependency{}, errors.New("Git dependency registry contract is unsupported")
	}
	var selected *registryDependency
	for _, raw := range document.LocalDependencies {
		var candidate registryDependency
		if err := strictjson.Decode(raw, &candidate, true); err != nil {
			return Dependency{}, errors.New("Git dependency registry is invalid")
		}
		if candidate.ID != DependencyID {
			continue
		}
		if selected != nil {
			return Dependency{}, errors.New("Git dependency is ambiguous")
		}
		copy := candidate
		selected = &copy
	}
	wantedStyle := "posix"
	if runtime.GOOS == "windows" {
		wantedStyle = "windows"
	}
	if selected == nil || selected.Kind != "executable" || selected.Path.Style != wantedStyle || !digestPattern.MatchString(selected.SHA256) {
		return Dependency{}, errors.New("Git dependency is unavailable")
	}
	if !filepath.IsAbs(selected.Path.Value) {
		return Dependency{}, errors.New("Git dependency path is not canonical")
	}
	absolute, err := filepath.Abs(selected.Path.Value)
	if err != nil || !samePath(filepath.Clean(selected.Path.Value), absolute) {
		return Dependency{}, errors.New("Git dependency path is not canonical")
	}
	return Dependency{path: filepath.Clean(absolute), digest: selected.SHA256}, nil
}

type Result struct {
	Output    string
	RawOutput []byte
	ExitCode  int
	Err       error
}

type SystemRunner struct {
	dependency Dependency
	identity   os.FileInfo
	binding    *executableBinding
	// beforeVerifiedExec is a test seam for exercising the boundary between
	// verification and process creation. Production runners leave it nil.
	beforeVerifiedExec func() error
}

func NewSystemRunner(dependency Dependency) (SystemRunner, error) {
	return NewSystemRunnerContext(context.Background(), dependency)
}

func NewSystemRunnerContext(ctx context.Context, dependency Dependency) (SystemRunner, error) {
	file, identity, err := verifyExecutable(ctx, dependency, nil)
	if err != nil {
		if file != nil {
			_ = file.Close()
		}
		return SystemRunner{}, ErrDependencyUnavailable
	}
	binding, err := prepareVerifiedExecutable(ctx, file, dependency.path, dependency.digest)
	_ = file.Close()
	if err != nil {
		return SystemRunner{}, ErrDependencyUnavailable
	}
	return SystemRunner{dependency: dependency, identity: identity, binding: binding}, nil
}

func NewSystemRunnerFromRegistry(data []byte) (SystemRunner, error) {
	return NewSystemRunnerFromRegistryContext(context.Background(), data)
}

func NewSystemRunnerFromRegistryContext(ctx context.Context, data []byte) (SystemRunner, error) {
	dependency, err := DependencyFromRegistry(data)
	if err != nil {
		return SystemRunner{}, err
	}
	return NewSystemRunnerContext(ctx, dependency)
}

func (r SystemRunner) Run(ctx context.Context, dir string, args ...string) Result {
	return r.run(ctx, dir, nil, nil, args...)
}

func (r SystemRunner) RunEnv(ctx context.Context, dir string, environment map[string]string, args ...string) Result {
	return r.run(ctx, dir, nil, environment, args...)
}

func (r SystemRunner) RunInput(ctx context.Context, dir string, input []byte, environment map[string]string, args ...string) Result {
	return r.run(ctx, dir, input, environment, args...)
}

func (r SystemRunner) run(ctx context.Context, dir string, input []byte, environment map[string]string, args ...string) Result {
	if r.identity == nil {
		return Result{Err: ErrDependencyUnavailable, ExitCode: -1}
	}
	retained, _, err := verifyExecutable(ctx, r.dependency, r.identity)
	if err != nil {
		return Result{Err: ErrDependencyUnavailable, ExitCode: -1}
	}
	defer retained.Close()

	isolationRoot, err := os.MkdirTemp("", "repo-sync-git-isolation-*")
	if err != nil {
		return Result{Err: err, ExitCode: -1}
	}
	defer os.RemoveAll(isolationRoot)
	if err := os.Chmod(isolationRoot, 0o700); err != nil {
		return Result{Err: err, ExitCode: -1}
	}
	hooksPath := filepath.Join(isolationRoot, "hooks")
	if err := os.Mkdir(hooksPath, 0o700); err != nil {
		return Result{Err: err, ExitCode: -1}
	}
	attributesPath := filepath.Join(isolationRoot, "attributes")
	if err := os.WriteFile(attributesPath, nil, 0o600); err != nil {
		return Result{Err: err, ExitCode: -1}
	}
	commandArgs := append([]string{
		"--no-replace-objects",
		"-c", "core.hooksPath=" + hooksPath,
		"-c", "core.fsmonitor=false",
		"-c", "core.untrackedCache=false",
		"-c", "core.autocrlf=false",
		"-c", "core.safecrlf=false",
		"-c", "core.attributesFile=" + attributesPath,
		"-c", "core.pager=cat",
		"-c", "submodule.recurse=false",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "push.recurseSubmodules=false",
		"-c", "push.followTags=false",
		"-c", "rebase.autoStash=false",
		"-c", "rebase.updateRefs=false",
		"-c", "maintenance.auto=false",
		"-c", "gc.auto=0",
		"-c", "commit.gpgSign=false",
		"-c", "push.gpgSign=false",
		"-c", "tag.gpgSign=false",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "protocol.ssh.allow=always",
		"-c", "protocol.file.allow=never",
		"-c", "protocol.ext.allow=never",
	}, args...)
	if r.beforeVerifiedExec != nil {
		if err := r.beforeVerifiedExec(); err != nil {
			return Result{Err: err, ExitCode: -1}
		}
	}
	command, err := commandForVerifiedExecutable(ctx, r.binding, retained, r.dependency.path, commandArgs...)
	if err != nil {
		return Result{Err: ErrDependencyUnavailable, ExitCode: -1}
	}
	command.Dir = dir
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		upperName := strings.ToUpper(name)
		if !strings.HasPrefix(upperName, "GIT_") && !strings.HasPrefix(upperName, "GCM_") && upperName != "SSH_ASKPASS" && upperName != "SSH_ASKPASS_REQUIRE" {
			command.Env = append(command.Env, value)
		}
	}
	trustedEnvironment := map[string]string{
		"GIT_TERMINAL_PROMPT": "0", "GCM_INTERACTIVE": "never", "GIT_NO_LAZY_FETCH": "1",
		"GIT_OPTIONAL_LOCKS": "0",
		"GIT_EDITOR":         "true", "GIT_SEQUENCE_EDITOR": "true", "GIT_MERGE_AUTOEDIT": "no",
		"GIT_PAGER": "cat", "GIT_ATTR_NOSYSTEM": "1", "SSH_ASKPASS_REQUIRE": "never",
	}
	for key, value := range environment {
		upperKey := strings.ToUpper(key)
		switch upperKey {
		case "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_ATTR_NOSYSTEM":
			trustedEnvironment[upperKey] = value
		default:
			return Result{Err: fmt.Errorf("unsupported Git environment override"), ExitCode: -1}
		}
	}
	for key, value := range trustedEnvironment {
		command.Env = append(command.Env, key+"="+value)
	}
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	output, runErr := command.CombinedOutput()
	runtime.KeepAlive(r.binding)
	result := Result{Output: strings.TrimSpace(string(output)), RawOutput: output, Err: runErr}
	if runErr == nil {
		return result
	}
	result.ExitCode = -1
	if exitErr, ok := runErr.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
	}
	return result
}

func verifyExecutable(ctx context.Context, dependency Dependency, expected os.FileInfo) (*os.File, os.FileInfo, error) {
	if dependency.path == "" || !digestPattern.MatchString(dependency.digest) {
		return nil, nil, errors.New("dependency is incomplete")
	}
	file, err := securefile.OpenCanonicalRegularRetained(dependency.path)
	if err != nil {
		return nil, nil, err
	}
	identity, err := file.Stat()
	if err != nil || !identity.Mode().IsRegular() || identity.Size() < 0 || identity.Size() > maxExecutableSize || (runtime.GOOS != "windows" && identity.Mode()&0o111 == 0) || (expected != nil && !os.SameFile(expected, identity)) {
		_ = file.Close()
		return nil, nil, errors.New("dependency identity changed")
	}
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	var read int64
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return nil, nil, err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			read += int64(count)
			if read > maxExecutableSize {
				_ = file.Close()
				return nil, nil, errors.New("dependency exceeds size limit")
			}
			_, _ = hash.Write(buffer[:count])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = file.Close()
			return nil, nil, readErr
		}
	}
	if read != identity.Size() || hex.EncodeToString(hash.Sum(nil)) != dependency.digest {
		_ = file.Close()
		return nil, nil, errors.New("dependency digest changed")
	}
	live, err := os.Lstat(dependency.path)
	if err != nil || live.Mode()&os.ModeSymlink != 0 || !os.SameFile(identity, live) || live.Size() != identity.Size() || !live.ModTime().Equal(identity.ModTime()) {
		_ = file.Close()
		return nil, nil, errors.New("dependency changed while hashing")
	}
	return file, identity, nil
}

func samePath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}
