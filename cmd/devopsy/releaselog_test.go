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
	t.Parallel()
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
	t.Parallel()
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

// --log joins the session's log and the server's: each server part right
// after the anchor with its tag (shown without it), cleaned and
// indented, whatever the times say; a part no anchor claims (its
// operator gone before saving) at the end, never under a later anchor.
func TestJoinLogs(t *testing.T) {
	t.Parallel()
	anchor := remote.LogAnchor
	session := "2026-10-17 10:00:00 devopsy release: release of @prod\n" +
		"2026-10-17 10:00:05 devopsy: the images of 1 are ready\n" +
		"2026-10-17 10:00:05 " + anchor + " a1\n" +
		"2026-10-17 10:00:05 devopsy: release exited 0\n" +
		"2026-10-17 11:00:00 devopsy rollback: rollback of @prod\n" +
		"2026-10-17 11:00:02 " + anchor + " c3\n" +
		"2026-10-17 11:00:03 devopsy: rollback exited 0\n"
	// The server's clock behind the operator's: places nothing. b2's
	// session log was never saved.
	server := remote.LogSection + "2026-10-17 09:59:58 a1\n" +
		"devopsy: current is now 1\n" +
		"\x1b[32mdeployed\x1b[0m\n" +
		"\n" +
		remote.LogSection + "2026-10-17 10:30:00 b2\n" +
		"devopsy: orphan run\n" +
		remote.LogSection + "2026-10-17 10:59:58 c3\n" +
		"devopsy: current is now 0\n"
	got := string(joinLogs([]byte(session + remote.LogServerPart + "\n" + server)))
	pad := strings.Repeat(" ", 20)
	want := "2026-10-17 10:00:00 devopsy release: release of @prod\n" +
		"2026-10-17 10:00:05 devopsy: the images of 1 are ready\n" +
		"2026-10-17 10:00:05 " + anchor + "\n" +
		pad + "devopsy: current is now 1\n" +
		pad + "deployed\n" +
		"2026-10-17 10:00:05 devopsy: release exited 0\n" +
		"2026-10-17 11:00:00 devopsy rollback: rollback of @prod\n" +
		"2026-10-17 11:00:02 " + anchor + "\n" +
		pad + "devopsy: current is now 0\n" +
		"2026-10-17 11:00:03 devopsy: rollback exited 0\n" +
		"2026-10-17 10:30:00 " + anchor + "\n" +
		pad + "devopsy: orphan run\n"
	if got != want {
		t.Errorf("joined:\n%s\nwant:\n%s", got, want)
	}
}

// A release snapshot's tars run detached from before the switch, their
// output appended to the new release's server log (release-tar), so its
// first lines land before the activation's section: "snapshot <id>:
// mnt/database" and, in a release that syncs, every line of it,
// "snapshot <id> taken" or its failure included. --log shows them all
// the same (found by hand on an OrbStack server, 2026-10-27: neither the
// session nor --log showed them).
func TestJoinLogsBeforeTheFirstSection(t *testing.T) {
	t.Parallel()
	anchor := remote.LogAnchor
	session := "2026-10-27 02:26:04 devopsy release: release of @dev\n" +
		"2026-10-27 02:26:32 " + anchor + " a1\n" +
		"2026-10-27 02:26:40 devopsy: release exited 0\n"
	server := "devopsy: snapshot 20261027022606: mnt/database\n" +
		"devopsy: snapshot 20261027022606 taken\n" +
		remote.LogSection + "2026-10-27 02:26:32 a1\n" +
		"devopsy: current is now 20261027022604\n"
	got := string(joinLogs([]byte(session + remote.LogServerPart + "\n" + server)))
	for _, want := range []string{"snapshot 20261027022606: mnt/database", "snapshot 20261027022606 taken", "current is now 20261027022604"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
