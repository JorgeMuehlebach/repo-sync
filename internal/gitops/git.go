package gitops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/JorgeMuehlebach/repo-sync/internal/discovery"
	"github.com/JorgeMuehlebach/repo-sync/internal/gitexec"
	"github.com/JorgeMuehlebach/repo-sync/internal/validation"
)

const defaultCommitMessage = "chore: automatic repository sync"

type Result = gitexec.Result

type Runner interface {
	Run(ctx context.Context, dir string, args ...string) Result
}

type EnvironmentRunner interface {
	RunEnv(ctx context.Context, dir string, environment map[string]string, args ...string) Result
}

type InputRunner interface {
	RunInput(ctx context.Context, dir string, input []byte, environment map[string]string, args ...string) Result
}

type GitDependency = gitexec.Dependency

type SystemRunner struct{ delegate gitexec.SystemRunner }

func GitDependencyFromRegistry(data []byte) (GitDependency, error) {
	return gitexec.DependencyFromRegistry(data)
}

func NewSystemRunner(dependency GitDependency) (SystemRunner, error) {
	runner, err := gitexec.NewSystemRunner(dependency)
	if err != nil {
		return SystemRunner{}, err
	}
	return SystemRunner{delegate: runner}, nil
}

func NewSystemRunnerContext(ctx context.Context, dependency GitDependency) (SystemRunner, error) {
	runner, err := gitexec.NewSystemRunnerContext(ctx, dependency)
	if err != nil {
		return SystemRunner{}, err
	}
	return SystemRunner{delegate: runner}, nil
}

func NewSystemRunnerFromRegistry(data []byte) (SystemRunner, error) {
	runner, err := gitexec.NewSystemRunnerFromRegistry(data)
	if err != nil {
		return SystemRunner{}, err
	}
	return SystemRunner{delegate: runner}, nil
}

func NewSystemRunnerFromRegistryContext(ctx context.Context, data []byte) (SystemRunner, error) {
	runner, err := gitexec.NewSystemRunnerFromRegistryContext(ctx, data)
	if err != nil {
		return SystemRunner{}, err
	}
	return SystemRunner{delegate: runner}, nil
}

func (r SystemRunner) Run(ctx context.Context, dir string, args ...string) Result {
	return r.delegate.Run(ctx, dir, args...)
}

func (r SystemRunner) RunEnv(ctx context.Context, dir string, environment map[string]string, args ...string) Result {
	return r.delegate.RunEnv(ctx, dir, environment, args...)
}

func (r SystemRunner) RunInput(ctx context.Context, dir string, input []byte, environment map[string]string, args ...string) Result {
	return r.delegate.RunInput(ctx, dir, input, environment, args...)
}

type Target struct {
	ID                     string
	Path                   string
	Spec                   discovery.RepositorySpec
	ValidationRuntime      validation.Runtime
	ValidationRepository   string
	ExpectedAcceptedCommit string
	Reconcile              func(context.Context) error
}

type Outcome struct {
	AcceptedCommit  string
	AcceptedTree    string
	CandidateCommit string
	CandidateTree   string
	Validation      validation.Report
}

type OperationError struct {
	Code     string
	Phase    string
	Summary  string
	Findings []validation.Finding
	Report   *validation.Report
}

func (e *OperationError) Error() string { return e.Summary }

type Engine interface {
	Sync(context.Context, Target) (Outcome, *OperationError)
}

// ValidateTarget performs the same fail-closed repository and reconciliation
// checks used at each engine boundary without fetching or mutating the checkout.
func ValidateTarget(ctx context.Context, runner Runner, target Target) *OperationError {
	return newBase(runner, validation.Unavailable{}).reconcileTarget(ctx, target, target.Spec.Branch, "candidate")
}

type base struct {
	git       Runner
	validator validation.Validator
}

func newBase(runner Runner, validator validation.Validator) base {
	if runner == nil {
		runner = SystemRunner{}
	}
	if validator == nil {
		validator = validation.Unavailable{}
	}
	return base{git: runner, validator: validator}
}

