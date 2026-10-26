// Package remote runs devopsy on a server over SSH: `devopsy @<target> ...`.
//
// A target, in .devopsy/config.yaml, names an SSH destination and an
// environment of the project; its path on the server holds:
//
//	releases/<id>/   one copy of the project per release
//	current          symlink to the live release
//	shared/          kept across releases and linked into each release's
//	                 .devopsy/: .env, mnt/ and anything else put there
//
// Commands always run through `current`, so the paths compose stores in
// containers (bind mounts) stay valid when old releases are pruned.
package remote

import (
	"archive/tar"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// A release's directory, releases/<id>, holds the project as uploaded in
// CodeDir, where current points and the commands run, and devopsy's own
// files beside it, never in the project's tree: a build context sees only
// the project.
const (
	// CodeDir is the project's tree in a release's directory.
	CodeDir = "code"
	// TargetEnvFile is the release's environment, from the target's env.
	TargetEnvFile = "devopsy-target.env"
	// RecordFile describes the release: id, commit, branch, who, mode.
	RecordFile = "devopsy-release.json"
	// LogFile is the session's log of its release, then of each rollback
	// to it and sync into it: what the operator's devopsy said and saw,
	// the server's lines up to the switch, each after its time, saved by
	// the operator (releaseLog). ServerLogFile has the rest; --log joins
	// them.
	LogFile = "devopsy-log-session"
	// ImagesFile lists the images it runs, for its own tags (releaseImages).
	ImagesFile = images.ListFile
	// FailedFile marks a release that never went live or whose run failed.
	FailedFile = "devopsy-failed"
	// FetchingFile marks a new release while it fetches its images: one
	// left behind was interrupted without a signal (a CI session that
	// dropped), and img recover finishes what its script would have.
	FetchingFile = images.FetchingFile
	// RolledBackFile marks the release a rollback left.
	RolledBackFile = images.RolledBackFile
	// PendingFile marks a release from its upload until its switch.
	PendingFile = images.PendingFile
	// ServerLogFile is what the server printed from the switch on, as it
	// printed it, a LogSection line before each activation's part
	// (followOutput): written whatever happens to the session.
	ServerLogFile = "devopsy-log-server"
)

// ReleaseFile is name, one of a release's devopsy files, for the project
// at projectDir (its .devopsy/, through current or not): beside the
// release's code/. "" when projectDir is not under a code/ directory; the
// file may still not exist (a local project in a folder named code).
func ReleaseFile(projectDir, name string) string {
	real, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		return ""
	}
	code := filepath.Dir(real)
	if filepath.Base(code) != CodeDir {
		return ""
	}
	return filepath.Join(filepath.Dir(code), name)
}

// excluded reports whether a path, relative to the project root, stays out of
// a release: server-side state that lives in shared/ instead.
func excluded(rel string) bool {
	rel = filepath.ToSlash(rel)
	switch {
	case rel == ".git" || strings.HasPrefix(rel, ".git/"):
		return true
	case rel == ".devopsy/.env",
		rel == ".devopsy/"+LocalConfigFile,
		rel == ".devopsy/project.env", rel == ".devopsy/instance.env",
		rel == ".devopsy/"+InheritFile,
		rel == ".devopsy/compose.override.yaml",
		rel == ".devopsy/compose.override.yml",
		rel == ".devopsy/mnt" || strings.HasPrefix(rel, ".devopsy/mnt/"),
		// --import's spool: data, maybe secret, never part of a release.
		strings.HasPrefix(rel, ".devopsy/"+StreamSpoolPrefix):
		return true
	}
	return false
}

