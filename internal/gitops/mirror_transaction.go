package gitops

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/JorgeMuehlebach/repo-sync/internal/atomicfile"
	"github.com/JorgeMuehlebach/repo-sync/internal/securefile"
	"github.com/JorgeMuehlebach/repo-sync/internal/strictjson"
	"github.com/JorgeMuehlebach/repo-sync/internal/validation"
)

const (
	mirrorJournalSchemaVersion = 1
	mirrorJournalPrepared      = "prepared"
	mirrorJournalPromoted      = "promoted"
	mirrorJournalRollback      = "rollback-pending"
	mirrorJournalLimit         = 64 << 10
	mirrorGenerationFileLimit  = 200000
	mirrorGenerationByteLimit  = int64(8 << 30)
	mirrorGenerationMarkerName = "repo-sync-generation.json"
)

var (
	mirrorIDPattern         = regexp.MustCompile(`^[a-f0-9]{32}$`)
	mirrorObjectIDPattern   = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
	mirrorGenerationPattern = regexp.MustCompile(`^(?:generation|candidate)-[a-f0-9]{32}$`)
)

type mirrorLayout struct {
	exposedRoot string
	storageRoot string
	generations string
	journalPath string
}

type mirrorGeneration struct {
	Name   string
	Path   string
	ID     string
	Commit string
	Tree   string
}

type mirrorGenerationMarker struct {
	SchemaVersion int    `json:"schema_version"`
	GenerationID  string `json:"generation_id"`
}

type mirrorJournalGeneration struct {
	Name               string `json:"name"`
	ID                 string `json:"id"`
	Commit             string `json:"commit"`
	Tree               string `json:"tree"`
	PreviouslyAccepted bool   `json:"previously_accepted,omitempty"`
}

type mirrorJournal struct {
	SchemaVersion int                     `json:"schema_version"`
	TransactionID string                  `json:"transaction_id"`
	RepositoryID  string                  `json:"repository_id"`
	State         string                  `json:"state"`
	Baseline      mirrorJournalGeneration `json:"baseline"`
	Candidate     mirrorJournalGeneration `json:"candidate"`
}

// MirrorRecovery is a sanitized in-process recovery decision input. The app
// must compare CandidateCommit and CandidateTree with a reopened, schema-valid
// accepted status record before calling Finalize. Private state alone is not a
// completion barrier.
type MirrorRecovery struct {
	Pending             bool
	TransactionID       string
	JournalState        string
	ActiveGeneration    string
	BaselineCommit      string
	BaselineTree        string
	CandidateCommit     string
	CandidateTree       string
	BaselineWasAccepted bool
}

func (r MirrorRecovery) baselineOutcome() Outcome {
	result := Outcome{CandidateCommit: r.CandidateCommit, CandidateTree: r.CandidateTree}
	if r.BaselineWasAccepted {
		result.AcceptedCommit = r.BaselineCommit
		result.AcceptedTree = r.BaselineTree
	}
	return result
}

func (j mirrorJournal) baselineOutcome(report validation.Report) Outcome {
	result := Outcome{CandidateCommit: j.Candidate.Commit, CandidateTree: j.Candidate.Tree, Validation: report}
	if j.Baseline.PreviouslyAccepted {
		result.AcceptedCommit = j.Baseline.Commit
		result.AcceptedTree = j.Baseline.Tree
	}
	return result
}

func deriveMirrorLayout(target Target) (mirrorLayout, error) {
	absolute, err := filepath.Abs(target.Path)
	if target.ID == "" || target.Path == "" || !filepath.IsAbs(target.Path) || err != nil || !sameMirrorPath(filepath.Clean(target.Path), absolute) {
		return mirrorLayout{}, fmt.Errorf("mirror target is not absolute and canonical")
	}
	parent := filepath.Dir(target.Path)
	if parent == target.Path {
		return mirrorLayout{}, fmt.Errorf("mirror target has no managed parent")
	}
	digest := sha256.Sum256([]byte(target.ID + "\x00" + target.Path))
	storage := filepath.Join(parent, ".repo-sync-mirror-"+hex.EncodeToString(digest[:12]))
	return mirrorLayout{
		exposedRoot: target.Path,
		storageRoot: storage,
		generations: filepath.Join(storage, "generations"),
		journalPath: filepath.Join(storage, "promotion.json"),
	}, nil
}

