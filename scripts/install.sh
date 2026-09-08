#!/bin/sh
set -eu

repository="JorgeMuehlebach/repo-sync"
install_dir="${REPO_SYNC_INSTALL_DIR:-${HOME}/.local/bin}"
version="${REPO_SYNC_VERSION:-}"

case "$(uname -s)" in
  Darwin) target_os="darwin"; profile="${HOME}/.zprofile" ;;
  Linux) target_os="linux"; profile="${HOME}/.profile" ;;
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
candidate="${destination}.new"
backup="${destination}.previous"
was_running=0
stopped=0

restore_previous() {
  rm -f "$candidate"
  if [ -f "$backup" ]; then
    rm -f "$destination"
    mv "$backup" "$destination"
  fi
  if [ "$was_running" -eq 1 ] && [ "$stopped" -eq 1 ] && [ -x "$destination" ]; then
    "$destination" start >/dev/null 2>&1 || true
  fi
  stopped=0
}

cleanup() {
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
  echo "Checksum verification failed for $archive." >&2
  exit 1
fi

tar -xzf "${temporary_dir}/${archive}" -C "$temporary_dir"
reported_version=$("${temporary_dir}/repo-sync" version)
if [ "$reported_version" != "$version" ]; then
  echo "The staged executable reported version '$reported_version'; expected '$version'." >&2
  exit 1
fi

mkdir -p "$install_dir"
install -m 0755 "${temporary_dir}/repo-sync" "$candidate"
if [ -x "$destination" ]; then
  status_output=$("$destination" status 2>&1 || true)
  if printf '%s\n' "$status_output" | grep -Eq '^Service:[[:space:]]+running[[:space:]]*$'; then
    was_running=1
    if ! "$destination" stop; then
      rm -f "$candidate"
      echo "Could not stop the running Repo Sync service." >&2
      exit 1
    fi
    stopped=1
  fi
  rm -f "$backup"
  if ! mv "$destination" "$backup"; then
    rm -f "$candidate"
    if [ "$was_running" -eq 1 ]; then "$destination" start >/dev/null 2>&1 || true; fi
    stopped=0
    exit 1
  fi
fi

if ! mv "$candidate" "$destination"; then
  restore_previous
  exit 1
fi
installed_version=$("$destination" version 2>/dev/null || true)
if [ "$installed_version" != "$version" ]; then
  restore_previous
  echo "The installed executable reported version '$installed_version'; expected '$version'." >&2
  exit 1
fi
if [ "$was_running" -eq 1 ] && ! "$destination" start; then
  restore_previous
  echo "The upgraded Repo Sync service did not restart; the previous executable was restored." >&2
  exit 1
fi
rm -f "$backup"

case ":${PATH}:" in
  *":${install_dir}:"*) ;;
  *)
    if [ "${REPO_SYNC_NO_PATH:-0}" != "1" ]; then
      escaped_dir=$(printf '%s' "$install_dir" | sed "s/'/'\\\\''/g")
      path_line="export PATH='${escaped_dir}':\"\$PATH\" # repo-sync"
      if ! grep -Fqx "$path_line" "$profile" 2>/dev/null; then
        printf '\n%s\n' "$path_line" >> "$profile"
        echo "Added $install_dir to PATH in $profile. Open a new shell to use it."
      fi
    fi
    ;;
esac
echo "Installed repo-sync ${version} to ${destination}"
