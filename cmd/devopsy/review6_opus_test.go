package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

const review6OpusKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g op@laptop\n"

// Review 6 (Opus), a failing test proving a bug: a --project-users remove whose end failed is
// finished, as its fix and the listing's warning say ("a listing or an edit of its keys
// completes it, as a removal asked again does"), by the next listing of the user's keys. That
// settle (remote.keysSettle) runs --end-project-user and clears the mark, and nothing else:
// the user's registry logins (<space>/logins/<project>, their auth values) and its record of
// claimed images stay. A removal asked again then finds neither the entry nor the mark and says
// there is no such project user, so what the door kept for it is never removed (and a project
// added again under that name inherits the record of images claimed with the old logins).
//
// The settle is done here as keysSettle does it: endProjectUser, then the mark removed.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's keys are.
func TestReview6RemovalSettledByAKeysListingKeepsTheLogins(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	who := "devopsy-shop"
	entry := filepath.Join(t.TempDir(), who+".yaml")
	write(t, entry, "project: shop\n", 0o600)
	write(t, remote.DoorKeysFile(who), review6OpusKey, 0o600)
	space := t.TempDir()
	logins, record := filepath.Join(space, "logins", "shop", "config.json"), filepath.Join(space, "images", who)
	write(t, logins, `{"auths":{"registry.example.com":{"auth":"dXNlcjpzZWNyZXQ="}}}`+"\n", 0o600)
	write(t, record, "sha256:0123\n", 0o600)
	d := &remote.Door{Space: space}
	if code := removeProjectUser(d, "shop", who, entry, func(string) error { return errors.New("no docker") }); code == 0 {
		t.Fatal("a remove whose end failed passes")
	}
	// The admin follows the listing's warning: a listing of the user's keys settles the end.
	if err := endProjectUser(who, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(remote.DoorEndingFile(who)); err != nil {
		t.Fatal(err)
	}
	_ = removeProjectUser(d, "shop", who, entry, func(string) error { return nil })
	if fileExists(logins) || fileExists(record) {
		t.Errorf("a removal whose end a keys listing settled keeps its logins (%v) and its claimed images' record (%v), and asked again it removes neither", fileExists(logins), fileExists(record))
	}
}

// Review 6 (Opus), a failing test proving a bug: a key's removal and a user's removal share one
// mark (remote.DoorEndingFile), and the first to complete clears it for both. --project-users
// remove writes the mark and drops the entry outside the keys' lock, then waits a minute for
// that lock (lockedKeys) to empty the keys: while a key's removal holds it for its end (the util
// image's rebuild, a slow Docker), the user's removal fails there, its entry gone and its other
// keys kept. The key's removal then completes and runs `rm -f "$f.ending"` (KeysRemoveScript),
// clearing the user's removal's mark too. Asked again, the removal finds no entry and no mark,
// says there is no project user, and never empties the keys, which --authorized-keys prints by
// the file alone: the removed user still logs in (its shell, home and tree), the door aside.
//
// The key's removal is done here as its script ends: the key's line gone, the mark removed.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's keys are.
func TestReview6KeysRemovalClearsAUsersRemovalMark(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	who := "devopsy-shop"
	entry := filepath.Join(t.TempDir(), who+".yaml")
	keys := remote.DoorKeysFile(who)
	kept := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIM1Ehq6Bkq8p5W7iKjX1pXJ7ZcP9C0P1bQz2y3x4w5v6 dev@laptop\n"
	// As the user's removal, cut before its keys' write, left it: its marks, no entry, the keys.
	write(t, keys, review6OpusKey+kept, 0o600)
	write(t, remote.DoorRemovingFile(who), "shop\n", 0o600)
	write(t, remote.DoorEndingFile(who), "", 0o600)
	// The key's removal that held the lock completes: its key gone, the mark cleared.
	write(t, keys, kept, 0o600)
	if err := os.Remove(remote.DoorEndingFile(who)); err != nil {
		t.Fatal(err)
	}
	_ = removeProjectUser(&remote.Door{Space: t.TempDir()}, "shop", who, entry, func(string) error { return nil })
	if remote.KeysLeft(keys) {
		t.Error("a user's removal whose mark a key's removal cleared leaves its keys, which sshd takes, and asked again it does not empty them")
	}
}
