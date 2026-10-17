package remote

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
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

// SyncMarker starts the line the holder prints on stdout, followed by the
// child's directory on its server ($base), once the child is ready to be
// written. The only stdout devopsy reads from that session.
const SyncMarker = "devopsy: ready for sync in "

// SyncingInto is the line the holder of --sync prints on stderr: the
// release whose log the sync's is appended to.
var SyncingInto = regexp.MustCompile(`devopsy: syncing into ([0-9]{14})`)

// KeepAlive are the ssh options of a sync's sessions (the holder, the
// push, and the parent's sessions to the child): a session can stay quiet
// for the whole copy, and a NAT or firewall dropping it would abort the
// sync halfway, or leave a push writing into a child already restored.
var KeepAlive = []string{"-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=4"}

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

// StreamSpool is service's spool file in the environment directory base.
func StreamSpool(base, service string) string {
	return path.Join(base, StreamSpoolPrefix+service)
}

// SyncInheritPending is a re-sync's inherited variables, in the child's
// environment directory ($base): written before the holder prepares, read
// in place of shared/inherit.env by everything the sync runs on the child
// (InheritPendingEnv), moved over it on go and removed on abort, so a
// failed copy leaves the child's variables as they were.
const SyncInheritPending = ".devopsy-sync.inherit.env"

// SyncInheritPath is SyncInheritPending in the environment directory base.
func SyncInheritPath(base string) string {
	return path.Join(base, SyncInheritPending)
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
	// SyncInheritPending. Without, one left by a holder that died is
	// stale and goes.
	Inherit bool
}

// exports sets the child side's sync variables, the pending inherited
// variables' file among them (removed when stale); t is the child.
func (o *SyncOptions) exports(t *Target) string {
	s := fmt.Sprintf("export %s=%s %s=%s %s=%s\n", SyncFromTargetEnv, Quote(o.From), SyncToTargetEnv, Quote(t.Address), SyncArgsEnv, Quote(strings.Join(o.Args, " ")))
	s += "export " + InheritPendingEnv + "=\"$base\"/" + SyncInheritPending + "\n"
	if !o.Inherit {
		s += "rm -f \"$" + InheritPendingEnv + "\"\n"
	}
	return s
}

// syncStdin keeps devopsy's go or abort on fd 3 and gives every command
// /dev/null as stdin: none may read the answer, or wait on it, before the
// marker.
const syncStdin = "exec 3<&0 </dev/null\n"

// syncFinish defines sync_finish go|abort, in the child's current
// release: on go, the pending inherited variables move into
// shared/inherit.env (which every release links), so what follows reads
// them there; on abort they go. Then --sync-services
// finish go|abort, which starts what was stopped and waits for it to be
// healthy, removes the stream spools on abort, then runs the sync
// capability's finish child go|abort.
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
// From here until the services are restored, SIGPIPE is ignored
// (syncRestorePipe ends that): if devopsy goes away, a write to this
// session fails instead of killing what restores them.
func syncPrepare(projectName string) string {
	return `trap '' PIPE
` + devopsyCall(projectName, []string{"--sync-services", "prepare"}, false) + ` || status=$?
if [ "$status" != 0 ]; then
  sync_finish abort || true
  echo "devopsy: preparing the sync failed ($status)" >&2 || true
fi
`
}

// syncBlock is a sync's part on the child, in its current release with
// $status 0, the sync's variables exported (SyncOptions.exports): prepare
// (services stopped), the marker, devopsy's go or abort, finish; $status
// says how it went. failed says what happens without an answer.
func syncBlock(projectName, failed string) string {
	return syncFinish(projectName) + syncPrepare(projectName) + `if [ "$status" = 0 ]; then
` + syncWait(failed) + `  if [ "$reply" != go ]; then
    sync_finish abort || true
    status=1
  else
    sync_finish go || status=$?
  fi
fi
` + syncRestorePipe
}

// syncRestorePipe ends syncPrepare's SIGPIPE exemption, before the run
// step, which runs as release steps always do, and the sync's variables
// (SyncOptions.exports): the run and after steps are no sync's.
const syncRestorePipe = "trap - PIPE\nunset " + SyncFromTargetEnv + " " + SyncToTargetEnv + " " + SyncArgsEnv + " " + InheritPendingEnv + "\n"

// syncWait prints the marker and reads devopsy's answer into $reply.
// Without one, devopsy is gone (interrupted, its connection lost): nobody
// reads this session any more, and a write to it would kill what restores
// the services, so the rest goes to a file in $base.
func syncWait(failed string) string {
	return `  echo "` + SyncMarker + `$base" || true
  reply=
  read -r reply <&3 || true
  if [ -z "$reply" ]; then
    exec >>"$base/.devopsy-sync.log" 2>&1
    echo "devopsy: $(date -u '+%Y-%m-%d %H:%M:%S'): no answer from devopsy: restoring what was stopped" >&2
  fi
  if [ "$reply" != go ]; then
    echo "devopsy: the copy from the parent failed: ` + failed + `" >&2
  fi
`
}

