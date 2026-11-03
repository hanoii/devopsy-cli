package images

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Bug: a window's line says nothing of how a service with build: got its
// image: without a tag (rule: no tag check) source takes it for an image
// built here, whatever its registry digests say. In image mode the project
// builds elsewhere and every server pulls by digest, so a child whose
// release that syncs fails before its database ran can be held on the
// parent's image: the registry has it. get_images refuses the hold ("built
// by the project, is gone"), and its last try runs the previous release's
// lower image on the parent's data.
func TestRestartImageModeHoldsByDigest(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	built := func(ref, version string) Service {
		return Service{Name: "database", Ref: ref, Build: true, Data: true, Method: CopyVolumes, DataVersion: version}
	}
	prod := newSim(t, dp, "app-prod", &out)
	prod.e.mode = ModeImage
	dp.publish("registry.example.com/db:c2", "C2")
	if _, err := prod.release(false, built("registry.example.com/db:c2", "12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	dev := newSim(t, d, "app-dev", &out)
	dev.e.mode = ModeImage
	d.publish("registry.example.com/db:c1", "C1")
	r1, err := dev.release(false, built("registry.example.com/db:c1", "11.8"))
	if err != nil {
		dev.fail("dev: %v", err)
	}
	// The registry serves the parent's image too, not pulled here yet.
	d.publish("registry.example.com/db:c2", "C2")
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	d.publish("registry.example.com/db:c3", "C3")
	r2 := dev.e.release(built("registry.example.com/db:c3", "12.1"))
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
	dev.e.set.Restart = true
	defer func() { dev.e.set.Restart = false }()
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); err != nil {
		t.Errorf("the restart refuses a hold whose image the registry serves (%v), and its last try runs the lower image on the parent's data:\n%s", err, out.String())
		return
	}
	imgs, _, _ := composeFacts(dev.e.rel(r1))
	if digestOf(imgs["database"]) != digestOf(dp.digests["C2"][0]) {
		t.Errorf("database is held by %q, not the parent's digest:\n%s", imgs["database"], out.String())
	}
}
