package images

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Bug: allowedHere takes a release's listing for what it could run, but a
// release whose fetch failed lists whatever the name held (Undo's Bookmark,
// the reason source ranks failed releases last): its label, with the image
// of the release before. Here r2's build failed, so it lists r1's own 11.8
// build under 12.1; the parent's 12.1 data lands, and the restart's last try
// "finds" that image as allowed: r1's older image runs on the parent's data
// with no warning, and img ran then records it as 12.1. With nothing here
// the data allows, the release's own runs, said.
func TestAllowedHereSkipsAFailedFetch(t *testing.T) {
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
	// A release whose build fails: nothing moved, undone.
	r2 := dev.e.release(builtDB("app-dev", "12.1"))
	dev.e.set.Undo(dev.e.rel(r2), dev.prev(), false)
	os.WriteFile(filepath.Join(dev.e.rel(r2), failedFile), nil, 0o644)
	// A release that syncs, its data service's version now any.
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	r3 := dev.e.release(builtDB("app-dev", AnyVersion))
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
	e, _ := match(readList(dev.e.rel(r1)), entry{Service: "database"})
	if e.ID == own && e.DataVersion == "12.1" {
		t.Errorf("%s's own 11.8 build (%s) is listed as 12.1, what its next run records:\n%s", r1, own, out.String())
	}
}
