package main

import (
	"os"
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

// BUG: a reset across servers refuses on the kind of an entry its own wipe is about to
// remove, and only for the copy's first entry: the child's receive reads the entry's kind
// before the claim (planReceive), so the first entry is judged as the child has it, and
// every later one after the wipe emptied mnt. A child whose mnt/db is a file where the
// parent's is a directory (Docker made one for a bind mount, say) is refused the reset that
// starts it over, while the same mismatch on a later entry resets.
func TestResetAcrossWipesAnEntryOfAnotherKind(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"storage", "db"} {
		t.Run(entry, func(t *testing.T) {
			t.Parallel()
			s := newSyncSetup(t)
			s.releaseProd(nil)
			if out, code := s.run(nil, "@dev", "--release"); code != 0 {
				t.Fatalf("the child's first release (%d):\n%s", code, out)
			}
			s.logs()
			// The child's entry is a file, the parent's a directory.
			if err := os.RemoveAll(filepath.Join(s.mnt("dev"), entry)); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(s.mnt("dev"), entry), "a file\n", 0o644)
			env := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}
			out, code := s.run(env, "@devopsy@two:dev", "--reset", "--yes")
			if code != 0 {
				t.Errorf("a reset over mnt/%s of another kind (%d):\n%s", entry, code, out)
			}
		})
	}
}

// BUG: a full restore makes a missing volume that has options with the compose of the
// release it replaces, never of the one it restores: its data step runs in current, the
// old release, and ensureVolumes runs compose create there, naming the restored code's
// services. Where the two differ (the restored code added the service or the volume, its
// volume pruned since a rollback; a snapshot imported from elsewhere) compose knows no such
// service, or makes the volume with the old release's options, and the restore fails past
// its mark, its services down. Before, docker volume create took the restored labels'.
func TestRestoreMakesAVolumeWithTheRestoredCompose(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.labels(`FAKE_DB_VOLUME=dbdata FAKE_DB_VOLUME_OPTS='{"type":"cifs","o":"username=app,password=s3cret"}'`)
	s.releaseProd(nil)
	vol := filepath.Join(s.log+".vol", "shop-prod_dbdata")
	write(t, filepath.Join(vol, "ibd"), "pages\n", 0o644)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	was := s.current("prod")
	if err := os.RemoveAll(vol); err != nil {
		t.Fatal(err)
	}
	s.logs()
	out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--yes")
	docker, _ := s.logs()
	if code != 0 || readString(filepath.Join(vol, "ibd")) != "pages\n" || s.current("prod") == was {
		t.Fatalf("the restore (%d), volume %q:\n%s\n%s", code, readString(filepath.Join(vol, "ibd")), out, docker)
	}
	made := false
	for _, line := range strings.Split(docker, "\n") {
		if !strings.Contains(line, " create --no-build --no-recreate ") {
			continue
		}
		made = true
		if !strings.Contains(line, "/"+s.current("prod")+"/") {
			t.Errorf("the volume made with another release's compose than the restored one's (%s, current was %s):\n%s", s.current("prod"), was, line)
		}
	}
	if !made {
		t.Errorf("the volume not made by compose:\n%s", docker)
	}
}
