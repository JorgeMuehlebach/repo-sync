package discovery

import "testing"

func TestParseGitHubBranchURL(t *testing.T) {
	got, err := ParseGitHubBranchURL("https://github.com/Owner/Repo/tree/feature/sync")
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "owner/repo" || got.Branch != "feature/sync" || got.CloneURL != "https://github.com/Owner/Repo.git" {
		t.Fatalf("ParseGitHubBranchURL() = %#v", got)
	}
}

func TestParseGitHubBranchURLRejectsMissingBranch(t *testing.T) {
	if _, err := ParseGitHubBranchURL("https://github.com/owner/repo"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseGitHubBranchURLRejectsCredentials(t *testing.T) {
	if _, err := ParseGitHubBranchURL("https://token@github.com/owner/repo/tree/main"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCanonicalRemote(t *testing.T) {
	tests := map[string]string{
		"https://github.com/Owner/Repo.git":   "owner/repo",
		"ssh://git@github.com/Owner/Repo.git": "owner/repo",
		"git@github.com:Owner/Repo.git":       "owner/repo",
	}
	for input, want := range tests {
		got, err := CanonicalRemote(input)
		if err != nil {
			t.Fatalf("CanonicalRemote(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("CanonicalRemote(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := CanonicalRemote("https://token@github.com/owner/repo.git"); err == nil {
		t.Fatal("expected credential-bearing remote to be rejected")
	}
}

func TestNewGitHubRepositorySpecPreservesSlashBranch(t *testing.T) {
	got, err := NewGitHubRepositorySpec("Owner/Repo", "feature/sync")
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "owner/repo" || got.Branch != "feature/sync" || got.OriginalURL != "https://github.com/Owner/Repo/tree/feature/sync" {
		t.Fatalf("NewGitHubRepositorySpec() = %#v", got)
	}
}
