package gitops

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/JorgeMuehlebach/repo-sync/internal/validation"
)

func TestMirrorGenericSyncFailsClosedBeforeMutation(t *testing.T) {
	fixture := newRepositoryFixture(t)
	advanceRemote(t, fixture, "candidate must remain private\n")
	target := fixture.target(fixture.mirror)
	mirror := NewMirror(fixture.runner(), &recordingValidator{})
	before, err := mirror.pointers.Resolve(target.Path)
	if err != nil {
		t.Fatal(err)
	}

	outcome, failure := mirror.Sync(context.Background(), target)
	if failure == nil || failure.Code != "REPO-MIRROR-TRANSACTION-REQUIRED" || !reflect.DeepEqual(outcome, Outcome{}) {
		t.Fatalf("generic mirror sync = %#v, %#v", outcome, failure)
	}
	after, err := mirror.pointers.Resolve(target.Path)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("generic mirror sync moved pointer from %q to %q", before, after)
	}
	if recovery, recoveryFailure := mirror.InspectRecovery(context.Background(), target); recoveryFailure != nil || recovery.Pending {
		t.Fatalf("generic mirror sync left recovery state: %#v, %#v", recovery, recoveryFailure)
	}
}

func TestMirrorPromotionRequiresExplicitFinalization(t *testing.T) {
	fixture := newRepositoryFixture(t)
	advanceRemote(t, fixture, "promoted\n")
	beforeCommit := runGit(t, fixture.mirror, "rev-parse", "HEAD")
	target := fixture.target(fixture.mirror)
	target.ExpectedAcceptedCommit = beforeCommit
	mirror := NewMirror(fixture.runner(), &recordingValidator{})

	result, failure := mirror.SyncTransaction(context.Background(), target)
	if failure != nil {
		t.Fatal(failure)
	}
	if !mirrorIDPattern.MatchString(result.TransactionID) || result.Outcome.AcceptedCommit == beforeCommit {
		t.Fatalf("promotion result = %#v", result)
	}
	recovery := requireMirrorRecovery(t, mirror, target)
	if recovery.JournalState != mirrorJournalPromoted || recovery.ActiveGeneration != "candidate" || recovery.TransactionID != result.TransactionID || recovery.CandidateCommit != result.Outcome.AcceptedCommit || recovery.CandidateTree != result.Outcome.AcceptedTree {
		t.Fatalf("recovery = %#v", recovery)
	}
	blocked, blockedFailure := mirror.syncTransactionForTest(context.Background(), target)
	if blockedFailure == nil || blockedFailure.Code != "REPO-MIRROR-RECOVERY-REQUIRED" || blocked.AcceptedCommit != beforeCommit {
		t.Fatalf("blocked sync = %#v, %#v", blocked, blockedFailure)
	}
	if failure := mirror.Finalize(context.Background(), target, result.Outcome, result.TransactionID); failure != nil {
		t.Fatal(failure)
	}
	if recovery, failure := mirror.InspectRecovery(context.Background(), target); failure != nil || recovery.Pending {
		t.Fatalf("recovery after finalize = %#v, %#v", recovery, failure)
	}

	target.ExpectedAcceptedCommit = result.Outcome.AcceptedCommit
	noOp, failure := mirror.SyncTransaction(context.Background(), target)
	if failure != nil || noOp.TransactionID != "" || noOp.Outcome.AcceptedCommit != result.Outcome.AcceptedCommit {
		t.Fatalf("post-finalize sync = %#v, %#v", noOp, failure)
	}
}

