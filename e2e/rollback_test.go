//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// rollbackProject's rollback waits in its prepare step while
// ~/wait-prepare exists, and its run step (deploy, a release's too)
// records in ~/deployed the release it ran in, failing in the one
// ~/fail-run names.
func rollbackProject(t *testing.T, name string) string {
	return project(t, name, map[string]string{
		".devopsy/config.yaml":   "project: " + name + "\nenvironments:\n  prod:\n    mode: image\n    release: deploy\n    rollback: {prepare: [wait], run: deploy}\n",
		".devopsy/compose.yaml":  "services:\n  app:\n    image: busybox:1.37\n    command: [sleep, infinity]\n",
		".devopsy/commands/wait": "#!/bin/sh\n## Description: waits while ~/wait-prepare exists\nwhile [ -e \"$HOME/wait-prepare\" ]; do : >\"$HOME/preparing\"; echo waiting; sleep 0.2; done\n",
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts, failing in the release ~/fail-run names\n" +
			"rel=$(cd \"$DEVOPSY_PROJECT_DIR\" && pwd -P)\necho \"$rel\" >\"$HOME/deployed\"\n" +
			"if [ -s \"$HOME/fail-run\" ] && case \"$rel\" in */\"$(cat \"$HOME/fail-run\")\"/*) true ;; *) false ;; esac; then echo failing; exit 3; fi\n" +
			"exec devopsy up -d --wait\n",
	})
}

// twoReleases releases dir's project to server:prod twice and returns
// both ids, oldest first.
func twoReleases(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	var ids []string
	for i := 0; i < 2; i++ {
		if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
			t.Fatalf("release (%d):\n%s", code, out)
		}
		cur, _ := srv(t, "readlink "+name+"/prod/current")
		ids = append(ids, strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(cur), "releases/"), "/code"))
	}
	return ids[0], ids[1]
}

// A rollback cut before its switch (Ctrl-C at a terminal, a connection
// that dies silently) changes nothing live: current stays, neither
// release is marked failed or rolled back, the lock is freed, and the
// next rollback goes through.
func TestRollbackCutBeforeSwitch(t *testing.T) {
	dir := rollbackProject(t, "rbcut")
	first, second := twoReleases(t, dir, "rbcut")
	t.Cleanup(func() { srv(t, "rm -f wait-prepare preparing") })
	for _, cut := range []string{"Ctrl-C", "a silent drop"} {
		srv(t, "rm -f preparing; touch wait-prepare")
		if cut == "Ctrl-C" {
			inTerminal(t, dir, "devopsy @server:prod --rollback", "/tmp/rbcut")
		} else {
			background(t, dir, fast()+" devopsy @server:prod --rollback", "/tmp/rbcut")
		}
		eventually(t, time.Minute, cut+": the rollback's prepare waiting", func() bool { return exists(t, "preparing") })
		if cut == "Ctrl-C" {
			// The prepare step's lines reach the terminal as they come.
			eventually(t, 10*time.Second, cut+": the prepare step's output", func() bool {
				out, _ := op(t, "/", "cat /tmp/rbcut")
				return strings.Contains(out, "running 'devopsy wait' (prepare)") && strings.Contains(out, "waiting")
			})
			ctrlC(t)
			if got := terminalCode(t, "/tmp/rbcut"); got != "exit=130" {
				out, _ := op(t, "/", "cat /tmp/rbcut")
				t.Errorf("%s: %s\n%s", cut, got, out)
			}
		} else {
			dropSilently(t)
		}
		eventually(t, quiet(6, 20*time.Second), cut+": the release lock freed", func() bool { return lockFree(t, "rbcut/prod/.lock") })
		if cut != "Ctrl-C" {
			undrop(t)
			op(t, "/", "pkill devopsy || true")
		}
		if out, _ := srv(t, "pgrep -af '[c]ommands/wait' || true"); strings.TrimSpace(out) != "" {
			t.Errorf("%s: the prepare step still runs: %s", cut, out)
		}
		if now, _ := srv(t, "readlink rbcut/prod/current"); strings.TrimSpace(now) != "releases/"+second+"/code" {
			t.Errorf("%s: current is %s, not %s", cut, now, second)
		}
		if out, _ := srv(t, "ls rbcut/prod/releases/*/devopsy-failed rbcut/prod/releases/*/devopsy-rolled-back 2>/dev/null"); out != "" {
			t.Errorf("%s: marked: %s", cut, out)
		}
		srv(t, "rm -f wait-prepare")
	}
	if out, code := op(t, dir, "devopsy @server:prod --rollback"); code != 0 {
		t.Fatalf("the next rollback (%d):\n%s", code, out)
	}
	if now, _ := srv(t, "readlink rbcut/prod/current"); strings.TrimSpace(now) != "releases/"+first+"/code" {
		t.Errorf("rolled back to %s, not %s", now, first)
	}
}

