package gitops

import (
	"context"
	"errors"
	"os"

	"github.com/JorgeMuehlebach/repo-sync/internal/validation"
)

// Mirror promotes validated commits by atomically retargeting a stable
// filesystem pointer to an immutable generation. It never checks out into the
// generation currently exposed at Target.Path.
type Mirror struct {
	base
	pointers mirrorPointerBackend
}

func NewMirror(runner Runner, validator validation.Validator) Mirror {
	return Mirror{base: newBase(runner, validator), pointers: systemMirrorPointerBackend{}}
}

// MirrorSyncResult returns the promotion transaction identity in the same call
// as the promoted outcome. The application must use TransactionID when it
// finalizes or rolls back after persisting and reopening accepted status.
type MirrorSyncResult struct {
	Outcome       Outcome
	TransactionID string
}

func (m Mirror) Sync(ctx context.Context, target Target) (Outcome, *OperationError) {
	return Outcome{}, &OperationError{
		Code:    "REPO-MIRROR-TRANSACTION-REQUIRED",
		Phase:   "promotion",
		Summary: "mirror synchronization requires the transactional API and an accepted-status completion barrier",
	}
}

// ValidateTarget verifies the stable pointer, managed generation placement,
// marker, Git identity, and clean checkout without fetching, journalling, or
// changing either generation or pointer.
func (m Mirror) ValidateTarget(ctx context.Context, target Target) *OperationError {
	if target.Spec.Branch != "main" {
		return &OperationError{Code: "REPO-MIRROR-BRANCH", Phase: "candidate", Summary: "mirror mode requires branch main"}
	}
	_, _, failure := m.inspectActiveLayout(ctx, target, "candidate")
	return failure
}

// ValidateMirrorTarget is an explicit alias for callers that also use the
// package-level ValidateTarget helper for non-managed repositories.
func (m Mirror) ValidateMirrorTarget(ctx context.Context, target Target) *OperationError {
	return m.ValidateTarget(ctx, target)
}

// SyncTransaction is the only mirror entry point that may promote a generation.
// It returns the transaction identity required by the durable accepted-status
// completion barrier. Generic Engine callers fail closed through Sync.
func (m Mirror) SyncTransaction(ctx context.Context, target Target) (MirrorSyncResult, *OperationError) {
	outcome, transactionID, failure := m.syncTransaction(ctx, target)
	return MirrorSyncResult{Outcome: outcome, TransactionID: transactionID}, failure
}

