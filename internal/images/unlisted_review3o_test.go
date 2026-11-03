package images

import (
	"bytes"
	"errors"
	"testing"
)

// Bug: Land walks the release's listing, and a listing has the services
// compose gives without profiles, while a copy writes every profile's
// (cli.SyncServices, --profile '*'). A parent whose profile is on sends the
// window of a service whose profile is off on the child: its files land, and
// its history does not. Turned on later, the child's first release adopts its
// label, however low: 11.8 runs on the parent's 12.1 data with no refusal, no
// --accept-image, where the README says the receiving side's history becomes
// the source's for every service whose files the copy writes.
func TestLandingOutsideTheListingKeepsNoWindow(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	prod := newSim(t, dp, "app-prod", &out)
	dp.publish("mariadb:12.1", "J1")
	if _, err := prod.release(false, dbSvc("mariadb:12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev := newSim(t, d, "app-dev", &out)
	d.publish("caddy:2-alpine", "C1")
	if _, err := dev.release(false, proxySvc); err != nil {
		dev.fail("dev: %v", err)
	}
	rel := dev.e.rel(dev.cur)
	l := dev.e.set.SyncLanding(data, "vm1:prod")
	if err := dev.e.set.CheckLanding(rel, l); err != nil {
		dev.fail("the sync: %v", err)
	}
	if err := dev.e.set.Land(dev.base(), rel, l); err != nil {
		dev.fail("land: %v", err)
	}
	// Its profile turned on, at a version below the data the sync wrote.
	d.publish("mariadb:11.8", "I1")
	if _, err := dev.release(false, proxySvc, db118); !errors.Is(err, ErrRefused) {
		t.Errorf("a release runs 11.8 on the parent's 12.1 data the sync wrote, unrefused (%v), its history %v:\n%s", err, dev.history("database"), out.String())
	}
}

// Bug, the same walk the other way: a reset's wipe empties every profile's
// data (down --volumes --profile '*', every entry of mnt), and ResetLanding
// says every history starts over "the ones the copy writes nothing for
// included", but only the listing's do. A service whose profile is off keeps
// the window of data that no longer exists: turned on later, its label is
// refused for it, the very thing that comment fixed.
func TestResetOutsideTheListingKeepsItsHistory(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-dev", &out)
	d.publish("mariadb:12.1", "J1")
	d.publish("caddy:2-alpine", "C1")
	if _, err := m.release(false, proxySvc, dbSvc("mariadb:12.1")); err != nil {
		m.fail("with its profile on: %v", err)
	}
	if _, err := m.release(false, proxySvc); err != nil {
		m.fail("with its profile off: %v", err)
	}
	if err := m.e.set.Land(m.base(), m.e.rel(m.cur), m.e.set.ResetLanding()); err != nil {
		m.fail("the wipe: %v", err)
	}
	d.publish("mariadb:11.8", "I1")
	if _, err := m.release(false, proxySvc, db118); err != nil {
		t.Errorf("its data was wiped and no copy wrote any, and a release is refused against it (%v), its history %v:\n%s", err, m.history("database"), out.String())
	}
}
