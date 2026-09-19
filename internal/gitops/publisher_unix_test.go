//go:build !windows

package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPublisherCanonicalizesCandidateStoreBelowAliasedTempRoot(t *testing.T) {
	fixture := newRepositoryFixture(t)
	realTemp := t.TempDir()
	alias := filepath.Join(t.TempDir(), "temp-alias")
	if err := os.Symlink(realTemp, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("TMPDIR", alias)

	_, index, failure := NewPublisher(fixture.runner(), nil).captureCandidateTree(context.Background(), fixture.publisher)
	if failure != nil {
		t.Fatal(failure)
	}
	defer index.cleanup()
	resolved, err := filepath.EvalSymlinks(index.validationRepository)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(resolved) != index.validationRepository {
		t.Fatalf("validation repository = %q, want canonical %q", index.validationRepository, resolved)
	}
}
