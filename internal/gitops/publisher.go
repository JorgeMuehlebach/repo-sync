package gitops

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/JorgeMuehlebach/repo-sync/internal/atomicfile"
	"github.com/JorgeMuehlebach/repo-sync/internal/securefile"
	"github.com/JorgeMuehlebach/repo-sync/internal/validation"
)

const (
	maxCandidatePaths     = 16_384
	maxCandidateFileBytes = 16 << 20
	maxCandidateTotal     = 256 << 20
	maxCandidateImport    = maxCandidateTotal + 64<<20
	maxGitIndexBytes      = 64 << 20
)

var errCandidateSnapshotLimit = errors.New("candidate snapshot limit exceeded")

type Publisher struct {
	base
	CommitMessage string
}

func NewPublisher(runner Runner, validator validation.Validator) Publisher {
	return Publisher{base: newBase(runner, validator), CommitMessage: defaultCommitMessage}
}

func (p Publisher) Sync(ctx context.Context, target Target) (Outcome, *OperationError) {
	if failure := p.reconcileTarget(ctx, target, target.Spec.Branch, "candidate"); failure != nil {
		return Outcome{}, failure
	}
	originalCommit, originalTree, failure := p.commitAndTree(ctx, target.Path, "HEAD")
	if failure != nil {
		return Outcome{}, failure
	}
	candidateTree, index, failure := p.captureCandidateTree(ctx, target.Path)
	if failure != nil {
		return Outcome{}, failure
	}
	defer index.cleanup()
	preimage := publishPreimage{
		OriginalCommit: originalCommit,
		OriginalTree:   originalTree,
		CandidateTree:  candidateTree,
		Index:          index,
		CurrentCommit:  originalCommit,
		CurrentTree:    originalTree,
	}
	outcome := Outcome{CandidateTree: candidateTree}
	fail := func(operationFailure *OperationError) (Outcome, *OperationError) {
		if preimage.MutationAttempted {
			if rollbackFailure := p.restorePreimage(context.WithoutCancel(ctx), target, preimage); rollbackFailure != nil {
				return outcome, rollbackFailure
			}
		}
		return outcome, operationFailure
	}
	validationTarget := target
	validationTarget.ValidationRepository = index.validationRepository
	if failure := p.rejectFilterAttributes(ctx, index.validationRepository, candidateTree); failure != nil {
		return outcome, failure
	}
	report, failure := p.validateTree(ctx, validationTarget, validation.ModePublish, candidateTree, "")
	outcome.Validation = report
	if failure != nil {
		return outcome, failure
	}
	if failure := p.reconcileTarget(ctx, target, target.Spec.Branch, "publish"); failure != nil {
		return outcome, failure
	}
	currentCommit, currentTree, failure := p.commitAndTree(ctx, target.Path, "HEAD")
	if failure != nil || currentCommit != originalCommit || currentTree != originalTree {
		return outcome, &OperationError{Code: "REPO-PUBLISH-RACE", Phase: "publish", Summary: "publisher branch changed during candidate validation"}
	}
	if candidateTree != originalTree {
		if failure := index.importObjects(ctx, p.git, target.Path, candidateTree); failure != nil {
			return outcome, failure
		}
		indexMutationAttempted, failure := index.promote()
		preimage.MutationAttempted = indexMutationAttempted
		if failure != nil {
			return fail(failure)
		}
		stagedTree := p.git.Run(ctx, target.Path, "write-tree")
		if stagedTree.Err != nil || stagedTree.Output != candidateTree {
			return fail(&OperationError{Code: "REPO-PUBLISH-INDEX-RACE", Phase: "publish", Summary: "validated index changed before commit"})
		}
		message := p.CommitMessage
		if message == "" {
			message = defaultCommitMessage
		}
		commit, mutationFailure := runSafeMutation(ctx, p.git, target.Path, "publish", "commit", "-m", message)
		if mutationFailure != nil {
			return fail(mutationFailure)
		}
		if commit.Err != nil {
			if committedHead, committedTree, committedFailure := p.commitAndTree(ctx, target.Path, "HEAD"); committedFailure == nil && committedTree == candidateTree {
				preimage.CurrentCommit, preimage.CurrentTree = committedHead, committedTree
			}
			return fail(commandFailure("REPO-PUBLISH-COMMIT", "publish", "commit failed", commit))
		}
		currentCommit, currentTree, failure = p.commitAndTree(ctx, target.Path, "HEAD")
		if failure != nil {
			return fail(failure)
		}
		preimage.CurrentCommit, preimage.CurrentTree = currentCommit, currentTree
		outcome.CandidateCommit, outcome.CandidateTree = currentCommit, currentTree
		report, failure = p.validateTree(ctx, target, validation.ModePublish, currentTree, currentCommit)
		outcome.Validation = report
		if failure != nil {
			return fail(failure)
		}
	}

	var push Result
	for attempt := 0; attempt < 3; attempt++ {
		fetched, fetchedFailure := p.fetchRemoteCandidate(ctx, target, target.Spec.Branch)
		if fetchedFailure != nil {
			return fail(fetchedFailure)
		}
		cleanupFetched := func() { p.deleteCandidateRef(context.WithoutCancel(ctx), target.Path, fetched.Ref, fetched.Commit) }
		if attributeFailure := p.rejectFilterAttributes(ctx, target.Path, fetched.Tree); attributeFailure != nil {
			cleanupFetched()
			return fail(attributeFailure)
		}
		preimage.MutationAttempted = true
		rebase, mutationFailure := runSafeMutation(ctx, p.git, target.Path, "publish", "rebase", fetched.Commit)
		if mutationFailure != nil {
			cleanupFetched()
			return fail(mutationFailure)
		}
		if rebase.Err != nil {
			abort, abortFailure := runSafeMutation(context.WithoutCancel(ctx), p.git, target.Path, "publish", "rebase", "--abort")
			cleanupFetched()
			if abortFailure != nil || abort.Err != nil {
				return outcome, publishRollbackRefused("failed rebase could not be aborted safely")
			}
			return fail(commandFailure("REPO-PUBLISH-REBASE", "publish", "rebase failed", rebase))
		}
		currentCommit, currentTree, failure = p.commitAndTree(ctx, target.Path, "HEAD")
		if failure != nil {
			cleanupFetched()
			return fail(failure)
		}
		preimage.CurrentCommit, preimage.CurrentTree = currentCommit, currentTree
		outcome.CandidateCommit, outcome.CandidateTree = currentCommit, currentTree
		report, failure = p.validateTree(ctx, target, validation.ModePublish, currentTree, currentCommit)
		outcome.Validation = report
		if failure != nil {
			cleanupFetched()
			return fail(failure)
		}
		if failure := p.reconcileTarget(ctx, target, target.Spec.Branch, "publish"); failure != nil {
			cleanupFetched()
			return fail(failure)
		}
		verifiedCommit, verifiedTree, verifyFailure := p.commitAndTree(ctx, target.Path, "HEAD")
		if verifyFailure != nil || verifiedCommit != currentCommit || verifiedTree != currentTree {
			cleanupFetched()
			return fail(&OperationError{Code: "REPO-PUBLISH-RACE", Phase: "publish", Summary: "validated publish tree changed before push"})
		}
		push, mutationFailure = runSafeMutation(ctx, p.git, target.Path, "publish", "push", "--recurse-submodules=no", "origin", currentCommit+":refs/heads/"+target.Spec.Branch)
		cleanupFetched()
		if mutationFailure != nil {
			return fail(mutationFailure)
		}
		if push.Err == nil {
			return Outcome{
				AcceptedCommit:  currentCommit,
				AcceptedTree:    currentTree,
				CandidateCommit: currentCommit,
				CandidateTree:   currentTree,
				Validation:      report,
			}, nil
		}
	}
	return fail(commandFailure("REPO-PUBLISH-PUSH", "publish", "push failed after three attempts", push))
}

