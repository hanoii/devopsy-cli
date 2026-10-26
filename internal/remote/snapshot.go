package remote

import (
	"strconv"
	"strings"
)

// A snapshot is a copy of an environment's data (every service with
// data's entries of mnt and named volumes, as files, whatever its copy
// label), its code and the images that ran on them, taken with its
// services with data stopped for one moment: a directory of its own
// under the environment's snapshots (basePrelude's $snaps), named by
// an id the server picks as it does a release's (ReleaseIDFlag), holding
// one .tar.zst per entry or volume, the manifest, copies of the
// environment's variables and image history, and a copy of the release
// it was taken in (code/ and the files beside it, its images listing,
// whose images it bookmarks as s<id>: images.Set.idOf).
//
// Without a mirror (the default) the tars are written from the stopped
// data, the downtime the time they take. With one (the environment's
// snapshots: {mirror: true}, unless the server's says false), the data
// is copied twice into a mirror that stays, .mirror among the
// environment's snapshots: once while everything runs (most of it,
// inconsistent), once stopped (only what changed since: the downtime);
// the tars are made from the mirror afterwards, nobody waiting. Only a
// directory renamed into place is a snapshot.
const (
	// SnapshotStepFlag is the hidden flag a snapshot's scripts call, in
	// the current release, for each of their steps (runSnapshotStep).
	SnapshotStepFlag = "--snapshot-step"
	// SnapshotLockFile, in $base, is held by whatever writes the mirror
	// or a snapshot's directory: a take's first pass and its tar, its
	// stopped pass too (with the release and sync locks), a removal. One
	// order everywhere: release lock, sync lock, snapshot lock, image
	// lock, none waited for while a later one is held.
	SnapshotLockFile = ".devopsy-snapshot.lock"
	// SnapshotHoldingFile, in $base, says a take holds the snapshot
	// lock: only with the lock busy (flock -n failing), never alone.
	SnapshotHoldingFile = ".devopsy-snapshot.holding"
	// SnapshotMirror is the mirror's directory among the environment's
	// snapshots (under the snapshot root): mnt/ and volumes/, root's. Only
	// an environment with snapshots: {mirror: true} keeps one.
	SnapshotMirror = ".mirror"
	// SnapshotComplete, in the mirror, names the snapshot whose stopped
	// pass completed it: a holder of the snapshot lock finding that
	// snapshot's .tmp finishes its tars from the mirror.
	SnapshotComplete = ".devopsy-complete"
	// SnapshotManifest is a snapshot's manifest (SnapshotFormat first).
	SnapshotManifest = "manifest"
	// SnapshotFormat is the manifest's format: a restore refuses one it
	// does not know, naming the upgrade.
	SnapshotFormat = "1"
	// SnapshotData holds a snapshot's tars, mnt.<entry>.tar.zst and
	// volume.<key>.tar.zst, and its copies of the environment's
	// shared/.env (shared.env), shared/inherit.env (inherit.env) and
	// image history (data-images).
	SnapshotData = "data"
)

// snapshotLock takes the snapshot lock (fd 6) for anything but a take,
// and removes a take's holding file a take killed outright left, so
// --status never says a snapshot is taken while another holds it.
func snapshotLock(stdin bool) string {
	return `exec 6>"$base/` + SnapshotLockFile + `"
` + lockWait(6, stdin, "a snapshot of $base") + `
rm -f "$base/` + SnapshotHoldingFile + `"
`
}

