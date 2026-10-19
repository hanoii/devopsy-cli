//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// ctrlC sends SIGINT to the foreground group of devopsy's terminal, as
// a Ctrl-C there does.
func ctrlC(t *testing.T) {
	t.Helper()
	if out, code := op(t, "/", `pid=$(pgrep -f '^devopsy @server' | head -n 1) && kill -INT -$(ps -o pgid= -p "$pid" | tr -d ' ')`); code != 0 {
		t.Fatalf("Ctrl-C (%d): %s", code, out)
	}
}

// Ctrl-C at a terminal: before the switch it cancels the release, after
// it the release goes on and devopsy says so.
func TestInterrupt(t *testing.T) {
	dir := dropProject(t, "ctrlc")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	first, _ := srv(t, "readlink ctrlc/prod/current")
	// Before the switch: cancelled.
	srv(t, "rm -f preparing; touch wait-prepare")
	t.Cleanup(func() { srv(t, "rm -f wait-prepare preparing wait-run running") })
	op(t, dir, "sleep 1.1")
	inTerminal(t, dir, "devopsy @server:prod --release", "/tmp/before")
	eventually(t, time.Minute, "prepare waiting", func() bool { return exists(t, "preparing") })
	ctrlC(t)
	if got := terminalCode(t, "/tmp/before"); got != "exit=130" {
		out, _ := op(t, "/", "cat /tmp/before")
		t.Errorf("before the switch: %s\n%s", got, out)
	}
	if now, _ := srv(t, "readlink ctrlc/prod/current"); now != first {
		t.Errorf("current changed: %s", now)
	}
	srv(t, "rm -f wait-prepare")

	// After it: the release goes on.
	srv(t, "rm -f running; touch wait-run")
	op(t, dir, "sleep 1.1")
	inTerminal(t, dir, "devopsy @server:prod --release", "/tmp/after")
	eventually(t, time.Minute, "the run step waiting", func() bool { return exists(t, "running") })
	ctrlC(t)
	time.Sleep(time.Second)
	srv(t, "rm -f wait-run")
	if got := terminalCode(t, "/tmp/after"); got != "exit=0" {
		out, _ := op(t, "/", "cat /tmp/after")
		t.Errorf("after the switch: %s\n%s", got, out)
	}
	if out, _ := op(t, "/", "cat /tmp/after"); !strings.Contains(out, "the release goes on") {
		t.Errorf("not said:\n%s", out)
	}
	if now, _ := srv(t, "readlink ctrlc/prod/current"); now == first {
		t.Errorf("current did not switch")
	}
}