func (b base) validateRepository(ctx context.Context, target Target, expectedBranch string) *OperationError {
	root := b.git.Run(ctx, target.Path, "rev-parse", "--show-toplevel")
	if root.Err != nil {
		return commandFailure("REPO-GIT-ROOT", "candidate", "repository root validation failed", root)
	}
	wantRoot, err := filepath.Abs(target.Path)
	if err != nil {
		return internalFailure("candidate")
	}
	gotRoot, err := filepath.Abs(root.Output)
	if err != nil {
		return internalFailure("candidate")
	}
	wantInfo, wantErr := os.Stat(wantRoot)
	gotInfo, gotErr := os.Stat(gotRoot)
	if wantErr != nil || gotErr != nil || !os.SameFile(wantInfo, gotInfo) {
		return &OperationError{Code: "REPO-GIT-ROOT", Phase: "candidate", Summary: "configured path is not the repository root"}
	}
	dotGitPath := filepath.Join(gotRoot, ".git")
	dotGitInfo, dotGitErr := os.Lstat(dotGitPath)
	if dotGitErr != nil || dotGitInfo.Mode()&os.ModeSymlink != 0 || !dotGitInfo.IsDir() {
		return &OperationError{Code: "REPO-GIT-STORAGE", Phase: "candidate", Summary: "linked worktrees and redirected Git directories are not supported"}
	}
	for _, arguments := range [][]string{{"rev-parse", "--absolute-git-dir"}, {"rev-parse", "--git-common-dir"}} {
		location := b.git.Run(ctx, target.Path, arguments...)
		if location.Err != nil {
			return commandFailure("REPO-GIT-STORAGE", "candidate", "Git storage identity could not be established", location)
		}
		resolved := location.Output
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(target.Path, resolved)
		}
		resolvedInfo, err := os.Stat(resolved)
		if err != nil || !os.SameFile(dotGitInfo, resolvedInfo) {
			return &OperationError{Code: "REPO-GIT-STORAGE", Phase: "candidate", Summary: "shared or redirected Git storage is not supported"}
		}
	}
	if failure := rejectUnsafeLocalGitSettings(ctx, b.git, target.Path, "candidate"); failure != nil {
		return failure
	}
	if failure := validateOriginRemote(ctx, b.git, target.Path, target.Spec.Key, "candidate"); failure != nil {
		return failure
	}
	branch := b.git.Run(ctx, target.Path, "branch", "--show-current")
	if branch.Err != nil {
		return commandFailure("REPO-GIT-BRANCH", "candidate", "branch validation failed", branch)
	}
	if branch.Output != expectedBranch {
		return &OperationError{Code: "REPO-GIT-BRANCH", Phase: "candidate", Summary: "configured branch is not checked out"}
	}
	_, headTree, headFailure := b.commitAndTree(ctx, target.Path, "HEAD")
	if headFailure != nil {
		return headFailure
	}
	if failure := b.rejectFilterAttributes(ctx, target.Path, headTree); failure != nil {
		return failure
	}
	return b.ensureNoOperationInProgress(ctx, target.Path)
}

func (b base) reconcileTarget(ctx context.Context, target Target, expectedBranch, phase string) *OperationError {
	if target.Reconcile != nil {
		if err := target.Reconcile(ctx); err != nil {
			code := "REPO-CONTEXT-RECONCILIATION"
			if validation.ErrorCode(err) == validation.CodeMigrationNeeded {
				code = validation.CodeMigrationNeeded
			}
			return &OperationError{Code: code, Phase: phase, Summary: "pinned context validation inputs are no longer reconciled"}
		}
	}
	if failure := b.validateRepository(ctx, target, expectedBranch); failure != nil {
		failure.Phase = phase
		return failure
	}
	return nil
}

type fetchedCandidate struct {
	Commit string
	Tree   string
	Ref    string
}

func (b base) fetchRemoteCandidate(ctx context.Context, target Target, expectedBranch string) (fetchedCandidate, *OperationError) {
	if failure := b.reconcileTarget(ctx, target, expectedBranch, "fetch"); failure != nil {
		return fetchedCandidate{}, failure
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fetchedCandidate{}, internalFailure("fetch")
	}
	ref := "refs/repo-sync/candidates/" + hex.EncodeToString(nonce[:])
	fetch, mutationFailure := runSafeMutation(ctx, b.git, target.Path, "fetch",
		"fetch", "--no-tags", "--no-prune", "--no-recurse-submodules", "origin",
		"refs/heads/"+target.Spec.Branch+":"+ref,
	)
	if mutationFailure != nil {
		return fetchedCandidate{}, mutationFailure
	}
	if fetch.Err != nil {
		return fetchedCandidate{}, commandFailure("REPO-FETCH-FAILED", "fetch", "fetch failed", fetch)
	}
	commit, tree, failure := b.commitAndTree(ctx, target.Path, ref)
	if failure != nil {
		b.deleteCandidateRef(ctx, target.Path, ref, "")
		return fetchedCandidate{}, failure
	}
	return fetchedCandidate{Commit: commit, Tree: tree, Ref: ref}, nil
}

