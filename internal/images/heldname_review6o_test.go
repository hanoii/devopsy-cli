package images

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Bug: a child held on an image its parent built, on one server, is held by
// image: <the parent's own name> in its compose file (holdOn: "built under
// another name"), a name the parent's next release moves. Its bookmark holds
// the right image, and the listing says that image, but the next up in the
// child (a --reload, any plain up, its run step) follows the name: the
// parent's newer build, at a version the child's data never had, on the
// child's data. holdOn guards a name others run from moving, not from a name
// moving under it.
func TestHoldOnParentsNameFollowsItsNextBuild(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	d := newFake()
	prod := newSim(t, d, "app-prod", &out)
	if _, err := prod.release(false, builtDB("app-prod", "12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	dev := newSim(t, d, "app-dev", &out)
	r1, err := dev.release(false, builtDB("app-dev", "11.8"))
	if err != nil {
		dev.fail("dev: %v", err)
	}
	// A release that syncs, whose run step fails before its database ran.
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	failed := dev.e.release(builtDB("app-dev", "12.1"))
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
	held := dev.runs("database")
	if heldIn(dev.e.rel(r1))["database"] == "" || held != prod.runs("database") {
		dev.fail("setup: %s is not held on the parent's build (%v, runs %s)", r1, heldIn(dev.e.rel(r1)), held)
	}
	// The parent's next release builds under its name.
	if _, err := prod.release(false, builtDB("app-prod", "13")); err != nil {
		prod.fail("prod: %v", err)
	}
	// A plain up in the child, as a reload's.
	dev.up()
	if got := dev.runs("database"); got != held {
		imgs, _, _ := composeFacts(filepath.Clean(dev.e.rel(r1)))
		t.Errorf("the child's up runs the parent's next build %s on its data, not the %s it is held on (image: %s):\n%s", got, held, imgs["database"], out.String())
	}
}