type publishPreimage struct {
	OriginalCommit    string
	OriginalTree      string
	CandidateTree     string
	CurrentCommit     string
	CurrentTree       string
	Index             candidateIndex
	MutationAttempted bool
}

func (p Publisher) restorePreimage(ctx context.Context, target Target, preimage publishPreimage) *OperationError {
	branch := p.git.Run(ctx, target.Path, "branch", "--show-current")
	if branch.Err != nil || branch.Output != target.Spec.Branch {
		return publishRollbackRefused("publisher branch changed; automatic rollback was refused")
	}
	currentCommit, currentTree, failure := p.commitAndTree(ctx, target.Path, "HEAD")
	if failure != nil {
		return publishRollbackRefused("publisher HEAD could not be inspected during rollback")
	}
	if currentCommit != preimage.OriginalCommit {
		if currentCommit != preimage.CurrentCommit || currentTree != preimage.CurrentTree {
			return publishRollbackRefused("publisher HEAD changed outside the publish transaction")
		}
		status := p.git.Run(ctx, target.Path, "status", "--porcelain=v1", "--untracked-files=all")
		if status.Err != nil || status.Output != "" {
			return publishRollbackRefused("publisher has concurrent worktree content; rollback was refused")
		}
		restoreRef, mutationFailure := runSafeMutation(ctx, p.git, target.Path, "publish", "update-ref", "HEAD", preimage.OriginalCommit, currentCommit)
		if mutationFailure != nil {
			return publishRollbackRefused(mutationFailure.Summary)
		}
		if restoreRef.Err != nil {
			return commandFailure("REPO-PUBLISH-ROLLBACK-FAILED", "publish", "publisher branch could not be restored", restoreRef)
		}
		restoreTree, mutationFailure := runSafeMutation(ctx, p.git, target.Path, "publish", "read-tree", "-u", "-m", currentCommit, preimage.CandidateTree)
		if mutationFailure != nil {
			return publishRollbackRefused(mutationFailure.Summary)
		}
		if restoreTree.Err != nil {
			return commandFailure("REPO-PUBLISH-ROLLBACK-FAILED", "publish", "publisher worktree could not be restored", restoreTree)
		}
	} else if currentTree != preimage.OriginalTree {
		return publishRollbackRefused("publisher original branch tree changed during rollback")
	}

	indexTree := p.git.Run(ctx, target.Path, "write-tree")
	if indexTree.Err != nil {
		return publishRollbackRefused("publisher index could not be inspected during rollback")
	}
	currentIndex, _, currentIndexExists, indexErr := readStableIndex(preimage.Index.realPath)
	if indexErr != nil {
		return publishRollbackRefused("publisher index changed during rollback")
	}
	if currentIndexExists && bytes.Equal(currentIndex, preimage.Index.original) && currentIndexExists == preimage.Index.originalExists {
		if failure := p.verifyRestoredCandidate(ctx, target.Path, preimage.CandidateTree); failure != nil {
			return failure
		}
		return nil
	}
	if !currentIndexExists || indexTree.Output != preimage.CandidateTree {
		return publishRollbackRefused("publisher index does not match the reversible preimage")
	}
	if failure := p.requireWorktreeMatchesIndex(ctx, target.Path); failure != nil {
		return failure
	}
	if failure := preimage.Index.restoreOriginal(currentIndex); failure != nil {
		return failure
	}
	return p.verifyRestoredCandidate(ctx, target.Path, preimage.CandidateTree)
}

