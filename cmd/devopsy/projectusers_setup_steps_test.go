package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// Each failure of an apply is said once: a root run that never ran (the util image's container
// failing to start) is said as that alone, nothing it was asked judged again as "did not hold",
// the config it left waiting said in the run's last words; a config that cannot be written is
// said as that alone, never again as a config that does not name the directory.
func TestProjectUsersSetupRootRunThatNeverRan(t *testing.T) {
	t.Parallel()
	s, _, space := doorSetupServer(t)
	out, code := s.run([]string{"FAKE_ROOT_RUN_FAILS=1"}, "@devopsy@one:", "--project-users", "setup", space, "--yes")
	if code != 1 || !strings.Contains(out, "devopsy: error: devopsy could not run its setup as root: ") || strings.Contains(out, "what was applied did not hold") || strings.Count(out, "devopsy: error: ") != 2 ||
		!strings.Contains(out, "devopsy: error: this server is not ready for project users\ndevopsy: | door: space: is not added to "+configFile(s)+" while the project users' directory is not set up\n") {
		t.Errorf("a root run that never ran (%d):\n%s", code, out)
	}
	s, _, space = doorSetupServer(t)
	// The config a link to a file in a directory no one may write: the write fails.
	shut := filepath.Join(filepath.Dir(s.root), "shut")
	write(t, filepath.Join(shut, "config.yaml"), readString(configFile(s)), 0o644)
	if err := os.Remove(configFile(s)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(shut, "config.yaml"), configFile(s)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shut, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(shut, 0o755) })
	out, code = s.run(nil, "@devopsy@one:", "--project-users", "setup", space, "--yes")
	if code != 1 || !strings.Contains(out, "devopsy: error: devopsy could not add the project users' directory to "+configFile(s)+": ") || strings.Contains(out, "what was applied did not hold") {
		t.Errorf("a config that cannot be written (%d):\n%s", code, out)
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

// Bug (review round 2): the door's W0002 for an unguarded space reaches a project user's
// release through --mnt-ready, which asks the door quietly (doorAsk(false, ...)) and folds what
// the door said into one error of its own: the code's two lines become "the door would not
// ready <mnt>: W0002: ...", then "devopsy: | devopsy: | report W0002 to the server's admin:
// exit status 2", the detail kept inside the message with its prefix doubled, so the code is not
// the first line's (SayForAdmin's form) and the detail reads broken.
func TestMntReadyPassesTheDoorsCodeOn(t *testing.T) {
	t.Parallel()
	if reachesDocker() {
		t.Skip("this user reaches Docker, so --mnt-ready asks no door")
	}
	var said strings.Builder
	report.New(&said, false, 0).SayForAdmin(report.ErrorLevel, report.SpaceUnguarded)
	bin := t.TempDir()
	write(t, filepath.Join(bin, "said"), said.String(), 0o644)
	write(t, filepath.Join(bin, "sudo"), "#!/bin/sh\ncat '"+filepath.Join(bin, "said")+"' >&2\nexit 2\n", 0o755)
	dir := t.TempDir()
	out, code := runDevopsy(t, dir, []string{"PATH=" + bin + ":" + fakeBin(t) + ":/usr/bin:/bin"}, remote.MntReadyFlag, dir)
	if code != 1 || !strings.Contains(out, said.String()) || strings.Contains(out, "exit status 2") {
		t.Errorf("the door's W0002 through --mnt-ready (%d):\n%s\nwant the door's lines as it said them:\n%s", code, out, said.String())
	}
}
