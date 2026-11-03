package images

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// Bug: a refused rollback names the newest release snapshot whose data
// allows it, among every directory with a manifest (snapshotFix), a take
// cut off before its rename included: a mirror's take writes its manifest
// before its tars, and only a renamed directory is a snapshot. Copied, the
// line's id is no snapshot (<id>.tmp, whose name also sorts after <id>).
func TestRollbackFixSkipsUnfinishedSnapshot(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	d.publish("mariadb:11.8", "I1")
	r0, _ := m.release(false, db118)
	releaseSnapshot(t, m, "20261102000001", r0)
	releaseSnapshot(t, m, "20261102000002.tmp", r0)
	d.publish("mariadb:12.1", "J1")
	if _, err := m.release(false, dbSvc("mariadb:12.1")); err != nil {
		m.fail("setup: %v", err)
	}
	out.Reset()
	if err := m.rollback(r0, false); !errors.Is(err, ErrRefused) {
		m.fail("the rollback past the upgrade: %v", err)
	}
	if strings.Contains(out.String(), ".tmp") || !strings.Contains(out.String(), "--snapshots restore 20261102000001") {
		t.Errorf("the fix names a take that never completed:\n%s", out.String())
	}
}
