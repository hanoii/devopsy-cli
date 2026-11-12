package remote

import (
	"fmt"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// ActivateScript makes a release current, in one SSH session under the
// release lock: remote before steps in the current release; shared/ and
// the .env levels linked into the new one (for a rollback, the release
// before current); its role and imports (releases only); prepare steps;
// devopsy --missing-vars, so a variable compose requires is never found
// missing once live; its images, built or pulled and bookmarked
// (releaseImages), or a rollback's restored; the switch; the run step,
// going back to the previous release when it fails and restarting that
// one, its images first; pruning; remote after steps.
// Nothing before the switch changes what is live.
//
// With sync, a release syncs its parent's data once current, before
// the run step (SyncOptions): it stops the release's services labeled to,
// prints SyncMarker and waits for devopsy's go or abort on stdin. Not for
// rollbacks.
func ActivateScript(t *Target, id string, rollback bool, projectName string, p Phases, sync *SyncOptions) string {
	return activateScript(t, id, rollback, projectName, p, sync, nil)
}

// RestoreOptions are a full restore's (RestoreScript): the snapshot it
// restores, by id.
type RestoreOptions struct {
	Snapshot string
	// WithEnv brings the snapshot's shared/.env and inherit.env back
	// (RestoreEnvEnv), By is who restores (the record's).
	WithEnv bool
	By      string
}

// RestoreEnvEnv and RestoreByEnv carry RestoreOptions' WithEnv and By to
// the server's devopsy.
const (
	RestoreEnvEnv = "DEVOPSY_INTERNAL_RESTORE_ENV"
	RestoreByEnv  = "DEVOPSY_INTERNAL_RESTORE_BY"
)

// RestoreScript restores t's snapshot r.Snapshot whole: a new release,
// its id the server's, made from the snapshot's copy of its release, run
// as a rollback runs the release it restores (no upload, its own steps,
// --check-mounts restored, its images from its bookmarks), with the
// snapshot's data written in the stretch a sync's copy would be, between
// the images and the switch: the snapshot's tars checked whole before
// anything stops, then the set stopped, each entry and volume emptied and
// extracted, its history with it. Never
// marks the release it leaves rolled back: a rollback after it goes back
// there.
func RestoreScript(t *Target, projectName string, r *RestoreOptions) string {
	return activateScript(t, "", true, projectName, Phases{}, nil, r)
}

func activateScript(t *Target, id string, rollback bool, projectName string, p Phases, sync *SyncOptions, restore *RestoreOptions) string {
	if !rollback && len(p.Run) == 0 {
		// runRemote refuses a release without one (remote.Steps).
		panic("remote.ActivateScript: a release without a run step")
	}
	a := &activation{t: t, id: id, rollback: rollback, project: projectName, p: p, sync: sync, restore: restore}
	return a.head() +
		a.pick() +
		a.guards() +
		a.beforeSteps() +
		a.links() +
		a.checks() +
		a.fetch() +
		a.restoreData() +
		a.goLive() +
		a.runStretch() +
		a.failure() +
		a.complete() +
		a.prune() +
		a.afterSteps()
}

// toBase is every link's way up, from releases/<id>/code/.devopsy to $base,
// so the release root moves with a plain mv.
const toBase = "../../../../"

// activation is what every step of one activation's script reads: a new
// release (rollback false), a rollback, or a full restore (rollback true
// and restore set), a sync or not.
type activation struct {
	t        *Target
	id       string
	rollback bool
	project  string
	p        Phases
	sync     *SyncOptions
	restore  *RestoreOptions
}

// newRelease says this activation makes a release of its own, which goes
// live through a pending file: a new one or a restore's, never a rollback's.
func (a *activation) newRelease() bool {
	return !a.rollback || a.restore != nil
}

// releaseSnap says a snapshot of what this activation replaces is taken.
func (a *activation) releaseSnap() bool {
	return a.newRelease() && a.t.SnapshotsRelease.Enabled
}

// undo settles a failed fetch of rel (img undo).
func (a *activation) undo(rel string) string {
	if a.rollback {
		return `img undo --rollback ` + rel
	}
	return `img undo ` + rel
}

// undoLocked is undo past the fetch, whose image lock is gone: under the
// lock taken again.
func (a *activation) undoLocked() string {
	return `  if image_lock shared undoing; then
    ` + a.undo(`"$rel"`) + `
    image_unlock
  fi
`
}

// step runs one of the phases' steps, on a pseudo-terminal when the operator
// has one (with_tty), as the fetch and the run step: activations are never -t.
func (a *activation) step(phase, dir string, args []string, onFail string) string {
	return runningLine(args, phase) + inTTY(dir, devopsyCall(a.project, args, false), onFail)
}

// inTTY runs cmd in dir through with_tty, onFail when it fails.
func inTTY(dir, cmd, onFail string) string {
	return fmt.Sprintf("(cd %s && with_tty %s) || %s\n", dir, Quote(cmd), onFail)
}

// snapStep is a --snapshot-step in the current release, args as they are.
func (a *activation) snapStep(word string, args ...string) string {
	return "(cd \"$base/current\" && " + devopsyCall(a.project, []string{SnapshotStepFlag, word}, false) + " " + strings.Join(args, " ") + ")"
}

// steps reads a phase's steps from the restored release's own config.
// A restore's are its rollback:, else its release:, as a restart's are.
func (a *activation) steps(phase string) string {
	kind := "rollback"
	if a.restore != nil {
		kind = "restart"
	}
	return devopsyCall(a.project, []string{"--release-steps", kind, phase}, false)
}

// head is what the whole script relies on, and what an earlier activation cut
// off left: the previous release, a cut rollback.
func (a *activation) head() string {
	// One stream: nothing an activation prints is a result, and on two
	// pipes a line on stdout could reach the log after SwitchMarker on
	// stderr and be cut from it.
	s := prelude(a.t, false) + "exec 1>&2\n" + NoExit255
	s += watchSession(a.sync != nil)
	// Defined first: the fetch and not_live's callers use them.
	s += releaseImages(a.project, a.t)
	// get_images <dir> <against>, for the restart after a failed run:
	// under the shared image lock, a release's images restored in place
	// of against's (img restore), fetched again when an image it builds
	// is gone or was built with other build arguments; that build failing,
	// its bookmarks as they are (--as-built), never what a name holds. --restart: nothing refused, a data service the
	// rule would refuse held on the failed release's image its data ran,
	// in every one of the three (a fetch keeps the hold).
	s += `get_images() {
  image_lock shared fetching || return $?
  gi=0
  img restore --restart "$1" "$2" || { [ $? -eq 1 ] && fetch_images "$1" && img restore --restart --fetched "$1" "$2"; } || img restore --restart --as-built "$1" "$2" || gi=$?
  image_unlock
  return "$gi"
}
`
	// The release lock is held until a dropped session's fetch ends, so
	// none is still running.
	// Its last line: what runs on the data now goes into its history
	// before anything reads it. A release killed past its switch ran its
	// run step's up on the image it holds the data on, and its img ran
	// never came (OrbStack cuts, round 8: a rollback then held the data
	// on the older image); a settle below that makes a service again
	// records it itself (ranOnData).
	s += `prev=$(readlink "$base/current" 2>/dev/null || true)
prev=${prev%/` + CodeDir + `}; rb_from=$prev
` + recoverFetch + `[ -z "$prev" ] || img ran "$base" "$base/$prev" cut || true
`
	// A rollback cut off past its switch (ActivatingFile: this script
	// holds the lock, so none runs), current its target: cut_from is the
	// release it left. A rollback finishes it (below); a release or a
	// restore leaves it as it is, marking that release rolled back as
	// the rollback would have.
	s += `cut_line=$(cat "$base/` + ActivatingFile + `" 2>/dev/null || true)
cut_from=$(printf '%s' "$cut_line" | cut -f3)
if [ "$(printf '%s' "$cut_line" | cut -f2)" != rollback ] || [ "releases/$(printf '%s' "$cut_line" | cut -f1)" != "$prev" ] || [ ! -d "$base/releases/${cut_from:-.}" ]; then cut_from=; fi
`
	if a.newRelease() {
		s += `if [ -n "$cut_from" ] && [ ! -e "$base/releases/$cut_from/` + RolledBackFile + `" ]; then
  : >"$base/releases/$cut_from/` + RolledBackFile + `"
  devopsy --say info "$cut_from is marked rolled back: the rollback from it was cut off past its switch"
fi
`
	}
	return s
}

// pick says which release this activation makes live, with not_live for all
// that follows: for a restore also its locks, and the release it makes.
func (a *activation) pick() string {
	s := ""
	switch {
	case a.restore != nil:
		// A restore goes through a mark (a complete one clears it), as a
		// release that syncs does. Its release: made from the snapshot's
		// copy, pending until the switch, failed if it never goes live.
		s += `snap="$snaps"/` + Quote(a.restore.Snapshot) + `
[ -f "$snap/` + SnapshotManifest + `" ] || { devopsy --say error "$base has no snapshot ` + a.restore.Snapshot + `"; exit 1; }
[ -L "$base/current" ] || { devopsy --say error "$base has no current release: release it first, then restore"; exit 1; }
` + settleSync(a.project) + `exec 5>"$base/` + SyncLockFile + `"
` + lockWait(5, false, "a sync or an export from $base") + `
` + snapshotLock(false) + `[ -f "$snap/` + SnapshotManifest + `" ] || { devopsy --say error "$base has no snapshot ` + a.restore.Snapshot + `"; exit 1; }
export ` + RestoreByEnv + `=` + Quote(a.restore.By) + map[bool]string{true: " " + RestoreEnvEnv + "=1"}[a.restore.WithEnv] + `
id=$(devopsy ` + ReleaseIDFlag + ` "$base/releases")
devopsy --say info "restoring snapshot ` + a.restore.Snapshot + ` as release $id"
` + timerStart("restore_t") + NotLive + a.snapStep("make-release", `"$snap"`, `"$base/releases/$id"`) + ` || { rm -rf "$base/releases/$id"; exit 1; }
`
	case a.rollback:
		s += partialGuard(a.t, a.project, "exit 1")
		// The newest live release older than current (images.RollbackTarget):
		// never a failed, rolled-back or still fetching one.
		// A cut rollback (cut_from) is finished, never followed by one
		// further back: current is its target, the release it left
		// (rb_from) the one to mark rolled back. $prev stays current until
		// the switch (the undo, not_live's words), then becomes rb_from:
		// what a failure goes back to.
		s += `id=
if [ -n "$cut_from" ]; then
  id=${prev#releases/}; rb_from=releases/$cut_from
  devopsy --say info "finishing the rollback to $id from $cut_from, cut off past its switch"
fi
if [ -z "$id" ]; then
  # Its data services checked leniently first (exit 3, said): a refused
  # rollback runs no before or prepare step.
  id=$(img rollback-target "$base") || { rt=$?; [ "$rt" -ne 3 ] || exit 3; devopsy --say error "no release before the current one"; exit 1; }
  devopsy --say info "rolling back to $id"
fi
` + RollbackNotLive
	default:
		s += "id=" + Quote(a.id) + "\n" + NotLive
	}
	return s
}

// guards say where the release is, end the activation when it is gone, and
// hold its pending file until the switch.
func (a *activation) guards() string {
	s := `rel="$base/releases/$id"
code="$rel/` + CodeDir + `"
[ -d "$code" ] || { devopsy --say error "release $id is gone: removed while it waited for the release lock"; exit 1; }
`
	if a.sync == nil && !a.rollback {
		s += partialGuard(a.t, a.project, "not_live 1")
	}
	if a.newRelease() {
		// Held until the switch: --releases tells this release in
		// progress from one cut off before (ReleasesScript), whatever
		// else holds the release lock.
		s += `if [ -e "$rel/` + PendingFile + `" ]; then
  exec 7<"$rel/` + PendingFile + `"
  flock -w 5 7 || true
fi
`
	}
	return s
}

// beforeSteps run in the current release, before anything changes.
func (a *activation) beforeSteps() string {
	s := ""
	// A rollback's steps are the restored release's, read from its own
	// config on the server (ReleaseStepsFlag): the checkout's may have
	// changed since. Its before steps run in the current release, as a
	// release's do. On a pseudo-terminal like a release's (with_tty,
	// which may run them in script's own shell: the release's code
	// exported for that).
	// A restore's are the restored release's rollback:, else its
	// release:, as a restart's are.
	if a.rollback {
		s += `rb_code="$rel/` + CodeDir + `"; export rb_code
` + inTTY(`"$base/current"`, a.steps("before")+` "$rb_code"`, "not_live $?") + `unset rb_code
`
	} else if len(a.p.Before) > 0 {
		s += "if [ -n \"$prev\" ]; then\n"
		for _, args := range a.p.Before {
			s += a.step("before", `"$base/current"`, args, "not_live $?")
		}
		s += "else\n  devopsy --say warning \"no current release: remote before steps skipped\"\nfi\n"
	}
	return s
}

// links give the release its shared files.
func (a *activation) links() string {
	s := ""
	// Every link is relative, from releases/<id>/code/.devopsy up to $base
	// (toBase), so the release root moves with a plain mv.
	s += `# Always linked, so editing the server's .env applies without a new release.
touch "$base/shared/.env"
for f in "$base"/shared/* "$base"/shared/.[!.]*; do
  [ -e "$f" ] || [ -L "$f" ] || continue
  rm -rf "$code/.devopsy/${f##*/}"
  ln -s "` + toBase + `shared/${f##*/}" "$code/.devopsy/${f##*/}"
done
`
	// inherit.env (a child's, from its parent's inherit:) and parent.env
	// (its copy of its parent's server's levels) likewise: --inherit, a
	// reload and a release write them into a running release.
	s += `ln -sfn "` + toBase + `shared/` + InheritFile + `" "$code/.devopsy/` + InheritFile + `"
ln -sfn "` + toBase + `shared/` + ParentEnvFile + `" "$code/.devopsy/` + ParentEnvFile + `"
`
	// The project's and the instance's .env, shared by their environments:
	// linked even when missing, so one created later applies too. Each
	// level is a directory above $base.
	for _, l := range a.t.Levels {
		up := toBase + strings.Repeat("../", strings.Count(a.t.Path, "/")-strings.Count(l.Dir, "/"))
		s += fmt.Sprintf("ln -sfn %s.env \"$code/.devopsy/%s\"\n", up, l.Link)
	}
	if a.restore != nil && a.restore.WithEnv {
		// --with-env: the new release reads the snapshot's variables (a
		// file it carries) from its checks to its data step, which puts
		// them into shared/: its images are listed and fetched for them,
		// compose's own .env read included.
		s += `[ ! -f "$snap/` + SnapshotData + `/shared.env" ] || ln -sfn "$snap/` + SnapshotData + `/shared.env" "$code/.devopsy/.env"
[ ! -f "$snap/` + SnapshotData + `/inherit.env" ] || ln -sfn "$snap/` + SnapshotData + `/inherit.env" "$code/.devopsy/` + InheritFile + `"
`
	}
	return s
}

// checks are prepare and what refuses a release before its images.
func (a *activation) checks() string {
	s := ""
	if a.sync != nil && a.sync.Images != "" {
		// The parent's data's windows, for the check before the fetch (img
		// check) and the take-over once the copy is complete.
		s += a.sync.imagesExport()
	}
	if a.sync != nil {
		// What the sync inherits this time, in the pending files (the
		// operator's, or the child's own from a parent on its server):
		// the checks before the switch (prepare, --missing-vars, the
		// images) see what the release will run with, as a first
		// release's do in inherit.env. Read only where they exist.
		s += a.sync.sameServerCopy() + "export " + InheritPendingEnv + "=\"$base\"/" + Quote(SyncInheritPending(a.sync.ID)) + "\n"
	}
	if !a.rollback {
		s += fmt.Sprintf(PrepareScript, devopsyCall(a.project, []string{"--prepare-release"}, false))
	}
	if a.rollback {
		s += inTTY(`"$code"`, a.steps("prepare"), "not_live $?")
	}
	for _, args := range a.p.Prepare {
		s += a.step("prepare", `"$code"`, args, "not_live $?")
	}
	s += fmt.Sprintf("(cd \"$code\" && %s) || not_live $?\n", devopsyCall(a.project, []string{"--missing-vars"}, false))
	if a.rollback {
		// What the restored release's owners' step needs after the
		// switch, and only that (its labels refuse nothing): the util
		// image, which may have been pruned since, and no owned entry a
		// link.
		// A restore's names its snapshot: the entries it carries are
		// replaced, their links checked in its tars (check-tars).
		snapArg := ""
		if a.restore != nil {
			snapArg = ` "$snap"`
		}
		s += "(cd \"$code\" && " + devopsyCall(a.project, []string{"--check-mounts", "restored"}, false) + snapArg + ") || not_live 1\n"
	}
	if a.restore != nil {
		// The snapshot's tars read and checked whole, before anything
		// stops (devopsy's own, still checked: they sit on disk), in the
		// restored code, whose binds the restored data meets.
		s += "(cd \"$code\" && " + devopsyCall(a.project, []string{SnapshotStepFlag, "check-tars"}, false) + " \"$snap\") || not_live 1\n"
	}
	if !a.rollback {
		// The checks reading compose's configuration, which fails while a
		// required variable is unset: after the prepare steps that make
		// them (a first release's secrets) and --missing-vars, or they
		// would pass unchecked. Another environment's compose project of
		// the same name, or a network the project names that another
		// project owns already: they would share it (cli.ProjectConflict,
		// cli.NetworkConflicts).
		s += "(cd \"$code\" && " + devopsyCall(a.project, []string{"--check-names"}, false) + " \"$base\") || not_live 1\n"
		// Mounts whose data would not be what the labels say: a writable
		// one in the release's tree, data or an owner with no entry, an
		// entry with two owners, ./mnt whole writable, an owned path
		// through a link (cli.CheckMounts); and the util image the owners'
		// step needs after the switch.
		s += "(cd \"$code\" && " + devopsyCall(a.project, []string{"--check-mounts"}, false) + ") || not_live 1\n"
	}
	return s
}

// fetch gets the release's images, the last stretch that a session gone
// cancels, and the log's check after it.
func (a *activation) fetch() string {
	s := ""
	// fetch_release: the release's images under the shared lock,
	// undone before the lock goes on a failure or a signal, so no
	// removal comes in between: a new release's own images are
	// bookmarked, so they go with it, and its fetch tags go (img undo; a
	// rollback's target keeps its bookmarks). A
	// signal only sets fetch_sig: the fetch's child gets it too and
	// fails, and the same path undoes it, with signals and SIGPIPE
	// ignored from there on, so neither a second Ctrl-C nor a write to
	// a gone session stops it. A session that drops without a signal
	// (CI's -T) leaves FetchingFile, which the next activation, --destroy
	// and --releases remove recover; a rollback writes none, which
	// would mark a good release failed.
	// A new release: its data services against their data's history
	// first (img check, exit 3: a refused release builds and pulls
	// nothing), then its fetch and its record (img record: the listing,
	// the bookmarks, devopsy's compose file). A rollback: its images
	// restored against the release it leaves (img restore, which checks
	// its data services before any pull), fetched again only when an image
	// it builds is gone (exit 1); a full restore checks nothing (--restore:
	// its data and their history come back from the snapshot). A signal
	// during either (img ignores it) skips the fetch.
	fetchStep := `{ [ -z "$fetch_sig" ] && img check ` + a.sync.againArg() + `"$1" && [ -z "$fetch_sig" ] && fetch_images "$1" && img record "$1" ${prev:+"$base/$prev"}; }`
	if a.rollback {
		restoreFlag := ""
		if a.restore != nil {
			restoreFlag = "--restore "
		}
		fetchStep = `{ img restore ` + restoreFlag + `"$1" "$base/$rb_from" || { [ $? -eq 1 ] && [ -z "$fetch_sig" ] && fetch_images "$1" && img restore ` + restoreFlag + `--fetched "$1" "$base/$rb_from"; }; }`
	}
	// A release with snapshots on, replacing a current one, takes the
	// snapshot of what it replaces (part 4 of snapshots): its locks and
	// check before the fetch, its first pass during it, its stopped pass
	// inside the stretch that cancels, leaving the services stopped for
	// the run step to start under the new code, and its tars from the
	// mirror after, detached (release-tar).
	// A restore takes one too, of the data it replaces (its first write
	// turns the snapshot's lines into its own: restore-data).
	if a.releaseSnap() {
		s += releaseSnapshotSetup(a.t, a.project, a.restore != nil)
	}
	s += `fetch_release() {
  image_lock shared fetching || return $?
  fr=0
` + map[bool]string{true: releaseSnapshotFirstPass(a.project), false: ""}[a.releaseSnap()] + `  ` + fetchStep + ` || fr=$?
` + map[bool]string{true: releaseSnapshotStopped(a.project, a.restore != nil), false: ""}[a.releaseSnap()] + `
  # Disarmed here, under the lock, by taking the watcher's file away,
  # atomically against the watcher, which takes it before it signals:
  # when it got there first, its SIGTERM is on its way, or here, and the
  # fetch is undone like one interrupted. With a heartbeat, only on the
  # session's next byte (waitBeat): a session that went silent shortly
  # before never sees its release go live. Past this, the release goes
  # on.
  if [ -n "${watch_pid:-}" ]; then
` + waitBeat("fetch_sig=1") + `  fi
  # The watcher fired: nobody reads the session, whose buffers may be
  # full, and a write there would block the undo with the lock held.
  ws_gone=
  [ -z "${watch_pid:-}" ] || [ ! -e "$ws_armed.fired" ] || ws_gone=1
  [ -z "${watch_pid:-}" ] || rm -f "$ws_armed.off" "$ws_armed.fired"
  [ -z "$fetch_sig" ] || fr=130
  # Past this point the release goes on, and signals are ignored to its
  # end: a trapped one still kills the foreground child (rm, ln, mv in
  # make_current), which set -e turns into the script's end, halfway
  # through the switch, and one stopping the run step left its
  # grandchildren (script's session) running into the restart.
  # Ignored, children inherit that; script, which reads its signals
  # even ignored, gets its own session (tty_detach, with_tty).
  if [ "$fr" -eq 0 ]; then
    trap '' INT TERM HUP
    tty_detach=1
  fi
  if [ "$fr" -ne 0 ]; then
    trap '' INT TERM HUP PIPE
    set +e
    if [ -n "$ws_gone" ]; then
      (umask 077; : >>"$1/` + ServerLogFile + `") || true
      exec >>"$1/` + ServerLogFile + `" 2>&1
      echo "` + LogSection + `$(date -u '+%Y-%m-%d %H:%M:%S') cancel.$$"
      devopsy --say warning "the session is gone: undoing the fetch"
    fi
    [ -z "$fetch_sig" ] || devopsy --say error "the images' fetch was interrupted"
    ` + a.undo(`"$1"`) + `
  fi
  image_unlock
  return "$fr"
}
fetch_sig=
trap 'fetch_sig=1' INT TERM HUP
trap '' PIPE
`
	if !a.rollback {
		s += `: >"$rel/` + FetchingFile + `"
`
	}
	s += `fr=0
fetch_release "$rel" || fr=$?
[ "$fr" -eq 0 ] || [ -z "$fetch_sig" ] || fr=130
# A write before the marker goes: a session gone during the fetch that
# watchSession did not see (none on -t, which gets HUP) fails here (PIPE
# is ignored, so the write fails and set -e ends it), the marker kept for
# img recover and current as it was, through not_live (whose hook starts
# what a release snapshot's stopped pass left stopped). img record ignores
# SIGPIPE, so nothing died sooner.
[ "$fr" -ne 0 ] || devopsy --say info "the images of $id are ready" || not_live 1
rm -f "$rel/` + FetchingFile + `"
[ "$fr" -eq 0 ] || not_live "$fr"
out="$rel/` + ServerLogFile + `"
# Made before the switch: a full disk or a bad mode fails here, the
# release not live, rather than at the log's first write after it.
if ! (umask 077; : >>"$out"); then
  set +e
` + a.undoLocked() + `  not_live 1
fi
`
	return s
}

// restoreData is a restore's data step.
func (a *activation) restoreData() string {
	s := ""
	if a.restore != nil {
		// The data, once the images are ready and past the point a session
		// gone cancels: before its first write a failure starts the set
		// again; from it the set stays down under a restore mark. Nothing
		// interrupts it, so its output goes to the server's log from here
		// (followOutput's marker tells the operator's devopsy it goes on),
		// and a failure settles the fetch as the log check does.
		s += "trap '' PIPE\n" + followOutput + `ln -sfn "` + toBase + `shared/.env" "$code/.devopsy/.env"
ln -sfn "` + toBase + `shared/` + InheritFile + `" "$code/.devopsy/` + InheritFile + `"
[ -z "${rs_id:-}" ] || export ` + RestoreSnapshotEnv + `="$rs_id"
rd_status=0
` + a.snapStep("restore-data", `"$snap"`, `"$rel"`) + ` || rd_status=$?
if [ "$rd_status" != 0 ]; then
  set +e
  # The fetch settled first (img undo): what the settle below starts again,
  # or makes again (images.Recreate), runs the current release's images by
  # its bookmarks, which the restore's fetch never moved.
` + a.undoLocked() + `` + killedSays(a.t, a.project, "rd_status", "nothing restored") + `  not_live 1
fi
` + sayTook("restore_t", `"snapshot `+a.restore.Snapshot+`'s data restored"`)
	}
	return s
}

// goLive is the switch.
func (a *activation) goLive() string {
	s := ""
	activating, snapField := "release", ""
	switch {
	case a.restore != nil:
		activating, snapField = "restore", a.restore.Snapshot
	case a.rollback:
		activating = "rollback"
		// From the switch on, $prev is what a failure goes back to.
		s += "prev=$rb_from\n"
	}
	// The mark before the pending file goes: a shell killed between them
	// leaves a release still INCOMPLETE, whose mark --status and
	// --releases ignore (its id is not current).
	s += `printf '%s\t%s\t%s\t%s\n' "$id" ` + activating + ` "${prev#releases/}" ` + Quote(snapField) + ` >"$base/` + ActivatingFile + `" || not_live 1
`
	if a.newRelease() {
		s += `rm -f "$rel/` + PendingFile + `"
exec 7<&-
`
	}
	s += `make_current "releases/$id"
`
	if a.releaseSnap() {
		s += `[ -z "${rs_id:-}" ] || ` + a.snapStep("release-record", Quote("live"), `"$rel"`) + ` || true
` + `[ -z "${rs_id:-}" ] || export ` + HeldStopsEnv + `=1
`
	}
	s += `devopsy --say info "current is now $id" || true
`
	return s
}

// restartPrevious brings the release before back after a failure, in the
// failure's branch.
func (a *activation) restartPrevious() string {
	// The previous release, back after a failed run, restarts with its own
	// images and its own run step: its config's rollback:, else its
	// release: (ReleaseStepsFlag).
	// The previous release runs its own images by its bookmarks, which the
	// new release's fetch never moved; a floating tag the new one pulled
	// stays the registry's.
	// Not on a partial copy (a sync's that went wrong): the previous
	// release's code would run on it.
	s := `    if [ -e "$base/` + SyncIncompleteFile + `" ]; then
      if [ "$(cut -f3 "$base/` + SyncIncompleteFile + `" 2>/dev/null)" = ` + IncompleteImport + ` ]; then
        rs_service=$(cut -f2 "$base/` + SyncIncompleteFile + `" 2>/dev/null) || true
        devopsy --say warning "not restarting ${prev#releases/}: the import of $rs_service did not complete, and nothing runs on its data until one does" ` + importFixes(a.t, "rs_service") + `
      elif [ "$(cut -f3 "$base/` + SyncIncompleteFile + `" 2>/dev/null)" = ` + IncompleteRestore + ` ]; then
        rs_snapshot=$(cut -f2 "$base/` + SyncIncompleteFile + `" 2>/dev/null) || true
        devopsy --say warning "not restarting ${prev#releases/}: the restore of snapshot $rs_snapshot did not complete, and nothing runs on its data until one does" ` + restoreFixes(a.t, "rs_snapshot") + `
      else
        devopsy --say warning "not restarting ${prev#releases/}: the data is from a sync that did not complete, and nothing runs on it until one does: ` + IncompleteWhy + `" ` + syncFixes(a.t) + `
      fi
      rm -f "$base/` + ActivatingFile + `"; exit "$status"
    fi
    get_images "$base/$prev" "$rel" || {
      gi_s=$?
      devopsy --say warning "getting the images of ${prev#releases/} failed ($gi_s): restarting it anyway"
      # Its data services still held where the failed release's image ran
      # on their data, without the image lock or Docker (img hold):
      # never its old compose file's older image on that data.
      img hold "$base/$prev" "$rel" || true
    }
`
	// Its entries of mnt to their owners, by its own labels: the new
	// release's may have given them to others.
	s += `    (cd "$base/current" && ` + devopsyCall(a.project, []string{"--mnt-owners", "restored"}, false) + `) || devopsy --say error "giving the entries of mnt and the named volumes to their owners failed ($?): restarting ${prev#releases/} anyway"
`
	// No bind source of it may lead out of its entry through a link in
	// the data as the failed release left it (a sync's copy, a restore's):
	// as a rollback checks before its switch (review).
	s += `    if ! (cd "$base/current" && ` + devopsyCall(a.project, []string{"--check-mounts", "restored", "restart"}, false) + `); then
      devopsy --say error "not restarting ${prev#releases/}: what the failed release started still runs, from its code, until a release goes live" ` + sayRemedies(report.Fix("once what the check says is fixed:", "devopsy @"+HintAddress(a.t)+" --release")) + `
      rm -f "$base/` + ActivatingFile + `"; exit "$status"
    fi
`
	s += `    devopsy --say info "restarting ${prev#releases/} with its own run step"
    ` + inTTY(`"$base/current"`, devopsyCall(a.project, []string{"--release-steps", "restart", "run"}, false), `devopsy --say error "restarting ${prev#releases/} failed too ($?): its services may be down" `+sayRemedies(SeeHint(HintAddress(a.t), "what runs", "--status"), LogHint(HintAddress(a.t)))) + `    img ran "$base" "$base/$prev" || true
`
	if a.releaseSnap() {
		s += `    [ -z "${rs_id:-}" ] || ` + a.snapStep("release-record", Quote("done"), `"$rel"`) + ` || true
`
	}
	return s
}

// runStretch is what runs on the new release: owners, a sync, the run step.
func (a *activation) runStretch() string {
	s := ""
	// The entries of mnt created when missing and given to their owners
	// (cli.MntOwnerLabel), right after the switch, from the labels of
	// what is now current (a rollback's restored release's): it changes
	// live data, so never before it. Before a sync, whose first one
	// creates the services with data before its finish hook, and before
	// the run step. A failure is a failed run step: the usual way back.
	// A restored release's (restored: a rollback's, the restart's) labels
	// that this devopsy cannot read only warn: a rollback never fails for
	// them, as it never checks roles or imports.
	ownersArgs := []string{"--mnt-owners"}
	if a.rollback {
		ownersArgs = append(ownersArgs, "restored")
	}
	owners := `(cd "$base/current" && ` + devopsyCall(a.project, ownersArgs, false) + `) || { status=$?; failed="giving the entries of mnt and the named volumes to their owners"; }
`
	s += "status=0\nfailed=run\n"
	if a.sync != nil {
		s += owners + `if [ "$status" = 0 ]; then
` + a.sync.exports(a.t) + `cd "$base/current"
` + syncBlock(a.project, "the release does not go live", true) + `cd "$base"
[ "$status" = 0 ] || failed=sync
else
  [ -z "${` + InheritPendingEnv + `:-}" ] || rm -f "$` + InheritPendingEnv + `" "${` + InheritPendingEnv + `%.env}.` + ParentEnvFile + `"
fi
`
	}
	// The server's log starts here, after the switch (signals ignored
	// from before it, and its one write tolerates a gone session) and
	// after a sync, which has its own way through a dropped session
	// (syncWait) and whose copy's lines reach the session's log from the
	// push, so --log shows them in their place. PIPE is ignored again: a
	// sync's end restores it.
	if a.restore == nil {
		s += "trap '' PIPE\n" + followOutput
	}
	if a.sync == nil {
		s += owners
	}
	if a.rollback {
		s += `if [ "$status" = 0 ]; then
  ` + inTTY(`"$base/current"`, a.steps("run"), "status=$?") + `fi
`
	} else {
		s += `if [ "$status" = 0 ]; then
` + runningLine(a.p.Run, "run") + `  ` + timerStart("run_t") + `  ` + inTTY(`"$base/current"`, devopsyCall(a.project, a.p.Run, false), "status=$?") + `  [ "$status" != 0 ] || ` + sayTook("run_t", Quote("'devopsy "+strings.Join(a.p.Run, " ")+"' done")) + `fi
`
	}
	// What ran on the data, failed or not, before a restart replaces it:
	// the history the next activation's checks and holds go by.
	if a.releaseSnap() {
		// Only once something started them: the run step, else the restart
		// of the release before (below). A cut sync's copy or a failed
		// restore keeps them down, its record saying so.
		s += `[ -z "${rs_id:-}" ] || [ "$status" != 0 ] || ` + a.snapStep("release-record", Quote("done"), `"$rel"`) + ` || true
`
	}
	// A restore records what ran only once restored started its set on
	// the images it holds (startLeftDown makes again what another image
	// runs): a data service its run step left as it was is still the
	// container that ran the replaced data, on that data's image (review:
	// its old image became the restored data's newest). A failed one ran
	// nothing on the restored data but its hold.
	if a.restore == nil {
		s += `img ran "$base" "$rel" || true
`
	}
	return s
}

// failure is the way back from a failed run stretch.
func (a *activation) failure() string {
	s := ""
	// A failure while Docker does not answer (the machine shutting down,
	// whose stop ended the run step, or the daemon gone): the restart and
	// the report could only fail too, current back on images nothing
	// restored. Left as an activation cut off past its switch instead,
	// its mark kept, which --status names and the next release finishes.
	// timeout: a daemon stopping can hang a call, under the lock.
	s += `if [ "$status" != 0 ] && ! { if command -v timeout >/dev/null 2>&1; then timeout 5 docker version; else docker version; fi; } >/dev/null 2>&1; then
  devopsy --say error "$failed failed ($status) and Docker does not answer: nothing restarted, the release is left as one cut off past its switch" ` + sayRemedies(report.Hint("once Docker answers again, to see how to finish it:", "devopsy @"+HintAddress(a.t)+" --status")) + `
  exit "$status"
fi
`
	// A restore whose run step failed keeps its mark, so the previous
	// release is not restarted (the restart skips a marked environment):
	// what the failed step started goes down too, never left running on
	// the restored data (review of 2026-10-25, round 2).
	restoreDown := ""
	if a.restore != nil {
		restoreDown = firstDown(a.project)
	}
	s += `if [ "$status" != 0 ]; then
  touch "$rel/` + FailedFile + `"
` + failureReport(a.project) + restoreDown + `  if [ -n "$prev" ]; then
    # Killed in the restart, the mark names the release restarted, current.
    printf '%s\trestart\t%s\t\n' "${prev#releases/}" "$id" >"$base/` + ActivatingFile + `" || true
    make_current "$prev"
    devopsy --say error "$failed failed ($status), current is back to ${prev#releases/}"
` + a.restartPrevious() + `  else
` + firstDown(a.project) + `    rm -f "$base/current"
    devopsy --say error "$failed failed ($status), no previous release to go back to"
  fi
  # The activation ended, failed: no cut one (ActivatingFile).
  rm -f "$base/` + ActivatingFile + `"
  exit "$status"
fi
`
	return s
}

// complete is the activation gone through, what follows only reporting when cut.
func (a *activation) complete() string {
	s := ""
	if a.restore != nil {
		// Its mark and record go once the run step started the set.
		s += a.snapStep("restored", `"$snap"`) + " || { rm -f \"$base/" + ActivatingFile + "\"; exit 1; }\n"
		s += `img ran "$base" "$rel" || true
`
	}
	if a.rollback && a.restore == nil {
		// The release it left: never counted toward keep, never rolled back
		// to again, its log kept until pruning takes it.
		s += `: >"$base/$prev/` + RolledBackFile + `"
devopsy --say info "${prev#releases/} is marked rolled back"
`
	}
	// The activation went through: what follows (pruning, after steps)
	// resumes or only reports when cut.
	s += `rm -f "$base/` + ActivatingFile + `"
`
	return s
}

// prune removes what keep no longer counts.
func (a *activation) prune() string {
	s := ""
	// keep: the target's or project's, else the server's, at most its
	// max_keep. Only releases that went live and stayed count: failed ones
	// never ran, a rolled-back one was left, and a rollback skips both. One
	// newer than the oldest release kept stays, with its log; older ones
	// go. A rollback marks the release it left (images.RolledBackFile).
	s += fmt.Sprintf(`k=%d
[ "$k" -gt 0 ] || k=$keep
if [ "$k" -gt "$max_keep" ]; then
  devopsy --say warning "keeping $max_keep releases, this server's max_keep, not $k"
  k=$max_keep
fi
# What pruning removes (images.ToPrune): releases older than the k-th
# live one, current and those still fetching aside.
pruned=$(img to-prune "$base" "$k")
# Removing images needs the server's image lock alone: when another
# release holds it, pruning waits for the next release, directories
# included (one gone without its images would leave tags nothing finds).
if [ -n "$pruned" ]; then
  if image_lock exclusive removing now; then
    # A release whose images Docker could not finish removing stays, for
    # the next pruning.
    img remove-releases --quiet "$base" $pruned || true
    img prune
    image_unlock
  else
    devopsy --say warning "pruning skipped: $il_busy; the next release prunes"
  fi
fi
`, a.t.Keep)
	return s
}

// afterSteps run once it is live: a failure leaves it so.
func (a *activation) afterSteps() string {
	s := ""
	afterFail := `{ s=$?; devopsy --say error "an after step failed ($s): it stays live"; exit "$s"; }`
	if a.rollback {
		s += inTTY(`"$base/current"`, a.steps("after"), afterFail)
	}
	for _, args := range a.p.After {
		s += a.step("after", `"$base/current"`, args, afterFail)
	}
	return s
}
