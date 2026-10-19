//go:build e2e

package e2e

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// startSync syncs dev from prod in the background, its import waiting
// wait seconds, and returns once the import runs (the rsync before it
// went: the copy started).
func startSync(t *testing.T, name, dir string, wait int) {
	t.Helper()
	srv(t, "rm -f importing-"+name+"-dev; echo "+strconv.Itoa(wait)+" >import-wait-"+name+"-dev")
	t.Cleanup(func() { srv(t, "rm -f import-wait-"+name+"-dev importing-"+name+"-dev") })
	background(t, dir, fast()+" devopsy @dev --sync --yes", "/tmp/"+name+".log")
	eventually(t, time.Minute, "the import running", func() bool { return exists(t, "importing-"+name+"-dev") })
}

// Both connections cut mid-copy: the push gives up first (6 beats, its
// import killed), then the holder (18): a partial copy, the child down
// and marked, the parent back, every lock freed.
func TestSyncDropBoth(t *testing.T) {
	dir := syncProject(t, "sdboth")
	syncReleaseBoth(t, "sdboth", dir)
	startSync(t, "sdboth", dir, 120)
	dropSilently(t)
	took := eventually(t, quiet(18, 30*time.Second), "the holder's decision", func() bool {
		return lockFree(t, "sdboth/dev/.lock") && lockFree(t, "sdboth/prod/.sync-lock")
	})
	t.Logf("decided %s after the drop", took)
	if !exists(t, "sdboth/dev/.devopsy-sync.incomplete") || syncState(t, "sdboth", "dev", "data") != "exited" || syncState(t, "sdboth", "prod", "data") != "running" {
		t.Errorf("after: marked %v, dev data %q, prod data %q", exists(t, "sdboth/dev/.devopsy-sync.incomplete"), syncState(t, "sdboth", "dev", "data"), syncState(t, "sdboth", "prod", "data"))
	}
	if out, _ := srv(t, "pgrep -f '[c]apability sync import' || true"); strings.TrimSpace(out) != "" {
		t.Errorf("the import still runs: %s", out)
	}
	undrop(t)
	op(t, "/", "pkill devopsy || true")
}

// Only the push's connection cut: the push gives up, devopsy notices its
// session gone and answers abort; the copy had started: partial.
func TestSyncDropPush(t *testing.T) {
	dir := syncProject(t, "sdpush")
	syncReleaseBoth(t, "sdpush", dir)
	startSync(t, "sdpush", dir, 120)
	dropConnection(t, "[-]-sync-push")
	eventually(t, quiet(18, 30*time.Second), "the sync's end", func() bool {
		return lockFree(t, "sdpush/dev/.lock") && lockFree(t, "sdpush/prod/.sync-lock")
	})
	if !exists(t, "sdpush/dev/.devopsy-sync.incomplete") || syncState(t, "sdpush", "dev", "data") != "exited" || syncState(t, "sdpush", "prod", "data") != "running" {
		t.Errorf("after: marked %v, dev data %q, prod data %q", exists(t, "sdpush/dev/.devopsy-sync.incomplete"), syncState(t, "sdpush", "dev", "data"), syncState(t, "sdpush", "prod", "data"))
	}
	undrop(t)
	op(t, "/", "pkill devopsy || true")
}

// Only the holder's connection cut: the push completes (the files say
// so), and the holder, its answer silent, goes on: the child synced.
func TestSyncDropHolder(t *testing.T) {
	dir := syncProject(t, "sdhold")
	syncReleaseBoth(t, "sdhold", dir)
	srv(t, "echo two >sdhold/prod/shared/mnt/data/stamp; rm -f finish.log")
	startSync(t, "sdhold", dir, 2)
	dropConnection(t, "[s]yncing into")
	eventually(t, quiet(18, 40*time.Second), "the holder's end", func() bool { return lockFree(t, "sdhold/dev/.lock") })
	finish, _ := srv(t, "cat finish.log")
	if exists(t, "sdhold/dev/.devopsy-sync.incomplete") || syncStamp(t, "sdhold", "dev") != "two" || syncState(t, "sdhold", "dev", "data") != "running" || !strings.Contains(finish, "child go") {
		t.Errorf("after: marked %v, stamp %q, dev data %q, finish %q", exists(t, "sdhold/dev/.devopsy-sync.incomplete"), syncStamp(t, "sdhold", "dev"), syncState(t, "sdhold", "dev", "data"), finish)
	}
	undrop(t)
	op(t, "/", "pkill devopsy || true")
}