func (l mirrorLayout) generationPath(name string) string {
	return filepath.Join(l.generations, name)
}

func (l mirrorLayout) generationName(path string) string {
	name, _ := l.directGenerationName(path)
	return name
}

func (l mirrorLayout) directGenerationName(value string) (string, bool) {
	absolute, err := filepath.Abs(value)
	if err != nil || !sameMirrorPath(filepath.Clean(value), absolute) {
		return "", false
	}
	relative, err := filepath.Rel(l.generations, absolute)
	if err != nil || relative == "." || filepath.IsAbs(relative) || strings.Contains(relative, string(filepath.Separator)) || !mirrorGenerationPattern.MatchString(relative) {
		return "", false
	}
	return relative, true
}

func newMirrorTransactionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func mirrorGenerationIDFromPath(generationPath string) (string, error) {
	name := filepath.Base(filepath.Clean(generationPath))
	if !mirrorGenerationPattern.MatchString(name) {
		return "", fmt.Errorf("generation path has no managed identity")
	}
	separator := strings.IndexByte(name, '-')
	if separator < 0 || separator == len(name)-1 {
		return "", fmt.Errorf("generation path has no managed identity")
	}
	id := name[separator+1:]
	if !mirrorIDPattern.MatchString(id) {
		return "", fmt.Errorf("generation path identity is invalid")
	}
	return id, nil
}

func writeMirrorGenerationMarker(generationPath, generationID string) error {
	pathID, err := mirrorGenerationIDFromPath(generationPath)
	if err != nil || !mirrorIDPattern.MatchString(generationID) || pathID != generationID {
		return fmt.Errorf("generation marker identity does not match its managed path")
	}
	marker := mirrorGenerationMarker{SchemaVersion: 1, GenerationID: generationID}
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	gitDirectory := filepath.Join(generationPath, ".git")
	if _, err := securefile.VerifyDirectoryNoReparse(gitDirectory); err != nil {
		return err
	}
	if err := atomicfile.Write(filepath.Join(gitDirectory, mirrorGenerationMarkerName), data, 0o600); err != nil {
		return err
	}
	if err := syncMirrorDirectory(gitDirectory); err != nil {
		return err
	}
	return nil
}

func readMirrorGenerationMarker(generationPath string) (string, error) {
	file, err := securefile.OpenRegularBeneath(generationPath, ".git/"+mirrorGenerationMarkerName)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, mirrorJournalLimit+1))
	if err != nil || len(data) > mirrorJournalLimit {
		return "", fmt.Errorf("generation marker is unreadable")
	}
	var marker mirrorGenerationMarker
	if err := strictjson.Decode(data, &marker, true); err != nil || marker.SchemaVersion != 1 || !mirrorIDPattern.MatchString(marker.GenerationID) {
		return "", fmt.Errorf("generation marker is invalid")
	}
	pathID, err := mirrorGenerationIDFromPath(generationPath)
	if err != nil || marker.GenerationID != pathID {
		return "", fmt.Errorf("generation marker identity does not match its managed path")
	}
	return marker.GenerationID, nil
}

func writeMirrorJournal(layout mirrorLayout, journal mirrorJournal) error {
	if err := validateMirrorJournal(layout, journal); err != nil {
		return err
	}
	if _, err := securefile.VerifyDirectoryNoReparse(layout.storageRoot); err != nil {
		return err
	}
	if info, err := os.Lstat(layout.journalPath); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("journal is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > mirrorJournalLimit {
		return fmt.Errorf("journal exceeds limit")
	}
	return atomicfile.Write(layout.journalPath, data, 0o600)
}

