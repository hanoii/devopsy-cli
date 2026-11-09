package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// BUG: across servers, an entry the parent copies that the child's labels do not name (the
// child's release mounts another: a branch cut before the parent's mount, a variable of its
// own) is refused only by the child's receive of that entry (planReceive), once the parent
// stopped its services and the entries before it were written: the child is left with a
// partial copy, marked, its services down. The push has the child's list before it stops
// anything (pushChecks) and refuses nothing there; on one server the same sync copies the
// entry and completes.
func TestSyncAcrossRefusesAnUnnamedEntryBeforeAnythingChanges(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("the child's first release (%d):\n%s", code, out)
	}
	s.logs()
	// The child's app mounts mnt/other, the parent's mnt/storage: the parent copies db and
	// storage, and the child's labels name db and other.
	write(t, s.log+".shop-dev", "FAKE_APP_MOUNT=/other\n", 0o644)
	write(t, filepath.Join(s.mnt("prod"), "db", "ibdata"), "newer rows\n", 0o644)
	// On one server the sync copies both and completes.
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code != 0 || readString(filepath.Join(s.mnt("dev"), "db", "ibdata")) != "newer rows\n" {
		t.Fatalf("the same sync on one server (%d):\n%s", code, out)
	}
	s.logs()
	write(t, filepath.Join(s.mnt("prod"), "db", "ibdata"), "newest rows\n", 0o644)
	env := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	out, code := s.run(env, "@devopsy@two:dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code == 0 {
		return // copied, or skipped with a word: either is a sync that completes
	}
	marked := fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile))
	if parentChanged(docker) || marked || readString(filepath.Join(s.mnt("dev"), "db", "ibdata")) == "newest rows\n" {
		t.Errorf("refused after the parent stopped and the child was written (marked %v, the child's db %q):\n%s\n%s", marked, strings.TrimSpace(readString(filepath.Join(s.mnt("dev"), "db", "ibdata"))), out, docker)
	}
}
