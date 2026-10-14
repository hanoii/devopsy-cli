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
// the child's release lock, pauses and stops its services and prints
// SyncMarker; then the push, on the parent's server, which copies (devopsy
// --sync-push). The push's result goes back to the holder on its stdin, go
// or abort, and the holder restarts the services and runs the release's run
// step.

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
// its services paused meanwhile.
var ChildSSH = []string{"-o", "ForwardAgent=no", "-o", "BatchMode=yes", "-o", "ConnectTimeout=30", "-o", "StrictHostKeyChecking=accept-new"}

// RsyncImage is devopsy's rsync image, built on a server from
// RsyncDockerfile when missing. Its tag changes with the Dockerfile.
const RsyncImage = "devopsy-rsync:1"

// RsyncDockerfile is RsyncImage's Dockerfile, one line per element.
var RsyncDockerfile = []string{"FROM alpine:3.22", "RUN apk add --no-cache rsync"}

// Variables of a sync, set for the parent's --sync-push and what it runs
// (the sync capability's push), and DEVOPSY_SYNC_FROM_TARGET and
// DEVOPSY_SYNC_ARGS for the child's prepare and finish.
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
	// rsync in the generic path; a sync capability's push owns them (its
	// own switches, or passed to --sync-rsync after --).
	SyncArgsEnv = "DEVOPSY_SYNC_ARGS"
	// SyncFromTargetEnv is the parent's address.
	SyncFromTargetEnv = "DEVOPSY_SYNC_FROM_TARGET"
	// SyncCustomEnv names the parent's services labeled
	// devopsy.sync=custom, space-separated, for the sync capability's push.
	SyncCustomEnv = "DEVOPSY_SYNC_CUSTOM"
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
}

// exports sets the child side's sync variables.
func (o *SyncOptions) exports() string {
	return fmt.Sprintf("export %s=%s %s=%s\n", SyncFromTargetEnv, Quote(o.From), SyncArgsEnv, Quote(strings.Join(o.Args, " ")))
}

// syncStdin keeps devopsy's go or abort on fd 3 and gives every command
// /dev/null as stdin: none may read the answer, or wait on it, before the
// marker.
const syncStdin = "exec 3<&0 </dev/null\n"

// syncFinish defines sync_finish go|abort, in the child's current
// release: --sync-services finish (start what was stopped, wait for it to
// be healthy, unpause), then the sync capability's finish hook when the
// project has one. Both run whatever the other did; it fails if either
// did.
func syncFinish(projectName string) string {
	return `sync_finish() {
  finished=0
  ` + devopsyCall(projectName, []string{"--sync-services", "finish"}, false) + ` || finished=$?
  if [ -x .devopsy/capabilities/sync/finish ]; then
    echo "devopsy: running 'devopsy --capability sync finish $1'" >&2
    ` + devopsyCall(projectName, []string{"--capability", "sync", "finish"}, false) + ` "$1" || finished=$?
  fi
  return "$finished"
}
`
}

// syncPrepare readies the child's current release to be written: the
// sync capability's prepare hook when the project has one, then
// --sync-services prepare (flush, pause, stop). A failure sets $status
// and restores what they changed.
//
// From here until the services are restored, SIGPIPE is ignored
// (syncRestorePipe ends that): if devopsy goes away, a write to this
// session fails instead of killing what restores them.
func syncPrepare(projectName string) string {
	return `trap '' PIPE
if [ -x .devopsy/capabilities/sync/prepare ]; then
  echo "devopsy: running 'devopsy --capability sync prepare'" >&2
  ` + devopsyCall(projectName, []string{"--capability", "sync", "prepare"}, false) + ` || status=$?
fi
if [ "$status" = 0 ]; then
  ` + devopsyCall(projectName, []string{"--sync-services", "prepare"}, false) + ` || status=$?
fi
if [ "$status" != 0 ]; then
  sync_finish abort || true
  echo "devopsy: preparing the sync failed ($status)" >&2 || true
fi
`
}

// syncRestorePipe ends syncPrepare's SIGPIPE exemption, before the run
// step, which runs as release steps always do.
const syncRestorePipe = "trap - PIPE\n"

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
    echo "devopsy: $(date -u '+%Y-%m-%d %H:%M:%S'): no answer from devopsy: restoring what was paused or stopped" >&2
  fi
  if [ "$reply" != go ]; then
    echo "devopsy: the copy from the parent failed: ` + failed + `" >&2
  fi
