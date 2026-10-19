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

// A holder killed mid-copy (kill -9, the OOM killer): devopsy stops the
// push, the holder's files stay, and the next to take the lock settles
// them. --status tells a sync under way from leftovers, a release
// refuses before uploading, and a sync completes it.
func TestSyncKilledHolder(t *testing.T) {
	dir := syncProject(t, "skill")
	syncReleaseBoth(t, "skill", dir)
	startSync(t, "skill", dir, 120)
	if out, _ := op(t, dir, "devopsy @dev --status"); !strings.Contains(out, "a sync is copying into it") {
		t.Errorf("--status during the sync:\n%s", out)
	}
	if out, code := srv(t, "pkill -9 -f '[s]yncing into'"); code != 0 {
		t.Fatalf("killing the holder (%d): %s", code, out)
	}
	eventually(t, time.Minute, "the locks freed", func() bool {
		return lockFree(t, "skill/dev/.lock") && lockFree(t, "skill/prod/.sync-lock")
	})
	if out, _ := srv(t, "pgrep -af '[c]apability sync import' || true"); strings.TrimSpace(out) != "" {
		t.Errorf("the import still runs: %s", out)
	}
	if !exists(t, "skill/dev/.devopsy-sync.started") || syncState(t, "skill", "prod", "data") != "running" {
		t.Errorf("after the kill: started %v, prod's data %q", exists(t, "skill/dev/.devopsy-sync.started"), syncState(t, "skill", "prod", "data"))
	}
	if out, _ := op(t, dir, "devopsy @dev --status"); !strings.Contains(out, "did not complete") {
		t.Errorf("--status after:\n%s", out)
	}
	releases, _ := srv(t, "ls skill/dev/releases | wc -l")
	op(t, dir, "sleep 1.1")
	if out, code := op(t, dir, "devopsy @dev --release"); code == 0 || !strings.Contains(out, "did not complete") {
		t.Errorf("a release on it (%d):\n%s", code, out)
	}
	if again, _ := srv(t, "ls skill/dev/releases | wc -l"); again != releases {
		t.Errorf("uploaded anyway: %s releases, were %s", again, releases)
	}
	srv(t, "echo 0 >import-wait-skill-dev; rm -f finish.log")
	if out, code := op(t, dir, "devopsy @dev --sync --yes"); code != 0 || exists(t, "skill/dev/.devopsy-sync.incomplete") || exists(t, "skill/dev/.devopsy-sync.started") || syncState(t, "skill", "dev", "data") != "running" {
		t.Errorf("a sync after it (%d):\n%s", code, out)
	}
	if finish, _ := srv(t, "cat finish.log"); !strings.HasPrefix(finish, "child partial\n") || !strings.HasSuffix(finish, "child go\n") {
		t.Errorf("finish: settled, then completed: %q", finish)
	}
}

// A --release-sync whose push is cut mid-copy: the release fails, the
// previous one is current again but not restarted on the partial copy,
// --releases says FAILED, and a rollback refuses.
func TestReleaseSyncCut(t *testing.T) {
	dir := syncProject(t, "rscut")
	syncReleaseBoth(t, "rscut", dir)
	first, _ := srv(t, "readlink rscut/dev/current")
	srv(t, "rm -f importing-rscut-dev; echo 120 >import-wait-rscut-dev")
	t.Cleanup(func() { srv(t, "rm -f import-wait-rscut-dev importing-rscut-dev") })
	op(t, dir, "sleep 1.1")
	background(t, dir, fast()+" devopsy @dev --release-sync --yes", "/tmp/rscut.log")
	eventually(t, 2*time.Minute, "the import running", func() bool { return exists(t, "importing-rscut-dev") })
	dropConnection(t, "[-]-sync-push")
	eventually(t, quiet(18, 40*time.Second), "the release's end", func() bool {
		return lockFree(t, "rscut/dev/.lock") && lockFree(t, "rscut/prod/.sync-lock")
	})
	undrop(t)
	log, _ := op(t, "/", "sleep 1; cat /tmp/rscut.log")
	if now, _ := srv(t, "readlink rscut/dev/current"); now != first {
		t.Errorf("current %s, want %s", now, first)
	}
	if !exists(t, "rscut/dev/.devopsy-sync.incomplete") || syncState(t, "rscut", "dev", "data") != "exited" || !strings.Contains(log, "not restarting") {
		t.Errorf("marked %v, data %q:\n%s", exists(t, "rscut/dev/.devopsy-sync.incomplete"), syncState(t, "rscut", "dev", "data"), log)
	}
	if out, _ := op(t, dir, "devopsy @dev --releases"); !strings.Contains(out, "FAILED") {
		t.Errorf("--releases:\n%s", out)
	}
	if out, code := op(t, dir, "devopsy @dev --rollback"); code == 0 || !strings.Contains(out, "did not complete") {
		t.Errorf("a rollback (%d):\n%s", code, out)
	}
	op(t, "/", "pkill devopsy || true")
}
