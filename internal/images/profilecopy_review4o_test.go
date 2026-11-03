package images

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// Bug: a copy writes every profile's data (cli.SyncDirs over cli.SyncServices,
// --profile '*'), and ParentData sends only the windows of what Set.List gives,
// the profiles that are on. With database's profile off on the parent, its
// files land on the child and its window does not, so the child's check judges
// nothing and its history stays: 11.8 runs on the parent's 12.1 data. It is
// also the chain: a window Land kept for a profile that is off (for the
// release that turns it on) goes no further than that environment. And
// an --export of such a service carries no record, so its import takes the
// next label, whatever it is.
func TestCopiedDataOfAProfileOffCarriesItsWindow(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	prod := newSim(t, dp, "app-prod", &out)
	dp.publish("mariadb:12.1", "J1")
	dp.publish("caddy:2-alpine", "C1")
	if _, err := prod.release(false, proxySvc, dbSvc("mariadb:12.1")); err != nil {
		prod.fail("with its profile on: %v", err)
	}
	if _, err := prod.release(false, proxySvc); err != nil {
		prod.fail("with its profile off: %v", err)
	}
	dev := newSim(t, d, "app-dev", &out)
	d.publish("mariadb:11.8", "I1")
	if _, err := dev.release(false, db118); err != nil {
		dev.fail("dev: %v", err)
	}
	if err := syncInto(t, prod, dev, false); !errors.Is(err, ErrRefused) {
		t.Errorf("the copy writes the parent's 12.1 data, and the child's 11.8 is not refused (%v), its history %v:\n%s", err, dev.history("database"), out.String())
	}
	rel := prod.e.rel(prod.cur)
	if rec := prod.e.set.ExportData(prod.base(), rel, "database"); !strings.Contains(rec, "12.1") {
		t.Errorf("the export of its data records nothing of 12.1: %q", rec)
	}
}