`
}

// HolderScript is the child's side of --sync: under the child's release
// lock, in its current release, it prepares, prints the marker, waits for
// go or abort, finishes and runs run (the release's run step). fromTarget is
// the parent's address.
func HolderScript(t *Target, projectName, fromTarget string, args, run []string) string {
	s := "set -eu\n" + basePrelude(t) + fmt.Sprintf(`if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  echo "devopsy: $base is a plain devopsy directory, not managed with releases: nothing to sync into" >&2
  exit 1
fi
[ -d "$base/current/.devopsy" ] || { echo "devopsy: $base has no current release: run 'devopsy @%s --release' first: it syncs on its own, or --no-sync releases without the data and --sync later fills it" >&2; exit 1; }
`, t.Name) + syncStdin + `exec 9>"$base/.lock"
` + lockWait(9, false, "the release running on $base") + `
id=$(readlink "$base/current"); id=${id#releases/}
echo "devopsy: syncing into $id" >&2
` + (&SyncOptions{From: fromTarget, Args: args}).exports() + `cd "$base/current"
status=0
` + syncFinish(projectName) + syncPrepare(projectName) + `[ "$status" = 0 ] || exit "$status"
` + syncWait("restoring what was paused or stopped") + `if [ "$reply" != go ]; then
  sync_finish abort || true
  exit 1
fi
sync_finish go || exit "$?"
` + syncRestorePipe
	if len(run) > 0 {
		s += fmt.Sprintf("echo \"devopsy: running 'devopsy %s' (run)\" >&2\n", strings.Join(run, " ")) + "with_tty " + Quote(devopsyCall(projectName, run, false)) + "\n"
	}
	return s
}

// PushScript is the parent's side of a sync: under a shared hold of the
// parent's release lock (a release of the parent waits) and an exclusive
// one on its sync lock (other syncs from it wait: each push pauses,
// stops and restores the parent's services from their state, so two at
// once would restore them under the other's copy), devopsy --sync-push
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
exec 9>"$base/.lock"
` + lockWait(9, true, "the release running on $base") + `
exec 8>"$base/.sync-lock"
` + lockWait(8, false, "another sync from $base") + `
cd "$base/current"
export` + exports + "\n" + devopsyCall(projectName, []string{"--sync-push"}, true) + "\n"
}

// CurrentExistsScript prints "current" when the target has a current
// release, else "none": whether a release is its first, and whether a
// parent was released.
func CurrentExistsScript(t *Target) string {
	return "set -eu\n" + basePrelude(t) + `if [ -d "$base/current/.devopsy" ]; then echo current; else echo none; fi
`
}

// buildRsyncImage builds RsyncImage when the host lacks it.
func buildRsyncImage() string {
	return "docker image inspect " + Quote(RsyncImage) + " >/dev/null 2>&1 || { echo \"devopsy: building " + RsyncImage + "\" >&2; printf '%s\\n' " + quoteAll(RsyncDockerfile) + " | docker build -q -t " + Quote(RsyncImage) + " - >/dev/null; }\n"
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
// daemon's module of that name (RsyncModule), as root in RsyncImage, on
// the host's network to reach the forward.
func SyncReceiveScript(toDir, port string, dirs, files, args []string) string {
	script := "set -eu\nmnt=$(cd " + Quote(path.Join(toDir, "mnt")) + " && pwd -P)\n" + buildRsyncImage() +
		`copy() {
  echo "devopsy: rsync mnt/$1" >&2
  docker run --rm --network host -v "$mnt:/mnt" ` + Quote(RsyncImage) + " rsync -a --delete --numeric-ids $4 " + strings.TrimSpace(quoteAll(args)+` "rsync://127.0.0.1:`+port+`/$2" "/mnt/$3"`) + ` >&2 || {
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
// is its .devopsy/), as its compose project: --sync-exec across servers.
func SyncExecScript(toDir, toProject string, args []string) string {
	return "set -eu\ncd " + Quote(path.Dir(toDir)) + "\n" + devopsyCall(toProject, args, true) + "\n"
}

// BuildRsyncImageArgs are docker's arguments to build RsyncImage from its
// Dockerfile on stdin.
func BuildRsyncImageArgs() []string {
	return []string{"build", "-q", "-t", RsyncImage, "-"}
}
