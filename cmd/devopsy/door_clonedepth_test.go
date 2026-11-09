package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// BUG: the door's clone and mark believe a path at any depth of a clone. Below a take, a
// clone is a root container's copy of the project's entries, owners kept, so a directory
// the project user made in its entry is its own there too, and it can swap what is in it
// for a link between the door's Real and the deploy user's write: clone's MkdirAll and
// mark's remove and create then follow the link, as the deploy user, anywhere it writes (a
// directory under its release root takes another project's names from it, one in another's
// mnt is an entry that project cannot write). The fake's real answers what realpath said
// before the swap; on disk the link is already there. The forms hold a mount's source to
// what devopsy's own clones make (cloneShape); these requests are not held to it.
//
// Not parallel: it sets PATH (doorFakes).
func TestDoorCloneRequestsFollowNoLinkInAClone(t *testing.T) {
	doorFakes(t)
	root := t.TempDir()
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(root, "space")}}
	// own is the project user's, made in its entry and copied with its owner.
	own := filepath.Join(side.policy.Clones(), "abc", "1", "mnt", "data", "own")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "releases")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(own, "sub")); err != nil {
		t.Fatal(err)
	}
	side.clone(filepath.Join(own, "sub", "blog-prod"))
	if _, err := os.Lstat(filepath.Join(victim, "blog-prod")); err == nil {
		t.Errorf("clone made a directory as the deploy user through a link in a clone: %s", filepath.Join(victim, "blog-prod"))
	}
	side.mark(filepath.Join(own, "sub"), "id1")
	if _, err := os.Lstat(filepath.Join(victim, remote.SnapshotComplete)); err == nil {
		t.Errorf("mark wrote as the deploy user through a link in a clone")
	}
}
