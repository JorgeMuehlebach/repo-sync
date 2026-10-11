package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/JorgeMuehlebach/repo-sync/internal/config"
	"github.com/JorgeMuehlebach/repo-sync/internal/notify"
	"github.com/JorgeMuehlebach/repo-sync/internal/state"
	"github.com/JorgeMuehlebach/repo-sync/internal/validation"
)

const (
	maintenanceCodeRuntime    = "REPO-MAINTENANCE-RUNTIME"
	maintenanceCodeHeld       = "REPO-MAINTENANCE-HELD"
	maintenanceCodeProtocol   = "REPO-MAINTENANCE-PROTOCOL"
	maintenanceCodeUnsafePlan = "REPO-MAINTENANCE-UNSAFE-PLAN"
)

func (a *Application) maintenanceCommand(ctx context.Context, args []string) error {
	if len(args) != 1 || (args[0] != "enable" && args[0] != "disable") {
		return fmt.Errorf("usage: repo-sync maintenance enable|disable")
	}
	if args[0] == "disable" {
		if err := state.Update(a.statePath, func(current *state.State) error {
			current.ManagedSkillLinks = false
			current.Maintenance = state.Maintenance{}
			return nil
		}); err != nil {
			return err
		}
		fmt.Fprintln(a.out, "Managed skill-link maintenance is disabled.")
		return nil
	}

	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	repositories, err := configuredRepositories(cfg, "")
	if err != nil {
		return err
	}
	release, err := a.acquireRepositoryLocks(repositories)
	if err != nil {
		return err
	}
	defer release()
	request, err := a.managedSkillLinkRequest(repositories)
	if err != nil {
		return fmt.Errorf("managed skill-link preflight failed: %w", err)
	}
	report, err := a.maintainer.PlanManagedSkillLinks(ctx, request)
	if err != nil {
		return fmt.Errorf("managed skill-link preflight failed")
	}
	if report.Verdict != validation.VerdictPass {
		return fmt.Errorf("managed skill-link preflight requires attention (%s)", maintenanceReportCheck(report))
	}
	now := a.now().UTC()
	if err := state.Update(a.statePath, func(current *state.State) error {
		current.ManagedSkillLinks = true
		current.Maintenance = state.Maintenance{LastAttempt: now, LastSuccess: now}
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintln(a.out, "Managed skill-link maintenance is enabled.")
	return nil
}

// refreshValidationSetup rebuilds every configured runtime binding from
// existing source roots. It performs no discovery, fetch, or publication, and
// applies all staged bindings through one state update.
func (a *Application) refreshValidationSetup(ctx context.Context, options setupOptions) error {
	if stateName := a.serviceState(); stateName != "stopped" {
		return fmt.Errorf("validation refresh requires the Repo Sync service to be stopped")
	}
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	configSnapshot, err := os.ReadFile(a.configPath)
	if err != nil {
		return err
	}
	repositories, err := configuredRepositories(cfg, "")
	if err != nil {
		return err
	}
	if len(repositories) == 0 {
		return fmt.Errorf("no repositories configured; run repo-sync config add <github-branch-url>")
	}
	release, err := a.acquireRepositoryLocks(repositories)
	if err != nil {
		return err
	}
	defer release()
	machineState, err := state.Load(a.statePath)
	if err != nil {
		return err
	}
	if hasPendingCompletions(repositories, machineState) {
		return fmt.Errorf("repository completion recovery is pending; run repo-sync sync before setup")
	}
	if err := validateUniqueRepositoryPaths(repositories, machineState); err != nil {
		return err
	}
	staged := make(map[string]state.ValidationRuntime, len(repositories))
	revisions := make(map[string]uint64, len(repositories))
	for _, repository := range repositories {
		repoState, ok := repositoryState(machineState, repository)
		if !ok || repoState.Path == "" {
			return fmt.Errorf("repository %s has not completed setup", repository.ID)
		}
		if repository.Config.SourceID == "" {
			return fmt.Errorf("repository %s has no configured source id", repository.ID)
		}
		resolved := setupOptions{
			Contextctl: valueOr(options.Contextctl, repoState.ValidationRuntime.ContextctlPath),
			Registry:   valueOr(options.Registry, repoState.ValidationRuntime.RegistryPath),
			TrustState: valueOr(options.TrustState, repoState.ValidationRuntime.TrustStatePath),
		}
		if resolved.Contextctl == "" || resolved.Registry == "" || resolved.TrustState == "" {
			return fmt.Errorf("repository %s has incomplete validation dependency paths", repository.ID)
		}
		runtimeState, buildErr := buildValidationRuntime(resolved, repository, repoState.Path)
		if buildErr != nil {
			return fmt.Errorf("stage repository %s validation runtime: %w", repository.ID, buildErr)
		}
		staged[repository.ID] = runtimeState
		revisions[repository.ID] = repoState.Revision
	}
	currentConfig, err := os.ReadFile(a.configPath)
	if err != nil || !bytes.Equal(configSnapshot, currentConfig) {
		return fmt.Errorf("portable configuration changed during setup")
	}
	if err := state.Update(a.statePath, func(current *state.State) error {
		for _, repository := range repositories {
			repoState, ok := repositoryState(*current, repository)
			if !ok || repoState.Revision != revisions[repository.ID] {
				return errRepositoryStateCAS
			}
			if reflect.DeepEqual(repoState.ValidationRuntime, staged[repository.ID]) {
				continue
			}
			if repoState.Revision == ^uint64(0) {
				return fmt.Errorf("repository state revision exhausted")
			}
			repoState.Revision++
			repoState.ValidationRuntime = staged[repository.ID]
			current.Repositories[repository.ID] = repoState
			if repository.Config.IsLegacy() && repository.LegacyStateKey != repository.ID {
				delete(current.Repositories, repository.LegacyStateKey)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if _, err := a.refreshContextStatus("stopped"); err != nil {
		return fmt.Errorf("refresh context status: %w", err)
	}
	fmt.Fprintf(a.out, "Refreshed validation dependencies for %d repositories.\n", len(repositories))
	return nil
}

func (a *Application) acquireRepositoryLocks(repositories []configuredRepository) (func(), error) {
	releases := make([]func(), 0, len(repositories))
	for _, repository := range repositories {
		release, err := a.acquireLock(repository.ID)
		if err != nil {
			for index := len(releases) - 1; index >= 0; index-- {
				releases[index]()
			}
			return nil, fmt.Errorf("repository %s is already being synchronized", repository.ID)
		}
		releases = append(releases, release)
	}
	return func() {
		for index := len(releases) - 1; index >= 0; index-- {
			releases[index]()
		}
	}, nil
}

func (a *Application) managedSkillLinkRequest(repositories []configuredRepository) (validation.MaintenanceRequest, error) {
	machineState, err := state.Load(a.statePath)
	if err != nil {
		return validation.MaintenanceRequest{}, err
	}
	if hasPendingCompletions(repositories, machineState) {
		return validation.MaintenanceRequest{}, fmt.Errorf("repository completion is pending")
	}
	if err := validateUniqueRepositoryPaths(repositories, machineState); err != nil {
		return validation.MaintenanceRequest{}, err
	}
	if err := a.reconcileRuntimeSnapshot(repositories, machineState); err != nil {
		return validation.MaintenanceRequest{}, err
	}
	var common *state.ValidationRuntime
	paths := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		repoState, ok := repositoryState(machineState, repository)
		if !ok || repoState.Path == "" || repoState.Failure != nil || repoState.AcceptedCommit == "" || repoState.AcceptedTree == "" || repoState.Validation.ObjectID != repoState.AcceptedTree || repoState.Validation.Verdict != string(validation.VerdictPass) {
			return validation.MaintenanceRequest{}, fmt.Errorf("repository %s is not healthy", repository.ID)
		}
		paths = append(paths, repoState.Path)
		candidate := repoState.ValidationRuntime
		candidate.SourceID = ""
		if common == nil {
			common = &candidate
		} else if !reflect.DeepEqual(*common, candidate) {
			return validation.MaintenanceRequest{}, fmt.Errorf("configured repositories do not share one pinned validation runtime")
		}
	}
	if common == nil {
		return validation.MaintenanceRequest{}, fmt.Errorf("no repositories are enrolled")
	}
	statusReport, err := a.refreshContextStatus("")
	if err != nil {
		return validation.MaintenanceRequest{}, err
	}
	if len(statusReport.Repositories) != len(repositories) {
		return validation.MaintenanceRequest{}, fmt.Errorf("context status is incomplete")
	}
	for _, repository := range statusReport.Repositories {
		if repository.Error != nil || repository.AcceptedCommit == "" || repository.LastValidation == nil || repository.LastValidation.TreeObjectID == "" || repository.LastValidation.Verdict != string(validation.VerdictPass) {
			return validation.MaintenanceRequest{}, fmt.Errorf("context status contains an unhealthy repository")
		}
	}
	return validation.MaintenanceRequest{
		Runtime: validation.Runtime{
			ContextctlPath:   common.ContextctlPath,
			ContextctlDigest: common.ContextctlDigest,
			RegistryPath:     common.RegistryPath,
			RegistryDigest:   common.RegistryDigest,
			TrustStatePath:   common.TrustStatePath,
			TrustStateDigest: common.TrustStateDigest,
		},
		RepositoryPaths: paths,
		StatusPath:      filepath.Join(a.configDir, "context-status.json"),
	}, nil
}

func (a *Application) runManagedSkillMaintenance(ctx context.Context) {
	machineState, err := state.Load(a.statePath)
	if err != nil || !machineState.ManagedSkillLinks {
		return
	}
	cfg, err := config.Load(a.configPath)
	if err != nil {
		a.recordMaintenanceFailure(maintenanceCodeRuntime, nil)
		return
	}
	repositories, err := configuredRepositories(cfg, "")
	if err != nil {
		a.recordMaintenanceFailure(maintenanceCodeRuntime, nil)
		return
	}
	release, err := a.acquireRepositoryLocks(repositories)
	if err != nil {
		// A concurrent repository operation means this was not an exclusive
		// full-cycle completion barrier. Defer to the next full cycle rather
		// than turn transient local contention into operator-facing health.
		a.logf("managed skill-link maintenance deferred while a repository operation is active")
		return
	}
	defer release()
	request, err := a.managedSkillLinkRequest(repositories)
	if err != nil {
		a.recordMaintenanceFailure(maintenanceCodeRuntime, nil)
		return
	}
	report, err := a.maintainer.ApplyManagedSkillLinks(ctx, request)
	if err != nil {
		code := maintenanceCodeProtocol
		if validation.ErrorCode(err) == validation.CodeMaintenanceUnsafe {
			code = maintenanceCodeUnsafePlan
		}
		a.recordMaintenanceFailure(code, nil)
		return
	}
	if report.Verdict != validation.VerdictPass {
		a.recordMaintenanceFailure(maintenanceCodeHeld, &report)
		return
	}
	a.recordMaintenanceSuccess()
}

func maintenanceReportCheck(report validation.MaintenanceReport) string {
	for _, check := range report.Checks {
		if check.Status != "pass" {
			return check.ID
		}
	}
	return string(report.Verdict)
}

func (a *Application) recordMaintenanceSuccess() {
	now := a.now().UTC()
	if err := state.Update(a.statePath, func(current *state.State) error {
		if !current.ManagedSkillLinks {
			return nil
		}
		current.Maintenance = state.Maintenance{LastAttempt: now, LastSuccess: now}
		return nil
	}); err != nil {
		a.logf("managed skill-link maintenance state update failed")
	}
}

func (a *Application) recordMaintenanceFailure(code string, report *validation.MaintenanceReport) {
	now := a.now().UTC()
	findings := []state.Finding{}
	if report != nil {
		for _, check := range report.Checks {
			if check.Status == "pass" {
				continue
			}
			if len(check.Paths) == 0 {
				findings = append(findings, state.Finding{CheckID: check.ID})
			}
			for _, path := range check.Paths {
				findings = append(findings, state.Finding{CheckID: check.ID, Path: path})
			}
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].CheckID == findings[j].CheckID {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].CheckID < findings[j].CheckID
	})
	hash := sha256.New()
	_, _ = hash.Write([]byte(code))
	for _, finding := range findings {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(finding.CheckID))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(finding.Path))
	}
	fingerprint := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	var notifyVersion uint64
	var shouldNotify bool
	err := state.Update(a.statePath, func(current *state.State) error {
		if !current.ManagedSkillLinks {
			return nil
		}
		maintenance := current.Maintenance
		maintenance.LastAttempt = now
		maintenance.Failure = &state.Failure{Code: code, Phase: "maintenance", Summary: "managed skill-link maintenance requires attention", OccurredAt: now, Findings: findings}
		shouldNotify = maintenance.Notification.FailureKey != fingerprint ||
			(maintenance.Notification.Status == "pending" && (maintenance.Notification.AttemptedAt.IsZero() || now.Sub(maintenance.Notification.AttemptedAt) >= notificationPendingRetry))
		notifyVersion = maintenance.Notification.Version
		if shouldNotify {
			if notifyVersion == ^uint64(0) {
				return fmt.Errorf("maintenance notification version exhausted")
			}
			notifyVersion++
			maintenance.Notification = state.Notification{FailureKey: fingerprint, Status: "pending", AttemptedAt: now, Version: notifyVersion}
		}
		current.Maintenance = maintenance
		return nil
	})
	if err != nil {
		a.logf("managed skill-link maintenance state update failed")
		return
	}
	a.logf("managed skill-link maintenance failed code=%s", code)
	if !shouldNotify {
		return
	}
	notificationContext, cancel := context.WithTimeout(context.Background(), notificationTimeout)
	delivery := a.notifier.Notify(notificationContext, notify.Message{Title: "Repo Sync maintenance requires attention", Body: fmt.Sprintf("Managed skill-link maintenance failed check %s.", code)})
	cancel()
	if err := state.Update(a.statePath, func(current *state.State) error {
		notification := current.Maintenance.Notification
		if notification.FailureKey != fingerprint || notification.Status != "pending" || notification.Version != notifyVersion {
			return errNotificationCASMiss
		}
		notification.Status = string(delivery)
		notification.AttemptedAt = a.now().UTC()
		current.Maintenance.Notification = notification
		return nil
	}); err != nil && !errors.Is(err, errNotificationCASMiss) {
		a.logf("managed skill-link notification state update failed")
	}
}
