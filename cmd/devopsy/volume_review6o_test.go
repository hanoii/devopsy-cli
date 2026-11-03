package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/images"
)

// Bug: parentData takes a named volume with no history for one nothing ever
// made, but what the copy skips goes by the volume being there (syncData), and
// a real volume can have no history: Ran only writes what a release's listing
// names, so a data service started by hand or by a project command (a profile
// a release left off, a one-off) has none. The push copies it, with no window:
// CheckLanding never judges it and Land keeps the child's. Here prod's db, on
// a volume at 13, is copied into a child labeled 12: its 12 runs on that data
// unrefused, where before it was refused by the parent's label.
func TestSyncVolumeWithNoHistoryIsJudged(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, s.log+".shop-prod", "FAKE_DB_VOLUME=dbdata\nFAKE_DB_NO_BIND=1\nFAKE_DB_VERSION=13\n", 0o644)
	write(t, s.log+".shop-dev", "FAKE_DB_VOLUME=dbdata\nFAKE_DB_NO_BIND=1\nFAKE_DB_VERSION=12\n", 0o644)
	s.releaseProd(nil)
	vol := filepath.Join(s.log+".vol", "shop-prod_dbdata")
	write(t, filepath.Join(vol, "ibd"), "pages at 13\n", 0o644)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	// Its volume there, its history saying nothing of it.
	os.Remove(filepath.Join(s.root, "shop", "prod", images.HistoryFile))
	write(t, filepath.Join(s.root, "shop", "dev", images.HistoryFile), "1\tdb\tran\tbusybox:12\tsha256:d12\t\t12\t\n", 0o644)
	s.logs()
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	copied := readString(filepath.Join(s.log+".vol", "shop-dev_dbdata", "ibd"))
	if code == 0 || strings.Contains(copied, "at 13") {
		t.Errorf("a child at 12 took a volume at 13 unjudged (%d), its volume %q, its history %q:\n%s", code, copied, readString(filepath.Join(s.root, "shop", "dev", images.HistoryFile)), out)
	}
}