// A rollback whose run step fails marks the release it restored failed,
// brings back the one that was current and restarts it with its own run
// step; on a terminal, the lines of every phase show, colored, and --log
// puts them in the restored release's log. A second rollback finds
// nothing to go back to.
func TestRollbackRunFails(t *testing.T) {
	dir := rollbackProject(t, "rbfail")
	first, second := twoReleases(t, dir, "rbfail")
	srv(t, "echo "+first+" >fail-run")
	t.Cleanup(func() { srv(t, "rm -f fail-run") })
	inTerminal(t, dir, "devopsy @server:prod --rollback", "/tmp/rbfail")
	code := terminalCode(t, "/tmp/rbfail")
	out, _ := op(t, "/", "cat /tmp/rbfail")
	if code != "exit=3" {
		t.Errorf("the rollback: %s\n%s", code, out)
	}
	for _, line := range []string{
		"rolling back to " + first,
		"running 'devopsy wait' (prepare)",
		"current is now " + first,
		"running 'devopsy deploy' (run)",
		"failing",
		"run failed (3), current is back to " + second,
		"restarting " + second + " with its own run step",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("not said: %q\n%s", line, out)
		}
	}
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("no colors:\n%s", out)
	}
	if now, _ := srv(t, "readlink rbfail/prod/current"); strings.TrimSpace(now) != "releases/"+second+"/code" {
		t.Errorf("current is %s, not %s", now, second)
	}
	if !exists(t, "rbfail/prod/releases/"+first+"/devopsy-failed") || exists(t, "rbfail/prod/releases/"+second+"/devopsy-rolled-back") {
		out, _ := srv(t, "ls rbfail/prod/releases/*")
		t.Errorf("the markers:\n%s", out)
	}
	if out, _ := srv(t, "cat deployed"); !strings.Contains(out, "/"+second+"/") {
		t.Errorf("the last run step ran in %s, not %s", out, second)
	}
	if out, _ := srv(t, "docker ps --filter label=com.docker.compose.project=rbfail-prod --format '{{.State}}'"); strings.TrimSpace(out) != "running" {
		t.Errorf("not running: %q", out)
	}
	if out, _ := op(t, dir, "devopsy @server:prod --releases"); !strings.Contains(out, " "+first+"  ") || !strings.Contains(out[strings.Index(out, " "+first+"  "):], "FAILED") {
		t.Errorf("--releases:\n%s", out)
	}
	// The rollback's log is the restored release's, its server's part
	// after its anchor, before its end.
	log, _ := op(t, dir, "devopsy @server:prod --log "+first+" 2>/dev/null")
	rolling := strings.Index(log, "rolling back to "+first)
	anchor := strings.LastIndex(log, "from here, the server's own log")
	restart := strings.Index(log, "restarting "+second)
	end := strings.Index(log, "rollback exited 3")
	if rolling < 0 || anchor < rolling || restart < anchor || end < restart {
		t.Errorf("--log %s:\n%s", first, log)
	}
	if out, code := op(t, dir, "devopsy @server:prod --rollback"); code == 0 || !strings.Contains(out, "no release before the current one") {
		t.Errorf("a second rollback (%d):\n%s", code, out)
	}
	if now, _ := srv(t, "readlink rbfail/prod/current"); strings.TrimSpace(now) != "releases/"+second+"/code" {
		t.Errorf("after the second rollback, current is %s", now)
	}
}
