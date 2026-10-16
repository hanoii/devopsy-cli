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
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
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
	// LogFile is what its release printed, then each rollback to it.
	LogFile = "devopsy-log"
	// ImagesFile lists the images it runs, for its own tags (releaseImages).
	ImagesFile = "devopsy-images"
	// FailedFile marks a release that never went live or whose run failed.
	FailedFile = "devopsy-failed"
	// FetchingFile marks a new release while it fetches its images: one
	// left behind was interrupted without a signal (a CI session that
	// dropped), and recover_fetching finishes what its trap would have.
	FetchingFile = "devopsy-fetching"
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
			return nil, fmt.Errorf("build mode needs a git repository at %s: %w", projectRoot, err)
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

// Pack writes files (from Files) as a gzipped tar to w, keeping modes and
// symlinks.
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
	ID     string `json:"id"`
	Mode   string `json:"mode"`
	Time   string `json:"time"`
	By     string `json:"by"`
	Commit string `json:"commit,omitempty"`
	Branch string `json:"branch,omitempty"`
	Dirty  bool   `json:"dirty,omitempty"`
}

// NewRecord describes a release made now from projectRoot.
func NewRecord(projectRoot, mode string, now time.Time) Record {
	r := Record{
		ID:   now.UTC().Format("20060102150405"),
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
// colored locally as they arrive (Colorize), and ColorEnv is exported into
// every session for what the server can color itself.
var Color bool

// TTY is the operator's terminal size, "<cols> <rows>", when stderr is one
// (whatever the colors), exported into every session as TTYEnv: a session
// without a tty (a sync's) runs the run step on a pseudo-terminal of that
// size, so BuildKit draws its live progress (with_tty, basePrelude).
var TTY string

// TTYEnv carries TTY to the server.
const TTYEnv = "DEVOPSY_INTERNAL_TTY"

// Trace, when set, receives each SSH command, and with TraceScripts the
// script it runs.
var (
	Trace        func(string)
	TraceScripts bool
)

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
	opt := ""
	if shared {
		opt = "-s "
	}
	return fmt.Sprintf(`flock -n %s%d || { echo "devopsy: waiting for %s, up to 10 minutes" >&2; flock %s-w 600 %d || { echo "devopsy: %s did not end in 10 minutes" >&2; exit 75; }; }`, opt, fd, what, opt, fd, what)
}

// basePrelude sets $root, $keep and $max_keep from the server's devopsy
// (--release-settings: its user-level config's releases:) and $base to the
// target's directory: its path under $root. The
// server decides where releases live, so a laptop and CI always agree.
func basePrelude(t *Target) string {
	// The detached update check would inherit the scripts' locks (fds 8
	// and 9) and hold them while it asks GitHub.
	return `export DEVOPSY_NO_UPDATE_CHECK=1
settings=$(devopsy --release-settings) || { echo "devopsy: the server's devopsy cannot tell its release settings: upgrade it (devopsy --upgrade)" >&2; exit 1; }
with_tty() {
  if [ -n "${DEVOPSY_INTERNAL_TTY:-}" ] && [ ! -t 1 ] && command -v script >/dev/null 2>&1; then
    script -qec "stty cols ${DEVOPSY_INTERNAL_TTY%% *} rows ${DEVOPSY_INTERNAL_TTY#* } 2>/dev/null; $1" /dev/null
  else
    eval "$1"
  fi
}
root=$(printf '%s\n' "$settings" | sed -n 's/^root=//p')
keep=$(printf '%s\n' "$settings" | sed -n 's/^keep=//p')
max_keep=$(printf '%s\n' "$settings" | sed -n 's/^max_keep=//p')
base="$root"/` + Quote(t.Path) + `
`
}

// prelude adds, for release scripts, the checks and helpers they share.
func prelude(t *Target) string {
	return "set -eu\n" + basePrelude(t) + `if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  echo "devopsy: $base is a plain devopsy directory, not managed with releases: release and rollback do not apply" >&2
  exit 1
fi
if [ -d "$base/current" ] && [ ! -L "$base/current" ]; then
  echo "devopsy: $base/current is a directory, not a link to a release: containers of a failed release kept restarting and Docker recreated their bind mounts under it, as root. Take them down (docker compose -p <project> down) and remove it (docker run --rm -v \"$base:/env\" alpine rm -rf /env/current), then release again" >&2
  exit 1
fi
mkdir -p "$base/releases" "$base/shared/mnt"
# make_current releases/<id>: current points at its code.
make_current() { ln -sfn "$1/` + CodeDir + `" "$base/current.new" && mv -Tf "$base/current.new" "$base/current"; }
lock() {
  exec 9>"$base/.lock"
  ` + lockWait(9, false, "the release running on $base") + `
}
`
}

// UploadScript extracts a release from stdin. An id taken already (a
// second release of the target within the same second) is refused: mv
// would put the new tree inside the existing release and the old one
// would run again.
func UploadScript(t *Target, id string) string {
	return prelude(t) + fmt.Sprintf(`rel="$base/releases/"%s
[ ! -e "$rel" ] || { echo "devopsy: release $(basename "$rel") exists already on $base (another release in the same second?): try again" >&2; exit 1; }
rm -rf "$rel.tmp"
mkdir -p "$rel.tmp"
# -m: extraction time, not archive times (clock skew warnings; builds use
# content, not times).
tar -xzmf - -C "$rel.tmp"
mv "$rel.tmp" "$rel"
`, Quote(id))
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
	s := prelude(t)
	if sync != nil {
		s += syncStdin
	}
	// Defined first: not_live removes a first release's images.
	images := releaseImages(projectName, t, t.Mode)
	if images != "" {
		s += images
	} else {
		s += noImages
	}
	s += "lock\n"
	s += `prev=$(readlink "$base/current" 2>/dev/null || true)
prev=${prev%/` + CodeDir + `}
`
	if images != "" {
		// The release lock is held until a dropped session's fetch ends, so
		// none is still running.
		s += `if ls "$base"/releases/*/` + FetchingFile + ` >/dev/null 2>&1 && image_lock shared restoring; then
  recover_fetching
  image_unlock
fi
`
	}
	if rollback {
		s += `id=$(ls -1 "$base/releases" | grep -v '\.tmp$' | sort -r | while read -r r; do
  [ -e "$base/releases/$r/` + FailedFile + `" ] && continue
  if [ -n "${found:-}" ]; then echo "$r"; break; fi
  [ "releases/$r" = "$prev" ] && found=1
done)
[ -n "$id" ] || { echo "devopsy: no release before the current one" >&2; exit 1; }
echo "devopsy: rolling back to $id" >&2
` + RollbackNotLive
	} else {
		s += "id=" + Quote(id) + "\n" + NotLive
	}
	step := func(phase, dir string, args []string, onFail string) string {
		return fmt.Sprintf("echo \"devopsy: running 'devopsy %s' (%s)\" >&2\n(cd %s && %s) || %s\n",
			strings.Join(args, " "), phase, dir, devopsyCall(projectName, args, false), onFail)
	}
	s += `rel="$base/releases/$id"
code="$rel/` + CodeDir + `"
`
	// A rollback's steps are the restored release's, read from its own
	// config on the server (ReleaseStepsFlag): the checkout's may have
	// changed since. Its before steps run in the current release, as a
	// release's do.
	steps := func(phase string) string {
		return devopsyCall(projectName, []string{"--release-steps", "rollback", phase}, false)
	}
	if rollback {
		s += `(cd "$base/current" && ` + steps("before") + ` "$rel/` + CodeDir + `") || not_live $?
`
	} else if len(p.Before) > 0 {
		s += "if [ -n \"$prev\" ]; then\n"
		for _, args := range p.Before {
			s += step("before", `"$base/current"`, args, "not_live $?")
		}
		s += "else\n  echo \"devopsy: no current release: remote before steps skipped\" >&2\nfi\n"
	}
	s += `# Always linked, so editing the server's .env applies without a new release.
touch "$base/shared/.env"
for f in "$base"/shared/* "$base"/shared/.[!.]*; do
  [ -e "$f" ] || [ -L "$f" ] || continue
  rm -rf "$code/.devopsy/${f##*/}"
  ln -s "$f" "$code/.devopsy/${f##*/}"
done
`
	// The project's and the instance's .env, shared by their environments:
	// linked even when missing, so one created later applies too.
	for _, l := range t.Levels {
		s += fmt.Sprintf("ln -sfn \"$root\"/%s/.env \"$code/.devopsy/%s\"\n", Quote(l.Dir), l.Link)
	}
	if !rollback {
		s += fmt.Sprintf(PrepareScript, devopsyCall(projectName, []string{"--prepare-release"}, false))
		// Another environment's compose project of the same name, or a
		// network the project names that another project owns already:
		// they would share it (cli.ProjectConflict, cli.NetworkConflicts).
		s += "(cd \"$code\" && " + devopsyCall(projectName, []string{"--check-names"}, false) + " \"$base\") || not_live 1\n"
	}
	if rollback {
		s += `(cd "$code" && ` + steps("prepare") + `) || not_live $?
`
	}
	for _, args := range p.Prepare {
		s += step("prepare", `"$code"`, args, "not_live $?")
	}
	s += fmt.Sprintf("(cd \"$code\" && %s) || not_live $?\n", devopsyCall(projectName, []string{"--missing-vars"}, false))
	if images != "" {
		// A failure may have moved names the live release runs: what it
		// got is bookmarked, so it goes with the release, and the live
		// release's own come back.
		// Interrupted (Ctrl-C, a lost terminal, SIGTERM), the same: the
		// trap runs once the fetch has ended, with set +e and SIGPIPE
		// ignored, so a write to a gone session cannot stop it.
		// A session that drops without a signal leaves FetchingFile for
		// recover_fetching; a rollback writes none, which would mark a good
		// release failed.
		// fetch_undo [now]: nothing to undo when get_images never took the
		// lock (it waited, or was interrupted waiting); the lock still held
		// when interrupted mid-fetch, never taken again (reopening fd 8
		// would let a removal in). A rollback's release keeps its
		// bookmarks: they are what it ran, and the names may hold the live
		// release's images. When the lock cannot be had, a new release
		// keeps FetchingFile for recover_fetching.
		undo := `    bookmark_images "$rel"
`
		left := `the next release, --releases remove or --destroy finishes it`
		if rollback {
			undo = ""
			left = `the names of ${prev#releases/}'s images may still be $id's until its next release`
		}
		s += `fetch_undo() {
  if [ -z "${gi_locked:-}" ]; then
    rm -f "$rel/` + FetchingFile + `"
    return 0
  fi
  if [ -n "${il_held:-}" ] || image_lock shared restoring "${1:-}"; then
` + undo + `    [ -z "$prev" ] || restore_images "$base/$prev" || true
    image_unlock
    rm -f "$rel/` + FetchingFile + `"
  else
    echo "devopsy: $il_busy: ` + left + `" >&2
  fi
}
fetch_interrupted() {
  trap '' INT TERM HUP PIPE
  set +e
  echo "devopsy: the images' fetch was interrupted" >&2
  fetch_undo now
  not_live 130
}
`
		if !rollback {
			s += `: >"$rel/` + FetchingFile + `"
`
		}
		s += `trap fetch_interrupted INT TERM HUP
get_images "$rel" || {
  s=$?
  trap - INT TERM HUP
  fetch_undo
  not_live "$s"
}
trap - INT TERM HUP
rm -f "$rel/` + FetchingFile + `"
`
	}
	s += `make_current "releases/$id"
echo "devopsy: current is now $id" >&2
`
	hasRun := rollback || len(p.Run) > 0
	// The previous release, back after a failed run, restarts with its own
	// images and its own run step: its config's rollback:, else its
	// release: (ReleaseStepsFlag).
	restart := ""
	if images != "" {
		// The new release's images took the names the previous one runs
		// (build mode's are the compose project's, a floating tag's the
		// registry's): its own back.
		restart = `    get_images "$base/$prev" || echo "devopsy: getting the images of ${prev#releases/} failed ($?): restarting it anyway" >&2
`
	}
	restart += `    echo "devopsy: restarting ${prev#releases/} with its own run step" >&2
    (cd "$base/current" && ` + devopsyCall(projectName, []string{"--release-steps", "restart", "run"}, false) + `) || echo "devopsy: restarting ${prev#releases/} failed too ($?): check the site" >&2
`
	if sync != nil || hasRun {
		s += "status=0\nfailed=run\n"
	}
	if sync != nil {
		s += sync.exports(t) + `cd "$base/current"
` + syncFinish(projectName) + syncPrepare(projectName) + `if [ "$status" = 0 ]; then
` + syncWait("the release does not go live") + `  if [ "$reply" != go ]; then
    sync_finish abort || true
    status=1
  else
    sync_finish go || status=$?
  fi
fi
cd "$base"
[ "$status" = 0 ] || failed=sync
` + syncRestorePipe
	}
	if rollback {
		s += `if [ "$status" = 0 ]; then
  (cd "$base/current" && with_tty ` + Quote(steps("run")) + `) || status=$?
fi
`
	} else if len(p.Run) > 0 {
		s += `if [ "$status" = 0 ]; then
` + fmt.Sprintf("  echo \"devopsy: running 'devopsy %s' (run)\" >&2\n", strings.Join(p.Run, " ")) + `  (cd "$base/current" && with_tty ` + Quote(devopsyCall(projectName, p.Run, false)) + `) || status=$?
fi
`
	}
	if sync != nil || hasRun {
		s += `if [ "$status" != 0 ]; then
  touch "$rel/` + FailedFile + `"
  if [ -n "$prev" ]; then
    make_current "$prev"
    echo "devopsy: $failed failed ($status), current is back to ${prev#releases/}" >&2
` + restart + `  else
    rm -f "$base/current"
    echo "devopsy: $failed failed ($status), no previous release to go back to" >&2
  fi
  exit "$status"
fi
`
	}
	// keep: the target's or project's, else the server's, at most its
	// max_keep. Only releases that went live count: failed ones never
	// ran, and a rollback skips them. A failed one newer than the oldest
	// release kept stays, with its log; older ones go.
	s += fmt.Sprintf(`k=%d
[ "$k" -gt 0 ] || k=$keep
if [ "$k" -gt "$max_keep" ]; then
  echo "devopsy: keeping $max_keep releases, this server's max_keep, not $k" >&2
  k=$max_keep
fi
# The k-th live release, newest first: none while fewer went live, and
# nothing goes.
oldest=$(ls -1 "$base/releases" | grep -v '\.tmp$' | sort -r | while read -r r; do
  [ -e "$base/releases/$r/`+FailedFile+`" ] || echo "$r"
done | sed -n "${k}p")
pruned=$(ls -1 "$base/releases" | grep -v '\.tmp$' | sort -r | awk -v o="$oldest" 'past { print } $0 == o { past = 1 }' | while read -r r; do
  [ "releases/$r/`+CodeDir+`" = "$(readlink "$base/current")" ] || echo "$r"
done)
# Removing images needs the server's image lock alone: when another
# release holds it, pruning waits for the next release, directories
# included (one gone without its images would leave tags nothing finds).
if [ -n "$pruned" ]; then
  if image_lock exclusive removing now; then
    for r in $pruned; do
      remove_images "$base/releases/$r"
      rm -rf "$base/releases/$r"
    done
    prune_images
    image_unlock
  else
    echo "devopsy: pruning skipped: $il_busy; the next release prunes" >&2
  fi
fi
`, t.Keep)
	afterFail := `{ s=$?; echo "devopsy: an after step failed ($s): it stays live" >&2; exit "$s"; }`
	if rollback {
		s += `(cd "$base/current" && ` + steps("after") + `) || ` + afterFail + "\n"
	}
	for _, args := range p.After {
		s += step("after", `"$base/current"`, args, afterFail)
	}
	return s
}

// ImagesLock is the server's image lock, under its release root: shared
// while a release fetches and bookmarks its images or restores them,
// exclusive while one removes them (releaseImages). Images are the
// server's, shared by every project on it.
const ImagesLock = ".devopsy-images.lock"

// ImagesHolders is the directory where each holder of ImagesLock writes a
// file named after its shell's pid (start time, compose name, what it
// does), so whoever waits can say who holds it and for how long.
const ImagesHolders = ".devopsy-images.holders"

// ImagesLeftFile, in a project's directory, records the images a removal
// could not remove (Docker refused: a container, even a stopped one, uses
// it), one tab-separated line each: time, compose name, release id, ref,
// image id, Docker's reason. Every pruning, --releases remove and
// --destroy retries them (retry_left), and --releases lists them.
const ImagesLeftFile = ".devopsy-images-left"

// leftPath is ImagesLeftFile's path in a script, in t's project's directory
// under the release root.
func leftPath(t *Target) string {
	return `"$root"/` + Quote(strings.SplitN(t.Path, "/", 2)[0]) + `/` + ImagesLeftFile
}

// noImages stands in for releaseImages' lock and removal functions when
// there is no mode, so scripts call them either way.
const noImages = `image_lock() { :; }
image_unlock() { :; }
bookmark_images() { :; }
restore_images() { return 1; }
recover_fetching() { :; }
image_holders() { echo "another release"; }
remove_images() { :; }
prune_images() { :; }
`

// releaseImages defines the shell functions that give each release its own
// images, for mode (noImages for any other): fetch_images (fetchImages) and
//
//   - bookmark_images <dir>: once fetched, every image the release runs
//     (compose config --images, recorded in ImagesFile) gets a second
//     tag of its own, <repository>:devopsy-<compose name>-<release id>
//     (bookmark, BookmarkPrefix), plus -<cksum of the ref> when the
//     release names the repository more than once (postgres:15 and
//     postgres:16), so no bookmark overwrites another. Compose keeps
//     running the names the files give (drupal11-prod-app, mariadb:11);
//     the bookmark keeps that release's image when a later build or pull
//     moves them. References by digest get one too, though they are
//     exact: docker image prune -f deletes an image pulled by digest only
//     once no container uses it. Never fails.
//   - restore_images <dir>: points the names back at the release's
//     bookmarks, all or nothing, digests aside: a rollback or a restart
//     runs exactly the images that release ran, without building or
//     pulling. Fails when the release has no record or a bookmark is gone.
//   - get_images <dir>: restore_images, else fetch_images and
//     bookmark_images, under the shared image lock. A new release has no
//     record yet, so it fetches.
//   - remove_images <dir>: a release going removes its bookmarks, and an
//     image no other bookmark holds (any release, environment, instance or
//     project: any devopsy- tag) loses the names the release listed that
//     still point to it, so Docker deletes it. A name that moved on stays,
//     and Docker refuses one a container uses (never -f). Under the
//     exclusive lock; never fails.
//   - prune_images: retry_left, then docker image prune -f, once after
//     removals, for what they left dangling. A name Docker refused to
//     remove is recorded in the project's ImagesLeftFile (left_add) and
//     retried by retry_left, under the same exclusive lock: dropped once
//     removed, gone, moved on, or held by a release again.
//   - recover_fetching: a release left with FetchingFile (its session
//     dropped mid-fetch, no signal) is marked failed, what it got
//     bookmarked, and current's names ($prev, when set) restored. The
//     caller holds the image lock.
//   - image_lock shared|exclusive <doing> [now] and image_unlock: the
//     server's image lock (ImagesLock) on fd 8, always taken after the
//     release lock (fd 9). With now it never waits; otherwise up to 10
//     minutes, naming the holders (image_holders). Returns 75 when it
//     could not take it. A flock ends with its processes, so a holder that
//     dies frees it; a fetch's children keep it until they end, as they
//     keep the release lock.
func releaseImages(projectName string, t *Target, mode string) string {
	fetch := fetchImages(projectName, mode)
	if fetch == "" {
		return ""
	}
	name := t.ComposeName()
	if name == "" {
		name = projectName
	}
	prefix := BookmarkPrefix(name)
	holder := name
	if holder == "" {
		holder = "a release"
	}
	return fetch + `left=` + leftPath(t) + `
tab=$(printf '\t')
il_busy=
image_repo() {
  ir=${1%%@*}
  case ${ir##*/} in
    *:*) echo "${ir%:*}" ;;
    *) echo "$ir" ;;
  esac
}
# The repository as Docker resolves it: library/nginx and docker.io/nginx
# are nginx, so they share bookmarks and count as one.
repo_key() {
  rk=$(image_repo "$1")
  rk=${rk#index.docker.io/}
  rk=${rk#docker.io/}
  echo "${rk#library/}"
}
release_id() { echo "${1##*/}"; }
bookmark() {
  bk_repo=$(image_repo "$1")
  bk_key=$(repo_key "$1")
  bk_n=0
  while read -r bk_ref; do
    [ -n "$bk_ref" ] || continue
    if [ "$(repo_key "$bk_ref")" = "$bk_key" ]; then bk_n=$((bk_n + 1)); fi
  done <"$2/` + ImagesFile + `"
  if [ "$bk_n" -gt 1 ]; then
    echo "$bk_repo:` + prefix + `$(release_id "$2")-$(printf '%s' "$1" | cksum | cut -d ' ' -f 1)"
  else
    echo "$bk_repo:` + prefix + `$(release_id "$2")"
  fi
}
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
# image_users <image id>: the compose projects, this one aside, with a
# container (even a stopped one) of that image or one built on it.
image_users() {
  docker ps -a --filter "ancestor=$1" --format '{{.Label "com.docker.compose.project"}}' 2>/dev/null | sort -u |
    awk -v p=` + Quote(name) + ` '$0 != p { printf "%s%s", (n++ ? ", " : ""), ($0 == "" ? "a container outside Compose" : $0) }'
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
    echo "devopsy: waiting for the server's images, held by $(image_holders), up to 10 minutes" >&2
    il_s=0
    flock -w 600 $il_opt 8 || il_s=$?
    if [ "$il_s" -ne 0 ]; then
      il_busy="the server's images are still held by $(image_holders)"
      # Above 128, a signal ended the wait (Ctrl-C), not the 10 minutes.
      [ "$il_s" -gt 128 ] || echo "devopsy: $il_busy after 10 minutes" >&2
      exec 8>&-
      return 75
    fi
  fi
  # A removal needs answers: with Docker down, every inspect fails, and
  # the releases would go without their images.
  if [ "$1" = exclusive ] && ! docker version >/dev/null 2>&1; then
    il_busy="Docker is not answering"
    [ "${3:-}" = now ] || echo "devopsy: $il_busy" >&2
    exec 8>&-
    return 75
  fi
  il_held=1
  # Only for messages: a full disk must not fail the lock.
  mkdir -p "$root/` + ImagesHolders + `" 2>/dev/null || true
  printf '%s %s %s\n' "$(date +%s)" ` + Quote(strings.ReplaceAll(holder, " ", "_")) + ` "$2" 2>/dev/null >"$root/` + ImagesHolders + `/$$" || true
}
image_unlock() {
  rm -f "$root/` + ImagesHolders + `/$$" 2>/dev/null || true
  il_held=
  exec 8>&-
}
bookmark_images() {
  bm_id=$(release_id "$1")
  # A listing that fails keeps the previous one: its bookmarks stay
  # found, so they still go with the release.
  if ! (cd "$1/` + CodeDir + `" && ` + devopsyCall(projectName, []string{"--", "config", "--images"}, false) + `) >"$1/` + ImagesFile + `.tmp" 2>/dev/null; then
    rm -f "$1/` + ImagesFile + `.tmp"
    echo "devopsy: could not list the images of $bm_id: a rollback to it builds or pulls them again" >&2
    return 0
  fi
  bm_old=
  if [ -s "$1/` + ImagesFile + `" ]; then
    bm_old=$(while read -r ref; do [ -z "$ref" ] || bookmark "$ref" "$1"; done <"$1/` + ImagesFile + `")
  fi
  mv -f "$1/` + ImagesFile + `.tmp" "$1/` + ImagesFile + `"
  bm_new=
  while read -r ref; do
    [ -n "$ref" ] || continue
    bm=$(bookmark "$ref" "$1")
    bm_new="$bm_new$bm
"
    docker tag "$ref" "$bm" 2>/dev/null || true
  done <"$1/` + ImagesFile + `"
  # A release fetched again (a rollback or restart whose bookmarks were
  # gone) may list other names: the old bookmarks nothing lists would
  # hold their images forever.
  printf '%s\n' "$bm_old" | while read -r bm; do
    [ -n "$bm" ] || continue
    printf '%s' "$bm_new" | grep -qxF -e "$bm" || docker image rm "$bm" >/dev/null 2>&1 || true
  done
  return 0
}
restore_images() {
  [ -s "$1/` + ImagesFile + `" ] || return 1
  ri_id=$(release_id "$1")
  while read -r ref; do
    case $ref in '' | *@*) continue ;; esac
    docker image inspect "$(bookmark "$ref" "$1")" >/dev/null 2>&1 || return 1
  done <"$1/` + ImagesFile + `"
  while read -r ref; do
    case $ref in '' | *@*) continue ;; esac
    ri_bm=$(bookmark "$ref" "$1")
    # A shared name (mariadb:11) another compose project runs stays on
    # its newer image: moving it back would change that project at its
    # next recreate.
    ri_cur=$(docker image inspect --format '{{.Id}}' "$ref" 2>/dev/null || true)
    if [ -n "$ri_cur" ] && [ "$ri_cur" != "$(docker image inspect --format '{{.Id}}' "$ri_bm" 2>/dev/null || true)" ]; then
      ri_users=$(image_users "$ri_cur")
      if [ -n "$ri_users" ]; then
        echo "devopsy: $ref stays on its newer image, which $ri_users run: $ri_id runs it too" >&2
        continue
      fi
    fi
    docker tag "$ri_bm" "$ref" || return 1
  done <"$1/` + ImagesFile + `"
  echo "devopsy: images of $ri_id restored" >&2
}
get_images() {
  image_lock shared fetching || return $?
  gi_locked=1
  gi=0
  restore_images "$1" || { fetch_images "$1" && bookmark_images "$1"; } || gi=$?
  image_unlock
  return "$gi"
}
remove_images() {
  [ -s "$1/` + ImagesFile + `" ] || return 0
  rm_id=$(release_id "$1")
  # What the first pass freed is kept in the release until it goes, so a
  # removal cut off between the passes (Ctrl-C, a dropped session) is
  # finished by the next one: the bookmarks are gone by then.
  rm_freef="$1/` + ImagesFile + `-free"
  rm_free=
  [ ! -f "$rm_freef" ] || rm_free=" $(tr '\n' ' ' <"$rm_freef")"
  rm_some=
  while read -r ref; do
    [ -n "$ref" ] || continue
    rm_bm=$(bookmark "$ref" "$1")
    rm_img=$(docker image inspect --format '{{.Id}}' "$rm_bm" 2>/dev/null) || continue
    rm_some=1
    # Refused when it is the image's last name and a container, even a
    # stopped one, uses it: say so, nothing will list it again.
    rm_err=$(docker image rm "$rm_bm" 2>&1 >/dev/null) || left_add "$rm_bm" "$rm_img" "$rm_err"
    rm_held=
    for rm_tag in $(docker image inspect --format '{{range .RepoTags}}{{println .}}{{end}}' "$rm_img" 2>/dev/null); do
      case ${rm_tag##*:} in devopsy-*) rm_held=1 ;; esac
    done
    if [ -z "$rm_held" ]; then
      rm_free="$rm_free $rm_img"
      echo "$rm_img" 2>/dev/null >>"$rm_freef" || true
    fi
  done <"$1/` + ImagesFile + `"
  if [ -z "${rm_free# }" ]; then
    [ -z "$rm_some" ] || echo "devopsy: the images of $rm_id stay: other releases hold them" >&2
    return 0
  fi
  rm_gone=
  while read -r ref; do
    [ -n "$ref" ] || continue
    rm_img=$(docker image inspect --format '{{.Id}}' "$ref" 2>/dev/null) || continue
    case " $rm_free " in *" $rm_img "*) ;; *) continue ;; esac
    if rm_err=$(docker image rm "$ref" 2>&1 >/dev/null); then
      rm_gone="${rm_gone:+$rm_gone, }$ref"
    else
      left_add "$ref" "$rm_img" "$rm_err"
    fi
  done <"$1/` + ImagesFile + `"
  [ -z "$rm_gone" ] || echo "devopsy: removed $rm_gone, which only $rm_id held" >&2
  return 0
}
recover_fetching() {
  for rf in "$base"/releases/*/` + FetchingFile + `; do
    [ -e "$rf" ] || continue
    rf_rel=${rf%/*}
    echo "devopsy: the images' fetch of release ${rf_rel##*/} was interrupted: marking it failed, its images bookmarked" >&2
    bookmark_images "$rf_rel"
    [ -z "${prev:-}" ] || restore_images "$base/$prev" || true
    touch "$rf_rel/` + FailedFile + `"
    rm -f "$rf"
  done
}
left_add() {
  echo "devopsy: $1 stays: $3 (recorded: --releases lists it, the next pruning retries)" >&2
  mkdir -p "${left%/*}"
  if [ -f "$left" ] && awk -F "$tab" -v r="$1" -v i="$2" '$4 == r && $5 == i { f = 1 } END { exit !f }' "$left"; then
    return 0
  fi
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$(date +%s)" ` + Quote(strings.ReplaceAll(holder, " ", "_")) + ` "$rm_id" "$1" "$2" "$(printf '%s' "$3" | tr '\t\n' '  ')" >>"$left"
}
# retry_left: this project's record, under the exclusive lock.
retry_left() {
  [ -s "$left" ] || return 0
  retry_file "$left"
}
retry_file() {
  : >"$1.tmp"
  # A last line without its newline (a write cut short) counts too.
  while rl_ref=; IFS="$tab" read -r rl_t rl_name rl_rel rl_ref rl_img rl_why || [ -n "$rl_ref" ]; do
    [ -n "$rl_ref" ] || continue
    # Gone, or the name moved on: nothing left to do. Any other error
    # keeps the line.
    if ! rl_id=$(docker image inspect --format '{{.Id}}' "$rl_ref" 2>/dev/null); then
      case $(docker image inspect "$rl_ref" 2>&1 >/dev/null || true) in
        *"No such image"*) ;;
        *) printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$rl_t" "$rl_name" "$rl_rel" "$rl_ref" "$rl_img" "$rl_why" >>"$1.tmp" ;;
      esac
      continue
    fi
    [ "$rl_id" = "$rl_img" ] || continue
    case ${rl_ref##*:} in
      devopsy-*) ;;
      *)
        # A release holds it again: its own removal decides.
        rl_held=
        for rl_tag in $(docker image inspect --format '{{range .RepoTags}}{{println .}}{{end}}' "$rl_img" 2>/dev/null); do
          case ${rl_tag##*:} in devopsy-*) rl_held=1 ;; esac
        done
        [ -z "$rl_held" ] || continue
        ;;
    esac
    if docker image rm "$rl_ref" >/dev/null 2>&1; then
      echo "devopsy: removed $rl_ref, left by release $rl_rel of $rl_name" >&2
    else
      printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$rl_t" "$rl_name" "$rl_rel" "$rl_ref" "$rl_img" "$rl_why" >>"$1.tmp"
    fi
  done <"$1"
  if [ -s "$1.tmp" ]; then
    mv -f "$1.tmp" "$1"
  else
    rm -f "$1" "$1.tmp"
  fi
}
prune_images() {
  retry_left
  pi=$(docker image prune -f 2>/dev/null | sed -n 's/^Total reclaimed space: //p') || true
  case $pi in '' | 0B) ;; *) echo "devopsy: dangling images pruned, $pi freed" >&2 ;; esac
  return 0
}
`
}

// BookmarkPrefix is the start of a release's image bookmarks, the release
// id following it: devopsy-<compose name>-, then the 14-digit id, then,
// when the release names a repository more than once, - and the ref's
// cksum (up to 10 digits). Docker's tags end at 128 characters, so a
// name over 94 is cut to 85 and followed by the first 8 hex digits of its
// sha256, which keeps names that differ only past the cut apart.
func BookmarkPrefix(composeName string) string {
	if composeName == "" {
		return "devopsy-"
	}
	if len(composeName) > 94 {
		sum := sha256.Sum256([]byte(composeName))
		composeName = composeName[:85] + "-" + hex.EncodeToString(sum[:4])
	}
	return "devopsy-" + composeName + "-"
}

// fetchImages defines fetch_images <dir>, which gets a release's images
// before it goes live, by mode: build pulls the services without a build
// and builds the others, refreshing their base images; image pulls every
// service with an image, buildable ones included, so a tag missing from
// the registry fails there. Through `devopsy --`, past project commands
// named pull or build, on a pseudo-terminal (with_tty) for BuildKit's live
// progress. Empty for any other mode.
func fetchImages(projectName, mode string) string {
	var what, cmd string
	switch mode {
	case ModeBuild:
		what = "pulling and building images (build mode)"
		cmd = devopsyCall(projectName, []string{"--", "pull", "--ignore-buildable"}, false) +
			" && " + devopsyCall(projectName, []string{"--", "build", "--pull"}, false)
	case ModeImage:
		what = "pulling images (image mode)"
		cmd = devopsyCall(projectName, []string{"--", "pull"}, false)
	default:
		return ""
	}
	return `fetch_images() {
  echo "devopsy: ` + what + `" >&2
  (cd "$1/` + CodeDir + `" && with_tty ` + Quote(cmd) + `)
}
`
}

// NotLive is not_live, for a new release that does not go live: marked
// failed, so rollbacks skip it, or, for a first release with nothing in
// shared/ yet, no directory left at all, its releases' images removed
// first (the directory is their only record), unless the server's image
// lock is busy: then it is marked failed too. $base, $prev, $rel and $id
// are set, and releaseImages' functions (or noImages) defined.
const NotLive = `not_live() {
  nl_gone=
  if [ -z "$prev" ] && [ ! -s "$base/shared/.env" ] && [ -z "$(ls -A "$base/shared/mnt" 2>/dev/null)" ]; then
    nl_gone=1
    if ls "$base"/releases/*/` + FetchingFile + ` >/dev/null 2>&1; then
      # fetch_undo could not take the lock: recover_fetching finds what
      # the fetch got through the marker, so the directory stays.
      nl_gone=
    elif ls "$base"/releases/*/` + ImagesFile + ` >/dev/null 2>&1; then
      if image_lock exclusive removing now; then
        for r in "$base"/releases/*; do
          remove_images "$r"
        done
        prune_images
        image_unlock
      else
        echo "devopsy: keeping $base: $il_busy, and the images its releases got go with it ('--releases remove failed' removes them)" >&2
        nl_gone=
      fi
    fi
  fi
  if [ -n "$nl_gone" ]; then
    # A first release that never went live, with nothing set yet: leave
    # no directory behind (an instance's, when it is empty, neither).
    rm -rf "$base"
    rmdir "$(dirname "$base")" 2>/dev/null || true
    echo "devopsy: removed $base: its first release did not go live" >&2
  else
    touch "$rel/` + FailedFile + `"
    echo "devopsy: release $id not made current" >&2
  fi
  exit "$1"
}
`

// RollbackNotLive is not_live for a rollback: the release it would restore
// stays as it was.
const RollbackNotLive = `not_live() {
  echo "devopsy: nothing changed: current is still ${prev#releases/}" >&2
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
  echo "devopsy: $base has no release and no .devopsy/ project: run 'devopsy @%s --release' first" >&2
  exit 1
fi
`, t.Name)
}

