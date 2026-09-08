# Repository guidance

- Keep Repo Sync small and dependency-light.
- Preserve the system Git executable as the authority for authentication, hooks, LFS, filters, and repository behavior.
- Never force-push, hard-reset, or automatically resolve conflicts.
- Keep portable configuration separate from machine-local resolved paths and runtime state.
- Run `go test ./...`, `go vet ./...`, and `go build ./cmd/repo-sync` before pushing.
