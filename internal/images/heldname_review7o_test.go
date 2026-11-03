package images

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// heldByBookmark leaves dev's first release, which pulls mariadb:11.8, held
// by its bookmark on the build a parent on the same server ran on the data:
// a release that syncs, built, whose run step fails before its database ran,
// restarted on the first. The parent's build is a name only the history
// gives, so holdOn keeps it out of the compose file.
func heldByBookmark(t *testing.T, out *bytes.Buffer) (dev *sim, d *fakeDocker, r1, parents string) {
	t.Helper()
	d = newFake()
	d.publish("mariadb:11.8", "M1")
	prod := newSim(t, d, "app-prod", out)
	if _, err := prod.release(false, builtDB("app-prod", "13")); err != nil {
		prod.fail("prod: %v", err)
	}
	parents = prod.runs("database")
	dev = newSim(t, d, "app-dev", out)
	r1, err := dev.release(false, dbAt("mariadb:11.8", "11.8"))
	if err != nil {
		dev.fail("dev: %v", err)
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	failed := dev.e.release(builtDB("app-dev", "13"))
	rel := dev.e.rel(failed)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("check: %v", err)
	}
	dev.fetch(dev.e.svcs[failed])
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = failed
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	dev.e.set.SyncData = ""
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	dev.strict = true
	dev.restart(r1, failed)
	if dev.runs("database") != parents || heldIn(dev.e.rel(r1))["database"] == "" {
		dev.fail("setup: %s is not held on the parent's build (runs %s)", r1, dev.runs("database"))
	}
	if d.names[familiar("mariadb:11.8")] != "M1" {
		dev.fail("setup: mariadb:11.8 moved already, to %s", d.names[familiar("mariadb:11.8")])
	}
	return dev, d, r1, parents
}

// Bug: a hold by bookmark lists the held image under the release's own name
// (holdOn: e.Ref = o.Ref), with Kind own. RestoreOwn takes that for a built
// image and puts the name on its bookmark, so the next release whose fetch
// fails retags the pulled mariadb:11.8 to the parent's build. From then on the
// release's "own" image is that build: a landing that lets it run again
// (unhold reads what the name holds) runs version 13 on data at 11.8, and img
// ran writes it down as 11.8.
func TestBookmarkHoldFailedFetchRetagsPulledName(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dev, d, _, parents := heldByBookmark(t, &out)
	// An export from another server, at 11.8.
	other := newFake()
	other.publish("mariadb:11.8", "M1")
	base := newSim(t, other, "app-base", &out)
	if _, err := base.release(false, dbAt("mariadb:11.8", "11.8")); err != nil {
		base.fail("base: %v", err)
	}
	d.publish("mariadb:13", "M3")
	id := dev.e.release(dbAt("mariadb:13", "13"))
	dev.fetch(dev.e.svcs[id])
	dev.e.set.Undo(dev.e.rel(id), dev.prev(), false)
	os.WriteFile(filepath.Join(dev.e.rel(id), failedFile), nil, 0o644)
	if got := d.names[familiar("mariadb:11.8")]; got != "M1" {
		t.Errorf("a failed fetch retagged the pulled mariadb:11.8 to %s (the parent's build is %s), not M1", got, parents)
	}
	// What it leads to: 11.8 data imported, whose window lets the release's
	// own run again.
	data := DataFor(base.e.set.ExportData(base.base(), base.e.rel(base.cur), "database"), "database")
	rel := dev.e.rel(dev.cur)
	if err := dev.e.set.CheckLanding(rel, dev.e.set.ImportLanding(data, "database")); err != nil {
		dev.fail("the import's check: %v", err)
	}
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.ImportLanding(data, "database")); err != nil {
		dev.fail("the import: %v", err)
	}
	dev.up()
	if got := dev.runs("database"); got != "M1" {
		t.Errorf("11.8 data runs %s, the parent's version 13 build under the name mariadb:11.8, not M1:\n%s", got, out.String())
	}
}

// Bug: the same entry read back: derived again, source finds it in the release
// itself (its Ref the release's own), so holdOn takes it for a hold under the
// release's own name and moves mariadb:11.8 onto the parent's build, the retag
// of a pulled name its comment rules out. A --no-code restore of a snapshot of
// the held environment, the restart after another failure before its database
// ran and a full restore all derive it again.
func TestBookmarkHoldDerivedAgainRetagsPulledName(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dev, d, _, parents := heldByBookmark(t, &out)
	dev.e.set.Snapshots = filepath.Join(dev.base(), "snapshots")
	snap := dev.snapshot("20261017000001")
	if err := dev.noCode(snap); err != nil {
		dev.fail("--no-code: %v", err)
	}
	if got := d.names[familiar("mariadb:11.8")]; got != "M1" {
		t.Errorf("deriving the hold again retagged the pulled mariadb:11.8 to %s (the parent's build is %s), not M1:\n%s", got, parents, out.String())
	}
}
