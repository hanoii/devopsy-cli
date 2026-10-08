package remote

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ConfigFile is a project's devopsy config, inside .devopsy/. Committed.
const ConfigFile = "config.yaml"

// LocalConfigFile is read over ConfigFile, for one machine. Not committed,
// never released.
const LocalConfigFile = "config.local.yaml"

// UserConfigFile is the user-level config: $DEVOPSY_HOME/config.yaml, else
// $XDG_CONFIG_HOME/devopsy/config.yaml (~/.config/devopsy/config.yaml). Its
// targets work from any directory, and on a server its releases: say where
// that machine keeps releases. Not ~/.devopsy: devopsy would take the home
// directory for a project.
func UserConfigFile() string {
	if h := os.Getenv("DEVOPSY_HOME"); h != "" {
		return filepath.Join(h, ConfigFile)
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "devopsy", ConfigFile)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "devopsy", ConfigFile)
}

// DefaultKeep is how many releases stay on a server when nothing says.
const DefaultKeep = 5

// InstancesRequired is the value of instances: that makes every remote
// command name an instance.
const InstancesRequired = "required"

// Modes of a target.
const (
	ModeImage = "image" // only .devopsy/ is released; images come from a registry
	ModeBuild = "build" // the whole project is released and built on the server
)

// Project is what a project's config says about the project itself.
type Project struct {
	// Name is the project's name on every server: its directory under the
	// release root, and the start of its compose project names.
	Name string
	// Instances is InstancesRequired or "".
	Instances string
	// Keep is the project's releases.keep, 0 when it sets none.
	Keep int
	// Dir is the project's .devopsy/.
	Dir string
}

// Target is one environment of a project, or a user-level target.
type Target struct {
	Name string `yaml:"-"`
	// Host is the SSH destination, like deploy@203.0.113.10 or an alias from
	// ~/.ssh/config. DEVOPSY_TARGET_HOST_<NAME> replaces it, and
	// DEVOPSY_TARGET_HOST sets it when the config has none (see LoadTarget).
	Host string `yaml:"host"`
	// Path is the directory on the server: relative to its release root,
	// or absolute. Without one, <project>[/<instance>]/<target>.
	Path string `yaml:"path"`
	Mode string `yaml:"mode"`
	// Env is written into each release as .devopsy/target.env: per-target,
	// committed, non-secret settings like DEVOPSY_DOMAINS.
	Env map[string]string `yaml:"env"`
	// Release and Rollback are what --release and --rollback run: required
	// for them, see Steps.
	Release  *Steps `yaml:"release"`
	Rollback *Steps `yaml:"rollback"`
	// Releases holds the target's keep.
	Releases *TargetReleases `yaml:"releases"`
	// Source, on a user-level target, is the local project directory it
	// releases from: release and rollback run only there, and the project's
	// name and settings come from its config. "~/" means the home directory.
	Source string `yaml:"source"`

	// User is set for targets from the user-level file.
	User bool `yaml:"-"`
	// File is where the target was defined.
	File string `yaml:"-"`
	// Pattern is the targets: key this target matched, when not its name.
	Pattern string `yaml:"-"`
	// Project is the target's project: nil for a user-level target without
	// source.
	Project *Project `yaml:"-"`
	// Instance is the instance named for this command, if any.
	Instance string `yaml:"-"`
	// Levels are the directories above the target's path holding shared
	// .env files, relative to the release root: the project's and the
	// instance's. None for an explicit path.
	Levels []Level `yaml:"-"`
	// Keep is how many releases to keep: the target's releases.keep, else
	// the project's, else 0 (the server's default).
	Keep int `yaml:"-"`
	// nulls are its env keys set to null: they remove a default.
	nulls map[string]bool
	// From says where each value came from, for --debug: "host", "path",
	// "mode", "source", "release", "rollback", "keep", "env.KEY". A file,
	// "defaults in" a file, or a variable.
	From map[string]string `yaml:"-"`
}

