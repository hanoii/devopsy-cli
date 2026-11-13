package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Review round 1 (Fable). Bug: a destroy refuses a tree it will not remove as root
// (destroyablePath: a directory named for the project under a parent) only in its host
// step, its seventh, once the user's keys, containers, volumes, data and images are gone: a
// refusal after the change, never before. A tree: is whatever a door: users: line or an
// entry names, nothing checks its name when it is read, and the home the mark keeps is
// checked there too. The check belongs in ready, with the tree's own.
func TestReview1DestroyRefusesATreeItWillNotRemoveBeforeAnything(t *testing.T) {
	t.Parallel()
	c := newRemovalCutIn(t, t.TempDir())
	s := c.destroyServer()
	// A tree that is a directory, and not named for its project.
	tree := filepath.Join(t.TempDir(), "site")
	write(t, filepath.Join(tree, "prod", "shared", "db"), "data\n", 0o600)
	write(t, c.entry, "project: shop\ntree: "+tree+"\n", 0o600)
	state := func() string {
		return readString(filepath.Join(s.state, "containers")) + readString(filepath.Join(s.state, "volumes")) + readString(filepath.Join(s.state, "images"))
	}
	before := state()
	out, code := runDevopsy(t, t.TempDir(), c.env(), remote.ProjectUsersFlag, "remove", "shop", "--destroy")
	if code != 1 || !strings.Contains(out, "is not a project's directory") {
		t.Fatalf("a tree not named for its project (%d):\n%s", code, out)
	}
	if !remote.KeysLeft(c.keys) || fileExists(c.destroying) || fileExists(c.removing) || !fileExists(c.logins) || state() != before ||
		!fileExists(filepath.Join(c.space, "mnt", "shop-prod", "db", "f")) {
		t.Errorf("refused at its host step, after the change: keys left %v, its mark %v, its logins %v, its data %v, containers, volumes and images as they were %v:\n%s",
			remote.KeysLeft(c.keys), fileExists(c.destroying), fileExists(c.logins), fileExists(filepath.Join(c.space, "mnt", "shop-prod", "db", "f")), state() == before, out)
	}
}
