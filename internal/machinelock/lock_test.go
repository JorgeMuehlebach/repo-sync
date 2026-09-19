package machinelock

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireSerializesAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machine.lock")
	_, release, err := Acquire(path, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Acquire(path, 0, "test"); err == nil {
		t.Fatal("second owner acquired live lock")
	}
	release()
	_, release, err = Acquire(path, 0, "test")
	if err != nil {
		t.Fatalf("released lock could not be reacquired: %v", err)
	}
	release()
}

func TestAcquireReleasesOnProcessDeath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machine.lock")
	command := exec.Command(os.Args[0], "-test.run=^TestAcquireCrashHelper$")
	command.Env = append(os.Environ(), "REPO_SYNC_MACHINE_LOCK_HELPER="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("lock helper failed: %v: %s", err, output)
	}
	_, release, err := Acquire(path, time.Second, "test")
	if err != nil {
		t.Fatalf("lock survived owner process: %v", err)
	}
	release()
}

func TestAcquireCrashHelper(t *testing.T) {
	path := os.Getenv("REPO_SYNC_MACHINE_LOCK_HELPER")
	if path == "" {
		t.Skip("subprocess helper")
	}
	if _, _, err := Acquire(path, 0, "test"); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}
