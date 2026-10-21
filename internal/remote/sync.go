package remote

import (
	"cmp"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// A sync copies a parent environment's data (shared/mnt) into a child's,
// host to host: devopsy @<child> --sync, and a child's first release.
// devopsy opens two sessions: the holder, on the child's server, which takes
// the child's release lock, stops its services labeled
// devopsy.sync.stop=true and prints SyncMarker; then the push, on the
// parent's server, which copies (devopsy --sync-push): rsync, then each
// stream through a spool file in the child's directory (StreamSpool). The
// push's result goes back to the holder on its stdin, go or abort, and the
// holder restarts the services and runs the release's run step.

// HalfCopiedNotice follows "mnt/<entry>" when a copy was cut: what a
// service that reads the entry raw is left with.
const HalfCopiedNotice = "is half copied: a database copied raw cannot start on it until the next sync completes it"

// What a sync writes into the child's directory ($base), so the child
// knows what happened to its data whatever its answer says: the push's
// first write into the child (rsync or an import) claims the gate,
// SyncStartedFile, which then records the writers: each import appends
// a WriterLine before it writes, and the last write appends a DoneLine.
// The holder decides by taking that record in one move (SyncTakenFile):
// a writer appending after it finds no file and stops. Its own sync's
// done is go (the copy is complete), taken without it partial (written
// in part: the stop=true services stay down, SyncIncompleteFile, and
// whatever still writes is ended, --sync-services finish partial),
// nothing taken the answer (nothing written). A holder that never
// decided (killed) leaves them for whoever takes the lock next
// (--sync-services settle), which ends the writers first.
const (
	// SyncOpenPrefix and the sync's id name the gate (SyncOpenFile).
	SyncOpenPrefix  = ".devopsy-sync.open."
	SyncStartedFile = ".devopsy-sync.started"
	// SyncTakenFile is SyncStartedFile once the holder decided, until its
	// finish ended what the record names.
	SyncTakenFile = ".devopsy-sync.taken"
	// SyncHoldingFile says a sync's holder holds the child's lock, from
	// before its prepare to after its finish: with the lock held
	// (SyncRunning), a sync runs. One a killed holder left counts for
	// nothing once the lock is free.
	SyncHoldingFile = ".devopsy-sync.holding"
	// SyncLockFile is an environment's sync lock: a push from it or an
	// export of it holds it exclusively (syncSourceLock).
	SyncLockFile = ".devopsy-sync.lock"
	// SyncIncompleteFile marks a child whose data no sync completed on:
	// one line, IncompleteLine. Releases and rollbacks refuse, a push
	// from it too, until a sync completes and removes it.
	SyncIncompleteFile = ".devopsy-sync.incomplete"
)

// SyncOpenFile is sync id's gate: the holder writes it (the parent's
// address and the id) before its marker and removes it before deciding;
// the first write renames it to SyncStartedFile, and one that finds
// neither never starts. Named by the id, so the rename alone decides: a
// write can only ever claim its own sync's gate.
func SyncOpenFile(id string) string { return SyncOpenPrefix + id }

// WriterLine records an import in SyncStartedFile before it writes: its
// service, the pid of the devopsy running it, and the service's state
// before (running: stopped to end a cut import, it is recorded for the
// next completing sync to start).
func WriterLine(service string, pid int, was string) string {
	return fmt.Sprintf("writer import %s %d %s", service, pid, cmp.Or(was, "none"))
}

// SyncContainerPrefix starts the names of the containers a sync runs to
// write into a child (rsync, on one server or across two), then the
// sync's id: what --sync-services finish partial and settle remove.
const SyncContainerPrefix = "devopsy-sync-"

// DoneLine records the copy's last write in SyncStartedFile.
func DoneLine(id string) string {
	return "done " + id
}

// Why a child is marked (SyncIncompleteFile), the line's code: a copy
// cut, a holder killed before it decided (settled by the next
// lock-taker), or a complete copy its project's finish child go refused.
const (
	IncompleteCopy    = "copy"
	IncompleteSettled = "settled"
	IncompleteFinish  = "finish"
)

// IncompleteLine is SyncIncompleteFile's content: since when (UTC, the
// first mark's: a later one keeps it), the parent's address, the code
// and why, for people, tab separated.
func IncompleteLine(since, from, code, why string) string {
	return since + "\t" + from + "\t" + code + "\t" + why + "\n"
}

// ParseIncomplete reads IncompleteLine.
func ParseIncomplete(s string) (since, from, code, why string) {
	f := strings.SplitN(strings.TrimRight(s, "\n"), "\t", 4)
	f = append(f, "", "", "", "")
	return f[0], f[1], f[2], f[3]
}

// IncompleteWhy is shell printing the mark's why and since ($base set),
// for a message.
const IncompleteWhy = `$(cut -f4 "$base/` + SyncIncompleteFile + `" 2>/dev/null) (since $(cut -f1 "$base/` + SyncIncompleteFile + `" 2>/dev/null) UTC)`

// SyncCopied starts the line a sync's log gets, followed by the
// parent's address, once the copy went through, whatever the run step
// does after it: --status's last sync.
const SyncCopied = report.Prefix + "copied the data from @"

// SyncMarker starts the line the holder prints (on stderr: holders print
// on one stream), followed by the child's directory on its server
// ($base), a tab and the sync's id (SyncIDEnv), once the child is ready
// to be written.
const SyncMarker = report.Prefix + "ready for sync in "

// SyncIDEnv carries the sync's id, from the holder's marker, to the
// push and the child's side of its writes: a write claims the gate
// only when it names this sync (ClaimWriteScript), so one left over
// from an earlier sync never writes into a newer one.
const SyncIDEnv = "DEVOPSY_INTERNAL_SYNC_ID"

// SyncingInto is the line the holder of --sync prints on stderr: the
// release whose log the sync's is appended to.
var SyncingInto = regexp.MustCompile(regexp.QuoteMeta(report.Prefix) + `syncing into ([0-9]{14})`)

// KeepAlive are the ssh options of a sync's sessions (the holder, the
// push, and the parent's sessions to the child): a session can stay quiet
// for the whole copy, and a NAT or firewall dropping it would abort the
// sync halfway, or leave a push writing into a child already restored.
//
// Every 3 beats of the heartbeat (HeartbeatEvery: 15 seconds), so the
// end-to-end tests' shorter beat keeps the order between the push's
// silence, the operator noticing and the holder's silence.
var KeepAlive = []string{"-o", "ServerAliveInterval=" + strconv.Itoa(3*int(HeartbeatEvery().Seconds())), "-o", "ServerAliveCountMax=3"}

// Bounded are KeepAlive plus a connect timeout: for sessions devopsy
// must not hang on, a release log's save after a dropped connection
// (it waited on connect, minutes, with nobody to tell), --export and
// --import (their data can pause, and a dead connection otherwise
// lasts until TCP gives up).
var Bounded = append([]string{"-o", "ConnectTimeout=30"}, KeepAlive...)

// ChildSSH are the options of the parent's sessions to the child (the
// rsync session and --sync-exec), with KeepAlive: the operator's agent,
// forwarded to the parent for the push, never goes one hop further
// (the child is the unreviewed side, and a ForwardAgent in the parent's
// ssh config would send it), a key the child refuses fails at once
// instead of waiting for a password nobody can type, and a child the
// parent cannot reach fails in seconds, not after the TCP timeout, with
// its services stopped meanwhile.
var ChildSSH = []string{"-o", "ForwardAgent=no", "-o", "BatchMode=yes", "-o", "ConnectTimeout=30", "-o", "StrictHostKeyChecking=accept-new"}

// UtilImage is devopsy's own image for what needs root on a host's files,
// built there from UtilDockerfile when missing: syncs' rsync, the tar of
// --export and --import, emptying mnt on --destroy. Its tag changes with
// the Dockerfile: hosts never rebuild an image they have. It was
// devopsy-rsync until October 2026: hosts keep that one until removed.
const UtilImage = "devopsy-util:1"

// UtilDockerfile is UtilImage's Dockerfile, one line per element. GNU
// tar, not busybox's: --numeric-owner and --overwrite as documented.
var UtilDockerfile = []string{"FROM alpine:3.22", "RUN apk add --no-cache rsync tar"}

// StreamSpoolPrefix starts the name of a stream's spool file in an
// environment's directory ($base): what an export wrote, read by the
// import once complete. Locally, --import's temporary file under
// .devopsy/ starts with it too. Every file a sync leaves in $base starts
// with .devopsy-sync.
const StreamSpoolPrefix = ".devopsy-sync.stream."

// StreamSpool is service's spool file for sync id in the environment
// directory base: one per sync, so an export that outlived its sync
// (its push killed outright) never writes into the next one's.
func StreamSpool(base, service, id string) string {
	return path.Join(base, StreamSpoolPrefix+service+"."+id)
}

// SyncInheritPending is a re-sync's inherited variables, in the child's
// environment directory ($base), named by the sync's id: written before
// the holder takes the lock, read in place of shared/inherit.env by
// everything the sync runs on the child (InheritPendingEnv), moved over it
// on go and removed on abort, so a failed copy leaves the child's
// variables as they were. One per sync: two syncs of a child at once never
// read or remove each other's, and finish removes any left
// (SyncInheritPrefix).
func SyncInheritPending(id string) string {
	return SyncInheritPrefix + id + ".env"
}

// SyncInheritPrefix starts every sync's SyncInheritPending.
const SyncInheritPrefix = ".devopsy-sync.inherit."

// SyncInheritPath is sync id's SyncInheritPending in the environment
// directory base.
func SyncInheritPath(base, id string) string {
	return path.Join(base, SyncInheritPending(id))
}

// InheritPendingEnv names the file devopsy reads in place of the
// release's inherit.env while a sync runs on the child (SyncInheritPath),
// when it exists: devopsy talking to devopsy.
const InheritPendingEnv = "DEVOPSY_INTERNAL_INHERIT_FILE"

// Variables of a sync. DEVOPSY_SYNC_TO* and DEVOPSY_SYNC_SSH_PORT are
// --sync-push's own, set for it by the push session; the sync capability
// gets SyncFromTargetEnv, SyncToTargetEnv, SyncArgsEnv and its side's
// lists (SyncRsyncEnv, SyncStreamEnv, SyncStopEnv).
const (
	// SyncToEnv is the child's server, user@hostname as the parent's
	// server reaches it; empty when both are the same host.
	SyncToEnv = "DEVOPSY_SYNC_TO"
	// SyncSSHPortEnv is the port of SyncToEnv.
	SyncSSHPortEnv = "DEVOPSY_SYNC_SSH_PORT"
	// SyncToBaseEnv is the child's directory on its server, SyncToDirEnv
	// its current release's .devopsy/.
	SyncToBaseEnv = "DEVOPSY_SYNC_TO_BASE"
	SyncToDirEnv  = "DEVOPSY_SYNC_TO_DIR"
	// SyncToProjectEnv is the child's compose project, SyncToTargetEnv
	// its address.
	SyncToProjectEnv = "DEVOPSY_SYNC_TO_PROJECT"
	SyncToTargetEnv  = "DEVOPSY_SYNC_TO_TARGET"
	// SyncArgsEnv are the arguments after --sync's --, space-joined: for
	// rsync, and for the sync capability's scripts to read.
	SyncArgsEnv = "DEVOPSY_SYNC_ARGS"
	// SyncFromTargetEnv is the parent's address.
	SyncFromTargetEnv = "DEVOPSY_SYNC_FROM_TARGET"
	// SyncRsyncEnv, SyncStreamEnv and SyncStopEnv name a side's services
	// labeled devopsy.sync=rsync, devopsy.sync=stream (depends_on order)
	// and devopsy.sync.stop=true, space-separated, for the sync
	// capability's actions (cli.SyncLists).
	SyncRsyncEnv  = "DEVOPSY_SYNC_RSYNC"
	SyncStreamEnv = "DEVOPSY_SYNC_STREAM"
	SyncStopEnv   = "DEVOPSY_SYNC_STOP"
	// SyncWatchEnv tells --sync-push that its stdin stays open for as
	// long as devopsy's push session lives: EOF means the session is gone,
	// and the push stops.
	SyncWatchEnv = "DEVOPSY_INTERNAL_SYNC_WATCH"
	// ColorEnv tells --sync-push that the operator's terminal takes
	// colors: servers have no terminal of their own to ask.
	ColorEnv = "DEVOPSY_INTERNAL_COLOR"
)

// SyncOptions make ActivateScript sync the parent's data into the new
// release before its run step.
type SyncOptions struct {
	// From is the parent's address.
	From string
	// Args go to rsync (DEVOPSY_SYNC_ARGS).
	Args []string
	// Inherit: the operator wrote this sync's inherited variables into
	// its SyncInheritPending.
	Inherit bool
	// ID is the sync's, chosen by the operator's devopsy: its pending
	// inherited variables are written before the holder runs.
	ID string
}

// exports sets the child side's sync variables, the sync's id and its
// pending inherited variables' file among them; t is the child.
func (o *SyncOptions) exports(t *Target) string {
	s := fmt.Sprintf("export %s=%s %s=%s %s=%s %s=%s\n", SyncFromTargetEnv, Quote(o.From), SyncToTargetEnv, Quote(t.Address), SyncArgsEnv, Quote(strings.Join(o.Args, " ")), SyncIDEnv, Quote(o.ID))
	s += "export " + InheritPendingEnv + "=\"$base\"/" + Quote(SyncInheritPending(o.ID)) + "\n"
	return s
}

// syncFinish defines sync_finish go|abort|partial, in the child's
// current release: on go, the pending inherited variables move into
// shared/inherit.env (which every release links), so what follows reads
// them there; otherwise they go. Then --sync-services finish, which
// starts what was stopped (not on partial) and runs the sync
// capability's finish child.
func syncFinish(projectName string) string {
	pending := `"$` + InheritPendingEnv + `"`
	shared := `"$base/shared/` + InheritFile + `"`
	return `sync_finish() {
  if [ "$1" = go ]; then
    if [ -f ` + pending + ` ]; then
      mv -Tf ` + pending + ` ` + shared + ` || return 1
    fi
  else
    rm -f ` + pending + `
  fi
  ` + devopsyCall(projectName, []string{"--sync-services", "finish"}, false) + ` "$1"
}
`
}

// syncPrepare readies the child's current release to be written:
// --sync-services prepare checks the stream services have an import,
// runs the sync capability's prepare child and stops the services labeled
// devopsy.sync.stop=true. A failure sets $status and restores what it
// changed.
//
// release is an activation's: one with nothing current before it ($prev
// empty) tells prepare it is a first release, whatever containers an
// earlier one that failed left (an import's).
//
// From here until the services are restored, SIGPIPE is ignored
// (syncRestorePipe ends that): if devopsy goes away, a write to this
// session fails instead of killing what restores them.
func syncPrepare(projectName string, release bool) string {
	call := devopsyCall(projectName, []string{"--sync-services", "prepare"}, false)
	if release {
		call += ` $([ -n "$prev" ] || echo first)`
	}
	return `trap '' PIPE
` + call + ` || status=$?
if [ "$status" != 0 ]; then
  sync_finish abort || true
  devopsy --say error "preparing the sync failed ($status)" || true
fi
`
}

// syncBlock is a sync's part on the child, in its current release with
// $status 0, the sync's variables exported (SyncOptions.exports): prepare
// (services stopped), the gate (SyncOpenFile) and the marker, the push's
// end (devopsy's answer, or none), then finish with what the files say
// was written; $status says how it went. failed says what a failure
// means here; release is an activation's, which may be a first release
// (syncPrepare).
func syncBlock(projectName, failed string, release bool) string {
	open, started, taken, incomplete := `"$base/`+SyncOpenPrefix+`$sync_id"`, `"$base/`+SyncStartedFile+`"`, `"$base/`+SyncTakenFile+`"`, `"$base/`+SyncIncompleteFile+`"`
	// The sync's id, in the gate and in the copy's last write
	// (DoneLine).
	return syncFinish(projectName) + `sync_id=$` + SyncIDEnv + `
: >"$base/` + SyncHoldingFile + `"
` + syncPrepare(projectName, release) + `if [ "$status" = 0 ]; then
  printf '%s %s\n' "$` + SyncFromTargetEnv + `" "$sync_id" >` + open + `
` + syncWait() + `  # Closed first: a write that has not claimed it never starts (one
  # rename or the unlink wins), so nothing is written into what abort
  # restores.
  rm -f ` + open + `
  # The writers' record taken in one move: a write appending after it
  # finds no file and stops.
  mv -f ` + started + ` ` + taken + ` 2>/dev/null || true
  # A push that went through and wrote nothing (the parent's labels
  # copy nothing) is go: no data the labels cover is partial any more.
  sync_word=abort
  if grep -qx "` + DoneLine("$sync_id") + `" ` + taken + ` 2>/dev/null; then
    sync_word=go
  elif [ -e ` + taken + ` ]; then
    # Written in part: the copy was cut.
    sync_word=partial
  elif [ -e ` + incomplete + ` ]; then
    # Nothing written over an earlier sync's partial copy, whatever the
    # answer: only a sync that writes completes it.
    sync_word=partial
  elif [ "$reply" = go ]; then
    sync_word=go
  fi
  case "$reply.$sync_word" in
    go.go) ;;
    *.go) devopsy --say info "the copy had completed: going on with it" || true ;;
    *.partial)
      # Marked before the files go: a kill in between leaves one or
      # the other. A mark keeps the first one's time.
      sync_since=$(cut -f1 ` + incomplete + ` 2>/dev/null) || true
      [ -n "$sync_since" ] || sync_since=$(date -u '+%Y-%m-%d %H:%M:%S')
      sync_from=$(head -n 1 ` + taken + ` 2>/dev/null | cut -d' ' -f1) || true
      if [ -e ` + taken + ` ]; then
        devopsy --say error "the copy was cut: the services it stopped stay down until a sync completes, and ` + failed + `" || true
        printf '%s\t%s\t` + IncompleteCopy + `\t%s\n' "$sync_since" "${sync_from:-$` + SyncFromTargetEnv + `}" "the copy from ${sync_from:-$` + SyncFromTargetEnv + `} was cut: the data is partly the parent's, partly what it was" >` + incomplete + ` || true
      else
        devopsy --say error "nothing was copied, and the data is still an earlier sync's that did not complete: the services stay down until a sync writes it, and ` + failed + `" "if the parent no longer has that data, fix the child's entries or labels by hand, or --destroy it and release it again" || true
      fi
      ;;
    *) devopsy --say error "the copy from the parent failed before writing anything: restoring what was stopped, and ` + failed + `" || true ;;
  esac
  # finish partial ends what the record names, then it goes.
  if [ "$sync_word" = go ]; then
    sync_finish go || status=$?
  else
    sync_finish "$sync_word" || true
    status=1
  fi
  rm -f ` + taken + `
fi
rm -f "$base/` + SyncHoldingFile + `"
` + syncRestorePipe
}

// syncRestorePipe ends syncPrepare's SIGPIPE exemption, before the run
// step, which runs as release steps always do, and the sync's variables
// (SyncOptions.exports): the run and after steps are no sync's.
const syncRestorePipe = "trap - PIPE\nunset " + SyncFromTargetEnv + " " + SyncToTargetEnv + " " + SyncArgsEnv + " " + InheritPendingEnv + " " + SyncIDEnv + "\n"

// syncWait prints the marker and reads devopsy's answer into $reply.
// Without one, devopsy is gone (interrupted, its connection lost): nobody
// reads this session any more, and a write to it would kill what restores
// the services, so the rest goes to a file in $base.
func syncWait() string {
	return `  printf '%s%s\t%s\n' "` + SyncMarker + `" "$base" "$sync_id" || true
  reply=
  read -r reply <&3 || true
  if [ -z "$reply" ]; then
    sync_gone=1
    ` + trimSyncLog + `
    exec >>"$base/.devopsy-sync.log" 2>&1
    devopsy --say warning "$(date -u '+%Y-%m-%d %H:%M:%S'): no answer from devopsy"
  fi
`
}

// SyncLogMax is the size past which .devopsy-sync.log, appended to by
// every sync whose session went, keeps only its last SyncLogKeep lines.
const (
	SyncLogMax  = 1 << 20
	SyncLogKeep = 1000
)

// trimSyncLog cuts .devopsy-sync.log to its last lines once too big,
// $base set; never fails.
var trimSyncLog = fmt.Sprintf(`{ [ ! -f "$base/.devopsy-sync.log" ] || [ "$(wc -c <"$base/.devopsy-sync.log")" -lt %d ] || { tail -n %d "$base/.devopsy-sync.log" >"$base/.devopsy-sync.log.tmp" && mv -f "$base/.devopsy-sync.log.tmp" "$base/.devopsy-sync.log"; }; } 2>/dev/null || true`, SyncLogMax, SyncLogKeep)

// HolderScript is the child's side of --sync: under the child's release
// lock, in its current release, it prepares, prints the marker, waits for
// go or abort, finishes and, with run, runs the run step of the release
// (its own config's release:, --release-steps release run: what is
// deployed decides, never the checkout). o says where from (From, the
// parent's address).
func HolderScript(t *Target, projectName string, o *SyncOptions, run bool) string {
	s := "set -eu\n" + NoExit255 + basePrelude(t) + fmt.Sprintf(`if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  devopsy --say error "$base is a plain devopsy directory, not managed with releases: nothing to sync into"
  exit 1
fi
[ -d "$base/current/.devopsy" ] || { devopsy --say error "$base has no current release: run 'devopsy @%s --release' first: it syncs on its own, or --release-no-sync releases without the data and --sync later fills it"; exit 1; }
`, hintAddress(t)) + watchSession(true, true) + releaseLock(false, false) + `# Armed only while it waits for the lock, when nothing changed yet: a
# session gone then ends it; from here on it restores on its own. With a
# heartbeat, disarmed only by the session's next byte (within a beat): a
# session that went silent while this waited ends it too, at its quiet
# time, never noticed only once the services are stopped.
if [ -n "${` + HeartbeatEnv + `:-}" ]; then
  mv "$ws_armed" "$ws_armed.next" 2>/dev/null || { devopsy --say error "the session ended while this waited for the release lock: nothing changed"; exit 1; }
  while [ -e "$ws_armed.next" ]; do sleep 0.2; done
  [ -e "$ws_armed.off" ] || { devopsy --say error "the session ended while this waited for the release lock: nothing changed"; exit 1; }
else
  mv "$ws_armed" "$ws_armed.off" 2>/dev/null || { devopsy --say error "the session ended while this waited for the release lock: nothing changed"; exit 1; }
fi
# A holder its watcher ended while it waited leaves its .fired.
rm -f "$ws_armed.off" "$base"/.devopsy-watch.*.fired
# A child's first release failing while this waited takes current away.
[ -d "$base/current/.devopsy" ] || { devopsy --say error "$base has no current release any more: its release failed while this sync waited"; exit 1; }
id=$(readlink "$base/current"); id=${id#releases/}; id=${id%/` + CodeDir + `}
exec 1>&2
devopsy --say info "syncing into $id"
` + o.exports(t) + `cd "$base/current"
status=0
` + syncBlock(projectName, "the run step does not run", false) + `[ "$status" = 0 ] || exit "$status"
`
	if run {
		// The run step goes on whatever happens to the session, after
		// the copy: --log shows the copy's lines in their place.
		s += `out="$base/releases/$id/` + ServerLogFile + `"
trap '' PIPE
` + followOutput
		s += "with_tty " + Quote(devopsyCall(projectName, []string{"--release-steps", "release", "run"}, false)) + "\n"
	}
	return s
}

// syncSourceLock takes the locks a read of a live environment's data
// holds (PushScript, ExportScript): its release lock shared (a release
// waits) and its sync lock exclusively (each stops and restores the
// environment's stop=true services, so two at once would restore them
// under the other).
func syncSourceLock() string {
	return releaseLock(true, false) + `exec 8>"$base/` + SyncLockFile + `"
` + lockWait(8, false, "another sync from $base") + "\n"
}

// PushScript is the parent's side of a sync: under a shared hold of the
// parent's release lock (a release of the parent waits) and an exclusive
// one on its sync lock (other syncs from it wait: each push stops and
// restores the parent's services from their state, so two at once would
// restore them under the other's copy), devopsy --sync-push
// in its current release, with env (the DEVOPSY_SYNC_* variables).
func PushScript(p *Target, projectName string, env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	exports := ""
	for _, k := range keys {
		exports += " " + k + "=" + Quote(env[k])
	}
	return "set -eu\n" + basePrelude(p) + `[ -d "$base/current/.devopsy" ] || { devopsy --say error "the parent $base has no current release"; exit 1; }
` + syncSourceLock() + `if [ -e "$base/` + SyncIncompleteFile + `" ] || [ -e "$base/` + SyncStartedFile + `" ] || [ -e "$base/` + SyncTakenFile + `" ]; then
  devopsy --say error "the parent $base holds data from a sync that did not complete: sync it first (devopsy @` + hintAddress(p) + ` --sync)"
  exit 1
fi
cd "$base/current"
export` + exports + "\n" + devopsyCall(projectName, []string{"--sync-push"}, true) + "\n"
}

// ExportScript writes service's data to stdout (devopsy --export), in the
// target's current release under a shared hold of its release lock (a
// release waits, as for a sync's push) and an exclusive one on its sync
// lock: a push from this environment stops and restores the same
// services a stop=true export does.
func ExportScript(t *Target, projectName, service string) string {
	return "set -eu\n" + basePrelude(t) + `[ -d "$base/current/.devopsy" ] || { devopsy --say error "$base has no current release: nothing to export"; exit 1; }
` + syncSourceLock() + `cd "$base/current"
` + devopsyCall(projectName, []string{"--export", service}, true) + "\n"
}

// ImportEndEnv carries the line devopsy @<target> --import appends to the
// data once its own stdin ended cleanly: the server's devopsy --import
// requires it at the end of what it read, and takes it off. A session cut
// halfway reaches the server as a plain EOF, which alone would pass for
// complete data.
const ImportEndEnv = "DEVOPSY_INTERNAL_IMPORT_END"

// ImportScript replaces service's data with stdin (devopsy --import), in
// the target's current release under its release lock, as a sync holds it.
// end is what the data must end with (ImportEndEnv).
func ImportScript(t *Target, projectName, service, end string) string {
	return "set -eu\n" + basePrelude(t) + fmt.Sprintf(`[ -d "$base/current/.devopsy" ] || { devopsy --say error "$base has no current release: run 'devopsy @%s --release' first"; exit 1; }
`, hintAddress(t)) + `` + releaseLock(false, false) + `cd "$base/current"
# A sync's holder killed outright left its record: its writers end first.
if [ -e "$base/` + SyncStartedFile + `" ] || [ -e "$base/` + SyncTakenFile + `" ]; then
  ` + devopsyCall(projectName, []string{"--sync-services", "settle"}, false) + ` || true
fi
export ` + ImportEndEnv + "=" + Quote(end) + "\n" + devopsyCall(projectName, []string{"--import", service, "--yes"}, true) + "\n"
}

// CurrentExistsScript prints "current" when the target has a current
// release, else "none": whether a release is its first, and whether a
// parent was released. Then "incomplete" when a sync left it with a
// partial copy (SyncIncompleteFile, or a sync's files no holder
// settled): a release that does not sync refuses before uploading.
func CurrentExistsScript(t *Target) string {
	return "set -eu\n" + basePrelude(t) + `if [ -d "$base/current/.devopsy" ]; then echo current; else echo none; fi
` + IncompleteCheck + `
`
}

// ClaimWriteScript claims the right to write into the child at base for
// sync id, before the first write (its SyncOpenFile renamed to
// SyncStartedFile), or fails: its holder decided already.
func ClaimWriteScript(base, id string) string {
	open, started := Quote(path.Join(base, SyncOpenFile(id))), Quote(path.Join(base, SyncStartedFile))
	mine := `[ "$(head -n 1 ` + started + ` 2>/dev/null | cut -d' ' -f2)" = ` + Quote(id) + ` ]`
	return "mv -f " + open + " " + started + " 2>/dev/null || " + mine + " || { devopsy --say error \"the child's side ended already (" + base + "): nothing copied\"; exit 1; }\n"
}

// ParseCurrent reads CurrentExistsScript's output.
func ParseCurrent(out string) (current, incomplete bool) {
	lines := strings.Fields(out)
	return slices.Contains(lines, "current"), slices.Contains(lines, "incomplete")
}

// IncompleteCheck prints "incomplete" when the environment's data is a
// partial copy, $base set.
const IncompleteCheck = `if [ -e "$base/` + SyncIncompleteFile + `" ]; then echo incomplete; elif { [ -e "$base/` + SyncStartedFile + `" ] || [ -e "$base/` + SyncTakenFile + `" ]; } && ! ` + LockBusy + `; then echo incomplete; fi`

// SyncRunning succeeds while a sync's holder holds the child's release
// lock ($base set), its prepare included.
const SyncRunning = `{ [ -e "$base/` + SyncHoldingFile + `" ] && ` + LockBusy + `; }`

// SourceBusy succeeds while a sync from the environment (its push) or an
// export of it holds its sync lock ($base set).
const SourceBusy = `{ [ -e "$base/` + SyncLockFile + `" ] && ! flock -n "$base/` + SyncLockFile + `" true; } 2>/dev/null`

// LockBusy succeeds while something holds the environment's release
// lock ($base set): a sync's files are its own then, not leftovers.
const LockBusy = `{ [ -e "$base/.lock" ] && ! flock -n -s "$base/.lock" true; } 2>/dev/null`

// buildUtilImage builds UtilImage when the host lacks it.
func buildUtilImage() string {
	return "docker image inspect " + Quote(UtilImage) + " >/dev/null 2>&1 || { devopsy --say info \"building " + UtilImage + "\"; printf '%s\\n' " + quoteAll(UtilDockerfile) + " | docker build -q -t " + Quote(UtilImage) + " - >/dev/null; }\n"
}

// RsyncVanished is rsync's exit code when files vanished between its scan
// and their copy: what a live service does to its own temporary files.
// The copy is complete without them, so both paths say so and go on.
const RsyncVanished = 24

// RsyncVanishedNotice follows "rsync mnt/<entry>: " on both paths.
const RsyncVanishedNotice = "some files vanished during the copy (a running service removed them), the rest is copied"

// RsyncModule is the daemon's module for an entry of mnt: its name, and
// mnt for `.` (all of it).
func RsyncModule(entry string) string {
	if entry == "." {
		return "mnt"
	}
	return entry
}

// SyncReceiveScript runs on the child's server, from the parent's over SSH
// with a reverse forward of port to the parent's rsync daemon: one rsync
// per entry of the child's mnt (toDir is its current .devopsy/, base its
// directory), from the daemon's module of that name (RsyncModule), as
// root in UtilImage, on the host's network to reach the forward.
// SyncStartedFile goes first, and with last (no stream follows)
// a DoneLine once every entry is copied.
//
// Once the holder decided (SyncStartedFile gone, or another sync's),
// nothing may be written: a watchdog removes the copy's container. Why:
// after a silent cut between the servers, the rsync otherwise waits for
// TCP to give up, writing again if the link comes back; sshd sends
// nothing on a quiet forward, and rsync's own --timeout counts its
// keepalives into the forward's local socket.
func SyncReceiveScript(toDir, base, id, port string, dirs, files, args []string, last bool) string {
	started := Quote(path.Join(base, SyncStartedFile))
	script := "set -eu\nmnt=$(cd " + Quote(path.Join(toDir, "mnt")) + " && pwd -P)\n" + buildUtilImage() + ClaimWriteScript(base, id) +
		`rs_name=` + Quote(SyncContainerPrefix+id+"-receive") + `
mine() { [ "$(head -n 1 ` + started + ` 2>/dev/null | cut -d' ' -f2)" = ` + Quote(id) + ` ]; }
( while sleep 1; do
    mine || { docker rm -f "$rs_name"; exit 0; }
  done ) </dev/null >/dev/null 2>&1 &
rs_watch=$!
trap 'kill "$rs_watch" 2>/dev/null || true' EXIT
copy() {
  mine || { devopsy --say error "the child's side decided already: mnt/$1 not copied"; exit 1; }
  devopsy --say info "rsync mnt/$1"
  docker run --rm --name "$rs_name" --network host -v "$mnt:/mnt" ` + Quote(UtilImage) + " rsync -a --delete --numeric-ids $4 " + strings.TrimSpace(quoteAll(args)+` "rsync://127.0.0.1:`+port+`/$2" "/mnt/$3"`) + ` >&2 || {
    [ "$?" = ` + fmt.Sprint(RsyncVanished) + ` ] || { devopsy --say error "mnt/$1 ` + HalfCopiedNotice + `"; exit 1; }
    devopsy --say warning "rsync mnt/$1: ` + RsyncVanishedNotice + `"
  }
}
`
	for _, d := range dirs {
		src, dst, opts := RsyncModule(d)+"/", d+"/", ""
		if slices.Contains(files, d) {
			// A file: its module serves it alone, into mnt, and in place
			// (a container bind-mounts the file's inode).
			src, dst, opts = d+"/"+d, "", "--inplace"
		}
		script += "copy " + Quote(d) + " " + Quote(src) + " " + Quote(dst) + " " + Quote(opts) + "\n"
	}
	if last {
		// Appended to the record, never created: once the holder took it,
		// the copy is not this sync's to complete. (A take between the
		// check and the append leaves a record of this line alone, which
		// the next lock-taker settles.)
		script += "mine && printf '%s\\n' " + Quote(DoneLine(id)) + " >>" + started + " || { devopsy --say error \"the child's side decided already: the copy does not count\"; exit 1; }\n"
	}
	return script
}

// SyncExecScript runs devopsy args in the child's current release (toDir
// is its .devopsy/), as its compose project, with env exported (the sync's
// addresses and arguments, for the child's import): --sync-exec across
// servers.
func SyncExecScript(toDir, toProject string, env map[string]string, args []string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	exports := ""
	for _, k := range keys {
		exports += "export " + k + "=" + Quote(env[k]) + "\n"
	}
	return "set -eu\ncd " + Quote(path.Dir(toDir)) + "\n" + exports + devopsyCall(toProject, args, true) + "\n"
}

// BuildUtilImageArgs are docker's arguments to build UtilImage from its
// Dockerfile on stdin.
func BuildUtilImageArgs() []string {
	return []string{"build", "-q", "-t", UtilImage, "-"}
}
