package remote

import (
	"strings"
	"testing"
)

// Every script that reads a snapshot's directory holds the snapshot lock
// (fd 6) while it does: a full restore reads the snapshot's tars and then
// writes its mark naming it, while a take's pruning and --snapshots remove
// remove snapshots under that lock alone, with the release lock gone, so
// without it the snapshot being restored can go halfway through.
func TestRestoreScriptsTakeSnapshotLock(t *testing.T) {
	t.Parallel()
	tg := &Target{Name: "prod", Host: "h", Path: "app/prod", Mode: ModeImage}
	for name, s := range map[string]string{
		"restore":      RestoreScript(tg, "app-prod", &RestoreOptions{Snapshot: "20261025120000"}),
		"restore data": RestoreDataScript(tg, "app-prod", &RestoreOptions{Snapshot: "20261025120000"}),
	} {
		lock := strings.Index(s, `exec 6>"$base/`+SnapshotLockFile+`"`)
		check := strings.Index(s, quoteAll([]string{SnapshotStepFlag, "check-tars"}))
		if lock < 0 || check < 0 || lock > check {
			t.Errorf("%s: the snapshot lock (at %d) is not held before the snapshot's tars are read (at %d)", name, lock, check)
		}
	}
}

// Whatever holds the snapshot lock but a take removes a take's holding
// file first (one a take killed outright left), so --status, which
// reads the file with the lock held as a take running, never says a
// snapshot is taken while a removal, an export or a restore holds it.
func TestSnapshotLockHolders(t *testing.T) {
	t.Parallel()
	tg := &Target{Name: "prod", Host: "h", Path: "app/prod", Mode: ModeImage}
	r := &RestoreOptions{Snapshot: "20261025120000"}
	clear := `rm -f "$base/` + SnapshotHoldingFile + `"`
	for name, s := range map[string]string{
		"remove":       SnapshotsRemoveScript(tg, "app-prod", []string{"20261025120000"}),
		"restore":      RestoreScript(tg, "app-prod", r),
		"restore data": RestoreDataScript(tg, "app-prod", r),
		"export":       SnapshotExportScript(tg, "20261025120000"),
		"import":       SnapshotImportScript(tg, "app-prod"),
		"destroy":      DestroyScript(tg, "app-prod"),
	} {
		lock := strings.Index(s, `exec 6>"$base/`+SnapshotLockFile+`"`)
		if lock < 0 || !strings.Contains(s[lock:], clear) {
			t.Errorf("%s: takes the snapshot lock (at %d) and leaves a take's holding file", name, lock)
		}
	}
	if s := SnapshotScript(tg, "app-prod", 3); strings.Contains(s, clear) {
		t.Error("a take removes its own holding file")
	}
}
