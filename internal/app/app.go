package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JorgeMuehlebach/repo-sync/internal/atomicfile"
	"github.com/JorgeMuehlebach/repo-sync/internal/background"
	"github.com/JorgeMuehlebach/repo-sync/internal/config"
	"github.com/JorgeMuehlebach/repo-sync/internal/discovery"
	"github.com/JorgeMuehlebach/repo-sync/internal/gitops"
	"github.com/JorgeMuehlebach/repo-sync/internal/notify"
	"github.com/JorgeMuehlebach/repo-sync/internal/securefile"
	"github.com/JorgeMuehlebach/repo-sync/internal/state"
	"github.com/JorgeMuehlebach/repo-sync/internal/strictjson"
	"github.com/JorgeMuehlebach/repo-sync/internal/validation"
	"gopkg.in/yaml.v3"
)

const (
	repositoryOperationTimeout = 20 * time.Minute
	repositoryLockWait         = 5 * time.Second
	notificationTimeout        = 10 * time.Second
	notificationPendingRetry   = 2 * notificationTimeout
	maxContextctlBytes         = 128 << 20
	maxRuntimeJSONBytes        = 4 << 20
	maxContextStatusBytes      = 1 << 20
	contextStatusHeartbeat     = 30 * time.Second
	contextRegistrySchema      = 2
	contextStatusSchema        = 2
	contextStatusProtocol      = "repo-sync.context-status.v2"
)

var (
	statusRepositoryIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,127}$`)
	statusDottedIDPattern     = regexp.MustCompile(`^[a-z0-9]+(?:[.-][a-z0-9]+)+$`)
	statusCheckIDPattern      = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:-[A-Z0-9]+)+$`)
	statusDigestPattern       = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	statusObjectIDPattern     = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	statusSemverPattern       = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

type Application struct {
	version        string
	in             io.Reader
	out            io.Writer
	errOut         io.Writer
	configDir      string
	configPath     string
	statePath      string
	logPath        string
	lockDir        string
	homeDir        string
	publisher      gitops.Engine
	mirror         gitops.Engine
	validator      validation.Validator
	notifier       notify.Notifier
	now            func() time.Time
	serviceFactory func() (background.Controller, error)
	// writeContextStatus is injectable only for completion-barrier fault tests.
	// Production leaves it nil and uses the durable atomic writer.
	writeContextStatus func(string, []byte, os.FileMode) error
	// afterEngineSync is a test-only scheduling hook for proving that config
	// removal remains serialized after a Git side effect and before state commit.
	afterEngineSync func()
	// Completion hooks are test-only crash/concurrency checkpoints. Production
	// leaves both nil.
	afterPendingCompletion func() error
	afterStatusBarrier     func() error
	// reconcileRuntime exists so tests can isolate orchestration behavior. Production
	// callers leave it nil and always use the fail-closed reconciliation below.
	reconcileRuntime func(state.Repository, configuredRepository) error
}

func (a *Application) reconcileStoredRuntime(repositoryState state.Repository, repository configuredRepository) error {
	if a.reconcileRuntime != nil {
		return a.reconcileRuntime(repositoryState, repository)
	}
	return validateStoredRuntime(repositoryState, repository)
}

func (a *Application) reconcilePinnedTarget(pinnedRepository configuredRepository, pinnedState state.Repository) error {
	currentRepository, repositories, err := a.reconciledPortableRepository(pinnedRepository)
	if err != nil {
		return err
	}
	machineState, err := state.Load(a.statePath)
	if err != nil {
		return err
	}
	if err := validateUniqueRepositoryPaths(repositories, machineState); err != nil {
		return err
	}
	currentState, ok := repositoryState(machineState, currentRepository)
	if !ok || currentState.ID != pinnedState.ID || currentState.Mode != pinnedState.Mode || currentState.Path != pinnedState.Path || currentState.ValidationRuntime != pinnedState.ValidationRuntime || currentState.AcceptedCommit != pinnedState.AcceptedCommit || currentState.AcceptedTree != pinnedState.AcceptedTree || currentState.Revision != pinnedState.Revision {
		return fmt.Errorf("machine-local repository binding changed")
	}
	return a.reconcileStoredRuntime(currentState, currentRepository)
}

func (a *Application) reconcilePortableRepository(pinnedRepository configuredRepository) error {
	_, _, err := a.reconciledPortableRepository(pinnedRepository)
	return err
}

func (a *Application) reconciledPortableRepository(pinnedRepository configuredRepository) (configuredRepository, []configuredRepository, error) {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return configuredRepository{}, nil, err
	}
	repositories, err := configuredRepositories(cfg, "")
	if err != nil {
		return configuredRepository{}, nil, err
	}
	var currentRepository *configuredRepository
	for index := range repositories {
		if repositories[index].ID == pinnedRepository.ID {
			if currentRepository != nil {
				return configuredRepository{}, nil, fmt.Errorf("repository identity is no longer unique")
			}
			currentRepository = &repositories[index]
		}
	}
	if currentRepository == nil || currentRepository.Config != pinnedRepository.Config || currentRepository.Spec != pinnedRepository.Spec || currentRepository.LegacyStateKey != pinnedRepository.LegacyStateKey || currentRepository.Revision != pinnedRepository.Revision {
		return configuredRepository{}, nil, fmt.Errorf("portable repository configuration changed")
	}
	return *currentRepository, repositories, nil
}

func (a *Application) reconcileRuntimeSnapshot(repositories []configuredRepository, machineState state.State) error {
	if err := validateUniqueRepositoryPaths(repositories, machineState); err != nil {
		return err
	}
	for _, repository := range repositories {
		repositoryState, ok := repositoryState(machineState, repository)
		if !ok || repository.Config.SourceID == "" || repositoryState.Path == "" || repositoryState.ValidationRuntime.SourceID == "" {
			return &unreconciledStatusError{RepositoryID: repository.ID}
		}
		if err := a.reconcileStoredRuntime(repositoryState, repository); err != nil {
			return fmt.Errorf("repository %s validation runtime is not reconciled: %w", repository.ID, err)
		}
	}
	return nil
}

func (a *Application) preflightRepositories(ctx context.Context, repositories []configuredRepository, machineState state.State) error {
	if err := a.reconcileRuntimeSnapshot(repositories, machineState); err != nil {
		return err
	}
	for _, repository := range repositories {
		repositoryState, _ := repositoryState(machineState, repository)
		runner, err := pinnedGitRunner(ctx, repositoryState)
		if err != nil {
			return fmt.Errorf("repository %s trusted Git dependency is unavailable: %w", repository.ID, err)
		}
		pinnedRepository, pinnedState := repository, repositoryState
		target := gitops.Target{
			ID:                     repository.ID,
			Path:                   repositoryState.Path,
			Spec:                   repository.Spec,
			ValidationRuntime:      validationRuntime(repositoryState.ValidationRuntime),
			ExpectedAcceptedCommit: repositoryState.AcceptedCommit,
			Reconcile: func(context.Context) error {
				return a.reconcilePinnedTarget(pinnedRepository, pinnedState)
			},
		}
		var failure *gitops.OperationError
		if repository.Config.EffectiveMode() == config.ModeMirror {
			failure = gitops.NewMirror(runner, validation.Unavailable{}).ValidateTarget(ctx, target)
		} else {
			failure = gitops.ValidateTarget(ctx, runner, target)
		}
		if failure != nil {
			return fmt.Errorf("repository %s preflight failed: %s", repository.ID, failure.Code)
		}
	}
	return nil
}

func New(version string, in io.Reader, out, errOut io.Writer) (*Application, error) {
	dir, err := config.DefaultDir()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find home directory: %w", err)
	}
	return &Application{
		version:    version,
		in:         in,
		out:        out,
		errOut:     errOut,
		configDir:  dir,
		configPath: filepath.Join(dir, "config.yaml"),
		statePath:  filepath.Join(dir, "state.json"),
		logPath:    filepath.Join(dir, "repo-sync.log"),
		lockDir:    filepath.Join(dir, "locks"),
		homeDir:    home,
		validator:  validation.Runner{},
		notifier:   notify.NewNative(),
		now:        time.Now,
	}, nil
}

func (a *Application) defaults() {
	if a.validator == nil {
		a.validator = validation.Unavailable{}
	}
	if a.notifier == nil {
		a.notifier = notify.NewNative()
	}
	if a.now == nil {
		a.now = time.Now
	}
}

func (a *Application) Run(args []string) error {
	a.defaults()
	if len(args) == 0 {
		a.printUsage()
		return nil
	}
	switch args[0] {
	case "help", "-h", "--help":
		a.printUsage()
		return nil
	case "version", "-v", "--version":
		fmt.Fprintln(a.out, a.version)
		return nil
	case "setup":
		options, err := parseSetupOptions(args[1:])
		if err != nil {
			return err
		}
		return a.setup(context.Background(), options)
	case "start":
		return a.start()
	case "stop":
		return a.stop()
	case "uninstall":
		return a.uninstall()
	case "status":
		options, err := parseStatusOptions(args[1:])
		if err != nil {
			return err
		}
		return a.status(options)
	case "sync":
		selector, err := parseRepositorySelector(args[1:])
		if err != nil {
			return err
		}
		return a.syncAll(context.Background(), true, selector)
	case "config":
		return a.configCommand(args[1:])
	case "run":
		return a.runService()
	default:
		return fmt.Errorf("unknown command %q; run repo-sync help", args[0])
	}
}

func (a *Application) printUsage() {
	fmt.Fprintln(a.out, `Repo Sync keeps selected GitHub branches synchronized across computers.

Usage:
  repo-sync setup [--repository <id>] [--path <path>] [validation options]
  repo-sync start
  repo-sync stop
  repo-sync status [--repository <id>] [--json]
  repo-sync sync [--repository <id>]
  repo-sync uninstall
  repo-sync config add <branch-url> [--id <id> --mode <publish|mirror> --source <source-id>]
  repo-sync config remove <branch-url-or-id>
  repo-sync config list
  repo-sync version

Validation options:
  --contextctl <absolute-path> --registry <absolute-path> --trust-state <absolute-path>`)
}

type configuredRepository struct {
	Config         config.Repository
	Spec           discovery.RepositorySpec
	ID             string
	LegacyStateKey string
	Revision       uint64
}

func configuredRepositories(cfg config.Config, selector string) ([]configuredRepository, error) {
	var repositories []configuredRepository
	for _, repository := range cfg.Repositories {
		spec, err := discovery.ParseGitHubBranchURL(repository.URL)
		if err != nil {
			return nil, err
		}
		id, err := repository.StateKey()
		if err != nil {
			return nil, err
		}
		legacyKey, err := repository.LegacyStateKey()
		if err != nil {
			return nil, err
		}
		if selector != "" && selector != id {
			continue
		}
		repositories = append(repositories, configuredRepository{Config: repository, Spec: spec, ID: id, LegacyStateKey: legacyKey, Revision: repository.EffectiveRevision()})
	}
	if selector != "" && len(repositories) == 0 {
		return nil, fmt.Errorf("repository %q is not configured", selector)
	}
	return repositories, nil
}

