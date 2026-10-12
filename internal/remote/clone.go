package remote

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// A clone copies a parent environment's data (shared/mnt) into a child's,
// host to host: devopsy @<child> --clone, and a child's first release.
// devopsy opens two sessions: the holder, on the child's server, which takes
// the child's release lock, pauses and stops its services and prints
// CloneMarker; then the push, on the parent's server, which copies (devopsy
// --clone-push). The push's result goes back to the holder on its stdin, go
// or abort, and the holder restarts the services and runs the release's run
// step.

// CloneMarker starts the line the holder prints on stdout, followed by the
// child's directory on its server ($base), once the child is ready to be
// written. The only stdout devopsy reads from that session.
const CloneMarker = "devopsy: ready for clone in "

// CloningInto is the line the holder of --clone prints on stderr: the
// release whose log the clone's is appended to.
var CloningInto = regexp.MustCompile(`devopsy: cloning into ([0-9]{14})`)

// KeepAlive are the ssh options of a clone's sessions (the holder, the
// push, and the parent's sessions to the child): a session can stay quiet
// for the whole copy, and a NAT or firewall dropping it would abort the
// clone halfway, or leave a push writing into a child already restored.
var KeepAlive = []string{"-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=4"}

// ChildSSH are the options of the parent's sessions to the child (the
// rsync session and --clone-exec), with KeepAlive: the operator's agent,
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

// Variables of a clone, set for the parent's --clone-push and what it runs
// (the clone capability's push), and DEVOPSY_CLONE_FROM_TARGET and
// DEVOPSY_CLONE_ARGS for the child's prepare and finish.
const (
	// CloneToEnv is the child's server, user@hostname as the parent's
	// server reaches it; empty when both are the same host.
	CloneToEnv = "DEVOPSY_CLONE_TO"
	// CloneSSHPortEnv is the port of CloneToEnv.
	CloneSSHPortEnv = "DEVOPSY_CLONE_SSH_PORT"
	// CloneToBaseEnv is the child's directory on its server, CloneToDirEnv
	// its current release's .devopsy/.
	CloneToBaseEnv = "DEVOPSY_CLONE_TO_BASE"
	CloneToDirEnv  = "DEVOPSY_CLONE_TO_DIR"
	// CloneToProjectEnv is the child's compose project, CloneToTargetEnv
	// its address.
	CloneToProjectEnv = "DEVOPSY_CLONE_TO_PROJECT"
	CloneToTargetEnv  = "DEVOPSY_CLONE_TO_TARGET"
	// CloneArgsEnv are the arguments after --clone's --, space-joined: for
	// rsync in the generic path; a clone capability's push owns them (its
	// own switches, or passed to --clone-rsync after --).
	CloneArgsEnv = "DEVOPSY_CLONE_ARGS"
	// CloneFromTargetEnv is the parent's address.
	CloneFromTargetEnv = "DEVOPSY_CLONE_FROM_TARGET"
	// CloneCustomEnv names the parent's services labeled
	// devopsy.clone=custom, space-separated, for the clone capability's push.
	CloneCustomEnv = "DEVOPSY_CLONE_CUSTOM"
	// CloneWatchEnv tells --clone-push that its stdin stays open for as
	// long as devopsy's push session lives: EOF means the session is gone,
	// and the push stops.
	CloneWatchEnv = "DEVOPSY_CLONE_WATCH"
)

// CloneOptions make ActivateScript clone the parent's data into the new
// release before its run step.
type CloneOptions struct {
	// From is the parent's address.
	From string
	// Args go to rsync (DEVOPSY_CLONE_ARGS).
	Args []string
}

// exports sets the child side's clone variables.
func (o *CloneOptions) exports() string {
	return fmt.Sprintf("export %s=%s %s=%s\n", CloneFromTargetEnv, Quote(o.From), CloneArgsEnv, Quote(strings.Join(o.Args, " ")))
}

// cloneStdin keeps devopsy's go or abort on fd 3 and gives every command
// /dev/null as stdin: none may read the answer, or wait on it, before the
// marker.
const cloneStdin = "exec 3<&0 </dev/null\n"

// cloneFinish defines clone_finish go|abort, in the child's current
// release: --clone-services finish (start what was stopped, wait for it to
// be healthy, unpause), then the clone capability's finish hook when the
// project has one. Both run whatever the other did; it fails if either
// did.
func cloneFinish(projectName string) string {
	return `clone_finish() {
  finished=0
  ` + devopsyCall(projectName, []string{"--clone-services", "finish"}, false) + ` || finished=$?
  if [ -x .devopsy/capabilities/clone/finish ]; then
    echo "devopsy: running 'devopsy --capability clone finish $1'" >&2
    ` + devopsyCall(projectName, []string{"--capability", "clone", "finish"}, false) + ` "$1" || finished=$?
  fi
  return "$finished"
}
`
}