// Files lists what a release contains, relative to projectRoot (the
// directory that holds .devopsy/). Image mode: .devopsy/ only. Build mode:
// the files git tracks or would track (gitignore applies), with local changes.
func Files(projectRoot, mode string) ([]string, error) {
	var files []string
	if mode == ModeBuild {
		cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		cmd.Dir = projectRoot
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("build mode uploads the files git lists, and %s is in no git repository: nothing changed", projectRoot)
		}
		for _, f := range strings.Split(string(out), "\x00") {
			if f == "" {
				continue
			}
			// Deleted locally but still in the index.
			if _, err := os.Lstat(filepath.Join(projectRoot, f)); err != nil {
				continue
			}
			files = append(files, f)
		}
		// .devopsy/ is part of every release, even if gitignored in part.
		files = append(files, ".devopsy")
	} else {
		files = []string{".devopsy"}
	}

	// Expand directories (only .devopsy here) and filter.
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		err := filepath.WalkDir(filepath.Join(projectRoot, f), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(projectRoot, p)
			if excluded(rel) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() && d.Type()&fs.ModeSymlink == 0 {
				return nil
			}
			if !seen[rel] {
				seen[rel] = true
				out = append(out, rel)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

// Size is the bytes of files' (from Files) regular files: what Pack reports
// to its progress function in all.
func Size(projectRoot string, files []string) (int64, error) {
	var n int64
	for _, rel := range files {
		fi, err := os.Lstat(filepath.Join(projectRoot, rel))
		if err != nil {
			return 0, err
		}
		if fi.Mode().IsRegular() {
			n += fi.Size()
		}
	}
	return n, nil
}

// progressReader calls progress with every read's byte count.
type progressReader struct {
	r        io.Reader
	progress func(int64)
}

func (p progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.progress(int64(n))
	}
	return n, err
}

// Pack writes files (from Files) as a gzipped tar to w, keeping symlinks.
// Modes are git's, whatever the checkout's: 0755 for a file with any
// executable bit, else 0644. Why: a release must not depend on who made
// it, and a checkout's modes can be anything git does not track (ddev's
// Mutagen writes 0600 files to the host): a bind-mounted file a container
// reads as another user (10001) failed then. A mode a project needs is
// its image's (COPY --chmod) or its deploy's.
// extra are generated files, path: content, added after the project's.
// progress, when set, gets the bytes of the project's files as they are
// read: through a pipe, what the reader at its end has taken, up to Size.
func Pack(w io.Writer, projectRoot string, files []string, extra map[string][]byte, progress func(int64)) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, rel := range files {
		full := filepath.Join(projectRoot, rel)
		fi, err := os.Lstat(full)
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(full); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = path.Join(CodeDir, filepath.ToSlash(rel))
		hdr.Uname, hdr.Gname, hdr.Uid, hdr.Gid = "", "", 0, 0
		if fi.Mode().IsRegular() {
			hdr.Mode = 0o644
			if fi.Mode()&0o111 != 0 {
				hdr.Mode = 0o755
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(full)
			if err != nil {
				return err
			}
			var r io.Reader = f
			if progress != nil {
				r = progressReader{f, progress}
			}
			_, err = io.Copy(tw, r)
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := extra[name]
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), ModTime: time.Now()}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(content); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// Record is what a release remembers about where it came from.
type Record struct {
	Mode   string `json:"mode"`
	Time   string `json:"time"`
	By     string `json:"by"`
	Commit string `json:"commit,omitempty"`
	Branch string `json:"branch,omitempty"`
	Dirty  bool   `json:"dirty,omitempty"`
	// Restore is the snapshot a restore made the release from.
	Restore string `json:"restore,omitempty"`
}

// NewRecord describes a release made now from projectRoot.
func NewRecord(projectRoot, mode string, now time.Time) Record {
	r := Record{
		Mode: mode,
		Time: now.UTC().Format(time.RFC3339),
		By:   Whoami(),
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = projectRoot
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	if r.Commit = git("rev-parse", "HEAD"); r.Commit != "" {
		r.Branch = git("rev-parse", "--abbrev-ref", "HEAD")
		r.Dirty = git("status", "--porcelain") != ""
	}
	return r
}

// DirtyOutsideDevopsy reports uncommitted changes in the project outside
// .devopsy/: in image mode they are not released, since the image is the
// commit's.
func DirtyOutsideDevopsy(projectRoot string) bool {
	cmd := exec.Command("git", "status", "--porcelain", "--", ".", ":(exclude).devopsy")
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// Whoami is who releases: user@host, or the CI user.
func Whoami() string {
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("LOGNAME")
	}
	host, _ := os.Hostname()
	if v := os.Getenv("GITLAB_USER_LOGIN"); v != "" {
		user, host = v, "gitlab-ci"
	} else if v := os.Getenv("GITHUB_ACTOR"); v != "" {
		user, host = v, "github-actions"
	}
	return user + "@" + host
}

// JSON encodes a record.
func (r Record) JSON() []byte {
	b, _ := json.Marshal(r)
	return b
}

// Quote quotes s for a POSIX shell.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runningLine is the line before a step runs.
func runningLine(args []string, phase string) string {
	return sayQuoted(report.InfoLevel, "running 'devopsy "+strings.Join(args, " ")+"' ("+phase+")")
}

func quoteAll(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}

// Verbose makes devopsy on the server verbose too (DEVOPSY_VERBOSE, which
// older versions ignore).
var Verbose bool

// Color says the operator's terminal takes colors: sessions' lines are
// colored locally as they arrive (report.Colorize), and ColorEnv is exported into
// every session for what the server can color itself.
var Color bool

// TTY is the operator's terminal size, "<cols> <rows>", when stderr is one
// (whatever the colors), exported into every session as TTYEnv: a session
// without a tty (a sync's) runs the run step on a pseudo-terminal of that
// size, so BuildKit draws its live progress (with_tty, basePrelude).
var TTY string

// TTYEnv carries TTY to the server.
const TTYEnv = "DEVOPSY_INTERNAL_TTY"

// Trace, when set, says each SSH command at debug level (-v), and the
// script it runs at trace level (-vv), with the project's secrets masked.
var Trace *report.Logger

// devopsyCall is the remote devopsy invocation, with the project name fixed
// when compose.yaml has none (the release directory would name it). With
// exec, the shell is replaced by devopsy.
func devopsyCall(projectName string, args []string, exec bool) string {
	env := ""
	if Verbose {
		env = "DEVOPSY_VERBOSE=1 "
	}
	if projectName != "" {
		env += "COMPOSE_PROJECT_NAME=" + Quote(projectName) + " "
	}
	if exec {
		env += "exec "
	}
	return env + "devopsy " + quoteAll(args)
}

// lockWait takes the lock on fd (shared with -s): at once, or saying what
// it waits for, up to 10 minutes, then giving up with 75.
func lockWait(fd int, shared bool, what string) string {
	return lockWaitOr(fd, shared, what, "exit 75")
}

// lockWaitOr is lockWait running timeout when the wait ends: an
// activation's not_live, so a release that waited in vain is failed, not
// left pending.
func lockWaitOr(fd int, shared bool, what, timeout string) string {
	opt := ""
	if shared {
		opt = "-s "
	}
	return fmt.Sprintf(`flock -n %s%d || { devopsy --say info "waiting for %s, up to 10 minutes"; flock %s-w 600 %d || { devopsy --say error "%s did not end in 10 minutes"; %s; }; }`, opt, fd, what, opt, fd, what, timeout)
}

// releaseLock takes the environment's release lock on fd 9 (lockWait,
// shared or exclusive), creating $base first with create; without it, a
// $base gone meanwhile fails. A lock taken on a .lock that --destroy
// removed while it waited is no lock (whoever opens
// the path next locks another file), so it opens the path again.
func releaseLock(shared, create bool) string {
	mk := `[ -d "$base" ] || { devopsy --say error "$base is gone"; exit 1; }`
	if create {
		mk = `mkdir -p "$base"`
	}
	// /dev/fd names the open file on Linux, not on macOS (where -ef is
	// always false): checked on the file just opened, compared after the
	// wait only where it can, so the loop never spins. Holding the lock,
	// no import or sync's holder runs: their holding files are a shell
	// killed outright's (a reboot), which would read as running whenever
	// anything holds the lock.
	return `while :; do
  ` + mk + `
  exec 9>"$base/.lock"
  if [ /dev/fd/9 -ef "$base/.lock" ]; then rl_check=1; else rl_check=; fi
  ` + lockWait(9, shared, "the release running on $base") + `
  if [ -z "$rl_check" ] || [ /dev/fd/9 -ef "$base/.lock" ]; then break; fi
done
if [ -e "$base/` + SyncImportingFile + `" ] || [ -e "$base/` + SyncHoldingFile + `" ]; then rm -f "$base/` + SyncImportingFile + `" "$base/` + SyncHoldingFile + `"; fi
`
}

// removeBase removes $base under its release lock (releaseLock): moved
// aside first, so another session opening $base/.lock while rm -rf runs
// neither locks the going directory nor puts a file in it while rm -rf
// empties it. What rm -rf could not remove goes back to $base, unless a
// new one exists already, so running it again finds it.
const removeBase = `mv "$base" "$base.rm.$$" && { rm -rf "$base.rm.$$" || { [ -e "$base" ] || mv "$base.rm.$$" "$base"; false; }; }`

// ProtocolCheckEnv set to warn turns the refusal of an older server's
// devopsy into a warning, the command going on at the operator's risk
// (a local build ahead of the servers): passed to every session
// (SSHStreams).
const ProtocolCheckEnv = "DEVOPSY_PROTOCOL_CHECK"

// Protocol is what every script needs from the server's devopsy, which
// --release-settings reports (protocol=): raise it only for a change
// every script depends on (the prelude, --say), or a format both sides
// share that no script's text shows; a script calling something new needs
// a feature instead (featureFlags, namedFeatures), so an older server
// still runs the rest.
// Either way it refuses before anything changes instead of failing
// halfway. 1: --images; 2: --watch-session;
// 3: --sync-services finish partial, settle, import <service> last,
// --stdin-guard, --watch-session --heartbeat; 4: a sync's gate tied to its id;
// 5: --say; 6: --sync-services prepare first, --release-steps release, a
// sync's done holding its id, a stream's spool named by it; 7: a sync's
// record of its writers (the started file, taken by the holder, the
// writers ended by finish partial and settle), --release-id, a
// watcher's disarm on the session's next byte (<armed>.next); 8:
// features= in --release-settings.
const Protocol = 8

// basePrelude sets $root, $keep and $max_keep from the server's devopsy
// (--release-settings: its user-level config's releases:), $base to the
// target's directory (its path under $root), and $snaps to where its
// snapshots go, with $snap_max_keep (its snapshots:). The
// server decides where releases live, so a laptop and CI always agree.
func basePrelude(t *Target) string {
	// The detached update check would inherit the scripts' locks (fds 8
	// and 9) and hold them while it asks GitHub. The checks of the
	// server's devopsy say so with a plain echo: it may lack --say.
	return `export DEVOPSY_NO_UPDATE_CHECK=1
` + serverCheck(upgradeRef(t)) + `with_tty() {
  # Past a fetch, script only in a session of its own (tty_detach): it
  # reads the terminal's signals even ignored. Without setsid, plainly.
  if [ -n "${DEVOPSY_INTERNAL_TTY:-}" ] && [ ! -t 1 ] && command -v script >/dev/null 2>&1 && { [ -z "${tty_detach:-}" ] || command -v setsid >/dev/null 2>&1; }; then
    ${tty_detach:+setsid -w} script -qec "stty cols ${DEVOPSY_INTERNAL_TTY%% *} rows ${DEVOPSY_INTERNAL_TTY#* } 2>/dev/null; $1" /dev/null
  else
    eval "$1"
  fi
}
root=$(printf '%s\n' "$settings" | sed -n 's/^root=//p')
keep=$(printf '%s\n' "$settings" | sed -n 's/^keep=//p')
max_keep=$(printf '%s\n' "$settings" | sed -n 's/^max_keep=//p')
base="$root"/` + Quote(t.Path) + `
# Where this environment's snapshots go: the server's snapshot root, by
# the same path, else its own snapshots/.
snap_root=$(printf '%s\n' "$settings" | sed -n 's/^snapshot_root=//p')
snap_max_keep=$(printf '%s\n' "$settings" | sed -n 's/^snapshot_max_keep=//p')
snaps="$base/snapshots"
[ -z "$snap_root" ] || snaps="$snap_root"/` + Quote(t.Path) + `
# The images' rules count a snapshot's bookmarks as the environment's own.
export ` + images.SnapshotsEnv + `="$snaps" ` + images.BaseEnv + `="$base"
`
}

// serverCheck refuses a server's devopsy older than Protocol, or lacking
// a feature the script needs ($devopsy_needs, WithNeeds), before anything
// else, naming @ref in the upgrade hint, and leaves its
// --release-settings in $settings. Its lines are a plain echo: the
// server's devopsy may lack --say.
func serverCheck(ref string) string {
	return `settings=$(devopsy --release-settings) || { ` + echoLine(report.ErrorLevel, "the server's devopsy cannot tell its release settings: upgrade it (devopsy @"+ref+" --upgrade)") + `; exit 1; }
server_protocol=$(printf '%s\n' "$settings" | sed -n 's/^protocol=//p')
if ! [ "$server_protocol" -ge ` + strconv.Itoa(Protocol) + ` ] 2>/dev/null; then
  if [ "${` + ProtocolCheckEnv + `:-}" = warn ]; then
    ` + echoLine(report.WarnLevel, "the server's devopsy speaks protocol ${server_protocol:-unknown}, older than this one's "+strconv.Itoa(Protocol)+": going on anyway ("+ProtocolCheckEnv+"=warn), and what it lacks may fail halfway: upgrade it (devopsy @"+ref+" --upgrade)") + `
  else
    ` + echoLine(report.ErrorLevel, "the server's devopsy is older than this one needs: upgrade it (devopsy @"+ref+" --upgrade), or set "+ProtocolCheckEnv+"=warn to go on at your own risk") + `
    exit 1
  fi
fi
# What this script calls that the server's devopsy lacks (Needs). One
# that reports no features is older than 8, said above already.
server_features=" $(printf '%s\n' "$settings" | sed -n 's/^features=//p') "
missing=
[ "$server_features" = "  " ] || for f in ${` + needsEnv + `:-}; do
  case "$server_features" in *" $f "*) ;; *) missing="$missing${missing:+, }$f" ;; esac
done
if [ -n "$missing" ]; then
  if [ "${` + ProtocolCheckEnv + `:-}" = warn ]; then
    ` + echoLine(report.WarnLevel, "the server's devopsy lacks what this needs ($missing): going on anyway ("+ProtocolCheckEnv+"=warn), and it may fail halfway: upgrade it (devopsy @"+ref+" --upgrade)") + `
  else
    ` + echoLine(report.ErrorLevel, "the server's devopsy lacks what this needs ($missing): upgrade it (devopsy @"+ref+" --upgrade), or set "+ProtocolCheckEnv+"=warn to go on at your own risk") + `
    exit 1
  fi
fi
`
}

// UpgradeHint is the command that upgrades t's server's devopsy.
func UpgradeHint(t *Target) string { return "devopsy @" + upgradeRef(t) + " --upgrade" }

// upgradeRef names t's server for --upgrade, a server's command: the
// alias as typed, else the server alone (<host>:).
func upgradeRef(t *Target) string {
	if t.Alias != "" {
		return t.Alias
	}
	return t.Host + ":"
}

// hintAddress is the address a hint in a script names: the alias as
// typed when it has source: (it releases only from its checkout; one
// without cannot release), else the resolved one, else the server.
func hintAddress(t *Target) string {
	if t.Source == "" {
		return cmp.Or(t.Address, t.Host)
	}
	return cmp.Or(t.Alias, t.Address, t.Host)
}

// prelude adds, for release scripts, the checks, the release lock and
// the helpers they share: --destroy may remove $base meanwhile.
//
// Only the upload creates $base: an activation whose release went while
// it waited (--destroy, a failed first release, --releases remove)
// fails instead of making it again.
func prelude(t *Target, create bool) string {
	return "set -eu\n" + basePrelude(t) + `if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  devopsy --say error "$base is a plain devopsy directory, not managed with releases: release and rollback do not apply"
  exit 1
fi
if [ -d "$base/current" ] && [ ! -L "$base/current" ]; then
  devopsy --say error "$base/current is a directory, not a link to a release" "containers of a failed release kept restarting and Docker recreated their bind mounts under it, as root" "take them down, remove it, then release again:" "` + report.CmdMark + `docker compose -p <project> down" "` + report.CmdMark + `docker run --rm -v \"$base:/env\" alpine rm -rf /env/current"
  exit 1
fi
` + releaseLock(false, create) + `mkdir -p "$base/releases" "$base/shared/mnt"
# An upload cut off leaves its .tmp, none running while the lock is held.
# A watcher's file goes with its script: a sync's holder arms its own
# before it waits for this lock.
rm -rf "$base"/releases/*.tmp
for ws_f in "$base"/.devopsy-watch.*; do
  ws_p=${ws_f##*/.devopsy-watch.}
  [ ! -e "$ws_f" ] || kill -0 "${ws_p%%.*}" 2>/dev/null || rm -f "$ws_f"
done
# make_current releases/<id>: current points at its code.
make_current() { ln -sfn "$1/` + CodeDir + `" "$base/current.new" && mv -Tf "$base/current.new" "$base/current"; }
`
}

// UploadScript extracts a release from stdin, under the release lock,
// and prints its id on stdout, its only output there: chosen by the
// server's devopsy (ReleaseIDFlag), never the operator's clock.
//
// A release of a marked environment that does not sync (syncs false) is
// refused here, under the lock, before LockedMarker: devopsy sends no tar
// and no release is left, with no session of its own to find the mark.
// One that syncs goes on: the sync is the fix (partialGuard refuses the
// rest at activation).
func UploadScript(t *Target, syncs bool) string {
	guard := ""
	if !syncs {
		guard = `if [ -e "$base/` + SyncStartedFile + `" ] || [ -e "$base/` + SyncTakenFile + `" ]; then
  devopsy --say error "@` + t.Address + `'s data is from a sync that did not complete, and nothing runs on it until one does: nothing released" "` + report.CmdMark + `devopsy @` + hintAddress(t) + ` --sync" ` + resetFixLines(t) + `
  exit 1
fi
` + markRefusal(t, "nothing released", `"`+report.CmdMark+`devopsy @`+hintAddress(t)+` --sync" "or --release-sync to release with it" `+resetFixLines(t), "exit 1")
	}
	return prelude(t, true) + guard + lockedGate(false, "") + `id=$(devopsy ` + ReleaseIDFlag + ` "$base/releases")
rel="$base/releases/$id"
[ ! -e "$rel" ] || { devopsy --say error "release $id exists already on $base"; exit 1; }
rm -rf "$rel.tmp"
mkdir -p "$rel.tmp"
# -m: extraction time, not archive times (clock skew warnings; builds use
# content, not times). Guarded: a session gone silently would leave tar
# waiting, the release lock held, until TCP gives up. A failure takes the
# .tmp along: nothing of a cut upload stays.
if ! devopsy ` + StdinGuardFlag + ` | tar -xzmf - -C "$rel.tmp"; then
  rm -rf "$rel.tmp"
  exit 1
fi
# Pending until its activation switches to it: one that never gets there
# (cut, interrupted, its operator gone) is never rolled back to nor kept.
: >"$rel.tmp/` + PendingFile + `"
mv "$rel.tmp" "$rel"
echo "$id"
`
}

// ReleaseIDFlag is the hidden devopsy flag that prints a new release's
// id (NextReleaseID) for the releases in a directory (Protocol 7).
const ReleaseIDFlag = "--release-id"

// ReleaseIDFormat is a release id: a UTC second, 20261019214911.
var ReleaseIDFormat = regexp.MustCompile(`^[0-9]{14}$`)

const releaseIDLayout = "20060102150405"

// NextReleaseID is a new release's id: now's second, or a second past the
// newest of ids when that is not earlier. Why: ids order releases (pruning,
// rollbacks, --releases), so a release is always newer than every one
// there, whether two land in one second or older ids came from another
// clock (an operator's, before the server chose them).
func NextReleaseID(now time.Time, ids []string) string {
	next := now.UTC().Truncate(time.Second)
	for _, id := range ids {
		if !ReleaseIDFormat.MatchString(id) {
			continue
		}
		if t, err := time.Parse(releaseIDLayout, id); err == nil && !t.Before(next) {
			next = t.Add(time.Second)
		}
	}
	return next.Format(releaseIDLayout)
}

// Phases are the remote parts of a release's or rollback's steps, as
// devopsy arguments.
type Phases struct {
	// Before runs in the current release, before anything changes.
	Before [][]string
	// Prepare runs in the new (or restored) release before it goes live.
	Prepare [][]string
	// Run runs once it is current. When it fails, current goes back to
	// the previous release, restarted with its own steps (ReleaseStepsFlag).
	Run []string
	// After runs in the current release once it is live.
	After [][]string
}

// RemotePhases are the remote phases of steps.
func RemotePhases(steps *Steps) Phases {
	p := Phases{Run: StepArgs(steps.Run)}
	for _, c := range Commands(steps.Before, true) {
		p.Before = append(p.Before, StepArgs(c))
	}
	for _, c := range steps.Prepare {
		p.Prepare = append(p.Prepare, StepArgs(c))
	}
	for _, c := range Commands(steps.After, true) {
		p.After = append(p.After, StepArgs(c))
	}
	return p
}

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
// extracted, the data services held on the images that ran on it. Never
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
	// One stream: nothing an activation prints is a result, and on two
	// pipes a line on stdout could reach the log after SwitchMarker on
	// stderr and be cut from it.
	s := prelude(t, false) + "exec 1>&2\n" + NoExit255
	s += watchSession(sync != nil, true)
	// Defined first: the fetch and not_live's callers use them.
	s += releaseImages(projectName, t, t.Mode)
	// get_images <dir> <against>, for the restart after a failed run:
	// under the shared image lock, a release's images restored in place
	// of against's (img restore), fetched again when an image it builds
	// is gone.
	s += `get_images() {
  image_lock shared fetching || return $?
  gi=0
  img restore "$1" "$2" || { [ $? -eq 1 ] && fetch_images "$1" && img restore --fetched "$1" "$2"; } || gi=$?
  image_unlock
  return "$gi"
}
`
	// The release lock is held until a dropped session's fetch ends, so
	// none is still running.
	s += `prev=$(readlink "$base/current" 2>/dev/null || true)
prev=${prev%/` + CodeDir + `}
` + recoverFetch(`${prev:+"$base/$prev"}`)
	switch {
	case restore != nil:
		// A restore goes through a mark (a complete one clears it), as a
		// release that syncs does. Its release: made from the snapshot's
		// copy, pending until the switch, failed if it never goes live.
		snapStep := func(word string, args ...string) string {
			return "(cd \"$base/current\" && " + devopsyCall(projectName, []string{SnapshotStepFlag, word}, false) + " " + strings.Join(args, " ") + ")"
		}
		s += `snap="$snaps"/` + Quote(restore.Snapshot) + `
[ -f "$snap/` + SnapshotManifest + `" ] || { devopsy --say error "$base has no snapshot ` + restore.Snapshot + `"; exit 1; }
[ -L "$base/current" ] || { devopsy --say error "$base has no current release: release it first, then restore"; exit 1; }
` + settleSync(projectName) + `exec 5>"$base/` + SyncLockFile + `"
` + lockWait(5, false, "a sync or an export from $base") + `
` + snapshotLock(false) + `[ -f "$snap/` + SnapshotManifest + `" ] || { devopsy --say error "$base has no snapshot ` + restore.Snapshot + `"; exit 1; }
export ` + ImagesLockEnv + `="$root/` + ImagesLock + `" ` + ImagesLeftEnv + `=` + leftPath(t) + ` ` + ImagesModeEnv + `=` + Quote(cmp.Or(t.Mode, ModeBuild)) + `
export ` + RestoreByEnv + `=` + Quote(restore.By) + map[bool]string{true: " " + RestoreEnvEnv + "=1"}[restore.WithEnv] + `
id=$(devopsy ` + ReleaseIDFlag + ` "$base/releases")
devopsy --say info "restoring snapshot ` + restore.Snapshot + ` as release $id"
` + NotLive + snapStep("make-release", `"$snap"`, `"$base/releases/$id"`) + ` || { rm -rf "$base/releases/$id"; exit 1; }
`
	case rollback:
		s += partialGuard(t, projectName, "exit 1")
		// The newest live release older than current (images.RollbackTarget):
		// never a failed, rolled-back or still fetching one.
		s += `id=$(img rollback-target "$base") || { devopsy --say error "no release before the current one"; exit 1; }
devopsy --say info "rolling back to $id"
` + RollbackNotLive
	default:
		s += "id=" + Quote(id) + "\n" + NotLive
	}
	// On a pseudo-terminal when the operator has one (with_tty), as
	// the fetch and the run step: activations are never -t.
	step := func(phase, dir string, args []string, onFail string) string {
		return runningLine(args, phase) + fmt.Sprintf("(cd %s && with_tty %s) || %s\n", dir, Quote(devopsyCall(projectName, args, false)), onFail)
	}
	s += `rel="$base/releases/$id"
code="$rel/` + CodeDir + `"
[ -d "$code" ] || { devopsy --say error "release $id is gone: removed while it waited for the release lock"; exit 1; }
`
	if sync == nil && !rollback {
		s += partialGuard(t, projectName, "not_live 1")
	}
	if !rollback || restore != nil {
		// Held until the switch: --releases tells this release in
		// progress from one cut off before (ReleasesScript), whatever
		// else holds the release lock.
		s += `if [ -e "$rel/` + PendingFile + `" ]; then
  exec 7<"$rel/` + PendingFile + `"
  flock -w 5 7 || true
fi
`
	}
	// A rollback's steps are the restored release's, read from its own
	// config on the server (ReleaseStepsFlag): the checkout's may have
	// changed since. Its before steps run in the current release, as a
	// release's do. On a pseudo-terminal like a release's (with_tty,
	// which may run them in script's own shell: the release's code
	// exported for that).
	// A restore's are the restored release's rollback:, else its
	// release:, as a restart's are.
	kind := "rollback"
	if restore != nil {
		kind = "restart"
	}
	steps := func(phase string) string {
		return devopsyCall(projectName, []string{"--release-steps", kind, phase}, false)
	}
	if rollback {
		s += `rb_code="$rel/` + CodeDir + `"; export rb_code
(cd "$base/current" && with_tty ` + Quote(steps("before")+` "$rb_code"`) + `) || not_live $?
unset rb_code
`
	} else if len(p.Before) > 0 {
		s += "if [ -n \"$prev\" ]; then\n"
		for _, args := range p.Before {
			s += step("before", `"$base/current"`, args, "not_live $?")
		}
		s += "else\n  devopsy --say warning \"no current release: remote before steps skipped\"\nfi\n"
	}
	s += `# Always linked, so editing the server's .env applies without a new release.
touch "$base/shared/.env"
for f in "$base"/shared/* "$base"/shared/.[!.]*; do
  [ -e "$f" ] || [ -L "$f" ] || continue
  rm -rf "$code/.devopsy/${f##*/}"
  ln -s "$f" "$code/.devopsy/${f##*/}"
done
`
	// inherit.env (a child's, from its parent's inherit:) likewise: --inherit
	// writes it into a running release.
	s += `ln -sfn "$base/shared/` + InheritFile + `" "$code/.devopsy/` + InheritFile + `"
`
	// The project's and the instance's .env, shared by their environments:
	// linked even when missing, so one created later applies too.
	for _, l := range t.Levels {
		s += fmt.Sprintf("ln -sfn \"$root\"/%s/.env \"$code/.devopsy/%s\"\n", Quote(l.Dir), l.Link)
	}
	if sync != nil && sync.Images != "" {
		// The parent's data images, for the check at the end of the fetch
		// (img record) and the take-over once the copy is complete.
		s += sync.imagesExport()
	}
	if sync != nil && sync.Inherit {
		// What the sync inherits this time, in the pending file the
		// operator wrote: the checks before the switch (prepare,
		// --missing-vars, the images) see what the release will run
		// with, as a first release's do in inherit.env.
		s += "export " + InheritPendingEnv + "=\"$base\"/" + Quote(SyncInheritPending(sync.ID)) + "\n"
	}
	if !rollback {
		s += fmt.Sprintf(PrepareScript, devopsyCall(projectName, []string{"--prepare-release"}, false))
	}
	if rollback {
		s += `(cd "$code" && with_tty ` + Quote(steps("prepare")) + `) || not_live $?
`
	}
	for _, args := range p.Prepare {
		s += step("prepare", `"$code"`, args, "not_live $?")
	}
	s += fmt.Sprintf("(cd \"$code\" && %s) || not_live $?\n", devopsyCall(projectName, []string{"--missing-vars"}, false))
	if rollback {
		// What the restored release's owners' step needs after the
		// switch, and only that (its labels refuse nothing): the util
		// image, which may have been pruned since, and no owned entry a
		// link.
		// A restore's names its snapshot: the entries it carries are
		// replaced, their links checked in its tars (check-tars).
		snapArg := ""
		if restore != nil {
			snapArg = ` "$snap"`
		}
		s += "(cd \"$code\" && " + devopsyCall(projectName, []string{"--check-mounts", "restored"}, false) + snapArg + ") || not_live 1\n"
	}
	if restore != nil {
		// The snapshot's tars read and checked whole, before anything
		// stops (devopsy's own, still checked: they sit on disk), in the
		// restored code, whose binds the restored data meets.
		s += "(cd \"$code\" && " + devopsyCall(projectName, []string{SnapshotStepFlag, "check-tars"}, false) + " \"$snap\") || not_live 1\n"
	}
	if !rollback {
		// The checks reading compose's configuration, which fails while a
		// required variable is unset: after the prepare steps that make
		// them (a first release's secrets) and --missing-vars, or they
		// would pass unchecked. Another environment's compose project of
		// the same name, or a network the project names that another
		// project owns already: they would share it (cli.ProjectConflict,
		// cli.NetworkConflicts).
		s += "(cd \"$code\" && " + devopsyCall(projectName, []string{"--check-names"}, false) + " \"$base\") || not_live 1\n"
		// Mounts whose data would not be what the labels say: a writable
		// one in the release's tree, data or an owner with no entry, an
		// entry with two owners, ./mnt whole writable, an owned path
		// through a link (cli.CheckMounts); and the util image the owners'
		// step needs after the switch.
		s += "(cd \"$code\" && " + devopsyCall(projectName, []string{"--check-mounts"}, false) + ") || not_live 1\n"
	}
	// fetch_release: the release's images under the shared lock,
	// undone before the lock goes on a failure or a signal, so no
	// removal comes in between: a new release's own images are
	// bookmarked, so they go with it, and the live release's names come
	// back (img undo; a rollback's target keeps its bookmarks). A
	// signal only sets fetch_sig: the fetch's child gets it too and
	// fails, and the same path undoes it, with signals and SIGPIPE
	// ignored from there on, so neither a second Ctrl-C nor a write to
	// a gone session stops it. A session that drops without a signal
	// (CI's -T) leaves FetchingFile, which the next activation, --destroy
	// and --releases remove recover; a rollback writes none, which
	// would mark a good release failed.
	undo := `img undo "$1" ${prev:+"$base/$prev"}`
	if rollback {
		undo = `img undo --rollback "$1" ${prev:+"$base/$prev"}`
	}
	// A new release: what its pulled names held first (a refused one
	// puts them back), then its fetch and its record (img record: the
	// listing, the bookmarks, devopsy's compose file, and the checks of
	// the pulled data services against their data's history, exit 3).
	// A rollback: its images restored against the release it leaves
	// (img restore), fetched again only when an image it builds is gone
	// (exit 1). A signal during either (img ignores it) skips the fetch.
	fetchStep := `{ [ -z "$fetch_sig" ] && img names "$1" && [ -z "$fetch_sig" ] && fetch_images "$1" && img record "$1" ${prev:+"$base/$prev"}; }`
	if rollback {
		fetchStep = `{ img restore "$1" "$base/$prev" || { [ $? -eq 1 ] && [ -z "$fetch_sig" ] && fetch_images "$1" && img restore --fetched "$1" "$base/$prev"; }; }`
	}
	// A release with snapshots on, replacing a current one, takes the
	// snapshot of what it replaces (part 4 of snapshots): its locks and
	// check before the fetch, its first pass during it, its stopped pass
	// inside the stretch that cancels, leaving the services stopped for
	// the run step to start under the new code, and its tars from the
	// mirror after, detached (release-tar).
	// A restore takes one too, of the data it replaces (its first write
	// turns the snapshot's lines into its own: restore-data).
	releaseSnap := (!rollback || restore != nil) && t.SnapshotsRelease.Enabled
	if releaseSnap {
		s += releaseSnapshotSetup(t, projectName, restore != nil)
	}
	s += `fetch_release() {
  image_lock shared fetching || return $?
  fr=0
` + map[bool]string{true: releaseSnapshotFirstPass(projectName), false: ""}[releaseSnap] + `  ` + fetchStep + ` || fr=$?
` + map[bool]string{true: releaseSnapshotStopped(projectName, restore != nil), false: ""}[releaseSnap] + `
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
    ` + undo + `
  fi
  image_unlock
  return "$fr"
}
fetch_sig=
trap 'fetch_sig=1' INT TERM HUP
trap '' PIPE
`
	if !rollback {
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
  if image_lock shared undoing; then
    ` + strings.ReplaceAll(undo, `"$1"`, `"$rel"`) + `
    image_unlock
  fi
  not_live 1
fi
`
	if restore != nil {
		// The data, once the images are ready and past the point a session
		// gone cancels: before its first write a failure starts the set
		// again; from it the set stays down under a restore mark. Nothing
		// interrupts it, so its output goes to the server's log from here
		// (followOutput's marker tells the operator's devopsy it goes on),
		// and a failure puts the images' names back as the log check does.
		s += "trap '' PIPE\n" + followOutput + `[ -z "${rs_id:-}" ] || export ` + RestoreSnapshotEnv + `="$rs_id"
if ! (cd "$base/current" && ` + devopsyCall(projectName, []string{SnapshotStepFlag, "restore-data"}, false) + ` "$snap" "$rel"); then
  set +e
  if image_lock shared undoing; then
    ` + strings.ReplaceAll(undo, `"$1"`, `"$rel"`) + `
    image_unlock
  fi
  not_live 1
fi
`
	}
	if !rollback || restore != nil {
		s += `rm -f "$rel/` + PendingFile + `"
exec 7<&-
`
	}
	s += `make_current "releases/$id"
`
	if releaseSnap {
		s += `[ -z "${rs_id:-}" ] || (cd "$base/current" && ` + devopsyCall(projectName, []string{SnapshotStepFlag, "release-record", "live"}, false) + ` "$rel") || true
`
	}
	s += `devopsy --say info "current is now $id" || true
`
	// The previous release, back after a failed run, restarts with its own
	// images and its own run step: its config's rollback:, else its
	// release: (ReleaseStepsFlag).
	// The new release's images took the names the previous one runs
	// (build mode's are the compose project's, a floating tag's the
	// registry's): its own back.
	// Not on a partial copy (a sync's that went wrong): the previous
	// release's code would run on it.
	restart := `    if [ -e "$base/` + SyncIncompleteFile + `" ]; then
      if [ "$(cut -f3 "$base/` + SyncIncompleteFile + `" 2>/dev/null)" = ` + IncompleteImport + ` ]; then
        rs_service=$(cut -f2 "$base/` + SyncIncompleteFile + `" 2>/dev/null) || true
        devopsy --say warning "not restarting ${prev#releases/}: the import of $rs_service did not complete, and nothing runs on its data until one does" ` + importFixLines(t, "rs_service") + `
      elif [ "$(cut -f3 "$base/` + SyncIncompleteFile + `" 2>/dev/null)" = ` + IncompleteRestore + ` ]; then
        rs_snapshot=$(cut -f2 "$base/` + SyncIncompleteFile + `" 2>/dev/null) || true
        devopsy --say warning "not restarting ${prev#releases/}: the restore of snapshot $rs_snapshot did not complete, and nothing runs on its data until one does" ` + restoreFixLines(t, "rs_snapshot") + `
      else
        devopsy --say warning "not restarting ${prev#releases/}: the data is from a sync that did not complete, and nothing runs on it until one does: ` + IncompleteWhy + `" "` + report.CmdMark + `devopsy @` + hintAddress(t) + ` --sync" ` + resetFixLines(t) + `
      fi
      exit "$status"
    fi
    get_images "$base/$prev" "$rel" || {
      gi_s=$?
      # Its data services still held on the newest image their data ran,
      # never on what its last activation held (img hold).
      img hold "$base" "$base/$prev" || true
      devopsy --say warning "getting the images of ${prev#releases/} failed ($gi_s): restarting it anyway, its data services on the newest image their data ran"
    }
`
	// Its entries of mnt to their owners, by its own labels: the new
	// release's may have given them to others.
	restart += `    (cd "$base/current" && ` + devopsyCall(projectName, []string{"--mnt-owners", "restored"}, false) + `) || devopsy --say error "giving the entries of mnt to their owners failed ($?): restarting ${prev#releases/} anyway"
`
	restart += `    devopsy --say info "restarting ${prev#releases/} with its own run step"
    (cd "$base/current" && with_tty ` + Quote(devopsyCall(projectName, []string{"--release-steps", "restart", "run"}, false)) + `) || devopsy --say error "restarting ${prev#releases/} failed too ($?): check the site"
`
	if releaseSnap {
		restart += `    [ -z "${rs_id:-}" ] || (cd "$base/current" && ` + devopsyCall(projectName, []string{SnapshotStepFlag, "release-record", "done"}, false) + ` "$rel") || true
`
	}
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
	if rollback {
		ownersArgs = append(ownersArgs, "restored")
	}
	owners := `(cd "$base/current" && ` + devopsyCall(projectName, ownersArgs, false) + `) || { status=$?; failed="giving the entries of mnt to their owners"; }
`
	s += "status=0\nfailed=run\n"
	if sync != nil {
		s += owners + `if [ "$status" = 0 ]; then
` + sync.exports(t) + `cd "$base/current"
` + syncBlock(projectName, "the release does not go live", true) + `cd "$base"
[ "$status" = 0 ] || failed=sync
else
  [ -z "${` + InheritPendingEnv + `:-}" ] || rm -f "$` + InheritPendingEnv + `"
fi
`
	}
	// The server's log starts here, after the switch (signals ignored
	// from before it, and its one write tolerates a gone session) and
	// after a sync, which has its own way through a dropped session
	// (syncWait) and whose copy's lines reach the session's log from the
	// push, so --log shows them in their place. PIPE is ignored again: a
	// sync's end restores it.
	if restore == nil {
		s += "trap '' PIPE\n" + followOutput
	}
	if sync == nil {
		s += owners
	}
	if rollback {
		s += `if [ "$status" = 0 ]; then
  (cd "$base/current" && with_tty ` + Quote(steps("run")) + `) || status=$?
fi
`
	} else {
		s += `if [ "$status" = 0 ]; then
` + runningLine(p.Run, "run") + `  (cd "$base/current" && with_tty ` + Quote(devopsyCall(projectName, p.Run, false)) + `) || status=$?
fi
`
	}
	// What ran on the data, failed or not, before a restart replaces it:
	// the history the next activation's checks and holds go by.
	// A restore whose run step failed keeps its mark, so the previous
	// release is not restarted (the restart skips a marked environment):
	// what the failed step started goes down too, never left running on
	// the restored data (review of 2026-10-25, round 2).
	restoreDown := ""
	if restore != nil {
		restoreDown = firstDown(projectName)
	}
	if releaseSnap {
		// Only once something started them: the run step, else the restart
		// of the release before (below). A cut sync's copy or a failed
		// restore keeps them down, its record saying so.
		s += `[ -z "${rs_id:-}" ] || [ "$status" != 0 ] || (cd "$base/current" && ` + devopsyCall(projectName, []string{SnapshotStepFlag, "release-record", "done"}, false) + ` "$rel") || true
`
	}
	s += `img ran "$base" "$rel" || true
if [ "$status" != 0 ]; then
  touch "$rel/` + FailedFile + `"
` + failureReport(projectName) + restoreDown + `  if [ -n "$prev" ]; then
    make_current "$prev"
    devopsy --say error "$failed failed ($status), current is back to ${prev#releases/}"
` + restart + `  else
` + firstDown(projectName) + `    rm -f "$base/current"
    devopsy --say error "$failed failed ($status), no previous release to go back to"
  fi
  exit "$status"
fi
`
	if restore != nil {
		// Its mark and record go once the run step started the set.
		s += "(cd \"$base/current\" && " + devopsyCall(projectName, []string{SnapshotStepFlag, "restored"}, false) + " \"$snap\") || exit 1\n"
	}
	if rollback && restore == nil {
		// The release it left: never counted toward keep, never rolled back
		// to again, its log kept until pruning takes it.
		s += `: >"$base/$prev/` + RolledBackFile + `"
devopsy --say info "${prev#releases/} is marked rolled back"
`
	}
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
`, t.Keep)
	afterFail := `{ s=$?; devopsy --say error "an after step failed ($s): it stays live"; exit "$s"; }`
	if rollback {
		s += `(cd "$base/current" && with_tty ` + Quote(steps("after")) + `) || ` + afterFail + "\n"
	}
	for _, args := range p.After {
		s += step("after", `"$base/current"`, args, afterFail)
	}
	return s
}

// failureReport says, after a failed run or sync, what the release's
// containers were left as, before anything replaces them: the restart of
// the previous release recreates every container whose image or config
// differs, and a failed first release takes them all down (firstDown),
// so their state, health checks and logs would go with them, and --log
// would only say that something failed. It goes where the script's
// output goes, the server's log from the switch on: one detail line per
// container, then, for each one neither running and healthy nor exited
// 0 (a one-shot init's), its last health checks (Docker keeps five) and
// its last 100 log lines. A restarting container shows its last exit
// code, 0 too when what it runs exits cleanly: still a failure. A function, so set -- only changes its own
// arguments.
func failureReport(projectName string) string {
	ps := devopsyCall(projectName, []string{"--", "--profile", "*", "ps", "-a", "--format", "{{.Name}} {{.Service}} {{.State}} {{.ExitCode}} {{.Health}}"}, false)
	logs := devopsyCall(projectName, []string{"--", "--profile", "*", "logs", "--no-color", "--tail", "100"}, false)
	health := Quote(`{{if .State.Health}}{{range .State.Health.Log}}{{.End.Format "15:04:05"}} exit {{.ExitCode}}: {{json .Output}}{{println}}{{end}}{{end}}`)
	return `  failure_report() {
    fr_ps=$(cd "$base/current" && ` + ps + `) || fr_ps=
    fr_bad= fr_badc=
    set --
    while read -r fr_name fr_svc fr_state fr_code fr_health; do
      [ -n "$fr_svc" ] || continue
      if [ "$fr_state" = running ]; then
        set -- "$@" "$fr_svc: running${fr_health:+, $fr_health}"
        case $fr_health in ''|healthy) continue ;; esac
      else
        set -- "$@" "$fr_svc: $fr_state ($fr_code)"
        case $fr_state:$fr_code in exited:0|created:0) continue ;; esac
      fi
      fr_bad="$fr_bad $fr_svc" fr_badc="$fr_badc $fr_name"
    done <<FR_EOF
$fr_ps
FR_EOF
    [ $# = 0 ] || devopsy --say info "the containers as the failed $failed left them:" "$@" || true
    for fr_c in $fr_badc; do
      fr_h=$(docker inspect -f ` + health + ` "$fr_c" 2>/dev/null) || fr_h=
      [ -z "$fr_h" ] || devopsy --say info "the last health checks of $fr_c:" "$fr_h" || true
    done
    [ -z "$fr_bad" ] || { devopsy --say info "the last logs of$fr_bad:" || true; (cd "$base/current" && ` + logs + ` $fr_bad) || true; }
  }
  failure_report
`
}

// firstDown takes down what a failed first release started (its run step,
// a sync's import), where a later release's restart of the previous one
// would replace it: with nothing current, nothing else would. What they
// were left as went first, into the server's log (failureReport), since
// the down takes it with the containers; volumes and mnt stay, like the
// release's directory. Never a stop: a stopped container with restart:
// always starts again with Docker, and its bind mounts through current,
// gone, would come back as root's directories (prelude refuses a current
// that is one).
func firstDown(projectName string) string {
	return `    (cd "$base/current" && ` + devopsyCall(projectName, []string{"--", "--profile", "*", "down", "--remove-orphans"}, false) + `) || devopsy --say warning "taking down what the release started failed ($?): it may still run"
`
}

// followOutput runs where a script must go on whatever happens to its
// session: right after a release's switch, from where it goes live, or
// fails and brings the previous release back (after the sync, in a
// release that syncs: the sync has its own way through a drop, and its
// copy's lines come through the push), and before a sync's run step. A session
// gone without a signal (CI's -T) kills whatever writes to it next: the
// shell, compose, the switch back halfway. So the output goes to a file
// ($out: ServerLogFile, appended after a LogSection line; the operator's
// devopsy stops logging at SwitchMarker, which is written even with the
// session gone: PIPE is ignored by then), both streams in their order,
// and a tail copies it to the session's stderr: only the tail dies with
// it. A tag (time and pid) on the LogSection line and the marker ties
// this part to the session log's anchor. No tail once a sync found
// devopsy gone (sync_gone: stderr is .devopsy-sync.log by then). The
// tail ends half a second after the shell (the EXIT trap, which keeps the exit status: on a -t session
// the pty hangs up as soon as the shell exits, which cut the last lines,
// and inotify has the tail copy a line in milliseconds), or a second
// after a helper finds the shell gone without its trap (killed): never
// tail's --pid, which, like kill -0, takes a zombie for alive, and
// nothing may reap one. Both are killed with SIGKILL: they start while
// the script ignores INT, TERM and HUP (from the fetch to the run step),
// which they inherit. Neither holds the locks (5, 6, 8, 9: a lock the
// script drops before its run step must go then) nor the sync's answer.
// Without GNU's tail (macOS, in tests), as before.
const followOutput = `if tail --version >/dev/null 2>&1; then
  (umask 077; : >>"$out")
  fo_tag="${fo_tag:-$(date -u '+%Y%m%d%H%M%S').$$}"
  echo "` + LogSection + `$(date -u '+%Y-%m-%d %H:%M:%S') $fo_tag" >>"$out" || true
  if [ -z "${sync_gone:-}" ]; then
    fo_off=$(wc -c <"$out")
    tail -c +$((fo_off + 1)) -s 0.2 -f "$out" </dev/null 3<&- 5>&- 6>&- 8>&- 9>&- >&2 &
    fo_tp=$!
    ( while fo_st=$(sed -n 's/^State:[[:space:]]*//p' /proc/$$/status 2>/dev/null) && [ -n "$fo_st" ] && [ "${fo_st%% *}" != Z ]; do sleep 1; done
      sleep 1; kill -9 $fo_tp ) </dev/null >/dev/null 2>&1 3<&- 5>&- 6>&- 8>&- 9>&- &
    fo_hp=$!
    echo "` + SwitchMarker + ` $fo_tag" >&2 || true
    trap 'fo_s=$?; kill -9 $fo_hp 2>/dev/null || true; sleep 0.5; kill -9 $fo_tp 2>/dev/null || true; wait $fo_tp 2>/dev/null || true; [ "$fo_s" != 255 ] || fo_s=254; exit $fo_s' EXIT
  fi
  exec >>"$out" 2>&1
fi
`

// ImagesAcceptEnv names, space-separated, the services whose image an
// activation or a sync's holder lets through the checks against their
// data's history (img record, sync-check, sync-done): the operator's
// --accept-image.
const ImagesAcceptEnv = "DEVOPSY_INTERNAL_IMAGES_ACCEPT"

// WatchEnv tells an activation that its stdin stays open for as long as
// the operator's devopsy runs (a pipe it keeps), so EOF there means the
// session is gone: activations are always -T, whose drop sends no
// signal.
const WatchEnv = "DEVOPSY_INTERNAL_WATCH"

// WatchSessionFlag is the hidden devopsy flag that watches a session's
// stdin on the server (watchSession).
const WatchSessionFlag = "--watch-session"

// watchSession starts devopsy --watch-session in the background on the
// session's stdin (only fd 4 reaches it: a background job's stdin is
// /dev/null), and gives every command after it /dev/null as stdin, so
// none can eat or wait on what comes in. With answer (a sync), what
// comes in reaches the script's fd 3 through a fifo: devopsy's go or
// abort, EOF when the session is gone. Armed, the session's end before
// the switch sends SIGTERM to the script's group (the shell leads one
// under sshd; else the shell alone): before the fetch the script just
// ends (its release is pending since the upload), during it fetch_sig's
// trap undoes the fetch, at once rather than at the next write, a build
// later. Taking $ws_armed (the watcher takes it before it signals),
// right before the switch, disarms it: from there on a release finishes
// whatever happens to its session (followOutput). A plain release is
// armed when its stdin is the operator's pipe (WatchEnv: always, from
// devopsy; until October 2026 a -t session was armed on its terminal,
// whose hangup reached the shell alone, which ran its trap only once the
// build ended), a syncing one always (its stdin is the answer's pipe), a
// sync's holder never: a SIGTERM would stop it halfway through
// restoring.
func watchSession(answer, armed bool) string {
	args := `"$$"`
	s := `ws_armed="$base/.devopsy-watch.$$"
rm -f "$ws_armed" "$ws_armed.off" "$ws_armed.fired"
`
	if answer {
		// Both ends open before the watcher starts, the read end on fd 3,
		// the write end (5, read-write: it never waits) handed to the
		// watcher and closed here: the watcher is the only writer, so its
		// end is EOF, and no open ever waits for the other side.
		s += `ws_fifo="$base/.devopsy-sync.answer.$$"
rm -f "$ws_fifo"
mkfifo "$ws_fifo"
exec 5<>"$ws_fifo" 3<"$ws_fifo"
rm -f "$ws_fifo"
`
		args += ` --answer-fd 5`
	}
	// With devopsy's heartbeat, a silence is the session gone: a
	// holder's waits longer than the push it answers for.
	beats := QuietBeats
	if answer {
		beats = HolderQuietBeats
	}
	args += ` ${` + HeartbeatEnv + `:+--heartbeat $((` + HeartbeatEnv + ` * ` + strconv.Itoa(beats) + `))}`
	start := `exec 4<&0 </dev/null
devopsy ` + WatchSessionFlag + ` ` + args + `%s <&4 4<&- 3<&- 8>&- 9>&- >/dev/null 2>&1 &
watch_pid=$!
exec 4<&-
`
	if answer {
		start += "exec 5>&-\n"
	}
	cancel := ` --armed "$ws_armed"`
	switch {
	case armed && answer:
		s += `: >"$ws_armed"
` + fmt.Sprintf(start, cancel)
	case armed:
		s += `if [ -n "${` + WatchEnv + `:-}" ]; then
  : >"$ws_armed"
` + fmt.Sprintf(start, cancel) + `fi
`
	default:
		s += fmt.Sprintf(start, "")
	}
	return s
}

// partialGuard refuses a release or rollback (then runs refuse) on a
// partial copy from a sync, which nothing runs on: only a sync completes
// it; or on an import that did not complete, with its own ways out. A
// holder that never decided (killed) left its files, settled first.
func partialGuard(t *Target, projectName, refuse string) string {
	return `if [ -n "$prev" ]; then
` + settleSync(projectName) + `fi
` + markRefusal(t, "nothing changed", `"`+report.CmdMark+`devopsy @`+hintAddress(t)+` --sync" "or --release-sync to release with it" `+resetFixLines(t), refuse)
}

// recoverFetch finishes what a release's fetch killed outright left (img
// recover: the release marked failed, current's own names back, cur the
// current release's directory), under the release lock, before anything
// runs current's images: every script that runs or restores them.
// releaseImages defines img and image_lock.
func recoverFetch(cur string) string {
	return `if ls "$base"/releases/*/` + FetchingFile + ` >/dev/null 2>&1 && image_lock shared restoring; then
  img recover "$base" ` + cur + `
  image_unlock
fi
`
}

// releaseSnapshotSetup is a release's part before its fetch when its
// environment takes release snapshots and something is current: the
// effective keep checked (one would remove the snapshot with its own
// release), the sync and snapshot locks, the check (3: skipped, said),
// the snapshot's id, the server log made (a full disk fails here), and
// not_live's hook, which starts what a stopped pass left stopped.
func releaseSnapshotSetup(t *Target, projectName string, restore bool) string {
	// A restore holds the sync and snapshot locks already, to its end.
	locks := `  exec 5>"$base/` + SyncLockFile + `"
` + lockWaitOr(5, false, "a sync or an export from $base", "not_live 75") + `
  exec 6>"$base/` + SnapshotLockFile + `"
` + lockWaitOr(6, false, "a snapshot of $base", "not_live 75") + `
  rm -f "$base/` + SnapshotHoldingFile + `"
`
	unlock := "    exec 6>&- 5>&-\n"
	if restore {
		locks, unlock = "", "    :\n"
	}
	return `rs_id=
if [ -n "$prev" ]; then
  # A release that syncs runs no partialGuard: a killed holder's files are
  # settled (into a mark) before the check reads it.
` + settleSync(projectName) + `  export ` + snapshotEnv(t, t.SnapshotsManual.Keep) + `
  # The server's image lock, record and mode, for the snapshot's bookmarks
  # and the detached tar's pruning.
  export ` + ImagesLockEnv + `="$root/` + ImagesLock + `" ` + ImagesLeftEnv + `=` + leftPath(t) + ` ` + ImagesModeEnv + `=` + Quote(cmp.Or(t.Mode, ModeBuild)) + `
  rs_k=` + strconv.Itoa(t.Keep) + `
  [ "$rs_k" -gt 0 ] || rs_k=$keep
  [ "$rs_k" -le "$max_keep" ] || rs_k=$max_keep
  if [ "$rs_k" -le 1 ]; then
    devopsy --say error "this server keeps 1 release (releases: keep), and a release snapshot goes with the release it belongs to: keep 2 or more, or turn release snapshots off"
    not_live 1
  fi
` + locks + `  (umask 077; mkdir -p "$snaps")
  rs=0
  (cd "$base/current" && ` + devopsyCall(projectName, []string{SnapshotStepFlag, "release-check"}, false) + `) || rs=$?
  case $rs in
    0) rs_id=$(devopsy ` + ReleaseIDFlag + ` "$snaps") ;;
    3) ;;
    *) not_live 1 ;;
  esac
  if [ -n "$rs_id" ]; then
    # A take from here to its detached tar's end, which keeps fd 6: the
    # holding file says so to --snapshots and --status.
    : >"$base/` + SnapshotHoldingFile + `"
    (umask 077; : >>"$rel/` + ServerLogFile + `") || not_live 1
    not_live_hook() {
      if grep -q '^snapshot ' "$base/` + SyncStateFile + `" 2>/dev/null; then
        (cd "$base/current" && ` + devopsyCall(projectName, []string{"--sync-services", "settle"}, false) + `) || true
      fi
    }
  else
` + unlock + `  fi
fi
`
}

// releaseSnapshotFirstPass starts a release snapshot's first pass (the
// mirror's, when it keeps one) beside the fetch.
func releaseSnapshotFirstPass(projectName string) string {
	return `  rs_pid=
  if [ -n "${rs_id:-}" ]; then
    (cd "$base/current" && ` + devopsyCall(projectName, []string{SnapshotStepFlag, "mirror"}, true) + `) &
    rs_pid=$!
  fi
`
}

// releaseSnapshotStopped, after a release's fetch went through and
// before its disarm (a session gone or a signal still cancels it, the
// services started again through not_live's hook): the first pass waited
// for, the stopped pass, which leaves the services stopped, and the tars
// from the mirror started detached, holding the snapshot lock alone and
// nothing of the session's (its output to the release's server log),
// then the locks this script held for it given up.
func releaseSnapshotStopped(projectName string, restore bool) string {
	unlock := "    exec 6>&- 5>&-\n"
	if restore {
		// A restore keeps them to its data step.
		unlock = ""
	}
	return `  if [ -n "${rs_pid:-}" ]; then
    rs_fp=0
    wait "$rs_pid" || rs_fp=$?
    [ "$fr" -ne 0 ] || fr=$rs_fp
  fi
  if [ -n "${rs_id:-}" ]; then
    if [ "$fr" -eq 0 ] && [ -z "$fetch_sig" ]; then
      (cd "$base/current" && ` + devopsyCall(projectName, []string{SnapshotStepFlag, "release-stopped"}, false) + ` "$rs_id" "$base/$prev" "$1") || fr=$?
      if [ "$fr" -eq 0 ]; then
        rs_tar="cd \"\$base/current\" && COMPOSE_PROJECT_NAME=` + Quote(projectName) + ` exec nice devopsy ` + quoteAll([]string{SnapshotStepFlag, "release-tar"}) + ` \"\$rs_id\" $$"
        if command -v setsid >/dev/null 2>&1; then
          base="$base" rs_id="$rs_id" setsid sh -c "$rs_tar" </dev/null >>"$1/` + ServerLogFile + `" 2>&1 3<&- 5>&- 7<&- 8>&- 9>&- &
        else
          base="$base" rs_id="$rs_id" sh -c "$rs_tar" </dev/null >>"$1/` + ServerLogFile + `" 2>&1 3<&- 5>&- 7<&- 8>&- 9>&- &
        fi
      fi
    fi
` + unlock + `  fi
`
}

// settleSync settles what a sync's holder or a take killed outright left
// (--sync-services settle: a sync's files become its mark, a take's
// stopped services start), under the release lock, in the current
// release, before anything reads the mark.
func settleSync(projectName string) string {
	return `if [ -e "$base/` + SyncStartedFile + `" ] || [ -e "$base/` + SyncTakenFile + `" ] || grep -q '^snapshot ' "$base/` + SyncStateFile + `" 2>/dev/null; then
  (cd "$base/current" && ` + devopsyCall(projectName, []string{"--sync-services", "settle"}, false) + `) || true
fi
`
}

// NoExit255 keeps a script's own exit status off 255, ssh's for a
// connection that failed or was lost (sshLost on the operator's side): a
// step that exits 255 (drush on a PHP error) ends the script with 254.
// followOutput's EXIT trap, which replaces this one, does the same.
const NoExit255 = "trap 'ec=$?; [ \"$ec\" != 255 ] || exit 254' EXIT\n"

// LogSection starts each activation's part of ServerLogFile, before its
// time (UTC, logTime's layout), where --log puts it among the session's
// lines.
const LogSection = "devopsy-log-section "

// SwitchMarker is the line followOutput prints on the session before its
// output goes to ServerLogFile: the operator's devopsy takes it out
// (LineMarker) and records nothing of the session after it, which the
// server's log has.
const SwitchMarker = "devopsy-internal-switch"

// LogAnchor is the session's log's line where it stops at SwitchMarker,
// followed by the marker's tag: --log puts the server's part with that
// tag (its LogSection line's) after it, so neither two machines' clocks
// nor a part whose session log was never saved decide where one goes.
const LogAnchor = report.Prefix + "from here, the server's own log (" + ServerLogFile + "):"

// LogServerPart separates LogReadScript's two logs.
const LogServerPart = "devopsy-internal-server-log"

// ImagesLock is the server's image lock, under its release root: shared
// while a release fetches and bookmarks its images or restores them,
// exclusive while one removes them (releaseImages). Images are the
// server's, shared by every project on it.
const ImagesLock = ".devopsy-images.lock"

// ImagesHolders is the directory where each holder of ImagesLock writes a
// file named after its shell's pid (start time, compose name, what it
// does), so whoever waits can say who holds it and for how long.
const ImagesHolders = ".devopsy-images.holders"

// ImagesLeftFile, in a project's directory, records the names Docker
// refused to remove (images.LeftFile): --releases lists them, and the
// project's removals retry them.
const ImagesLeftFile = images.LeftFile

// leftPath is ImagesLeftFile's path in a script, in t's project's directory
// under the release root.
func leftPath(t *Target) string {
	return `"$root"/` + Quote(strings.SplitN(t.Path, "/", 2)[0]) + `/` + ImagesLeftFile
}

// releaseImages defines the shell functions that give each release its own
// images, for mode (build when empty: only tests build a target without
// one). The rules are the server's
// devopsy's (--images, internal/images: bookmarks, restores, removals,
// the record); the script keeps what needs the shell: the lock, which a
// fetch's children inherit, and fetching on a terminal.
//
//   - img <action> ...: devopsy --images for the environment (its compose
//     name, its project's record).
//   - fetch_images <dir>: builds or pulls a release's images (fetchImages).
//   - image_lock shared|exclusive <doing> [now] and image_unlock: the
//     server's image lock (ImagesLock) on fd 8, always taken after the
//     release lock (fd 9). With now it never waits; otherwise up to 10
//     minutes, naming the holders (image_holders). Returns 75 when it
//     could not take it, $il_busy saying why: removals also need Docker
//     to answer, or every image would look gone. A flock ends with its
//     processes, so a holder that dies frees it; a fetch's children keep
//     it until they end, as they keep the release lock.
func releaseImages(projectName string, t *Target, mode string) string {
	if mode == "" {
		mode = ModeBuild
	}
	fetch := fetchImages(projectName, mode)
	name := t.ComposeName()
	if name == "" {
		name = projectName
	}
	holder := name
	if holder == "" {
		holder = "a release"
	}
	return fetch + `left=` + leftPath(t) + `
il_busy=
img() { ` + devopsyCall(projectName, []string{"--images", "--name", name, "--mode", mode, "--address", hintAddress(t)}, false) + ` --left "$left" ${` + ImagesAcceptEnv + `:+--accept "$` + ImagesAcceptEnv + `"} "$@"; }
image_holders() {
  ih=
  ih_now=$(date +%s)
  for ih_f in "$root/` + ImagesHolders + `"/*; do
    [ -f "$ih_f" ] || continue
    if ! kill -0 "${ih_f##*/}" 2>/dev/null; then
      rm -f "$ih_f"
      continue
    fi
    read -r ih_since ih_who ih_what <"$ih_f" || continue
    ih_s=$((ih_now - ih_since))
    if [ "$ih_s" -ge 60 ]; then ih_t="$((ih_s / 60))m$((ih_s % 60))s"; else ih_t="${ih_s}s"; fi
    ih="${ih:+$ih, }$ih_who ($ih_what, $ih_t)"
  done
  echo "${ih:-another release}"
}
image_lock() {
  il_opt=
  [ "$1" = shared ] && il_opt=-s
  exec 8>"$root/` + ImagesLock + `"
  if ! flock -n $il_opt 8; then
    if [ "${3:-}" = now ]; then
      il_busy="the server's images are in use by $(image_holders)"
      exec 8>&-
      return 75
    fi
    devopsy --say info "waiting for the server's images, held by $(image_holders), up to 10 minutes"
    il_s=0
    flock -w 600 $il_opt 8 || il_s=$?
    if [ "$il_s" -ne 0 ]; then
      il_busy="the server's images are still held by $(image_holders)"
      # Above 128, a signal ended the wait (Ctrl-C), not the 10 minutes.
      [ "$il_s" -gt 128 ] || devopsy --say error "$il_busy after 10 minutes"
      exec 8>&-
      return 75
    fi
  fi
  if [ "$1" = exclusive ] && ! docker version >/dev/null 2>&1; then
    il_busy="Docker is not answering"
    [ "${3:-}" = now ] || devopsy --say warning "$il_busy"
    exec 8>&-
    return 75
  fi
  # Only for messages: a full disk must not fail the lock.
  mkdir -p "$root/` + ImagesHolders + `" 2>/dev/null || true
  printf '%s %s %s\n' "$(date +%s)" ` + Quote(strings.ReplaceAll(holder, " ", "_")) + ` "$2" 2>/dev/null >"$root/` + ImagesHolders + `/$$" || true
}
image_unlock() {
  rm -f "$root/` + ImagesHolders + `/$$" 2>/dev/null || true
  exec 8>&-
}
`
}

// fetchImages defines fetch_images <dir>, which gets a release's images
// before it goes live, by mode: build pulls the services without a build
// and builds the others, refreshing their base images; image pulls every
// service with an image, buildable ones included, so a tag missing from
// the registry fails there. Through `devopsy --`, past project commands
// named pull or build, on a pseudo-terminal (with_tty) for BuildKit's live
// progress. Empty for any other mode.
//
// Either way every profile's services without a build are pulled too
// (ProfilePull): a tool run now and then (`--profile tools run --rm jq`)
// would otherwise keep the tag it pulled first, never its fixes. Pulled
// only: the listing and the bookmarks stay the default services' (`config
// --images`), and a profile's buildable services are neither built nor
// pulled, so a tool nobody pushed never fails a release. In build mode
// that is the same one pull; image mode pulls its default services'
// third-party images twice (a manifest request each, nothing
// downloaded), since Compose cannot pull a profile's non-buildable
// services and the default buildable ones in one call.
func fetchImages(projectName, mode string) string {
	var what, cmd string
	switch mode {
	case ModeBuild:
		what = "pulling and building images (build mode)"
		cmd = devopsyCall(projectName, append([]string{"--"}, ProfilePull...), false) +
			" && " + devopsyCall(projectName, []string{"--", "build", "--pull"}, false)
	case ModeImage:
		what = "pulling images (image mode)"
		cmd = devopsyCall(projectName, []string{"--", "pull"}, false) +
			" && " + devopsyCall(projectName, append([]string{"--"}, ProfilePull...), false)
	default:
		return ""
	}
	// Without devopsy's own compose file (images.NoReleaseComposeEnv): a
	// rollback's or a restart's fetch is of the project's images, never of
	// the holds the release's last activation wrote, which may name an
	// image nothing runs any more (the restore decides the holds after).
	return `fetch_images() {
  devopsy --say info "` + what + `"
  (cd "$1/` + CodeDir + `" && export ` + images.NoReleaseComposeEnv + `=1 && with_tty ` + Quote(cmd) + `)
}
`
}

// ProfilePull is compose's arguments to pull every service without a
// build, every profile's included (fetchImages).
var ProfilePull = []string{"--profile", "*", "pull", "--ignore-buildable"}

// NotLive is not_live, for a new release that does not go live: marked
// failed, so rollbacks skip it and keep never counts it. A first one
// leaves its environment's directory too: --destroy, or --releases
// remove failed, takes it away. $rel and $id are set.
const NotLive = `not_live() {
  ! command -v not_live_hook >/dev/null 2>&1 || not_live_hook
  touch "$rel/` + FailedFile + `"
  devopsy --say error "release $id not made current"
  exit "$1"
}
`

// RollbackNotLive is not_live for a rollback: the release it would restore
// stays as it was.
const RollbackNotLive = `not_live() {
  devopsy --say error "nothing changed: current is still ${prev#releases/}"
  exit "$1"
}
`

// PrepareScript runs devopsy --prepare-release (cli.PrepareRelease) in a
// new release before it becomes current, with not_live defined; %s is the
// call. It checks the project's devopsy.role is free on the host and writes
// its devopsy.import labels into the release's target.env, so rollbacks
// keep what each release had. Only for projects with those labels (outside
// comments).
const PrepareScript = `if grep -qsE '^[^#]*devopsy\.(role|import\.)' "$code"/.devopsy/compose.yaml "$code"/.devopsy/compose.override.y*ml; then
  (cd "$code" && %s) || not_live 1
fi
`

// enter changes to where commands run: the current release, or the path
// itself for a plain devopsy directory (a project maintained in place).
func enter(t *Target) string {
	return basePrelude(t) + fmt.Sprintf(`if [ -d "$base/current" ]; then
  cd "$base/current"
elif [ -d "$base/.devopsy" ]; then
  cd "$base"
else
  devopsy --say error "$base has no release and no .devopsy/ project: run 'devopsy @%s --release' first"
  exit 1
fi
`, hintAddress(t))
}

// RunScript runs args in the current release, or in a plain directory.
func RunScript(t *Target, projectName string, args []string) string {
	// The operator's own arguments, through "$@": never read as the
	// script's needs (Needs), since a server's devopsy that lacks one
	// says so itself.
	return "set -eu\n" + enter(t) + "set -- " + quoteAll(args) + "\n" + devopsyCall(projectName, nil, true) + `"$@"` + "\n"
}

// ShellScript opens the user's login shell where commands run: the current
// release, or the path itself for a plain directory.
func ShellScript(t *Target) string {
	return "set -eu\n" + enter(t) + `exec "${SHELL:-/bin/sh}" -l` + "\n"
}

// RootShellScript opens the user's login shell in the server's release
// root: --shell-host on a server named alone.
func RootShellScript(t *Target) string {
	return "set -eu\n" + basePrelude(t) + `cd "$root"
exec "${SHELL:-/bin/sh}" -l
`
}

// UpgradeScript replaces the server's devopsy, as the deploy user, who owns
// it (devopsy-server installs it so): devopsy --upgrade [version].
func UpgradeScript(args []string) string {
	return "set -eu\nexec " + devopsyCall("", append([]string{"--upgrade"}, args...), false) + "\n"
}

// ReleasesScript prints one line per release: id, current flag, failed flag,
// its images (ImagesFile, space separated) and the record JSON, tab
// separated. First, the project's ImagesLeftFile, each line after left.
func ReleasesScript(t *Target) string {
	return "set -eu\n" + basePrelude(t) + fmt.Sprintf(`if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  devopsy --say error "$base is a plain devopsy directory, without releases"
  exit 1
fi
left=`+leftPath(t)+`
# awk ends every line: a last one cut short of its newline would take
# the first release's.
[ ! -s "$left" ] || awk -v t="$(printf '\t')" '{ print "left" t $0 }' "$left"
[ -d "$base/releases" ] || exit 0
cur=$(readlink "$base/current" 2>/dev/null || true)
syncing=; if `+SyncRunning+`; then syncing=1; fi
ls -1 "$base/releases" | grep -v '\.tmp$' | sort -r | while read -r r; do
  c=-; [ "releases/$r/`+CodeDir+`" = "$cur" ] && c=current
  # A pending release is under way while its activation holds its
  # marker's lock (asked without waiting), whatever else marks it (it
  # is fetching), else it was cut off before its switch.
  p="$base/releases/$r/`+PendingFile+`"
  f=-
  # Through a redirection: flock's own open of a path creates the file,
  # which could bring the marker back on a release going live meanwhile.
  if [ -e "$p" ] && ! { flock -n -s 7; } 2>/dev/null 7<"$p" && [ -e "$p" ]; then
    f=in-progress
  else
    [ ! -e "$p" ] || f=incomplete
    # A release cut off mid-fetch is failed too, recovered or not.
    { [ -e "$base/releases/$r/`+FailedFile+`" ] || [ -e "$base/releases/$r/`+FetchingFile+`" ]; } && f=failed
    [ ! -e "$base/releases/$r/`+RolledBackFile+`" ] || f=rolled-back
    # The current release, while a sync into it runs.
    [ "$c" != current ] || [ -z "$syncing" ] || [ "$f" != - ] || f=syncing
  fi
  # Names, each with its version or short digest (name=version): the
  # ids stay on the server.
  i=
  [ ! -f "$base/releases/$r/%s" ] || i=$(awk -F '\t' '$1 != "" { v = $6; if (v == "" && $5 != "") { v = $5; sub(/.*@/, "", v); v = substr(v, 1, 19) } printf "%%s%%s ", $1, (v == "" ? "" : "=" v) }' "$base/releases/$r/%s")
  printf '%%s\t%%s\t%%s\t%%s\t' "$r" "$c" "$f" "$i"
  [ ! -f "$base/releases/$r/%s" ] || tr -d '\n' < "$base/releases/$r/%s"
  echo
done
`, ImagesFile, ImagesFile, RecordFile, RecordFile)
}

// Release is one line of ReleasesScript's output.
type Release struct {
	ID              string
	Current, Failed bool
	// RolledBack: a rollback left it (images.RolledBackFile).
	RolledBack bool
	// Incomplete: uploaded, never made current, nothing running it now
	// (PendingFile); InProgress: the same while a release runs.
	Incomplete, InProgress bool
	// Syncing: current, while a sync into it runs (SyncRunning).
	Syncing bool
	// Images are the names it runs (ImagesFile), each followed by = and
	// its version or short digest when the listing has one: what a
	// rollback to it restores, and what removing it may free.
	Images []string
	Record Record
}

// ParseReleases reads ReleasesScript's output, newest first.
func ParseReleases(out string) []Release {
	var rs []Release
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 5)
		if len(parts) < 3 || parts[0] == "left" {
			continue
		}
		r := Release{ID: parts[0], Current: parts[1] == "current", Failed: parts[2] == "failed", RolledBack: parts[2] == "rolled-back",
			Incomplete: parts[2] == "incomplete", InProgress: parts[2] == "in-progress", Syncing: parts[2] == "syncing"}
		if len(parts) > 3 {
			r.Images = strings.Fields(parts[3])
		}
		if len(parts) == 5 {
			_ = json.Unmarshal([]byte(parts[4]), &r.Record)
		}
		rs = append(rs, r)
	}
	return rs
}

