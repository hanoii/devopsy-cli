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

// TargetsFile is the targets definition, inside .devopsy/. Committed.
const TargetsFile = "targets.yaml"

// LocalTargetsFile adds or replaces whole targets, for one machine. Not
// committed, never released.
const LocalTargetsFile = "targets.local.yaml"

// TargetEnvFile is written into each release from the target's env.
const TargetEnvFile = "target.env"

// UserTargetsFile is the user-level targets file:
// $XDG_CONFIG_HOME/devopsy/targets.yaml (~/.config/devopsy/targets.yaml), or
// $DEVOPSY_HOME/targets.yaml. Its targets work from any directory, for running
// commands on servers, never for release or rollback. Not ~/.devopsy: devopsy
// would take the home directory for a project.
func UserTargetsFile() string {
	if h := os.Getenv("DEVOPSY_HOME"); h != "" {
		return filepath.Join(h, TargetsFile)
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "devopsy", TargetsFile)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "devopsy", TargetsFile)
}

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
	// ~/.ssh/config. DEVOPSY_TARGET_HOST_<NAME> replaces it, and
	// DEVOPSY_TARGET_HOST sets it when targets.yaml has none (see LoadTarget).
	Host string `yaml:"host"`
	// Path is the absolute directory on the server.
	Path string `yaml:"path"`
	Mode string `yaml:"mode"`
	// Env is written into each release as .devopsy/target.env: per-target,
	// committed, non-secret settings like DEVOPSY_DOMAINS.
	Env map[string]string `yaml:"env"`
	// Release and Rollback are what `release` and `rollback` run: required
	// for them, see Steps.
	Release  *Steps `yaml:"release"`
	Rollback *Steps `yaml:"rollback"`
	// Source, on a user-level target, is the local project directory it
	// releases from: release and rollback run only there. Without it a
	// user-level target never releases. "~/" means the home directory.
	Source string `yaml:"source"`
	// User is set for targets from the user-level file.
	User bool `yaml:"-"`
	// File is where the target was defined.
	File string `yaml:"-"`
	// nulls are its env keys set to null: they remove a default.
	nulls map[string]bool
	// From says where each value came from, for --debug: "host", "path",
	// "mode", "source", "release", "rollback", "env.KEY". A file, "defaults
	// in" a file, or a variable.
	From map[string]string `yaml:"-"`
}

// Steps are what `release` or `rollback` runs, in three phases, so the
// upload and the switch of `current` always happen at the same point:
//
//   - Before: local devopsy commands, in order, before anything touches the
//     server. A failure stops there.
//   - then `release` uploads, and both switch `current`;
//   - Remote: one devopsy command on the server, in the new current, under
//     the release lock. A failure switches `current` back. Several remote
//     steps belong in one project command.
//   - After: local devopsy commands once the release is live. A failure is
//     reported; nothing is undone.
//
// Each entry is a devopsy command line, split on spaces (no quoting).
type Steps struct {
	Before StepList `yaml:"before"`
	Remote string   `yaml:"remote"`
	After  StepList `yaml:"after"`
}

// UnmarshalYAML refuses unknown keys: a typo (remotes:) would otherwise
// release without running anything.
func (s *Steps) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected before, remote and after", n.Line)
	}
	for i := 0; i < len(n.Content); i += 2 {
		switch k := n.Content[i].Value; k {
		case "before", "remote", "after":
		default:
			return fmt.Errorf("line %d: unknown key %q (before, remote, after)", n.Content[i].Line, k)
		}
	}
	type plain Steps
	return n.Decode((*plain)(s))
}

// StepList is one command or a list of them.
type StepList []string

// UnmarshalYAML takes a single string as a list of one.
func (l *StepList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*l = StepList{n.Value}
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*l = list
	return nil
}

// StepArgs splits a step into devopsy's arguments.
func StepArgs(step string) []string {
	return strings.Fields(step)
}

