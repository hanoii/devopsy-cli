package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Review round 1 of refactor/fixes, bug: a child's sync refused for a
// bind source leading out of its entry offers --reset in the message's
// prose, "(or start the child over without it: devopsy @dev --reset)",
// the "or ..." start the branch removed everywhere else, instead of as
// one of the numbered fixes under "to fix it, do one of these:" beside
// the mounts check's own (cmd/devopsy/sync.go, CheckLeavingBinds' what).
func TestReviewSyncLeavingBindResetIsAFix(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	write(t, s.log+".shop-dev", "FAKE_APP_MOUNT2=/cache/public\n", 0o644)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	if err := os.MkdirAll(filepath.Join(s.mnt("dev"), "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(s.mnt("dev"), "cache", "public")); err != nil {
		t.Fatal(err)
	}
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	if code == 0 {
		t.Fatalf("a sync into a child with a link out of its entry at a bind source went through:\n%s", out)
	}
	if strings.Contains(out, "(or start the child over") || !strings.Contains(out, "$ devopsy @devopsy@one:dev --reset") {
		t.Errorf("the reset is offered in prose, not as a fix:\n%s", out)
	}
}

// Review round 1 of refactor/fixes, bug: the status capability's command
// on a warning line is its fix, on an info line a hint, but the entry
// keeps a command once by its commands alone: an info line naming it
// first leaves the warning's fix a hint, shown without "to fix it:" and
// "hint": true in --json (parseStatusShow).
func TestReviewStatusShowWarningCommandStaysAFix(t *testing.T) {
	t.Parallel()
	es, bad := parseStatusShow("info\tmaintenance\tscheduled\tdevopsy maintenance off\nwarning\tmaintenance\ton\tdevopsy maintenance off\n")
	if len(bad) != 0 || len(es) != 1 || es[0].Level != remote.StatusWarning || len(es[0].Fixes) != 1 {
		t.Fatalf("entries %+v, bad %q", es, bad)
	}
	if es[0].Fixes[0].Hint {
		t.Errorf("the warning's command is a hint: %+v", es[0].Fixes)
	}
}
