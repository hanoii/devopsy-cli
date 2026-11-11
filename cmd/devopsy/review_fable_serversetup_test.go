package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/setup"
)

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
