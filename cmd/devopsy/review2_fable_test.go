package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Review round 2 (Fable). Bug: a destroy refuses a home that is a link, or no directory, only
// in its host step (remote.DoorUserDeleteScript's gone: "is a link, left"), its seventh, after
// userdel dropped the user and once its keys, containers, volumes, data and images are gone:
// a refusal after the change, never before, and every run again stops at the same place until
// an admin removes the link by hand. ready checks the home's shape alone (destroyablePath),
// and the tree's being a directory, never the home's: a home moved to another disk and linked
// back passes it.
func TestReview2DestroyRefusesAHomeItWillNotRemoveBeforeAnything(t *testing.T) {
	t.Parallel()
	dir := func(t *testing.T, home string) {
		if err := os.Mkdir(home, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, c := range map[string]struct {
		home func(t *testing.T, home string)
		// outside makes the Unix user one outside the project users' group, which the host
		// step refuses as no project user.
		outside bool
		passes  bool
	}{
		"a directory": {home: dir, passes: true},
		"a link to a directory": {home: func(t *testing.T, home string) {
			if err := os.Symlink(t.TempDir(), home); err != nil {
				t.Fatal(err)
			}
		}},
		"a file":                          {home: func(t *testing.T, home string) { write(t, home, "", 0o600) }},
		"a user outside the door's group": {home: dir, outside: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := filepath.Join(t.TempDir(), "devopsy-shop")
			c.home(t, home)
			x := &userDestroy{d: &remote.Door{Space: t.TempDir()}, project: "shop", who: "devopsy-shop", root: t.TempDir(),
				lookup: func(who string) (*user.User, error) {
					return &user.User{Username: who, Uid: "59999", Gid: "59999", HomeDir: home}, nil
				},
				projectUser: func(string) bool { return !c.outside }}
			err := x.ready(destroyMark{}, false)
			if c.passes && err != nil {
				t.Errorf("a home that is a directory is refused: %v", err)
			}
			if !c.passes && err == nil {
				t.Errorf("ready passes %s, which the host step refuses once everything else is gone", name)
			}
		})
	}
}

// Review round 2 (Fable). Bug: --server-info says a tagged image is one "nor any container
// uses" when the containers could not be listed, or only in part (ContainersUnread): each
// image's count of containers comes from that list (hostImages), so a read that failed is
// taken for none, with docker rmi as its hint. The one-offs and the dangling images of the
// same report go unread then (gcScope.imagesUsed); this line goes by the same absence and is
// made all the same.
func TestReview2ServerInfoImagesUnusedOnlyWhenTheContainersRead(t *testing.T) {
	t.Parallel()
	info := remote.ServerInfo{
		Root:             "/home/devopsy",
		ContainersUnread: "docker inspect: unexpected EOF",
		Images:           []remote.HostImage{{ID: "sha256:aaa", Tags: []string{"tool:1"}, Size: 1000}},
	}
	for _, s := range leftSays(info, "vm1", "vm1") {
		if strings.Contains(s, "nor any container uses it") {
			t.Errorf("with the containers unread, an image is said to be used by none:\n%s", s)
		}
	}
}

// Review round 2 (Fable). Bug: what --gc's count could not read (GCFacts.Unread, which
// leftFacts fills from gcScope.find) is shown by no line of --server-info: with the dangling
// images unread it says "--gc would remove 1 one-off container" as if that were all, and with
// nothing else counted it says nothing of --gc at all. Unknown reads as none.
func TestReview2ServerInfoSaysWhatGCCouldNotCount(t *testing.T) {
	t.Parallel()
	for name, gc := range map[string]*remote.GCFacts{
		"with something counted": {OneOffs: 1, Unread: []string{"the dangling images: docker image ls: unexpected EOF"}},
		"with nothing counted":   {Unread: []string{"the dangling images: docker image ls: unexpected EOF"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			says := strings.Join(leftSays(remote.ServerInfo{Root: "/home/devopsy", GC: gc}, "vm1", "vm1"), "\n")
			if !strings.Contains(says, "docker image ls: unexpected EOF") {
				t.Errorf("what --gc's count could not read is said nowhere:\n%s", says)
			}
		})
	}
}