func (m Mirror) syncTransaction(ctx context.Context, target Target) (Outcome, string, *OperationError) {
	if target.Spec.Branch != "main" {
		return Outcome{}, "", &OperationError{Code: "REPO-MIRROR-BRANCH", Phase: "candidate", Summary: "mirror mode requires branch main"}
	}
	if recovery, failure := m.InspectRecovery(ctx, target); failure != nil {
		return Outcome{}, "", failure
	} else if recovery.Pending {
		return recovery.baselineOutcome(), recovery.TransactionID, &OperationError{
			Code: "REPO-MIRROR-RECOVERY-REQUIRED", Phase: "recovery",
			Summary: "an unfinished mirror promotion must be reconciled with accepted status before synchronization",
		}
	}

	layout, baseline, failure := m.inspectActiveLayout(ctx, target, "candidate")
	if failure != nil {
		return Outcome{}, "", failure
	}
	if err := m.cleanupOrphanedMirrorCandidates(layout, baseline.Path); err != nil {
		return Outcome{}, "", mirrorFilesystemFailure("cleanup", "unreferenced private mirror generations could not be safely cleaned", err)
	}
	accepted := previouslyAccepted(target, baseline.Commit, baseline.Tree)
	if target.ExpectedAcceptedCommit != "" && baseline.Commit != target.ExpectedAcceptedCommit {
		return accepted, "", &OperationError{Code: "REPO-MIRROR-ACCEPTED-DRIFT", Phase: "candidate", Summary: "mirror pointer does not match the accepted commit"}
	}

	transactionID, err := newMirrorTransactionID()
	if err != nil {
		return accepted, "", internalFailure("candidate")
	}
	candidatePath := layout.generationPath("candidate-" + transactionID)
	if err := copyMirrorGeneration(ctx, baseline.Path, candidatePath); err != nil {
		return accepted, "", mirrorFilesystemFailure("candidate", "private mirror generation could not be created", err)
	}
	candidateID := transactionID
	if err := writeMirrorGenerationMarker(candidatePath, candidateID); err != nil {
		return accepted, "", mirrorFilesystemFailure("candidate", "private mirror generation could not be identified", err)
	}
	journalOwnsCandidate := false
	privateCandidatePath := candidatePath
	privateCandidatePresent := true
	defer func() {
		if !journalOwnsCandidate && privateCandidatePresent {
			_ = removeVerifiedMirrorGeneration(layout, privateCandidatePath, candidateID)
		}
	}()

	candidateTarget := target
	candidateTarget.Path = candidatePath
	candidateTarget.ExpectedAcceptedCommit = ""
	candidateTarget.Reconcile = nil
	fetched, fetchFailure := m.fetchRemoteCandidate(ctx, candidateTarget, "main")
	if fetchFailure != nil {
		return accepted, "", fetchFailure
	}
	candidate := accepted
	candidate.CandidateCommit = fetched.Commit
	candidate.CandidateTree = fetched.Tree

	privateMutationComplete := false
	defer func() {
		if !privateMutationComplete {
			m.deleteCandidateRef(context.WithoutCancel(ctx), candidatePath, fetched.Ref, fetched.Commit)
		}
	}()

	ancestry := m.git.Run(ctx, candidatePath, "merge-base", "--is-ancestor", baseline.Commit, fetched.Commit)
	if ancestry.ExitCode == 1 {
		return candidate, "", &OperationError{Code: "REPO-MIRROR-DIVERGED", Phase: "candidate", Summary: "mirror candidate is not a fast-forward"}
	}
	if ancestry.Err != nil {
		return candidate, "", commandFailure("REPO-MIRROR-ANCESTRY", "candidate", "mirror ancestry check failed", ancestry)
	}
	if failure := m.rejectFilterAttributes(ctx, candidatePath, fetched.Tree); failure != nil {
		return candidate, "", failure
	}
	validationTarget := target
	validationTarget.ValidationRepository = candidatePath
	report, validationFailure := m.validateTree(ctx, validationTarget, validation.ModeMirror, fetched.Tree, fetched.Commit)
	candidate.Validation = report
	if validationFailure != nil {
		return candidate, "", validationFailure
	}

	if failure := m.requireBaselineUnchanged(ctx, target, baseline); failure != nil {
		return candidate, "", failure
	}
	if fetched.Commit == baseline.Commit {
		m.deleteCandidateRef(context.WithoutCancel(ctx), candidatePath, fetched.Ref, fetched.Commit)
		privateMutationComplete = true
		return Outcome{
			AcceptedCommit: baseline.Commit, AcceptedTree: baseline.Tree,
			CandidateCommit: fetched.Commit, CandidateTree: fetched.Tree, Validation: report,
		}, "", nil
	}

	transition, mutationFailure := runSafeMutation(ctx, m.git, candidatePath, "promotion", "read-tree", "--reset", "-u", fetched.Commit)
	if mutationFailure != nil {
		return candidate, "", mutationFailure
	}
	if transition.Err != nil {
		return candidate, "", commandFailure("REPO-MIRROR-PROMOTION", "promotion", "private mirror generation checkout failed", transition)
	}
	advance, mutationFailure := runSafeMutation(ctx, m.git, candidatePath, "promotion", "update-ref", "HEAD", fetched.Commit, baseline.Commit)
	if mutationFailure != nil {
		return candidate, "", mutationFailure
	}
	if advance.Err != nil {
		return candidate, "", commandFailure("REPO-MIRROR-PROMOTION", "promotion", "private mirror generation ref could not be advanced", advance)
	}
	m.deleteCandidateRef(context.WithoutCancel(ctx), candidatePath, fetched.Ref, fetched.Commit)
	privateMutationComplete = true

	if failure := m.verifyGeneration(ctx, candidatePath, candidateID, fetched.Commit, fetched.Tree, "promotion"); failure != nil {
		return candidate, "", failure
	}
	if err := makeMirrorGenerationDurable(candidatePath); err != nil {
		return candidate, "", mirrorFilesystemFailure("promotion", "private mirror generation could not be made durable", err)
	}
	if failure := m.requireBaselineUnchanged(ctx, target, baseline); failure != nil {
		return candidate, "", failure
	}
	finalCandidatePath := layout.generationPath("generation-" + transactionID)
	if err := os.Rename(privateCandidatePath, finalCandidatePath); err != nil {
		return candidate, "", mirrorFilesystemFailure("promotion", "validated mirror generation could not be installed", err)
	}
	privateCandidatePresent = false
	candidatePath = finalCandidatePath
	if err := syncMirrorDirectory(layout.generations); err != nil {
		if renameErr := os.Rename(finalCandidatePath, privateCandidatePath); renameErr == nil {
			privateCandidatePresent = true
			candidatePath = privateCandidatePath
		}
		return candidate, "", mirrorFilesystemFailure("promotion", "validated mirror generation install was not durable", err)
	}

	journal := mirrorJournal{
		SchemaVersion: mirrorJournalSchemaVersion, TransactionID: transactionID,
		RepositoryID: target.ID, State: mirrorJournalPrepared,
		Baseline: mirrorJournalGeneration{
			Name: baseline.Name, ID: baseline.ID, Commit: baseline.Commit, Tree: baseline.Tree,
			PreviouslyAccepted: target.ExpectedAcceptedCommit != "" && target.ExpectedAcceptedCommit == baseline.Commit,
		},
		Candidate: mirrorJournalGeneration{Name: layout.generationName(candidatePath), ID: candidateID, Commit: fetched.Commit, Tree: fetched.Tree},
	}
	if err := writeMirrorJournal(layout, journal); err != nil {
		if renameErr := os.Rename(finalCandidatePath, privateCandidatePath); renameErr == nil {
			privateCandidatePresent = true
			candidatePath = privateCandidatePath
			_ = syncMirrorDirectory(layout.generations)
		}
		return candidate, "", mirrorFilesystemFailure("promotion", "mirror promotion journal could not be written", err)
	}
	journalOwnsCandidate = true

	if err := m.pointers.Replace(target.Path, baseline.Path, candidatePath); err != nil {
		if active, resolveErr := m.pointers.Resolve(target.Path); resolveErr == nil && sameMirrorPath(active, candidatePath) {
			if _, rollbackFailure := m.rollbackJournalledPromotion(context.WithoutCancel(ctx), target, layout, journal); rollbackFailure != nil {
				return journal.baselineOutcome(report), transactionID, rollbackFailure
			}
		}
		return journal.baselineOutcome(report), transactionID, mirrorFilesystemFailure("promotion", "mirror pointer could not be atomically promoted", err)
	}
	if failure := m.verifyGeneration(ctx, target.Path, candidateID, fetched.Commit, fetched.Tree, "promotion"); failure != nil {
		_, rollbackFailure := m.rollbackJournalledPromotion(context.WithoutCancel(ctx), target, layout, journal)
		if rollbackFailure != nil {
			return journal.baselineOutcome(report), transactionID, rollbackFailure
		}
		return journal.baselineOutcome(report), transactionID, failure
	}
	journal.State = mirrorJournalPromoted
	if err := writeMirrorJournal(layout, journal); err != nil {
		_, rollbackFailure := m.rollbackJournalledPromotion(context.WithoutCancel(ctx), target, layout, journal)
		if rollbackFailure != nil {
			return journal.baselineOutcome(report), transactionID, rollbackFailure
		}
		return journal.baselineOutcome(report), transactionID, mirrorFilesystemFailure("promotion", "mirror promotion journal could not be advanced", err)
	}
	return Outcome{
		AcceptedCommit: fetched.Commit, AcceptedTree: fetched.Tree,
		CandidateCommit: fetched.Commit, CandidateTree: fetched.Tree, Validation: report,
	}, transactionID, nil
}