var (
	targetName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func readTargets(file string) (map[string]*Target, error) {
	f, err := readTargetsFile(file)
	if err != nil {
		return nil, err
	}
	return f.targets, nil
}

// DefaultsKey is the reserved entry of a targets file whose fields every
// target of that file (and, for a project, of its targets.local.yaml) takes
// unless it sets its own: mode, source, release, rollback, and env merged
// key by key. host and path stay each target's.
const DefaultsKey = "defaults"

// targetsFile is one targets file: its targets, its defaults (nil without),
// and per target (and for the defaults, under DefaultsKey) the env keys set
// to null, which remove a default instead of setting "".
type targetsFile struct {
	targets  map[string]*Target
	defaults *Target
	nulls    map[string]map[string]bool
}

func readTargetsFile(file string) (*targetsFile, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var nodes map[string]yaml.Node
	if err := yaml.Unmarshal(data, &nodes); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	f := &targetsFile{targets: map[string]*Target{}, nulls: map[string]map[string]bool{}}
	for name, node := range nodes {
		var t *Target
		if err := node.Decode(&t); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", file, name, err)
		}
		f.nulls[name] = nullEnv(&node)
		for k := range f.nulls[name] {
			delete(t.Env, k)
		}
		if t != nil {
			from := file
			if name == DefaultsKey {
				from = "defaults in " + file
			}
			t.From = origins(t, from)
		}
		if name == DefaultsKey {
			if t != nil && (t.Host != "" || t.Path != "") {
				return nil, fmt.Errorf("%s: defaults: host and path belong to each target", file)
			}
			f.defaults = t
			continue
		}
		f.targets[name] = t
	}
	return f, nil
}

// origins marks every value t sets as coming from from.
func origins(t *Target, from string) map[string]string {
	o := map[string]string{}
	set := func(field string, ok bool) {
		if ok {
			o[field] = from
		}
	}
	set("host", t.Host != "")
	set("path", t.Path != "")
	set("mode", t.Mode != "")
	set("source", t.Source != "")
	set("release", t.Release != nil)
	set("rollback", t.Rollback != nil)
	for k := range t.Env {
		o["env."+k] = from
	}
	return o
}

// nullEnv lists the env keys a target node sets to null (KEY: ~).
func nullEnv(n *yaml.Node) map[string]bool {
	nulls := map[string]bool{}
	if n.Kind != yaml.MappingNode {
		return nulls
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value != "env" || n.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		env := n.Content[i+1]
		for j := 0; j+1 < len(env.Content); j += 2 {
			if env.Content[j+1].Tag == "!!null" {
				nulls[env.Content[j].Value] = true
			}
		}
	}
	return nulls
}

// mergeDefaults returns over's fields on top of base's: for two defaults,
// or defaults under a target. env merges key by key; nulls are over's keys
// that remove base's.
func mergeDefaults(base, over *Target, nulls map[string]bool) *Target {
	if base == nil {
		return over
	}
	t := *over
	from := map[string]string{}
	for k, v := range base.From {
		from[k] = v
	}
	for k := range nulls {
		delete(from, "env."+k)
	}
	for k, v := range over.From {
		from[k] = v
	}
	t.From = from
	if t.Mode == "" {
		t.Mode = base.Mode
	}
	if t.Source == "" {
		t.Source = base.Source
	}
	if t.Release == nil {
		t.Release = base.Release
	}
	if t.Rollback == nil {
		t.Rollback = base.Rollback
	}
	env := map[string]string{}
	for k, v := range base.Env {
		env[k] = v
	}
	for k := range nulls {
		delete(env, k)
	}
	for k, v := range over.Env {
		env[k] = v
	}
	if len(env) > 0 {
		t.Env = env
	}
	return &t
}

// HostVar is the variable that replaces the host of the target name:
// DEVOPSY_TARGET_HOST_ and the name in upper case, anything but letters and
// digits as "_" (vm1-traefik: DEVOPSY_TARGET_HOST_VM1_TRAEFIK).
func HostVar(name string) string {
	suffix := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, name)
	return DefaultHostVar + "_" + suffix
}

// DefaultHostVar sets the host of targets that define none.
const DefaultHostVar = "DEVOPSY_TARGET_HOST"

// loadTargets reads every target, lowest precedence first: the user-level
// file, then the project's targets.yaml, then its targets.local.yaml.
// projectDir is "" outside a project. files are the files found.
func loadTargets(projectDir string) (targets map[string]*Target, files []string, err error) {
	targets = map[string]*Target{}
	// The user-level file's defaults apply to its targets only, and a
	// project's (targets.local.yaml's over targets.yaml's) to the project's
	// only: they never mix.
	var defaults *Target
	load := func(file string, user bool) error {
		if file == "" {
			return nil
		}
		f, err := readTargetsFile(file)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		files = append(files, file)
		if f.defaults != nil {
			defaults = mergeDefaults(defaults, f.defaults, f.nulls[DefaultsKey])
		}
		for n, t := range f.targets {
			if t != nil {
				t.User = user
				t.File = file
				t.nulls = f.nulls[n]
			}
			targets[n] = t
		}
		return nil
	}
	apply := func(user bool) {
		for n, t := range targets {
			if t != nil && t.User == user && defaults != nil {
				merged := mergeDefaults(defaults, t, t.nulls)
				targets[n] = merged
			}
		}
		defaults = nil
	}
	if err := load(UserTargetsFile(), true); err != nil {
		return nil, nil, err
	}
	apply(true)
	if projectDir != "" {
		if err := load(filepath.Join(projectDir, TargetsFile), false); err != nil {
			return nil, nil, err
		}
		if err := load(filepath.Join(projectDir, LocalTargetsFile), false); err != nil {
			return nil, nil, err
		}
		apply(false)
	}
	return targets, files, nil
}

