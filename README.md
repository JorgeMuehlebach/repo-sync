# Repo Sync

Repo Sync is a cross-platform CLI and per-user service for synchronizing explicitly configured GitHub branches. Each repository operates in one of two modes:

- `publish` captures local edits, validates the exact immutable candidate tree, commits it, rebases safely, validates again, and pushes.
- `mirror` fetches and validates the configured `main` branch into a private immutable generation, then atomically retargets a stable filesystem pointer without creating local commits.

Every promotion is fail-closed behind an independently installed `contextctl` validator. A missing validator, changed trust state, invalid report, timeout, dirty mirror, branch mismatch, divergence, unsafe Git configuration, or failed validation blocks publication. A rare post-promotion verification failure triggers conservative rollback; rollback refuses to overwrite concurrent edits and reports that manual recovery is required.

## Configuration and machine-local state

Portable configuration uses stable IDs and contains no checkout paths:

```yaml
interval: 5m
repositories:
  - id: personal-context
    url: https://github.com/example/llm-config/tree/main
    mode: publish
    source_id: example.personal-context
  - id: bird-deter-context
    url: https://github.com/example/bird-deter/tree/main
    mode: mirror
    source_id: example.bird-deter-context
```

The interval must be a whole number of seconds from `1s` through `24h`. At most 256 repositories may be configured. Mirror mode is intentionally limited to `main`.

One source/branch may have one publisher entry and one mirror entry so a computer can keep separate development and read-only context clones. IDs and checkout paths remain unique, and duplicate source/mode or branch/mode entries are rejected.

Repo Sync stores files below the operating system's user configuration directory:

- macOS: `~/Library/Application Support/repo-sync/`
- Linux: `${XDG_CONFIG_HOME:-~/.config}/repo-sync/`
- Windows: `%UserProfile%\.config\repo-sync\`

On Windows, Repo Sync copies an existing `config.yaml` and `state.json` from
`%AppData%\repo-sync` the first time the stable location is used. Existing files
in the stable location win, and the legacy files remain available for rollback.

`config.yaml` is portable configuration. `state.json` contains machine-local checkout paths, pinned validator identities, and operational state. Its private Repo Sync schema is independent of the shared context contract. `context-status.json` is an atomically maintained, bounded, sanitized schema-2 `repo-sync.context-status.v2` document for Context Doctor. It contains no checkout path, executable path, remote URL, hostname, username, command output, or notification text.

The service refreshes `context-status.json` every 30 seconds independently of the synchronization interval. Its `generated_at` value lets consumers treat a formerly running but crashed service as stale. Writers use an operating-system file lock, so the CLI and service cannot race each other. Repo Sync withholds a refresh if any configured repository is not reconciled instead of publishing a deceptively healthy partial document.

Legacy scalar repository entries remain readable only to support migration. The service will not start until every entry has `id`, `mode`, and `source_id` and has completed validation setup.

## Install

macOS or Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/JorgeMuehlebach/repo-sync/main/scripts/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/JorgeMuehlebach/repo-sync/main/scripts/install.ps1 | iex
```

The installers verify the release checksum and the staged binary's reported
version before replacement. Pin a release with `REPO_SYNC_VERSION`; set
`REPO_SYNC_NO_PATH=1` to leave the user PATH unchanged. An existing service is
always stopped before replacement and remains disabled until setup and status
have been reconciled explicitly.

```sh
curl -fsSL https://raw.githubusercontent.com/JorgeMuehlebach/repo-sync/main/scripts/install.sh \
  | REPO_SYNC_VERSION=0.2.0 sh
```

```powershell
$env:REPO_SYNC_VERSION = "0.2.0"
irm https://raw.githubusercontent.com/JorgeMuehlebach/repo-sync/main/scripts/install.ps1 | iex
```

