package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInstallersQuiesceBeforeReplacingExistingBinary(t *testing.T) {
	for _, test := range []struct {
		name        string
		path        string
		orderedText []string
	}{
		{
			name: "posix",
			path: "install.sh",
			orderedText: []string{
				`if ! "$installed_binary" stop`,
				`wait_for_legacy_locks "$legacy_config_dir"`,
				`install -m 0755 "${temporary_dir}/repo-sync" "$candidate"`,
				`mv "$candidate" "$installed_binary"`,
			},
		},
		{
			name: "powershell",
			path: "install.ps1",
			orderedText: []string{
				`& $InstalledBinary stop`,
				`Wait-LegacyLocksReleased (Join-Path $env:APPDATA "repo-sync")`,
				`Copy-Item -LiteralPath $staged -Destination $candidate`,
				`Move-Item -LiteralPath $candidate -Destination $installedBinary`,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := os.ReadFile(test.path)
			if err != nil {
				t.Fatal(err)
			}
			previous := -1
			for _, text := range test.orderedText {
				index := strings.Index(string(data), text)
				if index < 0 {
					t.Fatalf("installer lacks required upgrade step %q", text)
				}
				if index <= previous {
					t.Fatalf("upgrade step %q occurs before its prerequisite", text)
				}
				previous = index
			}
		})
	}
}

func TestPOSIXInstallerParses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell is not required on Windows")
	}
	path, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is unavailable")
	}
	if output, err := exec.Command(path, "-n", "install.sh").CombinedOutput(); err != nil {
		t.Fatalf("install.sh does not parse: %v\n%s", err, output)
	}
}

func TestPOSIXInstallerStopsAndWaitsBeforeReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX installer is not used on Windows")
	}
	root := t.TempDir()
	fakeBin := filepath.Join(root, "bin")
	installDir := filepath.Join(root, "install")
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	newBinary := filepath.Join(root, "new-repo-sync")
	writeExecutable(t, newBinary, `#!/bin/sh
case "${1:-}" in
  version) printf '%s\n' '9.9.9' ;;
  *) exit 0 ;;
esac
`)
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
set -eu
url=""
output=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) shift; output=$1 ;;
    http*) url=$1 ;;
  esac
  shift
done
case "$url" in
  */releases/latest) printf '%s\n' '{"tag_name":"v9.9.9"}' ;;
  */SHA256SUMS) printf '%s  %s\n' "$REPO_SYNC_TEST_DIGEST" "$REPO_SYNC_TEST_ARCHIVE" > "$output" ;;
  *) : > "$output" ;;
esac
`)
	writeExecutable(t, filepath.Join(fakeBin, "sha256sum"), `#!/bin/sh
printf '%s  %s\n' "$REPO_SYNC_TEST_DIGEST" "$1"
`)
	writeExecutable(t, filepath.Join(fakeBin, "tar"), `#!/bin/sh
set -eu
destination=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-C" ]; then
    shift
    destination=$1
  fi
  shift
done
cp "$REPO_SYNC_TEST_NEW_BINARY" "$destination/repo-sync"
chmod 0755 "$destination/repo-sync"
`)

	targetOS := "linux"
	legacyConfig := filepath.Join(root, "config", "repo-sync")
	if runtime.GOOS == "darwin" {
		targetOS = "darwin"
		legacyConfig = filepath.Join(home, "Library", "Application Support", "repo-sync")
	}
	legacyLock := filepath.Join(legacyConfig, "locks", "repository.lock")
	writeExecutable(t, filepath.Join(fakeBin, "install"), `#!/bin/sh
set -eu
if [ -e "$REPO_SYNC_TEST_LOCK" ]; then
  exit 88
fi
source=""
destination=""
for argument in "$@"; do
  source=$destination
  destination=$argument
done
cp "$source" "$destination"
chmod 0755 "$destination"
`)
	targetArch := "amd64"
	if runtime.GOARCH == "arm64" {
		targetArch = "arm64"
	}
	archive := "repo-sync_9.9.9_" + targetOS + "_" + targetArch + ".tar.gz"
	environment := []string{
		"PATH=" + fakeBin + ":/usr/bin:/bin",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"REPO_SYNC_INSTALL_DIR=" + installDir,
		"REPO_SYNC_TEST_ARCHIVE=" + archive,
		"REPO_SYNC_TEST_DIGEST=fixture-digest",
		"REPO_SYNC_TEST_NEW_BINARY=" + newBinary,
		"REPO_SYNC_TEST_LOCK=" + legacyLock,
	}

	t.Run("released legacy lock", func(t *testing.T) {
		events := filepath.Join(root, "success-events")
		installed := filepath.Join(installDir, "repo-sync")
		writeLegacyBinary(t, installed)
		if err := os.MkdirAll(filepath.Dir(legacyLock), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(legacyLock, []byte("fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			deadline := time.Now().Add(5 * time.Second)
			for {
				if data, err := os.ReadFile(events); err == nil && strings.Contains(string(data), "stop") {
					time.Sleep(150 * time.Millisecond)
					_ = os.Remove(legacyLock)
					return
				}
				if time.Now().After(deadline) {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		command := exec.Command("/bin/sh", "install.sh")
		command.Env = append(environment, "REPO_SYNC_TEST_EVENTS="+events, "REPO_SYNC_TEST_STOP_EXIT=0")
		output, err := command.CombinedOutput()
		<-done
		if err != nil {
			t.Fatalf("installer failed: %v\n%s", err, output)
		}
		data, err := os.ReadFile(installed)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != `#!/bin/sh
case "${1:-}" in
  version) printf '%s\n' '9.9.9' ;;
  *) exit 0 ;;
esac
` {
			t.Fatalf("installed binary = %q, want downloaded fixture", data)
		}
	})

	t.Run("stop failure preserves old binary", func(t *testing.T) {
		events := filepath.Join(root, "failure-events")
		installed := filepath.Join(installDir, "repo-sync")
		writeLegacyBinary(t, installed)
		before, err := os.ReadFile(installed)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command("/bin/sh", "install.sh")
		command.Env = append(environment, "REPO_SYNC_TEST_EVENTS="+events, "REPO_SYNC_TEST_STOP_EXIT=7")
		if output, err := command.CombinedOutput(); err == nil {
			t.Fatalf("installer replaced an installation it could not stop:\n%s", output)
		}
		after, err := os.ReadFile(installed)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatal("failed quiescence changed the installed binary")
		}
	})
}

func writeLegacyBinary(t *testing.T, path string) {
	t.Helper()
	writeExecutable(t, path, `#!/bin/sh
case "${1:-}" in
  version) printf '%s\n' '0.1.0' ;;
  stop) printf '%s\n' stop >> "$REPO_SYNC_TEST_EVENTS"; exit "$REPO_SYNC_TEST_STOP_EXIT" ;;
  *) exit 2 ;;
esac
`)
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestPowerShellInstallerParsesWhenAvailable(t *testing.T) {
	path, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh is unavailable")
	}
	command := `$errors = $null; [System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path 'install.ps1'), [ref]$null, [ref]$errors) > $null; if ($errors.Count) { $errors | Out-String | Write-Error; exit 1 }`
	if output, err := exec.Command(path, "-NoProfile", "-NonInteractive", "-Command", command).CombinedOutput(); err != nil {
		t.Fatalf("install.ps1 does not parse: %v\n%s", err, output)
	}
}