// SnapshotScript takes a manual snapshot of t's current release, keeping
// keep of them (at most the server's snapshot max_keep). One session:
// watched as an activation is (WatchEnv), so before the end of its
// stopped pass a session gone or a signal cancels it, the services
// started again; past it, it finishes on its own, its output in the
// current release's server log (followOutput, SwitchMarker), the
// session following it.
//
// In order: the first pass into the mirror under the snapshot lock
// alone (the release lock dropped, so no release waits minutes); then
// the release lock, the sync lock (fd 5: a push or an export from this
// environment stops the same services) and the snapshot lock again, the
// mark checked, the id, the stopped pass (devopsy stops every service
// with data and its dependents, copies what changed, starts them, and
// writes the snapshot's manifest and code); then the release and sync
// locks go, and the tars are made under the snapshot lock alone.
func SnapshotScript(t *Target, projectName string, keep int) string {
	mode := t.Mode
	if mode == "" {
		mode = ModeBuild
	}
	// A step, its word and then shell words as they are ("$id"). Up to
	// the stopped pass's end each runs in the background, the script
	// waiting: a signal the watcher sends the script alone (it leads no
	// group outside sshd) reaches the step, which starts the services
	// again.
	call := func(word string, shellArgs []string, exec bool) string {
		s := devopsyCall(projectName, []string{SnapshotStepFlag, word}, exec)
		for _, a := range shellArgs {
			s += " " + a
		}
		return s
	}
	// Each step in the release current is when it starts: a release may
	// switch current while the first pass runs, with no lock held.
	step := func(word string, shellArgs ...string) string {
		return `( cd "$base/current" && ` + call(word, shellArgs, true) + ` ) &
sb_pid=$!
trap 'kill -TERM "$sb_pid" 2>/dev/null || true' INT TERM HUP
sb=0
wait "$sb_pid" || sb=$?
while kill -0 "$sb_pid" 2>/dev/null; do sb=0; wait "$sb_pid" || sb=$?; done
trap - INT TERM HUP
[ "$sb" = 0 ] || exit "$sb"`
	}
	snapLock := `exec 6>"$base/` + SnapshotLockFile + `"
` + lockWait(6, false, "another snapshot of $base") + `
: >"$base/` + SnapshotHoldingFile + `"
`
	s := "set -eu\n" + basePrelude(t) + "exec 1>&2\n" + NoExit255
	s += `[ -L "$base/current" ] || { devopsy --say error "$base has no current release: release it first ('devopsy @` + hintAddress(t) + ` --release')"; exit 1; }
`
	s += watchSession(false, true)
	s += "export " + ImagesLeftEnv + "=" + leftPath(t) + " " + ImagesLockEnv + `="$root/` + ImagesLock + `" ` + ImagesModeEnv + "=" + Quote(mode) + `
export ` + snapshotEnv(t, keep) + `
umask 077
mkdir -p "$snaps"
cd "$base/current"
`
	refuse := markRefusal(t, "no snapshot taken", `"`+"a snapshot of it would restore as complete"+`"`, "exit 1")
	s += refuse + snapLock + step("leftovers") + "\n" + step("check") + "\n" + step("mirror") + "\nexec 6>&-\n"
	s += releaseLock(false, false) + `exec 5>"$base/` + SyncLockFile + `"
` + lockWait(5, false, "a sync or an export from $base") + "\n" + settleSync(projectName) + snapLock + refuse
	s += `cur=$(readlink "$base/current"); cur=${cur%/` + CodeDir + `}
id=$(devopsy ` + ReleaseIDFlag + ` "$snaps")
devopsy --say info "taking snapshot $id of ${cur#releases/}"
` + step("stopped", `"$id"`, `"$base/$cur"`) + `
# Past the stopped pass the snapshot is consistent in the mirror: it
# finishes on its own, whatever happens to the session.
trap '' INT TERM HUP
[ -z "${watch_pid:-}" ] || mv "$ws_armed" "$ws_armed.off" 2>/dev/null || true
exec 9>&- 5>&-
out="$base/$cur/` + ServerLogFile + `"
fo_tag="snapshot.$id"
` + followOutput + call("tar", []string{`"$id"`}, false) + "\n"
	return s
}

// SnapshotKeepEnv is how many manual snapshots a take keeps: its
// environment's snapshots: manual keep, which the server's devopsy caps
// by its own max_keep.
const SnapshotKeepEnv = "DEVOPSY_INTERNAL_SNAPSHOT_KEEP"