// Line is the release as --releases lists it: * for the current one, id,
// who, mode, branch@commit, FAILED.
func (r Release) Line() string {
	mark := " "
	if r.Current {
		mark = "*"
	}
	src := r.Record.Mode
	if r.Record.Commit != "" {
		short := r.Record.Commit
		if len(short) > 10 {
			short = short[:10]
		}
		src += " " + r.Record.Branch + "@" + short
		if r.Record.Dirty {
			src += "+dirty"
		}
	}
	if r.Record.Restore != "" {
		src += ", restore of snapshot " + r.Record.Restore
	}
	status := ""
	switch {
	case r.Failed:
		status = "  FAILED"
	case r.RolledBack:
		status = "  ROLLED BACK"
	case r.Incomplete:
		status = "  INCOMPLETE"
	case r.InProgress:
		status = "  IN PROGRESS"
	case r.Syncing:
		status = "  SYNCING"
	}
	return fmt.Sprintf("%s %s  %-24s  %s%s", mark, r.ID, r.Record.By, src, status)
}

// Left is an image a removal could not remove, from ImagesLeftFile.
type Left struct {
	Time            time.Time
	Name, Release   string // the compose name and release that listed it
	Ref, Image, Why string
}

// ParseLeft reads ReleasesScript's left lines.
func ParseLeft(out string) []Left {
	var ls []Left
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 7)
		if len(parts) != 7 || parts[0] != "left" {
			continue
		}
		sec, _ := strconv.ParseInt(parts[1], 10, 64)
		ls = append(ls, Left{Time: time.Unix(sec, 0).UTC(), Name: parts[2], Release: parts[3], Ref: parts[4], Image: parts[5], Why: parts[6]})
	}
	return ls
}

