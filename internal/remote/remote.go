// Package remote runs devopsy on a server over SSH: `devopsy @<target> ...`.
//
// A target, in .devopsy/targets.yaml, names an SSH destination and a path.
// On the server the path holds:
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
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// TargetsFile is the targets definition, inside .devopsy/.
const TargetsFile = "targets.yaml"

// Keep is how many releases stay on the server.
const Keep = 5

// Modes of a target.
const (
	ModeImage = "image" // only .devopsy/ is released; images come from a registry
	ModeBuild = "build" // the whole project is released and built on the server
)

// Target is one entry of targets.yaml.
type Target struct {
	Name string `yaml:"-"`
	// Host is the SSH destination, like deploy@203.0.113.10 or an alias from
	// ~/.ssh/config.
	Host string `yaml:"host"`
	// Path is the absolute directory on the server.
	Path string `yaml:"path"`
	Mode string `yaml:"mode"`
}

var targetName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// LoadTarget reads one target from projectDir/targets.yaml.
func LoadTarget(projectDir, name string) (*Target, error) {
	file := filepath.Join(projectDir, TargetsFile)
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s not found: define the target %q there", file, name)
		}
		return nil, err
	}
	var targets map[string]*Target
	if err := yaml.Unmarshal(data, &targets); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	t, ok := targets[name]
	if !ok || t == nil {
		names := make([]string, 0, len(targets))
		for n := range targets {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("no target %q in %s (targets: %s)", name, file, strings.Join(names, ", "))
	}
	t.Name = name
	if !targetName.MatchString(name) {
		return nil, fmt.Errorf("target name %q: use letters, digits, '.', '_' and '-'", name)
	}
	if t.Host == "" || t.Path == "" {
		return nil, fmt.Errorf("target %q: host and path are required", name)
	}
	if !path.IsAbs(t.Path) || path.Clean(t.Path) == "/" {
		return nil, fmt.Errorf("target %q: path must be an absolute directory, not /", name)
	}
	t.Path = path.Clean(t.Path)
	switch t.Mode {
	case "":
		t.Mode = ModeImage
	case ModeImage, ModeBuild:
	default:
		return nil, fmt.Errorf("target %q: mode must be %q or %q", name, ModeImage, ModeBuild)
	}
	return t, nil
}

