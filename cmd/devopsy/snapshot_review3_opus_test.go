package main

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Bug (review, opus, round 3): a full restore gives its owned entries the restored code's
// owners (cli.MntOwners(restored)) and makes its volumes with the restored code's compose,
// "never the current release's", but round 2's fix gives the owned volumes' roots by the
// current release's labels (ownProjectVolumes(ctx, cwd, ...)): the release the restore
// replaces. A volume the current release owns and the snapshot's code does not (the label
// added since the take) gets the current release's owner over the root the snapshot carried,
// and the run step's --mnt-owners restored, reading the restored code, never gives it back:
// the restored service, on its image's own user, then cannot write its own volume's root.
// The same call makes the current release's owned volumes (ensureVolumes, with its compose
// for one with options) and fails the restore, its services left down, on a chown only the
// replaced release asks for. Fix (cause): give the roots of the restored code's owned
// volumes (cli.OwnedVolumes(restored), made with restoredCode), as ownMnt takes restored's
// owners, never cwd's.
func TestRestoreGivesVolumesTheRestoredCodesOwnersReview3Opus(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.labels("FAKE_DB_VOLUME=dbdata FAKE_DB_NO_BIND=1 FAKE_VOLUME_NOCOPY=1")
	s.releaseProd(nil)
	write(t, filepath.Join(s.log+".vol", "shop-prod_dbdata", "ibd"), "pages\n", 0o644)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	// The current release's db owns its volume now; the snapshot's code has no owner for it.
	write(t, s.log+".current.shop-prod", "FAKE_DB_OWNER=2000000123:2000000123\n", 0o644)
	s.logs()
	out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--yes")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(docker, "target=/mnt/dbdata") {
		t.Fatalf("the restore (%d):\n%s\n%s", code, out, docker)
	}
	// The restore's own owners' containers (importContainer's), apart from the run step's.
	own := regexp.MustCompile(`devopsy-import-shop-prod-[0-9]+-owners-vol-[0-9]+ .*sh dbdata 2000000123:2000000123`)
	for _, line := range strings.Split(docker, "\n") {
		if own.MatchString(line) {
			t.Errorf("the restore gave the volume's root the owner of the release it replaces, which the restored code does not name:\n%s", line)
		}
	}
}
