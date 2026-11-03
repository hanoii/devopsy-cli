package images

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Bug: a restore lands one line of no version for each service its manifest
// names and its windows lack (RestoreLanding's over), and since Land also
// takes what the listing leaves out, that now includes a service in a profile
// that is off: the manifest counts every profile (cli.SyncServices), the
// windows only the ones compose lists (SnapshotData, by Set.List). A snapshot
// of an environment with database's profile off, restored, wipes database's
// real window though its data comes back as it was: with the profile on
// again, 11.8 runs on data 12.1 upgraded, unrefused.
func TestRestoreKeepsTheWindowOfAProfileOff(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	d.publish("mariadb:12.1", "J1")
	d.publish("caddy:2-alpine", "C1")
	if _, err := m.release(false, proxySvc, dbSvc("mariadb:12.1")); err != nil {
		m.fail("with its profile on: %v", err)
	}
	if _, err := m.release(false, proxySvc); err != nil {
		m.fail("with its profile off: %v", err)
	}
	dir := m.snapshot("20260101000000")
	data, err := os.ReadFile(filepath.Join(dir, "data", "images"))
	if err != nil {
		t.Fatal(err)
	}
	// As snapshotLanding gives it: every service with data its manifest names.
	l := m.e.set.RestoreLanding(string(data), filepath.Base(dir), []string{"database"})
	rel := m.e.rel(m.cur)
	if err := m.e.set.CheckLanding(rel, l); err != nil {
		m.fail("the restore's check: %v", err)
	}
	if err := m.e.set.Land(m.base(), rel, l); err != nil {
		m.fail("the data step: %v", err)
	}
	d.publish("mariadb:11.8", "I1")
	if _, err := m.release(false, proxySvc, db118); !errors.Is(err, ErrRefused) {
		t.Errorf("the restored data is 12.1's, and a release runs 11.8 on it unrefused (%v), its history %v:\n%s", err, m.history("database"), out.String())
	}
}
