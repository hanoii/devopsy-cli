package images

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// heldThenOver is a child whose r1 (a build at 11.8, key k1) was held on a
// failed release's 12.1 build, then a release that syncs brought a parent's
// 11.8 data and failed its run step, so r1's own is allowed again: the
// restart of r1 is next, r3's build under the name.
func heldThenOver(t *testing.T, out *bytes.Buffer) (dev *sim, r1, r3, own string) {
	t.Helper()
	dp, d := newFake(), newFake()
	dp.publish("mariadb:11.8", "P11")
	prod := newSim(t, dp, "app-prod", out)
	if _, err := prod.release(false, dbSvc("mariadb:11.8")); err != nil {
		prod.fail("prod: %v", err)
	}
	dev = newSim(t, d, "app-dev", out)
	db := builtDB("app-dev", "11.8")
	db.BuildKey = "k1;UID=a"
	r1, err := dev.release(false, db)
	if err != nil {
		dev.fail("dev: %v", err)
	}
	own = dev.runs("database")
	if _, err := dev.failRun(false, builtDB("app-dev", "12.1")); err != nil {
		dev.fail("failed release: %v", err)
	}
	if heldIn(dev.e.rel(r1))["database"] == "" {
		dev.fail("setup: %s is not held", r1)
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	r3 = dev.e.release(builtDB("app-dev", "11.8"))
	rel := dev.e.rel(r3)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("check: %v", err)
	}
	dev.fetch(dev.e.svcs[r3])
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = r3
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	dev.e.set.SyncData = ""
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	return dev, r1, r3, own
}

// Bug: the last try (--as-built) puts a bookmark back even when its build
// changed since: that is its whole promise (Set.AsBuilt, "never the failed
// release's image under the name"), and rebuilt says so for a service never
// held. For one whose hold is over, Restore requires !changed to take the
// bookmark withOwnBuilds kept, so with a build argument changed since (the
// rebuild get_images just tried having failed) it says the own image is
// "gone" and runs what the name holds: the failed release's build, under r1's
// code and its version, with the bookmark there all along.
func TestAsBuiltHoldOverBuildChanged(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dev, r1, r3, own := heldThenOver(t, &out)
	dev.e.svcs[r1][0].BuildKey = "k2;UID=b"
	dev.e.set.Restart = true
	if err := dev.e.set.Restore(dev.e.rel(r1), dev.e.rel(r3), false); err == nil {
		dev.fail("the first try takes the changed build as it is")
	}
	dev.e.set.AsBuilt = true
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), dev.e.rel(r3), false); err != nil {
		dev.fail("img restore --restart --as-built: %v", err)
	}
	if got := labelsIn(dev.e.rel(r1))["database"]; got != own {
		t.Errorf("%s's last try runs %s, the failed release's build, not its own %s, which its bookmark keeps:\n%s", r1, got, own, out.String())
	}
}

// Bug: withOwnBuilds keeps no bookmark of its own when the held image is the
// release's own build (a label raised over the same cached build: the failed
// release's image id is r1's), since the entry's bookmark holds it already,
// and Restore looks only at the entry of no service. So once the hold is over
// the last try says the image is gone and runs the failed release's build
// under the name, though the entry's own bookmark holds exactly that build.
func TestAsBuiltHoldOverSameImage(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	dp.publish("mariadb:11.8", "P11")
	prod := newSim(t, dp, "app-prod", &out)
	if _, err := prod.release(false, dbSvc("mariadb:11.8")); err != nil {
		prod.fail("prod: %v", err)
	}
	dev := newSim(t, d, "app-dev", &out)
	r1, err := dev.release(false, builtDB("app-dev", "11.8"))
	if err != nil {
		dev.fail("dev: %v", err)
	}
	own := dev.runs("database")
	// The label raised, the build cached: the same image under 12.1.
	r2 := dev.e.release(builtDB("app-dev", "12.1"))
	if err := dev.e.set.Check(dev.e.rel(r2)); err != nil {
		dev.fail("check: %v", err)
	}
	if err := dev.e.set.Record(dev.e.rel(r2), dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = r2
	dev.up()
	dev.e.set.Ran(dev.base(), dev.e.rel(r2))
	os.WriteFile(filepath.Join(dev.e.rel(r2), failedFile), nil, 0o644)
	dev.restart(r1, r2)
	if heldIn(dev.e.rel(r1))["database"] == "" {
		dev.fail("setup: %s is not held", r1)
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	r3 := dev.e.release(builtDB("app-dev", "11.8"))
	rel := dev.e.rel(r3)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("check: %v", err)
	}
	dev.fetch(dev.e.svcs[r3])
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = r3
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	dev.e.set.SyncData = ""
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	dev.e.set.Restart, dev.e.set.AsBuilt = true, true
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); err != nil {
		dev.fail("img restore --restart --as-built: %v", err)
	}
	if got := labelsIn(dev.e.rel(r1))["database"]; got != own {
		t.Errorf("%s's last try runs %s, the failed release's build, not its own %s, which its entry's bookmark holds:\n%s", r1, got, own, out.String())
	}
}