func (p Publisher) requireWorktreeMatchesIndex(ctx context.Context, repositoryPath string) *OperationError {
	diff := p.git.Run(ctx, repositoryPath, "diff-files", "--quiet", "--ignore-submodules=none")
	if diff.Err != nil {
		return publishRollbackRefused("publisher worktree changed during rollback")
	}
	others := p.git.Run(ctx, repositoryPath, "ls-files", "--others", "--exclude-standard", "-z")
	if others.Err != nil || rawOutput(others) != "" {
		return publishRollbackRefused("publisher has concurrent untracked content; rollback was refused")
	}
	return nil
}

func (p Publisher) verifyRestoredCandidate(ctx context.Context, repositoryPath, expectedTree string) *OperationError {
	tree, snapshot, failure := p.captureCandidateTree(ctx, repositoryPath)
	if failure != nil {
		return publishRollbackRefused("publisher reversible preimage could not be verified")
	}
	snapshot.cleanup()
	if tree != expectedTree {
		return publishRollbackRefused("publisher worktree changed while rollback was in progress")
	}
	return nil
}

func publishRollbackRefused(summary string) *OperationError {
	return &OperationError{Code: "REPO-PUBLISH-ROLLBACK-FAILED", Phase: "publish", Summary: summary}
}

type candidateIndex struct {
	realPath             string
	temporaryPath        string
	temporaryRoot        string
	objectPath           string
	realObjectPath       string
	realObjectInfo       os.FileInfo
	realIndexInfo        os.FileInfo
	validationRepository string
	original             []byte
	originalExists       bool
	permissions          os.FileMode
}