// snapshotEnv is the variables a take or a release with snapshots
// exports: how many of each kind stay and whether it keeps a mirror.
func snapshotEnv(t *Target, keep int) string {
	return SnapshotKeepEnv + "=" + strconv.Itoa(keep) + " " + SnapshotReleaseKeepEnv + "=" + strconv.Itoa(t.SnapshotsRelease.Keep) + " " + SnapshotMirrorEnv + "=" + map[bool]string{true: "1", false: "0"}[t.SnapshotsMirror]
}

// RestoreSnapshotEnv names a restore's own release snapshot (taken right
// before, in the same session), whose stopped services its data step keeps
// down and owns from its mark.
const RestoreSnapshotEnv = "DEVOPSY_INTERNAL_RESTORE_SNAPSHOT"

// SnapshotReleaseKeepEnv is how many release snapshots a pruning keeps
// (KeepAll: every one whose release is kept), sent by the operator from
// the environment's snapshots: {release: {keep}}.
const SnapshotReleaseKeepEnv = "DEVOPSY_INTERNAL_SNAPSHOT_RELEASE_KEEP"

// SnapshotMirrorEnv is 1 when a take's environment asks for a mirror
// (snapshots: {mirror: true}), sent by the operator as its keep is; the
// server's snapshots: {mirror: false} still wins.
const SnapshotMirrorEnv = "DEVOPSY_INTERNAL_SNAPSHOT_MIRROR"

// IncompleteRestore is the mark's code for a restore that did not
// complete, its From the snapshot's id: that snapshot cannot be removed
// meanwhile, by hand or by keep.
const IncompleteRestore = "restore"

// SnapshotsScript lists t's snapshots (--snapshot-step list), one line
// each, tab separated: id, kind, the release it holds, when it was
// taken, its size in bytes, and its state ("" taken, "in progress",
// "incomplete"). Read-only: no lock.
func SnapshotsScript(t *Target, projectName string) string {
	return "set -eu\n" + basePrelude(t) + `[ -d "$snaps" ] || exit 0
cd "$base/current" 2>/dev/null || cd "$base"
` + devopsyCall(projectName, []string{SnapshotStepFlag, "list"}, true) + "\n"
}

// SnapshotsRemoveScript removes t's snapshots ids, under the snapshot
// lock (waited for, as is the server's image lock): each with its
// bookmarks, the images only it held freed as a release's removal does.
// A snapshot a restore's mark names stays.
func SnapshotsRemoveScript(t *Target, projectName string, ids []string) string {
	mode := t.Mode
	if mode == "" {
		mode = ModeBuild
	}
	return "set -eu\n" + basePrelude(t) + `[ -d "$snaps" ] || { devopsy --say error "$base has no snapshots"; exit 1; }
[ -L "$base/current" ] || { devopsy --say error "$base has no current release"; exit 1; }
export ` + ImagesLeftEnv + "=" + leftPath(t) + " " + ImagesLockEnv + `="$root/` + ImagesLock + `" ` + ImagesModeEnv + "=" + Quote(mode) + `
` + snapshotLock(false) + `cd "$base/current"
` + devopsyCall(projectName, append([]string{SnapshotStepFlag, "remove"}, ids...), true) + "\n"
}