// clonePrepare readies the child's current release to be written: the
// clone capability's prepare hook when the project has one, then
// --clone-services prepare (flush, pause, stop). A failure sets $status
// and restores what they changed.
//
// From here until the services are restored, SIGPIPE is ignored
// (cloneRestorePipe ends that): if devopsy goes away, a write to this
// session fails instead of killing what restores them.
func clonePrepare(projectName string) string {
	return `trap '' PIPE
if [ -x .devopsy/capabilities/clone/prepare ]; then
  echo "devopsy: running 'devopsy --capability clone prepare'" >&2
  ` + devopsyCall(projectName, []string{"--capability", "clone", "prepare"}, false) + ` || status=$?
fi
if [ "$status" = 0 ]; then
  ` + devopsyCall(projectName, []string{"--clone-services", "prepare"}, false) + ` || status=$?
fi
if [ "$status" != 0 ]; then
  clone_finish abort || true
  echo "devopsy: preparing the clone failed ($status)" >&2 || true
fi
`
}

// cloneRestorePipe ends clonePrepare's SIGPIPE exemption, before the run
// step, which runs as release steps always do.
const cloneRestorePipe = "trap - PIPE\n"

// cloneWait prints the marker and reads devopsy's answer into $reply.
// Without one, devopsy is gone (interrupted, its connection lost): nobody
// reads this session any more, and a write to it would kill what restores
// the services, so the rest goes to a file in $base.
func cloneWait(failed string) string {
	return `  echo "` + CloneMarker + `$base" || true
  reply=
  read -r reply <&3 || true
  if [ -z "$reply" ]; then
    exec >>"$base/.devopsy-clone.log" 2>&1
    echo "devopsy: $(date -u '+%Y-%m-%d %H:%M:%S'): no answer from devopsy: restoring what was paused or stopped" >&2
  fi
  if [ "$reply" != go ]; then
    echo "devopsy: the copy from the parent failed: ` + failed + `" >&2
  fi
`
}

// HolderScript is the child's side of --clone: under the child's release
// lock, in its current release, it prepares, prints the marker, waits for
// go or abort, finishes and runs run (the release's run step). fromTarget is
// the parent's address.
func HolderScript(t *Target, projectName, fromTarget string, args, run []string) string {
	s := "set -eu\n" + basePrelude(t) + fmt.Sprintf(`if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  echo "devopsy: $base is a plain devopsy directory, not managed with releases: nothing to clone into" >&2
  exit 1
fi
[ -d "$base/current/.devopsy" ] || { echo "devopsy: $base has no current release: run 'devopsy @%s --release' first (it clones on its own)" >&2; exit 1; }
`, t.Name) + cloneStdin + `exec 9>"$base/.lock"
flock -w 600 9 || { echo "devopsy: another release is running on $base" >&2; exit 75; }
id=$(readlink "$base/current"); id=${id#releases/}
echo "devopsy: cloning into $id" >&2
` + (&CloneOptions{From: fromTarget, Args: args}).exports() + `cd "$base/current"
status=0
` + cloneFinish(projectName) + clonePrepare(projectName) + `[ "$status" = 0 ] || exit "$status"
` + cloneWait("restoring what was paused or stopped") + `if [ "$reply" != go ]; then
  clone_finish abort || true
  exit 1
fi
clone_finish go || exit "$?"
` + cloneRestorePipe
	if len(run) > 0 {
		s += fmt.Sprintf("echo \"devopsy: running 'devopsy %s' (run)\" >&2\n", strings.Join(run, " ")) + devopsyCall(projectName, run, false) + "\n"
	}
	return s
}

// PushScript is the parent's side of a clone: under a shared hold of the
// parent's release lock (a release of the parent waits, other clones from
// it do not), devopsy --clone-push in its current release, with env (the
// DEVOPSY_CLONE_* variables).
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
flock -s -w 600 9 || { echo "devopsy: a release is running on $base" >&2; exit 75; }
cd "$base/current"
export` + exports + "\n" + devopsyCall(projectName, []string{"--clone-push"}, true) + "\n"
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

// CloneReceiveScript runs on the child's server, from the parent's over SSH
// with a reverse forward of port to the parent's rsync daemon: one rsync
// per entry of the child's mnt (toDir is its current .devopsy/), as root in
// RsyncImage, on the host's network to reach the forward.
func CloneReceiveScript(toDir, port string, dirs, args []string) string {
	return "set -eu\nmnt=$(cd " + Quote(path.Join(toDir, "mnt")) + " && pwd -P)\n" + buildRsyncImage() +
		"for d in " + quoteAll(dirs) + `; do
  echo "devopsy: rsync mnt/$d" >&2
  docker run --rm --network host -v "$mnt:/mnt" ` + Quote(RsyncImage) + " rsync -a --delete --numeric-ids " + strings.TrimSpace(quoteAll(args)+` "rsync://127.0.0.1:`+port+`/mnt/$d/" "/mnt/$d/"`) + ` >&2
done
`
}

// CloneExecScript runs devopsy args in the child's current release (toDir
// is its .devopsy/), as its compose project: --clone-exec across servers.
func CloneExecScript(toDir, toProject string, args []string) string {
	return "set -eu\ncd " + Quote(path.Dir(toDir)) + "\n" + devopsyCall(toProject, args, true) + "\n"
}

// BuildRsyncImageArgs are docker's arguments to build RsyncImage from its
// Dockerfile on stdin.
func BuildRsyncImageArgs() []string {
	return []string{"build", "-q", "-t", RsyncImage, "-"}
}
