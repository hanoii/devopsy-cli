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
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TargetEnvFile is written into each release from the target's env.
const TargetEnvFile = "target.env"

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
		rel == ".devopsy/"+TargetEnvFile,
		rel == ".devopsy/compose.override.yaml",
		rel == ".devopsy/compose.override.yml",
		rel == ".devopsy/mnt" || strings.HasPrefix(rel, ".devopsy/mnt/"):
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

// Pack writes files (from Files) as a gzipped tar to w, keeping modes and
// symlinks.
// extra are generated files, path: content, added after the project's.
func Pack(w io.Writer, projectRoot string, files []string, extra map[string][]byte) error {
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
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uname, hdr.Gname, hdr.Uid, hdr.Gid = "", "", 0, 0
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(full)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
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

// RecordFile describes a release, at the root of each release.
const RecordFile = ".devopsy-release.json"

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

// Trace, when set, receives each SSH command and the script it runs.
var Trace func(string)

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

// basePrelude sets $root, $keep and $max_keep from the server's devopsy
// (--release-settings: its user-level config's releases:) and $base to the
// target's directory: its path, relative to $root unless absolute. The
// server decides where releases live, so a laptop and CI always agree.
func basePrelude(t *Target) string {
	return `settings=$(devopsy --release-settings) || { echo "devopsy: the server's devopsy cannot tell its release settings: upgrade it (devopsy --upgrade)" >&2; exit 1; }
root=$(printf '%s\n' "$settings" | sed -n 's/^root=//p')
keep=$(printf '%s\n' "$settings" | sed -n 's/^keep=//p')
max_keep=$(printf '%s\n' "$settings" | sed -n 's/^max_keep=//p')
path=` + Quote(t.Path) + `
case $path in /*) base=$path ;; *) base=$root/$path ;; esac
`
}

// prelude adds, for release scripts, the checks and helpers they share.
func prelude(t *Target) string {
	return "set -eu\n" + basePrelude(t) + `if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  echo "devopsy: $base is a plain devopsy directory, not managed with releases: release and rollback do not apply" >&2
  exit 1
fi
mkdir -p "$base/releases" "$base/shared/mnt"
make_current() { ln -sfn "$1" "$base/current.new" && mv -Tf "$base/current.new" "$base/current"; }
lock() {
  exec 9>"$base/.lock"
  flock -w 600 9 || { echo "devopsy: another release is running on $base" >&2; exit 75; }
}
`
}

// UploadScript extracts a release from stdin.
func UploadScript(t *Target, id string) string {
	return prelude(t) + fmt.Sprintf(`rel="$base/releases/"%s
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
	// Run runs once it is current. Restart runs in the previous release
	// when Run fails and current goes back to it.
	Run, Restart []string
	// After runs in the current release once it is live.
	After [][]string
}

// RemotePhases are the remote phases of steps. restart is the previous
// release's own run step, for when Run fails: the rollback's, else the
// release's.
func RemotePhases(steps *Steps, restart *Steps) Phases {
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
	if restart != nil {
		p.Restart = StepArgs(restart.Run)
	}
	return p
}

// ActivateScript makes a release current, in one SSH session under the
// release lock: remote before steps in the current release; shared/ and
// the .env levels linked into the new one (for a rollback, the release
// before current); its role and imports (releases only); prepare steps;
// devopsy --missing-vars, so a variable compose requires is never found
// missing once live; the switch; the run step, going back to the previous
// release when it fails and restarting that one; pruning; remote after
// steps. Nothing before the switch changes what is live.
func ActivateScript(t *Target, id string, rollback bool, projectName string, p Phases) string {
	s := prelude(t) + "lock\n"
	s += `prev=$(readlink "$base/current" 2>/dev/null || true)
`
	if rollback {
		s += `id=$(ls -1 "$base/releases" | grep -v '\.tmp$' | sort -r | while read -r r; do
  [ -e "$base/releases/$r/.devopsy-failed" ] && continue
  if [ -n "${found:-}" ]; then echo "$r"; break; fi
  [ "releases/$r" = "$prev" ] && found=1
done)
[ -n "$id" ] || { echo "devopsy: no release before the current one" >&2; exit 1; }
` + RollbackNotLive
	} else {
		s += "id=" + Quote(id) + "\n" + NotLive
	}
	step := func(phase, dir string, args []string, onFail string) string {
		return fmt.Sprintf("echo \"devopsy: running 'devopsy %s' (%s)\" >&2\n(cd %s && %s) || %s\n",
			strings.Join(args, " "), phase, dir, devopsyCall(projectName, args, false), onFail)
	}
	s += `rel="$base/releases/$id"
`
	if len(p.Before) > 0 {
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
  rm -rf "$rel/.devopsy/${f##*/}"
  ln -s "$f" "$rel/.devopsy/${f##*/}"
done
`
	// The project's and the instance's .env, shared by their environments:
	// linked even when missing, so one created later applies too.
	for _, l := range t.Levels {
		s += fmt.Sprintf("ln -sfn \"$root\"/%s/.env \"$rel/.devopsy/%s\"\n", Quote(l.Dir), l.Link)
	}
	if !rollback {
		s += fmt.Sprintf(PrepareScript, devopsyCall(projectName, []string{"--prepare-release"}, false))
	}
	for _, args := range p.Prepare {
		s += step("prepare", `"$rel"`, args, "not_live $?")
	}
	s += fmt.Sprintf("(cd \"$rel\" && %s) || not_live $?\n", devopsyCall(projectName, []string{"--missing-vars"}, false))
	s += `make_current "releases/$id"
echo "devopsy: current is now $id" >&2
`
	if len(p.Run) > 0 {
		restart := ""
		if len(p.Restart) > 0 {
			restart = fmt.Sprintf(`    echo "devopsy: restarting ${prev#releases/}: 'devopsy %s'" >&2
    (cd "$base/current" && %s) || echo "devopsy: restarting ${prev#releases/} failed too ($?): check the site" >&2
`, strings.Join(p.Restart, " "), devopsyCall(projectName, p.Restart, false))
		}
		s += `status=0
` + fmt.Sprintf("echo \"devopsy: running 'devopsy %s' (run)\" >&2\n", strings.Join(p.Run, " ")) + `(cd "$base/current" && ` + devopsyCall(projectName, p.Run, false) + `) || status=$?
if [ "$status" != 0 ]; then
  touch "$rel/.devopsy-failed"
  if [ -n "$prev" ]; then
    make_current "$prev"
    echo "devopsy: run failed ($status), current is back to ${prev#releases/}" >&2
` + restart + `  else
    rm -f "$base/current"
    echo "devopsy: run failed ($status), no previous release to go back to" >&2
  fi
  exit "$status"
fi
`
	}
	// keep: the target's or project's, else the server's, at most its
	// max_keep.
	s += fmt.Sprintf(`k=%d
[ "$k" -gt 0 ] || k=$keep
if [ "$k" -gt "$max_keep" ]; then
  echo "devopsy: keeping $max_keep releases, this server's max_keep, not $k" >&2
  k=$max_keep
fi
ls -1 "$base/releases" | grep -v '\.tmp$' | sort -r | tail -n +$((k + 1)) | while read -r r; do
  [ "releases/$r" = "$(readlink "$base/current")" ] || rm -rf "$base/releases/$r"
done
`, t.Keep)
	for _, args := range p.After {
		s += step("after", `"$base/current"`, args, `{ s=$?; echo "devopsy: an after step failed ($s): it stays live" >&2; exit "$s"; }`)
	}
	return s
}

// NotLive is not_live, for a new release that does not go live: marked
// failed, so rollbacks skip it, or, for a first release with nothing in
// shared/ yet, no directory left at all. $base, $prev, $rel and $id are set.
const NotLive = `not_live() {
  if [ -z "$prev" ] && [ ! -s "$base/shared/.env" ] && [ -z "$(ls -A "$base/shared/mnt" 2>/dev/null)" ]; then
    # A first release that never went live, with nothing set yet: leave
    # no directory behind (an instance's, when it is empty, neither).
    rm -rf "$base"
    rmdir "$(dirname "$base")" 2>/dev/null || true
    echo "devopsy: removed $base: its first release did not go live" >&2
  else
    touch "$rel/.devopsy-failed"
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
const PrepareScript = `if grep -qsE '^[^#]*devopsy\.(role|import\.)' "$rel"/.devopsy/compose.yaml "$rel"/.devopsy/compose.override.y*ml; then
  (cd "$rel" && %s) || not_live 1
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

// ReleasesScript prints one line per release: id, current flag, failed flag
// and the record JSON, tab separated.
func ReleasesScript(t *Target) string {
	return "set -eu\n" + basePrelude(t) + fmt.Sprintf(`if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  echo "devopsy: $base is a plain devopsy directory, without releases" >&2
  exit 1
fi
[ -d "$base/releases" ] || exit 0
cur=$(readlink "$base/current" 2>/dev/null || true)
ls -1 "$base/releases" | grep -v '\.tmp$' | sort -r | while read -r r; do
  c=-; [ "releases/$r" = "$cur" ] && c=current
  f=-; [ -e "$base/releases/$r/.devopsy-failed" ] && f=failed
  printf '%%s\t%%s\t%%s\t' "$r" "$c" "$f"
  tr -d '\n' < "$base/releases/$r/%s" 2>/dev/null || true
  echo
done
`, RecordFile)
}

// FormatReleases turns ReleasesScript output into a table.
func FormatReleases(out string) string {
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 4)
		if len(parts) < 3 {
			continue
		}
		var r Record
		if len(parts) == 4 {
			_ = json.Unmarshal([]byte(parts[3]), &r)
		}
		mark := " "
		if parts[1] == "current" {
			mark = "*"
		}
		src := r.Mode
		if r.Commit != "" {
			short := r.Commit
			if len(short) > 10 {
				short = short[:10]
			}
			src += " " + r.Branch + "@" + short
			if r.Dirty {
				src += "+dirty"
			}
		}
		status := ""
		if parts[2] == "failed" {
			status = "  FAILED"
		}
		fmt.Fprintf(&b, "%s %s  %-24s  %s%s\n", mark, parts[0], r.By, src, status)
	}
	if b.Len() == 0 {
		return "No releases yet.\n"
	}
	return b.String()
}

