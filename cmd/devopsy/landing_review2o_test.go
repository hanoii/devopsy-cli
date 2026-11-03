package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/images"
)

// Bug: an import made again only what it imported when that ran through it
// (recreateRunning); restoreServices now takes every running volumes
// service whose container another activation made, whatever the import
// wrote. An import of db recreates app, which it never touched and which
// runs on (devopsy.data.stop never): a release cut off past its switch
// (nothing finishes it by itself, Sessions, drops and signals) goes live
// for app without its run step, an unrelated service restarts, and its
// up failing fails an import that completed.
func TestImportLeavesOtherServicesContainers(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	base := filepath.Join(s.root, "shop", "prod")
	current := filepath.Join(base, "current")
	write(t, s.log+".shop-prod", "FAKE_DB=mnt\nFAKE_APP=mnt\n", 0o644)
	list := releaseFile(t, current, images.ListFile)
	write(t, list, "shop-prod-app\tsha256:new\tapp\town\t\t\t\tany\t\nshop-prod-db\tsha256:db\tdb\town\t\t\t\tany\t\n", 0o644)
	// app's container is the release before's, db's this one's.
	write(t, s.log+".containers.shop-prod", "app sha256:old\ndb sha256:db\n", 0o644)
	if err := os.MkdirAll(filepath.Join(s.mnt("prod"), "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.logs()
	_, errs, code := s.runIO(nil, tarOf(t, map[string]string{"db/file.txt": "imported\n"}), "@prod", "--import", "db", "--yes")
	docker, _ := s.logs()
	if code != 0 {
		t.Fatalf("the import of db (%d):\n%s", code, errs)
	}
	if strings.Contains(docker, "up -d --no-deps --wait app") {
		t.Errorf("an import of db made app again:\n%s\n%s", errs, docker)
	}
}
