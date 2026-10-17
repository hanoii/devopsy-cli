package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// The log keeps final states only: no terminal codes, spinner frames or
// the empty lines compose's redraws leave, each line after its time.
func TestReleaseLogText(t *testing.T) {
	l := &releaseLog{}
	l.Line("devopsy: running 'devopsy deploy' (run)")
	_, _ = l.Write([]byte("[+] up 1/2\n \x1b[32m✔\x1b[0m Network n Created      \n \u280b Container c Creating   0.0s\n\n\n"))
	_, _ = l.Write([]byte("\x1b[2A \u2819 Container c Starting\r\n\n ✔ Container c Healthy    0.9s\nwait"))
	_, _ = l.Write([]byte("ing\rdone\n"))
	got := regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d `).ReplaceAllString(string(l.Text()), "")
	want := "devopsy: running 'devopsy deploy' (run)\n[+] up 1/2\n ✔ Network n Created\n ✔ Container c Healthy    0.9s\ndone\n"
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
	if !strings.HasPrefix(string(l.Text()), "20") {
		t.Errorf("no time: %q", l.Text())
	}
}

// A progress display that goes back up to redraw its lines (BuildKit on a
// terminal) leaves only its last frame in the log.
func TestReleaseLogFrames(t *testing.T) {
	l := &releaseLog{}
	for _, chunk := range []string{"start\n", "#1 step one\n#2 step two 0.1s\n", "\x1b[2A\x1b[2K#1 step one DONE\n\x1b[2K#2 step two 0.5s\n", "\x1b[1A\x1b[2K#2 step two DONE\n", "end\n"} {
		_, _ = l.Write([]byte(chunk))
	}
	got := string(l.Text())
	for _, want := range []string{" start\n", " #1 step one DONE\n", " #2 step two DONE\n", " end\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "step one") != 1 || strings.Count(got, "step two") != 1 {
		t.Errorf("frames kept:\n%s", got)
	}
}

// --log joins the session's log and the server's: each server part at the
// time it started, cleaned and indented, a later activation's (a
// rollback's) in its place too.
func TestJoinLogs(t *testing.T) {
	session := "2026-10-17 10:00:00 devopsy release: release of @prod\n" +
		"2026-10-17 10:00:05 devopsy: the images of 1 are ready\n" +
		"2026-10-17 10:00:09 devopsy: release exited 0\n" +
		"2026-10-17 11:00:00 devopsy rollback: rollback of @prod\n" +
		"2026-10-17 11:00:03 devopsy: rollback exited 0\n"
	server := remote.LogSection + "2026-10-17 10:00:05\n" +
		"devopsy: current is now 1\n" +
		"\x1b[32mdeployed\x1b[0m\n" +
		"\n" +
		remote.LogSection + "2026-10-17 11:00:02\n" +
		"devopsy: current is now 0\n"
	got := string(joinLogs([]byte(session + remote.LogServerPart + "\n" + server)))
	pad := strings.Repeat(" ", 20)
	want := "2026-10-17 10:00:00 devopsy release: release of @prod\n" +
		"2026-10-17 10:00:05 devopsy: the images of 1 are ready\n" +
		"2026-10-17 10:00:05 devopsy: from here, the server's own log (" + remote.ServerLogFile + "):\n" +
		pad + "devopsy: current is now 1\n" +
		pad + "deployed\n" +
		"2026-10-17 10:00:09 devopsy: release exited 0\n" +
		"2026-10-17 11:00:00 devopsy rollback: rollback of @prod\n" +
		"2026-10-17 11:00:02 devopsy: from here, the server's own log (" + remote.ServerLogFile + "):\n" +
		pad + "devopsy: current is now 0\n" +
		"2026-10-17 11:00:03 devopsy: rollback exited 0\n"
	if got != want {
		t.Errorf("joined:\n%s\nwant:\n%s", got, want)
	}
	// No session log (its operator gone): the server's alone.
	if got := string(joinLogs([]byte(remote.LogServerPart + "\n" + server))); !strings.HasPrefix(got, "2026-10-17 10:00:05 devopsy: from here, the server's own log") || !strings.Contains(got, pad+"devopsy: current is now 0\n") {
		t.Errorf("server log alone:\n%s", got)
	}
}