// Level is a directory shared by several targets on a server, holding a
// .env linked into their releases as Link.
type Level struct {
	Name string // "project" or "instance"
	Dir  string // relative to the release root
	Link string // the file name in a release's .devopsy/
}

// TargetReleases is a target's or a project's releases: section.
type TargetReleases struct {
	Keep int `yaml:"keep"`
}

// Steps are what --release or --rollback runs, in three phases, so the
// upload and the switch of `current` always happen at the same point:
//
//   - Before: local devopsy commands, in order, before anything touches the
//     server. A failure stops there.
//   - then --release uploads, and both switch `current`;
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
	// targetPattern is a targets: key with * standing for one or more
	// name characters.
	targetPattern = regexp.MustCompile(`^[A-Za-z0-9_.*-]+$`)
	// nameRe is a project's or an instance's name: a directory and part of
	// compose and host names.
	nameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Releases is the releases: section of the user-level config: settings of
// the machine releases land on.
type Releases struct {
	// Root is where releases live: relative paths resolve against it.
	Root string `yaml:"root"`
	// Keep is the default for projects; MaxKeep caps any project's.
	Keep    int `yaml:"keep"`
	MaxKeep int `yaml:"max_keep"`
}

// configFile is one config file as read.
type configFile struct {
	project, instances string
	keep               int
	releases           *Releases
	defaults           *Target
	targets            map[string]*Target
	nulls              map[string]map[string]bool
	defaultsNulls      map[string]bool
}

// readConfigFile reads a project's (user false) or the user-level (user
// true) config. Unknown keys are errors: a typo would otherwise be ignored.
func readConfigFile(file string, user bool) (*configFile, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var top map[string]yaml.Node
	if err := yaml.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	f := &configFile{targets: map[string]*Target{}, nulls: map[string]map[string]bool{}}
	for key, node := range top {
		switch {
		case key == "project" && !user:
			if err := node.Decode(&f.project); err != nil || !nameRe.MatchString(f.project) {
				return nil, fmt.Errorf("%s: project: lowercase letters, digits and -", file)
			}
		case key == "instances" && !user:
			if err := node.Decode(&f.instances); err != nil || f.instances != InstancesRequired {
				return nil, fmt.Errorf("%s: instances: only %q", file, InstancesRequired)
			}
		case key == "releases":
			var r Releases
			if err := node.Decode(&r); err != nil {
				return nil, fmt.Errorf("%s: releases: %w", file, err)
			}
			for i := 0; i+1 < len(node.Content); i += 2 {
				switch k := node.Content[i].Value; {
				case k == "keep":
				case (k == "root" || k == "max_keep") && user:
				case k == "root" || k == "max_keep":
					return nil, fmt.Errorf("%s: releases: %s belongs to each machine's user-level config (%s there), never to a project", file, k, "~/.config/devopsy/config.yaml")
				default:
					return nil, fmt.Errorf("%s: releases: unknown key %q", file, k)
				}
			}
			if r.Keep < 0 || r.MaxKeep < 0 {
				return nil, fmt.Errorf("%s: releases: keep and max_keep are at least 1", file)
			}
			f.releases, f.keep = &r, r.Keep
		case key == "defaults":
			t, nulls, err := decodeTarget(file, "defaults", &node)
			if err != nil {
				return nil, err
			}
			if t != nil && (t.Host != "" || t.Path != "") {
				return nil, fmt.Errorf("%s: defaults: host and path belong to each target", file)
			}
			if t != nil {
				t.From = origins(t, "defaults in "+file)
			}
			f.defaults, f.defaultsNulls = t, nulls
		case key == "targets":
			var nodes map[string]yaml.Node
			if err := node.Decode(&nodes); err != nil {
				return nil, fmt.Errorf("%s: targets: %w", file, err)
			}
			for name, n := range nodes {
				if !targetName.MatchString(name) && !targetPattern.MatchString(name) {
					return nil, fmt.Errorf("%s: targets: %q: letters, digits, '.', '_', '-' and * in patterns", file, name)
				}
				t, nulls, err := decodeTarget(file, name, &n)
				if err != nil {
					return nil, err
				}
				if t != nil {
					t.From = origins(t, file)
					t.nulls = nulls
					t.User = user
					t.File = file
				}
				f.targets[name] = t
				f.nulls[name] = nulls
			}
		default:
			return nil, fmt.Errorf("%s: unknown key %q", file, key)
		}
	}
	return f, nil
}