func (p Publisher) captureCandidateTree(ctx context.Context, repoPath string) (string, candidateIndex, *OperationError) {
	indexResult := p.git.Run(ctx, repoPath, "rev-parse", "--git-path", "index")
	if indexResult.Err != nil {
		return "", candidateIndex{}, commandFailure("REPO-PUBLISH-INDEX", "candidate", "Git index could not be located", indexResult)
	}
	indexPath := indexResult.Output
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(repoPath, indexPath)
	}
	original, realIndexInfo, originalExists, err := readStableIndex(indexPath)
	if err != nil {
		return "", candidateIndex{}, &OperationError{Code: "REPO-PUBLISH-INDEX", Phase: "candidate", Summary: "Git index could not be read"}
	}
	permissions := os.FileMode(0o600)
	if originalExists {
		permissions = realIndexInfo.Mode().Perm()
	}
	objectsResult := p.git.Run(ctx, repoPath, "rev-parse", "--git-path", "objects")
	if objectsResult.Err != nil {
		return "", candidateIndex{}, commandFailure("REPO-PUBLISH-OBJECTS", "candidate", "Git object store could not be located", objectsResult)
	}
	realObjects := objectsResult.Output
	if !filepath.IsAbs(realObjects) {
		realObjects = filepath.Join(repoPath, realObjects)
	}
	realObjects, err = filepath.EvalSymlinks(realObjects)
	if err != nil {
		return "", candidateIndex{}, &OperationError{Code: "REPO-PUBLISH-OBJECTS", Phase: "candidate", Summary: "Git object store is not canonical"}
	}
	realObjectInfo, err := securefile.VerifyDirectoryNoReparse(realObjects)
	if err != nil {
		return "", candidateIndex{}, &OperationError{Code: "REPO-PUBLISH-OBJECTS", Phase: "candidate", Summary: "Git object store is not a canonical directory"}
	}
	objectFormat := p.git.Run(ctx, repoPath, "rev-parse", "--show-object-format")
	if objectFormat.Err != nil || (objectFormat.Output != "sha1" && objectFormat.Output != "sha256") {
		return "", candidateIndex{}, &OperationError{Code: "REPO-PUBLISH-OBJECTS", Phase: "candidate", Summary: "Git object format could not be established"}
	}
	temporaryRoot, err := os.MkdirTemp("", "repo-sync-candidate-")
	if err != nil {
		return "", candidateIndex{}, &OperationError{Code: "REPO-PUBLISH-INDEX", Phase: "candidate", Summary: "temporary candidate store could not be created"}
	}
	if err := os.Chmod(temporaryRoot, 0o700); err != nil {
		_ = os.RemoveAll(temporaryRoot)
		return "", candidateIndex{}, internalFailure("candidate")
	}
	validationRepository := filepath.Join(temporaryRoot, "repository.git")
	temporaryObjects := filepath.Join(validationRepository, "objects")
	if err := initializeTemporaryRepository(validationRepository, temporaryObjects, realObjects, objectFormat.Output); err != nil {
		_ = os.RemoveAll(temporaryRoot)
		return "", candidateIndex{}, internalFailure("candidate")
	}
	temporaryPath := filepath.Join(temporaryRoot, "index")
	index := candidateIndex{
		realPath: indexPath, temporaryPath: temporaryPath, temporaryRoot: temporaryRoot,
		objectPath: temporaryObjects, realObjectPath: realObjects, realObjectInfo: realObjectInfo, validationRepository: validationRepository,
		realIndexInfo: realIndexInfo, original: original, originalExists: originalExists, permissions: permissions,
	}
	if originalExists {
		if err := os.WriteFile(temporaryPath, original, permissions); err != nil {
			index.cleanup()
			return "", candidateIndex{}, internalFailure("candidate")
		}
	}
	envRunner, ok := p.git.(EnvironmentRunner)
	inputRunner, inputOK := p.git.(InputRunner)
	if !ok || !inputOK {
		index.cleanup()
		return "", candidateIndex{}, &OperationError{Code: "REPO-PUBLISH-INDEX", Phase: "candidate", Summary: "Git runner cannot create an isolated candidate"}
	}
	environment := map[string]string{
		"GIT_INDEX_FILE":                   temporaryPath,
		"GIT_OBJECT_DIRECTORY":             temporaryObjects,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": realObjects,
	}
	if !originalExists {
		readTree := envRunner.RunEnv(ctx, repoPath, environment, "read-tree", "HEAD")
		if readTree.Err != nil {
			readTree = envRunner.RunEnv(ctx, repoPath, environment, "read-tree", "--empty")
		}
		if readTree.Err != nil {
			index.cleanup()
			return "", candidateIndex{}, commandFailure("REPO-PUBLISH-INDEX", "candidate", "temporary Git index could not be initialized", readTree)
		}
	}
	stagedResult := envRunner.RunEnv(ctx, repoPath, environment, "ls-files", "-s", "-z")
	if stagedResult.Err != nil {
		index.cleanup()
		return "", candidateIndex{}, commandFailure("REPO-PUBLISH-SNAPSHOT", "candidate", "tracked candidate modes could not be enumerated", stagedResult)
	}
	trackedModes := make(map[string]string)
	for _, entry := range strings.Split(rawOutput(stagedResult), "\x00") {
		if entry == "" {
			continue
		}
		metadata, gitPath, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 || fields[2] != "0" || !safeCandidatePath(gitPath) {
			index.cleanup()
			return "", candidateIndex{}, &OperationError{Code: "REPO-PUBLISH-PATH", Phase: "candidate", Summary: "tracked candidate metadata is unsafe"}
		}
		trackedModes[gitPath] = fields[0]
	}
	fileModeResult := p.git.Run(ctx, repoPath, "config", "--bool", "core.fileMode")
	honorExecutableMode := runtime.GOOS != "windows"
	if fileModeResult.Err == nil {
		switch strings.ToLower(strings.TrimSpace(fileModeResult.Output)) {
		case "true":
			honorExecutableMode = runtime.GOOS != "windows"
		case "false":
			honorExecutableMode = false
		default:
			index.cleanup()
			return "", candidateIndex{}, &OperationError{Code: "REPO-GIT-CONFIG", Phase: "candidate", Summary: "core.fileMode is invalid"}
		}
	} else if fileModeResult.ExitCode != 1 {
		index.cleanup()
		return "", candidateIndex{}, commandFailure("REPO-GIT-CONFIG", "candidate", "core.fileMode could not be inspected", fileModeResult)
	}
	pathsResult := envRunner.RunEnv(ctx, repoPath, environment, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if pathsResult.Err != nil {
		index.cleanup()
		return "", candidateIndex{}, commandFailure("REPO-PUBLISH-SNAPSHOT", "candidate", "publish candidate paths could not be enumerated", pathsResult)
	}
	paths := strings.Split(rawOutput(pathsResult), "\x00")
	if len(paths) > maxCandidatePaths+1 {
		index.cleanup()
		return "", candidateIndex{}, &OperationError{Code: "REPO-PUBLISH-SNAPSHOT-LIMIT", Phase: "candidate", Summary: "publish candidate contains too many paths"}
	}
	var totalBytes int64
	for _, gitPath := range paths {
		if gitPath == "" {
			continue
		}
		if !safeCandidatePath(gitPath) {
			index.cleanup()
			return "", candidateIndex{}, &OperationError{Code: "REPO-PUBLISH-PATH", Phase: "candidate", Summary: "publish candidate contains an unsafe path"}
		}
		if trackedModes[gitPath] == "160000" {
			// Preserve a tracked gitlink in the isolated index so the trusted
			// validator can reject the immutable candidate tree.
			continue
		}
		contents, info, readErr := readStableCandidateFile(repoPath, gitPath)
		if errors.Is(readErr, os.ErrNotExist) {
			remove := envRunner.RunEnv(ctx, repoPath, environment, "update-index", "--force-remove", "--", gitPath)
			if remove.Err != nil {
				index.cleanup()
				return "", candidateIndex{}, commandFailure("REPO-PUBLISH-SNAPSHOT", "candidate", "deleted candidate path could not be captured", remove)
			}
			continue
		}
		if readErr != nil {
			index.cleanup()
			if errors.Is(readErr, errCandidateSnapshotLimit) {
				return "", candidateIndex{}, &OperationError{Code: "REPO-PUBLISH-SNAPSHOT-LIMIT", Phase: "candidate", Summary: "publish candidate exceeds snapshot limits"}
			}
			return "", candidateIndex{}, &OperationError{Code: "REPO-PUBLISH-SNAPSHOT-RACE", Phase: "candidate", Summary: "candidate file could not be captured beneath the repository root"}
		}
		mode := "100644"
		if totalBytes+int64(len(contents)) > maxCandidateTotal {
			index.cleanup()
			return "", candidateIndex{}, &OperationError{Code: "REPO-PUBLISH-SNAPSHOT-LIMIT", Phase: "candidate", Summary: "publish candidate exceeds snapshot limits"}
		}
		totalBytes += int64(len(contents))
		existingMode := trackedModes[gitPath]
		if !honorExecutableMode && (existingMode == "100644" || existingMode == "100755") {
			mode = existingMode
		} else if honorExecutableMode && info.Mode().Perm()&0o111 != 0 {
			mode = "100755"
		}
		object := inputRunner.RunInput(ctx, repoPath, contents, environment, "hash-object", "-w", "--stdin")
		if object.Err != nil {
			index.cleanup()
			return "", candidateIndex{}, commandFailure("REPO-PUBLISH-SNAPSHOT", "candidate", "candidate object could not be written", object)
		}
		cacheInfo := fmt.Sprintf("%s,%s,%s", mode, object.Output, gitPath)
		update := envRunner.RunEnv(ctx, repoPath, environment, "update-index", "--add", "--cacheinfo", cacheInfo)
		if update.Err != nil {
			index.cleanup()
			return "", candidateIndex{}, commandFailure("REPO-PUBLISH-SNAPSHOT", "candidate", "candidate index could not be updated", update)
		}
	}
	tree := envRunner.RunEnv(ctx, repoPath, environment, "write-tree")
	if tree.Err != nil {
		index.cleanup()
		return "", candidateIndex{}, commandFailure("REPO-PUBLISH-SNAPSHOT", "candidate", "publish candidate tree could not be written", tree)
	}
	return tree.Output, index, nil
}