// RunScript runs args in the current release, or in a plain directory.
func RunScript(t *Target, projectName string, args []string) string {
	return "set -eu\n" + enter(t) + devopsyCall(projectName, args, true) + "\n"
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
  echo "devopsy: $base is a plain devopsy directory, without releases" >&2
  exit 1
fi
left=`+leftPath(t)+`
# awk ends every line: a last one cut short of its newline would take
# the first release's.
[ ! -s "$left" ] || awk -v t="$(printf '\t')" '{ print "left" t $0 }' "$left"
[ -d "$base/releases" ] || exit 0
cur=$(readlink "$base/current" 2>/dev/null || true)
ls -1 "$base/releases" | grep -v '\.tmp$' | sort -r | while read -r r; do
  c=-; [ "releases/$r/`+CodeDir+`" = "$cur" ] && c=current
  f=-; [ -e "$base/releases/$r/`+FailedFile+`" ] && f=failed
  i=$(tr '\n' ' ' < "$base/releases/$r/%s" 2>/dev/null || true)
  printf '%%s\t%%s\t%%s\t%%s\t' "$r" "$c" "$f" "$i"
  tr -d '\n' < "$base/releases/$r/%s" 2>/dev/null || true
  echo
done
`, ImagesFile, RecordFile)
}

// Release is one line of ReleasesScript's output.
type Release struct {
	ID              string
	Current, Failed bool
	// Images are the names it runs (ImagesFile): what a rollback to it
	// restores, and what removing it may free.
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
		r := Release{ID: parts[0], Current: parts[1] == "current", Failed: parts[2] == "failed"}
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
	status := ""
	if r.Failed {
		status = "  FAILED"
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
			b.WriteString("    " + strings.Join(r.Images, " ") + "\n")
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
// fetch cut off is recovered first (recover_fetching), then every id is
// checked again, refusing them all when one is gone or current, then each
// release's images (remove_images) and directory go, and prune_images
// runs once.
func ReleasesRemoveScript(t *Target, projectName string, ids []string) string {
	images := releaseImages(projectName, t, t.Mode)
	if images == "" {
		images = noImages
	}
	return "set -eu\n" + basePrelude(t) + `if [ ! -d "$base/releases" ]; then
  echo "devopsy: $base has no releases" >&2
  exit 1
fi
exec 9>"$base/.lock"
` + lockWait(9, false, "the release running on $base") + `
` + images + `prev=$(readlink "$base/current" 2>/dev/null || true)
prev=${prev%/` + CodeDir + `}
image_lock exclusive removing || { echo "devopsy: nothing removed" >&2; exit 75; }
recover_fetching
for id in ` + quoteAll(ids) + `; do
  if [ ! -d "$base/releases/$id" ]; then
    echo "devopsy: release $id is gone: nothing removed" >&2
    image_unlock
    exit 1
  fi
  if [ "releases/$id" = "$prev" ]; then
    echo "devopsy: release $id is current: nothing removed" >&2
    image_unlock
    exit 1
  fi
done
for id in ` + quoteAll(ids) + `; do
  remove_images "$base/releases/$id"
  rm -rf "$base/releases/$id"
  echo "devopsy: removed release $id" >&2
done
prune_images
image_unlock
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
	args := append(append(append([]string{}, SSHOptions...), extra...), ttyFlag, t.Host, "sh -c "+Quote(script))
	if Trace != nil {
		opts := strings.Join(append(extra[:len(extra):len(extra)], ttyFlag), " ")
		if TraceScripts {
			Trace(fmt.Sprintf("devopsy: ssh %s %s, running:\n%s", opts, t.Host, strings.TrimRight(script, "\n")))
		} else {
			Trace(fmt.Sprintf("devopsy: ssh %s %s", opts, t.Host))
		}
	}

	cmd := SSHCommand(args...)
	if ctx.Done() != nil {
		cmd = SSHCommandContext(ctx, args...)
	}
	cmd.Stdin = stdin
	if stdin == nil {
		cmd.Stdin = os.Stdin
	}
	// The terminal gets the session's lines colored (Color); a caller's
	// own writer (parsed output) and the log get them as they are.
	var colored []*ColorWriter
	term := func(f *os.File) io.Writer {
		// Never a tty session: the user types there, and a held prompt
		// (no newline yet) would lag; the server's tty colors it anyway.
		if !Color || tty {
			return f
		}
		c := Colorize(f)
		colored = append(colored, c)
		return c
	}
	cmd.Stdout = stdout
	if stdout == nil {
		cmd.Stdout = term(os.Stdout)
	}
	cmd.Stderr = term(os.Stderr)
	if log != nil {
		cmd.Stdout = io.MultiWriter(cmd.Stdout, log)
		cmd.Stderr = io.MultiWriter(cmd.Stderr, log)
	}
	err := cmd.Run()
	for _, c := range colored {
		c.Flush()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// SSHCommand is ssh with args, or DEVOPSY_SSH_COMMAND, which replaces
// "ssh" like GIT_SSH_COMMAND, with them.
func SSHCommand(args ...string) *exec.Cmd {
	if custom := os.Getenv("DEVOPSY_SSH_COMMAND"); custom != "" {
		return exec.Command("sh", append([]string{"-c", custom + ` "$@"`, "sh"}, args...)...)
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

// LogReadScript prints the log of the release whose id starts with prefix
// (the newest such), or, without one, the most recently written log.
func LogReadScript(t *Target, prefix string) string {
	s := "set -eu\n" + basePrelude(t)
	if prefix == "" {
		s += `f=$(ls -1t "$base"/releases/*/` + LogFile + ` 2>/dev/null | head -n 1) || true
[ -n "$f" ] || { echo "devopsy: no release logs in $base/releases yet" >&2; exit 1; }
`
	} else {
		s += fmt.Sprintf(`r=$(ls -1 "$base/releases" 2>/dev/null | grep -v '\.tmp$' | grep -- "^"%s | sort -r | head -n 1) || true
[ -n "$r" ] || { echo "devopsy: no release starting with "%s" in $base/releases" >&2; exit 1; }
f="$base/releases/$r/%s"
[ -f "$f" ] || { echo "devopsy: release $r has no log" >&2; exit 1; }
`, Quote(prefix), Quote(prefix), LogFile)
	}
	return s + `echo "devopsy: $f" >&2
cat "$f"
`
}

// DestroyScript removes an environment, under its release lock: devopsy
// --destroy-data in its current release, else its newest one (a first
// release whose run step failed left shared/mnt without current): the
// release's own destroy step, maybe none, then compose down with volumes and built
// images, and shared/mnt emptied as root; then every release's images
// (releaseImages' remove_images, under the server's image lock, taken
// before anything goes: compose's down removes images too); then the
// directory. A failure before the removal removes nothing.
func DestroyScript(t *Target, projectName string) string {
	images := releaseImages(projectName, t, t.Mode)
	if images == "" {
		images = noImages
	}
	return "set -eu\n" + basePrelude(t) + `if [ ! -d "$base" ]; then
  echo "devopsy: nothing at $base" >&2
  exit 0
fi
if [ "$base" = "$root" ] || [ "$base" = / ]; then
  echo "devopsy: refusing to remove $base" >&2
  exit 1
fi
if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  echo "devopsy: $base is a plain devopsy directory, not an environment with releases: remove it by hand" >&2
  exit 1
fi
exec 9>"$base/.lock"
` + lockWait(9, false, "the release running on $base") + `
` + images + `image_lock exclusive removing || { echo "devopsy: $base stays as it was" >&2; exit 75; }
recover_fetching
dir=
if [ -d "$base/current" ]; then
  dir="$base/current"
else
  last=$(ls -1 "$base/releases" 2>/dev/null | grep -v '\.tmp$' | sort -r | head -n 1)
  [ -z "$last" ] || dir="$base/releases/$last/` + CodeDir + `"
fi
if [ -n "$dir" ]; then
  (cd "$dir" && ` + devopsyCall(projectName, []string{"--destroy-data"}, false) + `) || { s=$?; image_unlock; echo "devopsy: $base stays as it was" >&2; exit "$s"; }
else
  echo "devopsy: no release: nothing to take down" >&2
fi
for r in "$base"/releases/*; do
  [ -d "$r" ] && remove_images "$r"
done
prune_images
image_unlock
cd /
rm -rf "$base" || {
  echo "devopsy: could not remove everything in $base" >&2
  exit 1
}
echo "devopsy: removed $base" >&2
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

// InstanceExistsScript exits 0 when the target's instance has a directory on
// the server: --release asks before creating a new one.
func InstanceExistsScript(t *Target) string {
	dir := t.Project.Name + "/" + t.Instance
	return "set -eu\n" + basePrelude(t) + `[ -d "$root"/` + Quote(dir) + ` ]
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
  echo "devopsy: forgetting the images $d's removals left: $(cut -f 4 "$d/` + ImagesLeftFile + `" | tr '\n' ' ')" >&2
fi
rm -f "$d/.env" "$d/.lock" "$d/` + ImagesLeftFile + `" "$d/` + ImagesLeftFile + `.tmp" && rmdir "$d" && echo "devopsy: removed $d" >&2`
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