// SSH runs a script on the target's host with `sh -c`. DEVOPSY_SSH_COMMAND
// replaces "ssh", like GIT_SSH_COMMAND (for example
// "ssh -i key -o UserKnownHostsFile=known_hosts" in CI). It returns the
// remote exit code.
func SSH(t *Target, script string, stdin io.Reader, stdout io.Writer, tty bool) (int, error) {
	return SSHLog(t, script, stdin, stdout, tty, nil)
}

// SSHLog is SSH, also copying the session's output, both streams, to log.
func SSHLog(t *Target, script string, stdin io.Reader, stdout io.Writer, tty bool, log io.Writer) (int, error) {
	args := []string{}
	if tty {
		args = append(args, "-t")
	} else {
		args = append(args, "-T")
	}
	args = append(args, t.Host, "sh -c "+Quote(script))
	if Trace != nil {
		Trace(fmt.Sprintf("devopsy: ssh %s %s, running:\n%s", args[0], t.Host, strings.TrimRight(script, "\n")))
	}

	var cmd *exec.Cmd
	if custom := os.Getenv("DEVOPSY_SSH_COMMAND"); custom != "" {
		cmd = exec.Command("sh", append([]string{"-c", custom + ` "$@"`, "sh"}, args...)...)
	} else {
		cmd = exec.Command("ssh", args...)
	}
	cmd.Stdin = stdin
	if stdin == nil {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = stdout
	if stdout == nil {
		cmd.Stdout = os.Stdout
	}
	cmd.Stderr = os.Stderr
	if log != nil {
		cmd.Stdout = io.MultiWriter(cmd.Stdout, log)
		cmd.Stderr = io.MultiWriter(cmd.Stderr, log)
	}
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// LogsKept is how many release logs an environment keeps.
const LogsKept = 20

// LogSaveScript saves stdin as the environment's release log name, in
// logs/ next to releases/, keeping the newest LogsKept. Nothing when the
// environment is gone (a first release that did not go live).
func LogSaveScript(t *Target, name string) string {
	return "set -eu\n" + basePrelude(t) + fmt.Sprintf(`[ -d "$base/releases" ] || { cat > /dev/null; exit 0; }
mkdir -p "$base/logs"
umask 077
f="$base/logs/"%s
cat > "$f.tmp"
mv "$f.tmp" "$f"
ls -1 "$base/logs" | grep '\.log$' | sort -r | tail -n +%d | while read -r old; do
  rm -f "$base/logs/$old"
done
`, Quote(name), LogsKept+1)
}

// LogReadScript prints the newest release log whose name starts with
// prefix (any, when empty), or with list, the logs' names, newest first.
func LogReadScript(t *Target, prefix string, list bool) string {
	s := "set -eu\n" + basePrelude(t) + `logs=$(ls -1 "$base/logs" 2>/dev/null | grep '\.log$' | sort -r) || true
[ -n "$logs" ] || { echo "devopsy: no release logs in $base yet" >&2; exit 1; }
`
	if list {
		return s + `printf '%s\n' "$logs" | sed 's/\.log$//'` + "\n"
	}
	return s + fmt.Sprintf(`f=$(printf '%%s\n' "$logs" | grep -F -- %s | grep -- "^"%s | head -n 1) || true
[ -n "$f" ] || { echo "devopsy: no release log starting with "%s" in $base/logs" >&2; exit 1; }
echo "devopsy: $base/logs/$f" >&2
cat "$base/logs/$f"
`, Quote(prefix), Quote(prefix), Quote(prefix))
}

// DestroyScript removes the target's environment: down, with its volumes,
// in the current release, then its directory, under the release lock.
// Data bind-mounted from shared/mnt can belong to container users, so it is
// removed from a container.
func DestroyScript(t *Target, projectName string) string {
	return "set -eu\n" + basePrelude(t) + fmt.Sprintf(`if [ ! -d "$base" ]; then
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
flock -w 600 9 || { echo "devopsy: a release is running on $base" >&2; exit 75; }
if [ -d "$base/current" ]; then
  (cd "$base/current" && %s)
fi
if [ -d "$base/shared/mnt" ]; then
  docker run --rm --network none -v "$base/shared:/shared" %s rm -rf /shared/mnt
fi
cd /
rm -rf "$base"
echo "devopsy: removed $base" >&2
`, devopsyCall(projectName, []string{"down", "--volumes", "--remove-orphans"}, false), CleanupImage)
}

// CleanupImage removes files container users own, for DestroyScript.
const CleanupImage = "busybox:1.37.0"

// InstancesScript prints the project's instances on the server, one per
// line: the name, a tab, its targets (directories with releases).
func InstancesScript(t *Target) string {
	return "set -eu\n" + basePrelude(t) + `dir="$root"/` + Quote(t.Project.Name) + `
[ -d "$dir" ] || exit 0
for d in "$dir"/*/; do
  d=${d%/}
  [ -d "$d" ] && [ ! -d "$d/releases" ] || continue
  targets=
  for e in "$d"/*/; do
    e=${e%/}
    [ -d "$e/releases" ] && targets="$targets ${e##*/}"
  done
  [ -z "$targets" ] || printf '%s\t%s\n' "${d##*/}" "${targets# }"
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
