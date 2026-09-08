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
- Windows: `%UserProfile%\.config\repo-sync\config.yaml`

On Windows, Repo Sync copies an existing `config.yaml` and `state.json` from `%AppData%\repo-sync` the first time the stable location is used. Existing files in the stable location always win, and the legacy files are left untouched. This avoids the different AppData paths seen by packaged applications and Task Scheduler.

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

The installers verify release checksums and the staged binary, add the install directory to the user's PATH, and preserve a running service across upgrades. If validation or restart fails, the previous executable is restored. Pin a version with `REPO_SYNC_VERSION` or opt out of PATH changes with `REPO_SYNC_NO_PATH=1`:

```sh
curl -fsSL https://raw.githubusercontent.com/JorgeMuehlebach/repo-sync/main/scripts/install.sh | REPO_SYNC_VERSION=0.2.0 sh
```

```powershell
$env:REPO_SYNC_VERSION = "0.2.0"
irm https://raw.githubusercontent.com/JorgeMuehlebach/repo-sync/main/scripts/install.ps1 | iex
```

You can also download a binary from [GitHub Releases](https://github.com/JorgeMuehlebach/repo-sync/releases) or build it with Go 1.23 or newer:

```sh
go build -o repo-sync ./cmd/repo-sync
```

## Set up

Bootstrap an existing checkout directly, without scanning the home directory:

```sh
repo-sync bootstrap /path/to/docs --start
```

You can also bootstrap a branch URL. Repo Sync adopts an already recorded checkout or clones it to `~/repo-name`:

```sh
repo-sync bootstrap https://github.com/owner/docs/tree/main --start
```

For several preconfigured repositories, the original `config add` plus `setup` workflow remains available. Setup searches the user's home directory and optional `search_roots`; bootstrap never does. Hidden, system, dependency, and common build directories are skipped.

## Commands

```text
repo-sync bootstrap <path|branch-url>    Configure one checkout without a home scan
  --start                                Enable synchronization after bootstrap
repo-sync setup                         Discover or clone configured repositories
repo-sync start                         Install, enable, and start the user service
repo-sync stop                          Stop and disable synchronization
repo-sync status                        Show service and repository status
repo-sync sync                          Run one synchronization immediately
repo-sync sync --dry-run                Validate and show the planned Git actions
repo-sync doctor                        Check Git, config, checkouts, remotes, and service
repo-sync uninstall                     Remove the service but preserve configuration
repo-sync config add <branch-url>       Add a repository branch
repo-sync config remove <branch-url>    Remove a repository branch
repo-sync config list                   List configured repositories
repo-sync version                       Print the installed version
```

The background integration uses LaunchAgents on macOS, systemd user services on Linux, and a current-user Task Scheduler entry on Windows.

`sync --dry-run` does not acquire locks, write state or logs, or run `git add`, `commit`, `fetch`, `rebase`, or `push`. `doctor` is also read-only; its remote check uses `git ls-remote` and therefore requires network access.

## Synchronization behavior

For each configured checkout, Repo Sync:

1. Verifies that `origin` matches the configured GitHub repository.
2. Pauses if the configured branch is not currently checked out or another Git operation is active.
3. Stages all changes with `git add -A` and commits when necessary.
4. Fetches the configured branch and rebases onto `origin/<branch>`.
5. Pushes, retrying a short push race up to three times.
6. Records the result and continues with the other repositories.

Repo Sync never force-pushes, hard-resets, switches branches, or bypasses Git hooks. A failed rebase is aborted and reported by `repo-sync status`; the local commit remains intact.

## Releases and package managers

Release archives are checksum-verified and smoke-tested on native Windows, macOS, and Linux AMD64 runners before publication. GitHub build provenance can be verified with:

```sh
gh attestation verify --owner JorgeMuehlebach repo-sync_VERSION_OS_ARCH.EXT
```

Homebrew, Scoop, and Winget publication is intentionally handled after a release exists, because their manifests require the immutable release URLs and final SHA-256 values. The direct installers remain the supported installation path until those external tap, bucket, and `winget-pkgs` submissions are published.

## Roadmap

- Guided conflict recovery and optional automatic conflict policies.
- Native desktop notifications.
- Published Homebrew, Scoop, and Winget packages.
- Configurable per-repository intervals and staging policies.
- Support for Git hosts other than GitHub.

## Development

```sh
go test ./...
go vet ./...
go build ./cmd/repo-sync
```

Repo Sync is released under the [MIT License](LICENSE).
