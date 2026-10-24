//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// phasesProject is syncProject whose releases can be held on the server
// in the phases a session can drop in: the child's sync prepare (while
// ~/wait-prepare-<compose project> exists, ~/preparing-... saying so)
// and the run step (~/wait-run-..., ~/running-...), which also logs the
// release it runs in to ~/deploys-<compose project>. files are added.
func phasesProject(t *testing.T, name string, files map[string]string) string {
	all := map[string]string{
		".devopsy/commands/deploy":           "#!/bin/sh\n## Description: starts the project, once ~/wait-run-<project> is gone\nwhile [ -e \"$HOME/wait-run-$COMPOSE_PROJECT_NAME\" ]; do : >\"$HOME/running-$COMPOSE_PROJECT_NAME\"; echo waiting; sleep 0.2; done\nbasename \"$(dirname \"$(pwd -P)\")\" >>\"$HOME/deploys-$COMPOSE_PROJECT_NAME\"\nexec devopsy up -d --wait\n",
		".devopsy/capabilities/data/prepare": "#!/bin/sh\nwhile [ \"$1\" = child ] && [ -e \"$HOME/wait-prepare-$COMPOSE_PROJECT_NAME\" ]; do : >\"$HOME/preparing-$COMPOSE_PROJECT_NAME\"; sleep 0.2; done\n",
	}
	for p, content := range files {
		all[p] = content
	}
	return syncProjectWith(t, name, all)
}

// releaseState is a release's state on disk: failed, pending or live.
func releaseState(t *testing.T, env, id string) string {
	t.Helper()
	out, _ := srv(t, "cd "+env+"/releases/"+id+" && if [ -e devopsy-failed ]; then echo failed; elif [ -e devopsy-pending ]; then echo pending; else echo live; fi")
	return strings.TrimSpace(out)
}

// newest is the newest release's id of env (a path under the deploy
// user's home).
func newest(t *testing.T, env string) string {
	t.Helper()
	out, _ := srv(t, "ls "+env+"/releases | tail -n 1")
	return strings.TrimSpace(out)
}

// A release that syncs dropped silently during its images' fetch, before
// its switch: its watcher counts a holder's quiet time (its stdin is the
// sync's answer too), then cancels the fetch, the build killed; nothing
// live changed and no sync began. devopsy, its connection lost, says the
// server cancels the release, never that it finishes the sync.
func TestReleaseSyncDropDuringFetch(t *testing.T) {
	dir := phasesProject(t, "rsfetch", map[string]string{
		".devopsy/config.yaml": "project: rsfetch\ndefaults:\n  mode: build\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n",
		".devopsy/compose.yaml": `services:
  data:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [./mnt/data:/data]
    labels: {devopsy.data: mnt, devopsy.data.stop: own}
  db:
    image: busybox:1.37
    command: [sleep, infinity]
    labels: {devopsy.data: stream}
  slow:
    build: {context: ./slow, args: {WAIT: "${BUILD_WAIT:-0}"}}
    command: [sleep, infinity]
`,
		".devopsy/slow/Dockerfile": "FROM busybox:1.37\nARG WAIT\nRUN sleep \"$WAIT\"\n",
	})
	// Build mode packs what git tracks.
	if out, code := op(t, dir, "git config --global --add safe.directory '*' && git init -q && git add -A && git -c user.name=e2e -c user.email=e2e@localhost commit -qm e2e"); code != 0 {
		t.Fatalf("git (%d): %s", code, out)
	}
	syncReleaseBoth(t, "rsfetch", dir)
	first, _ := srv(t, "readlink rsfetch/dev/current")
	srv(t, "echo BUILD_WAIT=607 >>rsfetch/dev/shared/.env")
	t.Cleanup(func() { srv(t, "pkill -f '[s]leep 607' || true") })
	background(t, dir, fast()+" devopsy @dev --release-sync --yes", "/tmp/rsfetch.log")
	eventually(t, 3*time.Minute, "the build running", func() bool {
		out, _ := srv(t, "pgrep -f '[s]leep 607' || true")
		return strings.TrimSpace(out) != ""
	})
	id := newest(t, "rsfetch/dev")

	dropSilently(t)
	took := eventually(t, quiet(18, 30*time.Second), "the release lock freed", func() bool { return lockFree(t, "rsfetch/dev/.lock") })
	t.Logf("cancelled %s after the drop", took)
	if now, _ := srv(t, "readlink rsfetch/dev/current"); now != first {
		t.Errorf("current changed: %s, was %s", now, first)
	}
	if state := releaseState(t, "rsfetch/dev", id); state != "failed" || exists(t, "rsfetch/dev/releases/"+id+"/devopsy-fetching") {
		t.Errorf("release %s: %s, fetching marker %v", id, state, exists(t, "rsfetch/dev/releases/"+id+"/devopsy-fetching"))
	}
	eventually(t, 30*time.Second, "the build gone", func() bool {
		out, _ := srv(t, "pgrep -f '[s]leep 607' || true")
		return strings.TrimSpace(out) == ""
	})
	// A gate is named by its sync's id.
	_, ls := srv(t, "ls rsfetch/dev/.devopsy-sync.open.* >/dev/null 2>&1")
	gate := ls == 0
	if syncState(t, "rsfetch", "dev", "data") != "running" || gate || exists(t, "rsfetch/dev/.devopsy-sync.incomplete") {
		t.Errorf("the sync began: dev data %q, gate %v, marked %v", syncState(t, "rsfetch", "dev", "data"), gate, exists(t, "rsfetch/dev/.devopsy-sync.incomplete"))
	}
	if out, _ := op(t, "/", "cat /tmp/rsfetch.log"); strings.Contains(out, "finishes the sync") || !strings.Contains(out, "cancels the release") {
		t.Errorf("devopsy does not say the release is cancelled:\n%s", out)
	}
	if log, _ := srv(t, "cat rsfetch/dev/releases/"+id+"/devopsy-log-server"); !strings.Contains(log, "undoing the fetch") {
		t.Errorf("the undo is not in the release's log:\n%s", log)
	}
	undrop(t)
	op(t, "/", "pkill devopsy || true")
}

