package state

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := New()
	want.Enabled = true
	want.Repositories["example/docs"] = Repository{Path: "/work/docs", LastSync: time.Unix(123, 0).UTC()}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Repositories["example/docs"].Path != "/work/docs" {
		t.Fatalf("Load() = %#v", got)
	}
}
