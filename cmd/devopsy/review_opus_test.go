package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// BUG: a door request that a leaked key's process starts after the key's removal (its mv),
// but before the end reaches that process, takes keysAt from the keys file the removal left,
// so `still` holds for it: once past its sudo it outlives the end, and a sync key it adds
// after endProjectUser dropped the user's sync keys stays, a login for its sync's requests
// past the removal, for the key's life. keysAt marks the keys' change, but the end is what
// the request has to predate.
//
// Not parallel: it sets DEVOPSY_HOME (the door's directory).
func TestReviewOpusSyncKeyAddedPastTheEnd(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	tree := filepath.Join(t.TempDir(), "shop")
	_, pub, err := remote.NewSyncKey()
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.Fields(pub)[1]
	_, kept, err := remote.NewSyncKey()
	if err != nil {
		t.Fatal(err)
	}
	keys := remote.DoorKeysFile("devopsy-shop")
	if err := os.MkdirAll(filepath.Dir(keys), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keys, []byte("ssh-ed25519 "+blob+" leaked@laptop\n"+strings.TrimSpace(kept)+" kept@laptop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The removal of the leaked key, a rename as KeysRemoveScript's mv.
	if err := os.WriteFile(keys+".tmp", []byte(strings.TrimSpace(kept)+" kept@laptop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(keys+".tmp", keys); err != nil {
		t.Fatal(err)
	}
	// A request the leaked key's process started meanwhile: runDoor finds a key left.
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Tree: tree, Space: t.TempDir()}}
	if !remote.KeysLeft(keys) {
		t.Fatal("no key left")
	}
	if side.keysAt, err = os.Stat(keys); err != nil {
		t.Fatal(err)
	}
	// The end: the user's processes (not this request's, past its sudo), then its sync keys.
	if err := endProjectUser("devopsy-shop", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if code := side.syncKey([]string{"add", "1762600000.000009", tree + "/prod", "vm2:prod", "-", blob}); code == 0 &&
		strings.Contains(readString(remote.DoorSyncKeysFile("devopsy-shop")), "1762600000.000009") {
		t.Errorf("BUG: a sync key added after the end of the user's processes: %q", readString(remote.DoorSyncKeysFile("devopsy-shop")))
	}
}
