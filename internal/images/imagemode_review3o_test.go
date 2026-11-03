package images

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Bug: in image mode a hold's line with digests is taken for one the registry
// serves (source), the reason of its build: service's tag being empty. Across
// a parent in build mode, a line's digests are a local build's (the
// containerd store gives every image some), which no registry serves. A child
// in image mode, after a release that syncs whose run step failed before its
// database ran, holds on that digest: the restart's last try keeps it "for
// the run step to pull", and the previous release's run step cannot start its
// database. The same scenario in build mode runs its own image, said
// (TestRestartAsBuiltHoldOnParentsBuiltImage; README, "the previous release's
// own image runs on it").
func TestRestartImageModeHoldOnParentsLocalBuild(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	prod := newSim(t, dp, "app-prod", &out)
	if _, err := prod.release(false, builtDB("app-prod", "12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	if lines := readHistory(prod.base()).window("database"); len(lines) == 0 || len(lines[0].Digests) == 0 {
		prod.fail("setup: a local build's line has no digests here: %v", lines)
	}
	dev := newSim(t, d, "app-dev", &out)
	dev.e.mode = ModeImage
	r1Svc := Service{Name: "database", Ref: "registry.example.com/db:one", Build: true, Data: true, Method: CopyVolumes, DataVersion: "11.8"}
	d.publish("registry.example.com/db:one", "I1")
	r1, err := dev.release(false, r1Svc)
	if err != nil {
		dev.fail("dev: %v", err)
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	r2Svc := Service{Name: "database", Ref: "registry.example.com/db:two", Build: true, Data: true, Method: CopyVolumes, DataVersion: "12.1"}
	d.publish("registry.example.com/db:two", "J2")
	r2 := dev.e.release(r2Svc)
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
	defer func() { dev.e.set.Restart, dev.e.set.AsBuilt = false, false }()
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); !errors.Is(err, ErrRefused) {
		dev.fail("img restore --restart with the held image on the parent's server only: %v", err)
	}
	dev.e.set.AsBuilt = true
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); err != nil {
		dev.fail("img restore --restart --as-built: %v", err)
	}
	imgs, _, _ := composeFacts(dev.e.rel(r1))
	if img := imgs["database"]; img != "" {
		if err := d.Pull(img); err != nil {
			t.Errorf("%s's compose file points database at %s, a local build's digest on the parent's server, which no registry serves (%v): its run step cannot start it:\n%s", r1, img, err, out.String())
		}
	}

}
