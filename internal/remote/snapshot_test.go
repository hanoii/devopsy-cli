package remote

import (
	"os"
	"os/exec"
	"path/filepath"
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

// A full restore is a rollback's activation: its fetch restores the
// restored release's own images, moving the environment's build names
// (app-prod-app) to them before the switch. Its data step can still
// refuse or fail after that (a tar no service of the restored code has,
// a stop that fails, an extraction cut), and the release does not go
// live: the live release's own names must come back, as they do for
// every activation that fails after its fetch (img undo), or the next
// plain up, restart or --no-code restore runs the snapshot's build
// under the current release's code.
func TestRestoreDataFailureUndoesImages(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), `#!/bin/sh
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\nprotocol=8\n' "$FAKE_ROOT"; exit; fi
id=$(pwd -P); id=${id%/code}; id=${id##*/}
mkdir -p "$FAKE_ROOT/images"
case "$1 ${2:-}" in
  "--release-id "*) echo 9; exit 0 ;;
  "--snapshot-step make-release")
    cp -a "$FAKE_SNAP_REL" "$4" && rm -f "$4/`+FailedFile+`" && : >"$4/`+PendingFile+`"; exit ;;
  "--snapshot-step restore-data") exit "${FAIL_RESTORE_DATA:-0}" ;;
esac
case "$*" in
  "-- config --format json") printf '%s' '`+cfgJSON("app-prod-app", "mariadb:11")+`' ;;
  "-- --profile * pull --ignore-buildable") echo "$id" > "$FAKE_ROOT/images/mariadb:11" ;;
  "-- build --pull") echo "$id" > "$FAKE_ROOT/images/app-prod-app" ;;
esac
exit 0
`)
	fakeImageTools(t, bin)
	root := t.TempDir()
	tg := &Target{Name: "prod", Path: "app/prod", Mode: ModeBuild}
	base := filepath.Join(root, tg.Path)
	run := func(script string, env ...string) (string, error) {
		t.Helper()
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root, "FAKE_SNAP_REL=" + filepath.Join(base, "releases", "1")}, env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, id := range []string{"1", "2"} {
		write(t, filepath.Join(base, "releases", id, CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		if out, err := run(ActivateScript(tg, id, false, "app-prod", Phases{Run: []string{"deploy"}}, nil)); err != nil {
			t.Fatalf("release %s (%v):\n%s", id, err, out)
		}
	}
	if got := fakeImage(root, "app-prod-app"); got != "2" {
		t.Fatalf("app-prod-app after release 2: %q", got)
	}
	// The snapshot, taken in release 1.
	write(t, filepath.Join(base, "snapshots", "20261025000000", SnapshotManifest), "format\t1\nkind\tmanual\nrelease\t1\n")
	out, err := run(RestoreScript(tg, "app-prod", &RestoreOptions{Snapshot: "20261025000000"}), "FAIL_RESTORE_DATA=1")
	if l, _ := os.Readlink(filepath.Join(base, "current")); err == nil || l != "releases/2/code" {
		t.Fatalf("a restore whose data step fails (%v), current %q:\n%s", err, l, out)
	}
	if got := fakeImage(root, "app-prod-app"); got != "2" {
		t.Errorf("after a restore that did not go live, app-prod-app holds %q's image, not the live release 2's:\n%s", got, out)
	}
}
