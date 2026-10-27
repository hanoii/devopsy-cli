package remote

import (
	"strings"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// ReloadingSays starts the line a reload prints once it holds the lock,
// before the release's id: the operator appends its session's log to
// that release's.
const ReloadingSays = "reloading "

// ReloadScript is --reload on t: the current release with its variables
// as they are now and its imports read again, then its reload: step (else
// ReloadDefault, --release-steps reload run), in one session watched as
// an activation is (WatchEnv). Under the release lock, nothing changes
// until everything is checked: a fetch killed outright recovered, a
// killed sync's holder settled, a marked environment refused (only a sync,
// an import or a restore completes its data), then in the current release
// the imports read again into TargetReloadFile, which the checks read in
// place of the release's (TargetPendingEnv): --missing-vars,
// --check-names, --check-mounts and --check-images (a variable that needs
// another image than the release fetched needs a release; a reload never
// builds or pulls). A session gone or a signal until then changes nothing
// (waitBeat). From there on it goes on whatever happens to the session,
// its output in the release's server log (followOutput): the new imports
// in place, the owners given (an owner label may be a variable), the step.
// A step that fails leaves the release live, said, nothing restarted: the
// release did not change.
func ReloadScript(t *Target, projectName string) string {
	mode := t.Mode
	if mode == "" {
		mode = ModeBuild
	}
	call := func(args ...string) string { return devopsyCall(projectName, args, false) }
	refuse := `{ rm -f "$rl_pending"; devopsy --say error "nothing reloaded"; exit 1; }`
	// Nothing current: said before the lock, which refuses a missing
	// $base as one removed while it waited.
	return "set -eu\n" + basePrelude(t) + `[ -L "$base/current" ] || { devopsy --say error "$base has no current release: nothing to reload" "release it first:" "` + report.CmdMark + `devopsy @` + hintAddress(t) + ` --release"; exit 1; }
` + strings.TrimPrefix(prelude(t, false), "set -eu\n"+basePrelude(t)) + "exec 1>&2\n" + NoExit255 + `[ -L "$base/current" ] || { devopsy --say error "$base has no current release: nothing to reload" "release it first:" "` + report.CmdMark + `devopsy @` + hintAddress(t) + ` --release"; exit 1; }
cur=$(readlink "$base/current"); cur=${cur%/` + CodeDir + `}
rel="$base/$cur"
rl_pending="$rel/` + TargetReloadFile + `"
rm -f "$rl_pending"
` + watchSession(false, true) + `# Until the point of no return a signal (the watcher's: the session
# gone) ends it, nothing changed.
trap 'rm -f "$rl_pending"; devopsy --say error "interrupted: nothing reloaded" || true; exit 130' INT TERM HUP
` + releaseImages(projectName, t, mode) + recoverFetch(`"$rel"`) + settleSync(projectName) +
		markRefusal(t, "nothing reloaded", syncFixLines(t), "exit 1") + `devopsy --say info "` + ReloadingSays + `${cur#releases/}"
cd "$base/current"
` + call("--prepare-release", "reload") + ` || ` + refuse + `
export ` + TargetPendingEnv + `="$rl_pending"
` + call("--missing-vars") + ` || ` + refuse + `
` + call("--check-names") + ` "$base" || ` + refuse + `
` + call("--check-mounts") + ` || ` + refuse + `
` + call("--check-images", hintAddress(t)) + ` || ` + refuse + `
if [ -n "${watch_pid:-}" ]; then
` + waitBeat(`{ rm -f "$rl_pending"; devopsy --say error "the session ended: nothing reloaded"; exit 1; }`) + `fi
trap '' INT TERM HUP PIPE
# From here it goes on whatever happens to the session: the output to
# the release's server log, which the session follows.
out="$rel/` + ServerLogFile + `"
` + followOutput + `mv -f "$rl_pending" "$rel/` + TargetEnvFile + `"
unset ` + TargetPendingEnv + `
` + call("--mnt-owners") + ` || { devopsy --say error "giving the entries of mnt to their owners failed: the reload step does not run" "devopsy @` + hintAddress(t) + ` --log shows it"; exit 1; }
rs=0
with_tty ` + Quote(call("--release-steps", "reload", "run")) + ` || rs=$?
img ran "$base" "$rel" || true
[ "$rs" = 0 ] || devopsy --say error "the reload step failed ($rs): the release stays live, and what it started runs" "` + report.CmdMark + `devopsy @` + hintAddress(t) + ` --log" || true
exit "$rs"
`
}

// checkImages refuses (exit 1, saying what) a script whose run step
// would start the current release's services on variables that now need
// another image than it fetched (--check-images): a sync's holder, a
// --no-code restore. env is variables for the check alone (the pending
// files standing in for what the step will read); nothing without run.
func checkImages(t *Target, projectName string, run bool, env, nothing string, flags ...string) string {
	if !run {
		return ""
	}
	return `(cd "$base/current" && ` + env + devopsyCall(projectName, append(append([]string{"--check-images"}, flags...), hintAddress(t)), false) + `) || { devopsy --say error "` + nothing + `"; exit 1; }
`
}
