package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestHoldServerLetsGoWhenBusy keeps a busy hold letting go of the locks it took before the
// busy one: its caller gets nothing to hold, and a caller that went on would keep other
// environments locked.
func TestHoldServerLetsGoWhenBusy(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"prod", "stage"} {
		if err := os.MkdirAll(filepath.Join(root, "shop", env, "releases"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bases, err := deployEnvBases(root)
	if err != nil || len(bases) != 2 {
		t.Fatalf("bases %v: %v", bases, err)
	}
	// The last environment's lock held by another, as a release holds it.
	other, err := os.OpenFile(filepath.Join(bases[1], ".lock"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if ok, err := tryFlock(other, syscall.LOCK_EX); !ok || err != nil {
		t.Fatal("cannot take", bases[1], err)
	}
	hold, busy, err := holdServer(root, holdHow{})
	if err != nil || busy == "" || hold != nil {
		t.Fatalf("hold %v, busy %q, err %v", hold, busy, err)
	}
	// The first environment's lock, which holdServer took before it found the busy one, must be
	// free again.
	first, err := os.OpenFile(filepath.Join(bases[0], ".lock"), os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if ok, err := tryFlock(first, syscall.LOCK_EX); !ok || err != nil {
		t.Errorf("a busy holdServer left %s locked (%v)", bases[0], err)
	}
}
