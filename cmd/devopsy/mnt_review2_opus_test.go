package main

import (
	"regexp"
	"strings"
	"testing"
)

// Bug (review round 2): an --import of a service whose named volume devopsy.mnt.owner owns
// extracts the tar into the volume, its root then the tar's owner (mapped by range, or kept:
// root's, an image's own user, another server's), and gives owners back to the owned
// entries alone (ownMnt), never the owned volume's root (ownVolumes), though an import is an
// owners' point (internals/data.md) and the label's promise is the root's owner wherever
// owners are given: the service on its own uid then cannot write the root of its own
// volume until the next release. A snapshot's restore (snapshotRestoreData) has the same
// gap, and a sync's rsync or clone of the volume rewrites the root a reset's ownVolumes just
// gave. Fix (cause): one owners' step after every write into an environment's data, entries
// and volumes together (a helper the import, the restore and the sync's end all call), rather
// than ownMnt alone at each site.
func TestImportGivesAnOwnedVolumesRoot(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	env := []string{"FAKE_DB_VOLUME=dbdata", "FAKE_DB_NO_BIND=1", "FAKE_DB_OWNER=2000000123:2000000123", "FAKE_VOLUME_NOCOPY=1"}
	if out, code := s.run(env, "@solo", "--release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	_, errs, code := s.runIO(env, tarOf(t, map[string]string{"dbdata/ibd": "pages\n"}), "@solo", "--import", "db", "--yes")
	docker, _ := s.logs()
	if code != 0 {
		t.Fatalf("the import (%d):\n%s\n%s", code, errs, docker)
	}
	extract := strings.Index(docker, "--mount type=volume,source=shop-solo_dbdata,target=/mnt/dbdata")
	own := regexp.MustCompile(`-v shop-solo_dbdata:/vol/dbdata devopsy\.internal/util:5 sh -c chown "\$2" "/vol/\$1" sh dbdata 2000000123:2000000123\n`).FindStringIndex(docker)
	if extract < 0 || own == nil || own[0] < extract {
		t.Errorf("the owned volume's root not given after the import's extraction (extract at %d, owner at %v):\n%s\n%s", extract, own, errs, docker)
	}
}