func readMirrorJournal(layout mirrorLayout) (*mirrorJournal, error) {
	info, err := os.Lstat(layout.journalPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > mirrorJournalLimit {
		return nil, fmt.Errorf("journal is not a bounded regular file")
	}
	file, err := securefile.OpenRegularBeneath(layout.storageRoot, filepath.Base(layout.journalPath))
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, mirrorJournalLimit+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil || len(data) > mirrorJournalLimit {
		return nil, fmt.Errorf("journal is unreadable")
	}
	var journal mirrorJournal
	if err := strictjson.Decode(data, &journal, true); err != nil {
		return nil, fmt.Errorf("journal is malformed")
	}
	if err := validateMirrorJournal(layout, journal); err != nil {
		return nil, err
	}
	return &journal, nil
}

func validateMirrorJournal(layout mirrorLayout, journal mirrorJournal) error {
	if journal.SchemaVersion != mirrorJournalSchemaVersion || !mirrorIDPattern.MatchString(journal.TransactionID) || journal.RepositoryID == "" {
		return fmt.Errorf("journal identity is invalid")
	}
	switch journal.State {
	case mirrorJournalPrepared, mirrorJournalPromoted, mirrorJournalRollback:
	default:
		return fmt.Errorf("journal state is invalid")
	}
	for _, generation := range []mirrorJournalGeneration{journal.Baseline, journal.Candidate} {
		if !mirrorGenerationPattern.MatchString(generation.Name) || !mirrorIDPattern.MatchString(generation.ID) || !mirrorObjectIDPattern.MatchString(generation.Commit) || !mirrorObjectIDPattern.MatchString(generation.Tree) {
			return fmt.Errorf("journal generation identity is invalid")
		}
		if name, ok := layout.directGenerationName(layout.generationPath(generation.Name)); !ok || name != generation.Name {
			return fmt.Errorf("journal generation path is invalid")
		}
	}
	if journal.Baseline.Name == journal.Candidate.Name || journal.Baseline.ID == journal.Candidate.ID {
		return fmt.Errorf("journal generations are ambiguous")
	}
	return nil
}

func removeMirrorJournal(layout mirrorLayout) error {
	journal, err := readMirrorJournal(layout)
	if err != nil {
		return err
	}
	if journal == nil {
		return nil
	}
	if err := os.Remove(layout.journalPath); err != nil {
		return err
	}
	return syncMirrorDirectory(layout.storageRoot)
}

func copyMirrorGeneration(ctx context.Context, source, destination string) error {
	if _, err := securefile.VerifyDirectoryNoReparse(source); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if _, err := securefile.VerifyDirectoryNoReparse(filepath.Dir(destination)); err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	files := 0
	bytesCopied := int64(0)
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("generation path escaped source")
		}
		if relative == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("generation contains a link or unreadable entry")
		}
		destinationPath := filepath.Join(destination, relative)
		if entry.IsDir() {
			if _, err := securefile.VerifyDirectoryNoReparse(path); err != nil {
				return err
			}
			return os.Mkdir(destinationPath, info.Mode().Perm()|0o700)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("generation contains a special file")
		}
		files++
		bytesCopied += info.Size()
		if files > mirrorGenerationFileLimit || info.Size() < 0 || bytesCopied > mirrorGenerationByteLimit {
			return fmt.Errorf("generation exceeds copy limits")
		}
		relativeSlash := filepath.ToSlash(relative)
		input, err := securefile.OpenRegularBeneath(source, relativeSlash)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			_ = input.Close()
			return err
		}
		written, copyErr := io.Copy(output, io.LimitReader(input, info.Size()+1))
		if copyErr == nil && written != info.Size() {
			copyErr = fmt.Errorf("source changed while copying")
		}
		if copyErr == nil {
			copyErr = output.Sync()
		}
		closeErr := output.Close()
		inputInfo, statErr := input.Stat()
		inputCloseErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil || statErr != nil || inputCloseErr != nil || !os.SameFile(info, inputInfo) || inputInfo.Size() != info.Size() {
			return fmt.Errorf("source changed while copying")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return makeMirrorGenerationDurable(destination)
}