You can also download a binary from [GitHub Releases](https://github.com/JorgeMuehlebach/repo-sync/releases) or build with Go 1.23 or newer:

```sh
go build -o repo-sync ./cmd/repo-sync
```

### Upgrade from v0.1

The v0.1 path-existence locks are not compatible with the current
operating-system locks. Both installers therefore treat an existing binary as
an upgrade boundary: they invoke that exact installed binary's `stop` command
before replacement, abort if it cannot be identified or stopped, and wait for
v0.1 configuration, state, and repository lock files to disappear. They never
delete a legacy lock. A timeout means an old service or manual `repo-sync`
command may still be active and the binary is left unchanged.

An upgraded service remains stopped and disabled. Complete the current
`repo-sync setup` reconciliation, verify `repo-sync status`, and explicitly run
`repo-sync start`. Do not run an old manual sync in parallel with the upgrade.
Direct in-place binary replacement from v0.1 is unsupported; if installing a
download manually, first run the old binary's `stop` command and wait for all
old Repo Sync commands to exit.

The v2 workflow folds the earlier standalone `bootstrap` command into the
first targeted `setup`, replaces `sync --dry-run` with fail-closed candidate
validation during every normal sync, and moves cross-source diagnostics to the
independently installed `contextctl doctor`. Use `repo-sync status --json` for
Repo Sync's bounded machine-readable operational state.

## Set up a repository

Add a structured entry:

```sh
repo-sync config add https://github.com/example/bird-deter/tree/main \
  --id bird-deter-context \
  --mode mirror \
  --source example.bird-deter-context
```

Every setup invocation requires the v2 machine registry because Repo Sync uses
the registry-pinned Git executable and never falls back to inherited `PATH`.
For a new mirror, bootstrap its stable pointer before registering the source:

```sh
repo-sync setup \
  --repository bird-deter-context \
  --path /absolute/path/to/bird-deter-context \
  --registry /absolute/path/to/machine-registry.json
```

The bootstrap creates an independent initial generation but does not mark it
accepted or expose it globally. Register that exact lexical pointer with
`contextctl source register` as role `context-mirror`, then reconcile the full
trusted validation runtime:

```sh
contextctl source register \
  --registry /absolute/path/to/machine-registry.json \
  --path /absolute/path/to/bird-deter-context \
  --role context-mirror \
  --repo-sync-id bird-deter-context \
  --repo-sync-mode mirror
```

```sh
repo-sync setup \
  --repository bird-deter-context \
  --path /absolute/path/to/bird-deter-context \
  --contextctl /absolute/path/to/contextctl \
  --registry /absolute/path/to/machine-registry.json \
  --trust-state /absolute/path/to/trusted-validator-state.json
```

`--contextctl` and `--trust-state` are supplied together; `--registry` is always
required. The configured `contextctl` executable must be a canonical regular
executable outside the source checkout. Repo Sync pins SHA-256 identities for
it, the registry, and trusted-validator state; registry and trust-state files
are size-bounded and parsed as data. The bootstrap/full-setup split is required
only when the stable mirror pointer does not yet exist.

Setup verifies that the schema-2 machine registry has exactly one matching
enabled registration at the canonical publisher root or stable lexical mirror
root. A publisher must be registered as role `writable` with Repo Sync mode
`publisher`; a mirror must use role `context-mirror` and mode `mirror`.
Publisher manifests require publication mode `repo-sync`; mirrors accept
`repo-sync` or `manual-review`. Repository ID, `origin`, GitHub remote, branch,
source ID, and `.agents/context-source.yaml` publication metadata must agree.
Start and every synchronization recheck the same relationships and pinned
files. Legacy v1 context handshakes require migration; Repo Sync supports only
contract `2.0.x`.

Without `--path`, publisher setup searches the user's home directory plus
optional `search_roots`; mirror setup prompts for its stable pointer path. If
publisher setup finds no checkout it can clone one interactively. A targeted
setup may be repeated to reconcile one repository without changing another
development clone.

After all repositories are reconciled:

```sh
repo-sync start
```

The background integration uses a LaunchAgent on macOS, a systemd user service on Linux, and a current-user Task Scheduler entry on Windows.

## Commands

```text
repo-sync setup --registry <file> [--repository <id>] [--path <path>] [--contextctl <file> --trust-state <file>]
repo-sync start
repo-sync stop
repo-sync status [--repository <id>] [--json]
repo-sync sync [--repository <id>]
repo-sync uninstall
repo-sync config add <branch-url> --id <id> --mode <publish|mirror> --source <source-id>
repo-sync config remove <branch-url-or-id>
repo-sync config list
repo-sync version
```

`status --json` renders the same full sanitized model maintained in `context-status.json`; a repository selector filters only the command's output, not the persistent full snapshot. `uninstall` removes the service and preserves configuration and state.

## Safety model

### Publish mode

Repo Sync snapshots tracked and unignored untracked files into a private temporary Git object database and temporary index. File reads reject symlinks, reparse points, unsafe ancestors, special files, traversal names, oversized files, and read races. The validator receives the immutable tree through this private object repository while the real object database remains read-only.

Only a passing schema-2 `contextctl.report.v2` report for the exact source ID and tree allows import. Loose objects are bounded, content-hash verified, and checked with `git fsck` before the validated index is promoted. A candidate rejected by this initial validation does not add its blobs to the real object database. The resulting commit is validated again after commit/rebase and immediately before its exact object ID is pushed. Repo Sync never force-pushes.

### Mirror mode

Repo Sync requires a clean accepted generation on the configured branch,
including no ignored content or hidden skip-worktree/assume-unchanged index
state. It copies that generation into private `candidate-*` storage, fetches
only the exact configured branch, validates the immutable object ID, verifies
ancestry, and permits fast-forward promotion only. After validation and
durability it renames the private candidate to `generation-*`, journals the
transition, and atomically retargets the stable pointer. A reopened,
schema-verified `context-status.json` record is the completion barrier before
the journal is finalized. Potentially exposed generations are retained so a
concurrent reader cannot lose a generation it already resolved; cleanup is
limited to marker-verified private candidates. A failed completion barrier
rolls the pointer and durable state/status back conservatively. Mirror mode
does not add, commit, rebase, push, switch branches, hard-reset, or resolve
divergence.

### Git and process isolation

Repo Sync refuses shallow or shared Git storage, grafts, alternate object stores, sparse checkout state, active Git operations, repository-local external attributes, and candidate `.gitattributes` that select executable filters or custom merge drivers. Git commands neutralize hooks, signing, editors, pagers, fsmonitor, untracked cache, replacement objects, system/global attributes, credential prompts, and inherited `GIT_*`/`GCM_*` overrides. Candidate content never selects an executable, validator, helper, or approval path.

Repo Sync reads exactly one `context-system.git` executable dependency from the
schema-v2 machine registry, verifies its canonical no-link path and SHA-256,
and re-verifies it before every invocation. On Unix it executes a private,
digest-reverified snapshot copied from the retained verified descriptor, so a
package-manager pathname swap cannot substitute different bytes between
inspection and process creation. On Windows the retained non-shareable handle
prevents ordinary write, replacement, and deletion through process creation.
Discovery, cloning, publishing, mirroring, setup, service cycles, and recovery
never fall back to inherited
`PATH`. Validation execution uses private byte-verified snapshots of
`contextctl`, the registry, and trusted state outside both source and candidate
repositories. Repository-local and worktree credential helpers are rejected,
while machine-approved system/global credential helpers remain available for
HTTPS authentication.

`contextctl` receives an exact immutable tree, emits at most 1 MiB of strict JSON and no stderr, and runs under a 15-minute outer deadline. Unknown fields, duplicate JSON keys, path-bearing diagnostics, identity mismatches, inconsistent verdicts, and exit/report disagreements are rejected.

Failures are recorded using bounded stable codes and repository-relative diagnostic paths. Native notifications are best-effort, time-bounded, and deduplicated; unavailable notification delivery is itself durable state and never changes the validation decision.

## Development

```sh
go test ./...
go vet ./...
go test -race ./...
go build ./cmd/repo-sync
GOOS=linux go build ./cmd/repo-sync
GOOS=darwin go build ./cmd/repo-sync
GOOS=windows go build ./cmd/repo-sync
```

Cross-repository integration must also run Repo Sync against the exact released `contextctl` CLI and shared JSON schemas before release. Unit tests with a fake validator do not replace that gate.

Repo Sync is released under the [MIT License](LICENSE).