func (a *Application) configCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("config requires add, remove, or list")
	}
	cfg, err := config.Ensure(a.configPath)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("usage: repo-sync config list")
		}
		repositories, err := configuredRepositories(cfg, "")
		if err != nil {
			return err
		}
		if len(repositories) == 0 {
			fmt.Fprintln(a.out, "No repositories configured.")
		}
		for _, repository := range repositories {
			fmt.Fprintf(a.out, "%s  %s  %s", repository.ID, repository.Config.EffectiveMode(), repository.Spec.OriginalURL)
			if repository.Config.SourceID != "" {
				fmt.Fprintf(a.out, "  %s", repository.Config.SourceID)
			}
			fmt.Fprintln(a.out)
		}
		fmt.Fprintf(a.out, "Config: %s\n", a.configPath)
		return nil
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: repo-sync config add <branch-url> [--id <id> --mode <mode> --source <source-id>]")
		}
		candidate, err := discovery.ParseGitHubBranchURL(args[1])
		if err != nil {
			return err
		}
		id, mode, sourceID, err := parseConfigAddOptions(args[2:])
		if err != nil {
			return err
		}
		var entry config.Repository
		if id == "" || mode == "" || sourceID == "" {
			return fmt.Errorf("new repositories require --id, --mode, and --source; legacy scalar entries remain readable for migration")
		}
		entry = config.StructuredRepository(id, candidate.OriginalURL, config.Mode(mode), sourceID)
		if err := config.Update(a.configPath, func(current *config.Config) error {
			current.Repositories = append(current.Repositories, entry)
			return nil
		}); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "Added %s\n", candidate.OriginalURL)
		if _, err := a.refreshContextStatus(""); err != nil {
			var unreconciled *unreconciledStatusError
			if !errors.As(err, &unreconciled) {
				return fmt.Errorf("refresh context status: %w", err)
			}
			fmt.Fprintln(a.out, "Context status refresh is withheld until every configured repository completes setup.")
			a.logf("context status refresh withheld after config add")
		}
		return nil
	case "remove":
		if len(args) != 2 {
			return fmt.Errorf("usage: repo-sync config remove <branch-url-or-id>")
		}
		removeValue := args[1]
		configured, err := configuredRepositories(cfg, "")
		if err != nil {
			return err
		}
		var removing []configuredRepository
		for _, repository := range configured {
			if repository.ID == removeValue || repository.Config.URL == removeValue {
				removing = append(removing, repository)
			}
		}
		if len(removing) == 0 {
			return fmt.Errorf("%s is not configured", removeValue)
		}
		sort.Slice(removing, func(i, j int) bool { return removing[i].ID < removing[j].ID })
		var releases []func()
		for _, repository := range removing {
			release, lockErr := a.acquireLockWithTimeout(repository.ID, repositoryLockWait)
			if lockErr != nil {
				for index := len(releases) - 1; index >= 0; index-- {
					releases[index]()
				}
				return fmt.Errorf("repository %s is being synchronized; try removal again", repository.ID)
			}
			releases = append(releases, release)
		}
		defer func() {
			for index := len(releases) - 1; index >= 0; index-- {
				releases[index]()
			}
		}()
		machineState, err := state.Load(a.statePath)
		if err != nil {
			return err
		}
		for _, repository := range removing {
			if repoState, ok := repositoryState(machineState, repository); ok && repoState.PendingCompletion != nil {
				return fmt.Errorf("repository %s has a pending completion; run sync recovery before removal", repository.ID)
			}
		}
		pinned := make(map[string]configuredRepository, len(removing))
		var removedKeys []string
		for _, repository := range removing {
			pinned[repository.ID] = repository
			removedKeys = append(removedKeys, repository.ID)
			if repository.Config.IsLegacy() {
				removedKeys = append(removedKeys, repository.LegacyStateKey)
			}
		}
		if err := config.Update(a.configPath, func(current *config.Config) error {
			filtered := make([]config.Repository, 0, len(current.Repositories))
			removed := make(map[string]bool, len(removing))
			for _, repository := range current.Repositories {
				id, _ := repository.StateKey()
				if id == removeValue || repository.URL == removeValue {
					expected, ok := pinned[id]
					if !ok || expected.Config != repository || expected.Revision != repository.EffectiveRevision() {
						return fmt.Errorf("portable repository configuration changed during removal")
					}
					removed[id] = true
					continue
				}
				filtered = append(filtered, repository)
			}
			if len(removed) != len(pinned) {
				return fmt.Errorf("portable repository configuration changed during removal")
			}
			current.Repositories = filtered
			return nil
		}); err != nil {
			return err
		}
		if err := state.Update(a.statePath, func(current *state.State) error {
			for _, key := range removedKeys {
				delete(current.Repositories, key)
			}
			return nil
		}); err != nil {
			return err
		}
		if _, err := a.refreshContextStatus(""); err != nil {
			return fmt.Errorf("refresh context status: %w", err)
		}
		fmt.Fprintf(a.out, "Removed %s\n", removeValue)
		return nil
	default:
		return fmt.Errorf("unknown config command %q", args[0])
	}
}

type setupOptions struct {
	RepositoryID string
	Path         string
	Contextctl   string
	Registry     string
	TrustState   string
}

func parseSetupOptions(args []string) (setupOptions, error) {
	var options setupOptions
	for len(args) > 0 {
		if len(args) < 2 {
			return setupOptions{}, fmt.Errorf("setup option %s requires a value", args[0])
		}
		switch args[0] {
		case "--repository":
			options.RepositoryID = args[1]
		case "--path":
			options.Path = args[1]
		case "--contextctl":
			options.Contextctl = args[1]
		case "--registry":
			options.Registry = args[1]
		case "--trust-state":
			options.TrustState = args[1]
		default:
			return setupOptions{}, fmt.Errorf("unknown setup option %q", args[0])
		}
		args = args[2:]
	}
	if options.Path != "" && options.RepositoryID == "" {
		return setupOptions{}, fmt.Errorf("--path requires --repository")
	}
	if options.Registry == "" {
		return setupOptions{}, fmt.Errorf("--registry is required so Repo Sync can use the pinned Git executable")
	}
	if (options.Contextctl == "") != (options.TrustState == "") {
		return setupOptions{}, fmt.Errorf("--contextctl and --trust-state must be supplied together")
	}
	if options.Contextctl != "" && options.RepositoryID == "" {
		return setupOptions{}, fmt.Errorf("validation options require --repository")
	}
	return options, nil
}

func parseStatusOptions(args []string) (statusOptions, error) {
	var options statusOptions
	for len(args) > 0 {
		switch args[0] {
		case "--json":
			options.JSON = true
			args = args[1:]
		case "--repository":
			if len(args) < 2 {
				return statusOptions{}, fmt.Errorf("--repository requires a value")
			}
			options.RepositoryID = args[1]
			args = args[2:]
		default:
			return statusOptions{}, fmt.Errorf("unknown status option %q", args[0])
		}
	}
	return options, nil
}

func parseRepositorySelector(args []string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	if len(args) == 2 && args[0] == "--repository" && args[1] != "" {
		return args[1], nil
	}
	return "", fmt.Errorf("usage: repo-sync sync [--repository <id>]")
}

func parseConfigAddOptions(args []string) (string, string, string, error) {
	var id, mode, source string
	for len(args) > 0 {
		if len(args) < 2 {
			return "", "", "", fmt.Errorf("config option %s requires a value", args[0])
		}
		switch args[0] {
		case "--id":
			id = args[1]
		case "--mode":
			mode = args[1]
		case "--source":
			source = args[1]
		default:
			return "", "", "", fmt.Errorf("unknown config option %q", args[0])
		}
		args = args[2:]
	}
	return id, mode, source, nil
}