func makeMirrorGenerationDurable(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("generation contains an unsafe entry")
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("generation contains a special file")
		}
		file, err := securefile.OpenRegularNoFollow(path)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := syncMirrorDirectory(directories[index]); err != nil {
			return err
		}
	}
	return syncMirrorDirectory(filepath.Dir(root))
}

func sameMirrorPath(left, right string) bool {
	if mirrorPathCaseInsensitive() {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

// cleanupOrphanedMirrorCandidates bounds private disk use after crashes and
// rejected validation. It never follows or recursively removes an unresolved
// path: only a direct managed candidate with a valid marker is eligible.
func (m Mirror) cleanupOrphanedMirrorCandidates(layout mirrorLayout, activePath string) error {
	if _, err := securefile.VerifyDirectoryNoReparse(layout.generations); err != nil {
		return err
	}
	journal, err := readMirrorJournal(layout)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(layout.generations)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "candidate-") {
			continue
		}
		if !mirrorGenerationPattern.MatchString(entry.Name()) {
			return fmt.Errorf("private mirror generation has an invalid name")
		}
		path := layout.generationPath(entry.Name())
		if sameMirrorPath(path, activePath) || (journal != nil && journal.Candidate.Name == entry.Name()) {
			continue
		}
		id, err := readMirrorGenerationMarker(path)
		if err != nil {
			return fmt.Errorf("private mirror generation is not marker-verified")
		}
		if err := removeVerifiedMirrorGeneration(layout, path, id); err != nil {
			return err
		}
	}
	return nil
}

func removeVerifiedMirrorGeneration(layout mirrorLayout, path, expectedID string) error {
	name, ok := layout.directGenerationName(path)
	if !ok || !strings.HasPrefix(name, "candidate-") || !mirrorIDPattern.MatchString(expectedID) {
		return fmt.Errorf("mirror generation is not eligible for private cleanup")
	}
	journal, err := readMirrorJournal(layout)
	if err != nil {
		return err
	}
	if journal != nil && (journal.Baseline.Name == name || journal.Candidate.Name == name) {
		return fmt.Errorf("mirror generation is referenced by a promotion journal")
	}
	if _, err := securefile.VerifyDirectoryNoReparse(path); err != nil {
		return err
	}
	actualID, err := readMirrorGenerationMarker(path)
	if err != nil || actualID != expectedID {
		return fmt.Errorf("mirror generation marker changed before cleanup")
	}
	var paths []string
	err = filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("mirror generation contains an unsafe cleanup entry")
		}
		paths = append(paths, current)
		return nil
	})
	if err != nil {
		return err
	}
	actualID, err = readMirrorGenerationMarker(path)
	if err != nil || actualID != expectedID {
		return fmt.Errorf("mirror generation marker changed during cleanup inspection")
	}
	for index := len(paths) - 1; index >= 0; index-- {
		info, err := os.Lstat(paths[index])
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("mirror generation changed during cleanup")
		}
		if err := os.Remove(paths[index]); err != nil {
			return err
		}
	}
	return syncMirrorDirectory(layout.generations)
}

