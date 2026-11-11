package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bytes"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// The deploy user's release of a compose project under a project user's names is refused
// before its switch (--check-names): the door acts only on what it made, and would refuse
// that user's every compose command while it runs. Names are the door's (door.Policy's
// OwnName): a longer project's names are that project's.
func TestDoorNameOwner(t *testing.T) {
	t.Parallel()
	d := &remote.Door{Users: map[string]remote.DoorUser{"devopsy-shop": {Project: "shop"}, "devopsy-shop-admin": {Project: "shop-admin"}}}
	root := t.TempDir()
	for name, want := range map[string]string{
		"shop-stage":      "devopsy-shop",
		"shop-eu-prod":    "devopsy-shop",
		"shop-admin-prod": "devopsy-shop-admin",
		"blog-prod":       "",
		"shopping-prod":   "",
	} {
		if got := doorNameOwner(d, root, name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if got := doorNameOwner(nil, root, "shop-stage"); got != "" {
		t.Errorf("no door: %q", got)
	}
	// A project in the release root holds its names too, the deploy user's own: one whose
	// name extends a project user's is not that user's.
	if err := os.Mkdir(filepath.Join(root, "shop-eu"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"shop-eu-prod": "", "shop-eur-prod": "devopsy-shop", "shop-stage": "devopsy-shop"} {
		if got := doorNameOwner(d, root, name); got != want {
			t.Errorf("%s, beside the release root's shop-eu: %q, want %q", name, got, want)
		}
	}
}

// --check-names, the activation's check before its switch, refuses it as the deploy user, and
// refuses too when the server's door does not read.
func TestCheckNamesRefusesAProjectUsersNames(t *testing.T) {
	t.Parallel()
	home, rel := t.TempDir(), t.TempDir()
	write(t, filepath.Join(home, "config.yaml"), "releases:\n  root: "+t.TempDir()+"\ndoor:\n  space: /var/lib/devopsy-door\n", 0o644)
	write(t, filepath.Join(home, "door", "users", "devopsy-shop.yaml"), "project: shop\n", 0o644)
	write(t, filepath.Join(rel, ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	for name, refused := range map[string]bool{"shop-stage": true, "blog-prod": false} {
		// As the activation calls it: the release's compose project in its environment.
		out, code := runDevopsy(t, rel, []string{"DEVOPSY_HOME=" + home, "COMPOSE_PROJECT_NAME=" + name}, "--check-names", filepath.Dir(rel))
		if (code != 0) != refused || refused != strings.Contains(out, "is under the names of project user devopsy-shop here") {
			t.Errorf("%s (%d):\n%s", name, code, out)
		}
	}
	// A project user's entry that does not read stops the check rather than passing it.
	write(t, filepath.Join(home, "door", "users", "devopsy-shop.yaml"), "project: [not, a, name\n", 0o644)
	if out, code := runDevopsy(t, rel, []string{"DEVOPSY_HOME=" + home, "COMPOSE_PROJECT_NAME=blog-prod"}, "--check-names", filepath.Dir(rel)); code == 0 {
		t.Errorf("a door that does not read passes:\n%s", out)
	}
}

// --project-users remove ends the project user's processes once its entry and keys are
// gone, so the door answers no new call meanwhile: one it left running would outlive its
// logins. What the door kept for it goes too (its registry logins, its claimed images'
// record), another project's staying. A kill that fails fails the command, which says the
// rest is done.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's keys are.
func TestRemoveProjectUserEndsItsProcesses(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	entry := filepath.Join(t.TempDir(), "devopsy-shop.yaml")
	var ended []string
	end := func(fails error) func(string) error {
		return func(who string) error {
			if _, err := os.Stat(entry); err == nil {
				t.Errorf("%s's processes ended while its entry stays", who)
			}
			ended = append(ended, who)
			return fails
		}
	}
	write(t, entry, "project: shop\n", 0o600)
	// Its keys and its sync keys go with its entry: it logs in no more, by either.
	keys, syncKeys := remote.DoorKeysFile("devopsy-shop"), remote.DoorSyncKeysFile("devopsy-shop")
	write(t, keys, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g op@laptop\n", 0o600)
	write(t, syncKeys, "a sync's key\n", 0o600)
	other := remote.DoorKeysFile("devopsy-blog")
	write(t, other, "another's keys\n", 0o600)
	space := t.TempDir()
	logins, record := filepath.Join(space, "logins", "shop", "config.json"), filepath.Join(space, "images", "devopsy-shop")
	theirs := filepath.Join(space, "logins", "blog", "config.json")
	for _, f := range []string{logins, record, record + ".lock", theirs} {
		write(t, f, "{}\n", 0o600)
	}
	if code := removeProjectUser(&remote.Door{Space: space}, "shop", "devopsy-shop", entry, end(nil)); code != 0 || strings.Join(ended, " ") != "devopsy-shop" {
		t.Errorf("remove (exit %d) ended %q", code, ended)
	}
	if fileExists(logins) || fileExists(record) || fileExists(record+".lock") || !fileExists(theirs) {
		t.Errorf("after remove: its logins %v, its images' record %v and its lock %v, another's logins %v", fileExists(logins), fileExists(record), fileExists(record+".lock"), fileExists(theirs))
	}
	if fileExists(entry) || fileExists(keys) || fileExists(syncKeys) || !fileExists(other) {
		t.Errorf("after remove: its entry %v, its keys %v, its sync keys %v, another's keys %v", fileExists(entry), fileExists(keys), fileExists(syncKeys), fileExists(other))
	}
	ended = nil
	write(t, entry, "project: shop\n", 0o600)
	if code := removeProjectUser(nil, "shop", "devopsy-shop", entry, end(errors.New("no docker"))); code == 0 {
		t.Error("a kill that failed passes")
	}
	if !fileExists(remote.DoorEndingFile("devopsy-shop")) {
		t.Error("a kill that failed left no mark of the end pending")
	}
	// Asked again, its entry gone, the marked end completes and its mark goes.
	ended = nil
	if code := removeProjectUser(nil, "shop", "devopsy-shop", entry, end(nil)); code != 0 || strings.Join(ended, " ") != "devopsy-shop" || fileExists(remote.DoorEndingFile("devopsy-shop")) {
		t.Errorf("a removal asked again after a failed end (exit %d) ended %q, its mark there %v", code, ended, fileExists(remote.DoorEndingFile("devopsy-shop")))
	}
	ended = nil
	if code := removeProjectUser(nil, "shop", "devopsy-shop", entry, end(nil)); code == 0 || len(ended) != 0 {
		t.Errorf("no such user (exit %d) ended %q", code, ended)
	}
}

// --project-users remove drops the user's sync keys after its processes are ended, under the
// sync keys' lock (endProjectUser, as a key's removal does): a door request already past its
// sudo, which no end reaches, holds that lock with the user's keys file still as it found it,
// and writes its line before the removal gets the lock, so the removal drops it. Else the sync
// key would stay, and --authorized-keys print it for the Unix user, which stays too.
//
// The add here is the door's (editSyncKeys with its check under the lock): the removal runs
// while it holds the lock, past its check, as a request in flight does.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's keys are.
func TestRemoveProjectUserDropsSyncKeysUnderTheirLock(t *testing.T) {
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
		t.Errorf("a sync key added by a request in flight outlives --project-users remove:\n%s", data)
	}
}

// writeSyncKey writes a sync's key as the door writes it for who (remote.SyncKeyLine) into
// its sync keys file.
func writeSyncKey(t *testing.T, who string) {
	t.Helper()
	_, pub, err := remote.NewSyncKey()
	if err != nil {
		t.Fatal(err)
	}
	line, err := remote.SyncKeyLine("/usr/local/bin/devopsy", "1762600000.00000a", "/srv/shop/dev", "vm2:dev", false, strings.Fields(pub)[1], time.Now().Add(remote.SyncKeyLife))
	if err != nil {
		t.Fatal(err)
	}
	write(t, remote.DoorSyncKeysFile(who), line+"\n", 0o600)
}

// A project user's sync keys go before its processes are ended and stay gone through the end
// (endProjectUser, under their lock): sshd, told them by --authorized-keys, would otherwise
// take one a leaked key added for a login that lands after the end's last scan, its session
// the user's and past the key's removal.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's sync keys are.
func TestSyncKeysOpenNoLoginThroughTheEnd(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	who := "devopsy-shop"
	writeSyncKey(t, who)
	var told bytes.Buffer
	err := endProjectUser(who, func(string) error {
		// What sshd is told of the user's sync keys while its processes are ended.
		if code := printAuthorizedKeys(&told, remote.DoorSyncKeysFile(who)); code != 0 {
			t.Errorf("--authorized-keys' read of the sync keys: exit %d", code)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if told.Len() != 0 {
		t.Errorf("while %s's processes are ended, sshd still takes its sync key for a login:\n%s", who, told.String())
	}
	if fileExists(remote.DoorSyncKeysFile(who)) {
		t.Error("the sync keys stay after the end")
	}
}

// A door request begun before a user's end, its keysAt the keys file a key's removal left, adds
// no sync key after the end: the end makes that file anew (renewKeys), so the request's check
// under the sync keys' lock finds it changed. Else a leaked key's process, past its sudo,
// which no end reaches, would add one that outlives the removal for its life.
//
// Not parallel: it sets DEVOPSY_HOME (the door's directory).
func TestNoSyncKeyAddedPastTheEnd(t *testing.T) {
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
	side.keysFile = keys
	if side.keysAt, err = os.Stat(keys); err != nil {
		t.Fatal(err)
	}
	// The end: the user's processes (not this request's, past its sudo), then its sync keys.
	if err := endProjectUser("devopsy-shop", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if code := side.syncKey([]string{"add", "1762600000.000009", tree + "/prod", "vm2:prod", "-", blob}); code == 0 &&
		strings.Contains(readString(remote.DoorSyncKeysFile("devopsy-shop")), "1762600000.000009") {
		t.Errorf("a sync key added after the end of the user's processes: %q", readString(remote.DoorSyncKeysFile("devopsy-shop")))
	}
}

// --project-users add closes the deploy user's projects in the release root to everyone else:
// a project user enters the root to reach its own tree when it is not the deploy user's home,
// and what a project made before the server had project users (with the session's umask) is
// then that user's to read. Only a project's directory (none of a home's own, .cache or
// .config, nor the door's space) that the deploy user owns and others may enter changes, and
// add says which.
func TestCloseDeployProjects(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	space := filepath.Join(root, "door")
	for name, mode := range map[string]os.FileMode{"shop": 0o775, "blog": 0o755, "closed": 0o700, ".cache": 0o755, "door": 0o755} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "note"), "a file\n", 0o644)
	if closed, err := closeDeployProjects(root, space, os.Getuid()+1); err != nil || len(closed) != 0 {
		t.Errorf("another user's directories closed: %q (%v)", closed, err)
	}
	closed, err := closeDeployProjects(root, space, os.Getuid())
	if err != nil || strings.Join(closed, " ") != "blog shop" {
		t.Fatalf("closed %q (%v)", closed, err)
	}
	for _, name := range []string{"shop", "blog", "closed"} {
		if fi, err := os.Stat(filepath.Join(root, name)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v (%v)", name, fi.Mode(), err)
		}
	}
	for _, name := range []string{".cache", "door"} {
		if fi, err := os.Stat(filepath.Join(root, name)); err != nil || fi.Mode().Perm() != 0o755 {
			t.Errorf("%s, no project, closed: %v (%v)", name, fi.Mode(), err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(root, "note")); fi.Mode().Perm() != 0o644 {
		t.Errorf("a file changed: %v", fi.Mode())
	}
}

// A project user whose Unix user is gone already runs nothing to end: its removal goes on to
// its entry and keys rather than failing on the lookup.
func TestEndUserOfAUserGone(t *testing.T) {
	t.Parallel()
	if err := endUser("devopsy-nosuchuser-zz9"); err != nil {
		t.Errorf("a user gone: %v", err)
	}
}

// A removal's mark (remote.DoorRemovingFile) holds what is left of it until done: settled by
// the end that keysSettle runs, what the door kept for the project goes (its logins, its record
// and the record's lock), then both marks; with no mark, the end touches none of it.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the door's config and marks are.
func TestPendingRemovalFinishesWhatItKept(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	space := t.TempDir()
	write(t, filepath.Join(home, remote.ConfigFile), "door:\n  space: "+space+"\n", 0o600)
	logins := filepath.Join(door.LoginsDirOf(space, "shop"), "config.json")
	record := door.ImagesFileOf(space, "shop")
	for _, f := range []string{logins, record, record + ".lock"} {
		write(t, f, "{}\n", 0o600)
	}
	// With no removal pending, a key's end leaves them.
	if err := finishPendingRemoval("devopsy-shop"); err != nil || !fileExists(logins) || !fileExists(record) {
		t.Fatalf("a key's end with no removal pending (%v): its logins %v, its record %v", err, fileExists(logins), fileExists(record))
	}
	write(t, remote.DoorRemovingFile("devopsy-shop"), "shop\n", 0o600)
	write(t, remote.DoorEndingFile("devopsy-shop"), "", 0o600)
	if err := finishPendingRemoval("devopsy-shop"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{logins, record, record + ".lock", remote.DoorRemovingFile("devopsy-shop"), remote.DoorEndingFile("devopsy-shop")} {
		if fileExists(f) {
			t.Errorf("a pending removal finished left %s", f)
		}
	}
}

// A --project-users remove cut before its keys went is finished by a removal asked again,
// its keys included: sshd takes keys from the file alone, so a removal that said them removed
// and left them would let the removed user log in.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's keys are.
func TestRemovalAskedAgainTakesTheKeys(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	entry := filepath.Join(t.TempDir(), "devopsy-shop.yaml")
	keys := remote.DoorKeysFile("devopsy-shop")
	// As a first removal cut before its keys' write left it: its marks, the entry gone, the keys.
	write(t, keys, removedUserKey, 0o600)
	write(t, remote.DoorRemovingFile("devopsy-shop"), "shop\n", 0o600)
	write(t, remote.DoorEndingFile("devopsy-shop"), "", 0o600)
	if code := removeProjectUser(&remote.Door{Space: t.TempDir()}, "shop", "devopsy-shop", entry, func(string) error { return nil }); code != 0 {
		t.Fatalf("a removal asked again: exit %d", code)
	}
	if remote.KeysLeft(keys) {
		t.Error("a removal asked again said the user's keys removed, and they are there: sshd still takes them")
	}
}

// A user under door: users: in the config is told to be removed there, even with a key's
// removal's end pending: that mark is the key's, never a removal's, which writes one of its
// own (remote.DoorRemovingFile), so the removal takes nothing of the user's for its own.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's keys are.
func TestRemovalOfAConfigUserWithAKeysEndPending(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	entry := filepath.Join(t.TempDir(), "devopsy-shop.yaml")
	space := t.TempDir()
	record := filepath.Join(space, "images", "devopsy-shop")
	write(t, record, "sha256:1\n", 0o600)
	write(t, record+".lock", "", 0o600)
	write(t, remote.DoorKeysFile("devopsy-shop"), removedUserKey, 0o600)
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

// A removal whose end failed and that a listing of the user's keys then settles takes what
// the door kept for the project too (its logins, its record of claimed images): asked again,
// the removal would find nothing left to do, and a project added again by the name would
// inherit them.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's keys are.
func TestRemovalSettledByAKeysListingTakesWhatWasKept(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	who := "devopsy-shop"
	entry := filepath.Join(t.TempDir(), who+".yaml")
	write(t, entry, "project: shop\n", 0o600)
	write(t, remote.DoorKeysFile(who), removedUserKey, 0o600)
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

// A key's removal completing clears its own mark only, never a user's removal's: the user's
// removal, cut before its keys went, is still finished by a removal asked again, its keys
// emptied, which sshd would otherwise take.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's keys are.
func TestKeysRemovalLeavesAUsersRemovalMark(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	who := "devopsy-shop"
	entry := filepath.Join(t.TempDir(), who+".yaml")
	keys := remote.DoorKeysFile(who)
	kept := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIM1Ehq6Bkq8p5W7iKjX1pXJ7ZcP9C0P1bQz2y3x4w5v6 dev@laptop\n"
	// As the user's removal, cut before its keys' write, left it: its marks, no entry, the keys.
	write(t, keys, removedUserKey+kept, 0o600)
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

// removedUserKey is a project user's key, for the tests of its removal.
const removedUserKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g op@laptop\n"
