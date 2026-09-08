package app

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JorgeMuehlebach/repo-sync/internal/background"
	"github.com/JorgeMuehlebach/repo-sync/internal/config"
	"github.com/JorgeMuehlebach/repo-sync/internal/discovery"
	"github.com/JorgeMuehlebach/repo-sync/internal/gitops"
	"github.com/JorgeMuehlebach/repo-sync/internal/state"
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
	syncer         gitops.Syncer
	serviceFactory func() (background.Controller, error)
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
		syncer:     gitops.NewSyncer(),
	}, nil
}

func (a *Application) Run(args []string) error {
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
	case "bootstrap":
		return a.bootstrap(context.Background(), args[1:])
	case "setup":
		return a.setup(context.Background())
	case "start":
		return a.start()
	case "stop":
		return a.stop()
	case "uninstall":
		return a.uninstall()
	case "status":
		return a.status()
	case "sync":
		if len(args) == 2 && args[1] == "--dry-run" {
			return a.dryRun(context.Background())
		}
		if len(args) != 1 {
			return fmt.Errorf("usage: repo-sync sync [--dry-run]")
		}
		return a.syncAll(context.Background(), true)
	case "doctor":
		if len(args) != 1 {
			return fmt.Errorf("usage: repo-sync doctor")
		}
		return a.doctor(context.Background())
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
  repo-sync bootstrap <repository-path|github-branch-url> [--start]
  repo-sync setup
  repo-sync start
  repo-sync stop
  repo-sync status
  repo-sync sync [--dry-run]
  repo-sync doctor
  repo-sync uninstall
  repo-sync config add <github-branch-url>
  repo-sync config remove <github-branch-url>
  repo-sync config list
  repo-sync version`)
}

func (a *Application) bootstrap(ctx context.Context, args []string) error {
	var target string
	start := false
	for _, argument := range args {
		switch {
		case argument == "--start":
			start = true
		case strings.HasPrefix(argument, "-"):
			return fmt.Errorf("unknown bootstrap option %q", argument)
		case target == "":
			target = argument
		default:
			return fmt.Errorf("usage: repo-sync bootstrap <repository-path|github-branch-url> [--start]")
		}
	}
	if target == "" {
		return fmt.Errorf("usage: repo-sync bootstrap <repository-path|github-branch-url> [--start]")
	}

	var spec discovery.RepositorySpec
	var selected string
	var err error
	if strings.Contains(target, "://") {
		spec, err = discovery.ParseGitHubBranchURL(target)
		if err == nil {
			selected, err = a.bootstrapURL(ctx, spec)
		}
	} else {
		selected, err = a.expandPath(target)
		if err == nil {
			spec, selected, err = discovery.SpecFromPath(ctx, selected)
		}
	}
	if err != nil {
		return err
	}
	if err := a.saveBootstrap(spec, selected); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "Configured %s (%s) at %s\n", spec.Key, spec.Branch, selected)
	if !start {
		fmt.Fprintln(a.out, "Run repo-sync start when you are ready to enable automatic synchronization.")
		return nil
	}
	if err := a.requireResolvedRepositories(); err != nil {
		return err
	}
	fmt.Fprintln(a.out, "Warning: automatic synchronization stages all non-ignored changes, commits, rebases, and pushes.")
	return a.start()
}

func (a *Application) bootstrapURL(ctx context.Context, spec discovery.RepositorySpec) (string, error) {
	machineState, err := state.Load(a.statePath)
	if err != nil {
		return "", err
	}
	selected := machineState.Repositories[spec.StateKey()].Path
	if selected == "" {
		selected = filepath.Join(a.homeDir, spec.Name)
	}
	selected, err = a.expandPath(selected)
	if err != nil {
		return "", err
	}
	info, statErr := os.Stat(selected)
	if statErr == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("%s exists and is not a directory", selected)
		}
		if err := discovery.ValidatePath(ctx, selected, spec); err != nil {
			return "", err
		}
		if _, err := a.syncer.Inspect(ctx, selected, spec); err != nil {
			return "", err
		}
		return selected, nil
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	if err := os.MkdirAll(filepath.Dir(selected), 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(a.out, "Cloning %s into %s...\n", spec.CloneURL, selected)
	cmd := exec.CommandContext(ctx, "git", "clone", "--branch", spec.Branch, "--single-branch", spec.CloneURL, selected)
	cmd.Stdout = a.out
	cmd.Stderr = a.errOut
	cmd.Stdin = a.in
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("clone %s: %w", spec.Key, err)
	}
	return selected, nil
}

func (a *Application) saveBootstrap(spec discovery.RepositorySpec, selected string) error {
	if err := config.Update(a.configPath, func(current *config.Config) error {
		for _, raw := range current.Repositories {
			existing, err := discovery.ParseGitHubBranchURL(raw)
			if err == nil && existing.StateKey() == spec.StateKey() {
				return nil
			}
		}
		current.Repositories = append(current.Repositories, spec.OriginalURL)
		return nil
	}); err != nil {
		return err
	}
	return state.Update(a.statePath, func(current *state.State) error {
		repository := current.Repositories[spec.StateKey()]
		repository.Path = selected
		current.Repositories[spec.StateKey()] = repository
		return nil
	})
}

func (a *Application) requireResolvedRepositories() error {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	machineState, err := state.Load(a.statePath)
	if err != nil {
		return err
	}
	for _, raw := range cfg.Repositories {
		spec, err := discovery.ParseGitHubBranchURL(raw)
		if err != nil {
			return err
		}
		if machineState.Repositories[spec.StateKey()].Path == "" {
			return fmt.Errorf("%s (%s) has no local path; bootstrap it or run repo-sync setup before --start", spec.Key, spec.Branch)
		}
	}
	return nil
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
		if len(cfg.Repositories) == 0 {
			fmt.Fprintln(a.out, "No repositories configured.")
		}
		for _, raw := range cfg.Repositories {
			spec, err := discovery.ParseGitHubBranchURL(raw)
			if err != nil {
				fmt.Fprintf(a.out, "[invalid repository entry: %v]\n", err)
				continue
			}
			fmt.Fprintln(a.out, spec.OriginalURL)
		}
		fmt.Fprintf(a.out, "Config: %s\n", a.configPath)
		return nil
	case "add":
		if len(args) != 2 {
			return fmt.Errorf("usage: repo-sync config add <github-branch-url>")
		}
		candidate, err := discovery.ParseGitHubBranchURL(args[1])
		if err != nil {
			return err
		}
		if err := config.Update(a.configPath, func(current *config.Config) error {
			for _, raw := range current.Repositories {
				existing, err := discovery.ParseGitHubBranchURL(raw)
				if err == nil && existing.StateKey() == candidate.StateKey() {
					return fmt.Errorf("%s is already configured", candidate.OriginalURL)
				}
			}
			current.Repositories = append(current.Repositories, candidate.OriginalURL)
			return nil
		}); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "Added %s\n", args[1])
		return nil
	case "remove":
		if len(args) != 2 {
			return fmt.Errorf("usage: repo-sync config remove <github-branch-url>")
		}
		candidate, err := discovery.ParseGitHubBranchURL(args[1])
		if err != nil {
			return err
		}
		if err := config.Update(a.configPath, func(current *config.Config) error {
			filtered := make([]string, 0, len(current.Repositories))
			removed := false
			for _, raw := range current.Repositories {
				existing, parseErr := discovery.ParseGitHubBranchURL(raw)
				if parseErr == nil && existing.StateKey() == candidate.StateKey() {
					removed = true
					continue
				}
				filtered = append(filtered, raw)
			}
			if !removed {
				return fmt.Errorf("%s is not configured", args[1])
			}
			current.Repositories = filtered
			return nil
		}); err != nil {
			return err
		}
		if err := state.Update(a.statePath, func(current *state.State) error {
			delete(current.Repositories, candidate.StateKey())
			return nil
		}); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "Removed %s\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown config command %q", args[0])
	}
}

func (a *Application) setup(ctx context.Context) error {
	cfg, err := config.Ensure(a.configPath)
	if err != nil {
		return err
	}
	if len(cfg.Repositories) == 0 {
		return fmt.Errorf("no repositories configured; run repo-sync config add <github-branch-url>")
	}
	specs := make([]discovery.RepositorySpec, 0, len(cfg.Repositories))
	wanted := make(map[string]bool)
	for _, raw := range cfg.Repositories {
		spec, err := discovery.ParseGitHubBranchURL(raw)
		if err != nil {
			return fmt.Errorf("invalid repository configuration: %w", err)
		}
		specs = append(specs, spec)
		wanted[spec.Key] = true
	}
	roots := []string{a.homeDir}
	for _, root := range cfg.SearchRoots {
		expanded, err := a.expandPath(root)
		if err != nil {
			return err
		}
		roots = append(roots, expanded)
	}
	fmt.Fprintln(a.out, "Searching for configured repositories...")
	matches, err := discovery.Find(ctx, roots, wanted)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(a.in)
	selectedPaths := make(map[string]string)
	for _, spec := range specs {
		selected, err := a.selectRepository(ctx, reader, spec, matches[spec.Key])
		if err != nil {
			return err
		}
		selectedPaths[spec.StateKey()] = selected
		fmt.Fprintf(a.out, "Configured %s (%s) at %s\n", spec.Key, spec.Branch, selected)
	}
	if err := state.Update(a.statePath, func(current *state.State) error {
		for key, selected := range selectedPaths {
			repoState := current.Repositories[key]
			repoState.Path = selected
			current.Repositories[key] = repoState
		}
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "Setup complete. Config: %s\n", a.configPath)
	return nil
}

func (a *Application) selectRepository(ctx context.Context, reader *bufio.Reader, spec discovery.RepositorySpec, matches []string) (string, error) {
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
		selected, err := a.expandPath(value)
		if err != nil {
			return "", err
		}
		info, statErr := os.Stat(selected)
		if statErr == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", selected)
			}
			if err := discovery.ValidatePath(ctx, selected, spec); err != nil {
				return "", err
			}
			return selected, nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		if err := os.MkdirAll(filepath.Dir(selected), 0o755); err != nil {
			return "", err
		}
		fmt.Fprintf(a.out, "Cloning %s into %s...\n", spec.CloneURL, selected)
		cmd := exec.CommandContext(ctx, "git", "clone", "--branch", spec.Branch, "--single-branch", spec.CloneURL, selected)
		cmd.Stdout = a.out
		cmd.Stderr = a.errOut
		cmd.Stdin = a.in
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("clone %s: %w", spec.Key, err)
		}
		return selected, nil
	default:
		fmt.Fprintf(a.out, "Found multiple clones of %s (%s):\n", spec.Key, spec.Branch)
		for i, match := range matches {
			fmt.Fprintf(a.out, "  %d. %s\n", i+1, match)
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

func (a *Application) dryRun(ctx context.Context) error {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	machineState, err := state.Load(a.statePath)
	if err != nil {
		return err
	}
	var failures []error
	for _, raw := range cfg.Repositories {
		spec, parseErr := discovery.ParseGitHubBranchURL(raw)
		if parseErr != nil {
			failures = append(failures, parseErr)
			continue
		}
		repoState := machineState.Repositories[spec.StateKey()]
		fmt.Fprintf(a.out, "Checking %s (%s)...\n", spec.Key, spec.Branch)
		if repoState.Path == "" {
			failures = append(failures, fmt.Errorf("%s (%s) has no local path; bootstrap it or run repo-sync setup", spec.Key, spec.Branch))
			continue
		}
		inspection, inspectErr := a.syncer.Inspect(ctx, repoState.Path, spec)
		if inspectErr != nil {
			failures = append(failures, fmt.Errorf("%s: %w", spec.Key, inspectErr))
			continue
		}
		if len(inspection.Changes) == 0 {
			fmt.Fprintln(a.out, "  working tree is clean")
		} else {
			fmt.Fprintln(a.out, "  changes that would be staged and committed:")
			for _, change := range inspection.Changes {
				fmt.Fprintf(a.out, "    %s\n", change)
			}
		}
		fmt.Fprintf(a.out, "  would fetch origin %s, rebase, and push HEAD:%s\n", spec.Branch, spec.Branch)
	}
	return errors.Join(failures...)
}

func (a *Application) doctor(ctx context.Context) error {
	fmt.Fprintln(a.out, "Repo Sync doctor")
	var failures []error
	gitPath, err := exec.LookPath("git")
	if err != nil {
		fmt.Fprintln(a.out, "FAIL git executable was not found on PATH")
		return err
	}
	fmt.Fprintf(a.out, "PASS git executable: %s\n", gitPath)
	cfg, err := config.Load(a.configPath)
	if err != nil {
		fmt.Fprintf(a.out, "FAIL config: %v\n", err)
		return err
	}
	fmt.Fprintf(a.out, "PASS config: %s\n", a.configPath)
	machineState, err := state.Load(a.statePath)
	if err != nil {
		fmt.Fprintf(a.out, "FAIL state: %v\n", err)
		return err
	}
	if len(cfg.Repositories) == 0 {
		failure := fmt.Errorf("no repositories are configured")
		fmt.Fprintf(a.out, "FAIL repositories: %v\n", failure)
		failures = append(failures, failure)
	}
	for _, raw := range cfg.Repositories {
		spec, parseErr := discovery.ParseGitHubBranchURL(raw)
		if parseErr != nil {
			fmt.Fprintf(a.out, "FAIL repository config: %v\n", parseErr)
			failures = append(failures, parseErr)
			continue
		}
		label := fmt.Sprintf("%s (%s)", spec.Key, spec.Branch)
		repoState := machineState.Repositories[spec.StateKey()]
		if repoState.Path == "" {
			failure := fmt.Errorf("local path is not configured")
			fmt.Fprintf(a.out, "FAIL %s: %v\n", label, failure)
			failures = append(failures, fmt.Errorf("%s: %w", label, failure))
			continue
		}
		if _, inspectErr := a.syncer.Inspect(ctx, repoState.Path, spec); inspectErr != nil {
			fmt.Fprintf(a.out, "FAIL %s local checkout: %v\n", label, inspectErr)
			failures = append(failures, fmt.Errorf("%s: %w", label, inspectErr))
			continue
		}
		fmt.Fprintf(a.out, "PASS %s local checkout: %s\n", label, repoState.Path)
		if identityErr := a.syncer.CheckIdentity(ctx, repoState.Path); identityErr != nil {
			fmt.Fprintf(a.out, "FAIL %s identity: %v\n", label, identityErr)
			failures = append(failures, fmt.Errorf("%s: %w", label, identityErr))
		} else {
			fmt.Fprintf(a.out, "PASS %s Git identity\n", label)
		}
		remoteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		remoteErr := a.syncer.CheckRemote(remoteCtx, repoState.Path, spec)
		cancel()
		if remoteErr != nil {
			fmt.Fprintf(a.out, "FAIL %s remote access: %v\n", label, remoteErr)
			failures = append(failures, fmt.Errorf("%s: %w", label, remoteErr))
		} else {
			fmt.Fprintf(a.out, "PASS %s remote access\n", label)
		}
		ignored, ignoredErr := a.syncer.Ignored(ctx, repoState.Path)
		if ignoredErr != nil {
			fmt.Fprintf(a.out, "FAIL %s ignored-file check: %v\n", label, ignoredErr)
			failures = append(failures, fmt.Errorf("%s: %w", label, ignoredErr))
		} else if len(ignored) > 0 {
			fmt.Fprintf(a.out, "WARN %s has %d ignored path(s); these will not be synchronized\n", label, len(ignored))
		}
	}
	service, serviceErr := a.newService()
	if serviceErr != nil {
		fmt.Fprintf(a.out, "FAIL service: %v\n", serviceErr)
		failures = append(failures, serviceErr)
	} else {
		switch serviceStatus, statusErr := service.Status(); {
		case errors.Is(statusErr, background.ErrNotInstalled):
			fmt.Fprintln(a.out, "WARN service is not installed; repo-sync start will install it")
		case statusErr != nil:
			fmt.Fprintf(a.out, "FAIL service: %v\n", statusErr)
			failures = append(failures, statusErr)
		case serviceStatus == background.StatusRunning:
			fmt.Fprintln(a.out, "PASS service is running")
		default:
			fmt.Fprintln(a.out, "WARN service is installed but stopped")
		}
	}
	if len(failures) == 0 {
		fmt.Fprintln(a.out, "Doctor found no blocking problems.")
	}
	return errors.Join(failures...)
}

func (a *Application) syncAll(ctx context.Context, printProgress bool) error {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	var failures []error
	for _, raw := range cfg.Repositories {
		spec, parseErr := discovery.ParseGitHubBranchURL(raw)
		if parseErr != nil {
			failures = append(failures, parseErr)
			continue
		}
		machineState, loadErr := state.Load(a.statePath)
		if loadErr != nil {
			failures = append(failures, loadErr)
			continue
		}
		repoState := machineState.Repositories[spec.StateKey()]
		if repoState.Path == "" {
			err := fmt.Errorf("%s (%s) has no local path; run repo-sync setup", spec.Key, spec.Branch)
			repoState.LastError = err.Error()
			if updateErr := a.updateRepositoryState(spec.StateKey(), repoState); updateErr != nil {
				failures = append(failures, updateErr)
			}
			failures = append(failures, err)
			continue
		}
		if printProgress {
			fmt.Fprintf(a.out, "Syncing %s (%s)...\n", spec.Key, spec.Branch)
		}
		release, lockErr := a.acquireLock(spec.StateKey())
		if lockErr != nil {
			repoState.LastError = lockErr.Error()
			if updateErr := a.updateRepositoryState(spec.StateKey(), repoState); updateErr != nil {
				failures = append(failures, updateErr)
			}
			failures = append(failures, lockErr)
			continue
		}
		repoCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		syncErr := a.syncer.Sync(repoCtx, repoState.Path, spec)
		cancel()
		release()
		if syncErr != nil {
			repoState.LastError = syncErr.Error()
			failures = append(failures, fmt.Errorf("%s: %w", spec.Key, syncErr))
			a.logf("sync failed for %s (%s): %v", spec.Key, spec.Branch, syncErr)
		} else {
			repoState.LastError = ""
			repoState.LastSync = time.Now().UTC()
			if printProgress {
				fmt.Fprintln(a.out, "  up to date")
			}
			a.logf("sync completed for %s (%s)", spec.Key, spec.Branch)
		}
		if updateErr := a.updateRepositoryState(spec.StateKey(), repoState); updateErr != nil {
			failures = append(failures, updateErr)
		}
	}
	return errors.Join(failures...)
}

func (a *Application) updateRepositoryState(key string, repoState state.Repository) error {
	return state.Update(a.statePath, func(current *state.State) error {
		current.Repositories[key] = repoState
		return nil
	})
}

func (a *Application) acquireLock(key string) (func(), error) {
	if err := os.MkdirAll(a.lockDir, 0o755); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%x.lock", sha256.Sum256([]byte(key)))
	path := filepath.Join(a.lockDir, name)
	create := func() (*os.File, error) {
		return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
	file, err := create()
	if errors.Is(err, os.ErrExist) {
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > 10*time.Minute {
			_ = os.Remove(path)
			file, err = create()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("repository is already being synchronized")
	}
	_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
	_ = file.Close()
	return func() { _ = os.Remove(path) }, nil
}

func (a *Application) start() error {
	cfg, err := config.Ensure(a.configPath)
	if err != nil {
		return err
	}
	if len(cfg.Repositories) == 0 {
		return fmt.Errorf("no repositories configured; run repo-sync config add first")
	}
	machineState, err := state.Load(a.statePath)
	if err != nil {
		return err
	}
	for _, raw := range cfg.Repositories {
		spec, err := discovery.ParseGitHubBranchURL(raw)
		if err != nil {
			return err
		}
		if machineState.Repositories[spec.StateKey()].Path == "" {
			if err := a.setup(context.Background()); err != nil {
				return err
			}
			machineState, err = state.Load(a.statePath)
			if err != nil {
				return err
			}
			break
		}
	}
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
	fmt.Fprintln(a.out, "Repo Sync is enabled and running.")
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
	fmt.Fprintf(a.out, "Service removed. Configuration preserved at %s\n", a.configPath)
	return nil
}

func (a *Application) status() error {
	cfg, err := config.Ensure(a.configPath)
	if err != nil {
		return err
	}
	machineState, err := state.Load(a.statePath)
	if err != nil {
		return err
	}
	serviceState := "not installed"
	service, err := a.newService()
	if err == nil {
		switch status, statusErr := service.Status(); {
		case errors.Is(statusErr, background.ErrNotInstalled):
		case statusErr != nil:
			serviceState = "unknown: " + statusErr.Error()
		case status == background.StatusRunning:
			serviceState = "running"
		case status == background.StatusStopped:
			serviceState = "stopped"
		default:
			serviceState = "unknown"
		}
	}
	fmt.Fprintf(a.out, "Service: %s\nEnabled: %t\nInterval: %s\nConfig: %s\n", serviceState, machineState.Enabled, cfg.Interval, a.configPath)
	for _, raw := range cfg.Repositories {
		spec, parseErr := discovery.ParseGitHubBranchURL(raw)
		if parseErr != nil {
			fmt.Fprintf(a.out, "- invalid repository configuration: %v\n", parseErr)
			continue
		}
		repoState := machineState.Repositories[spec.StateKey()]
		fmt.Fprintf(a.out, "- %s (%s)\n  path: %s\n", spec.Key, spec.Branch, valueOr(repoState.Path, "not configured"))
		if repoState.LastError != "" {
			fmt.Fprintf(a.out, "  error: %s\n", repoState.LastError)
		} else if !repoState.LastSync.IsZero() {
			fmt.Fprintf(a.out, "  last sync: %s\n", repoState.LastSync.Local().Format(time.RFC3339))
		} else {
			fmt.Fprintln(a.out, "  last sync: never")
		}
	}
	return nil
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
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
		Description:  "Synchronize configured GitHub repository branches every five minutes.",
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
		a.logf("cannot start: %v", err)
		return
	}
	interval, err := cfg.Duration()
	if err != nil {
		a.logf("cannot start: %v", err)
		return
	}
	machineState, err := state.Load(a.statePath)
	if err != nil || !machineState.Enabled {
		return
	}
	_ = a.syncAll(ctx, false)
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
			_ = a.syncAll(ctx, false)
		}
	}
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
	fmt.Fprintf(file, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, values...))
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
