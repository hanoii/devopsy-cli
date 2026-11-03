package images

import (
	"bytes"
	"errors"
	"testing"
)

// Bug: a data service whose only data is a named volume counts as written by
// a copy (parentData: "a named volume counts as there"), but the push skips a
// volume the parent's server does not have (syncData). So a parent's service
// in a profile that was never up writes nothing into the child, and yet
// parentData, which now goes by every profile and sends a label's line when
// there is no history, replaces the child's window with that label. Here the
// child ran search 9 on its data; the parent says 8 of a service that never
// ran there; the sync writes nothing of it, and the child's data, still
// upgraded by 9, now reads as 8: a release of 8 goes through, unrefused.
func TestParentDataNeverMadeVolumeKeepsWindow(t *testing.T) {
	t.Parallel()
	search := func(ref, version string) Service {
		return Service{Name: "search", Ref: ref, Data: true, Method: CopyVolumes, DataVersion: version, EntriesKnown: true, Volumes: []string{"search-data"}}
	}
	d := newFake()
	var out bytes.Buffer
	d.publish("search:8", "S8")
	d.publish("search:9", "S9")
	d.publish("mariadb:11.8", "M1")
	prod := newSim(t, d, "app-prod", &out)
	// Its profile is off: every profile's list has it, the release's has not.
	prod.e.set.ListAll = func(string) ([]Service, error) { return []Service{dbSvc("mariadb:11.8"), search("search:8", "8")}, nil }
	if _, err := prod.release(false, dbSvc("mariadb:11.8")); err != nil {
		prod.fail("prod: %v", err)
	}
	dev := newSim(t, d, "app-dev", &out)
	if _, err := dev.release(false, dbSvc("mariadb:11.8"), search("search:9", "9")); err != nil {
		dev.fail("dev: %v", err)
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	rel := dev.e.rel(dev.cur)
	if err := dev.e.set.CheckLanding(rel, dev.e.set.SyncLanding(data, "")); err != nil {
		dev.fail("sync-check: %v", err)
	}
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	id := dev.e.release(dbSvc("mariadb:11.8"), search("search:8", "8"))
	if err := dev.e.set.Check(dev.e.rel(id)); !errors.Is(err, ErrRefused) {
		t.Errorf("search 8 on data search 9 upgraded, the sync having written none of it: %v\nwindow %+v\n%s", err, readHistory(dev.base()).window("search"), out.String())
	}
}
