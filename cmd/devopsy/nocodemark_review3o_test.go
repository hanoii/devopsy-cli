package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Bug: a marked environment's --no-code restore names what goes through when
// a label wants an image its release did not fetch (RestoreLanding's Marked:
// the full restore, as a mark refuses a release), but that check is never
// reached: --check-images runs ReloadCheck first, which asks the very same
// question (fetchedOther) over every service, and its own fix says --release,
// without reading the mark. Copied, the release refuses on the mark.
func TestNoCodeMarkedVariablesNeedAnotherImage(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.labels("FAKE_DB=none")
	shared := filepath.Join(s.root, "shop", "prod", "shared", ".env")
	write(t, shared, "FAKE_DB_IMAGE=mariadb:11\n", 0o600)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	write(t, shared, "FAKE_DB_IMAGE=mariadb:12\n", 0o600)
	write(t, filepath.Join(s.root, "shop", "prod", remote.SyncIncompleteFile), "2026-10-25 00:00:00\tapp\timport\tthe import of app did not complete\n", 0o644)
	s.logs()
	out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--no-code", "--yes")
	if code == 0 || !strings.Contains(out, "only a release fetches one") {
		t.Fatalf("the restore under variables that need another image (%d):\n%s", code, out)
	}
	if strings.Contains(out, "--release\n") {
		release, rcode := s.run(nil, "@prod", "--release")
		if rcode != 0 {
			t.Errorf("the refusal's fix is a release, which the mark refuses (%d):\n%s\nthe release:\n%s", rcode, out, release)
		}
	}
}
