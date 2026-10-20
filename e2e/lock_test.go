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