func (a *Application) setup(ctx context.Context, options setupOptions) error {
	_, _, _, registryData, err := stableRuntimeFile(options.Registry, "", true, maxRuntimeJSONBytes)
	if err != nil {
		return fmt.Errorf("registry path is not a canonical regular file: %w", err)
	}
	gitRunner, err := gitops.NewSystemRunnerFromRegistryContext(ctx, registryData)
	if err != nil {
		return fmt.Errorf("trusted Git dependency is unavailable: %w", err)
	}
	cfg, err := config.Ensure(a.configPath)
	if err != nil {
		return err
	}
	configSnapshot, err := os.ReadFile(a.configPath)
	if err != nil {
		return err
	}
	repositories, err := configuredRepositories(cfg, options.RepositoryID)
	if err != nil {
		return err
	}
	if len(repositories) == 0 {
		return fmt.Errorf("no repositories configured; run repo-sync config add <github-branch-url>")
	}
	allRepositories, err := configuredRepositories(cfg, "")
	if err != nil {
		return err
	}
	var setupLocks []func()
	for _, repository := range allRepositories {
		release, lockErr := a.acquireLock(repository.ID)
		if lockErr != nil {
			for index := len(setupLocks) - 1; index >= 0; index-- {
				setupLocks[index]()
			}
			return fmt.Errorf("repository %s is already being synchronized", repository.ID)
		}
		setupLocks = append(setupLocks, release)
	}
	defer func() {
		for index := len(setupLocks) - 1; index >= 0; index-- {
			setupLocks[index]()
		}
	}()
	setupState, err := state.Load(a.statePath)
	if err != nil {
		return err
	}
	if hasPendingCompletions(allRepositories, setupState) {
		return fmt.Errorf("repository completion recovery is pending; run repo-sync sync before setup")
	}
	wanted := make(map[string]bool)
	for _, repository := range repositories {
		wanted[repository.Spec.Key] = true
	}
	var matches map[string][]string
	if options.Path == "" {
		roots := []string{a.homeDir}
		for _, root := range cfg.SearchRoots {
			expanded, expandErr := a.expandPath(root)
			if expandErr != nil {
				return expandErr
			}
			roots = append(roots, expanded)
		}
		fmt.Fprintln(a.out, "Searching for configured repositories...")
		matches, err = discovery.Find(ctx, gitRunner, roots, wanted)
		if err != nil {
			return err
		}
	}
	reader := bufio.NewReader(a.in)
	selected := make(map[string]state.Repository)
	selectedPresent := make(map[string]bool)
	selectedRevision := make(map[string]uint64)
	bootstrappedMirrors := make(map[string]bool)
	for _, repository := range repositories {
		current, loadErr := state.Load(a.statePath)
		if loadErr != nil {
			return loadErr
		}
		var selectedPath string
		var bootstrapOutcome gitops.Outcome
		if repository.Config.EffectiveMode() == config.ModeMirror {
			rawPath := options.Path
			if rawPath == "" {
				if existing, ok := repositoryState(current, repository); ok {
					rawPath = existing.Path
				}
			}
			selectedPath, bootstrapOutcome, bootstrappedMirrors[repository.ID], err = a.selectMirrorRepository(ctx, gitRunner, reader, repository, rawPath)
		} else if options.Path != "" {
			selectedPath, err = a.selectExplicitRepository(ctx, gitRunner, repository.Spec, options.Path)
		} else {
			selectedPath, err = a.selectRepository(ctx, gitRunner, reader, repository.Spec, matches[repository.Spec.Key])
		}
		if err != nil {
			return err
		}
		selectedPath, err = canonicalConfiguredRepositoryRoot(repository, selectedPath)
		if err != nil {
			return err
		}
		if err := discovery.ValidatePath(ctx, gitRunner, selectedPath, repository.Spec); err != nil {
			return err
		}
		repoState := state.Repository{ID: repository.ID, Mode: string(repository.Config.EffectiveMode()), Path: selectedPath}
		if existing, ok := current.Repositories[repository.ID]; ok {
			repoState = existing
			selectedPresent[repository.ID] = true
			selectedRevision[repository.ID] = existing.Revision
			repoState.ID = repository.ID
			repoState.Mode = string(repository.Config.EffectiveMode())
			repoState.Path = selectedPath
		} else if repository.Config.IsLegacy() {
			if existing, ok := current.Repositories[repository.LegacyStateKey]; ok {
				repoState = existing
				selectedPresent[repository.ID] = true
				selectedRevision[repository.ID] = existing.Revision
				repoState.ID = repository.ID
				repoState.Mode = string(repository.Config.EffectiveMode())
				repoState.Path = selectedPath
			}
		}
		if bootstrappedMirrors[repository.ID] {
			repoState.CandidateCommit = bootstrapOutcome.CandidateCommit
			repoState.CandidateTree = bootstrapOutcome.CandidateTree
		}
		if options.Contextctl != "" && !bootstrappedMirrors[repository.ID] {
			if repository.Config.SourceID == "" {
				return fmt.Errorf("repository %s has no configured source id", repository.ID)
			}
			runtime, runtimeErr := buildValidationRuntime(options, repository, selectedPath)
			if runtimeErr != nil {
				return runtimeErr
			}
			repoState.ValidationRuntime = runtime
		}
		selected[repository.ID] = repoState
		fmt.Fprintf(a.out, "Configured %s (%s, %s) at %s\n", repository.ID, repository.Config.EffectiveMode(), repository.Spec.Branch, selectedPath)
	}
	prospective, err := state.Load(a.statePath)
	if err != nil {
		return err
	}
	for id, repoState := range selected {
		prospective.Repositories[id] = repoState
	}
	if err := validateUniqueRepositoryPaths(allRepositories, prospective); err != nil {
		return err
	}
	currentConfig, err := os.ReadFile(a.configPath)
	if err != nil || !bytes.Equal(configSnapshot, currentConfig) {
		return fmt.Errorf("portable configuration changed during setup")
	}
	if err := state.Update(a.statePath, func(current *state.State) error {
		for _, repository := range repositories {
			existing, exists := repositoryState(*current, repository)
			if exists != selectedPresent[repository.ID] || (exists && existing.Revision != selectedRevision[repository.ID]) {
				return errRepositoryStateCAS
			}
			repoState := selected[repository.ID]
			if repoState.Revision == ^uint64(0) {
				return fmt.Errorf("repository state revision exhausted")
			}
			repoState.Revision++
			current.Repositories[repository.ID] = repoState
			if repository.Config.IsLegacy() && repository.LegacyStateKey != repository.ID {
				delete(current.Repositories, repository.LegacyStateKey)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if _, err := a.refreshContextStatus(""); err != nil {
		var unreconciled *unreconciledStatusError
		if !errors.As(err, &unreconciled) {
			return fmt.Errorf("refresh context status: %w", err)
		}
		fmt.Fprintln(a.out, "Context status refresh is withheld until every configured repository completes validation setup.")
		a.logf("context status refresh withheld after setup")
	}
	for _, repository := range repositories {
		if bootstrappedMirrors[repository.ID] {
			fmt.Fprintf(a.out, "Mirror %s was initialized at its stable path. Register that exact path with contextctl, then rerun setup with --contextctl, --registry, and --trust-state.\n", repository.ID)
		}
	}
	fmt.Fprintf(a.out, "Setup complete. Config: %s\n", a.configPath)
	return nil
}

func buildValidationRuntime(options setupOptions, repository configuredRepository, repositoryPath string) (state.ValidationRuntime, error) {
	contextctl, contextctlInfo, contextctlDigest, _, err := stableRuntimeFile(options.Contextctl, repositoryPath, false, maxContextctlBytes)
	if err != nil || (runtime.GOOS != "windows" && contextctlInfo.Mode()&0o111 == 0) {
		return state.ValidationRuntime{}, fmt.Errorf("contextctl path is not a canonical regular file")
	}
	registry, _, registryDigest, registryData, err := stableRuntimeFile(options.Registry, repositoryPath, true, maxRuntimeJSONBytes)
	if err != nil {
		return state.ValidationRuntime{}, fmt.Errorf("registry path is not a canonical regular file")
	}
	trustState, _, trustStateDigest, trustStateData, err := stableRuntimeFile(options.TrustState, repositoryPath, true, maxRuntimeJSONBytes)
	if err != nil {
		return state.ValidationRuntime{}, fmt.Errorf("trust-state path is not a canonical regular file")
	}
	registryRevision, err := readRevision(registryData, "registry_revision")
	if err != nil || registryRevision < 1 {
		return state.ValidationRuntime{}, fmt.Errorf("registry revision is invalid")
	}
	if _, err := readContextContractVersion(registryData); err != nil {
		return state.ValidationRuntime{}, fmt.Errorf("registry contract version is unsupported")
	}
	if err := reconcileValidationSource(registryData, repository, repositoryPath); err != nil {
		return state.ValidationRuntime{}, err
	}
	trustRevision, err := readRevision(trustStateData, "state_revision")
	if err != nil || trustRevision < 0 {
		return state.ValidationRuntime{}, fmt.Errorf("trust-state revision is invalid")
	}
	if _, err := readContextContractVersion(trustStateData); err != nil {
		return state.ValidationRuntime{}, fmt.Errorf("trust-state contract version is unsupported")
	}
	return state.ValidationRuntime{
		Protocol:           validation.ReportProtocol,
		SourceID:           repository.Config.SourceID,
		ContextctlPath:     contextctl,
		ContextctlDigest:   contextctlDigest,
		RegistryPath:       registry,
		RegistryRevision:   registryRevision,
		RegistryDigest:     registryDigest,
		TrustStatePath:     trustState,
		TrustStateRevision: trustRevision,
		TrustStateDigest:   trustStateDigest,
	}, nil
}

type registrySnapshot struct {
	SchemaVersion    int                    `json:"schema_version"`
	ContractVersion  string                 `json:"contract_version"`
	Registrations    []registryRegistration `json:"registrations"`
	RegistryRevision int                    `json:"registry_revision"`
}

type registryRegistration struct {
	SourceID string            `json:"source_id"`
	Role     string            `json:"role"`
	Enabled  bool              `json:"enabled"`
	RootPath registryLocalPath `json:"root_path"`
	Git      *registryGit      `json:"git"`
	RepoSync *registryRepoSync `json:"repo_sync"`
}

type registryLocalPath struct {
	Style string `json:"style"`
	Value string `json:"value"`
}

type registryGit struct {
	RemoteName string `json:"remote_name"`
	RemoteURL  string `json:"remote_url"`
	Branch     string `json:"branch"`
}

type registryRepoSync struct {
	RepositoryID string `json:"repository_id"`
	Mode         string `json:"mode"`
}

type sourceManifestSnapshot struct {
	ContractVersion string `yaml:"contract_version"`
	SourceID        string `yaml:"source_id"`
	Publication     struct {
		Mode            string `yaml:"mode"`
		CanonicalBranch string `yaml:"canonical_branch"`
	} `yaml:"publication"`
}

func reconcileValidationSource(data []byte, repository configuredRepository, repositoryPath string) error {
	var registry registrySnapshot
	if err := strictjson.Decode(data, &registry, false); err != nil || registry.SchemaVersion != contextRegistrySchema || !supportedContextContract(registry.ContractVersion) || registry.RegistryRevision < 1 {
		return fmt.Errorf("registry cannot be reconciled with repository %s", repository.ID)
	}
	var matched *registryRegistration
	for index := range registry.Registrations {
		registration := &registry.Registrations[index]
		if registration.SourceID != repository.Config.SourceID || !registeredRootMatches(registration.RootPath, repository, repositoryPath) {
			continue
		}
		if matched != nil {
			return fmt.Errorf("registry contains multiple matching registrations for repository %s", repository.ID)
		}
		matched = registration
	}
	if matched == nil || !matched.Enabled || matched.Git == nil || matched.RepoSync == nil {
		return fmt.Errorf("registry has no enabled Repo Sync registration for repository %s", repository.ID)
	}
	expectedRole, expectedMode := "writable", "publisher"
	if repository.Config.EffectiveMode() == config.ModeMirror {
		expectedRole, expectedMode = "context-mirror", "mirror"
	}
	remoteKey, remoteErr := discovery.CanonicalRemote(matched.Git.RemoteURL)
	if matched.Role != expectedRole || matched.RepoSync.RepositoryID != repository.ID || matched.RepoSync.Mode != expectedMode ||
		matched.Git.RemoteName != "origin" || remoteErr != nil || remoteKey != repository.Spec.Key || matched.Git.Branch != repository.Spec.Branch {
		return fmt.Errorf("registry Repo Sync registration does not match repository %s", repository.ID)
	}
	if err := reconcileSourceManifest(repository, repositoryPath); err != nil {
		return err
	}
	return nil
}

func registeredRootMatches(root registryLocalPath, repository configuredRepository, repositoryPath string) bool {
	expectedStyle := "posix"
	if runtime.GOOS == "windows" {
		expectedStyle = "windows"
	}
	if root.Style != expectedStyle || !filepath.IsAbs(root.Value) {
		return false
	}
	absolute, err := filepath.Abs(root.Value)
	if err != nil || filepath.Clean(root.Value) != absolute {
		return false
	}
	canonical, err := canonicalConfiguredRepositoryRoot(repository, root.Value)
	return err == nil && canonical == absolute && canonical == filepath.Clean(repositoryPath)
}

func reconcileSourceManifest(repository configuredRepository, repositoryPath string) error {
	manifestRoot := repositoryPath
	if repository.Config.EffectiveMode() == config.ModeMirror {
		resolved, err := filepath.EvalSymlinks(repositoryPath)
		if err != nil {
			return fmt.Errorf("repository %s stable mirror pointer is unavailable", repository.ID)
		}
		manifestRoot, err = filepath.Abs(resolved)
		if err != nil {
			return fmt.Errorf("repository %s stable mirror pointer is unavailable", repository.ID)
		}
	}
	file, err := securefile.OpenRegularBeneath(manifestRoot, ".agents/context-source.yaml")
	if err != nil {
		return fmt.Errorf("repository %s source manifest is unavailable", repository.ID)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxRuntimeJSONBytes+1))
	if err != nil || len(data) > maxRuntimeJSONBytes {
		return fmt.Errorf("repository %s source manifest is invalid", repository.ID)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var manifest sourceManifestSnapshot
	if err := decoder.Decode(&manifest); err != nil {
		return fmt.Errorf("repository %s source manifest is invalid", repository.ID)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("repository %s source manifest contains multiple documents", repository.ID)
	}
	publicationModeOK := manifest.Publication.Mode == "repo-sync"
	if repository.Config.EffectiveMode() == config.ModeMirror {
		publicationModeOK = publicationModeOK || manifest.Publication.Mode == "manual-review"
	}
	if !supportedContextContract(manifest.ContractVersion) || manifest.SourceID != repository.Config.SourceID || !publicationModeOK || manifest.Publication.CanonicalBranch != repository.Spec.Branch {
		return fmt.Errorf("repository %s source manifest does not match Repo Sync configuration", repository.ID)
	}
	return nil
}

func stableRuntimeFile(path, repositoryPath string, capture bool, maximumBytes int64) (string, os.FileInfo, string, []byte, error) {
	if !filepath.IsAbs(path) {
		return "", nil, "", nil, fmt.Errorf("path must be absolute")
	}
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > maximumBytes {
		return "", nil, "", nil, fmt.Errorf("path is not a regular file")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, "", nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil || filepath.Clean(path) != canonical {
		return "", nil, "", nil, fmt.Errorf("path is not canonical")
	}
	if repositoryPath != "" {
		repositoryCanonical, err := filepath.EvalSymlinks(repositoryPath)
		if err != nil {
			return "", nil, "", nil, err
		}
		repositoryCanonical, err = filepath.Abs(repositoryCanonical)
		if err != nil || canonical == repositoryCanonical || pathWithin(canonical, repositoryCanonical) {
			return "", nil, "", nil, fmt.Errorf("runtime path is inside the repository")
		}
	}
	file, err := securefile.OpenCanonicalRegular(canonical)
	if err != nil {
		return "", nil, "", nil, err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(before, opened) || opened.Size() < 0 || opened.Size() > maximumBytes {
		_ = file.Close()
		return "", nil, "", nil, fmt.Errorf("path changed while opening")
	}
	hash := sha256.New()
	var data bytes.Buffer
	writer := io.Writer(hash)
	if capture {
		writer = io.MultiWriter(hash, &data)
	}
	written, copyErr := io.Copy(writer, io.LimitReader(file, maximumBytes+1))
	if copyErr != nil || written > maximumBytes {
		_ = file.Close()
		return "", nil, "", nil, fmt.Errorf("path could not be hashed")
	}
	if err := file.Close(); err != nil {
		return "", nil, "", nil, fmt.Errorf("path could not be closed")
	}
	after, err := os.Lstat(canonical)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return "", nil, "", nil, fmt.Errorf("path changed while hashing")
	}
	return canonical, opened, "sha256:" + hex.EncodeToString(hash.Sum(nil)), data.Bytes(), nil
}

func readRevision(data []byte, field string) (int, error) {
	var value map[string]json.RawMessage
	if err := strictjson.Decode(data, &value, false); err != nil {
		return 0, err
	}
	var revision int
	if err := json.Unmarshal(value[field], &revision); err != nil {
		return 0, err
	}
	return revision, nil
}

func readContextContractVersion(data []byte) (string, error) {
	var value map[string]json.RawMessage
	if err := strictjson.Decode(data, &value, false); err != nil {
		return "", err
	}
	var version string
	if err := json.Unmarshal(value["contract_version"], &version); err != nil || !supportedContextContract(version) {
		return "", fmt.Errorf("context contract version is unsupported")
	}
	return version, nil
}

func supportedContextContract(value string) bool {
	match := statusSemverPattern.FindStringSubmatch(value)
	return len(match) >= 3 && match[1] == "2" && match[2] == "0"
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (a *Application) selectExplicitRepository(ctx context.Context, runner gitops.Runner, spec discovery.RepositorySpec, rawPath string) (string, error) {
	selected, err := a.expandPath(rawPath)
	if err != nil {
		return "", err
	}
	info, statErr := os.Stat(selected)
	if statErr == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("%s is not a directory", selected)
		}
		if err := discovery.ValidatePath(ctx, runner, selected, spec); err != nil {
			return "", err
		}
		return canonicalRepositoryRoot(selected)
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	if err := os.MkdirAll(filepath.Dir(selected), 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(a.out, "Cloning %s into %s...\n", spec.CloneURL, selected)
	if err := gitops.SecureClone(ctx, runner, filepath.Dir(selected), selected, spec); err != nil {
		return "", fmt.Errorf("clone %s failed", spec.Key)
	}
	return canonicalRepositoryRoot(selected)
}

func (a *Application) selectMirrorRepository(ctx context.Context, runner gitops.Runner, reader *bufio.Reader, repository configuredRepository, rawPath string) (string, gitops.Outcome, bool, error) {
	if rawPath == "" {
		defaultPath := filepath.Join(a.homeDir, repository.Spec.Name+"-context")
		fmt.Fprintf(a.out, "Enter the stable mirror path for %s [%s]: ", repository.Spec.Key, defaultPath)
		value, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", gitops.Outcome{}, false, err
		}
		rawPath = strings.TrimSpace(value)
		if rawPath == "" {
			rawPath = defaultPath
		}
	}
	selected, err := a.expandPath(rawPath)
	if err != nil {
		return "", gitops.Outcome{}, false, err
	}
	if err := os.MkdirAll(filepath.Dir(selected), 0o755); err != nil {
		return "", gitops.Outcome{}, false, err
	}
	parent, err := canonicalRepositoryRoot(filepath.Dir(selected))
	if err != nil {
		return "", gitops.Outcome{}, false, err
	}
	selected = filepath.Join(parent, filepath.Base(selected))
	if _, err := os.Lstat(selected); err == nil {
		selected, err = canonicalMirrorRoot(selected)
		if err != nil {
			return "", gitops.Outcome{}, false, err
		}
		target := gitops.Target{ID: repository.ID, Path: selected, Spec: repository.Spec}
		if failure := gitops.NewMirror(runner, validation.Unavailable{}).ValidateTarget(ctx, target); failure != nil {
			return "", gitops.Outcome{}, false, failure
		}
		return selected, gitops.Outcome{}, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", gitops.Outcome{}, false, err
	}
	fmt.Fprintf(a.out, "Initializing stable mirror %s at %s...\n", repository.Spec.CloneURL, selected)
	outcome, failure := gitops.InitializeMirror(ctx, runner, gitops.Target{ID: repository.ID, Path: selected, Spec: repository.Spec})
	if failure != nil {
		return "", gitops.Outcome{}, false, failure
	}
	selected, err = canonicalMirrorRoot(selected)
	if err != nil {
		return "", gitops.Outcome{}, false, err
	}
	return selected, outcome, true, nil
}

func canonicalRepositoryRoot(path string) (string, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve repository path: %w", err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", fmt.Errorf("resolve repository path: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("repository path is not a canonical directory")
	}
	return filepath.Clean(canonical), nil
}

func canonicalConfiguredRepositoryRoot(repository configuredRepository, path string) (string, error) {
	if repository.Config.EffectiveMode() == config.ModeMirror {
		return canonicalMirrorRoot(path)
	}
	return canonicalRepositoryRoot(path)
}

// canonicalMirrorRoot preserves the stable lexical pointer. Resolving it to a
// generation would make registry/state identity change on every promotion.
func canonicalMirrorRoot(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil || !sameConfiguredPath(filepath.Clean(path), absolute) {
		return "", fmt.Errorf("mirror path is not absolute and canonical")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("resolve mirror parent: %w", err)
	}
	parent, err = filepath.Abs(parent)
	if err != nil || !sameConfiguredPath(parent, filepath.Dir(absolute)) {
		return "", fmt.Errorf("mirror path has a linked ancestor")
	}
	info, err := os.Lstat(absolute)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("mirror path is not a stable filesystem pointer")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve mirror pointer: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil || sameConfiguredPath(resolved, absolute) {
		return "", fmt.Errorf("mirror pointer does not resolve to an independent generation")
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil || !resolvedInfo.IsDir() {
		return "", fmt.Errorf("mirror pointer target is not a directory")
	}
	return absolute, nil
}

func sameConfiguredPath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func (a *Application) selectRepository(ctx context.Context, runner gitops.Runner, reader *bufio.Reader, spec discovery.RepositorySpec, matches []string) (string, error) {
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		defaultPath := filepath.Join(a.homeDir, spec.Name)
		fmt.Fprintf(a.out, "Could not find %s (%s).\n", spec.Key, spec.Branch)
		fmt.Fprintf(a.out, "Enter an existing repository path or a destination to clone [%s]: ", defaultPath)
		value, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		value = strings.TrimSpace(value)
		if value == "" {
			value = defaultPath
		}
		return a.selectExplicitRepository(ctx, runner, spec, value)
	default:
		fmt.Fprintf(a.out, "Found multiple clones of %s (%s):\n", spec.Key, spec.Branch)
		for index, match := range matches {
			fmt.Fprintf(a.out, "  %d. %s\n", index+1, match)
		}
		fmt.Fprint(a.out, "Select a repository: ")
		value, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		choice, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || choice < 1 || choice > len(matches) {
			return "", fmt.Errorf("invalid repository selection")
		}
		return matches[choice-1], nil
	}
}

func (a *Application) expandPath(value string) (string, error) {
	if value == "~" {
		value = a.homeDir
	} else if strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		value = filepath.Join(a.homeDir, value[2:])
	}
	return filepath.Abs(value)
}

func (a *Application) syncAll(ctx context.Context, printProgress bool, selector string) error {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	repositories, err := configuredRepositories(cfg, selector)
	if err != nil {
		return err
	}
	allRepositories, err := configuredRepositories(cfg, "")
	if err != nil {
		return err
	}
	var failures []error
	for _, repository := range repositories {
		if printProgress {
			fmt.Fprintf(a.out, "Syncing %s (%s, %s)...\n", repository.ID, repository.Config.EffectiveMode(), repository.Spec.Branch)
		}
		release, lockErr := a.acquireLock(repository.ID)
		if lockErr != nil {
			failure := &gitops.OperationError{Code: "REPO-OPERATION-LOCKED", Phase: "candidate", Summary: "repository is already being synchronized"}
			recordErr := a.recordOperationLockFailure(repository, failure)
			failures = append(failures, errors.Join(failure, recordErr))
			continue
		}
		machineState, loadErr := state.Load(a.statePath)
		if loadErr != nil {
			release()
			failures = append(failures, loadErr)
			continue
		}
		if pathErr := validateUniqueRepositoryPaths(allRepositories, machineState); pathErr != nil {
			release()
			failures = append(failures, pathErr)
			continue
		}
		repoState, ok := repositoryState(machineState, repository)
		if !ok || repoState.Path == "" {
			failure := &gitops.OperationError{Code: "REPO-SETUP-MISSING", Phase: "setup", Summary: "repository has no configured local path"}
			recordErr := a.recordFailure(repository, repoState, ok, gitops.Outcome{}, failure)
			release()
			failures = append(failures, errors.Join(failure, recordErr))
			continue
		}
		if repository.Config.SourceID == "" || repoState.ValidationRuntime.SourceID != repository.Config.SourceID {
			failure := &gitops.OperationError{Code: validation.CodeUnavailable, Phase: "validation", Summary: "repository validation is not reconciled"}
			recordErr := a.recordFailure(repository, repoState, true, gitops.Outcome{}, failure)
			release()
			failures = append(failures, errors.Join(failure, recordErr))
			continue
		}
		repoState.LastAttempt = a.now().UTC()
		repoState, updateErr := a.persistRepositoryState(repository, repoState, true)
		if updateErr != nil {
			release()
			failures = append(failures, fmt.Errorf("persist sync attempt for %s: %w", repository.ID, updateErr))
			continue
		}
		if runtimeErr := a.reconcileStoredRuntime(repoState, repository); runtimeErr != nil {
			code := "REPO-CONTEXT-RECONCILIATION"
			switch validation.ErrorCode(runtimeErr) {
			case validation.CodeMigrationNeeded:
				code = validation.CodeMigrationNeeded
			case validation.CodeUnsupported:
				code = validation.CodeUnsupported
			}
			failure := &gitops.OperationError{Code: code, Phase: "validation", Summary: "pinned context validation inputs are no longer reconciled"}
			recordErr := a.recordFailure(repository, repoState, true, gitops.Outcome{}, failure)
			release()
			failures = append(failures, errors.Join(failure, recordErr))
			continue
		}
		repoCtx, cancel := context.WithTimeout(ctx, repositoryOperationTimeout)
		var engine gitops.Engine
		if repository.Config.EffectiveMode() == config.ModeMirror {
			engine = a.mirror
		} else {
			engine = a.publisher
		}
		if engine == nil {
			runner, runnerErr := pinnedGitRunner(repoCtx, repoState)
			if runnerErr != nil {
				cancel()
				failure := &gitops.OperationError{Code: "REPO-GIT-DEPENDENCY", Phase: "validation", Summary: "trusted Git dependency is unavailable"}
				recordErr := a.recordFailure(repository, repoState, true, gitops.Outcome{}, failure)
				release()
				failures = append(failures, errors.Join(runnerErr, failure, recordErr))
				continue
			}
			if repository.Config.EffectiveMode() == config.ModeMirror {
				engine = gitops.NewMirror(runner, a.validator)
			} else {
				engine = gitops.NewPublisher(runner, a.validator)
			}
		}
		recoveredState, _, pendingErr := a.recoverAcceptedPendingCompletion(repository, repoState)
		if pendingErr != nil {
			cancel()
			release()
			failures = append(failures, fmt.Errorf("recover pending completion for %s: %w", repository.ID, pendingErr))
			continue
		}
		repoState = recoveredState
		newTarget := func(current state.Repository) gitops.Target {
			pinnedState := current
			pinnedRepository := repository
			return gitops.Target{
				ID:                     repository.ID,
				Path:                   current.Path,
				Spec:                   repository.Spec,
				ValidationRuntime:      validationRuntime(current.ValidationRuntime),
				ExpectedAcceptedCommit: current.AcceptedCommit,
				Reconcile: func(context.Context) error {
					return a.reconcilePinnedTarget(pinnedRepository, pinnedState)
				},
			}
		}
		target := newTarget(repoState)
		if mirror, ok := engine.(transactionalMirror); ok && repository.Config.EffectiveMode() == config.ModeMirror {
			recoveredState, recoveryErr := a.recoverMirrorTransaction(repoCtx, mirror, repository, repoState, target)
			if recoveryErr != nil {
				cancel()
				release()
				failures = append(failures, recoveryErr)
				continue
			}
			repoState = recoveredState
			target = newTarget(repoState)
			result, operationFailure := mirror.SyncTransaction(repoCtx, target)
			if operationFailure != nil {
				var recordErr error
				if result.TransactionID != "" {
					current, loadErr := a.currentRepositoryState(repository)
					if loadErr != nil {
						recordErr = loadErr
					} else {
						recordErr = a.rollbackMirrorTransaction(repoCtx, mirror, repository, current, target, result.Outcome, result.TransactionID, operationFailure, contextStatus{})
					}
				} else {
					recordErr = a.recordFailure(repository, repoState, true, result.Outcome, operationFailure)
				}
				cancel()
				release()
				failures = append(failures, errors.Join(operationFailure, recordErr))
				continue
			}
			if a.afterEngineSync != nil {
				a.afterEngineSync()
			}
			completionErr := a.completeMirrorTransaction(repoCtx, mirror, repository, repoState, target, result)
			cancel()
			if completionErr != nil {
				release()
				failures = append(failures, fmt.Errorf("complete mirror sync for %s: %w", repository.ID, completionErr))
				continue
			}
			release()
			if printProgress {
				fmt.Fprintln(a.out, "  up to date")
			}
			continue
		}
		outcome, operationFailure := engine.Sync(repoCtx, target)
		cancel()
		if operationFailure != nil {
			recordErr := a.recordFailure(repository, repoState, true, outcome, operationFailure)
			release()
			failures = append(failures, errors.Join(operationFailure, recordErr))
			continue
		}
		if a.afterEngineSync != nil {
			a.afterEngineSync()
		}
		if recordErr := a.recordSuccess(repository, repoState, outcome); recordErr != nil {
			release()
			failures = append(failures, fmt.Errorf("persist sync success for %s: %w", repository.ID, recordErr))
			continue
		}
		release()
		if printProgress {
			fmt.Fprintln(a.out, "  up to date")
		}
	}
	return errors.Join(failures...)
}

func repositoryState(machineState state.State, repository configuredRepository) (state.Repository, bool) {
	repoState, ok := machineState.Repositories[repository.ID]
	if !ok && repository.Config.IsLegacy() {
		repoState, ok = machineState.Repositories[repository.LegacyStateKey]
	}
	return repoState, ok
}

func validateUniqueRepositoryPaths(repositories []configuredRepository, machineState state.State) error {
	type resolvedPath struct {
		id   string
		path string
		info os.FileInfo
	}
	var resolved []resolvedPath
	for _, repository := range repositories {
		repoState, ok := repositoryState(machineState, repository)
		if !ok || repoState.Path == "" {
			continue
		}
		canonical, err := canonicalConfiguredRepositoryRoot(repository, repoState.Path)
		if err != nil || !sameConfiguredPath(canonical, filepath.Clean(repoState.Path)) {
			return fmt.Errorf("repository %s has a non-canonical local path", repository.ID)
		}
		info, err := os.Stat(canonical)
		if err != nil {
			return fmt.Errorf("repository %s local path is unavailable", repository.ID)
		}
		for _, existing := range resolved {
			if os.SameFile(existing.info, info) {
				return fmt.Errorf("repositories %s and %s cannot share local path %s", existing.id, repository.ID, canonical)
			}
		}
		resolved = append(resolved, resolvedPath{id: repository.ID, path: canonical, info: info})
	}
	return nil
}

func validationRuntime(runtime state.ValidationRuntime) validation.Runtime {
	return validation.Runtime{
		ContextctlPath:   runtime.ContextctlPath,
		ContextctlDigest: runtime.ContextctlDigest,
		RegistryPath:     runtime.RegistryPath,
		RegistryDigest:   runtime.RegistryDigest,
		TrustStatePath:   runtime.TrustStatePath,
		TrustStateDigest: runtime.TrustStateDigest,
		SourceID:         runtime.SourceID,
	}
}

// pinnedGitRunner reopens the registry after runtime reconciliation, verifies
// the exact digest captured by setup, and creates a runner for this one bounded
// operation. The runner itself re-verifies the pinned executable before every
// child process; Repo Sync never falls back to PATH.
func pinnedGitRunner(ctx context.Context, repositoryState state.Repository) (gitops.SystemRunner, error) {
	runtimeState := repositoryState.ValidationRuntime
	_, _, registryDigest, registryData, err := stableRuntimeFile(runtimeState.RegistryPath, repositoryState.Path, true, maxRuntimeJSONBytes)
	if err != nil || registryDigest != runtimeState.RegistryDigest {
		return gitops.SystemRunner{}, fmt.Errorf("registry identity changed")
	}
	runner, err := gitops.NewSystemRunnerFromRegistryContext(ctx, registryData)
	if err != nil {
		return gitops.SystemRunner{}, err
	}
	return runner, nil
}

func validateStoredRuntime(repositoryState state.Repository, repository configuredRepository) error {
	runtimeState := repositoryState.ValidationRuntime
	if runtimeState.Protocol == "contextctl.report.v1" {
		return &validation.ProtocolError{Code: validation.CodeMigrationNeeded}
	}
	if runtimeState.Protocol != validation.ReportProtocol || runtimeState.SourceID != repository.Config.SourceID {
		return fmt.Errorf("protocol or source identity does not match portable configuration")
	}
	_, contextctlInfo, contextctlDigest, _, err := stableRuntimeFile(runtimeState.ContextctlPath, repositoryState.Path, false, maxContextctlBytes)
	if err != nil || (runtime.GOOS != "windows" && contextctlInfo.Mode()&0o111 == 0) || contextctlDigest != runtimeState.ContextctlDigest {
		return fmt.Errorf("contextctl identity changed")
	}
	_, _, registryDigest, registryData, err := stableRuntimeFile(runtimeState.RegistryPath, repositoryState.Path, true, maxRuntimeJSONBytes)
	if err != nil || registryDigest != runtimeState.RegistryDigest {
		return fmt.Errorf("registry identity changed")
	}
	registryRevision, err := readRevision(registryData, "registry_revision")
	if err != nil || registryRevision != runtimeState.RegistryRevision || registryRevision < 1 {
		return fmt.Errorf("registry revision changed")
	}
	if _, err := readContextContractVersion(registryData); err != nil {
		return fmt.Errorf("registry contract version changed")
	}
	if err := reconcileValidationSource(registryData, repository, repositoryState.Path); err != nil {
		return err
	}
	_, _, trustDigest, trustData, err := stableRuntimeFile(runtimeState.TrustStatePath, repositoryState.Path, true, maxRuntimeJSONBytes)
	if err != nil || trustDigest != runtimeState.TrustStateDigest {
		return fmt.Errorf("trust-state identity changed")
	}
	trustRevision, err := readRevision(trustData, "state_revision")
	if err != nil || trustRevision != runtimeState.TrustStateRevision || trustRevision < 0 {
		return fmt.Errorf("trust-state revision changed")
	}
	if _, err := readContextContractVersion(trustData); err != nil {
		return fmt.Errorf("trust-state contract version changed")
	}
	return nil
}

func (a *Application) recordFailure(repository configuredRepository, repoState state.Repository, expectedPresent bool, outcome gitops.Outcome, failure *gitops.OperationError) error {
	now := a.now().UTC()
	repoState.ID = repository.ID
	repoState.Mode = string(repository.Config.EffectiveMode())
	repoState.LastAttempt = now
	repoState.LastError = ""
	if outcome.AcceptedCommit != "" {
		repoState.AcceptedCommit = outcome.AcceptedCommit
		repoState.AcceptedTree = outcome.AcceptedTree
	}
	repoState.CandidateCommit = outcome.CandidateCommit
	repoState.CandidateTree = outcome.CandidateTree
	if outcome.Validation.SchemaVersion != 0 {
		repoState.Validation = validationState(outcome.Validation, outcome.CandidateTree, now)
	}
	findings := make([]state.Finding, 0, len(failure.Findings))
	for _, finding := range failure.Findings {
		findings = append(findings, state.Finding{CheckID: finding.CheckID, Path: finding.Path})
	}
	repoState.Failure = &state.Failure{Code: failure.Code, Phase: failure.Phase, Summary: failure.Summary, OccurredAt: now, Findings: findings}
	fingerprint := failureFingerprint(repository.ID, failure, outcome.CandidateCommit, outcome.CandidateTree)
	shouldNotify := repoState.Notification.FailureKey != fingerprint ||
		(repoState.Notification.Status == "pending" && (repoState.Notification.AttemptedAt.IsZero() || now.Sub(repoState.Notification.AttemptedAt) >= notificationPendingRetry))
	notificationVersion := repoState.Notification.Version
	if shouldNotify {
		if notificationVersion == ^uint64(0) {
			return fmt.Errorf("notification version exhausted")
		}
		notificationVersion++
		repoState.Notification = state.Notification{FailureKey: fingerprint, Status: "pending", AttemptedAt: now, Version: notificationVersion}
	}
	persisted, err := a.persistRepositoryState(repository, repoState, expectedPresent)
	if err != nil {
		return err
	}
	repoState = persisted
	a.logf("sync failed repository=%s code=%s phase=%s", repository.ID, failure.Code, failure.Phase)
	var deliveryErr error
	if shouldNotify {
		notificationContext, cancel := context.WithTimeout(context.Background(), notificationTimeout)
		delivery := a.notifier.Notify(notificationContext, notify.Message{
			Title: "Repo Sync requires attention",
			Body:  fmt.Sprintf("Repository %s failed check %s.", repository.ID, failure.Code),
		})
		cancel()
		deliveryErr = a.completeNotificationCAS(repository, fingerprint, notificationVersion, delivery, a.now().UTC())
	}
	_, statusErr := a.refreshContextStatus("")
	return errors.Join(deliveryErr, statusErr)
}

// recordOperationLockFailure deliberately leaves the repository revision
// unchanged. The lock owner may already have pinned that generation; recording
// contention must not invalidate or overwrite its eventual authoritative
// completion. A later success clears this transient failure normally.
func (a *Application) recordOperationLockFailure(repository configuredRepository, failure *gitops.OperationError) error {
	now := a.now().UTC()
	var fingerprint string
	var notificationVersion uint64
	var shouldNotify bool
	err := state.Update(a.statePath, func(current *state.State) error {
		repoState, _ := repositoryState(*current, repository)
		repoState.ID = repository.ID
		repoState.Mode = string(repository.Config.EffectiveMode())
		repoState.LastAttempt = now
		repoState.LastError = ""
		repoState.Failure = &state.Failure{Code: failure.Code, Phase: failure.Phase, Summary: failure.Summary, OccurredAt: now}
		fingerprint = failureFingerprint(repository.ID, failure, repoState.CandidateCommit, repoState.CandidateTree)
		shouldNotify = repoState.Notification.FailureKey != fingerprint ||
			(repoState.Notification.Status == "pending" && (repoState.Notification.AttemptedAt.IsZero() || now.Sub(repoState.Notification.AttemptedAt) >= notificationPendingRetry))
		notificationVersion = repoState.Notification.Version
		if shouldNotify {
			if notificationVersion == ^uint64(0) {
				return fmt.Errorf("notification version exhausted")
			}
			notificationVersion++
			repoState.Notification = state.Notification{FailureKey: fingerprint, Status: "pending", AttemptedAt: now, Version: notificationVersion}
		}
		current.Repositories[repository.ID] = repoState
		if repository.Config.IsLegacy() && repository.LegacyStateKey != repository.ID {
			delete(current.Repositories, repository.LegacyStateKey)
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.logf("sync failed repository=%s code=%s phase=%s", repository.ID, failure.Code, failure.Phase)
	var deliveryErr error
	if shouldNotify {
		notificationContext, cancel := context.WithTimeout(context.Background(), notificationTimeout)
		delivery := a.notifier.Notify(notificationContext, notify.Message{
			Title: "Repo Sync requires attention",
			Body:  fmt.Sprintf("Repository %s failed check %s.", repository.ID, failure.Code),
		})
		cancel()
		deliveryErr = a.completeNotificationCAS(repository, fingerprint, notificationVersion, delivery, a.now().UTC())
	}
	_, statusErr := a.refreshContextStatus("")
	return errors.Join(deliveryErr, statusErr)
}

var errNotificationCASMiss = errors.New("notification state changed")

func (a *Application) completeNotificationCAS(repository configuredRepository, failureKey string, version uint64, delivery notify.Delivery, attemptedAt time.Time) error {
	err := state.Update(a.statePath, func(current *state.State) error {
		repoState, ok := repositoryState(*current, repository)
		if !ok || repoState.Notification.FailureKey != failureKey || repoState.Notification.Status != "pending" || repoState.Notification.Version != version {
			return errNotificationCASMiss
		}
		repoState.Notification.Status = string(delivery)
		repoState.Notification.AttemptedAt = attemptedAt
		current.Repositories[repository.ID] = repoState
		if repository.Config.IsLegacy() && repository.LegacyStateKey != repository.ID {
			delete(current.Repositories, repository.LegacyStateKey)
		}
		return nil
	})
	if errors.Is(err, errNotificationCASMiss) {
		return nil
	}
	if err != nil {
		return err
	}
	return nil
}

func (a *Application) recordSuccess(repository configuredRepository, repoState state.Repository, outcome gitops.Outcome) error {
	now := a.now().UTC()
	repoState.ID = repository.ID
	repoState.Mode = string(repository.Config.EffectiveMode())
	pending := state.PendingCompletion{
		AcceptedCommit: outcome.AcceptedCommit, AcceptedTree: outcome.AcceptedTree,
		CandidateCommit: outcome.CandidateCommit, CandidateTree: outcome.CandidateTree,
		Validation: validationState(outcome.Validation, outcome.CandidateTree, now), CompletedAt: now,
	}
	if repoState.PendingCompletion != nil {
		existing := *repoState.PendingCompletion
		pending.CompletedAt = existing.CompletedAt
		pending.Validation.CheckedAt = existing.Validation.CheckedAt
		if !reflect.DeepEqual(existing, pending) {
			return fmt.Errorf("repository %s has a conflicting pending completion", repository.ID)
		}
	} else {
		repoState.PendingCompletion = &pending
		persisted, err := a.persistRepositoryState(repository, repoState, true)
		if err != nil {
			return err
		}
		repoState = persisted
	}
	if a.afterPendingCompletion != nil {
		if err := a.afterPendingCompletion(); err != nil {
			return err
		}
	}
	completed := completedRepositoryState(repoState)
	completion, err := statusCompletion(repository.ID, completed)
	if err == nil {
		_, err = a.refreshContextStatusExpected("", &completion)
	}
	if err != nil {
		failure := &gitops.OperationError{Code: "REPO-CONTEXT-STATUS", Phase: "validation", Summary: "accepted update could not be verified in context status"}
		recordErr := a.recordPendingCompletionFailure(repository, repoState, failure)
		return errors.Join(err, recordErr)
	}
	if a.afterStatusBarrier != nil {
		if err := a.afterStatusBarrier(); err != nil {
			return &acceptedStatusCompletionError{err: err}
		}
	}
	if _, finalizeErr := a.persistRepositoryState(repository, completed, true); finalizeErr != nil {
		failure := &gitops.OperationError{Code: "REPO-STATE-FINALIZE", Phase: "validation", Summary: "accepted status was verified but private completion state could not be finalized"}
		recordErr := a.recordPendingCompletionFailure(repository, repoState, failure)
		return &acceptedStatusCompletionError{err: errors.Join(finalizeErr, recordErr)}
	}
	a.logf("sync completed repository=%s mode=%s", repository.ID, repository.Config.EffectiveMode())
	return nil
}

// acceptedStatusCompletionError means the shared status crossed its exact,
// reopened completion barrier even though private finalization did not finish.
// Mirror callers must preserve the promoted journal for restart repair rather
// than rolling back an already accepted candidate.
type acceptedStatusCompletionError struct {
	err error
}

func (e *acceptedStatusCompletionError) Error() string { return e.err.Error() }
func (e *acceptedStatusCompletionError) Unwrap() error { return e.err }

func completedRepositoryState(repoState state.Repository) state.Repository {
	if repoState.PendingCompletion == nil {
		return repoState
	}
	pending := *repoState.PendingCompletion
	repoState.LastSuccess = pending.CompletedAt
	repoState.LastSync = pending.CompletedAt
	repoState.LastError = ""
	repoState.AcceptedCommit = pending.AcceptedCommit
	repoState.AcceptedTree = pending.AcceptedTree
	repoState.CandidateCommit = pending.CandidateCommit
	repoState.CandidateTree = pending.CandidateTree
	repoState.Validation = pending.Validation
	repoState.PendingCompletion = nil
	repoState.Failure = nil
	repoState.Notification = state.Notification{}
	return repoState
}

func (a *Application) recoverAcceptedPendingCompletion(repository configuredRepository, repoState state.Repository) (state.Repository, bool, error) {
	if repoState.PendingCompletion == nil {
		return repoState, false, nil
	}
	completed := completedRepositoryState(repoState)
	expected, err := statusCompletion(repository.ID, completed)
	if err != nil {
		return repoState, false, err
	}
	status, err := a.readContextStatusForRecovery()
	if err != nil {
		return repoState, false, nil
	}
	item, ok := exactStatusRepository(status, repository.ID)
	if !ok || item.Error != nil || verifyStatusCompletion(status, expected) != nil {
		return repoState, false, nil
	}
	persisted, err := a.persistRepositoryState(repository, completed, true)
	if err != nil {
		return repoState, false, err
	}
	a.logf("recovered accepted pending completion repository=%s", repository.ID)
	return persisted, true, nil
}

func (a *Application) recordPendingCompletionFailure(repository configuredRepository, repoState state.Repository, failure *gitops.OperationError) error {
	if repoState.PendingCompletion == nil {
		return fmt.Errorf("pending completion disappeared while recording %s", failure.Code)
	}
	now := a.now().UTC()
	pending := repoState.PendingCompletion
	repoState.LastAttempt = now
	repoState.Failure = &state.Failure{Code: failure.Code, Phase: failure.Phase, Summary: failure.Summary, OccurredAt: now}
	fingerprint := failureFingerprint(repository.ID, failure, pending.CandidateCommit, pending.CandidateTree)
	shouldNotify := repoState.Notification.FailureKey != fingerprint ||
		(repoState.Notification.Status == "pending" && (repoState.Notification.AttemptedAt.IsZero() || now.Sub(repoState.Notification.AttemptedAt) >= notificationPendingRetry))
	version := repoState.Notification.Version
	if shouldNotify {
		if version == ^uint64(0) {
			return fmt.Errorf("notification version exhausted")
		}
		version++
		repoState.Notification = state.Notification{FailureKey: fingerprint, Status: "pending", AttemptedAt: now, Version: version}
	}
	if _, err := a.persistRepositoryState(repository, repoState, true); err != nil {
		return err
	}
	a.logf("pending completion failed repository=%s code=%s", repository.ID, failure.Code)
	if !shouldNotify {
		return nil
	}
	notificationContext, cancel := context.WithTimeout(context.Background(), notificationTimeout)
	delivery := a.notifier.Notify(notificationContext, notify.Message{Title: "Repo Sync requires attention", Body: fmt.Sprintf("Repository %s failed check %s.", repository.ID, failure.Code)})
	cancel()
	return a.completeNotificationCAS(repository, fingerprint, version, delivery, a.now().UTC())
}

func statusCompletion(repositoryID string, repoState state.Repository) (statusCompletionExpectation, error) {
	lastValidation, err := statusValidation(repositoryID, repoState.Validation)
	if err != nil {
		return statusCompletionExpectation{}, err
	}
	if lastValidation == nil {
		return statusCompletionExpectation{}, fmt.Errorf("repository %s has no attempted validation", repositoryID)
	}
	return statusCompletionExpectation{
		RepositoryID:    repositoryID,
		AcceptedCommit:  repoState.AcceptedCommit,
		AcceptedTree:    repoState.AcceptedTree,
		CandidateCommit: repoState.CandidateCommit,
		ValidationConfig: contextValidationConfig{
			Protocol:               repoState.ValidationRuntime.Protocol,
			SourceID:               repoState.ValidationRuntime.SourceID,
			ContextctlBinaryDigest: repoState.ValidationRuntime.ContextctlDigest,
			RegistryRevision:       repoState.ValidationRuntime.RegistryRevision,
			RegistryDigest:         repoState.ValidationRuntime.RegistryDigest,
			TrustStateRevision:     repoState.ValidationRuntime.TrustStateRevision,
			TrustStateDigest:       repoState.ValidationRuntime.TrustStateDigest,
		},
		LastValidation: *lastValidation,
	}, nil
}

func validationState(report validation.Report, tree string, checkedAt time.Time) state.Validation {
	checkIDs := make([]string, 0, len(report.Checks))
	seen := make(map[string]bool)
	for _, check := range report.Checks {
		if !seen[check.ID] {
			checkIDs = append(checkIDs, check.ID)
			seen[check.ID] = true
		}
	}
	sort.Strings(checkIDs)
	results := make([]state.ValidatorResult, 0, len(report.ValidatorResults))
	for _, result := range report.ValidatorResults {
		results = append(results, state.ValidatorResult{
			ContractID:            result.ContractID,
			ContractVersion:       result.ContractVersion,
			Disposition:           result.Disposition,
			CheckIDs:              append([]string(nil), result.CheckIDs...),
			SourceID:              result.SourceID,
			SourceRole:            result.SourceRole,
			AcceptedBundleDigest:  result.AcceptedBundleDigest,
			CandidateBundleDigest: result.CandidateBundleDigest,
		})
	}
	return state.Validation{ContractVersion: report.ContractVersion, ObjectID: tree, Verdict: string(report.Verdict), CheckIDs: checkIDs, ValidatorResults: results, CheckedAt: checkedAt}
}

func failureFingerprint(repositoryID string, failure *gitops.OperationError, commit, tree string) string {
	parts := []string{repositoryID, failure.Code, failure.Phase, commit, tree}
	for _, finding := range failure.Findings {
		parts = append(parts, finding.CheckID, finding.Path)
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:])
}

var errRepositoryStateCAS = errors.New("repository state changed during operation")

func (a *Application) persistRepositoryState(repository configuredRepository, repoState state.Repository, expectedPresent bool) (state.Repository, error) {
	if err := a.reconcilePortableRepository(repository); err != nil {
		return state.Repository{}, err
	}
	if repoState.Revision == ^uint64(0) {
		return state.Repository{}, fmt.Errorf("repository state revision exhausted")
	}
	expectedRevision := repoState.Revision
	repoState.Revision++
	err := state.Update(a.statePath, func(current *state.State) error {
		existing, exists := repositoryState(*current, repository)
		if exists != expectedPresent || (exists && existing.Revision != expectedRevision) {
			return errRepositoryStateCAS
		}
		current.Repositories[repository.ID] = repoState
		if repository.Config.IsLegacy() && repository.LegacyStateKey != repository.ID {
			delete(current.Repositories, repository.LegacyStateKey)
		}
		return nil
	})
	if err != nil {
		return state.Repository{}, err
	}
	if err := a.reconcilePortableRepository(repository); err != nil {
		return state.Repository{}, err
	}
	return repoState, nil
}

func (a *Application) acquireLock(key string) (func(), error) {
	return a.acquireLockWithTimeout(key, 0)
}

func (a *Application) acquireLockWithTimeout(key string, timeout time.Duration) (func(), error) {
	if err := os.MkdirAll(a.lockDir, 0o755); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%x.lock", sha256.Sum256([]byte(key)))
	path := filepath.Join(a.lockDir, name)
	file, release, err := lockRepositoryOperationFile(path, timeout)
	if err != nil {
		return nil, fmt.Errorf("repository is already being synchronized")
	}
	failed := true
	defer func() {
		if failed {
			release()
		}
	}()
	if err := file.Truncate(0); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	if _, err := fmt.Fprintf(file, "%d %s\n", os.Getpid(), now().UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	failed = false
	return release, nil
}

type statusOptions struct {
	RepositoryID string
	JSON         bool
}

type contextStatus struct {
	SchemaVersion int                 `json:"schema_version"`
	Protocol      string              `json:"protocol"`
	GeneratedAt   string              `json:"generated_at"`
	Service       contextService      `json:"service"`
	Repositories  []contextRepository `json:"repositories"`
}

type contextService struct {
	State           string `json:"state"`
	Enabled         bool   `json:"enabled"`
	IntervalSeconds int64  `json:"interval_seconds,omitempty"`
}

type contextRepository struct {
	RepositoryID     string                  `json:"repository_id"`
	Mode             string                  `json:"mode"`
	Branch           string                  `json:"branch"`
	LastAttemptAt    string                  `json:"last_attempt_at,omitempty"`
	LastSuccessAt    string                  `json:"last_success_at,omitempty"`
	AcceptedCommit   string                  `json:"accepted_commit,omitempty"`
	CandidateCommit  string                  `json:"candidate_commit,omitempty"`
	ValidationConfig contextValidationConfig `json:"validation_config"`
	LastValidation   *contextValidation      `json:"last_validation,omitempty"`
	Error            *contextError           `json:"error,omitempty"`
}

type contextValidationConfig struct {
	Protocol               string `json:"protocol"`
	SourceID               string `json:"source_id"`
	ContextctlBinaryDigest string `json:"contextctl_binary_digest"`
	RegistryRevision       int    `json:"registry_revision"`
	RegistryDigest         string `json:"registry_digest"`
	TrustStateRevision     int    `json:"trust_state_revision"`
	TrustStateDigest       string `json:"trust_state_digest"`
}

type contextValidation struct {
	ContractVersion  string                   `json:"contract_version"`
	TreeObjectID     string                   `json:"tree_object_id"`
	Verdict          string                   `json:"verdict"`
	CheckIDs         []string                 `json:"check_ids"`
	ValidatorResults []contextValidatorResult `json:"validator_results"`
}

type contextValidatorResult struct {
	ContractID            string   `json:"contract_id"`
	ContractVersion       string   `json:"contract_version"`
	Disposition           string   `json:"disposition"`
	CheckIDs              []string `json:"check_ids"`
	AcceptedBundleDigest  string   `json:"accepted_bundle_digest,omitempty"`
	CandidateBundleDigest string   `json:"candidate_bundle_digest,omitempty"`
}

type contextError struct {
	Code     string   `json:"code"`
	Phase    string   `json:"phase"`
	CheckIDs []string `json:"check_ids"`
	Paths    []string `json:"paths"`
}

type unreconciledStatusError struct {
	RepositoryID string
}

type pendingCompletionError struct {
	RepositoryID string
}

func (e *pendingCompletionError) Error() string {
	return fmt.Sprintf("repository %s has a pending accepted-status completion", e.RepositoryID)
}

func (e *unreconciledStatusError) Error() string {
	return fmt.Sprintf("repository %s has not been reconciled for context status", e.RepositoryID)
}

func (a *Application) status(options statusOptions) error {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	machineState, err := state.Load(a.statePath)
	if err != nil {
		return err
	}
	repositories, err := configuredRepositories(cfg, options.RepositoryID)
	if err != nil {
		return err
	}
	serviceState := a.serviceState()
	if options.JSON {
		fullReport, err := a.refreshContextStatus("")
		if err != nil {
			return err
		}
		report := fullReport
		if options.RepositoryID != "" {
			report.Repositories = make([]contextRepository, 0, 1)
			for _, repository := range fullReport.Repositories {
				if repository.RepositoryID == options.RepositoryID {
					report.Repositories = append(report.Repositories, repository)
				}
			}
		}
		encoder := json.NewEncoder(a.out)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(report)
	}

	fmt.Fprintf(a.out, "Service: %s\nEnabled: %t\nInterval: %s\nConfig: %s\n", serviceState, machineState.Enabled, cfg.Interval, a.configPath)
	for _, repository := range repositories {
		repoState, ok := machineState.Repositories[repository.ID]
		if !ok && repository.Config.IsLegacy() {
			repoState, ok = machineState.Repositories[repository.LegacyStateKey]
		}
		fmt.Fprintf(a.out, "- %s (%s, %s)\n  path: %s\n", repository.ID, repository.Config.EffectiveMode(), repository.Spec.Branch, valueOr(repoState.Path, "not configured"))
		if repoState.Failure != nil {
			fmt.Fprintf(a.out, "  error: %s (%s)\n", repoState.Failure.Code, repoState.Failure.Summary)
		} else if repoState.LastError != "" {
			fmt.Fprintln(a.out, "  error: legacy failure details suppressed")
		} else if !repoState.LastSuccess.IsZero() {
			fmt.Fprintf(a.out, "  last success: %s\n", repoState.LastSuccess.Local().Format(time.RFC3339))
		} else if !repoState.LastSync.IsZero() {
			fmt.Fprintf(a.out, "  last success: %s\n", repoState.LastSync.Local().Format(time.RFC3339))
		} else {
			fmt.Fprintln(a.out, "  last success: never")
		}
	}
	return nil
}

func (a *Application) refreshContextStatus(serviceOverride string) (contextStatus, error) {
	return a.refreshContextStatusExpected(serviceOverride, nil)
}

func (a *Application) refreshContextStatusExpected(serviceOverride string, completion *statusCompletionExpectation) (contextStatus, error) {
	release, err := a.acquireContextStatusLock()
	if err != nil {
		return contextStatus{}, err
	}
	defer release()

	cfg, err := config.Load(a.configPath)
	if err != nil {
		return contextStatus{}, err
	}
	machineState, err := state.Load(a.statePath)
	if err != nil {
		return contextStatus{}, err
	}
	repositories, err := configuredRepositories(cfg, "")
	if err != nil {
		return contextStatus{}, err
	}
	if err := a.reconcileRuntimeSnapshot(repositories, machineState); err != nil {
		return contextStatus{}, err
	}
	serviceState := serviceOverride
	if serviceState == "" {
		serviceState = a.serviceState()
	}
	generatedAt := time.Now().UTC()
	if a.now != nil {
		generatedAt = a.now().UTC()
	}
	statusPath := filepath.Join(a.configDir, "context-status.json")
	if previous, readErr := os.ReadFile(statusPath); readErr == nil {
		if len(previous) > maxContextStatusBytes {
			return contextStatus{}, fmt.Errorf("existing context status exceeds the protocol limit")
		}
		var identity struct {
			SchemaVersion int    `json:"schema_version"`
			Protocol      string `json:"protocol"`
			GeneratedAt   string `json:"generated_at"`
		}
		if decodeErr := strictjson.Decode(previous, &identity, false); decodeErr == nil && identity.SchemaVersion == contextStatusSchema && identity.Protocol == contextStatusProtocol {
			previousTime, timeErr := time.Parse(time.RFC3339, identity.GeneratedAt)
			if timeErr == nil && previousTime.After(generatedAt) {
				return contextStatus{}, fmt.Errorf("context status clock would move backwards")
			}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return contextStatus{}, readErr
	}
	report, err := buildContextStatus(cfg, machineState, repositories, serviceState, generatedAt, completion)
	if err != nil {
		return contextStatus{}, err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return contextStatus{}, fmt.Errorf("encode context status: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxContextStatusBytes {
		return contextStatus{}, fmt.Errorf("context status exceeds the protocol limit")
	}
	writeStatus := atomicfile.Write
	if a.writeContextStatus != nil {
		writeStatus = a.writeContextStatus
	}
	if err := writeStatus(statusPath, data, 0o600); err != nil {
		return contextStatus{}, fmt.Errorf("write context status: %w", err)
	}
	if err := verifyPersistedContextStatus(statusPath, data, report, completion); err != nil {
		return contextStatus{}, fmt.Errorf("verify context status: %w", err)
	}
	return report, nil
}

func (a *Application) acquireContextStatusLock() (func(), error) {
	if err := os.MkdirAll(a.lockDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(a.lockDir, "context-status.lock")
	file, release, err := lockContextStatusFile(path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if err := file.Truncate(0); err != nil {
		release()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		release()
		return nil, err
	}
	if _, err := fmt.Fprintf(file, "%d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		release()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func buildContextStatus(cfg config.Config, machineState state.State, repositories []configuredRepository, serviceState string, generatedAt time.Time, completion *statusCompletionExpectation) (contextStatus, error) {
	interval, err := cfg.Duration()
	if err != nil {
		return contextStatus{}, err
	}
	if serviceState != "running" && serviceState != "stopped" && serviceState != "unavailable" && serviceState != "unknown" {
		return contextStatus{}, fmt.Errorf("invalid service state")
	}
	report := contextStatus{
		SchemaVersion: contextStatusSchema,
		Protocol:      contextStatusProtocol,
		GeneratedAt:   generatedAt.UTC().Format(time.RFC3339Nano),
		Service:       contextService{State: serviceState, Enabled: machineState.Enabled, IntervalSeconds: int64(interval.Seconds())},
		Repositories:  make([]contextRepository, 0, len(repositories)),
	}
	for _, repository := range repositories {
		repoState, ok := machineState.Repositories[repository.ID]
		if !ok && repository.Config.IsLegacy() {
			repoState, ok = machineState.Repositories[repository.LegacyStateKey]
		}
		if !ok || repository.Config.SourceID == "" || repoState.ValidationRuntime.SourceID == "" {
			return contextStatus{}, &unreconciledStatusError{RepositoryID: repository.ID}
		}
		if err := validateStatusRuntime(repository, repoState); err != nil {
			return contextStatus{}, err
		}
		if repoState.PendingCompletion != nil {
			if completion == nil || completion.RepositoryID != repository.ID {
				return contextStatus{}, &pendingCompletionError{RepositoryID: repository.ID}
			}
			repoState = completedRepositoryState(repoState)
			expected, expectationErr := statusCompletion(repository.ID, repoState)
			if expectationErr != nil || !reflect.DeepEqual(expected, *completion) {
				return contextStatus{}, fmt.Errorf("repository %s pending completion does not match the authorized status update", repository.ID)
			}
		}
		item := contextRepository{
			RepositoryID:    repository.ID,
			Mode:            string(repository.Config.EffectiveMode()),
			Branch:          repository.Spec.Branch,
			AcceptedCommit:  repoState.AcceptedCommit,
			CandidateCommit: repoState.CandidateCommit,
			ValidationConfig: contextValidationConfig{
				Protocol:               repoState.ValidationRuntime.Protocol,
				SourceID:               repoState.ValidationRuntime.SourceID,
				ContextctlBinaryDigest: repoState.ValidationRuntime.ContextctlDigest,
				RegistryRevision:       repoState.ValidationRuntime.RegistryRevision,
				RegistryDigest:         repoState.ValidationRuntime.RegistryDigest,
				TrustStateRevision:     repoState.ValidationRuntime.TrustStateRevision,
				TrustStateDigest:       repoState.ValidationRuntime.TrustStateDigest,
			},
		}
		if !repoState.LastAttempt.IsZero() {
			item.LastAttemptAt = repoState.LastAttempt.UTC().Format(time.RFC3339Nano)
		}
		lastSuccess := repoState.LastSuccess
		if lastSuccess.IsZero() {
			lastSuccess = repoState.LastSync
		}
		if !lastSuccess.IsZero() {
			item.LastSuccessAt = lastSuccess.UTC().Format(time.RFC3339Nano)
		}
		if repoState.Validation.ContractVersion != "" {
			validationStatus, validationErr := statusValidation(repository.ID, repoState.Validation)
			if validationErr != nil {
				return contextStatus{}, validationErr
			}
			item.LastValidation = validationStatus
		}
		if repoState.Failure != nil {
			errorStatus, failureErr := statusFailure(repository.ID, *repoState.Failure)
			if failureErr != nil {
				return contextStatus{}, failureErr
			}
			item.Error = errorStatus
		} else if repoState.LastError != "" {
			item.Error = &contextError{Code: "REPO-LEGACY-ERROR", Phase: "publish", CheckIDs: []string{"REPO-LEGACY-ERROR"}, Paths: []string{}}
		}
		report.Repositories = append(report.Repositories, item)
	}
	return report, nil
}

func validateStatusRuntime(repository configuredRepository, repoState state.Repository) error {
	runtimeState := repoState.ValidationRuntime
	if !statusRepositoryIDPattern.MatchString(repository.ID) || runtimeState.Protocol != validation.ReportProtocol || runtimeState.SourceID != repository.Config.SourceID || len(runtimeState.SourceID) > 160 || !statusDottedIDPattern.MatchString(runtimeState.SourceID) || runtimeState.RegistryRevision < 1 || runtimeState.TrustStateRevision < 0 {
		return fmt.Errorf("repository %s has invalid context status configuration", repository.ID)
	}
	for _, digest := range []string{runtimeState.ContextctlDigest, runtimeState.RegistryDigest, runtimeState.TrustStateDigest} {
		if !statusDigestPattern.MatchString(digest) {
			return fmt.Errorf("repository %s has invalid context status digest", repository.ID)
		}
	}
	for _, objectID := range []string{repoState.AcceptedCommit, repoState.CandidateCommit} {
		if objectID != "" && !statusObjectIDPattern.MatchString(objectID) {
			return fmt.Errorf("repository %s has invalid context status commit", repository.ID)
		}
	}
	return nil
}

func statusValidation(repositoryID string, persisted state.Validation) (*contextValidation, error) {
	if !supportedContextContract(persisted.ContractVersion) || !statusObjectIDPattern.MatchString(persisted.ObjectID) || !validStatusVerdict(persisted.Verdict) || !validStatusCheckIDs(persisted.CheckIDs, 1, 128) || len(persisted.ValidatorResults) > 64 {
		return nil, fmt.Errorf("repository %s has invalid persisted validation details", repositoryID)
	}
	checkSet := make(map[string]bool, len(persisted.CheckIDs))
	for _, id := range persisted.CheckIDs {
		checkSet[id] = true
	}
	results := make([]contextValidatorResult, 0, len(persisted.ValidatorResults))
	for _, result := range persisted.ValidatorResults {
		if len(result.ContractID) > 160 || !statusDottedIDPattern.MatchString(result.ContractID) || !statusSemverPattern.MatchString(result.ContractVersion) || !validStatusDisposition(result.Disposition) || !validStatusCheckIDs(result.CheckIDs, 1, 128) {
			return nil, fmt.Errorf("repository %s has invalid persisted validator details", repositoryID)
		}
		for _, id := range result.CheckIDs {
			if !checkSet[id] {
				return nil, fmt.Errorf("repository %s has inconsistent persisted validator details", repositoryID)
			}
		}
		for _, digest := range []string{result.AcceptedBundleDigest, result.CandidateBundleDigest} {
			if digest != "" && !statusDigestPattern.MatchString(digest) {
				return nil, fmt.Errorf("repository %s has invalid persisted validator digest", repositoryID)
			}
		}
		results = append(results, contextValidatorResult{
			ContractID:            result.ContractID,
			ContractVersion:       result.ContractVersion,
			Disposition:           result.Disposition,
			CheckIDs:              append([]string{}, result.CheckIDs...),
			AcceptedBundleDigest:  result.AcceptedBundleDigest,
			CandidateBundleDigest: result.CandidateBundleDigest,
		})
	}
	if persisted.Verdict == "pass" {
		if len(results) == 0 {
			return nil, fmt.Errorf("repository %s has a passing validation without an applicable validator", repositoryID)
		}
		for _, result := range results {
			if result.Disposition != "pass" {
				return nil, fmt.Errorf("repository %s has an inconsistent passing validation", repositoryID)
			}
		}
	}
	return &contextValidation{
		ContractVersion:  persisted.ContractVersion,
		TreeObjectID:     persisted.ObjectID,
		Verdict:          persisted.Verdict,
		CheckIDs:         append([]string{}, persisted.CheckIDs...),
		ValidatorResults: results,
	}, nil
}

func statusFailure(repositoryID string, failure state.Failure) (*contextError, error) {
	phase := failure.Phase
	// The frozen shared schema predates the private mirror recovery/cleanup
	// phases. Those are promotion failures from a consumer's perspective.
	if phase == "recovery" || phase == "cleanup" {
		phase = "promotion"
	}
	if !validStatusCheckID(failure.Code) || !validStatusPhase(phase) {
		return nil, fmt.Errorf("repository %s has invalid persisted failure details", repositoryID)
	}
	checkIDs := []string{failure.Code}
	paths := make([]string, 0, len(failure.Findings))
	for _, finding := range failure.Findings {
		if !validStatusCheckID(finding.CheckID) || (finding.Path != "" && !safeStatusPath(finding.Path)) {
			return nil, fmt.Errorf("repository %s has invalid persisted failure finding", repositoryID)
		}
		checkIDs = append(checkIDs, finding.CheckID)
		if finding.Path != "" {
			paths = append(paths, finding.Path)
		}
	}
	return &contextError{
		Code:     failure.Code,
		Phase:    phase,
		CheckIDs: boundedValues(uniqueSorted(checkIDs), 128, failure.Code),
		Paths:    boundedValues(uniqueSorted(paths), 128, ""),
	}, nil
}

func validStatusCheckIDs(values []string, minimum, maximum int) bool {
	if len(values) < minimum || len(values) > maximum {
		return false
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] || !validStatusCheckID(value) {
			return false
		}
		seen[value] = true
	}
	return true
}

func validStatusCheckID(value string) bool {
	return len(value) <= 100 && statusCheckIDPattern.MatchString(value)
}

func validStatusVerdict(value string) bool {
	return value == "pass" || value == "fail" || value == "hold" || value == "incomplete"
}

func validStatusDisposition(value string) bool {
	return value == "pass" || value == "fail" || value == "hold" || value == "skip"
}

func validStatusPhase(value string) bool {
	return value == "setup" || value == "fetch" || value == "candidate" || value == "validation" || value == "promotion" || value == "publish" || value == "notification"
}

func safeStatusPath(value string) bool {
	if value == "" || len(value) > 512 || strings.ContainsAny(value, "\x00\r\n\\") || strings.HasPrefix(value, "/") || filepath.IsAbs(value) {
		return false
	}
	if len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' && (value[2] == '/' || value[2] == '\\') {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == ".." {
			return false
		}
	}
	return true
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func boundedValues(values []string, maximum int, required string) []string {
	if len(values) <= maximum {
		return values
	}
	values = append([]string(nil), values[:maximum]...)
	if required != "" {
		found := false
		for _, value := range values {
			found = found || value == required
		}
		if !found {
			values[len(values)-1] = required
			sort.Strings(values)
		}
	}
	return values
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (a *Application) serviceState() string {
	service, err := a.newService()
	if err != nil {
		return "unknown"
	}
	status, statusErr := service.Status()
	if errors.Is(statusErr, background.ErrNotInstalled) {
		return "unavailable"
	}
	if statusErr != nil {
		return "unknown"
	}
	if status == background.StatusRunning {
		return "running"
	}
	if status == background.StatusStopped {
		return "stopped"
	}
	return "unknown"
}

func (a *Application) start() error {
	cfg, err := config.Ensure(a.configPath)
	if err != nil {
		return err
	}
	if len(cfg.Repositories) == 0 {
		return fmt.Errorf("no repositories configured; run repo-sync config add first")
	}
	repositories, err := configuredRepositories(cfg, "")
	if err != nil {
		return err
	}
	machineState, err := state.Load(a.statePath)
	if err != nil {
		return err
	}
	needsSetup := false
	for _, repository := range repositories {
		entry, ok := machineState.Repositories[repository.ID]
		if !ok && repository.Config.IsLegacy() {
			entry, ok = machineState.Repositories[repository.LegacyStateKey]
		}
		if !ok || entry.Path == "" {
			needsSetup = true
			break
		}
	}
	if needsSetup {
		return fmt.Errorf("repository setup is incomplete; run repo-sync setup --registry <absolute-path>")
	}
	for _, repository := range repositories {
		if repository.Config.SourceID == "" {
			return fmt.Errorf("repository %s is a legacy entry; migrate it to id/mode/source before starting", repository.ID)
		}
	}
	if err := validateUniqueRepositoryPaths(repositories, machineState); err != nil {
		return err
	}
	for _, repository := range repositories {
		entry, ok := machineState.Repositories[repository.ID]
		if !ok && repository.Config.IsLegacy() {
			entry, ok = machineState.Repositories[repository.LegacyStateKey]
		}
		if !ok || entry.Path == "" {
			return fmt.Errorf("repository %s has not completed setup", repository.ID)
		}
	}
	if err := a.preflightRepositories(context.Background(), repositories, machineState); err != nil {
		return err
	}
	if err := a.enableService(); err != nil {
		return err
	}
	if !hasPendingCompletions(repositories, machineState) {
		if _, err := a.refreshContextStatus(""); err != nil {
			return fmt.Errorf("refresh context status: %w", err)
		}
	}
	fmt.Fprintln(a.out, "Repo Sync is enabled and running.")
	return nil
}

func (a *Application) enableService() error {
	service, err := a.newService()
	if err != nil {
		return a.startFailure(err)
	}
	status, statusErr := service.Status()
	if errors.Is(statusErr, background.ErrNotInstalled) {
		if err := service.Install(); err != nil {
			return a.startFailure(fmt.Errorf("install service: %w", err))
		}
		status = background.StatusStopped
	} else if statusErr != nil {
		return a.startFailure(fmt.Errorf("inspect service: %w", statusErr))
	}
	if err := state.Update(a.statePath, func(current *state.State) error {
		current.Enabled = true
		return nil
	}); err != nil {
		return err
	}
	if status != background.StatusRunning {
		if err := service.Start(); err != nil {
			return a.startFailure(fmt.Errorf("start service: %w", err))
		}
	}
	return nil
}

func (a *Application) startFailure(cause error) error {
	if err := state.Update(a.statePath, func(current *state.State) error {
		current.Enabled = false
		return nil
	}); err != nil {
		return errors.Join(cause, fmt.Errorf("disable synchronization after start failure: %w", err))
	}
	return cause
}

func (a *Application) stop() error {
	if err := state.Update(a.statePath, func(current *state.State) error {
		current.Enabled = false
		return nil
	}); err != nil {
		return err
	}
	service, err := a.newService()
	if err != nil {
		return err
	}
	status, statusErr := service.Status()
	if errors.Is(statusErr, background.ErrNotInstalled) {
		if _, err := a.refreshContextStatus("unavailable"); err != nil {
			return fmt.Errorf("refresh context status: %w", err)
		}
		fmt.Fprintln(a.out, "Repo Sync is disabled; no service is installed.")
		return nil
	}
	if statusErr != nil {
		return statusErr
	}
	if status == background.StatusRunning {
		if err := service.Stop(); err != nil {
			return fmt.Errorf("stop service: %w", err)
		}
	}
	if _, err := a.refreshContextStatus("stopped"); err != nil {
		return fmt.Errorf("refresh context status: %w", err)
	}
	fmt.Fprintln(a.out, "Repo Sync is stopped and disabled until repo-sync start is run.")
	return nil
}

func (a *Application) uninstall() error {
	if err := a.stop(); err != nil {
		return err
	}
	service, err := a.newService()
	if err != nil {
		return err
	}
	if err := service.Uninstall(); err != nil && !errors.Is(err, background.ErrNotInstalled) {
		return fmt.Errorf("uninstall service: %w", err)
	}
	if _, err := a.refreshContextStatus("unavailable"); err != nil {
		return fmt.Errorf("refresh context status: %w", err)
	}
	fmt.Fprintf(a.out, "Service removed. Configuration preserved at %s\n", a.configPath)
	return nil
}

func (a *Application) newService() (background.Controller, error) {
	if a.serviceFactory != nil {
		return a.serviceFactory()
	}
	program := &serviceProgram{app: a}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate repo-sync executable: %w", err)
	}
	return background.New(program, background.Config{
		Name:         "repo-sync",
		DisplayName:  "Repo Sync",
		Description:  "Synchronize configured GitHub repository branches.",
		Executable:   executable,
		Arguments:    []string{"run"},
		LogDirectory: a.configDir,
	})
}

func (a *Application) runService() error {
	service, err := a.newService()
	if err != nil {
		return err
	}
	return service.Run()
}

func (a *Application) runLoop(ctx context.Context) {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		a.logf("service cannot start code=REPO-CONFIG-INVALID")
		return
	}
	interval, err := cfg.Duration()
	if err != nil {
		a.logf("service cannot start code=REPO-INTERVAL-INVALID")
		return
	}
	machineState, err := state.Load(a.statePath)
	if err != nil || !machineState.Enabled {
		return
	}
	repositories, err := configuredRepositories(cfg, "")
	if err != nil || a.preflightRepositories(ctx, repositories, machineState) != nil {
		a.logf("service cannot start code=REPO-CONTEXT-RECONCILIATION")
		return
	}
	if !hasPendingCompletions(repositories, machineState) {
		if _, err := a.refreshContextStatus("running"); err != nil {
			a.logf("service cannot publish context status")
			return
		}
	} else {
		a.logf("service preserving accepted status while pending completion recovers")
	}
	heartbeatContext, stopHeartbeat := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(contextStatusHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatContext.Done():
				return
			case <-ticker.C:
				if _, heartbeatErr := a.refreshContextStatus("running"); heartbeatErr != nil {
					a.logf("context status heartbeat failed")
				}
			}
		}
	}()
	defer func() {
		stopHeartbeat()
		<-heartbeatDone
		if _, statusErr := a.refreshContextStatus("stopped"); statusErr != nil {
			a.logf("context status stop update failed")
		}
	}()
	if syncErr := a.syncAll(ctx, false, ""); syncErr != nil {
		a.logf("service sync cycle failed")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			machineState, err := state.Load(a.statePath)
			if err != nil || !machineState.Enabled {
				return
			}
			if syncErr := a.syncAll(ctx, false, ""); syncErr != nil {
				a.logf("service sync cycle failed")
			}
		}
	}
}

func hasPendingCompletions(repositories []configuredRepository, machineState state.State) bool {
	for _, repository := range repositories {
		if repoState, ok := repositoryState(machineState, repository); ok && repoState.PendingCompletion != nil {
			return true
		}
	}
	return false
}

func (a *Application) logf(format string, values ...any) {
	if err := os.MkdirAll(a.configDir, 0o755); err != nil {
		return
	}
	file, err := os.OpenFile(a.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	fmt.Fprintf(file, "%s %s\n", a.now().UTC().Format(time.RFC3339), fmt.Sprintf(format, values...))
}

type serviceProgram struct {
	app    *Application
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *serviceProgram) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		p.app.runLoop(ctx)
	}()
	return nil
}

func (p *serviceProgram) Stop() error {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel = nil
	p.done = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}
	return nil
}

func (p *serviceProgram) Done() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}
