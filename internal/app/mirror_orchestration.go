package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/JorgeMuehlebach/repo-sync/internal/config"
	"github.com/JorgeMuehlebach/repo-sync/internal/gitops"
	"github.com/JorgeMuehlebach/repo-sync/internal/notify"
	"github.com/JorgeMuehlebach/repo-sync/internal/state"
)

// transactionalMirror is deliberately stronger than gitops.Engine. A mirror
// promotion is incomplete until the app has persisted and reopened the shared
// accepted-status record, so production mirror operations must retain the
// transaction identity returned by SyncTransaction.
type transactionalMirror interface {
	gitops.Engine
	SyncTransaction(context.Context, gitops.Target) (gitops.MirrorSyncResult, *gitops.OperationError)
	InspectRecovery(context.Context, gitops.Target) (gitops.MirrorRecovery, *gitops.OperationError)
	Finalize(context.Context, gitops.Target, gitops.Outcome, string) *gitops.OperationError
	Rollback(context.Context, gitops.Target, gitops.Outcome, string) (gitops.Outcome, *gitops.OperationError)
	AcknowledgeRollback(context.Context, gitops.Target, string) *gitops.OperationError
}

func (a *Application) recoverMirrorTransaction(ctx context.Context, mirror transactionalMirror, repository configuredRepository, repoState state.Repository, target gitops.Target) (state.Repository, error) {
	recovery, failure := mirror.InspectRecovery(ctx, target)
	if failure != nil {
		recordErr := a.recordFailure(repository, repoState, true, gitops.Outcome{}, failure)
		return repoState, errors.Join(failure, recordErr)
	}
	if !recovery.Pending {
		return repoState, nil
	}

	status, statusErr := a.readContextStatusForRecovery()
	if statusErr == nil && recovery.ActiveGeneration == "candidate" {
		if accepted, ok := acceptedMirrorCandidate(status, repository, repoState, recovery); ok {
			repaired, repairErr := a.repairAcceptedMirrorState(repository, repoState, recovery, status, accepted)
			if repairErr != nil {
				return repoState, repairErr
			}
			outcome := gitops.Outcome{
				AcceptedCommit: recovery.CandidateCommit, AcceptedTree: recovery.CandidateTree,
				CandidateCommit: recovery.CandidateCommit, CandidateTree: recovery.CandidateTree,
			}
			if finalizeFailure := mirror.Finalize(ctx, target, outcome, recovery.TransactionID); finalizeFailure != nil {
				recordErr := a.recordPostBarrierFailure(repository, repaired, outcome, finalizeFailure)
				return repaired, errors.Join(finalizeFailure, recordErr)
			}
			a.logf("mirror recovery finalized repository=%s transaction=%s", repository.ID, recovery.TransactionID)
			return repaired, nil
		}
	}

	cause := &gitops.OperationError{
		Code: "REPO-MIRROR-RECOVERY-ROLLBACK", Phase: "promotion",
		Summary: "unfinished mirror promotion did not have an exact accepted status record and was rolled back",
	}
	rollbackOutcome := gitops.Outcome{CandidateCommit: recovery.CandidateCommit, CandidateTree: recovery.CandidateTree}
	return repoState, a.rollbackMirrorTransaction(ctx, mirror, repository, repoState, target, rollbackOutcome, recovery.TransactionID, cause, status)
}

func (a *Application) completeMirrorTransaction(ctx context.Context, mirror transactionalMirror, repository configuredRepository, repoState state.Repository, target gitops.Target, result gitops.MirrorSyncResult) error {
	if result.TransactionID == "" {
		return a.recordSuccess(repository, repoState, result.Outcome)
	}
	if err := a.recordSuccess(repository, repoState, result.Outcome); err != nil {
		var accepted *acceptedStatusCompletionError
		if errors.As(err, &accepted) {
			// The shared, reopened status already authorizes this candidate. Keep
			// the journal and pending private completion so restart recovery can
			// finish both; rolling back here would contradict accepted status.
			return err
		}
		current, loadErr := a.currentRepositoryState(repository)
		if loadErr != nil {
			return errors.Join(err, loadErr)
		}
		return a.rollbackMirrorTransaction(ctx, mirror, repository, current, target, result.Outcome, result.TransactionID, err, contextStatus{})
	}
	if failure := mirror.Finalize(ctx, target, result.Outcome, result.TransactionID); failure != nil {
		current, loadErr := a.currentRepositoryState(repository)
		if loadErr != nil {
			return errors.Join(failure, loadErr)
		}
		recordErr := a.recordPostBarrierFailure(repository, current, result.Outcome, failure)
		return errors.Join(failure, recordErr)
	}
	a.logf("mirror transaction finalized repository=%s transaction=%s", repository.ID, result.TransactionID)
	return nil
}