func (b base) deleteCandidateRef(ctx context.Context, repositoryPath, ref, expectedCommit string) {
	command := []string{"update-ref", "-d", ref}
	if expectedCommit != "" {
		command = append(command, expectedCommit)
	}
	_, _ = runSafeMutation(ctx, b.git, repositoryPath, "cleanup", command...)
}

func (b base) ensureNoOperationInProgress(ctx context.Context, repoPath string) *OperationError {
	for _, marker := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply", "sequencer", "index.lock"} {
		result := b.git.Run(ctx, repoPath, "rev-parse", "--git-path", marker)
		if result.Err != nil {
			return commandFailure("REPO-GIT-OPERATION", "candidate", "Git operation state could not be inspected", result)
		}
		markerPath := result.Output
		if !filepath.IsAbs(markerPath) {
			markerPath = filepath.Join(repoPath, markerPath)
		}
		if _, err := os.Stat(markerPath); err == nil {
			return &OperationError{Code: "REPO-GIT-OPERATION", Phase: "candidate", Summary: "a Git operation is in progress"}
		} else if !os.IsNotExist(err) {
			return &OperationError{Code: "REPO-GIT-OPERATION", Phase: "candidate", Summary: "Git operation state could not be inspected"}
		}
	}
	return nil
}

func (b base) requireClean(ctx context.Context, repoPath string) *OperationError {
	status := b.git.Run(ctx, repoPath, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching")
	if status.Err != nil {
		return commandFailure("REPO-GIT-STATUS", "candidate", "working-tree status failed", status)
	}
	if status.Output != "" {
		return &OperationError{Code: "REPO-GIT-DIRTY", Phase: "candidate", Summary: "mirror working tree is not clean"}
	}
	flags := b.git.Run(ctx, repoPath, "ls-files", "-v", "-z")
	if flags.Err != nil {
		return commandFailure("REPO-GIT-STATUS", "candidate", "Git index flags could not be inspected", flags)
	}
	for _, entry := range strings.Split(rawOutput(flags), "\x00") {
		if entry == "" {
			continue
		}
		prefix := entry[0]
		if prefix == 'S' || (prefix >= 'a' && prefix <= 'z') {
			return &OperationError{Code: "REPO-GIT-DIRTY", Phase: "candidate", Summary: "mirror index contains hidden worktree state"}
		}
	}
	return nil
}

func (b base) validateTree(ctx context.Context, target Target, mode validation.Mode, tree, commit string) (validation.Report, *OperationError) {
	validationRepository := target.ValidationRepository
	if validationRepository == "" {
		validationRepository = target.Path
	}
	report, err := b.validator.Validate(ctx, validation.Request{
		RepositoryID:   target.ID,
		SourceRoot:     target.Path,
		RepositoryPath: validationRepository,
		Mode:           mode,
		Tree:           tree,
		Commit:         commit,
		Runtime:        target.ValidationRuntime,
	})
	if err != nil {
		code := validation.ErrorCode(err)
		summary := "trusted validation protocol failed"
		if code == validation.CodeBundleChanged {
			summary = "trusted validator bundle identity changed"
		} else if code == validation.CodeTrustTamper {
			summary = "trusted validator state failed integrity verification"
		} else if code == validation.CodeUnavailable {
			summary = "trusted validator is unavailable"
		}
		return validation.Report{}, &OperationError{Code: code, Phase: "validation", Summary: summary}
	}
	if report.Verdict == validation.VerdictPass && report.Promotable {
		return report, nil
	}
	findings := report.Findings()
	code := "REPO-VALIDATION-FAILED"
	summary := "candidate context validation failed"
	switch report.Verdict {
	case validation.VerdictHold:
		code = "REPO-VALIDATION-HOLD"
		summary = "candidate context requires local approval or trust repair"
		for _, finding := range findings {
			if finding.CheckID == validation.CodeBundleChanged || finding.CheckID == validation.CodeTrustTamper || finding.CheckID == validation.CodeMigrationNeeded {
				code = finding.CheckID
				break
			}
		}
	case validation.VerdictIncomplete:
		code = "REPO-VALIDATION-INCOMPLETE"
		summary = "candidate context validation was incomplete"
	}
	return report, &OperationError{Code: code, Phase: "validation", Summary: summary, Findings: findings, Report: &report}
}

func (b base) commitAndTree(ctx context.Context, repoPath, revision string) (string, string, *OperationError) {
	commit := b.git.Run(ctx, repoPath, "rev-parse", revision+"^{commit}")
	if commit.Err != nil {
		return "", "", commandFailure("REPO-GIT-OBJECT", "candidate", "candidate commit could not be resolved", commit)
	}
	tree := b.git.Run(ctx, repoPath, "rev-parse", commit.Output+"^{tree}")
	if tree.Err != nil {
		return "", "", commandFailure("REPO-GIT-OBJECT", "candidate", "candidate tree could not be resolved", tree)
	}
	return commit.Output, tree.Output, nil
}

func (b base) rejectFilterAttributes(ctx context.Context, repoPath, tree string) *OperationError {
	paths := b.git.Run(ctx, repoPath, "ls-tree", "-r", "--name-only", "-z", tree)
	if paths.Err != nil {
		return commandFailure("REPO-GIT-ATTRIBUTES", "candidate", "candidate attributes could not be inspected", paths)
	}
	for _, path := range strings.Split(rawOutput(paths), "\x00") {
		if path == "" || filepath.Base(filepath.FromSlash(path)) != ".gitattributes" {
			continue
		}
		if strings.ContainsAny(path, "\r\n") || strings.Contains(path, "\\") || strings.Contains("/"+path+"/", "/../") {
			return &OperationError{Code: "REPO-GIT-ATTRIBUTES", Phase: "candidate", Summary: "candidate attributes path is unsafe"}
		}
		contents := b.git.Run(ctx, repoPath, "show", tree+":"+path)
		if contents.Err != nil {
			return commandFailure("REPO-GIT-ATTRIBUTES", "candidate", "candidate attributes could not be read", contents)
		}
		switch executableAttributeKind(contents.Output) {
		case "filter":
			return &OperationError{Code: "REPO-GIT-FILTER-ATTRIBUTE", Phase: "candidate", Summary: "candidate declares an executable Git filter"}
		case "merge":
			return &OperationError{Code: "REPO-GIT-MERGE-ATTRIBUTE", Phase: "candidate", Summary: "candidate declares a custom Git merge driver"}
		}
	}
	return nil
}

func executableAttributeKind(contents string) string {
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		for _, attribute := range fields[1:] {
			if attribute == "filter" || attribute == "-filter" || attribute == "!filter" || strings.HasPrefix(attribute, "filter=") {
				return "filter"
			}
			if attribute == "merge" || attribute == "-merge" || attribute == "!merge" || strings.HasPrefix(attribute, "merge=") {
				return "merge"
			}
		}
	}
	return ""
}