// Ctrl-C at a terminal after a release that syncs went current, before
// its copy began (the child's sync prepare running): devopsy answers
// abort and waits, saying so, while the prepare finishes; the sync aborts
// and the release fails: current goes back to the previous release,
// restarted with its own run step, all in the session. devopsy says the
// release does not go live, never that the session leaves it as it was:
// a release that syncs prints the sync's marker after its switch.
func TestReleaseSyncInterruptAfterSwitch(t *testing.T) {
	dir := phasesProject(t, "rsint", nil)
	syncReleaseBoth(t, "rsint", dir)
	first, _ := srv(t, "readlink rsint/dev/current")
	firstID := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(first), "releases/"), "/code")
	srv(t, "rm -f preparing-rsint-dev deploys-rsint-dev finish.log; touch wait-prepare-rsint-dev")
	t.Cleanup(func() { srv(t, "rm -f wait-prepare-rsint-dev preparing-rsint-dev deploys-rsint-dev") })
	inTerminal(t, dir, "devopsy @server:dev --release-sync --yes", "/tmp/rsint")
	eventually(t, 2*time.Minute, "the sync's prepare waiting", func() bool { return exists(t, "preparing-rsint-dev") })
	id := newest(t, "rsint/dev")
	if now, _ := srv(t, "readlink rsint/dev/current"); now == first {
		t.Fatalf("not switched yet: %s", now)
	}
	ctrlC(t)
	eventually(t, time.Minute, "devopsy waiting for the cancel", func() bool {
		out, _ := op(t, "/", "cat /tmp/rsint")
		return strings.Contains(out, "the server is cancelling it")
	})
	srv(t, "rm -f wait-prepare-rsint-dev")
	got := terminalCode(t, "/tmp/rsint")
	eventually(t, time.Minute, "the release's end", func() bool { return lockFree(t, "rsint/dev/.lock") })
	out, _ := op(t, "/", "cat /tmp/rsint")
	t.Logf("devopsy (%s):\n%s", got, out)

	// What the server did.
	deploys, _ := srv(t, "cat deploys-rsint-dev")
	finish, _ := srv(t, "cat finish.log")
	if now, _ := srv(t, "readlink rsint/dev/current"); now != first {
		t.Errorf("current %s, want %s back", now, first)
	}
	if state := releaseState(t, "rsint/dev", id); state != "failed" {
		t.Errorf("release %s: %s", id, state)
	}
	if strings.TrimSpace(deploys) != firstID || syncState(t, "rsint", "dev", "data") != "running" || syncState(t, "rsint", "prod", "data") != "running" {
		t.Errorf("restarted in %q (want %s), dev data %q, prod data %q", deploys, firstID, syncState(t, "rsint", "dev", "data"), syncState(t, "rsint", "prod", "data"))
	}
	if exists(t, "rsint/dev/.devopsy-sync.incomplete") || strings.TrimSpace(finish) != "child abort" {
		t.Errorf("marked %v, finish %q", exists(t, "rsint/dev/.devopsy-sync.incomplete"), finish)
	}
	// The abort was an answer: the holder restored in the session, never
	// into the sync's log.
	if log, _ := srv(t, "cat rsint/dev/.devopsy-sync.log rsint/dev/releases/"+id+"/devopsy-log-server 2>/dev/null"); strings.Contains(log, "no answer from devopsy") || !strings.Contains(log, "current is back to "+firstID) {
		t.Errorf("the server's logs:\n%s", log)
	}

	// What devopsy said.
	if got != "exit=130" {
		t.Errorf("devopsy: %s", got)
	}
	if strings.Contains(out, "leaves it as it was") || !strings.Contains(out, "does not go live") || !strings.Contains(out, "current is back to "+firstID) {
		t.Errorf("devopsy says the child is left as it was, not that the release does not go live:\n%s", out)
	}
}

