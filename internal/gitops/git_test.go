package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JorgeMuehlebach/repo-sync/internal/validation"
)

func TestSafeCandidatePathRejectsControlAndTraversalNames(t *testing.T) {
	for _, value := range []string{"docs/readme.md", "skills/tool/SKILL.md", "space is fine.md"} {
		if !safeCandidatePath(value) {
			t.Fatalf("safeCandidatePath(%q) = false", value)
		}
	}
	for _, value := range []string{"", "/absolute", "../escape", "a/../b", "a//b", "a\\b", "line\nbreak", "tab\tname", string([]byte{'b', 'a', 'd', 0xff})} {
		if safeCandidatePath(value) {
			t.Fatalf("safeCandidatePath(%q) = true", value)
		}
	}
}

func TestReadStableCandidateFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := readStableCandidateFile(dir, "link"); err == nil {
		t.Fatal("symlink was opened as a regular candidate file")
	}
}

func TestReadStableCandidateFileRejectsRegularFileSwappedToSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "candidate")
	outside := filepath.Join(dir, "outside")
	if err := os.WriteFile(path, []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := readStableCandidateFile(dir, "candidate"); err == nil {
		t.Fatal("file-to-symlink race was accepted")
	}
}

func TestReadStableCandidateFileRejectsSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "nested")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if _, _, err := readStableCandidateFile(root, "nested/secret"); err == nil {
		t.Fatal("candidate read escaped through a symlinked parent")
	}
}

func TestReadStableIndexRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "real-index")
	link := filepath.Join(directory, "index")
	if err := os.WriteFile(target, []byte("index"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, _, err := readStableIndex(link); err == nil {
		t.Fatal("symlinked Git index was accepted")
	}
}

func TestExecutableAttributeKindRejectsFiltersAndMergeDrivers(t *testing.T) {
	for _, value := range []string{"*.md filter=evil", "*.md filter", "*.md -filter", "*.md !filter", "[attr]macro filter=evil"} {
		if got := executableAttributeKind(value + "\n"); got != "filter" {
			t.Fatalf("filter attribute %q was accepted", value)
		}
	}
	for _, value := range []string{"*.md merge=evil", "*.md merge", "*.md -merge", "*.md !merge"} {
		if got := executableAttributeKind(value + "\n"); got != "merge" {
			t.Fatalf("merge attribute %q was accepted", value)
		}
	}
	if got := executableAttributeKind("# *.md filter=evil\n*.md text eol=lf\n"); got != "" {
		t.Fatal("benign/commented attributes were rejected")
	}
}

func TestValidationHoldPreservesStableCheckCode(t *testing.T) {
	tree := strings.Repeat("a", 40)
	validator := validatorFunc(func(_ context.Context, request validation.Request) (validation.Report, error) {
		return validation.Report{
			SchemaVersion: validation.ReportSchemaVersion, Protocol: validation.ReportProtocol, ContractVersion: "2.0.0", Command: "validate",
			GeneratedAt: "2026-09-19T00:00:00Z", Verdict: validation.VerdictHold, Promotable: false,
			Candidate: validation.Candidate{Kind: "git-tree", SourceID: request.Runtime.SourceID, ObjectID: request.Tree},
			Checks:    []validation.Check{{ID: validation.CodeBundleChanged, Status: "hold", Paths: []string{".agents/validators/example"}}},
		}, nil
	})
	engine := newBase(nil, validator)
	_, failure := engine.validateTree(context.Background(), Target{ID: "repo", Path: t.TempDir(), ValidationRuntime: validation.Runtime{SourceID: "example.source"}}, validation.ModeMirror, tree, tree)
	if failure == nil || failure.Code != validation.CodeBundleChanged || len(failure.Findings) != 1 {
		t.Fatalf("validation failure = %#v", failure)
	}
}

func TestCommandFailureNeverIncludesRawGitOutput(t *testing.T) {
	failure := commandFailure("REPO-FETCH-FAILED", "fetch", "fetch failed", Result{ExitCode: 1, Output: "https://secret@example.invalid"})
	if strings.Contains(failure.Error(), "secret") || !strings.Contains(failure.Error(), "Git exit 1") {
		t.Fatalf("failure = %q", failure.Error())
	}
}

type validatorFunc func(context.Context, validation.Request) (validation.Report, error)

func (f validatorFunc) Validate(ctx context.Context, request validation.Request) (validation.Report, error) {
	return f(ctx, request)
}
