package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every script that reads a snapshot's directory holds the snapshot lock
// (fd 6) while it does: a full restore reads the snapshot's tars and then
// writes its mark naming it, while a take's pruning and --snapshots remove
// remove snapshots under that lock alone, with the release lock gone, so
// without it the snapshot being restored can go halfway through.
//
// Held to the data step, with release snapshots on too: their setup
// reopening fd 6, or giving it up when it takes none (a marked
// environment, nothing with data), would let a removal or a take's
// pruning in before the restore's mark names the snapshot.
func TestRestoreScriptsTakeSnapshotLock(t *testing.T) {
	t.Parallel()
	tg := &Target{Name: "prod", Host: "h", Path: "app/prod", Mode: ModeImage}
	on := &Target{Name: "prod", Host: "h", Path: "app/prod", Mode: ModeImage, SnapshotsRelease: SnapshotSetting{Enabled: true, Keep: 1}}
	for name, s := range map[string]string{
		"restore":                         RestoreScript(tg, "app-prod", &RestoreOptions{Snapshot: "20261025120000"}),
		"restore data":                    RestoreDataScript(tg, "app-prod", &RestoreOptions{Snapshot: "20261025120000"}),
		"restore, release snapshots":      RestoreScript(on, "app-prod", &RestoreOptions{Snapshot: "20261025120000"}),
		"restore data, release snapshots": RestoreDataScript(on, "app-prod", &RestoreOptions{Snapshot: "20261025120000"}),
	} {
		lock := strings.Index(s, `exec 6>"$base/`+SnapshotLockFile+`"`)
		check := strings.Index(s, quoteAll([]string{SnapshotStepFlag, "check-tars"}))
		if lock < 0 || check < 0 || lock > check {
			t.Errorf("%s: the snapshot lock (at %d) is not held before the snapshot's tars are read (at %d)", name, lock, check)
		}
		data := strings.Index(s, quoteAll([]string{SnapshotStepFlag, "restore-data"}))
		if data < 0 || lock < 0 {
			continue
		}
		for _, give := range []string{"exec 6>", "exec 6>&-"} {
			if i := strings.Index(s[lock+1:data], give); i >= 0 {
				t.Errorf("%s: the snapshot lock given up or reopened (%q at %d) before the data step (at %d)", name, give, lock+1+i, data)
				break
			}
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

// A --no-code restore lets go of the sync and snapshot locks once its
// data is in, before the deployed run step, which may take minutes: an
// export of a snapshot, a removal or a take's first pass need not wait
// for it. Nothing it started meanwhile may keep them: the tail that
// follows its output to the session (followOutput) runs to the script's
// end.
func TestRestoreDataReleasesLocksForRunStep(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock")
	}
	if err := exec.Command("tail", "--version").Run(); err != nil {
		t.Skip("no GNU tail: followOutput does not tail")
	}
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), `#!/bin/sh
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\nprotocol=8\n' "$FAKE_ROOT"; exit; fi
case "$*" in
  "--release-steps release run")
    for l in .devopsy-snapshot.lock .devopsy-sync.lock; do
      if flock -n "$FAKE_BASE/$l" true; then echo "$l free"; else echo "$l held"; fi >>"$FAKE_ROOT/locks"
    done ;;
esac
exit 0
`)
	if err := os.Chmod(filepath.Join(bin, "devopsy"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	tg := &Target{Name: "prod", Path: "app/prod", Mode: ModeImage}
	base := filepath.Join(root, tg.Path)
	write(t, filepath.Join(base, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	if err := os.Symlink("releases/1/"+CodeDir, filepath.Join(base, "current")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(base, "snapshots", "20261025000000", SnapshotManifest), "format\t1\nkind\tmanual\n")
	cmd := exec.Command("sh", "-c", RestoreDataScript(tg, "app-prod", &RestoreOptions{Snapshot: "20261025000000"}))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root, "FAKE_BASE=" + base}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("restore (%v):\n%s", err, out)
	}
	if got := readFile(filepath.Join(root, "locks")); got != ".devopsy-snapshot.lock free\n.devopsy-sync.lock free\n" {
		t.Errorf("during the run step, after the script let them go:\n%s", got)
	}
}

// With release snapshots on, a release and a restore (full or
// --no-code) take one of the data they replace, never a rollback, and
// need a server with release snapshots (release-snapshots), which
// nothing without them does.
func TestReleaseSnapshotScripts(t *testing.T) {
	t.Parallel()
	on := &Target{Name: "prod", Host: "h", Path: "app/prod", Mode: ModeImage, SnapshotsRelease: SnapshotSetting{Enabled: true, Keep: 1}}
	off := &Target{Name: "prod", Host: "h", Path: "app/prod", Mode: ModeImage}
	phases := Phases{Run: []string{"deploy"}}
	r := &RestoreOptions{Snapshot: "20261025120000"}
	for name, s := range map[string]string{
		"release":   ActivateScript(on, "20261025120000", false, "app-prod", phases, nil),
		"restore":   RestoreScript(on, "app-prod", r),
		"--no-code": RestoreDataScript(on, "app-prod", r),
	} {
		if !slices.Contains(Needs(s), ReleaseSnapshotsFeature) {
			t.Errorf("%s with release snapshots needs %v", name, Needs(s))
		}
	}
	for name, s := range map[string]string{
		"off":           ActivateScript(off, "20261025120000", false, "app-prod", phases, nil),
		"rollback":      ActivateScript(on, "", true, "app-prod", Phases{}, nil),
		"restore off":   RestoreScript(off, "app-prod", r),
		"--no-code off": RestoreDataScript(off, "app-prod", r),
	} {
		if slices.Contains(Needs(s), ReleaseSnapshotsFeature) || strings.Contains(s, "release-check") {
			t.Errorf("%s takes a release snapshot", name)
		}
	}
}

// A sync's done line completes it, but over a restore's mark only one a
// parent that checked the mark wrote (CheckedDoneLine): an older parent,
// whose done line is plain, leaves the copy partial and the mark there.
func TestSyncDecisionRestoreMark(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		mark, done, want string
	}{
		{"", DoneLine("7.1"), "go"},
		{"", CheckedDoneLine("7.1"), "go"},
		{IncompleteImport, DoneLine("7.1"), "go"},
		{IncompleteRestore, CheckedDoneLine("7.1"), "go"},
		{IncompleteRestore, DoneLine("7.1"), "partial"},
	} {
		dir := t.TempDir()
		taken, incomplete := filepath.Join(dir, "taken"), filepath.Join(dir, "incomplete")
		if err := os.WriteFile(taken, []byte("writer db 1 up\n"+c.done+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if c.mark != "" {
			if err := os.WriteFile(incomplete, []byte(IncompleteLine("2026-10-26 00:00:00", "20261026000000", c.mark, "why")), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		out, err := exec.Command("sh", "-c", "sync_id=7.1; reply=go\n"+syncDecision(Quote(taken), Quote(incomplete))+"echo \"$sync_word\"").CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != c.want {
			t.Errorf("mark %q, %q: %s (%v), want %s", c.mark, c.done, out, err, c.want)
		}
	}

	// The partial copy over the restore's mark leaves the mark a
	// restore's: its data is as partial as the restore left it, and only
	// a restore or a sync writing every service of the snapshot clears
	// it. Written over as a sync's copy mark, the next plain done line
	// (the same older parent) would clear it, the snapshot's services
	// the parent does not copy started on half-restored data.
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, SyncTakenFile), []byte("writer db 1 up\n"+DoneLine("7.1")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mark := filepath.Join(base, SyncIncompleteFile)
	if err := os.WriteFile(mark, []byte(IncompleteLine("2026-10-26 00:00:00", "20261026000000", IncompleteRestore, "why")), 0o644); err != nil {
		t.Fatal(err)
	}
	block := syncBlock("app-dev", "the sync fails", false)
	i := strings.Index(block, "*.partial)")
	j := strings.Index(block[i:], ";;")
	if i < 0 || j < 0 {
		t.Fatalf("no *.partial) branch in syncBlock:\n%s", block)
	}
	script := "base=" + Quote(base) + "\nsync_id=7.1; reply=go\n" + syncDecision(`"$base/`+SyncTakenFile+`"`, `"$base/`+SyncIncompleteFile+`"`) + "case \"$reply.$sync_word\" in\n" + block[i:i+j] + ";;\nesac\n"
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{fakeDevopsy(t), SyncFromTargetEnv + "=vm1:prod"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the partial branch: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(mark)
	if m := ParseMark(string(data)); !m.IsRestore() || m.From != "20261026000000" {
		t.Errorf("an older parent's complete copy over a restore's mark wrote it over: %q\n%s", data, out)
	}
}