// RestoreDataScript restores the data of t's snapshot r.Snapshot under
// the current release (--no-code): never an activation, with no release
// to make, fail or switch back to. In order: the watcher, the release,
// sync and snapshot locks, the checks (the current labels fit the
// snapshot's manifest, the tars whole), the disarm (a session gone up to
// it cancels, nothing changed), the data step as a full restore's, then
// the deployed run step again, its output in the current release's
// server log, as a sync's.
func RestoreDataScript(t *Target, projectName string, r *RestoreOptions) string {
	mode := t.Mode
	if mode == "" {
		mode = ModeBuild
	}
	step := func(word string, args ...string) string {
		return "(cd \"$base/current\" && " + devopsyCall(projectName, []string{SnapshotStepFlag, word}, false) + " " + strings.Join(args, " ") + ")"
	}
	s := prelude(t, false) + "exec 1>&2\n" + NoExit255
	s += `[ -L "$base/current" ] || { devopsy --say error "$base has no current release: release it first, then restore"; exit 1; }
snap="$snaps"/` + Quote(r.Snapshot) + `
[ -f "$snap/` + SnapshotManifest + `" ] || { devopsy --say error "$base has no snapshot ` + r.Snapshot + `"; exit 1; }
export ` + ImagesLockEnv + `="$root/` + ImagesLock + `" ` + ImagesLeftEnv + `=` + leftPath(t) + ` ` + ImagesModeEnv + `=` + Quote(mode) + ` ` + RestoreByEnv + `=` + Quote(r.By)
	if r.WithEnv {
		s += " " + RestoreEnvEnv + "=1"
	}
	s += `
` + watchSession(false, true) + `exec 5>"$base/` + SyncLockFile + `"
` + lockWait(5, false, "a sync or an export from $base") + `
` + settleSync(projectName) + snapshotLock(false) + `cur=$(readlink "$base/current"); cur=${cur%/` + CodeDir + `}
` + releaseImages(projectName, t, mode) + recoverFetch(`"$base/$cur"`) + `devopsy --say info "restoring snapshot ` + r.Snapshot + `'s data under ${cur#releases/}"
` + step("check-fit", `"$snap"`) + ` || exit 1
` + step("check-tars", `"$snap"`) + ` || exit 1
`
	// With release snapshots on, the data it replaces first, as a
	// release's: what it stops stays down for the data step, and anything
	// that ends this before it starts them again (rd_unsnap).
	unsnap := ""
	if t.SnapshotsRelease.Enabled {
		s += `export ` + snapshotEnv(t, t.SnapshotsManual.Keep) + `
rs_id=
rs=0
` + step("release-check") + ` || rs=$?
case $rs in
  0) rs_id=$(devopsy ` + ReleaseIDFlag + ` "$snaps") ;;
  3) ;;
  *) exit 1 ;;
esac
rd_unsnap() {
  if grep -q '^snapshot ' "$base/` + SyncStateFile + `" 2>/dev/null; then
    (cd "$base/current" && ` + devopsyCall(projectName, []string{"--sync-services", "settle"}, false) + `) || true
  fi
}
if [ -n "$rs_id" ]; then
  # A take from here to its detached tar's end (fd 6): the holding file
  # says so to --snapshots and --status, as a release's does.
  : >"$base/` + SnapshotHoldingFile + `"
  # A signal or the watcher's (the session gone) ends a step, never the
  # shell: what the stopped pass left stopped starts again.
  rd_sig=
  trap 'rd_sig=1' INT TERM HUP
  (umask 077; : >>"$base/$cur/` + ServerLogFile + `") || exit 1
  ` + step("mirror") + ` || { rd_unsnap; exit 1; }
  [ -z "$rd_sig" ] || { devopsy --say error "interrupted: nothing restored" || true; exit 130; }
  ` + step("release-stopped", `"$rs_id"`, `"$base/$cur"`, `"$base/$cur"`) + ` || { rd_unsnap; exit 1; }
  [ -z "$rd_sig" ] || { rd_unsnap; devopsy --say error "interrupted: nothing restored, the services started again" || true; exit 130; }
  rs_tar="cd \"\$base/current\" && COMPOSE_PROJECT_NAME=` + Quote(projectName) + ` exec nice devopsy ` + quoteAll([]string{SnapshotStepFlag, "release-tar"}) + ` \"\$rs_id\" $$"
  if command -v setsid >/dev/null 2>&1; then
    base="$base" rs_id="$rs_id" setsid sh -c "$rs_tar" </dev/null >>"$base/$cur/` + ServerLogFile + `" 2>&1 3<&- 5>&- 7<&- 8>&- 9>&- &
  else
    base="$base" rs_id="$rs_id" sh -c "$rs_tar" </dev/null >>"$base/$cur/` + ServerLogFile + `" 2>&1 3<&- 5>&- 7<&- 8>&- 9>&- &
  fi
fi
`
		unsnap = "rd_unsnap; "
	}
	s += `if [ -n "${watch_pid:-}" ]; then
` + waitBeat(`{ `+unsnap+`devopsy --say error "the session ended: nothing restored"; exit 1; }`) + `fi
trap '' INT TERM HUP PIPE
# From here it goes on whatever happens to the session: the output to the
# current release's server log, which the session follows.
out="$base/$cur/` + ServerLogFile + `"
` + followOutput + `[ -z "${rs_id:-}" ] || export ` + RestoreSnapshotEnv + `="$rs_id"
` + step("restore-data", `"$snap"`, `"$base/$cur"`) + ` || { ` + unsnap + `exit 1; }
exec 6>&- 5>&-
rs=0
(cd "$base/current" && with_tty ` + Quote(devopsyCall(projectName, []string{"--release-steps", "release", "run"}, false)) + `) || rs=$?
img ran "$base" "$base/$cur" || true
if [ "$rs" = 0 ]; then
  ` + step("restored", `"$snap"`) + ` || rs=1
else
` + firstDown(projectName) + `  rd_snapshot=` + Quote(r.Snapshot) + `
  devopsy --say error "the run step failed ($rs): what it started is down, the restore stays marked" ` + restoreFixLines(t, "rd_snapshot") + ` || true
fi
exit "$rs"
`
	return s
}