func (m Mirror) inspectActiveLayout(ctx context.Context, target Target, phase string) (mirrorLayout, mirrorGeneration, *OperationError) {
	layout, err := deriveMirrorLayout(target)
	if err != nil {
		return mirrorLayout{}, mirrorGeneration{}, mirrorFilesystemFailure(phase, "mirror layout is invalid", err)
	}
	activePath, err := m.pointers.Resolve(target.Path)
	if err != nil {
		return mirrorLayout{}, mirrorGeneration{}, &OperationError{Code: "REPO-MIRROR-LAYOUT", Phase: phase, Summary: "mirror root is not a managed stable pointer"}
	}
	name, ok := layout.directGenerationName(activePath)
	if !ok || len(name) <= len("generation-") || name[:len("generation-")] != "generation-" {
		return mirrorLayout{}, mirrorGeneration{}, &OperationError{Code: "REPO-MIRROR-LAYOUT", Phase: phase, Summary: "mirror pointer leaves its managed generation area"}
	}
	id, err := readMirrorGenerationMarker(activePath)
	if err != nil {
		return mirrorLayout{}, mirrorGeneration{}, &OperationError{Code: "REPO-MIRROR-GENERATION", Phase: phase, Summary: "active mirror generation identity does not match its managed path"}
	}
	if failure := m.reconcileTarget(ctx, target, "main", phase); failure != nil {
		return mirrorLayout{}, mirrorGeneration{}, failure
	}
	if failure := m.requireClean(ctx, target.Path); failure != nil {
		return mirrorLayout{}, mirrorGeneration{}, failure
	}
	commit, tree, failure := m.commitAndTree(ctx, target.Path, "HEAD")
	if failure != nil {
		return mirrorLayout{}, mirrorGeneration{}, failure
	}
	return layout, mirrorGeneration{Name: name, Path: activePath, ID: id, Commit: commit, Tree: tree}, nil
}

