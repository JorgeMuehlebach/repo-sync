package discovery

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var remoteComponentPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

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
	if u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "github.com") || u.Port() != "" || u.Opaque != "" {
		return RepositorySpec{}, fmt.Errorf("repository URL must be an https://github.com link")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" {
		return RepositorySpec{}, fmt.Errorf("repository URL cannot contain credentials, query parameters, or fragments")
	}
	escapedPath := u.EscapedPath()
	if !strings.HasPrefix(escapedPath, "/") || strings.HasPrefix(escapedPath, "//") || strings.HasSuffix(escapedPath, "/") || strings.Contains(escapedPath, "\\") {
		return RepositorySpec{}, fmt.Errorf("repository URL path must use its canonical form")
	}
	parts := escapedPathParts(escapedPath)
	if len(parts) < 4 || parts[2] != "tree" {
		return RepositorySpec{}, fmt.Errorf("repository URL must include a branch, for example https://github.com/owner/repo/tree/main")
	}
	if strings.Contains(parts[0], "%") || strings.Contains(parts[1], "%") {
		return RepositorySpec{}, fmt.Errorf("repository owner and name must use their canonical form")
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
	if owner == "" || name == "" || branch == "" || !remoteComponentPattern.MatchString(owner) || !remoteComponentPattern.MatchString(name) || owner == "." || owner == ".." || name == "." || name == ".." {
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
		value := strings.TrimPrefix(raw, "git@github.com:")
		if strings.ContainsAny(value, "?#@:\\") {
			return "", fmt.Errorf("remote SSH path is invalid")
		}
		return canonicalOwnerRepo(value)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" && u.Scheme != "ssh" {
		return "", fmt.Errorf("remote scheme must be https or ssh")
	}
	if !strings.EqualFold(u.Hostname(), "github.com") || u.Port() != "" {
		return "", fmt.Errorf("remote is not hosted on github.com")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("remote URL cannot contain query parameters or fragments")
	}
	if u.Scheme == "https" && u.User != nil {
		return "", fmt.Errorf("remote URL cannot contain credentials or query parameters; use a credential manager")
	}
	if u.Scheme == "ssh" {
		if u.User == nil || u.User.Username() != "git" {
			return "", fmt.Errorf("SSH remotes must use the git user")
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			return "", fmt.Errorf("remote URL cannot contain credentials")
		}
	}
	if u.RawPath != "" || strings.Contains(u.EscapedPath(), "%") || !strings.HasPrefix(u.Path, "/") {
		return "", fmt.Errorf("remote path must use its canonical form")
	}
	return canonicalOwnerRepo(strings.TrimPrefix(u.Path, "/"))
}

func canonicalOwnerRepo(value string) (string, error) {
	if value == "" || strings.Trim(value, "/") != value || strings.Contains(value, "//") {
		return "", fmt.Errorf("remote path must use its canonical form")
	}
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return "", fmt.Errorf("remote must identify one GitHub owner and repository")
	}
	owner := strings.TrimSpace(parts[0])
	name := strings.TrimSuffix(strings.TrimSpace(parts[1]), ".git")
	if owner == "" || name == "" || owner == "." || owner == ".." || name == "." || name == ".." || !remoteComponentPattern.MatchString(owner) || !remoteComponentPattern.MatchString(name) {
		return "", fmt.Errorf("remote owner and repository cannot be empty")
	}
	return strings.ToLower(owner + "/" + name), nil
}