func decodeTarget(file, name string, n *yaml.Node) (*Target, map[string]bool, error) {
	var t *Target
	if err := n.Decode(&t); err != nil {
		return nil, nil, fmt.Errorf("%s: %s: %w", file, name, err)
	}
	nulls := nullEnv(n)
	if t != nil {
		for k := range nulls {
			delete(t.Env, k)
		}
		if t.Releases != nil && t.Releases.Keep < 0 {
			return nil, nil, fmt.Errorf("%s: %s: releases: keep is at least 1", file, name)
		}
	}
	return t, nulls, nil
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
	set("keep", t.Releases != nil && t.Releases.Keep > 0)
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
	if t.Releases == nil {
		t.Releases = base.Releases
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

// Config is what a project's config files and the user-level one define.
type Config struct {
	// Project is nil outside a project or without config.yaml.
	Project *Project
	// Targets by key: names and patterns, the project's over the
	// user-level file's.
	Targets map[string]*Target
	// Files are the files read.
	Files []string
}

// LoadConfig reads the user-level config, then the project's config.yaml
// and config.local.yaml (projectDir is its .devopsy/, "" outside one). Each
// file's defaults apply to its own targets only: the user-level file's and
// the project's never mix.
func LoadConfig(projectDir string) (*Config, error) {
	c := &Config{Targets: map[string]*Target{}}
	var defaults *Target
	var defaultsNulls map[string]bool
	read := func(file string, user bool) (*configFile, error) {
		f, err := readConfigFile(file, user)
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		c.Files = append(c.Files, file)
		if f.defaults != nil {
			defaults = mergeDefaults(defaults, f.defaults, f.defaultsNulls)
			if defaultsNulls == nil {
				defaultsNulls = map[string]bool{}
			}
			for k := range f.defaultsNulls {
				defaultsNulls[k] = true
			}
		}
		for n, t := range f.targets {
			c.Targets[n] = t
		}
		return f, nil
	}
	apply := func(user bool) {
		for n, t := range c.Targets {
			if t != nil && t.User == user && defaults != nil {
				c.Targets[n] = mergeDefaults(defaults, t, t.nulls)
			}
		}
		defaults, defaultsNulls = nil, nil
	}
	if file := UserConfigFile(); file != "" {
		if _, err := read(file, true); err != nil {
			return nil, err
		}
	}
	apply(true)
	if projectDir == "" {
		return c, nil
	}
	for _, name := range []string{ConfigFile, LocalConfigFile} {
		f, err := read(filepath.Join(projectDir, name), false)
		if err != nil {
			return nil, err
		}
		if f == nil {
			continue
		}
		if c.Project == nil {
			c.Project = &Project{Dir: projectDir}
		}
		if f.project != "" {
			c.Project.Name = f.project
		}
		if f.instances != "" {
			c.Project.Instances = f.instances
		}
		if f.keep > 0 {
			c.Project.Keep = f.keep
		}
	}
	apply(false)
	return c, nil
}

// LoadProject reads only what a project's config says about the project.
func LoadProject(projectDir string) (*Project, error) {
	c, err := LoadConfig(projectDir)
	if err != nil {
		return nil, err
	}
	return c.Project, nil
}

// ReleaseSettings are the user-level config's releases: with defaults: the
// root (the home directory), keep and max_keep (DefaultKeep). keep is
// capped at max_keep.
func ReleaseSettings() (Releases, error) {
	r := Releases{}
	if file := UserConfigFile(); file != "" {
		f, err := readConfigFile(file, true)
		if err != nil && !os.IsNotExist(err) {
			return r, err
		}
		if f != nil && f.releases != nil {
			r = *f.releases
		}
	}
	if r.Root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return r, err
		}
		r.Root = home
	}
	if strings.HasPrefix(r.Root, "~/") || r.Root == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return r, err
		}
		r.Root = filepath.Join(home, strings.TrimPrefix(r.Root, "~"))
	}
	if !filepath.IsAbs(r.Root) {
		return r, fmt.Errorf("releases: root must be an absolute directory: %s", r.Root)
	}
	if r.MaxKeep == 0 {
		r.MaxKeep = DefaultKeep
	}
	if r.Keep == 0 {
		r.Keep = DefaultKeep
	}
	r.Keep = min(r.Keep, r.MaxKeep)
	return r, nil
}

