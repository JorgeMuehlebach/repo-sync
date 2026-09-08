# Repo Sync

Repo Sync is a small cross-platform CLI that keeps selected GitHub repository branches synchronized across your computers. It discovers existing clones, remembers machine-specific paths, and runs as a per-user background service every five minutes.

> [!WARNING]
> Repo Sync automatically runs `git add -A`, commits every file not excluded by `.gitignore`, rebases onto the configured remote branch, and pushes. Review each repository's ignore rules before enabling the service. Repo Sync never stores GitHub credentials; it uses your installed Git and existing SSH agent or credential manager.

## Configuration

Repo Sync accepts GitHub branch links so the repository and branch are both explicit:

```yaml
interval: 5m
repositories:
  - https://github.com/owner/docs/tree/main
  - https://github.com/owner/skills/tree/master
```

The portable YAML configuration is stored in the operating system's user configuration directory:

- macOS: `~/Library/Application Support/repo-sync/config.yaml`
- Linux: `${XDG_CONFIG_HOME:-~/.config}/repo-sync/config.yaml`
- Windows: `%AppData%\repo-sync\config.yaml`

Resolved checkout paths and runtime status are stored separately in `state.json`, allowing the same repository list to be used on computers with different directory layouts.

## Install

macOS or Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/JorgeMuehlebach/repo-sync/main/scripts/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/JorgeMuehlebach/repo-sync/main/scripts/install.ps1 | iex
```

You can also download a binary from [GitHub Releases](https://github.com/JorgeMuehlebach/repo-sync/releases) or build it with Go 1.23 or newer:

```sh
go build -o repo-sync ./cmd/repo-sync
```

## Set up

Add one or more branch URLs, discover or clone them, and start the service:

```sh
repo-sync config add https://github.com/owner/docs/tree/main
repo-sync setup
repo-sync start
```

Setup searches the user's home directory and optional `search_roots`. Hidden, system, dependency, and common build directories are skipped. When multiple clones match, setup asks which one to use. When none match, it asks for an existing path or a destination to clone.

## Commands

```text
repo-sync setup                         Discover or clone configured repositories
repo-sync start                         Install, enable, and start the user service
repo-sync stop                          Stop and disable synchronization
repo-sync status                        Show service and repository status
repo-sync sync                          Run one synchronization immediately
repo-sync uninstall                     Remove the service but preserve configuration
repo-sync config add <branch-url>       Add a repository branch
repo-sync config remove <branch-url>    Remove a repository branch
repo-sync config list                   List configured repositories
repo-sync version                       Print the installed version
```

The background integration uses LaunchAgents on macOS, systemd user services on Linux, and a current-user Task Scheduler entry on Windows.

## Synchronization behavior

For each configured checkout, Repo Sync:

1. Verifies that `origin` matches the configured GitHub repository.
2. Pauses if the configured branch is not currently checked out or another Git operation is active.
3. Stages all changes with `git add -A` and commits when necessary.
4. Fetches the configured branch and rebases onto `origin/<branch>`.
5. Pushes, retrying a short push race up to three times.
6. Records the result and continues with the other repositories.

Repo Sync never force-pushes, hard-resets, switches branches, or bypasses Git hooks. A failed rebase is aborted and reported by `repo-sync status`; the local commit remains intact.

## Roadmap

- Guided conflict recovery and optional automatic conflict policies.
- Native desktop notifications.
- Homebrew, Scoop, and additional package-manager distribution.
- Configurable per-repository intervals and staging policies.
- Support for Git hosts other than GitHub.

## Development

```sh
go test ./...
go vet ./...
go build ./cmd/repo-sync
```

Repo Sync is released under the [MIT License](LICENSE).