func (a *Application) rollbackMirrorTransaction(ctx context.Context, mirror transactionalMirror, repository configuredRepository, repoState state.Repository, target gitops.Target, promoted gitops.Outcome, transactionID string, cause error, previousStatus contextStatus) error {
	restored, rollbackFailure := mirror.Rollback(ctx, target, promoted, transactionID)
	if rollbackFailure != nil {
		recordErr := a.recordFailure(repository, repoState, true, promoted, rollbackFailure)
		return errors.Join(cause, rollbackFailure, recordErr)
	}
	current, loadErr := a.currentRepositoryState(repository)
	if loadErr != nil {
		return errors.Join(cause, loadErr)
	}
	baselineAlreadyPrivate := current.AcceptedCommit == restored.AcceptedCommit && current.AcceptedTree == restored.AcceptedTree
	current.AcceptedCommit = restored.AcceptedCommit
	current.AcceptedTree = restored.AcceptedTree
	current.CandidateCommit = restored.CandidateCommit
	current.CandidateTree = restored.CandidateTree
	current.PendingCompletion = nil
	if accepted, ok := acceptedMirrorBaseline(previousStatus, repository, current, restored); ok {
		applyAcceptedStatusToState(&current, previousStatus, accepted, restored.AcceptedCommit, restored.AcceptedTree)
	} else if !baselineAlreadyPrivate {
		current.LastSuccess = time.Time{}
		current.LastSync = time.Time{}
		if current.Validation.ObjectID != restored.AcceptedTree {
			current.Validation = state.Validation{}
		}
	}
	failure := &gitops.OperationError{
		Code: "REPO-MIRROR-RECOVERY-ROLLBACK", Phase: "promotion",
		Summary: "mirror promotion was rolled back to the last verified generation",
	}
	if recordErr := a.recordFailure(repository, current, true, restored, failure); recordErr != nil {
		return errors.Join(cause, recordErr)
	}
	if verifyErr := a.verifyRollbackCompletion(repository, restored, failure.Code); verifyErr != nil {
		return errors.Join(cause, verifyErr)
	}
	if acknowledgeFailure := mirror.AcknowledgeRollback(ctx, target, transactionID); acknowledgeFailure != nil {
		latest, latestErr := a.currentRepositoryState(repository)
		if latestErr != nil {
			return errors.Join(cause, acknowledgeFailure, latestErr)
		}
		recordErr := a.recordFailure(repository, latest, true, restored, acknowledgeFailure)
		return errors.Join(cause, acknowledgeFailure, recordErr)
	}
	a.logf("mirror transaction rolled back repository=%s transaction=%s", repository.ID, transactionID)
	return cause
}

func (a *Application) currentRepositoryState(repository configuredRepository) (state.Repository, error) {
	machineState, err := state.Load(a.statePath)
	if err != nil {
		return state.Repository{}, err
	}
	repoState, ok := repositoryState(machineState, repository)
	if !ok {
		return state.Repository{}, fmt.Errorf("repository state disappeared during mirror transaction")
	}
	return repoState, nil
}

func (a *Application) readContextStatusForRecovery() (contextStatus, error) {
	release, err := a.acquireContextStatusLock()
	if err != nil {
		return contextStatus{}, err
	}
	defer release()
	return readPersistedContextStatus(filepath.Join(a.configDir, "context-status.json"))
}

