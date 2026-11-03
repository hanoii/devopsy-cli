package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/images"
)

// Bug: an import lands the export's window and starts the environment's
// own image on the data, which may upgrade it (the receiving side always
// runs its own image), but nothing records that it ran: Ran's comment says
// "after a run step, a sync, an import", no import calls it, and only the
// next activation's img ran catches up. Until then what reads the history
// without an activation reads a version below what the data ran: an
// --export's header, a parent's img data, a snapshot's windows. A database
// at 13 importing a dump of 12 upgrades it; its --export then says 12, and
// an environment at 12 imports it unrefused, running 12 on data 13 changed.
func TestImportRecordsWhatRanOnTheData(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	base := filepath.Join(s.root, "shop", "prod")
	current := filepath.Join(base, "current")
	list := releaseFile(t, current, images.ListFile)
	const digests = "FAKE_IMAGES=sha256:twelve=shop-prod-app@sha256:twelve sha256:thirteen=shop-prod-app@sha256:thirteen"
	write(t, s.log+".shop-prod", "FAKE_APP=mnt+own\n", 0o644)
	// An export of the data at 12.
	write(t, list, "shop-prod-app\tsha256:twelve\tapp\tpulled\tshop-prod-app@sha256:twelve\t\t\t12\tlatest\n", 0o644)
	write(t, filepath.Join(base, images.HistoryFile), "1\tapp\tran\tshop-prod-app\tsha256:twelve\tshop-prod-app@sha256:twelve\t12\tlatest\n", 0o644)
	write(t, filepath.Join(s.mnt("prod"), "storage", "file.txt"), "at 12\n", 0o644)
	exported, errs, code := s.runIO([]string{"FAKE_APP_VERSION=12", digests}, nil, "@prod", "--export", "app")
	if code != 0 {
		t.Fatalf("the export (%d):\n%s", code, errs)
	}
	// Since, a release at 13, whose container runs.
	write(t, list, "shop-prod-app\tsha256:thirteen\tapp\tpulled\tshop-prod-app@sha256:thirteen\t\t\t13\tlatest\n", 0o644)
	write(t, filepath.Join(base, images.HistoryFile), "2\tapp\tran\tshop-prod-app\tsha256:thirteen\tshop-prod-app@sha256:thirteen\t13\tlatest\n", 0o644)
	write(t, s.log+".containers.shop-prod", "app shop-prod-app@sha256:thirteen sha256:thirteen\n", 0o644)
	env := []string{"FAKE_APP_VERSION=13", digests}
	if _, errs, code := s.runIO(env, exported, "@prod", "--import", "app", "--yes"); code != 0 {
		t.Fatalf("the import of the dump at 12 into 13 (%d):\n%s", code, errs)
	}
	// 13 was stopped, the data replaced, 13 started on it.
	if docker, _ := s.logs(); !strings.Contains(docker, "start app") {
		t.Fatalf("setup: app did not start again on the data:\n%s", docker)
	}
	again, errs, code := s.runIO(env, nil, "@prod", "--export", "app")
	if code != 0 {
		t.Fatalf("the export after it (%d):\n%s", code, errs)
	}
	spool := filepath.Join(t.TempDir(), "export.tar")
	write(t, spool, string(again), 0o644)
	record := images.DataFor(tarRecord(spool), "app")
	if !strings.Contains(record, "\t13\t") {
		t.Errorf("app runs 13 on the imported data, and its export says only:\n%s", record)
	}
}