func runSafeMutation(ctx context.Context, runner Runner, repoPath, phase string, command ...string) (Result, *OperationError) {
	if failure := rejectUnsafeLocalGitSettings(ctx, runner, repoPath, phase); failure != nil {
		return Result{}, failure
	}
	environmentRunner, ok := runner.(EnvironmentRunner)
	if !ok {
		return Result{}, &OperationError{Code: "REPO-GIT-RUNNER-UNSAFE", Phase: phase, Summary: "Git runner cannot enforce a trusted mutation environment"}
	}
	hooksPath, err := os.MkdirTemp("", "repo-sync-hooks-")
	if err != nil {
		return Result{}, internalFailure(phase)
	}
	defer os.RemoveAll(hooksPath)
	if err := os.Chmod(hooksPath, 0o700); err != nil {
		return Result{}, internalFailure(phase)
	}
	attributesPath := filepath.Join(hooksPath, "attributes")
	if err := os.WriteFile(attributesPath, nil, 0o600); err != nil {
		return Result{}, internalFailure(phase)
	}
	arguments := []string{
		"-c", "core.hooksPath=" + hooksPath,
		"-c", "core.attributesFile=" + attributesPath,
		"-c", "commit.gpgSign=false",
		"-c", "push.gpgSign=false",
		"-c", "tag.gpgSign=false",
		"-c", "merge.default=text",
	}
	arguments = append(arguments, command...)
	return environmentRunner.RunEnv(ctx, repoPath, map[string]string{"GIT_ATTR_NOSYSTEM": "1"}, arguments...), nil
}