// InspectRecovery is read-only. The application must use a reopened,
// schema-valid accepted status record as the completion barrier and then call
// Finalize or Rollback explicitly.
func (m Mirror) InspectRecovery(ctx context.Context, target Target) (MirrorRecovery, *OperationError) {
	layout, err := deriveMirrorLayout(target)
	if err != nil {
		return MirrorRecovery{}, mirrorFilesystemFailure("recovery", "mirror layout is invalid", err)
	}
	journal, err := readMirrorJournal(layout)
	if err != nil {
		return MirrorRecovery{}, mirrorFilesystemFailure("recovery", "mirror promotion journal is invalid", err)
	}
	if journal == nil {
		return MirrorRecovery{}, nil
	}
	if journal.RepositoryID != target.ID {
		return MirrorRecovery{}, &OperationError{Code: "REPO-MIRROR-RECOVERY-TAMPER", Phase: "recovery", Summary: "mirror promotion journal belongs to a different repository"}
	}
	baselinePath := layout.generationPath(journal.Baseline.Name)
	candidatePath := layout.generationPath(journal.Candidate.Name)
	if failure := m.verifyGeneration(ctx, baselinePath, journal.Baseline.ID, journal.Baseline.Commit, journal.Baseline.Tree, "recovery"); failure != nil {
		return MirrorRecovery{}, recoveryTamper("baseline mirror generation failed verification")
	}
	if failure := m.verifyGeneration(ctx, candidatePath, journal.Candidate.ID, journal.Candidate.Commit, journal.Candidate.Tree, "recovery"); failure != nil {
		return MirrorRecovery{}, recoveryTamper("candidate mirror generation failed verification")
	}
	active, err := m.pointers.Resolve(target.Path)
	if err != nil {
		return MirrorRecovery{}, recoveryTamper("mirror pointer could not be inspected")
	}
	activeName := ""
	switch {
	case sameMirrorPath(active, baselinePath):
		activeName = "baseline"
	case sameMirrorPath(active, candidatePath):
		activeName = "candidate"
	default:
		return MirrorRecovery{}, recoveryTamper("mirror pointer does not match either journalled generation")
	}
	if journal.State == mirrorJournalRollback && activeName != "baseline" {
		return MirrorRecovery{}, recoveryTamper("rollback-pending mirror does not expose its baseline")
	}
	return MirrorRecovery{
		Pending: true, TransactionID: journal.TransactionID, JournalState: journal.State, ActiveGeneration: activeName,
		BaselineCommit: journal.Baseline.Commit, BaselineTree: journal.Baseline.Tree,
		CandidateCommit: journal.Candidate.Commit, CandidateTree: journal.Candidate.Tree,
		BaselineWasAccepted: journal.Baseline.PreviouslyAccepted,
	}, nil
}

// Finalize commits the pointer decision after the application has written,
// reopened, schema-validated, and identity-checked accepted status.
func (m Mirror) Finalize(ctx context.Context, target Target, outcome Outcome, transactionID string) *OperationError {
	layout, journal, failure := m.loadExpectedTransaction(ctx, target, transactionID)
	if failure != nil {
		return failure
	}
	if journal.State == mirrorJournalRollback || outcome.AcceptedCommit != journal.Candidate.Commit || outcome.AcceptedTree != journal.Candidate.Tree || outcome.CandidateCommit != journal.Candidate.Commit || outcome.CandidateTree != journal.Candidate.Tree {
		return recoveryTamper("finalize outcome does not match the promoted candidate")
	}
	active, err := m.pointers.Resolve(target.Path)
	if err != nil || !sameMirrorPath(active, layout.generationPath(journal.Candidate.Name)) {
		return recoveryTamper("finalize pointer does not expose the promoted candidate")
	}
	if failure := m.verifyGeneration(ctx, target.Path, journal.Candidate.ID, journal.Candidate.Commit, journal.Candidate.Tree, "recovery"); failure != nil {
		return recoveryTamper("finalize candidate failed verification")
	}
	if err := removeMirrorJournal(layout); err != nil {
		return mirrorFilesystemFailure("recovery", "finalized mirror journal could not be removed", err)
	}
	return nil
}

// Rollback atomically restores the previous generation but retains a durable
// rollback-pending journal until the application repairs and verifies state and
// accepted status.
func (m Mirror) Rollback(ctx context.Context, target Target, outcome Outcome, transactionID string) (Outcome, *OperationError) {
	layout, journal, failure := m.loadExpectedTransaction(ctx, target, transactionID)
	if failure != nil {
		return Outcome{}, failure
	}
	if outcome.CandidateCommit != "" && (outcome.CandidateCommit != journal.Candidate.Commit || outcome.CandidateTree != journal.Candidate.Tree) {
		return Outcome{}, recoveryTamper("rollback outcome does not match the journalled candidate")
	}
	restored, rollbackFailure := m.rollbackJournalledPromotion(ctx, target, layout, *journal)
	if rollbackFailure != nil {
		return restored, rollbackFailure
	}
	restored.Validation = outcome.Validation
	return restored, nil
}