// excluded reports whether a path, relative to the project root, stays out of
// a release: server-side state that lives in shared/ instead.
func excluded(rel string) bool {
	rel = filepath.ToSlash(rel)
	switch {
	case rel == ".git" || strings.HasPrefix(rel, ".git/"):
		return true
	case rel == ".devopsy/.env",
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
func Pack(w io.Writer, projectRoot string, files []string, record []byte) error {
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
	if record != nil {
		hdr := &tar.Header{Name: RecordFile, Mode: 0o644, Size: int64(len(record)), ModTime: time.Now()}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(record); err != nil {
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
		By:   whoami(),
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

func whoami() string {
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

// devopsyCall is the remote devopsy invocation, with the project name fixed
// when compose.yaml has none (the release directory would name it). With
// exec, the shell is replaced by devopsy.
func devopsyCall(projectName string, args []string, exec bool) string {
	env := ""
	if projectName != "" {
		env = "COMPOSE_PROJECT_NAME=" + Quote(projectName) + " "
	}
	if exec {
		env += "exec "
	}
	return env + "devopsy " + quoteAll(args)
}

// Shell helpers shared by the scripts. $base is the target path.
const prelude = `set -eu
base=%s
mkdir -p "$base/releases" "$base/shared/mnt"
make_current() { ln -sfn "$1" "$base/current.new" && mv -Tf "$base/current.new" "$base/current"; }
lock() {
  exec 9>"$base/.lock"
  flock -w 600 9 || { echo "devopsy: another release is running on $base" >&2; exit 75; }
}
`

// UploadScript extracts a release from stdin.
func UploadScript(t *Target, id string) string {
	return fmt.Sprintf(prelude, Quote(t.Path)) + fmt.Sprintf(`rel="$base/releases/"%s
rm -rf "$rel.tmp"
mkdir -p "$rel.tmp"
# -m: extraction time, not archive times (clock skew warnings; builds use
# content, not times).
tar -xzmf - -C "$rel.tmp"
mv "$rel.tmp" "$rel"
`, Quote(id))
}

// ActivateScript links shared/ into a release, makes it current, runs args
// there (if any) and goes back to the previous release when that fails.
// Then it prunes old releases. rollback picks the release before current
// instead of id.
//
// urlName, when known, is the compose project name: with the server's
// DEVOPSY_PUBLIC_DOMAIN, the script ends with the release's public URL.
func ActivateScript(t *Target, id string, rollback bool, projectName, urlName string, args []string) string {
	s := fmt.Sprintf(prelude, Quote(t.Path)) + "lock\n"
	s += `prev=$(readlink "$base/current" 2>/dev/null || true)
`
	if rollback {
		s += `id=$(ls -1 "$base/releases" | grep -v '\.tmp$' | sort -r | while read -r r; do
  [ -e "$base/releases/$r/.devopsy-failed" ] && continue
  if [ -n "${found:-}" ]; then echo "$r"; break; fi
  [ "releases/$r" = "$prev" ] && found=1
done)
[ -n "$id" ] || { echo "devopsy: no release before the current one" >&2; exit 1; }
`
	} else {
		s += "id=" + Quote(id) + "\n"
	}
	s += `rel="$base/releases/$id"
for f in "$base"/shared/* "$base"/shared/.[!.]*; do
  [ -e "$f" ] || [ -L "$f" ] || continue
  rm -rf "$rel/.devopsy/${f##*/}"
  ln -s "$f" "$rel/.devopsy/${f##*/}"
done
make_current "releases/$id"
echo "devopsy: current is now $id"
`
	if len(args) > 0 {
		s += `status=0
cd "$base/current"
` + devopsyCall(projectName, args, false) + ` || status=$?
if [ "$status" != 0 ]; then
  touch "$rel/.devopsy-failed"
  if [ -n "$prev" ]; then
    make_current "$prev"
    echo "devopsy: command failed ($status), current is back to ${prev#releases/}" >&2
  else
    rm -f "$base/current"
    echo "devopsy: command failed ($status), no previous release to go back to" >&2
  fi
  exit "$status"
fi
`
	}
	s += fmt.Sprintf(`ls -1 "$base/releases" | grep -v '\.tmp$' | sort -r | tail -n +%d | while read -r r; do
  [ "releases/$r" = "$(readlink "$base/current")" ] || rm -rf "$base/releases/$r"
done
`, Keep+1)
	if urlName != "" {
		s += `domain=$(sed -n 's/^DEVOPSY_PUBLIC_DOMAIN=//p' /etc/devopsy/devopsy.env 2>/dev/null | tail -n 1)
[ -z "$domain" ] || echo "devopsy: https://"` + Quote(urlName) + `".$domain"
`
	}
	return s
}

// RunScript runs args in the current release.
func RunScript(t *Target, projectName string, args []string) string {
	return fmt.Sprintf(`set -eu
cd %s/current 2>/dev/null || { echo "devopsy: no release on %s yet: run 'devopsy @%s release' first" >&2; exit 1; }
%s
`, Quote(t.Path), t.Path, t.Name, devopsyCall(projectName, args, true))
}

// ReleasesScript prints one line per release: id, current flag, failed flag
// and the record JSON, tab separated.
func ReleasesScript(t *Target) string {
	return fmt.Sprintf(`set -eu
base=%s
[ -d "$base/releases" ] || exit 0
cur=$(readlink "$base/current" 2>/dev/null || true)
ls -1 "$base/releases" | grep -v '\.tmp$' | sort -r | while read -r r; do
  c=-; [ "releases/$r" = "$cur" ] && c=current
  f=-; [ -e "$base/releases/$r/.devopsy-failed" ] && f=failed
  printf '%%s\t%%s\t%%s\t' "$r" "$c" "$f"
  tr -d '\n' < "$base/releases/$r/%s" 2>/dev/null || true
  echo
done
`, Quote(t.Path), RecordFile)
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
	args := []string{}
	if tty {
		args = append(args, "-t")
	} else {
		args = append(args, "-T")
	}
	args = append(args, t.Host, "sh -c "+Quote(script))

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
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}
