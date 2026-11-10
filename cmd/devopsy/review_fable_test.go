package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// BUG: --project-users remove drops the user's sync keys before its processes are ended and
// outside the sync keys' lock (removeProjectUser), where a key's removal drops them after, under
// it (KeysRemoveScript, internals/door.md: "removing any of the user's keys, or the user, drops
// them all once its processes are ended, under the sync keys' lock"). A door request already
// past its sudo, which the end of the user's processes does not reach, holds that lock with its
// keys file still as it found it, and writes its line once the removal has passed: the sync
// key stays after the remove, and --authorized-keys prints it for the Unix user, which stays
// too.
//
// The add here is the door's (editSyncKeys with its check under the lock): the removal runs
// while it holds the lock, past its check, as a request in flight does.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's keys are.
func TestReviewFableRemoveDropsSyncKeysUnderTheirLock(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	who := "devopsy-shop"
	entry := remote.DoorDir() + "/users/" + who + ".yaml"
	keys, syncKeys := remote.DoorKeysFile(who), remote.DoorSyncKeysFile(who)
	write(t, entry, "project: shop\n", 0o600)
	write(t, keys, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g op@laptop\n", 0o600)
	_, pub, err := remote.NewSyncKey()
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.Fields(pub)[1]
	removed := make(chan int, 1)
	// The door's check under the lock: the keys file is the one its request found. Then the
	// removal starts, and the request goes on to write once the removal reached the keys (or,
	// waiting for this lock as it should, got no further).
	still := func() error {
		if _, err := os.Stat(keys); err != nil {
			return err
		}
		go func() {
			removed <- removeProjectUser(&remote.Door{Space: t.TempDir()}, "shop", who, entry, func(string) error { return nil })
		}()
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if _, err := os.Stat(keys); err != nil {
				break
			}
		}
		return nil
	}
	if err := editSyncKeys(syncKeys, "/usr/local/bin/devopsy", []string{"add", "1762600000.00000a", "/home/devopsy-shop/shop/dev", "vm2:dev", "-", blob}, time.Now(), still); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-removed:
		if code != 0 {
			t.Fatalf("remove: exit %d", code)
		}
	case <-time.After(startWait):
		t.Fatal("remove did not end")
	}
	if data, err := os.ReadFile(syncKeys); err == nil {
		t.Errorf("BUG: a sync key added by a request in flight outlives --project-users remove:\n%s", data)
	}
}