func acceptedMirrorCandidate(status contextStatus, repository configuredRepository, repoState state.Repository, recovery gitops.MirrorRecovery) (contextRepository, bool) {
	item, ok := exactStatusRepository(status, repository.ID)
	if !ok || item.Mode != string(config.ModeMirror) || item.Branch != repository.Spec.Branch || item.Error != nil || item.LastSuccessAt == "" || item.LastValidation == nil {
		return contextRepository{}, false
	}
	expectedConfig := contextValidationConfig{
		Protocol: repoState.ValidationRuntime.Protocol, SourceID: repoState.ValidationRuntime.SourceID,
		ContextctlBinaryDigest: repoState.ValidationRuntime.ContextctlDigest,
		RegistryRevision:       repoState.ValidationRuntime.RegistryRevision, RegistryDigest: repoState.ValidationRuntime.RegistryDigest,
		TrustStateRevision: repoState.ValidationRuntime.TrustStateRevision, TrustStateDigest: repoState.ValidationRuntime.TrustStateDigest,
	}
	if item.ValidationConfig != expectedConfig || item.AcceptedCommit != recovery.CandidateCommit || item.CandidateCommit != recovery.CandidateCommit || item.LastValidation.TreeObjectID != recovery.CandidateTree || item.LastValidation.Verdict != "pass" {
		return contextRepository{}, false
	}
	return item, true
}

func acceptedMirrorBaseline(status contextStatus, repository configuredRepository, repoState state.Repository, restored gitops.Outcome) (contextRepository, bool) {
	if restored.AcceptedCommit == "" {
		return contextRepository{}, false
	}
	item, ok := exactStatusRepository(status, repository.ID)
	if !ok || item.Mode != string(config.ModeMirror) || item.Branch != repository.Spec.Branch || item.AcceptedCommit != restored.AcceptedCommit || item.LastValidation == nil || item.LastValidation.TreeObjectID != restored.AcceptedTree {
		return contextRepository{}, false
	}
	expectedConfig := contextValidationConfig{
		Protocol: repoState.ValidationRuntime.Protocol, SourceID: repoState.ValidationRuntime.SourceID,
		ContextctlBinaryDigest: repoState.ValidationRuntime.ContextctlDigest,
		RegistryRevision:       repoState.ValidationRuntime.RegistryRevision, RegistryDigest: repoState.ValidationRuntime.RegistryDigest,
		TrustStateRevision: repoState.ValidationRuntime.TrustStateRevision, TrustStateDigest: repoState.ValidationRuntime.TrustStateDigest,
	}
	return item, item.ValidationConfig == expectedConfig
}

func exactStatusRepository(status contextStatus, repositoryID string) (contextRepository, bool) {
	var matched *contextRepository
	for index := range status.Repositories {
		if status.Repositories[index].RepositoryID != repositoryID {
			continue
		}
		if matched != nil {
			return contextRepository{}, false
		}
		matched = &status.Repositories[index]
	}
	if matched == nil {
		return contextRepository{}, false
	}
	return *matched, true
}

func (a *Application) repairAcceptedMirrorState(repository configuredRepository, repoState state.Repository, recovery gitops.MirrorRecovery, status contextStatus, accepted contextRepository) (state.Repository, error) {
	applyAcceptedStatusToState(&repoState, status, accepted, recovery.CandidateCommit, recovery.CandidateTree)
	repoState.CandidateCommit = recovery.CandidateCommit
	repoState.CandidateTree = recovery.CandidateTree
	repoState.PendingCompletion = nil
	repoState.Failure = nil
	repoState.Notification = state.Notification{}
	return a.persistRepositoryState(repository, repoState, true)
}