func (m Mirror) rollbackJournalledPromotion(ctx context.Context, target Target, layout mirrorLayout, journal mirrorJournal) (Outcome, *OperationError) {
	baselinePath := layout.generationPath(journal.Baseline.Name)
	candidatePath := layout.generationPath(journal.Candidate.Name)
	active, err := m.pointers.Resolve(target.Path)
	if err != nil {
		return journal.baselineOutcome(validation.Report{}), rollbackRefused("mirror pointer could not be inspected")
	}
	switch {
	case sameMirrorPath(active, candidatePath):
		if err := m.pointers.Replace(target.Path, candidatePath, baselinePath); err != nil {
			return journal.baselineOutcome(validation.Report{}), rollbackRefused("mirror pointer could not be atomically restored")
		}
	case sameMirrorPath(active, baselinePath):
	default:
		return journal.baselineOutcome(validation.Report{}), rollbackRefused("mirror pointer changed outside the promotion transaction")
	}
	if failure := m.verifyGeneration(ctx, target.Path, journal.Baseline.ID, journal.Baseline.Commit, journal.Baseline.Tree, "recovery"); failure != nil {
		return journal.baselineOutcome(validation.Report{}), rollbackRefused("restored mirror baseline failed verification")
	}
	journal.State = mirrorJournalRollback
	if err := writeMirrorJournal(layout, journal); err != nil {
		return journal.baselineOutcome(validation.Report{}), rollbackRefused("rollback journal could not be persisted")
	}
	return journal.baselineOutcome(validation.Report{}), nil
}

// AcknowledgeRollback removes recovery evidence only after the application has
// persisted and verified the restored baseline state and accepted status.
func (m Mirror) AcknowledgeRollback(ctx context.Context, target Target, transactionID string) *OperationError {
	layout, journal, failure := m.loadExpectedTransaction(ctx, target, transactionID)
	if failure != nil {
		return failure
	}
	if journal.State != mirrorJournalRollback {
		return recoveryTamper("mirror rollback has not completed")
	}
	active, err := m.pointers.Resolve(target.Path)
	if err != nil || !sameMirrorPath(active, layout.generationPath(journal.Baseline.Name)) {
		return recoveryTamper("rollback acknowledgement pointer is not at baseline")
	}
	if failure := m.verifyGeneration(ctx, target.Path, journal.Baseline.ID, journal.Baseline.Commit, journal.Baseline.Tree, "recovery"); failure != nil {
		return recoveryTamper("rollback acknowledgement baseline failed verification")
	}
	if err := removeMirrorJournal(layout); err != nil {
		return mirrorFilesystemFailure("recovery", "rollback journal could not be removed", err)
	}
	return nil
}

func (m Mirror) loadExpectedTransaction(ctx context.Context, target Target, transactionID string) (mirrorLayout, *mirrorJournal, *OperationError) {
	recovery, failure := m.InspectRecovery(ctx, target)
	if failure != nil {
		return mirrorLayout{}, nil, failure
	}
	if !recovery.Pending || transactionID == "" || recovery.TransactionID != transactionID {
		return mirrorLayout{}, nil, recoveryTamper("mirror transaction identity does not match")
	}
	layout, err := deriveMirrorLayout(target)
	if err != nil {
		return mirrorLayout{}, nil, mirrorFilesystemFailure("recovery", "mirror layout is invalid", err)
	}
	journal, err := readMirrorJournal(layout)
	if err != nil || journal == nil {
		return mirrorLayout{}, nil, recoveryTamper("mirror transaction disappeared during reconciliation")
	}
	return layout, journal, nil
}

func recoveryTamper(summary string) *OperationError {
	return &OperationError{Code: "REPO-MIRROR-RECOVERY-TAMPER", Phase: "recovery", Summary: summary}
}

func rollbackRefused(summary string) *OperationError {
	return &OperationError{Code: "REPO-MIRROR-ROLLBACK-FAILED", Phase: "recovery", Summary: summary}
}