func TestMirrorRollbackRequiresDurableAcknowledgement(t *testing.T) {
	fixture := newRepositoryFixture(t)
	advanceRemote(t, fixture, "candidate\n")
	beforeCommit := runGit(t, fixture.mirror, "rev-parse", "HEAD")
	beforeTree := runGit(t, fixture.mirror, "rev-parse", "HEAD^{tree}")
	target := fixture.target(fixture.mirror)
	target.ExpectedAcceptedCommit = beforeCommit
	mirror := NewMirror(fixture.runner(), &recordingValidator{})
	result, failure := mirror.SyncTransaction(context.Background(), target)
	if failure != nil {
		t.Fatal(failure)
	}

	restored, rollbackFailure := mirror.Rollback(context.Background(), target, result.Outcome, result.TransactionID)
	if rollbackFailure != nil {
		t.Fatal(rollbackFailure)
	}
	if restored.AcceptedCommit != beforeCommit || restored.AcceptedTree != beforeTree || runGit(t, fixture.mirror, "rev-parse", "HEAD") != beforeCommit {
		t.Fatalf("restored outcome = %#v", restored)
	}
	recovery := requireMirrorRecovery(t, mirror, target)
	if recovery.JournalState != mirrorJournalRollback || recovery.ActiveGeneration != "baseline" {
		t.Fatalf("rollback recovery = %#v", recovery)
	}
	if _, failure := mirror.syncTransactionForTest(context.Background(), target); failure == nil || failure.Code != "REPO-MIRROR-RECOVERY-REQUIRED" {
		t.Fatalf("sync did not stop for rollback acknowledgement: %#v", failure)
	}
	if failure := mirror.AcknowledgeRollback(context.Background(), target, result.TransactionID); failure != nil {
		t.Fatal(failure)
	}
	if recovery, failure := mirror.InspectRecovery(context.Background(), target); failure != nil || recovery.Pending {
		t.Fatalf("recovery after acknowledgement = %#v, %#v", recovery, failure)
	}
}

func TestMirrorPreparedJournalRecoversAcrossRestart(t *testing.T) {
	t.Run("candidate status completed", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		advanceRemote(t, fixture, "candidate\n")
		target := fixture.target(fixture.mirror)
		target.ExpectedAcceptedCommit = runGit(t, fixture.mirror, "rev-parse", "HEAD")
		mirror := NewMirror(fixture.runner(), &recordingValidator{})
		result, failure := mirror.SyncTransaction(context.Background(), target)
		if failure != nil {
			t.Fatal(failure)
		}
		layout, journal := requireMirrorJournal(t, target)
		journal.State = mirrorJournalPrepared
		if err := writeMirrorJournal(layout, journal); err != nil {
			t.Fatal(err)
		}

		restarted := NewMirror(fixture.runner(), &recordingValidator{})
		recovery := requireMirrorRecovery(t, restarted, target)
		if recovery.JournalState != mirrorJournalPrepared || recovery.ActiveGeneration != "candidate" {
			t.Fatalf("restart recovery = %#v", recovery)
		}
		if failure := restarted.Finalize(context.Background(), target, result.Outcome, result.TransactionID); failure != nil {
			t.Fatal(failure)
		}
	})

	t.Run("candidate status absent", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		advanceRemote(t, fixture, "candidate\n")
		target := fixture.target(fixture.mirror)
		target.ExpectedAcceptedCommit = runGit(t, fixture.mirror, "rev-parse", "HEAD")
		mirror := NewMirror(fixture.runner(), &recordingValidator{})
		result, failure := mirror.SyncTransaction(context.Background(), target)
		if failure != nil {
			t.Fatal(failure)
		}
		layout, journal := requireMirrorJournal(t, target)
		candidatePath := layout.generationPath(journal.Candidate.Name)
		baselinePath := layout.generationPath(journal.Baseline.Name)
		if err := mirror.pointers.Replace(target.Path, candidatePath, baselinePath); err != nil {
			t.Fatal(err)
		}
		journal.State = mirrorJournalPrepared
		if err := writeMirrorJournal(layout, journal); err != nil {
			t.Fatal(err)
		}

		restarted := NewMirror(fixture.runner(), &recordingValidator{})
		recovery := requireMirrorRecovery(t, restarted, target)
		if recovery.JournalState != mirrorJournalPrepared || recovery.ActiveGeneration != "baseline" {
			t.Fatalf("restart recovery = %#v", recovery)
		}
		if _, failure := restarted.Rollback(context.Background(), target, result.Outcome, result.TransactionID); failure != nil {
			t.Fatal(failure)
		}
		if failure := restarted.AcknowledgeRollback(context.Background(), target, result.TransactionID); failure != nil {
			t.Fatal(failure)
		}
	})
}