// promote reports whether replacement of the real index was attempted. Object
// import is append-only and needs no rollback; callers must only enter the
// reversible rollback path once this index replacement boundary is crossed.
func (i candidateIndex) promote() (bool, *OperationError) {
	lockPath := i.realPath + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, &OperationError{Code: "REPO-PUBLISH-INDEX-RACE", Phase: "publish", Summary: "Git index is locked by another operation"}
	}
	committed := false
	defer func() {
		_ = lock.Close()
		if !committed {
			_ = os.Remove(lockPath)
		}
	}()
	current, currentInfo, currentExists, err := readStableIndex(i.realPath)
	if i.originalExists {
		if err != nil || !currentExists || !os.SameFile(i.realIndexInfo, currentInfo) || i.realIndexInfo.Mode() != currentInfo.Mode() || i.realIndexInfo.Size() != currentInfo.Size() || !i.realIndexInfo.ModTime().Equal(currentInfo.ModTime()) || !bytes.Equal(current, i.original) {
			return false, &OperationError{Code: "REPO-PUBLISH-INDEX-RACE", Phase: "publish", Summary: "Git index changed during validation"}
		}
	} else if err != nil || currentExists {
		return false, &OperationError{Code: "REPO-PUBLISH-INDEX-RACE", Phase: "publish", Summary: "Git index appeared during validation"}
	}
	candidate, _, candidateExists, err := readStableIndex(i.temporaryPath)
	if err != nil || !candidateExists {
		return false, internalFailure("publish")
	}
	if err := lock.Chmod(i.permissions); err != nil {
		return false, internalFailure("publish")
	}
	if _, err := lock.Write(candidate); err != nil {
		return false, internalFailure("publish")
	}
	if err := lock.Sync(); err != nil {
		return false, internalFailure("publish")
	}
	if err := lock.Close(); err != nil {
		return false, internalFailure("publish")
	}
	if err := atomicfile.Replace(lockPath, i.realPath); err != nil {
		return true, &OperationError{Code: "REPO-PUBLISH-INDEX", Phase: "publish", Summary: "validated Git index could not be installed"}
	}
	committed = true
	return true, nil
}

