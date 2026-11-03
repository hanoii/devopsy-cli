package images

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// syncedAndFailed is a child that builds its database (r1, at 11.8) after a
// release that syncs from a parent that pulls mariadb:12.1 and whose run
// step failed before the child's database ran: the window is the parent's
// line, mariadb:12.1 by its digest, and the restart of r1 must hold.
func syncedAndFailed(t *testing.T, out *bytes.Buffer) (dev *sim, d *fakeDocker, r1, failed string) {
	t.Helper()
	dp := newFake()
	d = newFake()
	d.strictRepo = true
	dp.publish("mariadb:12.1", "P12")
	d.publish("mariadb:12.1", "P12")
	prod := newSim(t, dp, "app-prod", out)
	if _, err := prod.release(false, dbSvc("mariadb:12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	dev = newSim(t, d, "app-dev", out)
	r1, err := dev.release(false, builtDB("app-dev", "11.8"))
	if err != nil {
		dev.fail("dev: %v", err)
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	failed = dev.e.release(builtDB("app-dev", "12.1"))
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
	dev.e.set.Ran(dev.base(), rel)
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	dev.e.set.SyncData = ""
	return dev, d, r1, failed
}

// Bug: source holds a window's digest in the service's own repository,
// whatever the line names. That is right for a mirror, but a service its
// release builds has no repository in any registry: the parent's
// mariadb@<digest> becomes app-dev-database@<digest>, which nothing serves.
// The restart's last resort (img hold, once get_images failed) writes that
// into the compose file, so the run step's up cannot pull it and the
// previous release does not come back. Before the digest moved, the hold
// was mariadb@<digest>, which the run step pulls.
func TestLastHoldDigestInBuiltRepository(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dev, d, r1, failed := syncedAndFailed(t, &out)
	if err := dev.e.set.LastHold(dev.e.rel(r1), dev.e.rel(failed)); err != nil {
		dev.fail("img hold: %v", err)
	}
	imgs, _, _ := composeFacts(dev.e.rel(r1))
	if img := imgs["database"]; img != "" {
		if err := d.Pull(img); err != nil {
			t.Errorf("the restart's up cannot get what the last resort held the service on, %s: %v\n%s", img, err, out.String())
		}
	}
}

// Bug: the same in the restart's last try when the failed release's own
// image is gone too (allowedHere finds nothing): "held on it anyway, for the
// run step to pull", on a reference no registry serves.
func TestAsBuiltDigestInBuiltRepository(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dev, d, r1, failed := syncedAndFailed(t, &out)
	gone, _ := d.ID("app-dev-database")
	for n, id := range d.names {
		if id == gone {
			delete(d.names, n)
		}
	}
	delete(d.images, gone)
	dev.e.set.Restart, dev.e.set.AsBuilt = true, true
	if err := dev.e.set.Restore(dev.e.rel(r1), dev.e.rel(failed), false); err != nil {
		dev.fail("img restore --restart --as-built: %v", err)
	}
	imgs, _, _ := composeFacts(dev.e.rel(r1))
	if img := imgs["database"]; img != "" {
		if err := d.Pull(img); err != nil {
			t.Errorf("the restart's up cannot get what its last try held the service on, %s: %v\n%s", img, err, out.String())
		}
	}
}
