package images

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Bug: a held service's own build keeps a bookmark now (withOwnBuilds), but
// Restore still takes it for gone once the hold is over: its restart's last
// try (--as-built, the build having failed) says "its own image is gone" and
// runs what the name holds, the failed release's build, under the release's
// code. AsBuilt's promise is the reverse ("never the failed release's image
// under the name"), and the bookmark that would give it back is dropped.
// Here r1 is held on 12.1's build, a release that syncs brings the parent's
// 11.8 data, so r1's own 11.8 is allowed again, and its run step fails.
func TestAsBuiltHoldOverRunsItsOwnBuild(t *testing.T) {
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
	if _, err := dev.failRun(false, builtDB("app-dev", "12.1")); err != nil {
		dev.fail("failed release: %v", err)
	}
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
	dev.e.set.Restart = true
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); !errors.Is(err, ErrGone) {
		dev.fail("img restore --restart: %v", err)
	}
	// The build fails: its last try.
	dev.e.set.AsBuilt = true
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); err != nil {
		dev.fail("img restore --restart --as-built: %v", err)
	}
	if got := labelsIn(dev.e.rel(r1))["database"]; got != own {
		t.Errorf("%s runs %s, not its own build %s, which its bookmark kept:\n%s", r1, got, own, out.String())
	}
}
