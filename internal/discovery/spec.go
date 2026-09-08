package discovery

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

type RepositorySpec struct {
	OriginalURL string
	Owner       string
	Name        string
	Branch      string
	CloneURL    string
	Key         string
}

func (s RepositorySpec) StateKey() string {
	return s.Key + "#" + s.Branch
}

func NewGitHubRepositorySpec(key, branch string) (RepositorySpec, error) {
	parts := strings.Split(strings.TrimSpace(key), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.TrimSpace(branch) == "" {
		return RepositorySpec{}, fmt.Errorf("repository owner, name, and branch are required")
	}
	escapedBranch := make([]string, 0)
	for _, part := range strings.Split(branch, "/") {
		if part == "" {
			return RepositorySpec{}, fmt.Errorf("branch cannot contain an empty path segment")
		}
		escapedBranch = append(escapedBranch, url.PathEscape(part))
	}
	return ParseGitHubBranchURL(fmt.Sprintf(
		"https://github.com/%s/%s/tree/%s",
		url.PathEscape(parts[0]),
		url.PathEscape(strings.TrimSuffix(parts[1], ".git")),
		strings.Join(escapedBranch, "/"),
	))
}

func ParseGitHubBranchURL(raw string) (RepositorySpec, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return RepositorySpec{}, fmt.Errorf("parse repository URL: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "github.com") {
		return RepositorySpec{}, fmt.Errorf("repository URL must be an https://github.com link")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return RepositorySpec{}, fmt.Errorf("repository URL cannot contain credentials, query parameters, or fragments")
	}
	parts := escapedPathParts(u.EscapedPath())
	if len(parts) < 4 || parts[2] != "tree" {
		return RepositorySpec{}, fmt.Errorf("repository URL must include a branch, for example https://github.com/owner/repo/tree/main")
	}
	owner, err := url.PathUnescape(parts[0])
	if err != nil {
		return RepositorySpec{}, fmt.Errorf("decode repository owner: %w", err)
	}
	name, err := url.PathUnescape(strings.TrimSuffix(parts[1], ".git"))
	if err != nil {
		return RepositorySpec{}, fmt.Errorf("decode repository name: %w", err)
	}
	branchParts := make([]string, 0, len(parts)-3)
	for _, part := range parts[3:] {
		decoded, err := url.PathUnescape(part)
		if err != nil {
			return RepositorySpec{}, fmt.Errorf("decode branch: %w", err)
		}
		branchParts = append(branchParts, decoded)
	}
	branch := strings.Join(branchParts, "/")
	if owner == "" || name == "" || branch == "" {
		return RepositorySpec{}, fmt.Errorf("repository owner, name, and branch are required")
	}
	escapedBranch := make([]string, 0, len(branchParts))
	for _, part := range branchParts {
		escapedBranch = append(escapedBranch, url.PathEscape(part))
	}
	return RepositorySpec{
		OriginalURL: fmt.Sprintf("https://github.com/%s/%s/tree/%s", owner, name, strings.Join(escapedBranch, "/")),
		Owner:       owner,
		Name:        name,
		Branch:      branch,
		CloneURL:    fmt.Sprintf("https://github.com/%s/%s.git", owner, name),
		Key:         strings.ToLower(owner + "/" + name),
	}, nil
}

func escapedPathParts(value string) []string {
	value = strings.Trim(value, "/")
	if value == "" {
		return nil
	}
	return strings.Split(value, "/")
}

func CanonicalRemote(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "git@github.com:") {
		return canonicalOwnerRepo(strings.TrimPrefix(raw, "git@github.com:"))
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(u.Hostname(), "github.com") {
		return "", fmt.Errorf("remote is not hosted on github.com")
	}
	allowedSSHUser := false
	if u.User != nil && strings.EqualFold(u.Scheme, "ssh") && u.User.Username() == "git" {
		_, hasPassword := u.User.Password()
		allowedSSHUser = !hasPassword
	}
	if (u.User != nil && !allowedSSHUser) || u.RawQuery != "" {
		return "", fmt.Errorf("remote URL cannot contain credentials or query parameters; use a credential manager")
	}
	return canonicalOwnerRepo(strings.TrimPrefix(path.Clean(u.Path), "/"))
}

func canonicalOwnerRepo(value string) (string, error) {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	if len(parts) != 2 {
		return "", fmt.Errorf("remote must identify one GitHub owner and repository")
	}
	owner := strings.TrimSpace(parts[0])
	name := strings.TrimSuffix(strings.TrimSpace(parts[1]), ".git")
	if owner == "" || name == "" {
		return "", fmt.Errorf("remote owner and repository cannot be empty")
	}
	return strings.ToLower(owner + "/" + name), nil
}
