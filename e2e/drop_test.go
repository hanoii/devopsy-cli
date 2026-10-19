//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// dropProject's prepare step and run step wait when a file on the server
// says so, writing all along, so a session can drop in either.
func dropProject(t *testing.T, name string) string {
	return project(t, name, map[string]string{
		".devopsy/config.yaml":     "project: " + name + "\nenvironments:\n  prod:\n    mode: image\n    release: {prepare: [wait], run: deploy}\n    rollback: deploy\n",
		".devopsy/compose.yaml":    "services:\n  app:\n    image: busybox:1.37\n    command: [sleep, infinity]\n",
		".devopsy/commands/wait":   "#!/bin/sh\n## Description: waits while ~/wait-prepare exists\nwhile [ -e \"$HOME/wait-prepare\" ]; do : >\"$HOME/preparing\"; echo waiting; sleep 0.2; done\n",
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts, after waiting while ~/wait-run exists\nwhile [ -e \"$HOME/wait-run\" ]; do : >\"$HOME/running\"; echo waiting; sleep 0.2; done\nexec devopsy up -d --wait\n",
	})
}

// A connection that dies silently before the switch (a laptop asleep):
// the heartbeat's silence cancels the release, its lock freed, nothing
// live changed, the undo in the release's own log.
func TestDropSilentlyBeforeSwitch(t *testing.T) {
	dir := dropProject(t, "dropbefore")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	first, _ := srv(t, "readlink dropbefore/prod/current")
	srv(t, "rm -f preparing; touch wait-prepare")
	t.Cleanup(func() { srv(t, "rm -f wait-prepare preparing") })
	op(t, dir, "sleep 1.1")
	background(t, dir, fast+" devopsy @server:prod --release", "/tmp/dropbefore.log")
	eventually(t, time.Minute, "prepare waiting", func() bool { return exists(t, "preparing") })

	dropSilently(t)
	took := eventually(t, 30*time.Second, "the release lock freed", func() bool {
		_, code := srv(t, "flock -n dropbefore/prod/.lock true")
		return code == 0
	})
	t.Logf("cancelled %s after the drop", took)
	if now, _ := srv(t, "readlink dropbefore/prod/current"); now != first {
		t.Errorf("current changed: %s, was %s", now, first)
	}
	if out, _ := srv(t, "pgrep -f '[c]ommands/wait' || true"); strings.TrimSpace(out) != "" {
		t.Errorf("prepare still running: %s", out)
	}
	undrop(t)
	op(t, "/", "pkill devopsy || true")
}

// A session that drops after the switch: the release goes on to its end.
func TestDropAfterSwitch(t *testing.T) {
	dir := dropProject(t, "dropafter")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	first, _ := srv(t, "readlink dropafter/prod/current")
	srv(t, "rm -f running; touch wait-run")
	t.Cleanup(func() { srv(t, "rm -f wait-run running") })
	op(t, dir, "sleep 1.1")
	background(t, dir, fast+" devopsy @server:prod --release", "/tmp/dropafter.log")
	eventually(t, time.Minute, "the run step waiting", func() bool { return exists(t, "running") })

	// The operator's ssh killed: the server sees the session close.
	op(t, "/", "pkill -9 ssh || true")
	time.Sleep(3 * time.Second)
	srv(t, "rm -f wait-run")
	eventually(t, 30*time.Second, "the release finished", func() bool {
		_, code := srv(t, "flock -n dropafter/prod/.lock true")
		return code == 0
	})
	if now, _ := srv(t, "readlink dropafter/prod/current"); now == first {
		t.Errorf("current did not switch: %s", now)
	}
	if out, _ := srv(t, "docker ps --filter label=com.docker.compose.project=dropafter-prod --format '{{.State}}'"); strings.TrimSpace(out) != "running" {
		t.Errorf("not running: %q", out)
	}
	if out, _ := srv(t, "cat dropafter/prod/current/../devopsy-log-server"); !strings.Contains(out, "waiting") {
		t.Errorf("the server's log: %s", out)
	}
}
