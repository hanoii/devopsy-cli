package images

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A snapshot of an environment whose built database was held on a pulled
// image (the failed release's, by its registry digest), imported into
// another server (FilterImported clears its ids), is fully restored there:
// the hold's line names the pulled image's digest, which the registry
// serves, so the restore pulls it by that digest and holds the database on
// it, as the snapshot's listing says. Instead source re-roots the digest
// into the service's own repository, the project's build name
// (app-stage-database@sha256:...), which no registry serves: the pull
// fails and the restore is refused, saying the image is gone and could
// not be pulled by its digest.
func TestImportedSnapshotHoldOnPulledImage(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	d := newFake()
	d.strictRepo = true
	prod := newSim(t, d, "app-prod", &out)
	prod.e.set.Snapshots = filepath.Join(prod.base(), "snapshots")
	d.publish("mariadb:12.1", "J1")
	r1, err := prod.release(false, builtDB("app-prod", "11.8"))
	if err != nil {
		prod.fail("release: %v", err)
	}
	if _, err := prod.failRun(false, dbSvc("mariadb:12.1")); err != nil {
		prod.fail("the failed release: %v", err)
	}
	if heldIn(prod.e.rel(r1))["database"] == "" || prod.runs("database") != "J1" {
		prod.fail("setup: %s is not held on the failed release's image (%v)", r1, heldIn(prod.e.rel(r1)))
	}
	snap := prod.snapshot("20261017000001")

	// Another server, whose registry serves mariadb:12.1 at the same digest.
	d2 := newFake()
	d2.strictRepo = true
	d2.publish("mariadb:12.1", "J1")
	stage := newSim(t, d2, "app-stage", &out)
	stage.e.set.Snapshots = filepath.Join(stage.base(), "snapshots")
	if _, err := stage.release(false, builtDB("app-stage", "11.8")); err != nil {
		stage.fail("release: %v", err)
	}
	dir := filepath.Join(stage.e.set.Snapshots, "20261017000002")
	if err := os.CopyFS(dir, os.DirFS(snap)); err != nil {
		t.Fatal(err)
	}
	if err := FilterImported(dir, stage.e.rel(stage.cur)); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	id, err := stage.fullRestore(dir, []Service{builtDB("app-stage", "11.8")})
	if err != nil {
		t.Fatalf("the full restore of the imported snapshot is refused, the hold's image pullable by its digest (mariadb@J1): %v\n%s", err, out.String())
	}
	if heldIn(stage.e.rel(id))["database"] == "" || stage.runs("database") != "J1" {
		t.Errorf("the restored release runs database on %s, not held on the snapshot's J1 (%v)\n%s", stage.runs("database"), heldIn(stage.e.rel(id)), out.String())
	}
}