// FormatReleases turns ReleasesScript output into a table, each release's
// images on the line under it, then the project's images left behind.
func FormatReleases(out string) string {
	var b strings.Builder
	for _, r := range ParseReleases(out) {
		b.WriteString(r.Line() + "\n")
		if len(r.Images) > 0 {
			shown := make([]string, len(r.Images))
			for i, img := range r.Images {
				// name=version: the version or short digest the release ran.
				if ref, v, ok := strings.Cut(img, "="); ok {
					img = ref + " (" + v + ")"
				}
				shown[i] = img
			}
			b.WriteString("    " + strings.Join(shown, " ") + "\n")
		}
	}
	if b.Len() == 0 {
		b.WriteString("No releases yet.\n")
	}
	if left := ParseLeft(out); len(left) > 0 {
		b.WriteString("\nImages the project's removals left (Docker refused; each pruning retries):\n")
		for _, l := range left {
			fmt.Fprintf(&b, "  %s  %s, release %s, %s\n    %s\n", l.Ref, l.Name, l.Release, l.Time.Format("2006-01-02 15:04"), l.Why)
		}
	}
	return b.String()
}

// ReleasesRemoveScript removes releases by id, under the release lock and
// the server's image lock (exclusive, waiting up to 10 minutes): a
// fetch cut off is recovered first (img recover), then every id is
// checked again, refusing them all when one is gone or current, then each
// release's images (img remove) and directory go, and img prune
// runs once.
//
// goneOK (the ids came from failed) passes over a release a pruning took
// meanwhile.
func ReleasesRemoveScript(t *Target, projectName string, ids []string, goneOK bool) string {
	flag := ""
	if goneOK {
		flag = "--gone-ok "
	}
	return "set -eu\n" + basePrelude(t) + `if [ ! -d "$base/releases" ]; then
  devopsy --say error "$base has no releases"
  exit 1
fi
` + releaseLock(false, false) + releaseImages(projectName, t, t.Mode) + `prev=$(readlink "$base/current" 2>/dev/null || true)
prev=${prev%/` + CodeDir + `}
image_lock exclusive removing || { devopsy --say error "nothing removed"; exit 75; }
` + lockedGate(true, "image_unlock; ") + `img recover "$base" ${prev:+"$base/$prev"}
# Every id checked first (gone, current: nothing removed), then each with
# its images (images.RemoveReleases).
rr=0
img remove-releases ` + flag + `"$base" ` + quoteAll(ids) + ` || rr=$?
[ "$rr" -ne 0 ] || img prune
image_unlock
exit "$rr"
`
}