func (i candidateIndex) restoreOriginal(expectedCurrent []byte) *OperationError {
	lockPath := i.realPath + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return &OperationError{Code: "REPO-PUBLISH-ROLLBACK-FAILED", Phase: "publish", Summary: "Git index is locked during rollback"}
	}
	committed := false
	defer func() {
		_ = lock.Close()
		if !committed {
			_ = os.Remove(lockPath)
		}
	}()
	current, _, exists, err := readStableIndex(i.realPath)
	if err != nil || !exists || !bytes.Equal(current, expectedCurrent) {
		return &OperationError{Code: "REPO-PUBLISH-ROLLBACK-FAILED", Phase: "publish", Summary: "Git index changed during rollback"}
	}
	if !i.originalExists {
		if err := lock.Close(); err != nil {
			return internalFailure("publish")
		}
		if err := os.Remove(i.realPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return &OperationError{Code: "REPO-PUBLISH-ROLLBACK-FAILED", Phase: "publish", Summary: "original Git index could not be restored"}
		}
		if err := os.Remove(lockPath); err != nil {
			return &OperationError{Code: "REPO-PUBLISH-ROLLBACK-FAILED", Phase: "publish", Summary: "Git index rollback lock could not be released"}
		}
		committed = true
		return nil
	}
	if err := lock.Chmod(i.permissions); err != nil {
		return internalFailure("publish")
	}
	if _, err := lock.Write(i.original); err != nil {
		return internalFailure("publish")
	}
	if err := lock.Sync(); err != nil {
		return internalFailure("publish")
	}
	if err := lock.Close(); err != nil {
		return internalFailure("publish")
	}
	if err := atomicfile.Replace(lockPath, i.realPath); err != nil {
		return &OperationError{Code: "REPO-PUBLISH-ROLLBACK-FAILED", Phase: "publish", Summary: "original Git index could not be restored"}
	}
	committed = true
	return nil
}

func readStableIndex(path string) ([]byte, os.FileInfo, bool, error) {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, nil
	}
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > maxGitIndexBytes {
		return nil, nil, false, fmt.Errorf("Git index is not a bounded regular file")
	}
	file, err := securefile.OpenRegularNoFollow(path)
	if err != nil {
		return nil, nil, false, err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(before, opened) || opened.Size() < 0 || opened.Size() > maxGitIndexBytes {
		_ = file.Close()
		return nil, nil, false, fmt.Errorf("Git index changed while opening")
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, maxGitIndexBytes+1))
	closeErr := file.Close()
	after, afterErr := os.Lstat(path)
	if readErr != nil || closeErr != nil || len(contents) > maxGitIndexBytes || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, nil, false, fmt.Errorf("Git index changed while reading")
	}
	return contents, opened, true, nil
}

