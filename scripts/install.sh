#!/bin/sh
set -eu

repository="JorgeMuehlebach/repo-sync"
install_dir="${REPO_SYNC_INSTALL_DIR:-${HOME}/.local/bin}"

case "$(uname -s)" in
  Darwin) target_os="darwin" ;;
  Linux) target_os="linux" ;;
  *) echo "Unsupported operating system: $(uname -s)" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  x86_64|amd64) target_arch="amd64" ;;
  arm64|aarch64) target_arch="arm64" ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

release_json=$(curl -fsSL "https://api.github.com/repos/${repository}/releases/latest")
version=$(printf '%s' "$release_json" | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)
if [ -z "$version" ]; then
  echo "Could not determine the latest Repo Sync version." >&2
  exit 1
fi

archive="repo-sync_${version}_${target_os}_${target_arch}.tar.gz"
base_url="https://github.com/${repository}/releases/download/v${version}"
temporary_dir=$(mktemp -d)
trap 'rm -rf "$temporary_dir"' EXIT INT TERM

curl -fsSL "${base_url}/${archive}" -o "${temporary_dir}/${archive}"
curl -fsSL "${base_url}/SHA256SUMS" -o "${temporary_dir}/SHA256SUMS"
expected=$(awk -v name="$archive" '$2 == name { print $1 }' "${temporary_dir}/SHA256SUMS")
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "${temporary_dir}/${archive}" | awk '{print $1}')
else
  actual=$(shasum -a 256 "${temporary_dir}/${archive}" | awk '{print $1}')
fi
if [ -z "$expected" ] || [ "$actual" != "$expected" ]; then
  echo "Checksum verification failed." >&2
  exit 1
fi

tar -xzf "${temporary_dir}/${archive}" -C "$temporary_dir"
mkdir -p "$install_dir"
install -m 0755 "${temporary_dir}/repo-sync" "${install_dir}/repo-sync"
echo "Installed repo-sync ${version} to ${install_dir}/repo-sync"
case ":${PATH}:" in
  *":${install_dir}:"*) ;;
  *) echo "Add ${install_dir} to PATH to run repo-sync." ;;
esac