// SnapshotExportScript writes t's snapshot id as one tar on stdout, its
// only output there (the snapshot's directory: its tars, manifest, code
// and copies of the environment's variables). It holds the snapshot's
// own directory locked, shared (fd 4), taken under the environment's
// snapshot lock and kept to its end, that lock given up before the tar:
// a removal or a pruning, holding the environment's lock, skips a
// snapshot it finds locked (removeSnapshotDir), and nothing else waits
// for an export, which may take as long as its pipe (an upload to a
// bucket; review: takes and releases with release snapshots waited 10
// minutes for one, then failed).
func SnapshotExportScript(t *Target, id string) string {
	return "set -eu\n" + basePrelude(t) + snapshotLock(true) + `[ -f "$snaps"/` + Quote(id) + `/` + SnapshotManifest + ` ] || { devopsy --say error "$base has no snapshot ` + id + `"; exit 1; }
exec 4<"$snaps"/` + Quote(id) + `
flock -s 4
exec 6>&-
exec tar -C "$snaps" -cf - ` + Quote(id) + `
`
}

// SnapshotImportScript stores the snapshot tar on stdin (one
// SnapshotExportScript wrote, from this environment on this server or
// another) as one of t's, under a new id: once the server holds the
// snapshot lock (LockedMarker, then the data through --stdin-guard),
// extracted into a .import directory, checked whole as a restore checks
// its tars (--snapshot-step imported), renamed into place. It needs a
// current release, as a restore does: on a new server, release first.
func SnapshotImportScript(t *Target, projectName string) string {
	return "set -eu\n" + basePrelude(t) + `[ -L "$base/current" ] || { devopsy --say error "$base has no current release: release it first ('devopsy @` + hintAddress(t) + ` --release'), then import the snapshot and restore it (--with-env brings its shared/.env back)"; exit 1; }
umask 077
mkdir -p "$snaps"
` + snapshotLock(false) + lockedGate(false, "") + `tmp="$snaps/.import.$$"
rm -rf "$tmp"
mkdir -p "$tmp"
devopsy ` + StdinGuardFlag + ` | (cd "$base/current" && ` + devopsyCall(projectName, []string{SnapshotStepFlag, "receive"}, false) + ` "$tmp") || { rm -rf "$tmp"; exit 1; }
(cd "$base/current" && ` + devopsyCall(projectName, []string{SnapshotStepFlag, "imported"}, false) + ` "$tmp") || { s=$?; rm -rf "$tmp"; exit "$s"; }
`
}
