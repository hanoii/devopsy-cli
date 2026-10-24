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
