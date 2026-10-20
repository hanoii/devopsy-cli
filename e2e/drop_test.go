//go:build e2e

package e2e

import (
	"regexp"
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
	dropBeforeSwitch(t, "dropbefore", fast(), quiet(6, 20*time.Second))
}

// The same with the real timeouts, whatever E2E_BEAT: 30 seconds.
func TestDropSilentlyRealTimeout(t *testing.T) {
	dropBeforeSwitch(t, "dropreal", "", 50*time.Second)
}

func dropBeforeSwitch(t *testing.T, name, env string, within time.Duration) {
	dir := dropProject(t, name)
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	first, _ := srv(t, "readlink "+name+"/prod/current")
	srv(t, "rm -f preparing; touch wait-prepare")
	t.Cleanup(func() { srv(t, "rm -f wait-prepare preparing") })
	op(t, dir, "sleep 1.1")
	background(t, dir, env+" devopsy @server:prod --release", "/tmp/"+name+".log")
	eventually(t, time.Minute, "prepare waiting", func() bool { return exists(t, "preparing") })

	dropSilently(t)
	took := eventually(t, within, "the release lock freed", func() bool { return lockFree(t, name+"/prod/.lock") })
	t.Logf("cancelled %s after the drop", took)
	if now, _ := srv(t, "readlink "+name+"/prod/current"); now != first {
		t.Errorf("current changed: %s, was %s", now, first)
	}
	if out, _ := srv(t, "pgrep -f '[c]ommands/wait' || true"); strings.TrimSpace(out) != "" {
		t.Errorf("prepare still running: %s", out)
	}
	undrop(t)
	op(t, "/", "pkill devopsy || true")
}

// A session that drops after the switch: the release goes on to its end,
// and --log joins the two logs. With only the operator's ssh killed,
// devopsy saves the session's log, and the server's part follows the
// anchor where the session's stops, before devopsy's own last lines;
// with devopsy killed too (CI's job cancelled), nothing is saved, and
// the server's part shows under the time it started.
func TestDropAfterSwitch(t *testing.T) {
	dir := dropProject(t, "dropafter")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	t.Cleanup(func() { srv(t, "rm -f wait-run running") })
	for _, kill := range []string{"ssh", "devopsy"} {
		first, _ := srv(t, "readlink dropafter/prod/current")
		srv(t, "rm -f running; touch wait-run")
		op(t, dir, "sleep 1.1")
		background(t, dir, fast()+" devopsy @server:prod --release", "/tmp/dropafter.log")
		eventually(t, time.Minute, kill+": the run step waiting", func() bool { return exists(t, "running") })

		// The operator's ssh killed: the server sees the session close.
		if kill == "devopsy" {
			op(t, "/", "pkill -9 -f '^devopsy @server' || true")
		}
		op(t, "/", "pkill -9 ssh || true")
		time.Sleep(3 * time.Second)
		srv(t, "rm -f wait-run")
		eventually(t, 30*time.Second, kill+": the release finished", func() bool {
			_, code := srv(t, "flock -n dropafter/prod/.lock true")
			return code == 0
		})
		eventually(t, 30*time.Second, kill+": devopsy's end", func() bool {
			_, code := op(t, "/", "pgrep -f '^devopsy @server'")
			return code != 0
		})
		if now, _ := srv(t, "readlink dropafter/prod/current"); now == first {
			t.Errorf("%s: current did not switch: %s", kill, now)
		}
		if out, _ := srv(t, "docker ps --filter label=com.docker.compose.project=dropafter-prod --format '{{.State}}'"); strings.TrimSpace(out) != "running" {
			t.Errorf("%s: not running: %q", kill, out)
		}
		if out, _ := srv(t, "cat dropafter/prod/current/../devopsy-log-server"); !strings.Contains(out, "waiting") {
			t.Errorf("%s: the server's log: %s", kill, out)
		}

		log, code := op(t, dir, "devopsy @server:prod --log 2>/dev/null")
		lines := strings.Split(strings.TrimSuffix(log, "\n"), "\n")
		anchor := -1
		for i, line := range lines {
			if strings.Contains(line, "from here, the server's own log") {
				if anchor >= 0 {
					t.Errorf("%s: two anchors in --log:\n%s", kill, log)
				}
				anchor = i
			}
		}
		if code != 0 || anchor < 0 {
			t.Fatalf("%s: no anchor in --log (%d):\n%s", kill, code, log)
		}
		// The server's part, indented, right after the anchor.
		end := anchor + 1
		for end < len(lines) && strings.HasPrefix(lines[end], strings.Repeat(" ", 20)) {
			end++
		}
		part := strings.Join(lines[anchor+1:end], "\n")
		for _, want := range []string{"running 'devopsy deploy' (run)", "waiting", "Healthy"} {
			if !strings.Contains(part, want) {
				t.Errorf("%s: %q not in the server's part of --log:\n%s", kill, want, log)
			}
		}
		session, rest := strings.Join(lines[:anchor], "\n"), strings.Join(lines[end:], "\n")
		if kill == "ssh" {
			if !strings.Contains(session, "current is now") || !strings.Contains(rest, "release exited") {
				t.Errorf("%s: the session's log around the server's part:\n%s", kill, log)
			}
		} else if session != "" || rest != "" || !regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9:]{8} `).MatchString(lines[anchor]) {
			t.Errorf("%s: not the server's part alone, under its time:\n%s", kill, log)
		}
	}
}
