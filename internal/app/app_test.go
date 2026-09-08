package app

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/JorgeMuehlebach/repo-sync/internal/config"
)

func TestConfigAddListAndRemove(t *testing.T) {
	dir := t.TempDir()
	var output bytes.Buffer
	application := &Application{
		version:    "test",
		in:         bytes.NewBuffer(nil),
		out:        &output,
		errOut:     &output,
		configDir:  dir,
		configPath: filepath.Join(dir, "config.yaml"),
		statePath:  filepath.Join(dir, "state.json"),
		lockDir:    filepath.Join(dir, "locks"),
		homeDir:    dir,
	}
	branchURL := "https://github.com/owner/repo/tree/main"
	if err := application.Run([]string{"config", "add", branchURL}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(application.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 1 || cfg.Repositories[0] != branchURL {
		t.Fatalf("repositories = %#v", cfg.Repositories)
	}
	output.Reset()
	if err := application.Run([]string{"config", "list"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte(branchURL)) {
		t.Fatalf("config list output = %q", output.String())
	}
	if err := application.Run([]string{"config", "remove", branchURL}); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(application.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 0 {
		t.Fatalf("repositories after remove = %#v", cfg.Repositories)
	}
}
