package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Bug (review 6): a --no-code restore runs the current release's run step
// again (--release-steps release run, as a sync's holder does), but on a
// release cut off past its switch (ActivatingFile) it neither refuses, as
// a reload does (cutRefusal), nor removes the mark once that run step
// went through, as the sync's run step does (sync.go): the release has
// had its run step now, yet --status still says it was cut, and the next
// --reload refuses it until a release. AGENTS.md's premise for the
// reload's refusal ("the others each run a run step or replace it ...
// and nothing else of devopsy's starts its code") leaves this one out.
func TestReview6NoCodeRestoreCut(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	s.logs()
	base := filepath.Join(s.root, "shop", "prod")
	id := strings.TrimPrefix(s.current("prod"), "releases/")
	cut := filepath.Join(base, remote.ActivatingFile)
	write(t, cut, id+"\trelease\t\t\n", 0o644)
	out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--no-code", "--yes")
	if code != 0 || !strings.Contains(out, "deploy in shop-prod") {
		t.Fatalf("the --no-code restore (%d):\n%s", code, out)
	}
	if _, err := os.Stat(cut); err == nil {
		t.Errorf("the cut release's run step ran (the restore's), and it still counts as cut:\n%s", out)
	}
	reload, rcode := s.run(nil, "@prod", "--reload")
	if rcode != 0 || strings.Contains(reload, "cut off past its switch") {
		t.Errorf("the reload after the restore's run step is refused (%d):\n%s", rcode, reload)
	}
}