// ReleasesHere reports whether release and rollback may run for t from the
// project whose .devopsy/ is projectDir ("" outside a project). A project's
// own targets always may. A user-level target belongs to no project, so
// only from its source: anywhere else, release would upload whatever
// project devopsy runs in to it.
func (t *Target) ReleasesHere(projectDir string) (bool, string) {
	if !t.User {
		return true, ""
	}
	if t.Source == "" {
		return false, fmt.Sprintf("@%s is a user-level target (%s) without source: release and rollback need a target defined by the project, in .devopsy/targets.yaml, or source: <the project's directory> on it", t.Name, t.File)
	}
	want := t.SourceDir()
	here := ""
	if projectDir != "" {
		here = filepath.Dir(projectDir)
	}
	if here != "" && samePath(here, want) {
		return true, ""
	}
	if here == "" {
		here = "outside a project"
	}
	return false, fmt.Sprintf("@%s releases only from its source, %s (%s); here: %s", t.Name, t.Source, t.File, here)
}

// SourceDir is a user-level target's source, with "~/" expanded; "" when it
// has none.
func (t *Target) SourceDir() string {
	dir := t.Source
	if home, err := os.UserHomeDir(); err == nil && (dir == "~" || strings.HasPrefix(dir, "~/")) {
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	return dir
}

// samePath compares two directories after resolving symbolic links.
func samePath(a, b string) bool {
	resolve := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return filepath.Clean(p)
	}
	return resolve(a) == resolve(b)
}