// Ctrl-C at a terminal during the copy of a release that syncs (its
// import running): devopsy stops the push, then answers abort; the copy
// had started, so the release fails, current goes back, and the previous
// release is not restarted on the partial copy: the child stays down and
// marked, the parent restored.
func TestReleaseSyncInterruptCopy(t *testing.T) {
	dir := phasesProject(t, "rscopy", nil)
	syncReleaseBoth(t, "rscopy", dir)
	first, _ := srv(t, "readlink rscopy/dev/current")
	srv(t, "rm -f importing-rscopy-dev deploys-rscopy-dev; echo 120 >import-wait-rscopy-dev")
	t.Cleanup(func() { srv(t, "rm -f import-wait-rscopy-dev importing-rscopy-dev deploys-rscopy-dev") })
	inTerminal(t, dir, "devopsy @server:dev --release-sync --yes", "/tmp/rscopy")
	eventually(t, 2*time.Minute, "the import running", func() bool { return exists(t, "importing-rscopy-dev") })
	id := newest(t, "rscopy/dev")
	ctrlC(t)
	got := terminalCode(t, "/tmp/rscopy")
	eventually(t, time.Minute, "the release's end", func() bool {
		return lockFree(t, "rscopy/dev/.lock") && lockFree(t, "rscopy/prod/.devopsy-sync.lock")
	})
	out, _ := op(t, "/", "cat /tmp/rscopy")
	if got != "exit=130" || !strings.Contains(out, "stopping the copy") {
		t.Errorf("devopsy (%s):\n%s", got, out)
	}
	if now, _ := srv(t, "readlink rscopy/dev/current"); now != first {
		t.Errorf("current %s, want %s back", now, first)
	}
	deploys, _ := srv(t, "cat deploys-rscopy-dev 2>/dev/null")
	if releaseState(t, "rscopy/dev", id) != "failed" || !exists(t, "rscopy/dev/.devopsy-sync.incomplete") || syncState(t, "rscopy", "dev", "data") != "exited" || strings.TrimSpace(deploys) != "" {
		t.Errorf("release %s, marked %v, dev data %q, restarted in %q", releaseState(t, "rscopy/dev", id), exists(t, "rscopy/dev/.devopsy-sync.incomplete"), syncState(t, "rscopy", "dev", "data"), deploys)
	}
	if out, _ := srv(t, "pgrep -af '[c]apability sync import' || true"); strings.TrimSpace(out) != "" || syncState(t, "rscopy", "prod", "data") != "running" {
		t.Errorf("the import still runs (%s), or prod data %q", out, syncState(t, "rscopy", "prod", "data"))
	}
}

// A release that syncs whose session drops in its run step, after the
// copy went: the run step goes on to its end on the server, the release
// live on the new data, whether the connection died silently (the
// holder's watcher giving up meanwhile) or closed.
func TestReleaseSyncDropInRunStep(t *testing.T) {
	dir := phasesProject(t, "rsrun", nil)
	syncReleaseBoth(t, "rsrun", dir)
	t.Cleanup(func() { srv(t, "rm -f wait-run-rsrun-dev running-rsrun-dev") })
	for _, how := range []string{"silently", "closed"} {
		first, _ := srv(t, "readlink rsrun/dev/current")
		srv(t, "rm -f running-rsrun-dev finish.log; touch wait-run-rsrun-dev; echo "+how+" >rsrun/prod/shared/mnt/data/stamp")
		background(t, dir, fast()+" devopsy @dev --release-sync --yes", "/tmp/rsrun.log")
		eventually(t, 2*time.Minute, how+": the run step waiting", func() bool { return exists(t, "running-rsrun-dev") })
		id := newest(t, "rsrun/dev")
		if how == "silently" {
			dropSilently(t)
			// Past the holder's quiet time: its watcher gives up on the
			// session, while the run step waits.
			time.Sleep(quiet(18, 5*time.Second))
		} else {
			op(t, "/", "pkill -9 -x ssh || true")
			time.Sleep(3 * time.Second)
		}
		srv(t, "rm -f wait-run-rsrun-dev")
		eventually(t, time.Minute, how+": the release's end", func() bool { return lockFree(t, "rsrun/dev/.lock") })
		finish, _ := srv(t, "cat finish.log")
		if now, _ := srv(t, "readlink rsrun/dev/current"); now == first || strings.TrimSpace(now) != "releases/"+id+"/code" {
			t.Errorf("%s: current %s, want release %s", how, now, id)
		}
		if state := releaseState(t, "rsrun/dev", id); state != "live" || syncStamp(t, "rsrun", "dev") != how || syncState(t, "rsrun", "dev", "data") != "running" || !strings.HasSuffix(finish, "child go\n") {
			t.Errorf("%s: release %s, stamp %q, dev data %q, finish %q", how, state, syncStamp(t, "rsrun", "dev"), syncState(t, "rsrun", "dev", "data"), finish)
		}
		if log, _ := srv(t, "cat rsrun/dev/releases/"+id+"/devopsy-log-server"); !strings.Contains(log, "waiting") {
			t.Errorf("%s: the server's log:\n%s", how, log)
		}
		undrop(t)
		out, _ := op(t, "/", "sleep 1; cat /tmp/rsrun.log")
		t.Logf("%s: devopsy:\n%s", how, out)
		op(t, "/", "pkill devopsy || true")
	}
}
