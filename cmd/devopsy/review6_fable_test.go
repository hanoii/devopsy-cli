package main

import (
	"path/filepath"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

const review6Key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g op@laptop\n"

// Review 6 (Fable), a bug: --project-users remove cut, or failed, after its entry went and
// before its keys did (lockedKeys waits a minute on the keys' lock, which a key's removal
// holds for as long as its end runs) leaves the end's mark, no entry, and the keys. Asked
// again, the removal goes by the mark to finishRemoval, which ends the user and says "removed
// its entry and keys ... it logs in no more", and never empties the keys: --authorized-keys
// prints them by the file alone, so the removed user still logs in, and a later add of the
// project takes those keys with it.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's keys are.
func TestReview6RemovalAskedAgainTakesTheKeys(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	entry := filepath.Join(t.TempDir(), "devopsy-shop.yaml")
	keys := remote.DoorKeysFile("devopsy-shop")
	// As a first removal cut before its keys' write left it: its marks, the entry gone, the keys.
	write(t, keys, review6Key, 0o600)
	write(t, remote.DoorRemovingFile("devopsy-shop"), "shop\n", 0o600)
	write(t, remote.DoorEndingFile("devopsy-shop"), "", 0o600)
	if code := removeProjectUser(&remote.Door{Space: t.TempDir()}, "shop", "devopsy-shop", entry, func(string) error { return nil }); code != 0 {
		t.Fatalf("a removal asked again: exit %d", code)
	}
	if remote.KeysLeft(keys) {
		t.Error("a removal asked again said the user's keys removed, and they are there: sshd still takes them")
	}
}

// Review 6 (Fable), a bug: a user under door: users: in the config, whose key's removal left
// its end pending (the mark), is "removed" by --project-users remove: with no entry file and
// a mark, the removal takes it for its own unfinished one, ends the user, drops what the door
// kept for it (its logins, its claimed images' record) and says its entry and keys are
// removed, while the config still names it and its keys stay. Without the mark it is told to
// remove the user from the config.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's keys are.
func TestReview6RemovalOfAConfigUserWithAnEndPending(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	entry := filepath.Join(t.TempDir(), "devopsy-shop.yaml")
	space := t.TempDir()
	record := filepath.Join(space, "images", "devopsy-shop")
	write(t, record, "sha256:1\n", 0o600)
	write(t, record+".lock", "", 0o600)
	write(t, remote.DoorKeysFile("devopsy-shop"), review6Key, 0o600)
	// A key's removal whose end failed (KeysRemoveScript's mark).
	write(t, remote.DoorEndingFile("devopsy-shop"), "", 0o600)
	d := &remote.Door{Space: space, Users: map[string]remote.DoorUser{"devopsy-shop": {Project: "shop"}}}
	code := removeProjectUser(d, "shop", "devopsy-shop", entry, func(string) error { return nil })
	if code == 0 {
		t.Error("a user the config names, with an end pending, was said removed: its entry is the config's, and stays")
	}
	if !fileExists(record) {
		t.Error("its claimed images' record went, and the user stays the door's")
	}
}
