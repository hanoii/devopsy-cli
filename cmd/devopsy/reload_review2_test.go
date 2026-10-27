package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// Bug (review 2): the fix a sync's images check names does not fix it
// when the variable that needs another image is an inherited one (the
// parent's inherit:, the usual case: the check reads the pending inherit
// file): "devopsy @<child> --release" fetches under the child's
// shared/inherit.env as it is (a plain release of a released child
// inherits nothing: inheritVars runs for a release that syncs and for
// --inherit), so the next --sync is refused again, in a loop; on a marked
// child the plain release is refused outright. What applies the parent's
// value is --release-sync, or --inherit then --release.
func TestSyncChecksImagesFix(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "config.yaml"), strings.Replace(readString(filepath.Join(s.dot, "config.yaml")), "  prod: {}\n", "  prod: {inherit: [FAKE_DB_IMAGE]}\n", 1), 0o644)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	s.logs()
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "FAKE_DB_IMAGE=mariadb:12\n", 0o644)
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	if code == 0 || !strings.Contains(out, "db's image is mariadb:12 now") {
		t.Fatalf("the sync (%d):\n%s", code, out)
	}
	fix := "$ devopsy @devopsy@one:dev --release"
	if !strings.Contains(out, fix) {
		t.Fatalf("the sync's fix is not %q:\n%s", fix, out)
	}
	s.logs()
	// The fix as named: a plain release of the child.
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("dev's release again (%d):\n%s", code, out)
	}
	s.logs()
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code != 0 || strings.Contains(out, "db's image is mariadb:12 now") {
		t.Errorf("the sync after the fix the check named (%d):\n%s", code, out)
	}
}
