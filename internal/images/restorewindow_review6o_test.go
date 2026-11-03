package images

import (
	"bytes"
	"testing"
)

// Bug: the fix for a volume nothing ever made stops at a copy (parentData's
// non-all branch): a snapshot's windows still carry, for a service whose
// volume did not exist at the take, its label's line. The take writes no tar
// of it (readSnapshotData), the restore extracts none and leaves the volume as
// it is, and yet Land replaces its window with that line. Here search was
// never up when the snapshot was taken (label 8), then ran at 9, upgrading its
// volume; a full restore leaves that volume alone, forgets 9, and search:8
// runs on it, unheld and unsaid.
func TestRestoreNeverMadeVolumeKeepsWindow(t *testing.T) {
	t.Parallel()
	search := func(ref, version string) Service {
		return Service{Name: "search", Ref: ref, Data: true, Method: CopyVolumes, DataVersion: version, EntriesKnown: true, Volumes: []string{"search-data"}}
	}
	var out bytes.Buffer
	d := newFake()
	d.publish("mariadb:11.8", "M1")
	d.publish("search:8", "S8")
	d.publish("search:9", "S9")
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = m.base() + "/snapshots"
	m.e.set.ListAll = func(string) ([]Service, error) { return []Service{dbSvc("mariadb:11.8"), search("search:8", "8")}, nil }
	if _, err := m.release(false, dbSvc("mariadb:11.8")); err != nil {
		m.fail("release: %v", err)
	}
	snap := m.snapshot("20261017000001")
	m.e.set.ListAll = nil
	// Its profile turned on, its label raised.
	if _, err := m.release(false, dbSvc("mariadb:11.8"), search("search:9", "9")); err != nil {
		m.fail("release: %v", err)
	}
	// The snapshot's code, the profile's variable still on: search at 8.
	if _, err := m.fullRestore(snap, []Service{dbSvc("mariadb:11.8"), search("search:8", "8")}); err != nil {
		m.fail("the full restore: %v", err)
	}
	if got := m.runs("search"); got != "S9" {
		t.Errorf("search runs %s on a volume 9 upgraded, which the snapshot does not hold: window %+v\n%s", got, readHistory(m.base()).window("search"), out.String())
	}
}
