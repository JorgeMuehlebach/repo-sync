package discovery

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

var skippedDirectories = map[string]bool{
	"Library":          true,
	"AppData":          true,
	"Application Data": true,
	"Local Settings":   true,
	"node_modules":     true,
	"vendor":           true,
	"target":           true,
	"dist":             true,
	"build":            true,
	".cache":           true,
}

func Find(ctx context.Context, roots []string, wanted map[string]bool) (map[string][]string, error) {
	result := make(map[string][]string)
	seenRoots := make(map[string]bool)
	for _, root := range roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		if seenRoots[absolute] {
			continue
		}
		seenRoots[absolute] = true
		err = filepath.WalkDir(absolute, func(current string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if entry != nil && entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !entry.IsDir() {
				return nil
			}
			name := entry.Name()
			if name == ".git" {
				repoPath := filepath.Dir(current)
				remote, err := gitOutput(ctx, repoPath, "config", "--get", "remote.origin.url")
				if err == nil {
					key, err := CanonicalRemote(remote)
					if err == nil && wanted[key] {
						result[key] = append(result[key], repoPath)
					}
				}
				return filepath.SkipDir
			}
			if current != absolute && (strings.HasPrefix(name, ".") || skippedDirectories[name]) {
				return filepath.SkipDir
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("search %s: %w", absolute, err)
		}
	}
	for key := range result {
		sort.Strings(result[key])
	}
	return result, nil
}

func ValidatePath(ctx context.Context, repoPath string, spec RepositorySpec) error {
	root, err := gitOutput(ctx, repoPath, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("%s is not a Git working tree", repoPath)
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absolutePath, err := filepath.Abs(repoPath)
	if err != nil {
		return err
	}
	rootInfo, rootErr := os.Stat(absoluteRoot)
	pathInfo, pathErr := os.Stat(absolutePath)
	if rootErr != nil || pathErr != nil || !os.SameFile(rootInfo, pathInfo) {
		return fmt.Errorf("%s is inside a repository; enter its root %s", repoPath, absoluteRoot)
	}
	remote, err := gitOutput(ctx, repoPath, "config", "--get", "remote.origin.url")
	if err != nil {
		return fmt.Errorf("read origin remote: %w", err)
	}
	key, err := CanonicalRemote(remote)
	if err != nil || key != spec.Key {
		return fmt.Errorf("%s origin does not match %s", repoPath, spec.Key)
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}
