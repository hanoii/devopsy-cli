//go:build e2e

package e2e

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// releasedID is the id a release's output says it releases.
var releasedID = regexp.MustCompile(`releasing ([0-9]{14}) to`)

// A release that finds another holding the release lock says it waits;
// cut while it waits (Ctrl-C at a terminal, its connection dropped
// silently), it changes nothing: the release holding the lock goes live
// as it would have, and none of the cut one's appears once the lock is
// free: devopsy sends the tar only once the upload holds the lock, so the
// script finds none.
func TestReleaseLockWait(t *testing.T) {
	dir := dropProject(t, "lockwait")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	t.Cleanup(func() { srv(t, "rm -f wait-prepare preparing") })
	for _, cut := range []string{"Ctrl-C", "a silent drop"} {
		before, _ := srv(t, "ls lockwait/prod/releases")
		srv(t, "rm -f preparing; touch wait-prepare")
		op(t, dir, "sleep 1.1")
		// Not @server, which ctrlC looks for.
		background(t, dir, "DEVOPSY_SERVER=server devopsy @prod --release; echo exit=$?", "/tmp/lockholder.log")
		eventually(t, time.Minute, cut+": the first release's prepare waiting", func() bool { return exists(t, "preparing") })
		op(t, dir, "sleep 1.1")
		if cut == "Ctrl-C" {
			inTerminal(t, dir, "devopsy @server:prod --release", "/tmp/lockwait")
		} else {
			background(t, dir, fast()+" devopsy @server:prod --release", "/tmp/lockwait")
		}
		eventually(t, time.Minute, cut+": the waiting line", func() bool {
			out, _ := op(t, "/", "cat /tmp/lockwait")
			return strings.Contains(out, "waiting for the release running on")
		})
		if cut == "Ctrl-C" {
			ctrlC(t)
			if got := terminalCode(t, "/tmp/lockwait"); got != "exit=130" {
				out, _ := op(t, "/", "cat /tmp/lockwait")
				t.Errorf("%s: %s\n%s", cut, got, out)
			}
		} else {
			dropConnection(t, "[s]tdin-guard")
			eventually(t, quiet(12, 30*time.Second), cut+": the operator's devopsy ended", func() bool {
				_, code := op(t, "/", "pgrep -f '^devopsy @server'")
				return code != 0
			})
			undrop(t)
		}
		out, _ := op(t, "/", "cat /tmp/lockwait")
		if !strings.Contains(out, "nothing changed") {
			t.Errorf("%s: not said that nothing changed:\n%s", cut, out)
		}

		// The first release goes on, unaffected.
		srv(t, "rm -f wait-prepare")
		var holder string
		eventually(t, 2*time.Minute, cut+": the first release's end", func() bool {
			holder, _ = op(t, "/", "cat /tmp/lockholder.log")
			return strings.Contains(holder, "exit=")
		})
		m := releasedID.FindStringSubmatch(holder)
		if !strings.Contains(holder, "exit=0") || m == nil {
			t.Fatalf("%s: the first release:\n%s", cut, holder)
		}
		if now, _ := srv(t, "readlink lockwait/prod/current"); strings.TrimSpace(now) != "releases/"+m[1]+"/code" {
			t.Errorf("%s: current is %s, not the first release's %s", cut, now, m[1])
		}

		// Whatever the cut release left on the server ends once the lock
		// is free, and leaves no release.
		eventually(t, time.Minute, cut+": the cut release's script ended", func() bool {
			_, code := srv(t, "pgrep -f '[s]tdin-guard'")
			return code != 0 && lockFree(t, "lockwait/prod/.lock")
		})
		after, _ := srv(t, "ls lockwait/prod/releases")
		if want := strings.Join(append(strings.Fields(before), m[1]), " "); strings.Join(strings.Fields(after), " ") != want {
			list, _ := op(t, dir, "devopsy @server:prod --releases")
			t.Errorf("%s while waiting for the lock: releases %s, want %s:\n%s", cut, strings.Fields(after), want, list)
		}
	}
}

