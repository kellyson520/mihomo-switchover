package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateStoreAtomicallyRecoversCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path, "MAIN")
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.CurrentChannel != "MAIN" {
		t.Fatalf("state=%+v", state)
	}
	matches, _ := filepath.Glob(path + ".corrupt.*")
	if len(matches) != 1 {
		t.Fatalf("corrupt backup count=%d", len(matches))
	}
}

func TestStateStoreRoundTripsProviderLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path, "MAIN")
	want := Default("MAIN")
	want.CurrentChannel = "BACKUP-USA"
	want.ProviderLocks["main"] = ProviderLock{Provider: "main", Group: "MAIN", Node: "节点-01"}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentChannel != want.CurrentChannel || got.ProviderLocks["main"].Node != "节点-01" {
		t.Fatalf("got=%+v want=%+v", got, want)
	}
}
