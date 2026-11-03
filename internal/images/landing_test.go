package images

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A refusal's fix runs the command again accepting what was refused, but
// drops what the operator accepted already: with --accept-image db named
// and files refused too, the line says --accept-image files alone. Copied,
// db is refused, whose line says db alone, and the two lines alternate for
// good: no copied line ever goes through.
func TestRefusalFixKeepsAccepted(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:12.1", "I2")
	d.publish("mariadb:11.8", "I1")
	files := func(version string) Service {
		return Service{Name: "files", Ref: "busybox:1", Data: true, Method: CopyVolumes, DataVersion: version}
	}
	d.publish("busybox:1", "B1")
	if _, err := m.release(false, dbSvc("mariadb:12.1"), files("2")); err != nil {
		m.fail("setup: %v", err)
	}
	id := m.e.release(dbSvc("mariadb:11.8"), files("1"))
	m.e.set.Accept = map[string]bool{"database": true}
	defer func() { m.e.set.Accept = nil }()
	out.Reset()
	if err := m.e.set.Check(m.e.rel(id)); !errors.Is(err, ErrRefused) {
		m.fail("files is lower: %v", err)
	}
	if !strings.Contains(out.String(), "--accept-image database,files") && !strings.Contains(out.String(), "--accept-image files,database") {
		t.Errorf("the fix drops what was accepted already:\n%s", out.String())
	}
}

// A sync's refusal keeps the operator's --from in its --accept-image line
// (Set.Again), but its other fix, the release that builds what a copy
// needs, says --release-sync alone: copied, it syncs from the configured
// parent, another source than the one the operator named, or fails on a
// parent that does not resolve.
func TestCopyNeedsFixKeepsFrom(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	prod := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	if _, err := prod.release(false, dbSvc("mariadb:11.8")); err != nil {
		prod.fail("setup: %v", err)
	}
	qa := newSim(t, d, "app-qa", &out)
	qa.release(false, builtDB("app-qa", "11.8"))
	own := qa.runs("database")
	qa.failRun(false, builtDB("app-qa", "12.1"))
	for n, id := range d.names {
		if id == own {
			delete(d.names, n)
		}
	}
	delete(d.images, own)
	qa.e.set.Again = "--sync --from vm2:prod"
	out.Reset()
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	if err := qa.e.set.CheckLanding(qa.e.rel(qa.cur), qa.e.set.SyncLanding(data, "")); !errors.Is(err, ErrRefused) {
		qa.fail("its own build is gone: %v", err)
	}
	if !strings.Contains(out.String(), "--release-sync --from vm2:prod") {
		t.Errorf("the fix drops the source the operator named:\n%s", out.String())
	}
}

// A child on another server than its parent, whose database the parent
// builds: a release that syncs lands the parent's window (its built
// image's lines, an image only the parent's server has), and its run
// step fails before the child's database ran. The restart of the release
// before, lower than the parent's data, holds; the held image cannot be
// had here, so the restart's last try (--as-built) should hold on the
// previous release's own name, that lower image running on the parent's
// data, said (README, "Data and its images"). Instead holdOn keeps the
// parent's own name as the hold's image: (the parent's compose name
// -database), which this server has no image of, so the previous
// release's compose file points its database at a name nothing here
// holds, and its run step's up cannot start it.
func TestRestartAsBuiltHoldOnParentsBuiltImage(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	prod := newSim(t, dp, "app-prod", &out)
	if _, err := prod.release(false, builtDB("app-prod", "12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	dev := newSim(t, d, "app-dev", &out)
	r1, err := dev.release(false, builtDB("app-dev", "11.8"))
	if err != nil {
		dev.fail("dev: %v", err)
	}
	own := dev.runs("database")
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	r2 := dev.e.release(builtDB("app-dev", "12.1"))
	rel := dev.e.rel(r2)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("the release that syncs at the parent's version: %v", err)
	}
	dev.fetch(dev.e.svcs[r2])
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = r2
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	// The run step fails before the database is made again: it still
	// runs r1's build, which img ran does not count.
	dev.e.set.Ran(dev.base(), rel)
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	dev.e.set.SyncData = ""
	// The restart, as get_images runs it: --restart refuses the hold (the
	// parent's image is not here), then --as-built holds all the same.
	dev.e.set.Restart = true
	defer func() { dev.e.set.Restart, dev.e.set.AsBuilt = false, false }()
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); !errors.Is(err, ErrRefused) {
		dev.fail("img restore --restart with the held image on the parent's server only: %v", err)
	}
	dev.e.set.AsBuilt = true
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); err != nil {
		dev.fail("img restore --restart --as-built: %v", err)
	}
	imgs, labels, _ := composeFacts(dev.e.rel(r1))
	if img := imgs["database"]; img != "" {
		if _, err := d.ID(img); err != nil {
			t.Errorf("%s's compose file points database at %s, which this server has no image of (%v): its run step cannot start it; the previous release's own name should hold it, %s running on the data, said:\n%s", r1, img, err, own, out.String())
		}
	}
	if labels["database"] != own {
		t.Errorf("database's digest label is %s, not %s's own build %s", labels["database"], r1, own)
	}
}

// releaseSnapshot writes a release snapshot's manifest at id, taken by
// the activation that replaced rel (its data as it was then).
func releaseSnapshot(t *testing.T, m *sim, id, rel string) {
	t.Helper()
	dir := filepath.Join(m.e.set.Snapshots, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, SnapshotManifest), []byte("format\t1\nkind\trelease\nrelease\t"+rel+"\ntaken\t2026-11-02T00:00:00Z\ncopy\tnone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A refused rollback names the release snapshot from before the data's
// change, so that the operator restores that data, not older data. The
// fix takes the newest release snapshot whose manifest names the
// rollback's target (snapshotFix): right when the data upgrade is the
// current release (its activation took the target's data). But when the
// upgrade failed and the restart holds the previous release on it, the
// rollback's target is one release further back, and the fix names the
// snapshot of that older release's data, taken when the held release
// replaced it: everything written between those two releases is lost by
// the restore it tells the operator to run. The data from before the
// change is the current (held) release's, in the snapshot the failed
// release took.
func TestRollbackRefusalNamesSnapshotBeforeChange(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	d.publish("mariadb:11.8", "I1")
	r0, _ := m.release(false, db118)
	r1, _ := m.release(false, db118)
	// r1's activation snapshotted r0's data; the data written while r1
	// ran is in no snapshot yet.
	releaseSnapshot(t, m, "20261102000001", r0)
	d.publish("mariadb:12.1", "J1")
	r2, _ := m.failRun(false, dbSvc("mariadb:12.1"))
	// r2's activation snapshotted r1's data, from before the upgrade.
	releaseSnapshot(t, m, "20261102000002", r1)
	if m.cur != r1 || m.runs("database") != "J1" || heldIn(m.e.rel(r1))["database"] == "" {
		m.fail("setup: after %s failed, current %s runs %s, held %v", r2, m.cur, m.runs("database"), heldIn(m.e.rel(r1)))
	}
	out.Reset()
	if err := m.rollback(r0, false); !errors.Is(err, ErrRefused) {
		m.fail("the rollback to %s past the upgrade: %v", r0, err)
	}
	if !strings.Contains(out.String(), "--snapshots restore 20261102000002") || strings.Contains(out.String(), "--snapshots restore 20261102000001") {
		t.Errorf("the refusal names the snapshot of %s's data, older than the data from before the change (%s's, in 20261102000002):\n%s", r0, r1, out.String())
	}
}