func TestMirrorAmbiguousPointerFailureRestoresLastKnownGood(t *testing.T) {
	fixture := newRepositoryFixture(t)
	advanceRemote(t, fixture, "candidate\n")
	before := runGit(t, fixture.mirror, "rev-parse", "HEAD")
	target := fixture.target(fixture.mirror)
	target.ExpectedAcceptedCommit = before
	pointers := &replaceThenFailPointer{delegate: systemMirrorPointerBackend{}}
	mirror := NewMirror(fixture.runner(), &recordingValidator{})
	mirror.pointers = pointers

	result, failure := mirror.SyncTransaction(context.Background(), target)
	if failure == nil || failure.Code != "REPO-MIRROR-ATOMIC-SWAP" || !mirrorIDPattern.MatchString(result.TransactionID) {
		t.Fatalf("promotion failure = %#v, result = %#v", failure, result)
	}
	if got := runGit(t, fixture.mirror, "rev-parse", "HEAD"); got != before {
		t.Fatalf("ambiguous replacement exposed %s, want %s", got, before)
	}
	recovery := requireMirrorRecovery(t, mirror, target)
	if recovery.JournalState != mirrorJournalRollback || recovery.ActiveGeneration != "baseline" {
		t.Fatalf("ambiguous replacement recovery = %#v", recovery)
	}
}

func TestMirrorRejectedCandidatesDoNotGrowManagedStorage(t *testing.T) {
	fixture := newRepositoryFixture(t)
	advanceRemote(t, fixture, "rejected\n")
	target := fixture.target(fixture.mirror)
	target.ExpectedAcceptedCommit = runGit(t, fixture.mirror, "rev-parse", "HEAD")
	mirror := NewMirror(fixture.runner(), &recordingValidator{verdict: validation.VerdictFail})
	layout, err := deriveMirrorLayout(target)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 5; attempt++ {
		if _, failure := mirror.syncTransactionForTest(context.Background(), target); failure == nil || failure.Code != "REPO-VALIDATION-FAILED" {
			t.Fatalf("attempt %d failure = %#v", attempt, failure)
		}
		entries, err := os.ReadDir(layout.generations)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name()[:len("generation-")] != "generation-" {
			t.Fatalf("attempt %d left private generations: %#v", attempt, entryNames(entries))
		}
	}
}