// SSH runs a script on the target's host with `sh -c`. DEVOPSY_SSH_COMMAND
// replaces "ssh", like GIT_SSH_COMMAND (for example
// "ssh -i key -o UserKnownHostsFile=known_hosts" in CI). It returns the
// remote exit code.
func SSH(t *Target, script string, stdin io.Reader, stdout io.Writer, tty bool) (int, error) {
	return SSHLog(t, script, stdin, stdout, tty, nil)
}

// SSHOptions go before every session's own arguments, also after
// DEVOPSY_SSH_COMMAND, whose options win: ssh keeps the first value of each.
var SSHOptions = []string{"-o", "ClearAllForwardings=yes"}

// SSHLog is SSH, also copying the session's output, both streams, to log.
func SSHLog(t *Target, script string, stdin io.Reader, stdout io.Writer, tty bool, log io.Writer) (int, error) {
	return SSHWith(t, nil, script, stdin, stdout, tty, log)
}

// SSHWith is SSHLog with extra ssh options after SSHOptions (a sync's push
// session forwards the agent: -o ForwardAgent=yes).
func SSHWith(t *Target, extra []string, script string, stdin io.Reader, stdout io.Writer, tty bool, log io.Writer) (int, error) {
	return SSHContext(context.Background(), t, extra, script, stdin, stdout, tty, log)
}

