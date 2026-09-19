package validation

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSystemExecutorScrubsInheritedInjectionEnvironment(t *testing.T) {
	if os.Getenv("REPO_SYNC_ENV_HELPER") == "1" {
		_, _ = fmt.Fprintf(os.Stdout, "%s|%s|%s|%s", os.Getenv("GIT_DIR"), os.Getenv("git_dir"), os.Getenv("GCM_TRAP"), os.Getenv("PYTHONPATH"))
		os.Exit(0)
	}
	t.Setenv("REPO_SYNC_ENV_HELPER", "1")
	t.Setenv("GIT_DIR", "unsafe")
	t.Setenv("git_dir", "unsafe-lower")
	t.Setenv("GCM_TRAP", "unsafe")
	t.Setenv("PYTHONPATH", "unsafe")
	var stdout, stderr bytes.Buffer
	exitCode, err := (SystemExecutor{}).Run(context.Background(), os.Args[0], []string{"-test.run=^TestSystemExecutorScrubsInheritedInjectionEnvironment$"}, &stdout, &stderr)
	if err != nil || exitCode != 0 {
		t.Fatalf("Run() = %d, %v, stderr %q", exitCode, err, stderr.String())
	}
	if stdout.String() != "|||" {
		t.Fatalf("unsafe environment reached child: %q", stdout.String())
	}
}

func TestSystemExecutorReturnsPromptlyAfterDeadline(t *testing.T) {
	if os.Getenv("REPO_SYNC_BLOCKING_HELPER") == "1" {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	t.Setenv("REPO_SYNC_BLOCKING_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	exitCode, err := (SystemExecutor{}).Run(ctx, os.Args[0], []string{"-test.run=^TestSystemExecutorReturnsPromptlyAfterDeadline$"}, io.Discard, io.Discard)
	if time.Since(started) > 2*time.Second {
		t.Fatalf("executor exceeded bounded shutdown window")
	}
	if exitCode == 0 || err == nil || (!strings.Contains(err.Error(), "killed") && !strings.Contains(err.Error(), "canceled") && !strings.Contains(err.Error(), "deadline")) {
		t.Fatalf("Run() = %d, %v", exitCode, err)
	}
}
