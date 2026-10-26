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
