package gitops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/JorgeMuehlebach/repo-sync/internal/discovery"
	"github.com/JorgeMuehlebach/repo-sync/internal/validation"
)

func (m Mirror) syncTransactionForTest(ctx context.Context, target Target) (Outcome, *OperationError) {
	result, failure := m.SyncTransaction(ctx, target)
	return result.Outcome, failure
}

func TestPublisherValidatesExactTemporaryIndexAndPublishes(t *testing.T) {
	fixture := newRepositoryFixture(t)
	beforeIndex := readIndex(t, fixture.publisher)
	if err := os.WriteFile(filepath.Join(fixture.publisher, "README.md"), []byte("published\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.publisher, "new.md"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	validator := &recordingValidator{}
	runner := &recordingRunner{delegate: fixture.runner()}
	outcome, failure := NewPublisher(runner, validator).Sync(context.Background(), fixture.target(fixture.publisher))
	if failure != nil {
		t.Fatal(failure)
	}
	if len(validator.requests) < 2 || outcome.AcceptedTree == "" || outcome.AcceptedTree != validator.requests[0].Tree {
		t.Fatalf("outcome/requests = %#v %#v", outcome, validator.requests)
	}
	for _, request := range validator.requests {
		if request.Tree != outcome.AcceptedTree {
			t.Fatalf("validated different tree %s, accepted %s", request.Tree, outcome.AcceptedTree)
		}
	}
	if bytes.Equal(beforeIndex, readIndex(t, fixture.publisher)) {
		t.Fatal("real index did not advance after validated publication")
	}
	if got := runGit(t, fixture.bare, "show", "main:README.md"); got != "published" {
		t.Fatalf("remote README = %q", got)
	}
	if got := runGit(t, fixture.publisher, "status", "--porcelain=v1"); got != "" {
		t.Fatalf("publisher is dirty: %q", got)
	}
	foundExactPush := false
	for _, call := range runner.calls {
		if gitSubcommand(call) == "push" && containsArgument(call, outcome.AcceptedCommit+":refs/heads/main") {
			foundExactPush = true
		}
		if gitSubcommand(call) == "push" && containsArgument(call, "HEAD:refs/heads/main") {
			t.Fatalf("publisher pushed symbolic HEAD: %#v", call)
		}
	}
	if !foundExactPush {
		t.Fatalf("publisher did not push the exact validated object ID: %#v", runner.calls)
	}
}

func TestPublisherNoOpStillValidatesWithoutCreatingCommit(t *testing.T) {
	fixture := newRepositoryFixture(t)
	before := runGit(t, fixture.publisher, "rev-parse", "HEAD")
	validator := &recordingValidator{}
	outcome, failure := NewPublisher(fixture.runner(), validator).Sync(context.Background(), fixture.target(fixture.publisher))
	if failure != nil {
		t.Fatal(failure)
	}
	if got := runGit(t, fixture.publisher, "rev-parse", "HEAD"); got != before || outcome.AcceptedCommit != before {
		t.Fatalf("no-op publisher moved HEAD: got %s, outcome %#v", got, outcome)
	}
	if len(validator.requests) < 2 {
		t.Fatalf("no-op publisher did not validate before publication checks: %#v", validator.requests)
	}
}

func TestPublisherValidationFailureLeavesRealIndexAndHeadUntouched(t *testing.T) {
	fixture := newRepositoryFixture(t)
	beforeIndex := readIndex(t, fixture.publisher)
	beforeHead := runGit(t, fixture.publisher, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(fixture.publisher, "README.md"), []byte("untrusted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.publisher, "untracked.md"), []byte("untrusted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	untrustedObject := runGit(t, fixture.publisher, "hash-object", "--no-filters", "untracked.md")
	untrustedObjectPath := filepath.Join(fixture.publisher, ".git", "objects", untrustedObject[:2], untrustedObject[2:])
	if _, err := os.Stat(untrustedObjectPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test object unexpectedly existed before validation: %v", err)
	}
	validator := &recordingValidator{verdict: validation.VerdictFail}
	_, failure := NewPublisher(fixture.runner(), validator).Sync(context.Background(), fixture.target(fixture.publisher))
	if failure == nil || failure.Code != "REPO-VALIDATION-FAILED" {
		t.Fatalf("failure = %#v", failure)
	}
	if !bytes.Equal(beforeIndex, readIndex(t, fixture.publisher)) {
		t.Fatal("validation failure changed the real Git index")
	}
	if _, err := os.Stat(untrustedObjectPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected candidate blob persisted in the real object store: %v", err)
	}
	if got := runGit(t, fixture.publisher, "rev-parse", "HEAD"); got != beforeHead {
		t.Fatalf("HEAD = %s, want %s", got, beforeHead)
	}
	if got := runGit(t, fixture.publisher, "status", "--porcelain=v1"); !strings.Contains(got, "README.md") || !strings.Contains(got, "untracked.md") {
		t.Fatalf("local edits were not preserved: %q", got)
	}
}

func TestPublisherRestoresPreimageAfterPostRebaseValidationFailure(t *testing.T) {
	fixture := newRepositoryFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.mirror, "remote.md"), []byte("remote\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, fixture.mirror, "add", "remote.md")
	runGit(t, fixture.mirror, "commit", "-m", "remote advance")
	runGit(t, fixture.mirror, "push", "origin", "HEAD:main")
	remoteHead := runGit(t, fixture.bare, "rev-parse", "refs/heads/main")
	originalHead := runGit(t, fixture.publisher, "rev-parse", "HEAD")
	originalIndex := readIndex(t, fixture.publisher)
	if err := os.WriteFile(filepath.Join(fixture.publisher, "README.md"), []byte("local candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	validationCalls := 0
	validator := validatorFunc(func(_ context.Context, request validation.Request) (validation.Report, error) {
		validationCalls++
		verdict := validation.VerdictPass
		if validationCalls == 3 {
			verdict = validation.VerdictFail
		}
		return reportForRequest(request, verdict), nil
	})
	_, failure := NewPublisher(fixture.runner(), validator).Sync(context.Background(), fixture.target(fixture.publisher))
	if failure == nil || failure.Code != "REPO-VALIDATION-FAILED" || validationCalls != 3 {
		t.Fatalf("failure/calls = %#v %d", failure, validationCalls)
	}
	if got := runGit(t, fixture.publisher, "rev-parse", "HEAD"); got != originalHead {
		t.Fatalf("publisher HEAD = %s, want original %s", got, originalHead)
	}
	if !bytes.Equal(readIndex(t, fixture.publisher), originalIndex) {
		t.Fatal("publisher index was not restored to its pre-publication image")
	}
	contents, err := os.ReadFile(filepath.Join(fixture.publisher, "README.md"))
	if err != nil || string(contents) != "local candidate\n" {
		t.Fatalf("local candidate was not restored: %q, %v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.publisher, "remote.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rebased remote content leaked into restored worktree: %v", err)
	}
	if got := runGit(t, fixture.bare, "rev-parse", "refs/heads/main"); got != remoteHead {
		t.Fatalf("failed publication changed remote HEAD to %s", got)
	}
}

func TestPublisherRefusesToOverwriteConcurrentIndexLock(t *testing.T) {
	fixture := newRepositoryFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.publisher, "README.md"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(fixture.publisher, ".git", "index.lock")
	runner := &afterCommandRunner{
		delegate: fixture.runner(),
		command:  "fsck",
		after: func() error {
			return os.WriteFile(lockPath, []byte("concurrent lock"), 0o600)
		},
	}
	_, failure := NewPublisher(runner, &recordingValidator{}).Sync(context.Background(), fixture.target(fixture.publisher))
	if failure == nil || failure.Code != "REPO-PUBLISH-INDEX-RACE" {
		t.Fatalf("failure = %#v", failure)
	}
	contents, err := os.ReadFile(lockPath)
	if err != nil || string(contents) != "concurrent lock" {
		t.Fatalf("concurrent index lock was changed: %q, %v", contents, err)
	}
}

func TestPublisherRechecksBranchAfterValidation(t *testing.T) {
	fixture := newRepositoryFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.publisher, "README.md"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	validator := validatorFunc(func(_ context.Context, request validation.Request) (validation.Report, error) {
		if runGit(t, fixture.publisher, "branch", "--show-current") == "main" {
			runGit(t, fixture.publisher, "switch", "-c", "concurrent")
		}
		return reportForRequest(request, validation.VerdictPass), nil
	})
	_, failure := NewPublisher(fixture.runner(), validator).Sync(context.Background(), fixture.target(fixture.publisher))
	if failure == nil || failure.Code != "REPO-GIT-BRANCH" {
		t.Fatalf("failure = %#v", failure)
	}
	if got := runGit(t, fixture.publisher, "branch", "--show-current"); got != "concurrent" {
		t.Fatalf("concurrent branch change was overwritten: %s", got)
	}
}

func TestPublisherPreservesTrackedModeAndRejectsOversizedFile(t *testing.T) {
	t.Run("filemode disabled preserves index mode", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		path := filepath.Join(fixture.publisher, "README.md")
		runGit(t, fixture.publisher, "config", "core.filemode", "false")
		if err := os.WriteFile(path, []byte("mode remains stable\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, failure := NewPublisher(fixture.runner(), &recordingValidator{}).Sync(context.Background(), fixture.target(fixture.publisher)); failure != nil {
			t.Fatal(failure)
		}
		if entry := runGit(t, fixture.publisher, "ls-tree", "HEAD", "README.md"); !strings.HasPrefix(entry, "100644 ") {
			t.Fatalf("tracked mode changed: %q", entry)
		}
	})
	t.Run("filemode enabled honors executable changes", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows has no POSIX executable mode")
		}
		fixture := newRepositoryFixture(t)
		path := filepath.Join(fixture.publisher, "README.md")
		runGit(t, fixture.publisher, "config", "core.filemode", "true")
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, failure := NewPublisher(fixture.runner(), &recordingValidator{}).Sync(context.Background(), fixture.target(fixture.publisher)); failure != nil {
			t.Fatal(failure)
		}
		if entry := runGit(t, fixture.publisher, "ls-tree", "HEAD", "README.md"); !strings.HasPrefix(entry, "100755 ") {
			t.Fatalf("+x change was omitted: %q", entry)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, failure := NewPublisher(fixture.runner(), &recordingValidator{}).Sync(context.Background(), fixture.target(fixture.publisher)); failure != nil {
			t.Fatal(failure)
		}
		if entry := runGit(t, fixture.publisher, "ls-tree", "HEAD", "README.md"); !strings.HasPrefix(entry, "100644 ") {
			t.Fatalf("-x change was omitted: %q", entry)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		path := filepath.Join(fixture.publisher, "large.bin")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(maxCandidateFileBytes + 1); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		validator := &recordingValidator{}
		_, failure := NewPublisher(fixture.runner(), validator).Sync(context.Background(), fixture.target(fixture.publisher))
		if failure == nil || failure.Code != "REPO-PUBLISH-SNAPSHOT-LIMIT" || len(validator.requests) != 0 {
			t.Fatalf("failure/requests = %#v %#v", failure, validator.requests)
		}
	})
}

func TestPublisherDisablesHooksAndSigning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell trap fixture is Unix-specific")
	}
	fixture := newRepositoryFixture(t)
	marker := filepath.Join(fixture.root, "executed")
	trap := filepath.Join(fixture.root, "trap.sh")
	writeExecutable(t, trap, "#!/bin/sh\necho executed >> \""+marker+"\"\nexit 1\n")
	hooks := filepath.Join(fixture.publisher, ".git", "hooks")
	for _, name := range []string{"pre-commit", "commit-msg", "pre-rebase", "pre-push"} {
		if err := os.WriteFile(filepath.Join(hooks, name), []byte("#!/bin/sh\necho hook >> \""+marker+"\"\nexit 1\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, fixture.publisher, "config", "commit.gpgSign", "true")
	runGit(t, fixture.publisher, "config", "gpg.program", trap)
	runGit(t, fixture.publisher, "config", "push.gpgSign", "true")
	runGit(t, fixture.publisher, "config", "core.fsmonitor", trap)
	if err := os.WriteFile(filepath.Join(fixture.publisher, "README.md"), []byte("safe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, failure := NewPublisher(fixture.runner(), &recordingValidator{}).Sync(context.Background(), fixture.target(fixture.publisher)); failure != nil {
		t.Fatal(failure)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configured executable ran: %v", err)
	}
}

func TestPublisherRejectsFilterAttributesWithoutExecutingFilter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell trap fixture is Unix-specific")
	}
	for _, attribute := range []string{"filter=evil", "filter"} {
		t.Run(attribute, func(t *testing.T) {
			fixture := newRepositoryFixture(t)
			marker := filepath.Join(fixture.root, "filter-executed")
			filter := filepath.Join(fixture.root, "filter.sh")
			writeExecutable(t, filter, "#!/bin/sh\necho filter >> \""+marker+"\"\ncat\n")
			runGit(t, fixture.publisher, "config", "filter.evil.clean", filter)
			if err := os.WriteFile(filepath.Join(fixture.publisher, ".gitattributes"), []byte("*.md "+attribute+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fixture.publisher, "README.md"), []byte("candidate\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, failure := NewPublisher(fixture.runner(), &recordingValidator{}).Sync(context.Background(), fixture.target(fixture.publisher))
			if failure == nil || failure.Code != "REPO-GIT-FILTER-ATTRIBUTE" {
				t.Fatalf("failure = %#v", failure)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("clean filter ran: %v", err)
			}
		})
	}
}

func TestPublisherRejectsMergeAttributesWithoutExecutingDriver(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell trap fixture is Unix-specific")
	}
	fixture := newRepositoryFixture(t)
	marker := filepath.Join(fixture.root, "merge-driver-executed")
	driver := filepath.Join(fixture.root, "merge-driver.sh")
	writeExecutable(t, driver, "#!/bin/sh\necho merge >> \""+marker+"\"\nexit 1\n")
	runGit(t, fixture.publisher, "config", "merge.evil.driver", driver)
	if err := os.WriteFile(filepath.Join(fixture.publisher, ".gitattributes"), []byte("*.md merge=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.publisher, "README.md"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, failure := NewPublisher(fixture.runner(), &recordingValidator{}).Sync(context.Background(), fixture.target(fixture.publisher))
	if failure == nil || failure.Code != "REPO-GIT-MERGE-ATTRIBUTE" {
		t.Fatalf("failure = %#v", failure)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("merge driver ran: %v", err)
	}
}

func TestMirrorFastForwardsOnlyAfterValidation(t *testing.T) {
	fixture := newRepositoryFixture(t)
	advanceRemote(t, fixture, "remote\n")
	validator := &recordingValidator{}
	runner := &recordingRunner{delegate: fixture.runner()}
	before := runGit(t, fixture.mirror, "rev-parse", "HEAD")
	outcome, failure := NewMirror(runner, validator).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
	if failure != nil {
		t.Fatal(failure)
	}
	if outcome.AcceptedCommit == before || outcome.AcceptedCommit != runGit(t, fixture.mirror, "rev-parse", "HEAD") {
		t.Fatalf("mirror outcome = %#v", outcome)
	}
	if got := runGit(t, fixture.mirror, "show", "HEAD:README.md"); got != "remote" {
		t.Fatalf("mirror README = %q", got)
	}
	for _, call := range runner.calls {
		command := gitSubcommand(call)
		for _, forbidden := range []string{"add", "commit", "rebase", "push", "checkout", "reset"} {
			if command == forbidden {
				t.Fatalf("mirror invoked forbidden command %q: %#v", forbidden, runner.calls)
			}
		}
	}
}

func TestMirrorNoOpStillValidatesWithoutPromotion(t *testing.T) {
	fixture := newRepositoryFixture(t)
	before := runGit(t, fixture.mirror, "rev-parse", "HEAD")
	validator := &recordingValidator{}
	runner := &recordingRunner{delegate: fixture.runner()}
	outcome, failure := NewMirror(runner, validator).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
	if failure != nil {
		t.Fatal(failure)
	}
	if outcome.AcceptedCommit != before || len(validator.requests) != 1 {
		t.Fatalf("no-op mirror outcome/validation = %#v %#v", outcome, validator.requests)
	}
	for _, call := range runner.calls {
		if gitSubcommand(call) == "merge" {
			t.Fatalf("no-op mirror attempted promotion: %#v", runner.calls)
		}
	}
}

func TestMirrorValidationFailurePreservesAcceptedCheckout(t *testing.T) {
	fixture := newRepositoryFixture(t)
	advanceRemote(t, fixture, "blocked\n")
	beforeHead := runGit(t, fixture.mirror, "rev-parse", "HEAD")
	beforeFile := runGit(t, fixture.mirror, "show", "HEAD:README.md")
	outcome, failure := NewMirror(fixture.runner(), &recordingValidator{verdict: validation.VerdictFail}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
	if failure == nil || failure.Code != "REPO-VALIDATION-FAILED" {
		t.Fatalf("failure = %#v", failure)
	}
	if got := runGit(t, fixture.mirror, "rev-parse", "HEAD"); got != beforeHead {
		t.Fatalf("HEAD moved to %s", got)
	}
	if got := runGit(t, fixture.mirror, "show", "HEAD:README.md"); got != beforeFile {
		t.Fatalf("working tree changed to %q", got)
	}
	if outcome.AcceptedCommit != "" || outcome.AcceptedTree != "" {
		t.Fatalf("unvalidated initial mirror HEAD was recorded as accepted: %#v", outcome)
	}
}

func TestMirrorRejectsCandidateAndExternalFilterAttributesBeforeCheckout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell trap fixture is Unix-specific")
	}
	t.Run("candidate attributes", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		marker := filepath.Join(fixture.root, "mirror-filter-executed")
		filter := filepath.Join(fixture.root, "mirror-filter.sh")
		writeExecutable(t, filter, "#!/bin/sh\necho filter >> \""+marker+"\"\ncat\n")
		runGit(t, fixture.mirror, "config", "filter.evil.smudge", filter)
		if err := os.WriteFile(filepath.Join(fixture.publisher, ".gitattributes"), []byte("*.md filter=evil\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, fixture.publisher, "add", ".gitattributes")
		runGit(t, fixture.publisher, "commit", "-m", "add filter attributes")
		runGit(t, fixture.publisher, "push", "origin", "main")
		_, failure := NewMirror(fixture.runner(), &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
		if failure == nil || failure.Code != "REPO-GIT-FILTER-ATTRIBUTE" {
			t.Fatalf("failure = %#v", failure)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("smudge filter ran: %v", err)
		}
	})
	t.Run("configured attributes file is neutralized", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		marker := filepath.Join(fixture.root, "configured-filter-executed")
		filter := filepath.Join(fixture.root, "configured-filter.sh")
		writeExecutable(t, filter, "#!/bin/sh\necho filter >> \""+marker+"\"\ncat\n")
		attributes := filepath.Join(fixture.root, "external-attributes")
		if err := os.WriteFile(attributes, []byte("*.md filter=evil\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, fixture.mirror, "config", "core.attributesFile", attributes)
		runGit(t, fixture.mirror, "config", "filter.evil.smudge", filter)
		_, failure := NewMirror(fixture.runner(), &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
		if failure != nil {
			t.Fatal(failure)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("configured smudge filter ran: %v", err)
		}
	})
	t.Run("implicit XDG attributes are neutralized", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		marker := filepath.Join(fixture.root, "xdg-filter-executed")
		filter := filepath.Join(fixture.root, "xdg-filter.sh")
		writeExecutable(t, filter, "#!/bin/sh\necho filter >> \""+marker+"\"\ncat\n")
		xdg := filepath.Join(fixture.root, "xdg")
		if err := os.MkdirAll(filepath.Join(xdg, "git"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(xdg, "git", "attributes"), []byte("*.md filter=evil\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("XDG_CONFIG_HOME", xdg)
		runGit(t, fixture.mirror, "config", "filter.evil.smudge", filter)
		_, failure := NewMirror(fixture.runner(), &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
		if failure != nil {
			t.Fatal(failure)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("implicit XDG smudge filter ran: %v", err)
		}
	})
}

func TestMirrorRollsBackInjectedPostPromotionFailure(t *testing.T) {
	fixture := newRepositoryFixture(t)
	advanceRemote(t, fixture, "candidate\n")
	beforeHead := runGit(t, fixture.mirror, "rev-parse", "HEAD")
	runner := &failAfterMergeRunner{delegate: fixture.runner()}
	_, failure := NewMirror(runner, &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
	if failure == nil || failure.Code != "REPO-MIRROR-PROMOTION" {
		t.Fatalf("failure = %#v", failure)
	}
	if got := runGit(t, fixture.mirror, "rev-parse", "HEAD"); got != beforeHead {
		t.Fatalf("rollback HEAD = %s, want %s", got, beforeHead)
	}
	if got := runGit(t, fixture.mirror, "show", "HEAD:README.md"); got != "initial" {
		t.Fatalf("rollback README = %q", got)
	}
	if got := runGit(t, fixture.mirror, "status", "--porcelain=v1"); got != "" {
		t.Fatalf("rollback left a dirty tree: %q", got)
	}
}

func TestMirrorPrivatePromotionFailurePreservesConcurrentIgnoredEdit(t *testing.T) {
	fixture := newRepositoryFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.mirror, ".git", "info", "exclude"), []byte("concurrent.tmp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	advanceRemote(t, fixture, "candidate\n")
	concurrentPath := filepath.Join(fixture.mirror, "concurrent.tmp")
	runner := &failAfterMergeRunner{delegate: fixture.runner(), editPath: concurrentPath}
	_, failure := NewMirror(runner, &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
	if failure == nil || failure.Code != "REPO-MIRROR-PROMOTION" {
		t.Fatalf("failure = %#v", failure)
	}
	contents, err := os.ReadFile(concurrentPath)
	if err != nil || string(contents) != "concurrent edit\n" {
		t.Fatalf("concurrent edit was not preserved: %q, %v", contents, err)
	}
}

func TestMirrorDivergenceFetchFailureTimeoutHoldAndRecovery(t *testing.T) {
	t.Run("divergence", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		if err := os.WriteFile(filepath.Join(fixture.mirror, "local.md"), []byte("local\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, fixture.mirror, "add", "local.md")
		runGit(t, fixture.mirror, "commit", "-m", "local divergence")
		advanceRemote(t, fixture, "remote divergence\n")
		_, failure := NewMirror(fixture.runner(), &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
		if failure == nil || failure.Code != "REPO-MIRROR-DIVERGED" {
			t.Fatalf("failure = %#v", failure)
		}
	})
	t.Run("fetch failure", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		runner := &failCommandRunner{delegate: fixture.runner(), command: "fetch"}
		_, failure := NewMirror(runner, &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
		if failure == nil || failure.Code != "REPO-FETCH-FAILED" {
			t.Fatalf("failure = %#v", failure)
		}
	})
	t.Run("validator timeout", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		advanceRemote(t, fixture, "timeout\n")
		validator := validatorFunc(func(context.Context, validation.Request) (validation.Report, error) {
			return validation.Report{}, context.DeadlineExceeded
		})
		_, failure := NewMirror(fixture.runner(), validator).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
		if failure == nil || failure.Code != validation.CodeUnsupported {
			t.Fatalf("failure = %#v", failure)
		}
	})
	t.Run("changed validator hold", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		advanceRemote(t, fixture, "hold\n")
		validator := validatorFunc(func(_ context.Context, request validation.Request) (validation.Report, error) {
			return validation.Report{
				SchemaVersion: validation.ReportSchemaVersion, Protocol: validation.ReportProtocol, ContractVersion: "2.0.0", Command: "validate",
				GeneratedAt: "2026-09-19T00:00:00Z", Verdict: validation.VerdictHold,
				Candidate: validation.Candidate{Kind: "git-tree", SourceID: request.Runtime.SourceID, ObjectID: request.Tree},
				Checks:    []validation.Check{{ID: validation.CodeBundleChanged, Status: "hold", Paths: []string{".agents/validators/example"}}},
			}, nil
		})
		_, failure := NewMirror(fixture.runner(), validator).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
		if failure == nil || failure.Code != validation.CodeBundleChanged {
			t.Fatalf("failure = %#v", failure)
		}
	})
	t.Run("recovery", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		advanceRemote(t, fixture, "recovered\n")
		before := runGit(t, fixture.mirror, "rev-parse", "HEAD")
		if _, failure := NewMirror(fixture.runner(), &recordingValidator{verdict: validation.VerdictFail}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror)); failure == nil {
			t.Fatal("initial validation failure was not surfaced")
		}
		if got := runGit(t, fixture.mirror, "rev-parse", "HEAD"); got != before {
			t.Fatalf("failed attempt moved HEAD to %s", got)
		}
		if _, failure := NewMirror(fixture.runner(), &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror)); failure != nil {
			t.Fatal(failure)
		}
		if got := runGit(t, fixture.mirror, "show", "HEAD:README.md"); got != "recovered" {
			t.Fatalf("recovery content = %q", got)
		}
	})
}

func TestMirrorRejectsDirtyWrongBranchAndOperationMarker(t *testing.T) {
	t.Run("dirty", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		if err := os.WriteFile(filepath.Join(fixture.mirror, "README.md"), []byte("dirty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, failure := NewMirror(fixture.runner(), &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
		if failure == nil || failure.Code != "REPO-GIT-DIRTY" {
			t.Fatalf("failure = %#v", failure)
		}
	})
	t.Run("ignored content", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		if err := os.WriteFile(filepath.Join(fixture.mirror, ".git", "info", "exclude"), []byte("ignored.tmp\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture.mirror, "ignored.tmp"), []byte("hidden\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, failure := NewMirror(fixture.runner(), &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
		if failure == nil || failure.Code != "REPO-GIT-DIRTY" {
			t.Fatalf("failure = %#v", failure)
		}
	})
	for name, flag := range map[string]string{"skip-worktree": "--skip-worktree", "assume-unchanged": "--assume-unchanged"} {
		t.Run(name, func(t *testing.T) {
			fixture := newRepositoryFixture(t)
			runGit(t, fixture.mirror, "update-index", flag, "README.md")
			_, failure := NewMirror(fixture.runner(), &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
			if failure == nil || failure.Code != "REPO-GIT-DIRTY" {
				t.Fatalf("failure = %#v", failure)
			}
		})
	}
	t.Run("branch", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		runGit(t, fixture.mirror, "switch", "-c", "feature")
		_, failure := NewMirror(fixture.runner(), &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
		if failure == nil || failure.Code != "REPO-GIT-BRANCH" {
			t.Fatalf("failure = %#v", failure)
		}
	})
	t.Run("operation", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		if err := os.WriteFile(filepath.Join(fixture.mirror, ".git", "index.lock"), []byte("locked"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, failure := NewMirror(fixture.runner(), &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
		if failure == nil || failure.Code != "REPO-GIT-OPERATION" {
			t.Fatalf("failure = %#v", failure)
		}
	})
	t.Run("sparse checkout", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		runGit(t, fixture.mirror, "config", "core.sparseCheckout", "true")
		_, failure := NewMirror(fixture.runner(), &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
		if failure == nil || failure.Code != "REPO-GIT-SPARSE-CHECKOUT" {
			t.Fatalf("failure = %#v", failure)
		}
	})
	t.Run("info attributes", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		path := filepath.Join(fixture.mirror, ".git", "info", "attributes")
		if err := os.WriteFile(path, []byte("*.md filter=evil\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, failure := NewMirror(fixture.runner(), &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
		if failure == nil || failure.Code != "REPO-GIT-EXTERNAL-ATTRIBUTES" {
			t.Fatalf("failure = %#v", failure)
		}
	})
	t.Run("local credential helper", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		marker := filepath.Join(fixture.root, "credential-helper-executed")
		runGit(t, fixture.mirror, "config", "credential.helper", "!echo executed > "+marker)
		_, failure := NewMirror(fixture.runner(), &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
		if failure == nil || failure.Code != "REPO-GIT-EXECUTABLE-CONFIG" {
			t.Fatalf("failure = %#v", failure)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("local credential helper executed: %v", err)
		}
	})
	t.Run("global credential helper remains available", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		globalHome := t.TempDir()
		if err := os.WriteFile(filepath.Join(globalHome, ".gitconfig"), []byte("[credential]\n\thelper = manager-approved\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", globalHome)
		if _, failure := NewMirror(fixture.runner(), &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror)); failure != nil {
			t.Fatalf("trusted global credential helper was rejected: %#v", failure)
		}
	})
	t.Run("multiple worktrees without worktree config remain inspectable", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		linked := filepath.Join(fixture.root, "linked-worktree")
		runGit(t, fixture.publisher, "worktree", "add", "--detach", linked, "HEAD")
		if failure := newBase(fixture.runner(), nil).validateRepository(context.Background(), fixture.target(fixture.publisher), "main"); failure != nil {
			t.Fatalf("repository without worktree-scoped config was rejected: %#v", failure)
		}
	})
	t.Run("worktree credential helper", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		marker := filepath.Join(fixture.root, "worktree-credential-helper-executed")
		runGit(t, fixture.mirror, "config", "extensions.worktreeConfig", "true")
		runGit(t, fixture.mirror, "config", "--worktree", "credential.helper", "!echo executed > "+marker)
		_, failure := NewMirror(fixture.runner(), &recordingValidator{}).syncTransactionForTest(context.Background(), fixture.target(fixture.mirror))
		if failure == nil || failure.Code != "REPO-GIT-EXECUTABLE-CONFIG" {
			t.Fatalf("failure = %#v", failure)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("worktree credential helper executed: %v", err)
		}
	})
}

func TestRepositoryValidationRejectsRemoteRewritesAndAlternateEndpoints(t *testing.T) {
	t.Run("URL rewrite", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		failure := newBase(fixture.systemRunner, nil).validateRepository(context.Background(), fixture.target(fixture.mirror), "main")
		if failure == nil || failure.Code != "REPO-GIT-EXECUTABLE-CONFIG" {
			t.Fatalf("failure = %#v", failure)
		}
	})
	t.Run("multiple fetch URLs", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		runGit(t, fixture.mirror, "remote", "set-url", "--add", "origin", "https://github.com/owner/other.git")
		failure := newBase(fixture.runner(), nil).validateRepository(context.Background(), fixture.target(fixture.mirror), "main")
		if failure == nil || failure.Code != "REPO-GIT-REMOTE" {
			t.Fatalf("failure = %#v", failure)
		}
	})
	t.Run("mismatched push URL", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		runGit(t, fixture.mirror, "remote", "set-url", "--push", "origin", "https://github.com/other/repo.git")
		failure := newBase(fixture.runner(), nil).validateRepository(context.Background(), fixture.target(fixture.mirror), "main")
		if failure == nil || failure.Code != "REPO-GIT-REMOTE" {
			t.Fatalf("failure = %#v", failure)
		}
	})
	for name, key := range map[string]string{
		"remote uploadpack":      "remote.origin.uploadpack",
		"SSH command":            "core.sshCommand",
		"alternate refs command": "core.alternateRefsCommand",
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newRepositoryFixture(t)
			runGit(t, fixture.mirror, "config", key, "malicious-command")
			failure := newBase(fixture.runner(), nil).validateRepository(context.Background(), fixture.target(fixture.mirror), "main")
			if failure == nil || failure.Code != "REPO-GIT-EXECUTABLE-CONFIG" {
				t.Fatalf("failure = %#v", failure)
			}
		})
	}
	t.Run("alternate object store", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		alternates := filepath.Join(fixture.mirror, ".git", "objects", "info", "alternates")
		if err := os.WriteFile(alternates, []byte(filepath.Join(fixture.publisher, ".git", "objects")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		failure := newBase(fixture.runner(), nil).validateRepository(context.Background(), fixture.target(fixture.mirror), "main")
		if failure == nil || failure.Code != "REPO-GIT-OBJECTS" {
			t.Fatalf("failure = %#v", failure)
		}
	})
	t.Run("shallow repository", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		head := runGit(t, fixture.mirror, "rev-parse", "HEAD")
		if err := os.WriteFile(filepath.Join(fixture.mirror, ".git", "shallow"), []byte(head+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		failure := newBase(fixture.runner(), nil).validateRepository(context.Background(), fixture.target(fixture.mirror), "main")
		if failure == nil || failure.Code != "REPO-GIT-OBJECTS" {
			t.Fatalf("failure = %#v", failure)
		}
	})
}

type repositoryFixture struct {
	root         string
	bare         string
	publisher    string
	mirror       string
	spec         discovery.RepositorySpec
	systemRunner SystemRunner
}

func newRepositoryFixture(t *testing.T) repositoryFixture {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(root, "remote.git")
	publisher := filepath.Join(root, "publisher")
	mirror := filepath.Join(root, "mirror")
	runGit(t, root, "init", "--bare", "--initial-branch=main", bare)
	runGit(t, root, "init", "--initial-branch=main", publisher)
	configureRepository(t, publisher, bare)
	if err := os.WriteFile(filepath.Join(publisher, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, publisher, "add", "README.md")
	runGit(t, publisher, "commit", "-m", "initial")
	runGit(t, publisher, "push", "-u", "origin", "main")
	runGit(t, root, "init", "--initial-branch=main", mirror)
	configureRepository(t, mirror, bare)
	runGit(t, mirror, "fetch", "origin", "main")
	runGit(t, mirror, "reset", "--hard", "FETCH_HEAD")
	spec, err := discovery.ParseGitHubBranchURL("https://github.com/owner/repo/tree/main")
	if err != nil {
		t.Fatal(err)
	}
	initializeManagedMirrorFixture(t, mirror, spec)
	return repositoryFixture{root: root, bare: bare, publisher: publisher, mirror: mirror, spec: spec, systemRunner: trustedTestSystemRunner(t)}
}

func initializeManagedMirrorFixture(t *testing.T, mirror string, spec discovery.RepositorySpec) {
	t.Helper()
	target := Target{ID: "repo-main", Path: mirror, Spec: spec}
	layout, err := deriveMirrorLayout(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.generations, 0o700); err != nil {
		t.Fatal(err)
	}
	id, err := newMirrorTransactionID()
	if err != nil {
		t.Fatal(err)
	}
	generation := layout.generationPath("generation-" + id)
	if err := os.Rename(mirror, generation); err != nil {
		t.Fatalf("move initial mirror generation: %v", err)
	}
	if err := writeMirrorGenerationMarker(generation, id); err != nil {
		t.Fatalf("mark initial mirror generation: %v", err)
	}
	if err := (systemMirrorPointerBackend{}).Create(mirror, generation); err != nil {
		t.Fatalf("create initial mirror pointer: %v", err)
	}
}

func configureRepository(t *testing.T, repository, bare string) {
	t.Helper()
	runGit(t, repository, "config", "user.name", "Repo Sync Test")
	runGit(t, repository, "config", "user.email", "repo-sync@example.invalid")
	runGit(t, repository, "config", "protocol.file.allow", "always")
	localURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(bare)}).String()
	runGit(t, repository, "config", "url."+localURL+".insteadOf", "https://github.com/owner/repo.git")
	runGit(t, repository, "remote", "add", "origin", "https://github.com/owner/repo.git")
}

func (f repositoryFixture) target(path string) Target {
	return Target{ID: "repo-main", Path: path, Spec: f.spec, ValidationRuntime: validation.Runtime{SourceID: "example.source"}}
}

func (f repositoryFixture) runner() Runner { return localFixtureRunner{delegate: f.systemRunner} }

var (
	testSystemRunnerOnce sync.Once
	testSystemRunner     SystemRunner
	testSystemRunnerErr  error
)

func trustedTestSystemRunner(t *testing.T) SystemRunner {
	t.Helper()
	testSystemRunnerOnce.Do(func() {
		path, err := exec.LookPath("git")
		if err != nil {
			testSystemRunnerErr = err
			return
		}
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			testSystemRunnerErr = err
			return
		}
		path, err = filepath.Abs(path)
		if err != nil {
			testSystemRunnerErr = err
			return
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			testSystemRunnerErr = err
			return
		}
		digest := sha256.Sum256(contents)
		style := "posix"
		if runtime.GOOS == "windows" {
			style = "windows"
		}
		registry, err := json.Marshal(map[string]any{
			"schema_version": 2, "contract_version": "2.0.0",
			"local_dependencies": []any{map[string]any{
				"id": "context-system.git", "path": map[string]any{"style": style, "value": path},
				"sha256": hex.EncodeToString(digest[:]), "kind": "executable",
			}},
		})
		if err != nil {
			testSystemRunnerErr = err
			return
		}
		testSystemRunner, testSystemRunnerErr = NewSystemRunnerFromRegistry(registry)
	})
	if testSystemRunnerErr != nil {
		t.Fatal(testSystemRunnerErr)
	}
	return testSystemRunner
}

// localFixtureRunner is restricted to tests: it permits the fixture's local
// file transport mapping while hiding that one deliberate URL rewrite from
// production's rejection check. Production always uses SystemRunner directly.
type localFixtureRunner struct{ delegate SystemRunner }

func (r localFixtureRunner) Run(ctx context.Context, dir string, args ...string) Result {
	if fixtureRewriteInspection(args) {
		return Result{Err: errors.New("not configured"), ExitCode: 1}
	}
	return r.delegate.Run(ctx, dir, append([]string{"-c", "protocol.file.allow=always"}, args...)...)
}

func (r localFixtureRunner) RunEnv(ctx context.Context, dir string, environment map[string]string, args ...string) Result {
	if fixtureRewriteInspection(args) {
		return Result{Err: errors.New("not configured"), ExitCode: 1}
	}
	return r.delegate.RunEnv(ctx, dir, environment, append([]string{"-c", "protocol.file.allow=always"}, args...)...)
}

func (r localFixtureRunner) RunInput(ctx context.Context, dir string, input []byte, environment map[string]string, args ...string) Result {
	if fixtureRewriteInspection(args) {
		return Result{Err: errors.New("not configured"), ExitCode: 1}
	}
	return r.delegate.RunInput(ctx, dir, input, environment, append([]string{"-c", "protocol.file.allow=always"}, args...)...)
}

func fixtureRewriteInspection(args []string) bool {
	for index, arg := range args {
		if arg == "--get-regexp" && index+1 < len(args) && strings.HasPrefix(args[index+1], "^url\\.") {
			return true
		}
	}
	return false
}

func advanceRemote(t *testing.T, fixture repositoryFixture, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fixture.publisher, "README.md"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, fixture.publisher, "add", "README.md")
	runGit(t, fixture.publisher, "commit", "-m", "remote update")
	runGit(t, fixture.publisher, "push", "origin", "main")
}

type recordingValidator struct {
	requests []validation.Request
	verdict  validation.Verdict
}

func (v *recordingValidator) Validate(_ context.Context, request validation.Request) (validation.Report, error) {
	v.requests = append(v.requests, request)
	verdict := v.verdict
	if verdict == "" {
		verdict = validation.VerdictPass
	}
	return reportForRequest(request, verdict), nil
}

func reportForRequest(request validation.Request, verdict validation.Verdict) validation.Report {
	status := string(verdict)
	if verdict == validation.VerdictIncomplete {
		status = "skip"
	}
	return validation.Report{
		SchemaVersion: validation.ReportSchemaVersion, Protocol: validation.ReportProtocol, ContractVersion: "2.0.0", Command: "validate",
		GeneratedAt: "2026-09-19T00:00:00Z", Verdict: verdict, Promotable: verdict == validation.VerdictPass,
		Candidate:        validation.Candidate{Kind: "git-tree", SourceID: request.Runtime.SourceID, ObjectID: request.Tree},
		Checks:           []validation.Check{{ID: "CTX-SCHEMA-" + strings.ToUpper(status), Status: status, Paths: []string{}}},
		ValidatorResults: []validation.ValidatorResult{}, LinkActions: []validation.LinkAction{},
	}
}

type recordingRunner struct {
	delegate Runner
	calls    [][]string
}

func (r *recordingRunner) RunInput(ctx context.Context, dir string, input []byte, environment map[string]string, args ...string) Result {
	r.calls = append(r.calls, append([]string(nil), args...))
	if delegate, ok := r.delegate.(InputRunner); ok {
		return delegate.RunInput(ctx, dir, input, environment, args...)
	}
	return Result{Err: errors.New("delegate has no input runner"), ExitCode: -1}
}

func (r *recordingRunner) Run(ctx context.Context, dir string, args ...string) Result {
	r.calls = append(r.calls, append([]string(nil), args...))
	return r.delegate.Run(ctx, dir, args...)
}

func (r *recordingRunner) RunEnv(ctx context.Context, dir string, environment map[string]string, args ...string) Result {
	r.calls = append(r.calls, append([]string(nil), args...))
	if delegate, ok := r.delegate.(EnvironmentRunner); ok {
		return delegate.RunEnv(ctx, dir, environment, args...)
	}
	return Result{Err: errors.New("delegate has no environment runner"), ExitCode: -1}
}

type failAfterMergeRunner struct {
	delegate Runner
	failed   bool
	editPath string
}

func (r *failAfterMergeRunner) Run(ctx context.Context, dir string, args ...string) Result {
	result := r.delegate.Run(ctx, dir, args...)
	if !r.failed && gitSubcommand(args) == "update-ref" && containsArgument(args, "HEAD") && result.Err == nil {
		r.failed = true
		if r.editPath != "" {
			if err := os.WriteFile(r.editPath, []byte("concurrent edit\n"), 0o600); err != nil {
				return Result{Err: err, ExitCode: -1}
			}
		}
		result.Err = errors.New("injected failure after Git completed promotion")
		result.ExitCode = 1
	}
	return result
}

type failCommandRunner struct {
	delegate Runner
	command  string
}

func (r *failCommandRunner) Run(ctx context.Context, dir string, args ...string) Result {
	if gitSubcommand(args) == r.command {
		return Result{Err: errors.New("injected command failure"), ExitCode: 1}
	}
	return r.delegate.Run(ctx, dir, args...)
}

func (r *failCommandRunner) RunEnv(ctx context.Context, dir string, environment map[string]string, args ...string) Result {
	if gitSubcommand(args) == r.command {
		return Result{Err: errors.New("injected command failure"), ExitCode: 1}
	}
	if delegate, ok := r.delegate.(EnvironmentRunner); ok {
		return delegate.RunEnv(ctx, dir, environment, args...)
	}
	return Result{Err: errors.New("delegate has no environment runner"), ExitCode: -1}
}

func (r *failAfterMergeRunner) RunEnv(ctx context.Context, dir string, environment map[string]string, args ...string) Result {
	delegate, ok := r.delegate.(EnvironmentRunner)
	if !ok {
		return Result{Err: errors.New("delegate has no environment runner"), ExitCode: -1}
	}
	result := delegate.RunEnv(ctx, dir, environment, args...)
	if !r.failed && gitSubcommand(args) == "update-ref" && containsArgument(args, "HEAD") && result.Err == nil {
		r.failed = true
		if r.editPath != "" {
			if err := os.WriteFile(r.editPath, []byte("concurrent edit\n"), 0o600); err != nil {
				return Result{Err: err, ExitCode: -1}
			}
		}
		result.Err = errors.New("injected failure after Git completed promotion")
		result.ExitCode = 1
	}
	return result
}

type afterCommandRunner struct {
	delegate Runner
	command  string
	after    func() error
	fired    bool
}

func (r *afterCommandRunner) afterCommand(args []string, result Result) Result {
	if !r.fired && gitSubcommand(args) == r.command && result.Err == nil {
		r.fired = true
		if err := r.after(); err != nil {
			return Result{Err: err, ExitCode: -1}
		}
	}
	return result
}

func (r *afterCommandRunner) Run(ctx context.Context, dir string, args ...string) Result {
	return r.afterCommand(args, r.delegate.Run(ctx, dir, args...))
}

func (r *afterCommandRunner) RunEnv(ctx context.Context, dir string, environment map[string]string, args ...string) Result {
	delegate, ok := r.delegate.(EnvironmentRunner)
	if !ok {
		return Result{Err: errors.New("delegate has no environment runner"), ExitCode: -1}
	}
	return r.afterCommand(args, delegate.RunEnv(ctx, dir, environment, args...))
}

func (r *afterCommandRunner) RunInput(ctx context.Context, dir string, input []byte, environment map[string]string, args ...string) Result {
	delegate, ok := r.delegate.(InputRunner)
	if !ok {
		return Result{Err: errors.New("delegate has no input runner"), ExitCode: -1}
	}
	return r.afterCommand(args, delegate.RunInput(ctx, dir, input, environment, args...))
}

func containsArgument(args []string, wanted string) bool {
	for _, arg := range args {
		if arg == wanted {
			return true
		}
	}
	return false
}

func gitSubcommand(args []string) string {
	for index := 0; index < len(args); index++ {
		if args[index] == "-c" && index+1 < len(args) {
			index++
			continue
		}
		if !strings.HasPrefix(args[index], "-") {
			return args[index]
		}
	}
	return ""
}

func readIndex(t *testing.T, repository string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repository, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{
		"-c", "core.longpaths=true",
		"-c", "core.autocrlf=false",
		"-c", "core.safecrlf=false",
	}, args...)
	command := exec.Command("git", commandArgs...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