// --destroy while a release holds the environment's release lock waits
// for it, saying so, then removes everything, the release that held the
// lock included; that release goes live first, unaffected.
func TestDestroyWaits(t *testing.T) {
	dir := dropProject(t, "destroywait")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	srv(t, "rm -f preparing; touch wait-prepare")
	t.Cleanup(func() { srv(t, "rm -f wait-prepare preparing") })
	op(t, dir, "sleep 1.1")
	background(t, dir, "devopsy @server:prod --release; echo exit=$?", "/tmp/destroyholder.log")
	eventually(t, time.Minute, "prepare waiting", func() bool { return exists(t, "preparing") })
	background(t, dir, "devopsy @server:prod --destroy --yes; echo exit=$?", "/tmp/destroywait.log")
	eventually(t, time.Minute, "the waiting line", func() bool {
		out, _ := op(t, "/", "cat /tmp/destroywait.log")
		return strings.Contains(out, "waiting for the release running on")
	})
	if !exists(t, "destroywait/prod/current") {
		t.Errorf("removed while the release held the lock")
	}
	srv(t, "rm -f wait-prepare")
	var holder, destroy string
	eventually(t, 2*time.Minute, "both ended", func() bool {
		holder, _ = op(t, "/", "cat /tmp/destroyholder.log")
		destroy, _ = op(t, "/", "cat /tmp/destroywait.log")
		return strings.Contains(holder, "exit=") && strings.Contains(destroy, "exit=")
	})
	if !strings.Contains(holder, "exit=0") || !strings.Contains(holder, "is now") {
		t.Errorf("the release:\n%s", holder)
	}
	if !strings.Contains(destroy, "exit=0") {
		t.Errorf("--destroy:\n%s", destroy)
	}
	if exists(t, "destroywait/prod") {
		out, _ := srv(t, "ls -laR destroywait")
		t.Errorf("left:\n%s", out)
	}
	if out, _ := srv(t, "docker ps -aq --filter label=com.docker.compose.project=destroywait-prod; docker image ls -q --filter reference='*:devopsy-destroywait-prod-*'"); strings.TrimSpace(out) != "" {
		t.Errorf("containers or bookmarks left: %s", out)
	}
}

// --destroy and --releases remove interrupted at a terminal (Ctrl-C)
// while they wait for the release lock remove nothing once it is free:
// the environment, its containers and the release they named stay: their
// scripts on the server go on waiting after the session ended, then find
// no go from devopsy.
func TestRemovalsCutWhileWaiting(t *testing.T) {
	dir := dropProject(t, "cutwait")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	t.Cleanup(func() { srv(t, "rm -f wait-prepare preparing") })
	for _, command := range []string{"--destroy --yes", "--releases remove %s --yes"} {
		old, _ := srv(t, "ls cutwait/prod/releases | head -n 1")
		old = strings.TrimSpace(old)
		if strings.Contains(command, "%s") {
			command = strings.Replace(command, "%s", old, 1)
		}
		srv(t, "rm -f preparing; touch wait-prepare")
		op(t, dir, "sleep 1.1")
		// Not @server, which ctrlC looks for.
		background(t, dir, "DEVOPSY_SERVER=server devopsy @prod --release; echo exit=$?", "/tmp/cutholder.log")
		eventually(t, time.Minute, command+": prepare waiting", func() bool { return exists(t, "preparing") })
		// On a terminal of its own, as inTerminal, but out of a shell's
		// background, which would start it with SIGINT ignored: these
		// commands take no Ctrl-C of their own, the terminal's ends them.
		if out, code := op(t, dir, "rm -f /tmp/cutwait && setsid -f script -qfec "+quote("devopsy @server:prod "+command)+" /tmp/cutwait >/dev/null 2>&1 </dev/null"); code != 0 {
			t.Fatalf("starting %s (%d): %s", command, code, out)
		}
		eventually(t, time.Minute, command+": the waiting line", func() bool {
			out, _ := op(t, "/", "cat /tmp/cutwait")
			return strings.Contains(out, "waiting for the release running on")
		})
		ctrlC(t)
		eventually(t, 30*time.Second, command+": devopsy's end", func() bool {
			_, code := op(t, "/", "pgrep -f '^devopsy @server'")
			return code != 0
		})
		srv(t, "rm -f wait-prepare")
		eventually(t, 2*time.Minute, command+": the release's end", func() bool {
			out, _ := op(t, "/", "cat /tmp/cutholder.log")
			return strings.Contains(out, "exit=")
		})
		// Whatever the cut command left waiting on the server has run.
		time.Sleep(3 * time.Second)
		eventually(t, time.Minute, command+": the lock free", func() bool { return lockFree(t, "cutwait/prod/.lock") })
		if !exists(t, "cutwait/prod/releases/"+old) {
			out, _ := op(t, "/", "cat /tmp/cutwait")
			list, _ := srv(t, "ls -la cutwait/prod cutwait/prod/releases")
			t.Fatalf("%s, interrupted while it waited, removed %s:\n%s\n%s", command, old, out, list)
		}
		if out, _ := srv(t, "docker ps --filter label=com.docker.compose.project=cutwait-prod --format '{{.State}}'"); strings.TrimSpace(out) != "running" {
			t.Errorf("%s, interrupted while it waited: not running: %q", command, out)
		}
	}
}

