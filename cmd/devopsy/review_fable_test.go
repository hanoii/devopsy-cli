package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// reviewSyncKey writes a sync's key as the door writes it for who (remote.SyncKeyLine) into
// its sync keys file.
func reviewSyncKey(t *testing.T, who string) {
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

// BUG: a project user's sync keys still open a login while its processes are ended, and until
// the drop that follows the end. endProjectUser ends first and drops after (the fourth
// review's fix, for a key added during the end), so through the whole end, and the time its
// container takes to go, sshd is still told the sync keys (--authorized-keys prints them): a
// login by one a leaked key added before, repeated from outside, lands after the last scan
// found no process and before the drop, and its session, the user's (--sync-child runs the
// environment's own commands, in a tree the leaked key wrote), outlives the key's removal and
// keeps the door where a key is left. The sync keys are to go before the end, and stay gone
// through it: the end under their lock, with the door refusing every add begun before it was
// over (TestReviewOpusSyncKeyAddedPastTheEnd is that other half).
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's sync keys are.
func TestSyncKeysOpenNoLoginThroughTheEnd(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	who := "devopsy-shop"
	reviewSyncKey(t, who)
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

// BUG: once --project-users remove took a project user's entry, the deploy user reads that
// user's tree itself. --server-info leaves a tree to its user's own read only while the door
// lists it (doorTrees), so with the tree in the release root (releases: root: on a volume of
// its own, or tree: there) the removal, an admin's answer to a leaked key, hands the tree to
// the deploy user's own walk (projectEnvironments): remote.EnvStatus reads its files with no
// bound (a FIFO where a release's record goes holds --server-info for good, the deadline of
// treeEnvironments being the other path's), follows its links, and statusHint offers
// `cd <tree>/<environment>/current && devopsy --status` again, devopsy running there what that
// user wrote. The tree is still its Unix user's, which remove keeps: whose a tree is comes
// from its owner (or from a mark the removal leaves), never from the door's list alone. The
// tree here is the test's own user's, so a fix by owner needs this test to make it another's.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the door's entries are.
func TestServerInfoReadsNoTreeOfARemovedProjectUser(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	root := t.TempDir()
	tree := filepath.Join(root, "shop")
	if err := os.MkdirAll(filepath.Join(tree, "prod", "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "config.yaml"), "door:\n  space: "+t.TempDir()+"\n", 0o600)
	entry := filepath.Join(remote.DoorDir(), "users", "devopsy-shop.yaml")
	write(t, entry, "project: shop\ntree: "+tree+"\n", 0o600)
	// walk is --server-info's list: how many trees it asked their user for, and the
	// environments the deploy user read itself.
	walk := func() (asked int, own []string) {
		for _, e := range environments(root, doorTrees(root), func(string, string) []remote.Environment { asked++; return nil }) {
			if e.User == "" {
				own = append(own, e.Path)
			}
		}
		return asked, own
	}
	// The control: with its entry, the tree is read as its user alone.
	if asked, own := walk(); asked != 1 || len(own) != 0 {
		t.Fatalf("with its entry: %d trees asked of their user, the deploy user read %v", asked, own)
	}
	d, err := remote.ServerDoor()
	if err != nil {
		t.Fatal(err)
	}
	if code := removeProjectUser(d, "shop", "devopsy-shop", entry, func(string) error { return nil }); code != 0 {
		t.Fatalf("remove: exit %d", code)
	}
	if _, own := walk(); len(own) != 0 {
		t.Errorf("after --project-users remove, the deploy user reads the tree its Unix user still owns: %v", own)
	}
}
