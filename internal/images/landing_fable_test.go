package images

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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