// --releases remove and --destroy interrupted at a terminal (Ctrl-C)
// while they wait for the server's image lock (another environment's
// fetch holds it) remove nothing once it is free, as when they wait for
// the release lock.
//
// Bug: their scripts print the locked marker, and take devopsy's go,
// before they take the image lock: --releases remove, cut while it waits
// for it, removes the release once the lock comes (img ignores the gone
// session's SIGPIPE). It should find no go, as it does for the release
// lock, and remove nothing. --destroy only stops by chance, on its first
// write to the gone session.
func TestRemovalsCutWhileWaitingForImages(t *testing.T) {
	dir := dropProject(t, "cutimages")
	for i := 0; i < 2; i++ {
		op(t, dir, "sleep 1.1")
		if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
			t.Fatalf("release (%d):\n%s", code, out)
		}
	}
	t.Cleanup(func() { srv(t, "rm -f hold-images") })
	for _, command := range []string{"--releases remove %s --yes", "--destroy --yes"} {
		old, _ := srv(t, "ls cutimages/prod/releases | head -n 1")
		old = strings.TrimSpace(old)
		if strings.Contains(command, "%s") {
			command = strings.Replace(command, "%s", old, 1)
		}
		// The image lock held shared, as a fetch on the server holds it.
		srv(t, "touch hold-images && (nohup flock -s .devopsy-images.lock sh -c 'while [ -e hold-images ]; do sleep 0.2; done' >/dev/null 2>&1 </dev/null &)")
		eventually(t, 30*time.Second, command+": the image lock held", func() bool { return !lockFree(t, ".devopsy-images.lock") })
		// On a terminal of its own, out of a shell's background (see
		// TestRemovalsCutWhileWaiting).
		if out, code := op(t, dir, "rm -f /tmp/cutimages && setsid -f script -qfec "+quote("devopsy @server:prod "+command)+" /tmp/cutimages >/dev/null 2>&1 </dev/null"); code != 0 {
			t.Fatalf("starting %s (%d): %s", command, code, out)
		}
		eventually(t, time.Minute, command+": the waiting line", func() bool {
			out, _ := op(t, "/", "cat /tmp/cutimages")
			return strings.Contains(out, "waiting for the server's images")
		})
		ctrlC(t)
		eventually(t, 30*time.Second, command+": devopsy's end", func() bool {
			_, code := op(t, "/", "pgrep -f '^devopsy @server'")
			return code != 0
		})
		srv(t, "rm -f hold-images")
		// Whatever the cut command left waiting on the server has run.
		time.Sleep(3 * time.Second)
		eventually(t, time.Minute, command+": the locks free", func() bool {
			return lockFree(t, ".devopsy-images.lock") && lockFree(t, "cutimages/prod/.lock")
		})
		if !exists(t, "cutimages/prod/releases/"+old) {
			out, _ := op(t, "/", "cat /tmp/cutimages")
			list, _ := srv(t, "ls -la cutimages/prod cutimages/prod/releases 2>&1")
			t.Errorf("%s, interrupted while it waited for the image lock, removed %s:\n%s\n%s", command, old, out, list)
		}
		if out, _ := srv(t, "docker ps --filter label=com.docker.compose.project=cutimages-prod --format '{{.State}}'"); strings.TrimSpace(out) != "running" {
			t.Errorf("%s, interrupted while it waited for the image lock: not running: %q", command, out)
		}
	}
}

