package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

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
