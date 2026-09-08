package gitops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/JorgeMuehlebach/repo-sync/internal/discovery"
)

const defaultCommitMessage = "chore: automatic repository sync"

var credentialURL = regexp.MustCompile(`(?i)(https?://)[^/@\s]+@`)

type Result struct {
	Output   string
	ExitCode int
	Err      error
}

type Runner interface {
	Run(ctx context.Context, dir string, args ...string) Result
}

type SystemRunner struct{}

func (SystemRunner) Run(ctx context.Context, dir string, args ...string) Result {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	output, err := cmd.CombinedOutput()
	result := Result{Output: strings.TrimSpace(string(output)), Err: err}
	if err == nil {
		return result
	}
	result.ExitCode = -1
	if exitErr, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
	}
	return result
}

type Syncer struct {
	Git           Runner
	CommitMessage string
}

type Inspection struct {
	Changes []string
}

func NewSyncer() Syncer {
	return Syncer{Git: SystemRunner{}, CommitMessage: defaultCommitMessage}
}

func (s Syncer) Inspect(ctx context.Context, repoPath string, spec discovery.RepositorySpec) (Inspection, error) {
	if s.Git == nil {
		s.Git = SystemRunner{}
	}
	if err := s.validate(ctx, repoPath, spec); err != nil {
		return Inspection{}, err
	}
	if err := s.ensureNoOperationInProgress(ctx, repoPath); err != nil {
		return Inspection{}, err
	}
	status := s.Git.Run(ctx, repoPath, "status", "--porcelain=v1")
	if status.Err != nil {
		return Inspection{}, commandError(status, "read working-tree status")
	}
	inspection := Inspection{}
	if status.Output != "" {
		inspection.Changes = strings.Split(status.Output, "\n")
	}
	return inspection, nil
}

func (s Syncer) CheckIdentity(ctx context.Context, repoPath string) error {
	if s.Git == nil {
		s.Git = SystemRunner{}
	}
	for _, field := range []string{"user.name", "user.email"} {
		result := s.Git.Run(ctx, repoPath, "config", "--get", field)
		if result.Err != nil || strings.TrimSpace(result.Output) == "" {
			return fmt.Errorf("Git %s is not configured", field)
		}
	}
	return nil
}

func (s Syncer) CheckRemote(ctx context.Context, repoPath string, spec discovery.RepositorySpec) error {
	if s.Git == nil {
		s.Git = SystemRunner{}
	}
	result := s.Git.Run(ctx, repoPath, "ls-remote", "--exit-code", "--heads", "origin", "refs/heads/"+spec.Branch)
	if result.Err != nil {
		return commandError(result, "reach origin branch")
	}
	return nil
}

func (s Syncer) Ignored(ctx context.Context, repoPath string) ([]string, error) {
	if s.Git == nil {
		s.Git = SystemRunner{}
	}
	result := s.Git.Run(ctx, repoPath, "status", "--porcelain=v1", "--ignored=matching")
	if result.Err != nil {
		return nil, commandError(result, "inspect ignored paths")
	}
	var ignored []string
	for _, line := range strings.Split(result.Output, "\n") {
		if strings.HasPrefix(line, "!! ") {
			ignored = append(ignored, strings.TrimSpace(strings.TrimPrefix(line, "!! ")))
		}
	}
	return ignored, nil
}

func (s Syncer) Sync(ctx context.Context, repoPath string, spec discovery.RepositorySpec) error {
	if s.Git == nil {
		s.Git = SystemRunner{}
	}
	if s.CommitMessage == "" {
		s.CommitMessage = defaultCommitMessage
	}
	inspection, err := s.Inspect(ctx, repoPath, spec)
	if err != nil {
		return err
	}
	if len(inspection.Changes) > 0 {
		if result := s.Git.Run(ctx, repoPath, "add", "-A"); result.Err != nil {
			return commandError(result, "stage changes")
		}
		diff := s.Git.Run(ctx, repoPath, "diff", "--cached", "--quiet")
		switch diff.ExitCode {
		case 0:
			// Changes may have disappeared between the status and add calls.
		case 1:
			if result := s.Git.Run(ctx, repoPath, "commit", "-m", s.CommitMessage); result.Err != nil {
				return commandError(result, "commit changes")
			}
		default:
			return commandError(diff, "inspect staged changes")
		}
	}

	var push Result
	for attempt := 0; attempt < 3; attempt++ {
		fetch := s.Git.Run(ctx, repoPath, "fetch", "origin", spec.Branch)
		if fetch.Err != nil {
			return commandError(fetch, "fetch origin")
		}
		rebase := s.Git.Run(ctx, repoPath, "rebase", "origin/"+spec.Branch)
		if rebase.Err != nil {
			_ = s.Git.Run(ctx, repoPath, "rebase", "--abort")
			return commandError(rebase, "rebase onto origin/"+spec.Branch)
		}
		push = s.Git.Run(ctx, repoPath, "push", "origin", "HEAD:"+spec.Branch)
		if push.Err == nil {
			return nil
		}
	}
	return commandError(push, "push after three attempts")
}

func (s Syncer) validate(ctx context.Context, repoPath string, spec discovery.RepositorySpec) error {
	root := s.Git.Run(ctx, repoPath, "rev-parse", "--show-toplevel")
	if root.Err != nil {
		return commandError(root, "locate repository root")
	}
	wantRoot, err := filepath.Abs(repoPath)
	if err != nil {
		return err
	}
	gotRoot, err := filepath.Abs(root.Output)
	if err != nil {
		return err
	}
	wantInfo, wantErr := os.Stat(wantRoot)
	gotInfo, gotErr := os.Stat(gotRoot)
	if wantErr != nil || gotErr != nil || !os.SameFile(wantInfo, gotInfo) {
		return fmt.Errorf("configured path %s is not the repository root %s", wantRoot, gotRoot)
	}
	remote := s.Git.Run(ctx, repoPath, "config", "--get", "remote.origin.url")
	if remote.Err != nil {
		return commandError(remote, "read origin remote")
	}
	key, err := discovery.CanonicalRemote(remote.Output)
	if err != nil || key != spec.Key {
		return fmt.Errorf("origin remote does not match configured repository %s", spec.Key)
	}
	branch := s.Git.Run(ctx, repoPath, "branch", "--show-current")
	if branch.Err != nil {
		return commandError(branch, "read current branch")
	}
	if branch.Output != spec.Branch {
		return fmt.Errorf("paused: configured branch is %q but %q is checked out", spec.Branch, branch.Output)
	}
	return nil
}

func (s Syncer) ensureNoOperationInProgress(ctx context.Context, repoPath string) error {
	for _, marker := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		result := s.Git.Run(ctx, repoPath, "rev-parse", "--git-path", marker)
		if result.Err != nil {
			return commandError(result, "inspect Git operation state")
		}
		markerPath := result.Output
		if !filepath.IsAbs(markerPath) {
			markerPath = filepath.Join(repoPath, markerPath)
		}
		if _, err := os.Stat(markerPath); err == nil {
			return fmt.Errorf("paused: Git operation in progress (%s)", marker)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect Git operation marker %s: %w", marker, err)
		}
	}
	return nil
}

func commandError(result Result, action string) error {
	if result.Output == "" {
		return fmt.Errorf("%s: %w", action, result.Err)
	}
	return fmt.Errorf("%s: %s", action, redact(result.Output))
}

func redact(value string) string {
	return credentialURL.ReplaceAllString(value, `${1}***@`)
}
