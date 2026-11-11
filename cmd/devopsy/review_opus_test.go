package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Bug (review, kept failing): a shell's session (--shell-host) exports the address it was
// opened on, and a devopsy run in that shell inside another environment's release names
// that address in its hints over the release's own DEVOPSY_TARGET (sessionHint prefers the
// session's). Copied, the hint acts on the shell's environment, not the one that failed:
// here a sync of prod offered for dev's cut sync, and the same reaches --destroy's "run it
// again" (destroyData, a release's --destroy run from the shell). The hint should name dev.
func TestReviewShellHintNamesTheReleaseItRunsIn(t *testing.T) {
	t.Parallel()
	s := newImportSetup(t)
	home := ""
	for _, kv := range s.syncSetup.env {
		if v, ok := strings.CutPrefix(kv, "DEVOPSY_HOME="); ok {
			home = v
		}
	}
	write(t, filepath.Join(home, "config.yaml"), "releases:\n  root: "+s.root+"\naliases:\n  box: {to: \"devopsy@one:prod\", project: shop}\n", 0o644)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	dev := filepath.Join(s.root, "shop", "dev")
	write(t, filepath.Join(dev, remote.SyncIncompleteFile), remote.IncompleteLine("2026-10-22 10:00:00", "devopsy@one:prod", remote.IncompleteCopy, "the copy was cut"), 0o644)
	// The operator's shell on prod, then a devopsy in dev's release.
	typed := "cd " + remote.Quote(filepath.Join(dev, "current")) + " && devopsy --import db --resolved --yes\n"
	out, errs, code := s.runIO([]string{"SHELL=/bin/sh"}, []byte(typed), "@box", "--shell-host")
	said := string(out) + errs
	if !strings.Contains(said, "only a sync completes it") {
		t.Fatalf("no sync mark's refusal (%d):\n%s", code, said)
	}
	if !strings.Contains(said, "sync it:\ndevopsy: | $ devopsy @devopsy@one:dev --sync\n") {
		t.Errorf("dev's cut sync, from a shell opened on prod, offers another environment's sync:\n%s", said)
	}
}
