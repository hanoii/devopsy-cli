package images

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Bug: the restart's last try, its hold's image not to be had, says "its
// release's own image runs" (ownInstead). But when another environment runs
// what the shared name holds now, the name stays (rightly), and nothing
// else points at its own image: no image: in its compose file, where holdOn
// and unhold hold by the release's own bookmark in that same case. Its run
// step's up runs the other environment's image on this environment's data,
// its listing and digest label naming its own, and img ran records that
// image at its own version.
func TestRestartLastTryOwnByBookmark(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	prod := newSim(t, dp, "app-prod", &out)
	if _, err := prod.release(false, builtDB("app-prod", "12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	shared := func(version string) Service {
		return Service{Name: "database", Ref: "shared-db:local", NeverPull: true, Build: true, Data: true, Method: CopyVolumes, DataVersion: version}
	}
	dev := newSim(t, d, "app-dev", &out)
	r1, err := dev.release(false, shared("11.8"))
	if err != nil {
		dev.fail("dev: %v", err)
	}
	own := dev.runs("database")
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	r2 := dev.e.release(shared("12.1"))
	rel := dev.e.rel(r2)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("the release that syncs: %v", err)
	}
	dev.fetch(dev.e.svcs[r2])
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = r2
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	dev.e.set.Ran(dev.base(), rel)
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	dev.e.set.SyncData = ""
	// Meanwhile another environment built and runs the shared name.
	stage := newSim(t, d, "app-stage", &out)
	if _, err := stage.release(false, shared("9")); err != nil {
		stage.fail("stage: %v", err)
	}
	other := stage.runs("database")
	dev.e.set.Restart = true
	defer func() { dev.e.set.Restart, dev.e.set.AsBuilt = false, false }()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); !errors.Is(err, ErrRefused) {
		dev.fail("img restore --restart with the held image on the parent's server only: %v", err)
	}
	dev.e.set.AsBuilt = true
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); err != nil {
		dev.fail("img restore --restart --as-built: %v", err)
	}
	dev.cur = r1
	dev.up()
	if got := dev.runs("database"); got != own {
		t.Errorf("dev's database runs %s (app-stage's is %s), not its own %s, its listing naming its own:\n%s", got, other, own, out.String())
	}
}
