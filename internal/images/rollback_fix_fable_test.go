package images

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releaseSnapshot writes a release snapshot's manifest at id, taken by
// the activation that replaced rel (its data as it was then).
func releaseSnapshot(t *testing.T, m *sim, id, rel string) {
	t.Helper()
	dir := filepath.Join(m.e.set.Snapshots, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, SnapshotManifest), []byte("format\t1\nkind\trelease\nrelease\t"+rel+"\ntaken\t2026-11-02T00:00:00Z\ncopy\tnone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A refused rollback names the release snapshot from before the data's
// change, so that the operator restores that data, not older data. The
// fix takes the newest release snapshot whose manifest names the
// rollback's target (snapshotFix): right when the data upgrade is the
// current release (its activation took the target's data). But when the
// upgrade failed and the restart holds the previous release on it, the
// rollback's target is one release further back, and the fix names the
// snapshot of that older release's data, taken when the held release
// replaced it: everything written between those two releases is lost by
// the restore it tells the operator to run. The data from before the
// change is the current (held) release's, in the snapshot the failed
// release took.
func TestRollbackRefusalNamesSnapshotBeforeChange(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	d.publish("mariadb:11.8", "I1")
	r0, _ := m.release(false, db118)
	r1, _ := m.release(false, db118)
	// r1's activation snapshotted r0's data; the data written while r1
	// ran is in no snapshot yet.
	releaseSnapshot(t, m, "20261102000001", r0)
	d.publish("mariadb:12.1", "J1")
	r2, _ := m.failRun(false, dbSvc("mariadb:12.1"))
	// r2's activation snapshotted r1's data, from before the upgrade.
	releaseSnapshot(t, m, "20261102000002", r1)
	if m.cur != r1 || m.runs("database") != "J1" || heldIn(m.e.rel(r1))["database"] == "" {
		m.fail("setup: after %s failed, current %s runs %s, held %v", r2, m.cur, m.runs("database"), heldIn(m.e.rel(r1)))
	}
	out.Reset()
	if err := m.rollback(r0, false); !errors.Is(err, ErrRefused) {
		m.fail("the rollback to %s past the upgrade: %v", r0, err)
	}
	if !strings.Contains(out.String(), "--snapshots restore 20261102000002") || strings.Contains(out.String(), "--snapshots restore 20261102000001") {
		t.Errorf("the refusal names the snapshot of %s's data, older than the data from before the change (%s's, in 20261102000002):\n%s", r0, r1, out.String())
	}
}
