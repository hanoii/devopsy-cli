package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Bug (review round 2): a root run that never ran (the util image's container failing to
// start) applied nothing, yet the apply's check after it says "what was applied did not hold",
// naming every item, beside the run's own error and the config's: applyDoorSetup judges again
// every step it meant to do, where --server-setup's apply (applySetup) drops from its check what
// a failed root run did not say it put.
func TestProjectUsersSetupRootRunThatNeverRan(t *testing.T) {
	t.Parallel()
	s, _, space := doorSetupServer(t)
	out, code := s.run([]string{"FAKE_ROOT_RUN_FAILS=1"}, "@devopsy@one:", "--project-users", "setup", space, "--yes")
	if code != 1 || !strings.Contains(out, "devopsy could not run its setup as root") || strings.Contains(out, "what was applied did not hold") {
		t.Errorf("a root run that never ran (%d):\n%s", code, out)
	}
}

// Bug (review round 2): a space refused before readSpace judges it (a parent others may write,
// swappable; or another user's, dirOwner) keeps Guarded false, so the first session says it "has
// no mount with nosuid and nodev" while the host's mounts show it on one with both.
func TestProjectUsersSetupRefusedSpaceKeepsItsMount(t *testing.T) {
	t.Parallel()
	s, host, space := doorSetupServer(t)
	parent := filepath.Dir(space)
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(host, procMounts), "/dev/vda1 / ext4 rw,relatime 0 0\n/dev/vdb "+parent+" ext4 rw,nosuid,nodev,relatime 0 0\n", 0o644)
	out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", space, "--yes")
	if code != 1 || !strings.Contains(out, "which other users can write") || strings.Contains(out, "has no mount with nosuid and nodev") {
		t.Errorf("a refused space on a guarded mount (%d):\n%s", code, out)
	}
}
