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

func TestParseGitHubBranchURLRejectsNonCanonicalRepositoryIdentity(t *testing.T) {
	for _, input := range []string{
		"https://github.com:443/owner/repo/tree/main",
		"https://github.com//owner/repo/tree/main",
		"https://github.com/owner/repo/tree/main/",
		"https://github.com/%6fwner/repo/tree/main",
		"https://github.com/owner/%72epo/tree/main",
		"https://github.com/owner%2Frepo/name/tree/main",
	} {
		if _, err := ParseGitHubBranchURL(input); err == nil {
			t.Fatalf("ParseGitHubBranchURL(%q) accepted a non-canonical identity", input)
		}
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
	for _, input := range []string{
		"https://token@github.com/owner/repo.git",
		"http://github.com/owner/repo.git",
		"file://github.com/owner/repo.git",
		"ext::sh -c evil",
		"https://github.com/owner/repo.git#fragment",
		"https://github.com:443/owner/repo.git",
		"ssh://root@github.com/owner/repo.git",
		"ssh://git@github.com:22/owner/repo.git",
		"git@github.com:owner/repo.git#fragment",
		"https://github.com/owner/repo/extra.git",
		"https://github.com/owner/%72epo.git",
		"https://github.com/owner/repo.git/",
		"https://github.com//owner/repo.git",
		"git@github.com:/owner/repo.git",
		"git@github.com:owner/repo.git/",
	} {
		if _, err := CanonicalRemote(input); err == nil {
			t.Fatalf("CanonicalRemote(%q) accepted unsafe remote", input)
		}
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
