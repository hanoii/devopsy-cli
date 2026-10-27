package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Second review of feat/reflink-syncs (opus). Each test names the bug it
// proves and fails on the branch as it is.

// Bug: where the parent's data clones, a custom service its labels do
// not stop is exported after the set started again (restore, then
// copyStreams), but with the environment computed for the stopped set
// (env := dataHookEnv(services, cli.StoppedNames(set))): the export hook
// is told in DEVOPSY_DATA_STOP that app is stopped while app runs. The
// README's contract says "A service in DEVOPSY_DATA_STOP is stopped:
// reach it with devopsy run --rm --no-deps -T <service>, not exec": a
// script following it runs a second container of a running service on
// prod's data, and one that dumps without a transaction because its
// writer is down dumps under the writer.
func TestReviewOpusExportToldStoppedWhileRunning(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "data")
	exports := s.log + ".exports"
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho \"$1|${DEVOPSY_DATA_STOP:-}|$(devopsy ps --services --status running | tr '\\n' ' ')\" >> \""+exports+"\"\necho \"dump of $1\"\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\ncat >/dev/null\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	// app copied as volumes, stopped own; db a dump, never stopped.
	s.labels("FAKE_APP=mnt+own\nFAKE_DB=stream")
	out, code := s.run([]string{"FAKE_REFLINK=1"}, "@dev", "--sync", "--yes")
	if code != 0 || !strings.Contains(out, "the data clones by reflinks") {
		t.Fatalf("sync (%d):\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(readString(exports)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("the export hook never ran:\n%s", out)
	}
	for _, l := range lines {
		f := strings.SplitN(l, "|", 3)
		if len(f) != 3 {
			t.Fatalf("export line %q", l)
		}
		running := strings.Fields(f[2])
		for _, stop := range strings.Fields(f[1]) {
			if slices.Contains(running, stop) {
				t.Errorf("export %s told DEVOPSY_DATA_STOP=%q while %s runs (running: %v):\n%s", f[0], f[1], stop, running, out)
			}
		}
	}
}

// Bug: the reflink check (cloneCheck: a docker run, then du over all of
// the data) now runs between the last refusal and the stop. A push
// cancelled while it runs (Ctrl-C, the session gone: the check's
// container killed with the push's context) finds reflinks false and
// goes on to stopServices anyway: prod's db is stopped and started again
// for a sync that was cancelled before anything changed, and copies
// nothing. Nothing should stop once the push's context is done.
func TestReviewOpusCancelDuringCheckStopsParent(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "data")
	finished := s.log + ".finish"
	write(t, filepath.Join(caps, "finish"), "#!/bin/sh\necho \"$1 $2\" >> \""+finished+"\"\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	os.Remove(finished)

	wait := filepath.Join(t.TempDir(), "check")
	cut := exec.Command(binary, "@dev", "--sync", "--yes")
	cut.Dir, cut.Stdin = s.project, strings.NewReader("")
	cut.Env = append(append([]string{}, s.env...), "FAKE_REFLINK=1", "FAKE_RUN_WAIT="+wait, "FAKE_RUN_WAIT_ON=-reflink ", "FAKE_SSH_DETACH=1")
	if err := cut.Start(); err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); !fileExists(wait); time.Sleep(50 * time.Millisecond) {
		if time.Since(start) > startWait {
			_ = cut.Process.Kill()
			t.Fatalf("the reflink check never ran:\n%s", readString(s.log))
		}
	}
	_ = cut.Process.Signal(syscall.SIGTERM)
	_ = cut.Wait()
	// The push ends with its finish parent hook, whatever happened.
	for start := time.Now(); !strings.Contains(readString(finished), "parent"); time.Sleep(100 * time.Millisecond) {
		if time.Since(start) > startWait {
			t.Fatalf("the push never finished:\n%s", readString(s.log))
		}
	}
	docker, _ := s.logs()
	for _, line := range strings.Split(docker, "\n") {
		if strings.HasPrefix(line, "shop-prod compose") && strings.Contains(line, " stop db") {
			t.Errorf("prod's db stopped by a push cancelled during its reflink check, before anything changed:\n%s", docker)
			break
		}
	}
}
