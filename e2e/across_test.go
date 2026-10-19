//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// acrossProject is syncProject with dev on the second server: the
// parent's server reaches the child's with the operator's agent, the
// rsync through a reverse forward, the streams through --sync-exec.
func acrossProject(t *testing.T, name string) string {
	secondServer(t)
	dir := syncProject(t, name)
	op(t, dir, "echo DEVOPSY_SERVER_DEV=server2 >>.devopsy/.env")
	return dir
}

// srv2State is syncState on the second server.
func srv2State(t *testing.T, name, env, service string) string {
	out, _ := srv2(t, "docker ps -a --filter label=com.docker.compose.project="+name+"-"+env+" --filter label=com.docker.compose.service="+service+" --format '{{.State}}'")
	return strings.TrimSpace(out)
}

// A child on another server: its first release syncs (the rsync's and
// the last import's files claimed on the child's server), a copy cut
// after the rsync leaves it partial, and a sync completes it.
func TestSyncAcrossServers(t *testing.T) {
	dir := acrossProject(t, "across")
	stamp := func() string {
		out, _ := srv2(t, "cat across/dev/shared/mnt/data/stamp")
		return strings.TrimSpace(out)
	}
	marked := func() bool {
		_, code := srv2(t, "test -e across/dev/.devopsy-sync.incomplete")
		return code == 0
	}
	srv(t, "rm -f fail-export; mkdir -p across/prod/shared/mnt/data && echo one >across/prod/shared/mnt/data/stamp")
	srv2(t, "rm -f finish.log")
	t.Cleanup(func() { srv(t, "rm -f fail-export") })
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, agent+" devopsy @dev --release"); code != 0 || stamp() != "one" || srv2State(t, "across", "dev", "data") != "running" {
		t.Fatalf("dev's first release, syncing across servers (%d), stamp %q:\n%s", code, stamp(), out)
	}

	srv(t, "echo two >across/prod/shared/mnt/data/stamp && touch fail-export")
	out, code := op(t, dir, agent+" devopsy @dev --sync --yes")
	finish, _ := srv2(t, "cat finish.log")
	if code == 0 || stamp() != "two" || !marked() || srv2State(t, "across", "dev", "data") != "exited" || !strings.Contains(finish, "child partial") {
		t.Errorf("a cut copy (%d), stamp %q, marked %v, data %q, finish %q:\n%s", code, stamp(), marked(), srv2State(t, "across", "dev", "data"), finish, out)
	}
	if left, _ := srv2(t, "ls -a across/dev | grep -E 'devopsy-sync\\.(open|started|done|stream)' || true"); strings.TrimSpace(left) != "" {
		t.Errorf("the sync's files left on the child: %s", left)
	}

	srv(t, "rm -f fail-export; echo three >across/prod/shared/mnt/data/stamp")
	if out, code := op(t, dir, agent+" devopsy @dev --sync --yes"); code != 0 || stamp() != "three" || marked() || srv2State(t, "across", "dev", "data") != "running" {
		t.Errorf("a complete sync (%d), stamp %q, marked %v:\n%s", code, stamp(), marked(), out)
	}
}

// The operator cut silently from the parent's server mid-import, the
// child's still reachable: the push gives up (its heartbeat's silence),
// which ends the import on the child's server through its session, the
// operator's ssh to the parent gives up (KeepAlive) and answers abort,
// and the child, written in part, stays down and marked.
func TestSyncAcrossDropParent(t *testing.T) {
	dir := acrossProject(t, "acrossdrop")
	srv(t, "mkdir -p acrossdrop/prod/shared/mnt/data && echo one >acrossdrop/prod/shared/mnt/data/stamp")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, agent+" devopsy @dev --release"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	srv2(t, "rm -f importing-acrossdrop-dev; echo 120 >import-wait-acrossdrop-dev")
	t.Cleanup(func() { srv2(t, "rm -f import-wait-acrossdrop-dev importing-acrossdrop-dev") })
	background(t, dir, fast()+" "+agent+" devopsy @dev --sync --yes", "/tmp/acrossdrop.log")
	eventually(t, time.Minute, "the import running", func() bool {
		_, code := srv2(t, "test -e importing-acrossdrop-dev")
		return code == 0
	})
	dropSilently(t)
	free := func(path string) bool {
		_, code := srv2(t, "flock -n "+quote(path)+" true")
		return code == 0
	}
	took := eventually(t, quiet(18, 45*time.Second), "the child's decision", func() bool { return free("acrossdrop/dev/.lock") })
	t.Logf("decided %s after the drop", took)
	if out, _ := srv2(t, "pgrep -af '[c]apability sync import' || true"); strings.TrimSpace(out) != "" {
		t.Errorf("the import still runs on the child's server: %s", out)
	}
	if _, code := srv2(t, "test -e acrossdrop/dev/.devopsy-sync.incomplete"); code != 0 || srv2State(t, "acrossdrop", "dev", "data") != "exited" {
		log, _ := op(t, "/", "cat /tmp/acrossdrop.log")
		t.Errorf("not marked, or data %q:\n%s", srv2State(t, "acrossdrop", "dev", "data"), log)
	}
	undrop(t)
	eventually(t, quiet(6, 30*time.Second), "the parent restored", func() bool {
		return lockFree(t, "acrossdrop/prod/.sync-lock") && syncState(t, "acrossdrop", "prod", "data") == "running"
	})
	op(t, "/", "pkill devopsy || true")
}