func (m Mirror) requireBaselineUnchanged(ctx context.Context, target Target, baseline mirrorGeneration) *OperationError {
	active, err := m.pointers.Resolve(target.Path)
	if err != nil || !sameMirrorPath(active, baseline.Path) {
		return promotionRace()
	}
	if failure := m.reconcileTarget(ctx, target, "main", "promotion"); failure != nil {
		failure.Code = "REPO-MIRROR-PROMOTION-RACE"
		failure.Summary = "mirror or pinned validation inputs changed during candidate validation"
		return failure
	}
	if failure := m.requireClean(ctx, target.Path); failure != nil {
		return promotionRace()
	}
	commit, tree, failure := m.commitAndTree(ctx, target.Path, "HEAD")
	if failure != nil || commit != baseline.Commit || tree != baseline.Tree {
		return promotionRace()
	}
	id, err := readMirrorGenerationMarker(active)
	if err != nil || id != baseline.ID {
		return promotionRace()
	}
	return nil
}

func (m Mirror) verifyGeneration(ctx context.Context, path, generationID, commit, tree, phase string) *OperationError {
	if isMirrorPointer(path) {
		resolved, resolveErr := m.pointers.Resolve(path)
		if resolveErr != nil {
			return &OperationError{Code: "REPO-MIRROR-GENERATION", Phase: phase, Summary: "mirror generation pointer could not be resolved"}
		}
		path = resolved
	}
	id, err := readMirrorGenerationMarker(path)
	if err != nil || id != generationID {
		return &OperationError{Code: "REPO-MIRROR-GENERATION", Phase: phase, Summary: "mirror generation identity changed"}
	}
	actualCommit, actualTree, failure := m.commitAndTree(ctx, path, "HEAD")
	if failure != nil || actualCommit != commit || actualTree != tree {
		return &OperationError{Code: "REPO-MIRROR-GENERATION", Phase: phase, Summary: "mirror generation does not match its journalled Git identity"}
	}
	if failure := m.requireClean(ctx, path); failure != nil {
		return &OperationError{Code: "REPO-MIRROR-GENERATION", Phase: phase, Summary: "mirror generation is not clean"}
	}
	return nil
}

func previouslyAccepted(target Target, commit, tree string) Outcome {
	if target.ExpectedAcceptedCommit == "" || target.ExpectedAcceptedCommit != commit {
		return Outcome{}
	}
	return Outcome{AcceptedCommit: commit, AcceptedTree: tree}
}

func promotionRace() *OperationError {
	return &OperationError{Code: "REPO-MIRROR-PROMOTION-RACE", Phase: "promotion", Summary: "mirror changed during candidate validation"}
}

func mirrorFilesystemFailure(phase, summary string, err error) *OperationError {
	if err == nil {
		return &OperationError{Code: "REPO-MIRROR-TRANSACTION", Phase: phase, Summary: summary}
	}
	var atomicErr *mirrorAtomicPointerError
	if errors.As(err, &atomicErr) {
		return &OperationError{Code: "REPO-MIRROR-ATOMIC-SWAP", Phase: phase, Summary: summary}
	}
	return &OperationError{Code: "REPO-MIRROR-TRANSACTION", Phase: phase, Summary: summary}
}