// --sync cut while its holder waits for the child's release lock (Ctrl-C
// at a terminal, its connection dropped silently) changes nothing on the
// child once the lock is free: its stop=true services keep running, and
// no sync hook runs there.
//
// Bug, the silent drop: the holder's watcher is never armed, and the
// holder reads the session's end only after its prepare: once the lock
// comes it stops the child's stop=true services, runs the sync
// capability's hooks (prepare child, finish child abort) and starts the
// services again, for a sync nobody is there for. Ctrl-C passes only
// because the holder's first write after the lock ("syncing into") goes
// to a closed session and ends it. It should end once it holds the lock,
// before prepare, as the gated scripts do.
func TestSyncCutWhileWaiting(t *testing.T) {
	dir := phasesProject(t, "syncwait", nil)
	syncReleaseBoth(t, "syncwait", dir)
	started := func() string {
		out, _ := srv(t, "docker inspect -f '{{.State.StartedAt}}' $(docker ps -aq --filter label=com.docker.compose.project=syncwait-dev --filter label=com.docker.compose.service=data)")
		return strings.TrimSpace(out)
	}
	t.Cleanup(func() { srv(t, "rm -f wait-run-syncwait-dev running-syncwait-dev") })
	for _, cut := range []string{"Ctrl-C", "a silent drop"} {
		srv(t, "rm -f running-syncwait-dev finish.log; touch wait-run-syncwait-dev")
		op(t, dir, "sleep 1.1")
		// A release of dev holding its lock, in its run step.
		background(t, dir, "devopsy @dev --release-no-sync; echo exit=$?", "/tmp/syncwaitholder.log")
		eventually(t, time.Minute, cut+": dev's release in its run step", func() bool { return exists(t, "running-syncwait-dev") })
		before := started()
		if cut == "Ctrl-C" {
			// On a terminal of its own (see TestRemovalsCutWhileWaiting).
			if out, code := op(t, dir, "rm -f /tmp/syncwait && setsid -f script -qfec "+quote("devopsy @server:dev --sync --yes")+" /tmp/syncwait >/dev/null 2>&1 </dev/null"); code != 0 {
				t.Fatalf("starting --sync (%d): %s", code, out)
			}
		} else {
			background(t, dir, fast()+" devopsy @server:dev --sync --yes", "/tmp/syncwait")
		}
		eventually(t, time.Minute, cut+": the waiting line", func() bool {
			out, _ := op(t, "/", "cat /tmp/syncwait")
			return strings.Contains(out, "waiting for the release running on")
		})
		if cut == "Ctrl-C" {
			ctrlC(t)
		} else {
			// Only the holder's session: the release holding the lock
			// keeps its own.
			dropConnection(t, "[s]yncing into")
		}
		eventually(t, quiet(12, 30*time.Second), cut+": devopsy's end", func() bool {
			_, code := op(t, "/", "pgrep -f '^devopsy @server'")
			return code != 0
		})
		srv(t, "rm -f wait-run-syncwait-dev")
		eventually(t, 2*time.Minute, cut+": the release's end", func() bool {
			out, _ := op(t, "/", "cat /tmp/syncwaitholder.log")
			return strings.Contains(out, "exit=")
		})
		// Whatever the cut sync left waiting on the server has run (a
		// holder's quiet time, with the tests' beat).
		time.Sleep(quiet(18, 5*time.Second))
		eventually(t, time.Minute, cut+": the lock free", func() bool { return lockFree(t, "syncwait/dev/.lock") })
		finish, _ := srv(t, "cat finish.log 2>/dev/null")
		if after := started(); strings.TrimSpace(finish) != "" || after != before || syncState(t, "syncwait", "dev", "data") != "running" {
			out, _ := op(t, "/", "cat /tmp/syncwait")
			t.Errorf("%s while it waited: finish hook %q, data started %s (was %s), data %q:\n%s", cut, finish, after, before, syncState(t, "syncwait", "dev", "data"), out)
		}
		undrop(t)
	}
}
