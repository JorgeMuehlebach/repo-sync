package discovery

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/JorgeMuehlebach/repo-sync/internal/gitexec"
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

type GitRunner interface {
	Run(context.Context, string, ...string) gitexec.Result
}

func Find(ctx context.Context, runner GitRunner, roots []string, wanted map[string]bool) (map[string][]string, error) {
	if runner == nil {
		return nil, fmt.Errorf("trusted Git dependency is unavailable")
	}
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
				remote, err := gitOutput(ctx, runner, repoPath, "config", "--get", "remote.origin.url")
				if errors.Is(err, gitexec.ErrDependencyUnavailable) {
					return err
				}
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

func ValidatePath(ctx context.Context, runner GitRunner, repoPath string, spec RepositorySpec) error {
	if runner == nil {
		return fmt.Errorf("trusted Git dependency is unavailable")
	}
	root, err := gitOutput(ctx, runner, repoPath, "rev-parse", "--show-toplevel")
	if err != nil {
		if errors.Is(err, gitexec.ErrDependencyUnavailable) {
			return err
		}
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
	remote, err := gitOutput(ctx, runner, repoPath, "config", "--get", "remote.origin.url")
	if err != nil {
		if errors.Is(err, gitexec.ErrDependencyUnavailable) {
			return err
		}
		return fmt.Errorf("read origin remote: %w", err)
	}
	key, err := CanonicalRemote(remote)
	if err != nil || key != spec.Key {
		return fmt.Errorf("%s origin does not match %s", repoPath, spec.Key)
	}
	return nil
}

func gitOutput(ctx context.Context, runner GitRunner, dir string, args ...string) (string, error) {
	result := runner.Run(ctx, dir, args...)
	if result.Err != nil {
		if errors.Is(result.Err, gitexec.ErrDependencyUnavailable) {
			return "", gitexec.ErrDependencyUnavailable
		}
		return "", fmt.Errorf("git %s failed", strings.Join(args, " "))
	}
	return result.Output, nil
}
