package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/setup"
)

// Bug: the InvocationID fallback for a restart that cuts its own run (round 1's fix 11) reads
// docker.service's InvocationID once, right after the cut. A real restart cuts the run when the
// old daemon stops, and systemd gives the unit a new InvocationID only when the new one
// starts, so that one read finds the old id: the apply calls the run a failure, exits 1, and
// on a first run writes no record, losing its keep answers. TestServerSetupRestartCutsItsOwnRun
// passes only because its fake changes the id before it cuts the run.
func TestReviewOpusRestartCutBeforeItsNewInvocation(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	stoppingSystemctl(t, s, "")
	out, code := s.run([]string{"FAKE_RESTART_CUTS=1"}, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || !strings.Contains(out, "Docker restarted, with daemon.json's values") {
		t.Errorf("(%d):\n%s", code, out)
	}
	if !fileExists(filepath.Join(filepath.Dir(s.root), "home", setup.RecordFile)) {
		t.Errorf("no record after a restart that went:\n%s\nsystemctl:\n%s", out, readString(filepath.Join(host, "systemctl.log")))
	}
}

