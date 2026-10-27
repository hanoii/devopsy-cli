package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Review 2 (opus): --status says a restore under way did not complete.
// A restore writes its mark right before its first write and keeps it,
// with the release lock held, until its run step went through; round 1
// made --server-info read EnvStatus's busy line as "that mark is what
// runs" (said, not warned), but --status, which parses the same lines,
// ignores busy and prints, in red, that the restore did not complete and
// that nothing runs on the data until one does, with a restore to run
// again as the fix, for the whole data step and run step.
func TestReview2StatusRestoreRunning(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	base := filepath.Join(s.root, "shop", "prod")
	if _, err := os.Stat(filepath.Join(base, ".lock")); err != nil {
		write(t, filepath.Join(base, ".lock"), "", 0o644)
	}
	write(t, filepath.Join(base, remote.SyncIncompleteFile), remote.IncompleteLine("2026-10-27 11:00:00", "20261020000000", remote.IncompleteRestore, "restoring"), 0o644)
	// The release lock held, as the restore's activation holds it: the
	// probes of it (LockBusy, flock -n -s .lock) fail.
	bin := filepath.Join(t.TempDir(), "bin")
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\ncase \"$*\" in \"-n -s \"*\"/.lock true\") exit 1 ;; esac\nexec "+filepath.Join(filepath.Dir(s.root), "bin", "flock")+" \"$@\"\n", 0o755)
	out, code := s.run([]string{"PATH=" + bin + ":" + filepath.Join(filepath.Dir(s.root), "bin") + ":/usr/bin:/bin"}, "@prod", "--status")
	if code != 0 || strings.Contains(out, "did not complete") {
		t.Errorf("--status during a restore says it did not complete (%d):\n%s", code, out)
	}
}

// Review 2 (opus): environments behind a symbolic link are not listed.
// --environments finds them (EnvironmentsScript's glob and [ -d ] follow
// links), and every script reaches them through $root/<path>, but
// environments() keeps only ReadDir's directories, which are never
// links: a project moved to another disk and linked back (shop ->
// /mnt/data/shop), or an environment so linked, vanishes from
// --server-info with whatever needs attention there.
func TestReview2EnvironmentsBehindLinks(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root, elsewhere := filepath.Join(tmp, "root"), filepath.Join(tmp, "data")
	for _, d := range []string{
		filepath.Join(elsewhere, "shop", "prod", "releases"),
		filepath.Join(elsewhere, "blog-prod", "releases"),
		filepath.Join(root, "blog"),
		filepath.Join(root, "app", "prod", "releases"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(elsewhere, "shop"), filepath.Join(root, "shop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "blog-prod"), filepath.Join(root, "blog", "prod")); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range environments(root) {
		got = append(got, e.Path)
	}
	for _, want := range []string{"app/prod", "blog/prod", "shop/prod"} {
		if !slices.Contains(got, want) {
			t.Errorf("no %s among %v", want, got)
		}
	}
}