func rejectUnsafeLocalGitSettings(ctx context.Context, runner Runner, repoPath, phase string) *OperationError {
	shallow := runner.Run(ctx, repoPath, "rev-parse", "--is-shallow-repository")
	if shallow.Err != nil {
		return commandFailure("REPO-GIT-OBJECTS", phase, "Git object storage could not be inspected", shallow)
	}
	if strings.EqualFold(strings.TrimSpace(shallow.Output), "true") {
		return &OperationError{Code: "REPO-GIT-OBJECTS", Phase: phase, Summary: "shallow repositories are not supported"}
	}
	for _, marker := range []string{"info/grafts", "objects/info/alternates"} {
		result := runner.Run(ctx, repoPath, "rev-parse", "--git-path", marker)
		if result.Err != nil {
			return commandFailure("REPO-GIT-OBJECTS", phase, "Git object storage could not be inspected", result)
		}
		markerPath := result.Output
		if !filepath.IsAbs(markerPath) {
			markerPath = filepath.Join(repoPath, markerPath)
		}
		if _, err := os.Lstat(markerPath); err == nil {
			return &OperationError{Code: "REPO-GIT-OBJECTS", Phase: phase, Summary: "Git grafts and alternate object stores are not supported"}
		} else if !os.IsNotExist(err) {
			return &OperationError{Code: "REPO-GIT-OBJECTS", Phase: phase, Summary: "Git object storage could not be inspected"}
		}
	}
	for _, setting := range []string{"core.sparseCheckout", "index.sparse"} {
		result := runner.Run(ctx, repoPath, "config", "--bool", setting)
		if result.Err == nil {
			if strings.EqualFold(strings.TrimSpace(result.Output), "true") {
				return &OperationError{Code: "REPO-GIT-SPARSE-CHECKOUT", Phase: phase, Summary: "sparse checkouts are not supported"}
			}
			continue
		}
		if result.ExitCode != 1 {
			return commandFailure("REPO-GIT-CONFIG", phase, "Git checkout configuration could not be inspected", result)
		}
	}
	infoAttributes := runner.Run(ctx, repoPath, "rev-parse", "--git-path", "info/attributes")
	if infoAttributes.Err != nil {
		return commandFailure("REPO-GIT-CONFIG", phase, "repository attributes could not be inspected", infoAttributes)
	}
	attributesPath := infoAttributes.Output
	if !filepath.IsAbs(attributesPath) {
		attributesPath = filepath.Join(repoPath, attributesPath)
	}
	if _, err := os.Lstat(attributesPath); err == nil {
		return &OperationError{Code: "REPO-GIT-EXTERNAL-ATTRIBUTES", Phase: phase, Summary: "repository-local Git attributes are not supported"}
	} else if !os.IsNotExist(err) {
		return &OperationError{Code: "REPO-GIT-CONFIG", Phase: phase, Summary: "repository attributes could not be inspected"}
	}
	if failure := rejectExecutableGitConfig(ctx, runner, repoPath, phase); failure != nil {
		return failure
	}
	if failure := rejectRepositoryCredentialHelpers(ctx, runner, repoPath, phase); failure != nil {
		return failure
	}
	mirror := runner.Run(ctx, repoPath, "config", "--bool", "remote.origin.mirror")
	if mirror.Err == nil && strings.EqualFold(strings.TrimSpace(mirror.Output), "true") {
		return &OperationError{Code: "REPO-GIT-REMOTE", Phase: phase, Summary: "mirror remotes are not supported"}
	}
	if mirror.Err != nil && mirror.ExitCode != 1 {
		return commandFailure("REPO-GIT-CONFIG", phase, "Git remote configuration could not be inspected", mirror)
	}
	return nil
}

func rejectExecutableGitConfig(ctx context.Context, runner Runner, directory, phase string) *OperationError {
	for _, pattern := range []string{
		`^url\..*\.(insteadof|pushinsteadof)$`,
		`^remote\.origin\.(uploadpack|receivepack|vcs)$`,
		`^core\.(sshcommand|askpass|alternaterefscommand)$`,
	} {
		result := runner.Run(ctx, directory, "config", "--get-regexp", pattern)
		if result.Err == nil {
			return &OperationError{Code: "REPO-GIT-EXECUTABLE-CONFIG", Phase: phase, Summary: "executable or rewriting Git configuration is not supported"}
		}
		if result.ExitCode != 1 {
			return commandFailure("REPO-GIT-CONFIG", phase, "Git executable configuration could not be inspected", result)
		}
	}
	return nil
}

