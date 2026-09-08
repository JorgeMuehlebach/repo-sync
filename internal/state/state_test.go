package state

import (
	"path/filepath"
	"strconv"
	"sync"
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

func TestConcurrentUpdatesPreserveGlobalAndRepositoryState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Update(path, func(current *State) error {
		current.Enabled = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	const count = 12
	errorsFound := make(chan error, count)
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			err := Update(path, func(current *State) error {
				current.Repositories["repo-"+strconv.Itoa(index)] = Repository{Path: "/repo"}
				return nil
			})
			if err != nil {
				errorsFound <- err
			}
		}(i)
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("repository updates overwrote the disabled state")
	}
	if len(got.Repositories) != count {
		t.Fatalf("repository count = %d, want %d", len(got.Repositories), count)
	}
}
