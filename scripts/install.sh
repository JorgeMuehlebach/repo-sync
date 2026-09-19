#!/bin/sh
set -eu

repository="JorgeMuehlebach/repo-sync"
install_dir="${REPO_SYNC_INSTALL_DIR:-${HOME}/.local/bin}"
version="${REPO_SYNC_VERSION:-}"

legacy_locks_present() {
  config_dir=$1
  if [ -e "${config_dir}/config.yaml.lock" ] || [ -e "${config_dir}/state.json.lock" ]; then
    return 0
  fi
  for lock_path in "${config_dir}"/locks/*.lock; do
    if [ -e "$lock_path" ]; then
      return 0
    fi
  done
  return 1
}

wait_for_legacy_locks() {
  config_dir=$1
  remaining=30
  while legacy_locks_present "$config_dir"; do
    if [ "$remaining" -eq 0 ]; then
      echo "Repo Sync v0.1 still owns, or left behind, a legacy lock below ${config_dir}." >&2
      echo "The upgrade was not installed. Stop every old repo-sync command and retry; do not delete a live lock." >&2
      return 1
    fi
    sleep 1
    remaining=$((remaining - 1))
  done
}

quiesce_existing_install() {
  installed_binary=$1
  legacy_config_dir=$2
  if [ ! -e "$installed_binary" ]; then
    return 1
  fi
  if [ -L "$installed_binary" ] || [ ! -f "$installed_binary" ] || [ ! -x "$installed_binary" ]; then
    echo "Refusing to replace non-executable install target: ${installed_binary}" >&2
    exit 1
  fi
  if ! existing_version=$("$installed_binary" version); then
    echo "Could not identify the existing Repo Sync installation; it was not replaced." >&2
    exit 1
  fi
  echo "Stopping and disabling existing Repo Sync ${existing_version} before upgrade..."
  if ! "$installed_binary" stop; then
    echo "Existing Repo Sync could not be stopped; the upgrade was not installed." >&2
    exit 1
  fi
  case "$existing_version" in
    0.1.*|v0.1.*)
      if ! wait_for_legacy_locks "$legacy_config_dir"; then
        exit 1
      fi
      ;;
  esac
  return 0
}

case "$(uname -s)" in
  Darwin)
    target_os="darwin"
    legacy_config_dir="${HOME}/Library/Application Support/repo-sync"
    profile="${HOME}/.zprofile"
    ;;
  Linux)
    target_os="linux"
    legacy_config_dir="${XDG_CONFIG_HOME:-${HOME}/.config}/repo-sync"
    profile="${HOME}/.profile"
    ;;
  *) echo "Unsupported operating system: $(uname -s)" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  x86_64|amd64) target_arch="amd64" ;;
  arm64|aarch64) target_arch="arm64" ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

if [ -z "$version" ]; then
  release_json=$(curl -fsSL "https://api.github.com/repos/${repository}/releases/latest")
  version=$(printf '%s' "$release_json" | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)
fi
version=${version#v}
if [ -z "$version" ]; then
  echo "Could not determine the latest Repo Sync version." >&2
  exit 1
fi

archive="repo-sync_${version}_${target_os}_${target_arch}.tar.gz"
base_url="https://github.com/${repository}/releases/download/v${version}"
temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/repo-sync.XXXXXX")
destination="${install_dir}/repo-sync"
candidate="${destination}.new.$$"
backup="${destination}.previous.$$"

cleanup() {
  rm -f "$candidate"
  if [ -e "$backup" ]; then
    rm -f "$destination"
    mv "$backup" "$destination"
  fi
  case "$temporary_dir" in
    "${TMPDIR:-/tmp}"/repo-sync.*) rm -rf "$temporary_dir" ;;
  esac
}
trap cleanup EXIT INT TERM

curl -fsSL "${base_url}/${archive}" -o "${temporary_dir}/${archive}"
curl -fsSL "${base_url}/SHA256SUMS" -o "${temporary_dir}/SHA256SUMS"
expected=$(awk -v name="$archive" '$2 == name { print $1 }' "${temporary_dir}/SHA256SUMS")
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "${temporary_dir}/${archive}" | awk '{print $1}')
else
  actual=$(shasum -a 256 "${temporary_dir}/${archive}" | awk '{print $1}')
fi
if [ -z "$expected" ] || [ "$actual" != "$expected" ]; then
  echo "Checksum verification failed for ${archive}." >&2
  exit 1
fi

tar -xzf "${temporary_dir}/${archive}" -C "$temporary_dir"
reported_version=$("${temporary_dir}/repo-sync" version)
if [ "$reported_version" != "$version" ]; then
  echo "The staged executable reported version '${reported_version}'; expected '${version}'." >&2
  exit 1
fi
mkdir -p "$install_dir"
installed_binary="$destination"
upgraded=0
if [ -e "$installed_binary" ]; then
  quiesce_existing_install "$installed_binary" "$legacy_config_dir"
  upgraded=1
fi
if [ -e "$candidate" ] || [ -e "$backup" ]; then
  echo "Refusing to overwrite an existing installer recovery file." >&2
  exit 1
fi
install -m 0755 "${temporary_dir}/repo-sync" "$candidate"
if [ -e "$installed_binary" ]; then
  mv "$installed_binary" "$backup"
fi
if ! mv "$candidate" "$installed_binary"; then
  if [ -e "$backup" ]; then mv "$backup" "$installed_binary"; fi
  exit 1
fi
installed_version=$("$installed_binary" version 2>/dev/null || true)
if [ "$installed_version" != "$version" ]; then
  rm -f "$installed_binary"
  if [ -e "$backup" ]; then mv "$backup" "$installed_binary"; fi
  echo "The installed executable reported version '${installed_version}'; expected '${version}'." >&2
  exit 1
fi
rm -f "$backup"
echo "Installed repo-sync ${version} to ${installed_binary}"
if [ "$upgraded" -eq 1 ]; then
  echo "The upgraded service remains disabled. Reconcile setup, then run repo-sync start."
fi
case ":${PATH}:" in
  *":${install_dir}:"*) ;;
  *)
    if [ "${REPO_SYNC_NO_PATH:-0}" != "1" ]; then
      escaped_dir=$(printf '%s' "$install_dir" | sed "s/'/'\\\\''/g")
      path_line="export PATH='${escaped_dir}':\"\$PATH\" # repo-sync"
      if ! grep -Fqx "$path_line" "$profile" 2>/dev/null; then
        printf '\n%s\n' "$path_line" >> "$profile"
        echo "Added ${install_dir} to PATH in ${profile}. Open a new shell to use it."
      fi
    else
      echo "Add ${install_dir} to PATH to run repo-sync."
    fi
    ;;
esac