func rejectRepositoryCredentialHelpers(ctx context.Context, runner Runner, directory, phase string) *OperationError {
	local := runner.Run(ctx, directory, "config", "--local", "--get-regexp", `^credential(\..+)?\.helper$`)
	if local.Err == nil {
		return &OperationError{Code: "REPO-GIT-EXECUTABLE-CONFIG", Phase: phase, Summary: "repository-local credential helpers are not supported"}
	}
	if local.ExitCode != 1 {
		return commandFailure("REPO-GIT-CONFIG", phase, "Git credential configuration could not be inspected", local)
	}
	worktreeConfig := runner.Run(ctx, directory, "config", "--local", "--bool", "extensions.worktreeConfig")
	if worktreeConfig.Err != nil {
		if worktreeConfig.ExitCode == 1 {
			return nil
		}
		return commandFailure("REPO-GIT-CONFIG", phase, "Git credential configuration could not be inspected", worktreeConfig)
	}
	if !strings.EqualFold(strings.TrimSpace(worktreeConfig.Output), "true") {
		return nil
	}
	worktree := runner.Run(ctx, directory, "config", "--worktree", "--get-regexp", `^credential(\..+)?\.helper$`)
	if worktree.Err == nil {
		return &OperationError{Code: "REPO-GIT-EXECUTABLE-CONFIG", Phase: phase, Summary: "repository-local credential helpers are not supported"}
	}
	if worktree.ExitCode != 1 {
		return commandFailure("REPO-GIT-CONFIG", phase, "Git credential configuration could not be inspected", worktree)
	}
	return nil
}

func validateOriginRemote(ctx context.Context, runner Runner, repoPath, expectedKey, phase string) *OperationError {
	urls, failure := configuredRemoteURLs(ctx, runner, repoPath, "remote.origin.url", phase)
	if failure != nil {
		return failure
	}
	if len(urls) != 1 {
		return &OperationError{Code: "REPO-GIT-REMOTE", Phase: phase, Summary: "origin must have exactly one fetch URL"}
	}
	key, err := discovery.CanonicalRemote(urls[0])
	if err != nil || key != expectedKey {
		return &OperationError{Code: "REPO-GIT-REMOTE", Phase: phase, Summary: "origin does not match configured repository"}
	}
	pushURLs, failure := configuredRemoteURLs(ctx, runner, repoPath, "remote.origin.pushurl", phase)
	if failure != nil {
		return failure
	}
	if len(pushURLs) > 1 {
		return &OperationError{Code: "REPO-GIT-REMOTE", Phase: phase, Summary: "origin must not have multiple push URLs"}
	}
	if len(pushURLs) == 1 {
		pushKey, pushErr := discovery.CanonicalRemote(pushURLs[0])
		if pushErr != nil || pushKey != expectedKey {
			return &OperationError{Code: "REPO-GIT-REMOTE", Phase: phase, Summary: "origin push URL does not match configured repository"}
		}
	}
	return nil
}

func configuredRemoteURLs(ctx context.Context, runner Runner, repoPath, key, phase string) ([]string, *OperationError) {
	result := runner.Run(ctx, repoPath, "config", "--get-all", key)
	if result.Err != nil {
		if result.ExitCode == 1 {
			return nil, nil
		}
		return nil, commandFailure("REPO-GIT-REMOTE", phase, "origin validation failed", result)
	}
	raw := rawOutput(result)
	if strings.Contains(raw, "\r") {
		return nil, &OperationError{Code: "REPO-GIT-REMOTE", Phase: phase, Summary: "origin contains an unsafe URL"}
	}
	lines := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	for _, line := range lines {
		if line == "" || strings.TrimSpace(line) != line {
			return nil, &OperationError{Code: "REPO-GIT-REMOTE", Phase: phase, Summary: "origin contains an unsafe URL"}
		}
	}
	return lines, nil
}

func rawOutput(result Result) string {
	if result.RawOutput != nil {
		return string(result.RawOutput)
	}
	return result.Output
}

func commandFailure(code, phase, summary string, result Result) *OperationError {
	if result.ExitCode != 0 {
		return &OperationError{Code: code, Phase: phase, Summary: fmt.Sprintf("%s (Git exit %d)", summary, result.ExitCode)}
	}
	return &OperationError{Code: code, Phase: phase, Summary: summary}
}

func internalFailure(phase string) *OperationError {
	return &OperationError{Code: "REPO-INTERNAL-ERROR", Phase: phase, Summary: "Repo Sync encountered an internal error"}
}