func (i candidateIndex) importObjects(ctx context.Context, runner Runner, repositoryPath, candidateTree string) *OperationError {
	currentObjectInfo, err := securefile.VerifyDirectoryNoReparse(i.realObjectPath)
	if err != nil || !os.SameFile(i.realObjectInfo, currentObjectInfo) {
		return &OperationError{Code: "REPO-PUBLISH-OBJECTS", Phase: "publish", Summary: "Git object store changed during validation"}
	}
	temporaryFSCK := runner.Run(ctx, i.validationRepository, "fsck", "--strict", "--no-dangling", "--no-reflogs", candidateTree)
	if temporaryFSCK.Err != nil {
		return commandFailure("REPO-PUBLISH-OBJECTS", "publish", "temporary candidate object graph failed integrity verification", temporaryFSCK)
	}
	fanouts, err := os.ReadDir(i.objectPath)
	if err != nil {
		return &OperationError{Code: "REPO-PUBLISH-OBJECTS", Phase: "publish", Summary: "validated candidate objects could not be enumerated"}
	}
	imported := 0
	var importedBytes int64
	for _, fanout := range fanouts {
		if !fanout.IsDir() || len(fanout.Name()) != 2 || !lowerHex(fanout.Name()) {
			continue
		}
		sourceDirectory := filepath.Join(i.objectPath, fanout.Name())
		objects, readErr := os.ReadDir(sourceDirectory)
		if readErr != nil {
			return &OperationError{Code: "REPO-PUBLISH-OBJECTS", Phase: "publish", Summary: "validated candidate objects could not be enumerated"}
		}
		for _, object := range objects {
			if object.IsDir() || (len(object.Name()) != 38 && len(object.Name()) != 62) || !lowerHex(object.Name()) {
				return &OperationError{Code: "REPO-PUBLISH-OBJECTS", Phase: "publish", Summary: "temporary candidate object store is invalid"}
			}
			imported++
			if imported > maxCandidatePaths*2+1024 {
				return &OperationError{Code: "REPO-PUBLISH-OBJECTS", Phase: "publish", Summary: "temporary candidate object store exceeds its bound"}
			}
			targetDirectory := filepath.Join(i.realObjectPath, fanout.Name())
			targetDirectoryInfo, directoryErr := securefile.VerifyDirectoryNoReparse(targetDirectory)
			if errors.Is(directoryErr, os.ErrNotExist) {
				directoryErr = os.Mkdir(targetDirectory, 0o755)
				if directoryErr == nil {
					targetDirectoryInfo, directoryErr = securefile.VerifyDirectoryNoReparse(targetDirectory)
				}
			}
			if directoryErr != nil {
				return &OperationError{Code: "REPO-PUBLISH-OBJECTS", Phase: "publish", Summary: "validated candidate object directory could not be created"}
			}
			objectID := fanout.Name() + object.Name()
			copied, err := importLooseObject(filepath.Join(sourceDirectory, object.Name()), filepath.Join(targetDirectory, object.Name()), targetDirectory, targetDirectoryInfo, objectID, maxCandidateImport-importedBytes)
			if err != nil {
				return &OperationError{Code: "REPO-PUBLISH-OBJECTS", Phase: "publish", Summary: "validated candidate object could not be imported"}
			}
			importedBytes += copied
		}
	}
	currentObjectInfo, err = securefile.VerifyDirectoryNoReparse(i.realObjectPath)
	if err != nil || !os.SameFile(i.realObjectInfo, currentObjectInfo) {
		return &OperationError{Code: "REPO-PUBLISH-OBJECTS", Phase: "publish", Summary: "Git object store changed during object import"}
	}
	fsck := runner.Run(ctx, repositoryPath, "fsck", "--strict", "--no-dangling", "--no-reflogs", candidateTree)
	if fsck.Err != nil {
		return commandFailure("REPO-PUBLISH-OBJECTS", "publish", "validated candidate object graph failed integrity verification", fsck)
	}
	verified := runner.Run(ctx, repositoryPath, "rev-parse", "--verify", candidateTree+"^{tree}")
	if verified.Err != nil || verified.Output != candidateTree {
		return &OperationError{Code: "REPO-PUBLISH-OBJECTS", Phase: "publish", Summary: "validated candidate tree was not imported exactly"}
	}
	return nil
}

func initializeTemporaryRepository(repositoryPath, objectPath, alternatePath, objectFormat string) error {
	if strings.ContainsAny(alternatePath, "\x00\r\n") {
		return fmt.Errorf("object path is unsafe")
	}
	for _, directory := range []string{filepath.Join(objectPath, "info"), filepath.Join(objectPath, "pack"), filepath.Join(repositoryPath, "refs", "heads")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
	}
	version := "0"
	extension := ""
	if objectFormat == "sha256" {
		version = "1"
		extension = "[extensions]\n\tobjectFormat = sha256\n"
	}
	configuration := "[core]\n\trepositoryformatversion = " + version + "\n\tbare = true\n" + extension
	if err := os.WriteFile(filepath.Join(repositoryPath, "config"), []byte(configuration), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(objectPath, "info", "alternates"), []byte(alternatePath+"\n"), 0o600)
}