func TestMirrorRetainsPreviouslyExposedGenerationForConcurrentReader(t *testing.T) {
	fixture := newRepositoryFixture(t)
	target := fixture.target(fixture.mirror)
	target.ExpectedAcceptedCommit = runGit(t, fixture.mirror, "rev-parse", "HEAD")
	mirror := NewMirror(fixture.runner(), &recordingValidator{})

	advanceRemote(t, fixture, "first accepted generation\n")
	first, failure := mirror.SyncTransaction(context.Background(), target)
	if failure != nil {
		t.Fatal(failure)
	}
	if failure := mirror.Finalize(context.Background(), target, first.Outcome, first.TransactionID); failure != nil {
		t.Fatal(failure)
	}
	retainedGeneration, err := mirror.pointers.Resolve(target.Path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(filepath.Join(retainedGeneration, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	target.ExpectedAcceptedCommit = first.Outcome.AcceptedCommit
	advanceRemote(t, fixture, "second accepted generation\n")
	second, failure := mirror.SyncTransaction(context.Background(), target)
	if failure != nil {
		t.Fatal(failure)
	}
	if failure := mirror.Finalize(context.Background(), target, second.Outcome, second.TransactionID); failure != nil {
		t.Fatal(failure)
	}
	target.ExpectedAcceptedCommit = second.Outcome.AcceptedCommit
	if result, failure := mirror.SyncTransaction(context.Background(), target); failure != nil || result.TransactionID != "" {
		t.Fatalf("follow-up cleanup sync = %#v, %#v", result, failure)
	}

	contents := make([]byte, 128)
	count, err := reader.Read(contents)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(contents[:count]); got != "first accepted generation\n" {
		t.Fatalf("retained reader saw %q", got)
	}
	if info, err := os.Stat(retainedGeneration); err != nil || !info.IsDir() {
		t.Fatalf("previously exposed generation was removed: %#v, %v", info, err)
	}
}

func TestInitializeMirrorCreatesIndependentStableLayout(t *testing.T) {
	fixture := newRepositoryFixture(t)
	path := filepath.Join(fixture.root, "bootstrapped-mirror")
	localURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(fixture.bare)}).String()
	runner := cloneRewriteRunner{delegate: fixture.runner(), localURL: localURL, canonicalURL: fixture.spec.CloneURL}
	target := Target{ID: "bootstrap-main", Path: path, Spec: fixture.spec}

	outcome, failure := InitializeMirror(context.Background(), runner, target)
	if failure != nil {
		t.Fatal(failure)
	}
	if outcome.AcceptedCommit != "" || outcome.CandidateCommit == "" || outcome.CandidateTree == "" {
		t.Fatalf("bootstrap outcome = %#v", outcome)
	}
	if !isMirrorPointer(path) {
		t.Fatal("bootstrap path is not a managed pointer")
	}
	layout, err := deriveMirrorLayout(target)
	if err != nil {
		t.Fatal(err)
	}
	active, err := (systemMirrorPointerBackend{}).Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	name, ok := layout.directGenerationName(active)
	if !ok || name[:len("generation-")] != "generation-" {
		t.Fatalf("bootstrap generation = %q", active)
	}
	wantID, err := mirrorGenerationIDFromPath(active)
	if err != nil {
		t.Fatal(err)
	}
	if markerID, err := readMirrorGenerationMarker(active); err != nil || markerID != wantID {
		t.Fatalf("bootstrap generation identity = %q, want %q: %v", markerID, wantID, err)
	}
	if markerID, err := readMirrorGenerationMarker(active); err != nil || name != "generation-"+markerID {
		t.Fatalf("bootstrap marker/path contract = %q / %q: %v", name, markerID, err)
	}
	wrongID, err := newMirrorTransactionID()
	if err != nil {
		t.Fatal(err)
	}
	for wrongID == wantID {
		wrongID, err = newMirrorTransactionID()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := writeMirrorGenerationMarker(active, wrongID); err == nil {
		t.Fatal("generation marker writer accepted an identity that did not match the directory suffix")
	}
	if markerID, err := readMirrorGenerationMarker(active); err != nil || markerID != wantID {
		t.Fatalf("rejected marker write changed identity = %q, want %q: %v", markerID, wantID, err)
	}
	if _, failure := InitializeMirror(context.Background(), runner, target); failure == nil || failure.Code != "REPO-MIRROR-LAYOUT" {
		t.Fatalf("repeat bootstrap failure = %#v", failure)
	}

	realPath := filepath.Join(fixture.root, "existing-real-mirror")
	if err := os.Mkdir(realPath, 0o700); err != nil {
		t.Fatal(err)
	}
	realTarget := target
	realTarget.ID = "existing-real"
	realTarget.Path = realPath
	if _, failure := InitializeMirror(context.Background(), runner, realTarget); failure == nil || failure.Code != "REPO-MIRROR-LAYOUT" {
		t.Fatalf("real directory bootstrap failure = %#v", failure)
	}
}

func TestMirrorRejectsGenerationMarkerThatDoesNotMatchDirectory(t *testing.T) {
	fixture := newRepositoryFixture(t)
	target := fixture.target(fixture.mirror)
	mirror := NewMirror(fixture.runner(), &recordingValidator{})
	active, err := mirror.pointers.Resolve(target.Path)
	if err != nil {
		t.Fatal(err)
	}
	wantID, err := mirrorGenerationIDFromPath(active)
	if err != nil {
		t.Fatal(err)
	}
	wrongID, err := newMirrorTransactionID()
	if err != nil {
		t.Fatal(err)
	}
	for wrongID == wantID {
		wrongID, err = newMirrorTransactionID()
		if err != nil {
			t.Fatal(err)
		}
	}
	markerPath := filepath.Join(active, ".git", mirrorGenerationMarkerName)
	marker := []byte("{\"schema_version\":1,\"generation_id\":\"" + wrongID + "\"}\n")
	if err := os.WriteFile(markerPath, marker, 0o600); err != nil {
		t.Fatal(err)
	}

	failure := mirror.ValidateTarget(context.Background(), target)
	if failure == nil || failure.Code != "REPO-MIRROR-GENERATION" {
		t.Fatalf("mismatched marker validation failure = %#v", failure)
	}
}

func requireMirrorRecovery(t *testing.T, mirror Mirror, target Target) MirrorRecovery {
	t.Helper()
	recovery, failure := mirror.InspectRecovery(context.Background(), target)
	if failure != nil || !recovery.Pending {
		t.Fatalf("recovery = %#v, %#v", recovery, failure)
	}
	return recovery
}

func requireMirrorJournal(t *testing.T, target Target) (mirrorLayout, mirrorJournal) {
	t.Helper()
	layout, err := deriveMirrorLayout(target)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := readMirrorJournal(layout)
	if err != nil || journal == nil {
		t.Fatalf("journal = %#v, %v", journal, err)
	}
	return layout, *journal
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

type replaceThenFailPointer struct {
	delegate mirrorPointerBackend
	fired    bool
}

func (p *replaceThenFailPointer) Resolve(path string) (string, error) {
	return p.delegate.Resolve(path)
}

func (p *replaceThenFailPointer) Create(path, target string) error {
	return p.delegate.Create(path, target)
}

func (p *replaceThenFailPointer) Replace(path, expected, next string) error {
	if err := p.delegate.Replace(path, expected, next); err != nil {
		return err
	}
	if !p.fired {
		p.fired = true
		return &mirrorAtomicPointerError{err: errors.New("injected ambiguous replacement result")}
	}
	return nil
}

type cloneRewriteRunner struct {
	delegate     Runner
	localURL     string
	canonicalURL string
}

func (r cloneRewriteRunner) arguments(args []string) []string {
	if gitSubcommand(args) != "clone" {
		return args
	}
	rewritten := []string{"-c", "url." + r.localURL + ".insteadOf=" + r.canonicalURL}
	return append(rewritten, args...)
}

func (r cloneRewriteRunner) Run(ctx context.Context, dir string, args ...string) Result {
	return r.delegate.Run(ctx, dir, r.arguments(args)...)
}

func (r cloneRewriteRunner) RunEnv(ctx context.Context, dir string, environment map[string]string, args ...string) Result {
	delegate, ok := r.delegate.(EnvironmentRunner)
	if !ok {
		return Result{Err: errors.New("delegate has no environment runner"), ExitCode: -1}
	}
	return delegate.RunEnv(ctx, dir, environment, r.arguments(args)...)
}

func (r cloneRewriteRunner) RunInput(ctx context.Context, dir string, input []byte, environment map[string]string, args ...string) Result {
	delegate, ok := r.delegate.(InputRunner)
	if !ok {
		return Result{Err: errors.New("delegate has no input runner"), ExitCode: -1}
	}
	return delegate.RunInput(ctx, dir, input, environment, r.arguments(args)...)
}
