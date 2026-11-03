package images

import (
	"bytes"
	"testing"
)

// Bug: an import records what runs on the data once it lands (RanCut, as
// after a run step), reading a container that "once started" counts: an
// exited one too. A service down on purpose while its data is imported stays
// down (restoreServices starts only what the import stopped), and nothing
// ran on the new data; the history says its image did. Here 11.8 data is
// imported under a stopped 12.1: its history ends on a 12.1 that never ran on
// it, so an --export carries 12.1, and the release at 11.8 that matches the
// data is refused as lower than the data's version. The next activation's
// ran cut writes the same line, so the fix is not only here.
func TestImportDownServiceRecordsNoRun(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:12.1", "J1")
	if _, err := m.release(false, dbSvc("mariadb:12.1")); err != nil {
		m.fail("setup: %v", err)
	}
	for i := range d.containers {
		if d.containers[i].project == "app-prod" {
			d.containers[i].state = "exited"
		}
	}
	data := otherAt(t, &out, "app-other", "mariadb:11.8")
	rel := m.e.rel(m.cur)
	l := m.e.set.ImportLanding(data, "database")
	if err := m.e.set.CheckLanding(rel, l); err != nil {
		m.fail("the import of 11.8 under 12.1: %v", err)
	}
	if err := m.e.set.Land(m.base(), rel, l); err != nil {
		m.fail("land: %v", err)
	}
	if err := m.e.set.RanCut(m.base(), rel); err != nil {
		m.fail("ran: %v", err)
	}
	if v := windowVersion(readHistory(m.base()).window("database")); v != "11.8" {
		t.Errorf("the data's version is %s, its history %v: nothing ran on the imported 11.8 data, its container exited before it landed", v, m.history("database"))
	}
}
