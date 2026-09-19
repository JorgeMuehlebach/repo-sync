package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/JorgeMuehlebach/repo-sync/internal/securefile"
)

// InitializeMirror bootstraps an absent mirror into an independent managed
// generation and a stable pointer. It does not register global links or mark
// the clone accepted; the application must subsequently run the normal mirror
// validation flow before exposing it to any global consumer.
func InitializeMirror(ctx context.Context, runner Runner, target Target) (Outcome, *OperationError) {
	return NewMirror(runner, nil).Initialize(ctx, target)
}

// Initialize is the instance form used when the application already owns a
// concrete Mirror with a pinned runner.
func (m Mirror) Initialize(ctx context.Context, target Target) (Outcome, *OperationError) {
	if target.Spec.Branch != "main" {
		return Outcome{}, &OperationError{Code: "REPO-MIRROR-BRANCH", Phase: "setup", Summary: "mirror mode requires branch main"}
	}
	if target.ExpectedAcceptedCommit != "" {
		return Outcome{}, &OperationError{Code: "REPO-MIRROR-BOOTSTRAP", Phase: "setup", Summary: "an absent mirror cannot already have an accepted commit"}
	}
	layout, err := deriveMirrorLayout(target)
	if err != nil {
		return Outcome{}, mirrorFilesystemFailure("setup", "mirror bootstrap layout is invalid", err)
	}
	if _, err := securefile.VerifyDirectoryNoReparse(filepath.Dir(target.Path)); err != nil {
		return Outcome{}, mirrorFilesystemFailure("setup", "mirror bootstrap parent is unsafe", err)
	}
	if info, err := os.Lstat(target.Path); err == nil {
		kind := "path"
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			kind = "real directory"
		}
		return Outcome{}, &OperationError{Code: "REPO-MIRROR-LAYOUT", Phase: "setup", Summary: "mirror bootstrap refuses an existing " + kind}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Outcome{}, mirrorFilesystemFailure("setup", "mirror bootstrap path could not be inspected", err)
	}
	if _, err := os.Lstat(layout.storageRoot); err == nil {
		return Outcome{}, &OperationError{Code: "REPO-MIRROR-LAYOUT", Phase: "setup", Summary: "mirror bootstrap refuses existing or ambiguous managed storage"}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Outcome{}, mirrorFilesystemFailure("setup", "mirror bootstrap storage could not be inspected", err)
	}
	if err := os.Mkdir(layout.storageRoot, 0o700); err != nil {
		return Outcome{}, mirrorFilesystemFailure("setup", "mirror bootstrap storage could not be created", err)
	}
	removeStorage := true
	defer func() {
		if removeStorage {
			_ = os.Remove(layout.generations)
			_ = os.Remove(layout.storageRoot)
		}
	}()
	if _, err := securefile.VerifyDirectoryNoReparse(layout.storageRoot); err != nil {
		return Outcome{}, mirrorFilesystemFailure("setup", "mirror bootstrap storage is unsafe", err)
	}
	if err := os.Mkdir(layout.generations, 0o700); err != nil {
		return Outcome{}, mirrorFilesystemFailure("setup", "mirror generation storage could not be created", err)
	}

	transactionID, err := newMirrorTransactionID()
	if err != nil {
		return Outcome{}, internalFailure("setup")
	}
	privatePath := layout.generationPath("candidate-" + transactionID)
	if err := SecureClone(ctx, m.git, layout.generations, privatePath, target.Spec); err != nil {
		return Outcome{}, mirrorBootstrapFailure("isolated mirror bootstrap clone failed", err)
	}
	generationID := transactionID
	if err := writeMirrorGenerationMarker(privatePath, generationID); err != nil {
		return Outcome{}, mirrorFilesystemFailure("setup", "bootstrap mirror generation could not be identified", err)
	}
	privateExists := true
	defer func() {
		if privateExists {
			_ = removeVerifiedMirrorGeneration(layout, privatePath, generationID)
		}
	}()
	commit, tree, failure := m.commitAndTree(ctx, privatePath, "HEAD")
	if failure != nil {
		failure.Phase = "setup"
		return Outcome{}, failure
	}
	if failure := m.requireClean(ctx, privatePath); failure != nil {
		return Outcome{}, &OperationError{Code: "REPO-MIRROR-BOOTSTRAP", Phase: "setup", Summary: "bootstrap mirror generation is not clean"}
	}
	if failure := m.verifyGeneration(ctx, privatePath, generationID, commit, tree, "setup"); failure != nil {
		return Outcome{}, failure
	}
	if err := makeMirrorGenerationDurable(privatePath); err != nil {
		return Outcome{}, mirrorFilesystemFailure("setup", "bootstrap mirror generation could not be made durable", err)
	}
	finalPath := layout.generationPath("generation-" + transactionID)
	if err := os.Rename(privatePath, finalPath); err != nil {
		return Outcome{}, mirrorFilesystemFailure("setup", "bootstrap mirror generation could not be installed", err)
	}
	privateExists = false
	finalExists := true
	defer func() {
		if finalExists {
			if err := os.Rename(finalPath, privatePath); err == nil {
				_ = removeVerifiedMirrorGeneration(layout, privatePath, generationID)
			}
		}
	}()
	if err := syncMirrorDirectory(layout.generations); err != nil {
		return Outcome{}, mirrorFilesystemFailure("setup", "bootstrap mirror generation install was not durable", err)
	}
	if err := m.pointers.Create(target.Path, finalPath); err != nil {
		return Outcome{}, mirrorFilesystemFailure("setup", "stable mirror pointer could not be created", err)
	}
	if failure := m.verifyGeneration(ctx, target.Path, generationID, commit, tree, "setup"); failure != nil {
		_ = os.Remove(target.Path)
		return Outcome{}, failure
	}
	finalExists = false
	removeStorage = false
	return Outcome{CandidateCommit: commit, CandidateTree: tree}, nil
}

func mirrorBootstrapFailure(summary string, err error) *OperationError {
	var failure *OperationError
	if errors.As(err, &failure) {
		copy := *failure
		copy.Phase = "setup"
		copy.Summary = summary
		return &copy
	}
	return mirrorFilesystemFailure("setup", summary, fmt.Errorf("bootstrap: %w", err))
}
