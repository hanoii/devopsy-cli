package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/setup"
)

// The release locks a restart takes are the deploy user's environments' alone: a project
// directory in the release root that another user owns (a project user's tree, with releases:
// root off the deploy user's home) is never walked, as --server-info walks none (ownedByMe).
// Bug: allEnvBases walks every directory of the root, so the apply opens, creating it, a
// .lock in a tree its project user can open to others, through whatever link that user put
// there, and that user refuses the admin's restart by holding it.
func TestRestartLocksOnlyOwnProjects(t *testing.T) {
	t.Parallel()
	// The test's directory, <tmp>/<test>/<n>, is an environment (<project>/<instance>/<env>)
	// of the project <tmp>, in the root above it; <tmp> is another user's wherever tests run
	// as a plain user in the system's /tmp.
	base := t.TempDir()
	project := filepath.Dir(filepath.Dir(base))
	if ownedByMe(project) {
		t.Skip(project + " is this user's: no directory of another user's to hold an environment")
	}
	write(t, filepath.Join(base, "releases", ".keep"), "", 0o644)
	if got := allEnvBases(filepath.Dir(project)); slices.Contains(got, base) {
		t.Errorf("an environment in %s, which another user owns, is among those whose lock a restart takes: %s", project, base)
	}
}

// A release root behind a link (a directory moved to a volume and linked back) and not made
// yet is set up in one run: the mounts drop-in's want comes from the root, and a second run
// changes nothing.
// Bug: the facts resolve the root's links only once it is there, so the first run puts the
// drop-in for the path as written, makes the root, then reads the root resolved, finds the
// drop-in not devopsy's and fails with "what was applied did not hold"; the second run puts
// it again for the other path.
func TestServerSetupRootBehindLink(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	tmp := filepath.Dir(s.root)
	if err := os.Mkdir(filepath.Join(tmp, "vol"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "vol"), filepath.Join(tmp, "data")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tmp, "home", "config.yaml"), "releases:\n  root: "+filepath.Join(tmp, "data", "srv")+"\n", 0o644)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || strings.Contains(out, "did not hold") {
		t.Errorf("the first run (%d):\n%s", code, out)
	}
	first := readString(filepath.Join(host, setup.MountsDropIn))
	out, code = s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || !strings.Contains(out, "nothing to change") || readString(filepath.Join(host, setup.MountsDropIn)) != first {
		t.Errorf("a second run changed something (%d):\n%s\nthe drop-in, first:\n%s\nthen:\n%s", code, out, first, readString(filepath.Join(host, setup.MountsDropIn)))
	}
}

// A release to a server whose release root is missing is refused before its upload makes
// anything: nothing of the root or the environment is there after it, and no lock was taken.
func TestReleaseRefusedLeavesNothing(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	if err := os.Remove(s.root); err != nil {
		t.Fatal(err)
	}
	out, code := s.run(nil, "@prod", "--release")
	if code == 0 || !strings.Contains(out, "the release root "+s.root+" is missing: nothing released") || !strings.Contains(out, "--server-setup") {
		t.Errorf("(%d):\n%s", code, out)
	}
	if fileExists(s.root) {
		t.Errorf("the refused release made %s:\n%s", s.root, out)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "flock") {
		t.Errorf("the refused release took a lock:\n%s", docker)
	}
}