// Targets lists the targets available from projectDir ("" outside a
// project), sorted by name, as defined: hosts from variables are not filled
// in and nothing is validated. For shell completion.
func Targets(projectDir string) []*Target {
	targets, _, err := loadTargets(projectDir)
	if err != nil {
		return nil
	}
	var out []*Target
	for n, t := range targets {
		if t == nil || !targetName.MatchString(n) {
			continue
		}
		t.Name = n
		out = append(out, t)
	}
	// The project's targets, then the user-level ones, each by name.
	sort.Slice(out, func(i, j int) bool {
		if out[i].User != out[j].User {
			return !out[i].User
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// LoadTarget reads one target from projectDir/targets.yaml.
//
// Hosts can come from variables, so a public repository need not name its
// servers: HostVar(name) replaces the target's host, and DefaultHostVar sets
// it when the target has none. They are read from the caller's environment,
// then, for the project's own targets, from projectEnv (its .devopsy/.env;
// nil for none). User-level targets ignore projectEnv: a project's settings
// must not redirect them.
func LoadTarget(projectDir, name string, projectEnv func(string) (string, bool)) (*Target, error) {
	targets, files, err := loadTargets(projectDir)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		where := UserTargetsFile()
		if projectDir != "" {
			where = filepath.Join(projectDir, TargetsFile) + " or " + where
		}
		return nil, fmt.Errorf("no targets defined: define %q in %s", name, where)
	}
	if name == DefaultsKey {
		return nil, fmt.Errorf("%q is not a target: it holds what the targets of its file share", name)
	}
	t, ok := targets[name]
	if !ok || t == nil {
		names := make([]string, 0, len(targets))
		for n := range targets {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("no target %q in %s (targets: %s)", name, strings.Join(files, ", "), strings.Join(names, ", "))
	}
	t.Name = name
	if !targetName.MatchString(name) {
		return nil, fmt.Errorf("target name %q: use letters, digits, '.', '_' and '-'", name)
	}
	lookup := func(k string) string {
		if v, ok := os.LookupEnv(k); ok {
			return v
		}
		if projectEnv != nil && !t.User {
			v, _ := projectEnv(k)
			return v
		}
		return ""
	}
	if t.From == nil {
		t.From = map[string]string{}
	}
	if v := lookup(HostVar(name)); v != "" {
		t.Host = v
		t.From["host"] = HostVar(name)
	} else if t.Host == "" {
		t.Host = lookup(DefaultHostVar)
		t.From["host"] = DefaultHostVar
	}
	if t.Host == "" {
		return nil, fmt.Errorf("target %q: no host: set host in %s, or %s or %s in the environment or .devopsy/.env", name, t.File, HostVar(name), DefaultHostVar)
	}
	if t.Path == "" {
		return nil, fmt.Errorf("target %q: path is required", name)
	}
	if !path.IsAbs(t.Path) || path.Clean(t.Path) == "/" {
		return nil, fmt.Errorf("target %q: path must be an absolute directory, not /", name)
	}
	t.Path = path.Clean(t.Path)
	switch t.Mode {
	case "":
		// Build works for every project, image only for those that never
		// build: a wrong build uploads extra files, a wrong image fails.
		t.Mode = ModeBuild
		t.From["mode"] = "devopsy's default"
	case ModeImage, ModeBuild:
	default:
		return nil, fmt.Errorf("target %q: mode must be %q or %q", name, ModeImage, ModeBuild)
	}
	for k := range t.Env {
		if !envName.MatchString(k) {
			return nil, fmt.Errorf("target %q: env: %q is not a variable name", name, k)
		}
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
		rel == ".devopsy/"+LocalTargetsFile,
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

// DirtyOutsideDevopsy reports uncommitted changes in the project outside
// .devopsy/: in image mode they are not released, since the image is the
// commit's.
func DirtyOutsideDevopsy(projectRoot string) bool {
	cmd := exec.Command("git", "status", "--porcelain", "--", ".", ":(exclude).devopsy")
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
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

// Shell helpers shared by the scripts. $base is the target path.
const prelude = `set -eu
base=%s
if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
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
// instead of id. It ends with the release's URL, its wildcard host or else
// its first domain, as devopsy on the server computes them.
func ActivateScript(t *Target, id string, rollback bool, projectName string, args []string) string {
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
# Always linked, so editing the server's .env applies without a new release.
touch "$base/shared/.env"
for f in "$base"/shared/* "$base"/shared/.[!.]*; do
  [ -e "$f" ] || [ -L "$f" ] || continue
  rm -rf "$rel/.devopsy/${f##*/}"
  ln -s "$f" "$rel/.devopsy/${f##*/}"
done
`
	if !rollback {
		s += fmt.Sprintf(PrepareScript, devopsyCall(projectName, []string{"--prepare-release"}, false))
	}
	s += `make_current "releases/$id"
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
	s += `vars=$(cd "$base/current" && ` + devopsyCall(projectName, []string{"--env"}, false) + ` 2>/dev/null) || vars=
host=$(printf '%s\n' "$vars" | sed -n "s/^DEVOPSY_WILDCARD_HOST='\(.*\)'$/\1/p")
domains=$(printf '%s\n' "$vars" | sed -n "s/^DEVOPSY_DOMAINS='\(.*\)'$/\1/p" | tr ',' ' ')
set -- $domains
if [ -n "$host" ]; then
  echo "devopsy: https://$host"
elif [ $# -gt 0 ]; then
  echo "devopsy: https://$1"
else
  echo "devopsy: no URL: no wildcard domain or domains (DEVOPSY_WILDCARD_DOMAIN, DEVOPSY_DOMAINS)"
fi
`
	return s
}

// PrepareScript runs devopsy --prepare-release (cli.PrepareRelease) in a
// new release before it becomes current, with $base, $rel and $id set; %s is
// the call. It checks the project's devopsy.role is free on the host and
// writes its devopsy.import labels into the release's target.env, so
// rollbacks keep what each release had. Only for projects with those labels,
// so servers with an older devopsy release the others. A failure leaves
// current alone.
const PrepareScript = `if grep -qsE 'devopsy\.(role|import\.)' "$rel"/.devopsy/compose.yaml "$rel"/.devopsy/compose.override.y*ml; then
  if ! (cd "$rel" && %s); then
    touch "$rel/.devopsy-failed"
    echo "devopsy: release $id not made current" >&2
    exit 1
  fi
fi
`

// enter changes to where commands run: the current release, or the path
// itself for a plain devopsy directory (like /srv/traefik, a git clone).
func enter(t *Target) string {
	return fmt.Sprintf(`base=%s
if [ -d "$base/current" ]; then
  cd "$base/current"
elif [ -d "$base/.devopsy" ]; then
  cd "$base"
else
  echo "devopsy: %s has no release and no .devopsy/ project: run 'devopsy @%s release' first" >&2
  exit 1
fi
`, Quote(t.Path), t.Path, t.Name)
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
	return fmt.Sprintf(`set -eu
base=%s
if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
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
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}
