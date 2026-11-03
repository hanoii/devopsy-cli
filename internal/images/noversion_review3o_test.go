package images

import (
	"bytes"
	"testing"
)

// Bug, docs: the README and --import's help say an import without a record,
// and a --resolved that starts its history over, leave data of no known
// version, "which the next release's label is taken for". Since an import and
// --resolved record what ran, that holds only for a service that stays down:
// one that runs is recorded at its own label the moment it lands, so the next
// release is judged against that, and a lower label refused. Either the words
// or the record is wrong.
func TestNoKnownVersionLabelIsTaken(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:12.1", "J1")
	if _, err := m.release(false, dbSvc("mariadb:12.1")); err != nil {
		m.fail("setup: %v", err)
	}
	rel := m.e.rel(m.cur)
	l := m.e.set.ImportLanding("", "database")
	if err := m.e.set.CheckLanding(rel, l); err != nil {
		m.fail("the import of a tar made by hand: %v", err)
	}
	if err := m.e.set.Land(m.base(), rel, l); err != nil {
		m.fail("land: %v", err)
	}
	if err := m.e.set.RanCut(m.base(), rel); err != nil {
		m.fail("ran: %v", err)
	}
	d.publish("mariadb:11.8", "I1")
	if _, err := m.release(false, db118); err != nil {
		t.Errorf("the README: data of no known version, \"which the next release's label is taken for\"; refused (%v), its history %v:\n%s", err, m.history("database"), out.String())
	}
}
