package images

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Bug: a copy's check minds only the built images its landing will need
// (copyNeeds), never a pulled one. A --no-code restore of a snapshot whose
// data's window ends on a pulled image passes the check while that image is
// gone from the server and the registry no longer serves it (an imported
// snapshot, whose images are bookmarked only where the server has them),
// then its data step lands the data and derive fails pulling the hold: the
// services stay down, marked, for a condition the check could have refused
// before anything stopped ("Check everything before changing anything").
// Here: the environment held on J2 (12.1) under an 11.8 release, the
// snapshot's 12.1 data having last run J1, which is gone everywhere.
func TestNoCodeCheckRefusesHeldPullBeforeStop(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	d.publish("mariadb:11.8", "I1")
	if _, err := m.release(false, db118); err != nil {
		m.fail("11.8: %v", err)
	}
	d.publish("mariadb:12.1", "J1")
	if _, err := m.failRun(false, dbSvc("mariadb:12.1")); err != nil {
		m.fail("failed 12.1: %v", err)
	}
	if heldIn(m.e.rel(m.cur))["database"] == "" {
		m.fail("setup: not held on J1")
	}
	snap := m.snapshot("20261017000001")
	d.publish("mariadb:12.1", "J2")
	if _, err := m.release(false, dbSvc("mariadb:12.1")); err != nil {
		m.fail("fix forward: %v", err)
	}
	if _, err := m.release(true, db118); err != nil {
		m.fail("accepted 11.8: %v", err)
	}
	if _, err := m.failRun(false, dbSvc("mariadb:12.1")); err != nil {
		m.fail("failed 12.1 again: %v", err)
	}
	if heldIn(m.e.rel(m.cur))["database"] == "" || m.runs("database") != "J2" {
		m.fail("setup: held %v, runs %s", heldIn(m.e.rel(m.cur)), m.runs("database"))
	}
	// J1, the image the snapshot's data ran last, goes from the server and
	// from the registry (as for a snapshot imported from another server).
	for n, id := range d.names {
		if id == "J1" {
			delete(d.names, n)
		}
	}
	delete(d.images, "J1")
	delete(d.registry, "J1")
	rel := m.e.rel(m.cur)
	raw, err := os.ReadFile(filepath.Join(snap, "data", "images"))
	if err != nil {
		t.Fatal(err)
	}
	data := string(raw)
	out.Reset()
	err = m.e.set.CheckLanding(rel, m.e.set.RestoreLanding(data, filepath.Base(snap), nil))
	if !errors.Is(err, ErrRefused) {
		land := m.e.set.Land(m.base(), rel, m.e.set.RestoreLanding(data, "", nil))
		t.Fatalf("the check let the --no-code restore through (%v), and its data step then failed (%v): the data is in, the services stay down, marked, for an image the check could have found gone before anything stopped\n%s", err, land, out.String())
	}
}