// HolderScript is the child's side of --sync: under the child's release
// lock, in its current release, it prepares, prints the marker, waits for
// go or abort, finishes and runs run (the release's run step). o says
// where from (From, the parent's address).
func HolderScript(t *Target, projectName string, o *SyncOptions, run []string) string {
	s := "set -eu\n" + basePrelude(t) + fmt.Sprintf(`if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  echo "devopsy: $base is a plain devopsy directory, not managed with releases: nothing to sync into" >&2
  exit 1
fi
[ -d "$base/current/.devopsy" ] || { echo "devopsy: $base has no current release: run 'devopsy @%s --release' first: it syncs on its own, or --release-no-sync releases without the data and --sync later fills it" >&2; exit 1; }
`, hintAddress(t)) + syncStdin + `` + releaseLock(false, false) + `id=$(readlink "$base/current"); id=${id#releases/}; id=${id%/` + CodeDir + `}
exec 1>&2
echo "devopsy: syncing into $id" >&2
out="$base/releases/$id/` + ServerLogFile + `"
` + followOutput + o.exports(t) + `cd "$base/current"
status=0
` + syncBlock(projectName, "restoring what was stopped") + `[ "$status" = 0 ] || exit "$status"
`
	if len(run) > 0 {
		s += fmt.Sprintf("echo \"devopsy: running 'devopsy %s' (run)\" >&2\n", strings.Join(run, " ")) + "with_tty " + Quote(devopsyCall(projectName, run, false)) + "\n"
	}
	return s
}

// syncSourceLock takes the locks a read of a live environment's data
// holds (PushScript, ExportScript): its release lock shared (a release
// waits) and its sync lock exclusively (each stops and restores the
// environment's stop=true services, so two at once would restore them
// under the other).
func syncSourceLock() string {
	return releaseLock(true, false) + `exec 8>"$base/.sync-lock"
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
	return "set -eu\n" + basePrelude(p) + `[ -d "$base/current/.devopsy" ] || { echo "devopsy: the parent $base has no current release" >&2; exit 1; }
` + syncSourceLock() + `cd "$base/current"
export` + exports + "\n" + devopsyCall(projectName, []string{"--sync-push"}, true) + "\n"
}

// ExportScript writes service's data to stdout (devopsy --export), in the
// target's current release under a shared hold of its release lock (a
// release waits, as for a sync's push) and an exclusive one on its sync
// lock: a push from this environment stops and restores the same
// services a stop=true export does.
func ExportScript(t *Target, projectName, service string) string {
	return "set -eu\n" + basePrelude(t) + `[ -d "$base/current/.devopsy" ] || { echo "devopsy: $base has no current release: nothing to export" >&2; exit 1; }
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
	return "set -eu\n" + basePrelude(t) + fmt.Sprintf(`[ -d "$base/current/.devopsy" ] || { echo "devopsy: $base has no current release: run 'devopsy @%s --release' first" >&2; exit 1; }
`, hintAddress(t)) + `` + releaseLock(false, false) + `cd "$base/current"
export ` + ImportEndEnv + "=" + Quote(end) + "\n" + devopsyCall(projectName, []string{"--import", service, "--yes"}, true) + "\n"
}

// CurrentExistsScript prints "current" when the target has a current
// release, else "none": whether a release is its first, and whether a
// parent was released.
func CurrentExistsScript(t *Target) string {
	return "set -eu\n" + basePrelude(t) + `if [ -d "$base/current/.devopsy" ]; then echo current; else echo none; fi
`
}

// buildUtilImage builds UtilImage when the host lacks it.
func buildUtilImage() string {
	return "docker image inspect " + Quote(UtilImage) + " >/dev/null 2>&1 || { echo \"devopsy: building " + UtilImage + "\" >&2; printf '%s\\n' " + quoteAll(UtilDockerfile) + " | docker build -q -t " + Quote(UtilImage) + " - >/dev/null; }\n"
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
// per entry of the child's mnt (toDir is its current .devopsy/), from the
// daemon's module of that name (RsyncModule), as root in UtilImage, on
// the host's network to reach the forward.
func SyncReceiveScript(toDir, port string, dirs, files, args []string) string {
	script := "set -eu\nmnt=$(cd " + Quote(path.Join(toDir, "mnt")) + " && pwd -P)\n" + buildUtilImage() +
		`copy() {
  echo "devopsy: rsync mnt/$1" >&2
  docker run --rm --network host -v "$mnt:/mnt" ` + Quote(UtilImage) + " rsync -a --delete --numeric-ids $4 " + strings.TrimSpace(quoteAll(args)+` "rsync://127.0.0.1:`+port+`/$2" "/mnt/$3"`) + ` >&2 || {
    [ "$?" = ` + fmt.Sprint(RsyncVanished) + ` ] || { echo "devopsy: mnt/$1 ` + HalfCopiedNotice + `" >&2; exit 1; }
    echo "devopsy: rsync mnt/$1: ` + RsyncVanishedNotice + `" >&2
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
