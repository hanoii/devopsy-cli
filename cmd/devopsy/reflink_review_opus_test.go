package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// Review of feat/reflink-syncs (opus). Each test names the bug it proves
// and fails on the branch as it is.

// Bug: the copy into a child by reflinks (shareInto) is cp -a as root,
// which makes FIFOs, sockets and device nodes in the child's entry on
// the host, where bind mounts are not nodev: what a sync's rsync never
// does (--no-D, decided in review round 4) and an import and a
// snapshot's tars refuse. Run here as devopsy runs it (the script from
// the docker call, its /s and /d this test's directories), it copies the
// parent's FIFO. A FIFO stands for every special file: it needs no root
// and no reflink.
func TestReviewOpusShareCopiesSpecialFiles(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("GNU cp (--reflink) runs the script")
	}
	if out, err := exec.Command("cp", "--version").Output(); err != nil || !strings.Contains(string(out), "GNU") {
		t.Skip("GNU cp runs the script")
	}
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	write(t, s.log+".shop-dev", "FAKE_APP_REFLINK=true\n", 0o644)
	out, code := s.run([]string{"FAKE_REFLINK=1"}, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "mnt/storage shared with the parent by reflinks") {
		t.Fatalf("shared (%d):\n%s\n%s", code, out, docker)
	}
	var script string
	for _, line := range strings.Split(docker, "\n") {
		if strings.Contains(line, "-share-") {
			if i := strings.Index(line, " sh -c "); i >= 0 {
				script = line[i+len(" sh -c "):]
			}
		}
	}
	if script == "" {
		t.Fatalf("no share call:\n%s", docker)
	}
	tmp := t.TempDir()
	src, dst := filepath.Join(tmp, "src"), filepath.Join(tmp, "dst")
	for _, d := range []string{src, dst} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(src, "pipe"), 0o666); err != nil {
		t.Fatal(err)
	}
	run := strings.ReplaceAll(strings.ReplaceAll(script, "/s/.", src+"/."), " /d", " "+dst)
	// Refusing (a failure, the entry then rsynced) or leaving none out
	// alike keep it out of the child's entry.
	_ = exec.Command("sh", "-c", run).Run()
	if info, err := os.Lstat(filepath.Join(dst, "pipe")); err == nil && info.Mode()&os.ModeNamedPipe != 0 {
		t.Errorf("the share made the parent's FIFO in the child's entry (an rsync's --no-D never does): %q", script)
	}
}

// Bug: where the child's mnt does not clone the parent's (ext4, or
// another filesystem: every server without XFS, and drupal11 labels its
// app), each sync of a service labeled devopsy.data.copy.reflink=true
// first empties the child's entry (shareInto's find ... rm -rf, as root),
// then finds cp --reflink=always failing, then rsyncs the whole entry
// again: every resync a full copy of the uploads instead of what
// changed, and a child app left running (its own labels) sees its
// entry empty meanwhile. The plan has a reflinks check against the
// child's mnt first, rsync for all when it fails, said once: nothing
// should empty the child's entry when the filesystem cannot clone.
func TestReviewOpusShareEmptiesBeforeKnowingItClones(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	write(t, s.log+".shop-dev", "FAKE_APP_REFLINK=true\nFAKE_APP_VOLUME=uploads\n", 0o644)
	write(t, s.log+".shop-prod", "FAKE_APP_VOLUME=uploads\n", 0o644)
	if err := os.MkdirAll(filepath.Join(s.log+".vol", "shop-prod_uploads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.log+".vol", "shop-dev_uploads"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No FAKE_REFLINK: nothing clones on this server.
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code != 0 {
		t.Fatalf("sync (%d):\n%s\n%s", code, out, docker)
	}
	for _, line := range strings.Split(docker, "\n") {
		if strings.Contains(line, "-share-") && strings.Contains(line, "rm -rf") {
			t.Errorf("the child's entry or volume emptied by a clone that cannot work here: %s", line)
		}
	}
	if n := strings.Count(out, "no reflinks between the parent's and the child's"); n > 1 {
		t.Errorf("said %d times, once per entry and volume, not once:\n%s", n, out)
	}
}

// Bug: shareInto mounts the child's entry by its path (-v
// <child mnt>/<entry>:/d), which Docker resolves on the host, and
// empties it as root: a child entry that is a symbolic link (an entry
// nobody owns may be one, the deploy user's: the data on another disk,
// or uploads shared with another environment) has its target emptied
// and written, where the rsync mounts the child's mnt whole and never
// leaves it, and an import, a restore and the owners' step (makeEntry)
// refuse an entry that is a link before writing anything. Nothing should
// mount a linked entry by its path.
func TestReviewOpusShareFollowsLinkedEntry(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	write(t, s.log+".shop-dev", "FAKE_APP_REFLINK=true\n", 0o644)
	elsewhere := filepath.Join(t.TempDir(), "uploads")
	write(t, filepath.Join(elsewhere, "keep.txt"), "not dev's\n", 0o644)
	entry := filepath.Join(s.mnt("dev"), "storage")
	if err := os.RemoveAll(entry); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, entry); err != nil {
		t.Fatal(err)
	}
	out, _ := s.run([]string{"FAKE_REFLINK=1"}, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	for _, line := range strings.Split(docker, "\n") {
		if strings.Contains(line, "-share-") && strings.Contains(line, " -v "+entry+":/d ") {
			t.Errorf("a root container emptied and wrote the child's entry, a link to %s, by its path (Docker follows it on the host):\n%s\n%s", elsewhere, line, out)
		}
	}
	if fileExists(filepath.Join(elsewhere, "file.txt")) {
		t.Errorf("the parent's files written through the child's linked entry into %s", elsewhere)
	}
}