func applyAcceptedStatusToState(repoState *state.Repository, status contextStatus, accepted contextRepository, commit, tree string) {
	repoState.AcceptedCommit = commit
	repoState.AcceptedTree = tree
	if parsed, err := time.Parse(time.RFC3339Nano, accepted.LastAttemptAt); err == nil {
		repoState.LastAttempt = parsed.UTC()
	}
	if parsed, err := time.Parse(time.RFC3339Nano, accepted.LastSuccessAt); err == nil {
		repoState.LastSuccess = parsed.UTC()
		repoState.LastSync = parsed.UTC()
	}
	if accepted.LastValidation != nil {
		checkedAt, _ := time.Parse(time.RFC3339Nano, status.GeneratedAt)
		repoState.Validation = persistedStatusValidation(*accepted.LastValidation, checkedAt.UTC())
	}
}

func persistedStatusValidation(value contextValidation, checkedAt time.Time) state.Validation {
	results := make([]state.ValidatorResult, 0, len(value.ValidatorResults))
	for _, result := range value.ValidatorResults {
		results = append(results, state.ValidatorResult{
			ContractID: result.ContractID, ContractVersion: result.ContractVersion, Disposition: result.Disposition,
			CheckIDs: append([]string(nil), result.CheckIDs...), AcceptedBundleDigest: result.AcceptedBundleDigest,
			CandidateBundleDigest: result.CandidateBundleDigest,
		})
	}
	return state.Validation{
		ContractVersion: value.ContractVersion, ObjectID: value.TreeObjectID, Verdict: value.Verdict,
		CheckIDs: append([]string(nil), value.CheckIDs...), ValidatorResults: results, CheckedAt: checkedAt,
	}
}

func (a *Application) verifyRollbackCompletion(repository configuredRepository, restored gitops.Outcome, failureCode string) error {
	current, err := a.currentRepositoryState(repository)
	if err != nil {
		return err
	}
	if current.AcceptedCommit != restored.AcceptedCommit || current.AcceptedTree != restored.AcceptedTree || current.CandidateCommit != restored.CandidateCommit || current.CandidateTree != restored.CandidateTree || current.Failure == nil || current.Failure.Code != failureCode {
		return fmt.Errorf("rolled-back mirror state could not be verified")
	}
	status, err := a.readContextStatusForRecovery()
	if err != nil {
		return err
	}
	item, ok := exactStatusRepository(status, repository.ID)
	if !ok || item.AcceptedCommit != restored.AcceptedCommit || item.CandidateCommit != restored.CandidateCommit || item.Error == nil || item.Error.Code != failureCode {
		return fmt.Errorf("rolled-back mirror status could not be verified")
	}
	return nil
}

// recordPostBarrierFailure keeps the already verified accepted status intact.
// This is used only when journal finalization fails after the completion
// barrier; recovery can therefore still prove the candidate authoritative.
func (a *Application) recordPostBarrierFailure(repository configuredRepository, repoState state.Repository, outcome gitops.Outcome, failure *gitops.OperationError) error {
	now := a.now().UTC()
	repoState.AcceptedCommit = outcome.AcceptedCommit
	repoState.AcceptedTree = outcome.AcceptedTree
	repoState.CandidateCommit = outcome.CandidateCommit
	repoState.CandidateTree = outcome.CandidateTree
	repoState.LastAttempt = now
	repoState.Failure = &state.Failure{Code: failure.Code, Phase: failure.Phase, Summary: failure.Summary, OccurredAt: now}
	fingerprint := failureFingerprint(repository.ID, failure, outcome.CandidateCommit, outcome.CandidateTree)
	version := repoState.Notification.Version
	if version == ^uint64(0) {
		return fmt.Errorf("notification version exhausted")
	}
	version++
	repoState.Notification = state.Notification{FailureKey: fingerprint, Status: "pending", AttemptedAt: now, Version: version}
	_, err := a.persistRepositoryState(repository, repoState, true)
	if err != nil {
		return err
	}
	a.logf("post-barrier mirror failure repository=%s code=%s", repository.ID, failure.Code)
	notificationContext, cancel := context.WithTimeout(context.Background(), notificationTimeout)
	delivery := a.notifier.Notify(notificationContext, notify.Message{Title: "Repo Sync requires attention", Body: fmt.Sprintf("Repository %s failed check %s.", repository.ID, failure.Code)})
	cancel()
	return a.completeNotificationCAS(repository, fingerprint, version, delivery, a.now().UTC())
}