// SSHContext is SSHWith ended when ctx is done: ssh gets
// SIGTERM, which closes the session, and SIGKILL after SSHKillDelay. Its
// output is cut then too, should something it started still hold it.
func SSHContext(ctx context.Context, t *Target, extra []string, script string, stdin io.Reader, stdout io.Writer, tty bool, log io.Writer) (int, error) {
	return SSHStreams(ctx, t, extra, script, stdin, stdout, nil, tty, log)
}

// SSHStreams is SSHContext with the session's stderr to stderr too, when
// not nil, instead of the terminal's.
func SSHStreams(ctx context.Context, t *Target, extra []string, script string, stdin io.Reader, stdout, stderr io.Writer, tty bool, log io.Writer) (int, error) {
	ttyFlag := "-T"
	if tty {
		ttyFlag = "-t"
	}
	// devopsy's sessions never need the host's port forwardings from
	// ~/.ssh/config: each one would try to bind them again, and warn that
	// they are in use while another session (or the user's own) holds them.
	if Color {
		script = ColorEnv + "=1; export " + ColorEnv + "\n" + script
	}
	if TTY != "" {
		script = TTYEnv + "=" + Quote(TTY) + "; export " + TTYEnv + "\n" + script
	}
	if os.Getenv(ProtocolCheckEnv) == "warn" {
		// Exported: a push checks the child's devopsy the same way
		// (ChildCheck).
		script = "export " + ProtocolCheckEnv + "=warn\n" + script
	}
	script = WithNeeds(script)
	args := append(append(append([]string{}, SSHOptions...), extra...), ttyFlag, t.Host, "sh -c "+Quote(script))
	if Trace != nil {
		opts := strings.Join(append(extra[:len(extra):len(extra)], ttyFlag), " ")
		Trace.Debug(fmt.Sprintf("ssh %s %s", opts, t.Host))
		Trace.Trace("running:\n" + strings.TrimRight(script, "\n"))
	}

	cmd := SSHCommand(args...)
	if ctx.Done() != nil {
		cmd = SSHCommandContext(ctx, args...)
	}
	if ctx.Value(noInterruptKey{}) != nil {
		// ssh keeps an inherited SIG_IGN for SIGINT: a terminal's Ctrl-C
		// reaches devopsy alone, which ends the session in its order.
		if sh, err := exec.LookPath("sh"); err == nil {
			cmd.Args = append([]string{"sh", "-c", `trap '' INT; exec "$@"`, "sh"}, cmd.Args...)
			cmd.Path = sh
		}
	}
	cmd.Stdin = stdin
	if stdin == nil {
		cmd.Stdin = os.Stdin
	}
	// The terminal gets the session's lines colored (Color); a caller's
	// own writer (parsed output) and the log get them as they are.
	var colored []*report.ColorWriter
	term := func(f *os.File) io.Writer {
		// Never a tty session: the user types there, and a held prompt
		// (no newline yet) would lag; the server's tty colors it anyway.
		if !Color || tty {
			return f
		}
		c := report.Colorize(f)
		colored = append(colored, c)
		return c
	}
	cmd.Stdout = stdout
	if stdout == nil {
		cmd.Stdout = term(os.Stdout)
	}
	cmd.Stderr = stderr
	if stderr == nil {
		cmd.Stderr = term(os.Stderr)
	}
	// A logged session (an activation, a sync's holder) may print
	// SwitchMarker: it never shows, and the log stops at it with
	// LogAnchor, where --log puts the server's own log (followOutput).
	// Other sessions stay a plain passthrough: a shell's typing, an
	// export's data.
	var flush []*MarkerWriter
	if log != nil {
		shown := &LineMarker{Prefix: SwitchMarker}
		logged := &LineMarker{Prefix: SwitchMarker, Cut: true, Found: func(tag string) {
			_, _ = log.Write([]byte(LogAnchor + tag + "\n"))
			if f, ok := ctx.Value(switchedKey{}).(*atomic.Bool); ok {
				f.Store(true)
			}
		}}
		flush = []*MarkerWriter{shown.Writer(cmd.Stdout), shown.Writer(cmd.Stderr), logged.Writer(log), logged.Writer(log)}
		cmd.Stdout = io.MultiWriter(flush[0], flush[2])
		cmd.Stderr = io.MultiWriter(flush[1], flush[3])
	}
	// A session whose script asks for a byte (BeatMarker, WithBeat) gets
	// one at once on its stdin: the server's watcher disarms on it, so a
	// live session goes on in a round trip. Taken out of both the
	// terminal's and the log's lines.
	if w, ok := ctx.Value(beatKey{}).(io.Writer); ok {
		lm := &LineMarker{Prefix: BeatMarker, Found: func(string) { _, _ = w.Write([]byte{0}) }}
		mw := lm.Writer(cmd.Stderr)
		flush = append([]*MarkerWriter{mw}, flush...)
		cmd.Stderr = mw
	}
	// A gated session (WithGate) sends stdin only once the script holds
	// the release lock: what is sent while it waits would otherwise
	// reach a script whose operator left meanwhile.
	if ctx.Value(gateKey{}) != nil {
		open := make(chan struct{})
		var once sync.Once
		lm := &LineMarker{Prefix: LockedMarker, Found: func(string) { once.Do(func() { close(open) }) }}
		// Outermost, so flushed first.
		mw := lm.Writer(cmd.Stderr)
		flush = append([]*MarkerWriter{mw}, flush...)
		cmd.Stderr = mw
		// An *os.File, so Wait never waits on a copy that never starts.
		r, w, err := os.Pipe()
		if err != nil {
			return 1, err
		}
		defer r.Close()
		src := cmd.Stdin
		cmd.Stdin = r
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			defer w.Close()
			select {
			case <-open:
				_, _ = io.Copy(w, src)
			case <-stop:
			case <-ctx.Done():
			}
		}()
	}
	err := cmd.Run()
	for _, f := range flush {
		f.Flush()
	}
	for _, c := range colored {
		c.Flush()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		// ssh killed by a signal devopsy did not send (its context's): the
		// connection is gone as for ssh's own 255, and is said the same.
		if ee.ExitCode() == -1 && ctx.Err() == nil {
			return 255, nil
		}
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

type noInterruptKey struct{}

type gateKey struct{}

// WithGate is ctx for a session whose script prints LockedMarker
// (lockedGate) once it holds the release lock: stdin is sent only then.
func WithGate(ctx context.Context) context.Context {
	return context.WithValue(ctx, gateKey{}, true)
}

// LockedMarker is the line a gated script (WithGate) prints on stderr
// once it holds the release lock. Why: a session cut while its script
// waits for the lock (Ctrl-C, a drop) left its data in the server's pipe,
// and the script went on with it once the lock came (a release uploaded
// after devopsy said nothing changed, a removal done); sent only now, it
// never comes, and the script stops on an empty or silent stdin.
const LockedMarker = "devopsy-internal-locked"

// lockedGate prints LockedMarker; with ack it then takes the "go" a
// gated session sends (AckInput), under --stdin-guard's bound, and
// without it runs undo (a lock to give back) and fails. A removal takes
// it once it holds every lock it waits for, the image lock included.
func lockedGate(ack bool, undo string) string {
	s := `printf '%s\n' "` + LockedMarker + `" >&2
`
	if ack {
		s += `lg_ack=$(devopsy ` + StdinGuardFlag + `) && [ "$lg_ack" = go ] || { ` + undo + `devopsy --say error "the session ended while this waited for the locks: nothing changed"; exit 1; }
`
	}
	return s
}

// AckInput is a gated session's stdin when the script takes no data
// (lockedGate with ack).
const AckInput = "go\n"

// InputEnd is the line a gated session's data ends with (GatedInput):
// data that may be empty (no variable to inherit) needs it to tell a
// complete one from a session cut before sending anything.
const InputEnd = "devopsy-internal-end"

// GatedInput is data for gatedRead: its lines, then InputEnd.
func GatedInput(data string) string {
	if data != "" && !strings.HasSuffix(data, "\n") {
		data += "\n"
	}
	return data + InputEnd + "\n"
}

// gatedRead defines gated_read <file>: prints LockedMarker (the lock
// held), then reads stdin into file through --stdin-guard's bound,
// requiring GatedInput's end line, which it takes off; without it (the
// session gone while the script waited) it fails having written
// nothing.
const gatedRead = `gated_read() {
  printf '%s\n' "` + LockedMarker + `" >&2
  if devopsy ` + StdinGuardFlag + ` >"$1.in" && [ "$(tail -n 1 "$1.in")" = ` + InputEnd + ` ]; then
    sed '$d' "$1.in" >"$1"
    rm -f "$1.in"
  else
    rm -f "$1.in"
    devopsy --say error "the session ended while this waited for the release lock: nothing changed"
    exit 1
  fi
}
`

type switchedKey struct{}

type beatKey struct{}

// WithBeat is ctx for a session whose stdin devopsy writes through w, the
// pipe it keeps open with a heartbeat: a BeatMarker line the script
// prints is answered with a byte on w at once.
func WithBeat(ctx context.Context, w io.Writer) context.Context {
	return context.WithValue(ctx, beatKey{}, w)
}

// BeatMarker is the line a script prints on stderr when its watcher waits
// for the session's next byte before it disarms (<armed>.next): past a
// sync holder's lock wait, and right before a release's switch. Why: a
// live session and one that died silently look the same until a byte
// comes, and the next heartbeat would take up to a beat; devopsy answers
// the marker at once, so only a silent session waits, until its quiet
// time ends it as gone. Once per session.
const BeatMarker = "devopsy-internal-beat"

// waitBeat takes the watcher's armed file to <armed>.next, asks for a byte
// (BeatMarker) and waits until the watcher disarms it on the session's next
// byte (.off) or takes it as gone (.fired, its SIGTERM on the way), then
// runs gone in the second case, and when the watcher had fired already.
// Without a heartbeat, the plain disarm. $ws_armed set. A watcher that
// never answers (killed outright) is taken for a session gone once every
// quiet time is past, two beats more: the script takes the file itself,
// so one that wakes late cannot signal it.
func waitBeat(gone string) string {
	return `if [ -n "${` + HeartbeatEnv + `:-}" ]; then
  if mv "$ws_armed" "$ws_armed.next" 2>/dev/null; then
    printf '%s\n' "` + BeatMarker + `" >&2
    ws_n=$(( ${` + HeartbeatEnv + `} * ` + strconv.Itoa(HolderQuietBeats+2) + ` * 5 ))
    while [ -e "$ws_armed.next" ] && [ "$ws_n" -gt 0 ]; do sleep 0.2; ws_n=$((ws_n - 1)); done
    mv "$ws_armed.next" "$ws_armed.fired" 2>/dev/null || true
    [ -e "$ws_armed.off" ] || ` + gone + `
  else
    ` + gone + `
  fi
elif ! mv "$ws_armed" "$ws_armed.off" 2>/dev/null; then
  ` + gone + `
fi
`
}

// WithSwitched is ctx for a logged session that sets switched once the
// script printed SwitchMarker: its release went live, whatever the
// release log's cap kept.
func WithSwitched(ctx context.Context, switched *atomic.Bool) context.Context {
	return context.WithValue(ctx, switchedKey{}, switched)
}

// WithoutInterrupt is ctx for a session devopsy ends itself on Ctrl-C (a
// sync's holder and push, which must end in order: the push, then abort
// to the holder): its ssh ignores SIGINT, which a terminal sends to the
// whole foreground group. Not a release's: Ctrl-C there ends the session,
// which cancels the release before its switch. Never a new process group
// instead: ssh asking at the terminal (a host key, a passphrase) would
// stop on SIGTTIN.
func WithoutInterrupt(ctx context.Context) context.Context {
	return context.WithValue(ctx, noInterruptKey{}, true)
}

// SSHCommand is ssh with args, or DEVOPSY_SSH_COMMAND, which replaces
// "ssh" like GIT_SSH_COMMAND, with them.
func SSHCommand(args ...string) *exec.Cmd {
	if custom := os.Getenv("DEVOPSY_SSH_COMMAND"); custom != "" {
		// exec: signals reach the command itself, never only a shell
		// around it (a cancel would leave the session open, a kill
		// would read as the shell's 137).
		return exec.Command("sh", append([]string{"-c", "exec " + custom + ` "$@"`, "sh"}, args...)...)
	}
	return exec.Command("ssh", args...)
}

// SSHKillDelay is how long an ssh ended by its context has to exit before
// SIGKILL, and its output to close after it exits.
var SSHKillDelay = 2 * time.Second

// SSHCommandContext is SSHCommand ended by ctx: SIGTERM, which closes the
// session, then SIGKILL after SSHKillDelay.
func SSHCommandContext(ctx context.Context, args ...string) *exec.Cmd {
	plain := SSHCommand(args...)
	cmd := exec.CommandContext(ctx, plain.Path, plain.Args[1:]...)
	cmd.Err = plain.Err
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = SSHKillDelay
	return cmd
}

// LogSaveScript saves stdin as release id's log, or appends it (a
// rollback's). Nothing when the release is gone (a first release that did
// not go live).
func LogSaveScript(t *Target, id string, appendTo bool) string {
	redirect := ">"
	if appendTo {
		redirect = ">>"
	}
	return "set -eu\n" + basePrelude(t) + fmt.Sprintf(`d="$base/releases/"%s
[ -d "$d" ] || { cat > /dev/null; exit 0; }
umask 077
cat %s "$d/%s"
`, Quote(id), redirect, LogFile)
}

// LogReadScript prints the logs of the release whose id starts with
// prefix (the newest such), or, without one, of the one most recently
// written: the session's (LogFile), then LogServerPart and the server's
// (ServerLogFile), for the operator to join (joinLogs).
func LogReadScript(t *Target, prefix string) string {
	s := "set -eu\n" + basePrelude(t)
	if prefix == "" {
		s += `f=$(ls -1t "$base"/releases/*/` + LogFile + ` "$base"/releases/*/` + ServerLogFile + ` 2>/dev/null | head -n 1) || true
[ -n "$f" ] || { devopsy --say error "no release logs in $base/releases yet"; exit 1; }
d=$(dirname "$f")
`
	} else {
		s += fmt.Sprintf(`r=$(ls -1 "$base/releases" 2>/dev/null | grep -v '\.tmp$' | grep -- "^"%s | sort -r | head -n 1) || true
[ -n "$r" ] || { devopsy --say error "no release starting with "%s" in $base/releases"; exit 1; }
d="$base/releases/$r"
[ -f "$d/%s" ] || [ -f "$d/%s" ] || { devopsy --say error "release $r has no log"; exit 1; }
`, Quote(prefix), Quote(prefix), LogFile, ServerLogFile)
	}
	return s + `devopsy --say info "the logs in $d"
cat "$d/` + LogFile + `" 2>/dev/null || true
echo "` + LogServerPart + `"
cat "$d/` + ServerLogFile + `" 2>/dev/null || true
`
}

// DestroyScript removes an environment, under its release lock: devopsy
// --destroy-data in its current release, else its newest one (a first
// release whose run step failed left shared/mnt without current): the
// release's own destroy step, maybe none, then compose down with volumes and built
// images, and shared/mnt emptied as root; then every release's images
// (img remove, under the server's image lock, taken
// before anything goes: compose's down removes images too); then the
// directory. A failure before the removal removes nothing.
func DestroyScript(t *Target, projectName string) string {
	return "set -eu\n" + basePrelude(t) + `if [ ! -d "$base" ]; then
  devopsy --say info "nothing at $base"
  exit 0
fi
if [ "$base" = "$root" ] || [ "$base" = / ]; then
  devopsy --say error "refusing to remove $base"
  exit 1
fi
if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  devopsy --say error "$base is a plain devopsy directory, not an environment with releases: remove it by hand"
  exit 1
fi
` + releaseLock(false, false) + `exec 5>"$base/` + SyncLockFile + `"
` + lockWait(5, false, "a sync or an export from $base") + `
` + snapshotLock(false) + releaseImages(projectName, t, t.Mode) + `image_lock exclusive removing || { devopsy --say error "$base stays as it was"; exit 75; }
` + lockedGate(true, "image_unlock; ") + `img recover "$base"
dir=
if [ -d "$base/current" ]; then
  dir="$base/current"
else
  last=$(ls -1 "$base/releases" 2>/dev/null | grep -v '\.tmp$' | sort -r | head -n 1)
  [ -z "$last" ] || dir="$base/releases/$last/` + CodeDir + `"
fi
if [ -n "$dir" ]; then
  (cd "$dir" && ` + devopsyCall(projectName, []string{"--destroy-data"}, false) + `) || { s=$?; image_unlock; devopsy --say error "$base stays as it was"; exit "$s"; }
else
  devopsy --say info "no release: nothing to take down"
fi
d_fail=
img remove "$base"/releases/* || d_fail=1
# Its snapshots' bookmarks too, wherever the snapshot root puts them.
for d in "$snaps"/*; do
  [ ! -d "$d" ] || img remove "$d" || d_fail=1
done
img prune
image_unlock
if [ -n "$d_fail" ]; then
  devopsy --say error "$base stays: its images are not all removed; run --destroy again"
  exit 1
fi
cd /
` + removeBase + ` || {
  devopsy --say error "could not remove everything in $base"
  exit 1
}
# Snapshots outside $base (the server's snapshot root): their directory.
case "$snaps" in "$base"/*) ;; *) rm -rf "$snaps" || { devopsy --say error "could not remove $snaps"; exit 1; } ;; esac
devopsy --say info "removed $base"
`
}

// EnvironmentsScript prints the project's environments on the server, one
// per line, as addresses: <environment>, or <instance>/<environment> for a
// project with instances (directories with releases).
func EnvironmentsScript(t *Target) string {
	glob := `"$dir"/*/releases`
	if t.Project.Instances {
		glob = `"$dir"/*/*/releases`
	}
	return "set -eu\n" + basePrelude(t) + `dir="$root"/` + Quote(t.Project.Name) + `
for r in ` + glob + `; do
  [ -d "$r" ] || continue
  d=${r%/releases}
  printf '%s\n' "${d#"$dir"/}"
done
`
}

// InstanceExistsScript prints "yes" when the target's instance has a
// directory on the server, else "no": --release asks before creating a
// new one. An answer, never the exit code: a failure (an older server's
// devopsy refusing) must stop the release, not read as a new instance.
func InstanceExistsScript(t *Target) string {
	dir := t.Project.Name + "/" + t.Instance
	return "set -eu\n" + basePrelude(t) + `if [ -d "$root"/` + Quote(dir) + ` ]; then echo yes; else echo no; fi
`
}

// RoleHoldersScript prints the compose projects running a container with
// the role, one per line: --release checks it before uploading anything.
func RoleHoldersScript(t *Target, role string) string {
	return "set -eu\ndocker ps --filter " + Quote("label=devopsy.role="+role) + ` --format '{{.Label "com.docker.compose.project"}}' | sort -u
`
}

// LeftoverScript lists, deepest first, the directories of t's levels (its
// instance's, then its project's) that hold nothing but their .env (and
// the lock --vars-project and --vars-instance take) once no environment
// is left in them: what --destroy leaves, since the .env is shared by
// every environment of that level. With remove, it removes them instead,
// checking again under the same rules: --destroy --purge.
func LeftoverScript(t *Target, remove bool) string {
	act := `echo "$d"`
	if remove {
		act = `if [ -s "$d/` + ImagesLeftFile + `" ]; then
  devopsy --say warning "forgetting the images $d's removals left: $(cut -f 4 "$d/` + ImagesLeftFile + `" | tr '\n' ' ')"
fi
rm -f "$d/.env" "$d/.lock" "$d/` + ImagesLeftFile + `" "$d/` + ImagesLeftFile + `.tmp" && rmdir "$d" && devopsy --say info "removed $d"`
	}
	s := "set -eu\n" + basePrelude(t) + "child=.env\n"
	for i := len(t.Levels) - 1; i >= 0; i-- {
		s += `d="$root"/` + Quote(t.Levels[i].Dir) + `
[ -d "$d" ] || exit 0
[ -z "$(ls -A "$d" | grep -vxF -e .env -e .lock -e ` + ImagesLeftFile + ` -e ` + ImagesLeftFile + `.tmp -e "$child" || true)" ] || exit 0
` + act + `
child=${d##*/}
`
	}
	return s
}
