package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// A run step that exits 255 (drush does, on a PHP error) failed: the
// release went back, which devopsy must not report as a lost
// connection whose release goes on. Fails on v0.42.0: ssh passes the
// script's status on, and sshLost takes every 255 for ssh's own
// (cmd/devopsy/remote.go:380; sync.go:289 for a sync's run step).
func TestRunStepExit255(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "commands", "deploy"), "#!/bin/sh\n[ -z \"${FAKE_RUN_EXIT:-}\" ] || exit \"$FAKE_RUN_EXIT\"\necho deployed\n", 0o755)
	if out, code := s.run(nil, "@solo", "--release", "--yes"); code != 0 {
		t.Fatalf("first release (%d):\n%s", code, out)
	}
	first := s.current("solo")
	out, code := s.run([]string{"FAKE_RUN_EXIT=255"}, "@solo", "--release", "--yes")
	if code == 0 || s.current("solo") != first {
		t.Fatalf("a failed run step (%d), current %q, was %q:\n%s", code, s.current("solo"), first, out)
	}
	if strings.Contains(out, "connection to @") {
		t.Errorf("a run step exiting 255 reported as a lost connection:\n%s", out)
	}
}

// A session lost on the way (ssh's 255: a network gone, the server
// rebooted) is said by devopsy, as a release's, a sync's and an
// import's are: --export, --destroy, --snapshots export and --snapshots
// import left the operator with ssh's line alone and exit 255, which
// says neither that devopsy's command did not complete nor what may be
// left on the server (an export's services stopped until the server
// notices, a destroy half done, a truncated file on stdout). Found by
// hand (2026-10-27, iptables drops on an OrbStack machine).
func TestLostSessionSays(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	for _, c := range []struct {
		lost  string
		stdin []byte
		args  []string
	}{
		{"'--export'", nil, []string{"@prod", "--export", "db"}},
		{"exec tar -C", nil, []string{"@prod", "--snapshots", "export", m[1]}},
		{"'receive'", []byte("not a tar"), []string{"@prod", "--snapshots", "import"}},
		{"'--destroy-data'", nil, []string{"@prod", "--destroy", "--yes"}},
	} {
		_, errs, code := s.runIO([]string{"FAKE_SSH_LOST=" + c.lost}, c.stdin, c.args...)
		if code != 255 {
			t.Fatalf("%v: the fake lost no session (%d):\n%s", c.args, code, errs)
		}
		if !strings.Contains(errs, "devopsy: error:") {
			t.Errorf("%v: a session lost on the way (%d) said nothing of devopsy's:\n%s", c.args, code, errs)
		}
	}
}
