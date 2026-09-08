package gitops

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/JorgeMuehlebach/repo-sync/internal/discovery"
)

type fakeRunner struct {
	root       string
	dirty      bool
	pushFail   int
	rebaseFail bool
	calls      []string
}

func (f *fakeRunner) Run(_ context.Context, _ string, args ...string) Result {
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if strings.HasPrefix(call, "rev-parse --git-path ") {
		return Result{Output: filepath.Join(f.root, ".git", args[len(args)-1])}
	}
	switch call {
	case "rev-parse --show-toplevel":
		return Result{Output: f.root}
	case "config --get remote.origin.url":
		return Result{Output: "git@github.com:owner/repo.git"}
	case "branch --show-current":
		return Result{Output: "main"}
	case "status --porcelain=v1":
		if f.dirty {
			return Result{Output: " M README.md"}
		}
		return Result{}
	case "diff --cached --quiet":
		return Result{ExitCode: 1, Err: errors.New("exit status 1")}
	case "push origin HEAD:main":
		if f.pushFail > 0 {
			f.pushFail--
			return Result{ExitCode: 1, Err: errors.New("rejected"), Output: "non-fast-forward"}
		}
		return Result{}
	case "rebase origin/main":
		if f.rebaseFail {
			return Result{ExitCode: 1, Err: errors.New("conflict"), Output: "CONFLICT"}
		}
		return Result{}
	default:
		return Result{}
	}
}

func TestSyncAbortsConflictingRebase(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	runner := &fakeRunner{root: root, rebaseFail: true}
	syncer := Syncer{Git: runner}
	spec := discovery.RepositorySpec{Key: "owner/repo", Branch: "main"}
	err := syncer.Sync(context.Background(), root, spec)
	if err == nil || !strings.Contains(err.Error(), "rebase") {
		t.Fatalf("Sync() error = %v", err)
	}
	seenAbort := false
	for _, call := range runner.calls {
		if call == "rebase --abort" {
			seenAbort = true
		}
		if strings.HasPrefix(call, "push ") {
			t.Fatalf("push occurred after a rebase conflict: %#v", runner.calls)
		}
	}
	if !seenAbort {
		t.Fatalf("rebase abort not called: %#v", runner.calls)
	}
}

func TestCommandErrorRedactsURLCredentials(t *testing.T) {
	err := commandError(Result{Err: errors.New("failed"), Output: "fatal: https://secret@github.com/owner/repo"}, "fetch")
	if strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "https://***@github.com") {
		t.Fatalf("redacted error = %q", err)
	}
}

func TestSyncCommitsRebasesAndPushes(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	runner := &fakeRunner{root: root, dirty: true}
	syncer := Syncer{Git: runner}
	spec := discovery.RepositorySpec{Key: "owner/repo", Branch: "main"}
	if err := syncer.Sync(context.Background(), root, spec); err != nil {
		t.Fatal(err)
	}
	wantTail := []string{
		"status --porcelain=v1",
		"add -A",
		"diff --cached --quiet",
		"commit -m chore: automatic repository sync",
		"fetch origin main",
		"rebase origin/main",
		"push origin HEAD:main",
	}
	if len(runner.calls) < len(wantTail) || !reflect.DeepEqual(runner.calls[len(runner.calls)-len(wantTail):], wantTail) {
		t.Fatalf("calls = %#v; want tail %#v", runner.calls, wantTail)
	}
}

func TestSyncRetriesRejectedPush(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	runner := &fakeRunner{root: root, pushFail: 1}
	syncer := Syncer{Git: runner}
	spec := discovery.RepositorySpec{Key: "owner/repo", Branch: "main"}
	if err := syncer.Sync(context.Background(), root, spec); err != nil {
		t.Fatal(err)
	}
	pushes := 0
	for _, call := range runner.calls {
		if call == "push origin HEAD:main" {
			pushes++
		}
	}
	if pushes != 2 {
		t.Fatalf("push attempts = %d, want 2", pushes)
	}
}

func TestSyncPausesOnDifferentBranch(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	runner := &fakeRunner{root: root}
	syncer := Syncer{Git: branchRunner{fakeRunner: runner, branch: "feature"}}
	spec := discovery.RepositorySpec{Key: "owner/repo", Branch: "main"}
	if err := syncer.Sync(context.Background(), root, spec); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("Sync() error = %v, want paused error", err)
	}
}

type branchRunner struct {
	*fakeRunner
	branch string
}

func (r branchRunner) Run(ctx context.Context, dir string, args ...string) Result {
	if strings.Join(args, " ") == "branch --show-current" {
		r.calls = append(r.calls, "branch --show-current")
		return Result{Output: r.branch}
	}
	return r.fakeRunner.Run(ctx, dir, args...)
}
