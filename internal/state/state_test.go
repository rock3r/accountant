package state

import (
	"os"
	"sync"
	"testing"
	"time"
)

func TestAtomicWritePermissions(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"
	st := New()
	st.Account("a1").AuthBroken = true
	if err := Save(path, st); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("state file perms %04o, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0700 {
		t.Fatalf("state dir perms %04o, want 0700", di.Mode().Perm())
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Accounts["a1"].AuthBroken {
		t.Fatal("round-trip lost auth_broken")
	}
}

func TestLockedRMWUnderParallelLoad(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir + "/state.json")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				id := "a" + string(rune('0'+i%4))
				err := store.Locked(func(st *State) (bool, error) {
					st.Account(id).UpdatedAt = time.Now()
					st.SetPin("anthropic", "s", id, time.Now())
					return true, nil
				})
				if err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	st, err := Load(store.File())
	if err != nil {
		t.Fatalf("state file must stay valid JSON under parallel load: %v", err)
	}
	if len(st.Pins) != 1 {
		t.Fatalf("pins: %d", len(st.Pins))
	}
}

func TestLockedSkipsWriteWhenClean(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir + "/state.json")
	if err := store.Locked(func(st *State) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.File()); !os.IsNotExist(err) {
		t.Fatal("clean RMW must not create the file")
	}
}

func TestLoadMissingFile(t *testing.T) {
	st, err := Load(t.TempDir() + "/missing.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Accounts) != 0 {
		t.Fatal("want empty state")
	}
}