func importLooseObject(sourcePath, targetPath, targetDirectory string, targetDirectoryInfo os.FileInfo, objectID string, remaining int64) (int64, error) {
	if remaining < 0 {
		return 0, fmt.Errorf("candidate object import exceeds its bound")
	}
	source, err := securefile.OpenRegularNoFollow(sourcePath)
	if err != nil {
		return 0, err
	}
	defer source.Close()
	sourceInfo, err := source.Stat()
	if err != nil || sourceInfo.Size() < 0 || sourceInfo.Size() > remaining {
		return 0, fmt.Errorf("candidate object import exceeds its bound")
	}
	if err := verifyLooseObject(sourcePath, objectID); err != nil {
		return 0, err
	}
	if info, err := os.Lstat(targetPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return 0, fmt.Errorf("existing object path is unsafe")
		}
		if err := verifyLooseObject(targetPath, objectID); err != nil {
			return 0, err
		}
		return sourceInfo.Size(), nil
	} else if !os.IsNotExist(err) {
		return 0, err
	}
	temporary, err := os.CreateTemp(targetDirectory, ".repo-sync-object-*")
	if err != nil {
		return 0, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	copied, err := io.Copy(temporary, io.LimitReader(source, remaining+1))
	if err != nil || copied > remaining || copied != sourceInfo.Size() {
		_ = temporary.Close()
		return 0, fmt.Errorf("candidate object could not be copied within bounds")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return 0, err
	}
	if err := temporary.Chmod(0o444); err != nil {
		_ = temporary.Close()
		return 0, err
	}
	if err := temporary.Close(); err != nil {
		return 0, err
	}
	if err := os.Link(temporaryPath, targetPath); err != nil {
		if info, statErr := os.Lstat(targetPath); statErr == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() {
			if verifyErr := verifyLooseObject(targetPath, objectID); verifyErr == nil {
				return copied, nil
			}
		}
		return 0, err
	}
	currentDirectoryInfo, err := securefile.VerifyDirectoryNoReparse(targetDirectory)
	if err != nil || !os.SameFile(targetDirectoryInfo, currentDirectoryInfo) {
		return 0, fmt.Errorf("object directory changed during import")
	}
	return copied, nil
}

func verifyLooseObject(path, objectID string) error {
	file, err := securefile.OpenRegularNoFollow(path)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := zlib.NewReader(file)
	if err != nil {
		return err
	}
	defer compressed.Close()
	var digest hash.Hash
	switch len(objectID) {
	case 40:
		digest = sha1.New()
	case 64:
		digest = sha256.New()
	default:
		return fmt.Errorf("unsupported Git object id")
	}
	written, err := io.Copy(digest, io.LimitReader(compressed, maxCandidateImport+1))
	if err != nil || written > maxCandidateImport {
		return fmt.Errorf("Git object exceeds integrity bound")
	}
	if hex.EncodeToString(digest.Sum(nil)) != objectID {
		return fmt.Errorf("Git object content does not match its id")
	}
	return nil
}

func lowerHex(value string) bool {
	for _, character := range []byte(value) {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return value != ""
}

func (i candidateIndex) cleanup() { _ = os.RemoveAll(i.temporaryRoot) }

func safeCandidatePath(gitPath string) bool {
	if gitPath == "" || len(gitPath) > 4096 || !utf8.ValidString(gitPath) || strings.HasPrefix(gitPath, "/") || strings.Contains(gitPath, "\\") {
		return false
	}
	for _, value := range []byte(gitPath) {
		if value < 0x20 || value == 0x7f {
			return false
		}
	}
	for _, component := range strings.Split(gitPath, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

func readStableCandidateFile(repositoryRoot, gitPath string) ([]byte, os.FileInfo, error) {
	file, err := securefile.OpenRegularBeneath(repositoryRoot, gitPath)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || opened.Size() < 0 || opened.Size() > maxCandidateFileBytes {
		_ = file.Close()
		return nil, nil, errCandidateSnapshotLimit
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, maxCandidateFileBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(contents) > maxCandidateFileBytes {
		return nil, nil, errCandidateSnapshotLimit
	}
	afterFile, err := securefile.OpenRegularBeneath(repositoryRoot, gitPath)
	if err != nil {
		return nil, nil, fmt.Errorf("candidate file changed while reading")
	}
	after, statErr := afterFile.Stat()
	closeErr = afterFile.Close()
	if statErr != nil || closeErr != nil || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, nil, fmt.Errorf("candidate file changed while reading")
	}
	return contents, opened, nil
}