// match finds name among targets: itself, else the pattern with the most
// literal characters. Two equally specific patterns are an error.
func match(targets map[string]*Target, name string) (*Target, string, error) {
	if t, ok := targets[name]; ok && !strings.Contains(name, "*") {
		return t, "", nil
	}
	best, bestKey, score, tie := (*Target)(nil), "", -1, false
	for key, t := range targets {
		if !strings.Contains(key, "*") {
			continue
		}
		re := "^" + strings.ReplaceAll(regexp.QuoteMeta(key), `\*`, `[A-Za-z0-9_.-]+`) + "$"
		if !regexp.MustCompile(re).MatchString(name) {
			continue
		}
		s := len(strings.ReplaceAll(key, "*", ""))
		switch {
		case s > score:
			best, bestKey, score, tie = t, key, s, false
		case s == score:
			tie = true
		}
	}
	if tie {
		return nil, "", fmt.Errorf("target %q matches several patterns equally: make one more specific", name)
	}
	return best, bestKey, nil
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

// InstanceVar names the instance when @<instance>:<target> does not.
const InstanceVar = "DEVOPSY_INSTANCE"

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
		return false, fmt.Sprintf("@%s is a user-level target (%s) without source: release and rollback need a project's target, in .devopsy/config.yaml, or source: <the project's directory> on it", t.Name, t.File)
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

// ComposeName is the compose project name of t's environment:
// <project>[-<instance>]-<target>, "" when t has no project.
func (t *Target) ComposeName() string {
	if t.Project == nil || t.Project.Name == "" {
		return ""
	}
	parts := []string{t.Project.Name}
	if t.Instance != "" {
		parts = append(parts, t.Instance)
	}
	parts = append(parts, strings.ToLower(t.Name))
	return strings.Join(parts, "-")
}

// Targets lists the named targets available from projectDir ("" outside a
// project), the project's then the user-level ones, each by name, as
// defined: hosts from variables are not filled in, patterns are left out
// and nothing is validated. For shell completion and --debug.
func Targets(projectDir string) []*Target {
	c, err := LoadConfig(projectDir)
	if err != nil {
		return nil
	}
	var out []*Target
	for n, t := range c.Targets {
		if t == nil || !targetName.MatchString(n) {
			continue
		}
		t.Name = n
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].User != out[j].User {
			return !out[i].User
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Patterns lists the target patterns available from projectDir, sorted.
func Patterns(projectDir string) []string {
	c, err := LoadConfig(projectDir)
	if err != nil {
		return nil
	}
	var out []string
	for n, t := range c.Targets {
		if t != nil && strings.Contains(n, "*") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// LoadTarget resolves one target for a command: name is the concrete
// target (a pattern's match too) and instance the instance named, "" for
// none.
//
// Hosts can come from variables, so a public repository need not name its
// servers: HostVar(name) replaces the target's host, and DefaultHostVar sets
// it when the target has none. They are read from the caller's environment,
// then, for the project's own targets, from projectEnv (its .devopsy/.env;
// nil for none). User-level targets ignore projectEnv: a project's settings
// must not redirect them.
func LoadTarget(projectDir, name, instance string, projectEnv func(string) (string, bool)) (*Target, error) {
	return loadTarget(projectDir, name, instance, projectEnv, true)
}

// DescribeTarget is LoadTarget for --debug: a target of a project that
// requires an instance resolves without one, its path showing where the
// instance goes.
func DescribeTarget(projectDir, name string, projectEnv func(string) (string, bool)) (*Target, error) {
	return loadTarget(projectDir, name, "", projectEnv, false)
}

func loadTarget(projectDir, name, instance string, projectEnv func(string) (string, bool), strict bool) (*Target, error) {
	c, err := LoadConfig(projectDir)
	if err != nil {
		return nil, err
	}
	if len(c.Targets) == 0 {
		where := UserConfigFile()
		if projectDir != "" {
			where = filepath.Join(projectDir, ConfigFile) + " or " + where
		}
		return nil, fmt.Errorf("no targets defined: define %q under targets: in %s", name, where)
	}
	if !targetName.MatchString(name) {
		return nil, fmt.Errorf("target name %q: use letters, digits, '.', '_' and '-'", name)
	}
	found, pattern, err := match(c.Targets, name)
	if err != nil {
		return nil, err
	}
	if found == nil {
		names := make([]string, 0, len(c.Targets))
		for n := range c.Targets {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("no target %q in %s (targets: %s)", name, strings.Join(c.Files, ", "), strings.Join(names, ", "))
	}
	t := *found
	t.Name, t.Pattern = name, pattern
	t.From = map[string]string{}
	for k, v := range found.From {
		t.From[k] = v
	}

	// The project: the project's own for its targets, the source's for a
	// user-level target.
	if !t.User {
		t.Project = c.Project
		if t.Project == nil || t.Project.Name == "" {
			return nil, fmt.Errorf("set project: (its name on servers) in %s", filepath.Join(projectDir, ConfigFile))
		}
	} else if t.Source != "" {
		p, err := LoadProject(filepath.Join(t.SourceDir(), ".devopsy"))
		if err != nil {
			return nil, err
		}
		if p == nil || p.Name == "" {
			return nil, fmt.Errorf("@%s: its source, %s, has no project: in .devopsy/config.yaml", name, t.Source)
		}
		t.Project = p
	}

	if instance != "" && !nameRe.MatchString(instance) {
		return nil, fmt.Errorf("instance %q: lowercase letters, digits and -", instance)
	}
	switch {
	case instance != "" && t.Project == nil:
		return nil, fmt.Errorf("@%s: an instance needs a project: this user-level target has no source", name)
	case instance != "" && t.Path != "":
		return nil, fmt.Errorf("@%s: it has its own path (%s), so it takes no instance", name, t.Path)
	case strict && instance == "" && t.Project != nil && t.Project.Instances == InstancesRequired && t.Path == "":
		return nil, fmt.Errorf("%s needs an instance: devopsy @<instance>:%s (or %s)", t.Project.Name, name, InstanceVar)
	}
	t.Instance = instance

	if t.Path == "" {
		if t.Project == nil {
			return nil, fmt.Errorf("@%s: a user-level target needs path: or source:", name)
		}
		dir := t.Project.Name
		t.Levels = []Level{{Name: "project", Dir: dir, Link: "project.env"}}
		if instance != "" {
			dir += "/" + instance
			t.Levels = append(t.Levels, Level{Name: "instance", Dir: dir, Link: "instance.env"})
		} else if !strict && t.Project.Instances == InstancesRequired {
			dir += "/<instance>"
		}
		t.Path = dir + "/" + name
		t.From["path"] = "<project>[/<instance>]/<target>, under the server's release root"
	}
	clean := path.Clean(t.Path)
	if !strict {
		clean = t.Path
	} else if clean == "/" || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return nil, fmt.Errorf("target %q: path must be a directory under the release root, or absolute (not /)", name)
	}
	t.Path = clean

	if t.Releases != nil && t.Releases.Keep > 0 {
		t.Keep = t.Releases.Keep
	} else if t.Project != nil && t.Project.Keep > 0 {
		t.Keep = t.Project.Keep
		t.From["keep"] = "releases: in " + filepath.Join(t.Project.Dir, ConfigFile)
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
	return &t, nil
}
