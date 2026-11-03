package images

import (
	"bytes"
	"testing"
)

// otherAt is the window another environment's export or sync of database
// carries, which ran ref.
func otherAt(t *testing.T, out *bytes.Buffer, name, ref string) string {
	t.Helper()
	d := newFake()
	other := newSim(t, d, name, out)
	d.publish(ref, "other-"+ref)
	if _, err := other.release(false, dbSvc(ref)); err != nil {
		other.fail("the other environment: %v", err)
	}
	return other.e.set.ParentData(other.base(), other.e.rel(other.cur))
}

// Bug: derive keeps a held service its compose no longer gives (a profile
// turned off), "nothing runs it to derive for". But that branch is reached
// only when the rule allows its release's own image: one it refuses is in
// held, a few lines above. So what it keeps is exactly the hold the data no
// longer needs. Here database is held on 12.1, its profile goes off, 11.8
// data lands on it (an import names any profile's service), its own image
// allowed; the profile comes back on, ReloadCheck passes and a reload runs
// the held 12.1 on the 11.8 data, upgrading it, where "a hold goes as soon
// as the data allows the release's own image again, and nothing else keeps
// one" (README).
func TestStaleHoldOnServiceComposeLacks(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	r1, err := m.release(false, db118)
	if err != nil {
		m.fail("r1: %v", err)
	}
	d.publish("mariadb:12.1", "J1")
	if _, err := m.failRun(false, dbSvc("mariadb:12.1")); err != nil {
		m.fail("r2: %v", err)
	}
	if m.cur != r1 || m.runs("database") != "J1" {
		m.fail("setup: current %s runs %s", m.cur, m.runs("database"))
	}
	data := otherAt(t, &out, "app-other", "mariadb:11.8")
	rel := m.e.rel(r1)
	// Its profile off: compose config no longer lists it.
	m.e.svcs[r1] = []Service{proxySvc}
	l := m.e.set.ImportLanding(data, "database")
	if err := m.e.set.CheckLanding(rel, l); err != nil {
		m.fail("the import of 11.8 data, its own 11.8 allowed: %v", err)
	}
	if err := m.e.set.Land(m.base(), rel, l); err != nil {
		m.fail("land: %v", err)
	}
	// Its profile back on.
	m.e.svcs[r1] = []Service{db118}
	if needs, err := m.e.set.ReloadCheck(rel); err != nil || len(needs) != 0 {
		m.fail("a reload is refused (%v): %v", err, needs)
	}
	m.up()
	if got := m.runs("database"); got != d.names["mariadb:11.8"] {
		t.Errorf("database's data is 11.8 and its own 11.8 is allowed, and it runs %s, not its own %s: the hold on 12.1 stayed (%v) and upgrades the data:\n%s", got, d.names["mariadb:11.8"], heldIn(rel), out.String())
	}
}
