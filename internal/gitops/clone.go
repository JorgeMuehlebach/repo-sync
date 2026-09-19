package gitops

import (
	"context"
	"fmt"
	"os"

	"github.com/JorgeMuehlebach/repo-sync/internal/discovery"
)

// SecureClone creates a non-checkout clone with an empty template and with the
// same hook, transport, and attribute isolation used by every Repo Sync Git
// operation. Candidate attributes are inspected before any worktree files are
// materialized.
func SecureClone(ctx context.Context, runner Runner, parent, destination string, spec discovery.RepositorySpec) error {
	if runner == nil {
		return fmt.Errorf("trusted Git dependency is unavailable")
	}
	if failure := rejectExecutableGitConfig(ctx, runner, parent, "setup"); failure != nil {
		return failure
	}
	template, err := os.MkdirTemp("", "repo-sync-empty-template-*")
	if err != nil {
		return fmt.Errorf("create isolated Git template: %w", err)
	}
	defer os.RemoveAll(template)
	if err := os.Chmod(template, 0o700); err != nil {
		return fmt.Errorf("secure isolated Git template: %w", err)
	}
	clone := runner.Run(ctx, parent,
		"clone", "--no-checkout", "--no-tags", "--single-branch", "--branch", spec.Branch,
		"--origin", "origin", "--template="+template, "--", spec.CloneURL, destination,
	)
	if clone.Err != nil {
		return commandFailure("REPO-CLONE-FAILED", "setup", "isolated clone failed", clone)
	}
	base := newBase(runner, nil)
	target := Target{Path: destination, Spec: spec}
	if failure := base.validateRepository(ctx, target, spec.Branch); failure != nil {
		return failure
	}
	materialize, failure := runSafeMutation(ctx, runner, destination, "setup", "read-tree", "--reset", "-u", "HEAD")
	if failure != nil {
		return failure
	}
	if materialize.Err != nil {
		return commandFailure("REPO-CLONE-CHECKOUT", "setup", "validated clone could not be materialized", materialize)
	}
	if failure := base.requireClean(ctx, destination); failure != nil {
		return &OperationError{Code: "REPO-CLONE-CHECKOUT", Phase: "setup", Summary: "materialized clone is not clean"}
	}
	return nil
}
