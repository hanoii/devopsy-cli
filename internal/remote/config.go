package remote

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
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
// aliases work from any directory, and on a server its releases: say where
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

// Values of a project's instances:.
const (
	// InstancesRequired makes every remote command name an instance.
	InstancesRequired = "required"
	// InstancesNone refuses instances.
	InstancesNone = "none"
)

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
	// Instances is InstancesRequired, InstancesNone or "" (optional).
	Instances string
	// Keep is the project's releases.keep, 0 when it sets none.
	Keep int
	// Dir is the project's .devopsy/.
	Dir string
}

// Target is an environment of a project as configured (environments: in
// config.yaml), and, once resolved for a command, where it goes: server,
// instance and paths. For an alias, User is set and Source is the alias's.
type Target struct {
	// Name is the environment's name (an alias's own name in Targets()).
	Name string `yaml:"-"`
	// Host is the server: an SSH destination or ~/.ssh/config alias, from
	// the address, DEVOPSY_SERVER_<ENVIRONMENT> or DEVOPSY_SERVER. Never in
	// a project's config: where it runs is a deployment fact.
	Host string `yaml:"-"`
	// Path is the directory on the server: relative to its release root,
	// or absolute. Without one, <project>[/<instance>]/<environment>.
	Path string `yaml:"path"`
	Mode string `yaml:"mode"`
	// Env is written into each release as .devopsy/target.env: per-
	// environment, committed, non-secret settings like DEVOPSY_DOMAINS.
	Env map[string]string `yaml:"env"`
	// Release and Rollback are what --release and --rollback run: required
	// for them, see Steps.
	Release  *Steps `yaml:"release"`
	Rollback *Steps `yaml:"rollback"`
	// Destroy is what --destroy runs in the current release before devopsy
	// removes the directory: a devopsy command line, required for it. The
	// project takes its containers, volumes and data down; devopsy knows
	// none of them.
	Destroy string `yaml:"destroy"`
	// Releases holds the environment's keep.
	Releases *TargetReleases `yaml:"releases"`

	// Address is the resolved target, <server>:[<instance>/]<environment>.
	Address string `yaml:"-"`
	// User and Source are set when the target came from an alias: it
	// releases only from Source.
	User   bool   `yaml:"-"`
	Source string `yaml:"-"`
	// Alias is the alias's name, if any.
	Alias string `yaml:"-"`
	// File is where the environment was defined.
	File string `yaml:"-"`
	// Pattern is the environments: key this one matched, when not its name.
	Pattern string `yaml:"-"`
	// Project is the target's project: nil only for an alias without source
	// or project.
	Project *Project `yaml:"-"`
	// Instance is the instance, if any.
	Instance string `yaml:"-"`
	// Levels are the directories above the target's path holding shared
	// .env files, relative to the release root: the project's and the
	// instance's. None for an explicit path.
	Levels []Level `yaml:"-"`
	// Keep is how many releases to keep: the environment's releases.keep,
	// else the project's, else 0 (the server's default).
	Keep int `yaml:"-"`
	// nulls are its env keys set to null: they remove a default.
	nulls map[string]bool
	// From says where each value came from, for --debug: "server", "path",
	// "mode", "release", "rollback", "destroy", "keep", "env.KEY". A file,
	// "defaults in" a file, a variable or the address.
	From map[string]string `yaml:"-"`
}

// Alias is a shortcut for a target, in the user-level config: to: is an
// address, in the project at source: (which it releases from) or, without
// source, named project: (commands only).
type Alias struct {
	To      string `yaml:"to"`
	Source  string `yaml:"source"`
	Project string `yaml:"project"`
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

// Steps are what --release or --rollback runs, in phases, so the upload,
// the switch of `current` and the release lock are always where devopsy
// puts them:
//
//   - Before: steps before anything changes, local (on this machine) then
//     remote (on the server, in the current release, under the lock). A
//     failure stops there.
//   - then --release uploads (complete, not live);
//   - Prepare: remote steps in the new release (for a rollback, the one
//     restored), before it goes live: generating secrets, say. Then
//     devopsy checks that every variable compose requires is set. A failure
//     leaves `current` alone.
//   - Run: one remote step, once the release is current: the release
//     itself. A failure switches `current` back and runs the previous
//     release's rollback run step (else its release run), so its
//     containers are the previous release's again.
//   - After: steps once it is live, remote then local. A failure is
//     reported; nothing is undone.
//
// The remote phases share one SSH session, which holds the lock: hence local
// before steps first and local after steps last. Each step is a devopsy
// command line, split on spaces (no quoting). A plain string is Run alone.
type Steps struct {
	Before  []Step
	Prepare StepList
	Run     string
	After   []Step
}

// Step is one devopsy command line, run locally or on the server.
type Step struct {
	Remote bool
	Cmd    string
}

func (s Step) String() string {
	if s.Remote {
		return "remote: " + s.Cmd
	}
	return "local: " + s.Cmd
}

// UnmarshalYAML refuses unknown keys: a typo (prepares:) would otherwise
// release without running anything.
func (s *Steps) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		s.Run = n.Value
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected a command, or before, prepare, run and after", n.Line)
	}
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		var err error
		switch k.Value {
		case "before":
			s.Before, err = decodePhase(v, "before")
		case "after":
			s.After, err = decodePhase(v, "after")
		case "prepare":
			err = v.Decode(&s.Prepare)
		case "run":
			err = v.Decode(&s.Run)
		default:
			return fmt.Errorf("line %d: unknown key %q (before, prepare, run, after)", k.Line, k.Value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// decodePhase reads before's or after's list of {local: cmd} and {remote:
// cmd}, in the order the one SSH session allows.
func decodePhase(n *yaml.Node, phase string) ([]Step, error) {
	if n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("line %d: %s is a list of {local: <command>} and {remote: <command>}", n.Line, phase)
	}
	var steps []Step
	for _, item := range n.Content {
		if item.Kind != yaml.MappingNode || len(item.Content) != 2 || (item.Content[0].Value != "local" && item.Content[0].Value != "remote") || item.Content[1].Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("line %d: a %s step is {local: <command>} or {remote: <command>}", item.Line, phase)
		}
		step := Step{Remote: item.Content[0].Value == "remote", Cmd: item.Content[1].Value}
		if len(steps) > 0 {
			last := steps[len(steps)-1]
			if phase == "before" && last.Remote && !step.Remote {
				return nil, fmt.Errorf("line %d: local before steps go first: remote ones run in the release's SSH session, under its lock", item.Line)
			}
			if phase == "after" && !last.Remote && step.Remote {
				return nil, fmt.Errorf("line %d: remote after steps go first: they run in the release's SSH session, under its lock", item.Line)
			}
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// Commands lists the command lines of steps run on the server (remote) or
// here.
func Commands(steps []Step, remote bool) []string {
	var out []string
	for _, s := range steps {
		if s.Remote == remote {
			out = append(out, s.Cmd)
		}
	}
	return out
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
	envName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	// envPattern is an environments: key with * standing for one or more
	// name characters.
	envPattern = regexp.MustCompile(`^[A-Za-z0-9_.*-]+$`)
	// nameRe is a project's or an instance's name: a directory and part of
	// compose and host names.
	nameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	varName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
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
	defaultsNulls      map[string]bool
	environments       map[string]*Target
	aliases            map[string]Alias
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
	f := &configFile{environments: map[string]*Target{}, aliases: map[string]Alias{}}
	for key, node := range top {
		switch {
		case key == "project" && !user:
			if err := node.Decode(&f.project); err != nil || !nameRe.MatchString(f.project) {
				return nil, fmt.Errorf("%s: project: lowercase letters, digits and -", file)
			}
		case key == "instances" && !user:
			if err := node.Decode(&f.instances); err != nil || (f.instances != InstancesRequired && f.instances != InstancesNone) {
				return nil, fmt.Errorf("%s: instances: %q or %q", file, InstancesRequired, InstancesNone)
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
					return nil, fmt.Errorf("%s: releases: %s belongs to each machine's user-level config (~/.config/devopsy/config.yaml there), never to a project", file, k)
				default:
					return nil, fmt.Errorf("%s: releases: unknown key %q", file, k)
				}
			}
			if r.Keep < 0 || r.MaxKeep < 0 {
				return nil, fmt.Errorf("%s: releases: keep and max_keep are at least 1", file)
			}
			f.releases, f.keep = &r, r.Keep
		case key == "defaults" && !user:
			t, nulls, err := decodeTarget(file, "defaults", &node)
			if err != nil {
				return nil, err
			}
			if t != nil && t.Path != "" {
				return nil, fmt.Errorf("%s: defaults: path belongs to each environment", file)
			}
			if t != nil {
				t.From = origins(t, "defaults in "+file)
			}
			f.defaults, f.defaultsNulls = t, nulls
		case key == "environments" && !user:
			var nodes map[string]yaml.Node
			if err := node.Decode(&nodes); err != nil {
				return nil, fmt.Errorf("%s: environments: %w", file, err)
			}
			for name, n := range nodes {
				if !envName.MatchString(name) && !envPattern.MatchString(name) {
					return nil, fmt.Errorf("%s: environments: %q: letters, digits, '.', '_', '-' and * in patterns", file, name)
				}
				t, nulls, err := decodeTarget(file, name, &n)
				if err != nil {
					return nil, err
				}
				if t == nil {
					t = &Target{}
				}
				t.From = origins(t, file)
				t.nulls = nulls
				t.File = file
				f.environments[name] = t
			}
		case key == "aliases" && user:
			if err := node.Decode(&f.aliases); err != nil {
				return nil, fmt.Errorf("%s: aliases: %w", file, err)
			}
			for i := 0; i+1 < len(node.Content); i += 2 {
				name := node.Content[i].Value
				if !envName.MatchString(name) {
					return nil, fmt.Errorf("%s: aliases: %q: letters, digits, '.', '_' and '-'", file, name)
				}
				for j := 0; j+1 < len(node.Content[i+1].Content); j += 2 {
					switch k := node.Content[i+1].Content[j].Value; k {
					case "to", "source", "project":
					default:
						return nil, fmt.Errorf("%s: aliases: %s: unknown key %q (to, source, project)", file, name, k)
					}
				}
				if a := f.aliases[name]; a.To == "" {
					return nil, fmt.Errorf("%s: aliases: %s: to: is required, an address like vm1:main", file, name)
				}
			}
		default:
			return nil, fmt.Errorf("%s: unknown key %q", file, key)
		}
	}
	return f, nil
}

func decodeTarget(file, name string, n *yaml.Node) (*Target, map[string]bool, error) {
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			switch k := n.Content[i].Value; k {
			case "path", "mode", "env", "release", "rollback", "destroy", "releases":
			case "server":
				return nil, nil, fmt.Errorf("%s: %s: no server: in a project's config: name it in the address (@<server>:%s), DEVOPSY_SERVER, or an alias", file, name, name)
			default:
				return nil, nil, fmt.Errorf("%s: %s: unknown key %q (path, mode, env, release, rollback, destroy, releases)", file, name, k)
			}
		}
	}
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
	set("path", t.Path != "")
	set("mode", t.Mode != "")
	set("release", t.Release != nil)
	set("rollback", t.Rollback != nil)
	set("destroy", t.Destroy != "")
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
// or defaults under an environment. env merges key by key; nulls are over's
// keys that remove base's.
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
	if t.Release == nil {
		t.Release = base.Release
	}
	if t.Rollback == nil {
		t.Rollback = base.Rollback
	}
	if t.Destroy == "" {
		t.Destroy = base.Destroy
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
	// Environments by key, names and patterns, defaults applied.
	Environments map[string]*Target
	// Aliases are the user-level config's.
	Aliases map[string]Alias
	// Files are the files read.
	Files []string
}

// LoadConfig reads the user-level config (aliases), then the project's
// config.yaml and config.local.yaml (projectDir is its .devopsy/, "" outside
// one).
func LoadConfig(projectDir string) (*Config, error) {
	c := &Config{Environments: map[string]*Target{}, Aliases: map[string]Alias{}}
	if file := UserConfigFile(); file != "" {
		f, err := readConfigFile(file, true)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if f != nil {
			c.Files = append(c.Files, file)
			c.Aliases = f.aliases
		}
	}
	if projectDir == "" {
		return c, nil
	}
	var defaults *Target
	for _, name := range []string{ConfigFile, LocalConfigFile} {
		file := filepath.Join(projectDir, name)
		f, err := readConfigFile(file, false)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		c.Files = append(c.Files, file)
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
		if f.defaults != nil {
			defaults = mergeDefaults(defaults, f.defaults, f.defaultsNulls)
		}
		for n, t := range f.environments {
			c.Environments[n] = t
		}
	}
	if defaults != nil {
		for n, t := range c.Environments {
			c.Environments[n] = mergeDefaults(defaults, t, t.nulls)
		}
	}
	return c, nil
}

// ValidName reports whether s can be a project's or an instance's name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// LoadProject reads only what a project's config says about the project.
func LoadProject(projectDir string) (*Project, error) {
	var p *Project
	for _, name := range []string{ConfigFile, LocalConfigFile} {
		f, err := readConfigFile(filepath.Join(projectDir, name), false)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if p == nil {
			p = &Project{Dir: projectDir}
		}
		if f.project != "" {
			p.Name = f.project
		}
		if f.instances != "" {
			p.Instances = f.instances
		}
		if f.keep > 0 {
			p.Keep = f.keep
		}
	}
	return p, nil
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
func match(envs map[string]*Target, name string) (*Target, string, error) {
	if t, ok := envs[name]; ok && !strings.Contains(name, "*") {
		return t, "", nil
	}
	best, bestKey, score, tie := (*Target)(nil), "", -1, false
	for key, t := range envs {
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

// PatternVar is the server variable of an environment pattern: its literal
// part, * and the separators next to it left out (pr-*: DEVOPSY_SERVER_PR,
// pr-big-*: DEVOPSY_SERVER_PR_BIG); "" for "*" alone.
func PatternVar(pattern string) string {
	name := strings.Trim(strings.ReplaceAll(pattern, "*", ""), "-_.")
	if name == "" || pattern == "" {
		return ""
	}
	return EnvironmentVar(name)
}

// Servers lists the servers a project's environments name, without an
// address: DEVOPSY_SERVER, then each environment's variable (a pattern's,
// DEVOPSY_SERVER_PR for pr-*), looked up in lookup. Each once, in that
// order.
func Servers(projectDir string, lookup func(string) (string, bool)) ([]string, error) {
	c, err := LoadConfig(projectDir)
	if err != nil {
		return nil, err
	}
	vars := []string{ServerVar}
	keys := make([]string, 0, len(c.Environments))
	for k := range c.Environments {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.Contains(k, "*") {
			if v := PatternVar(k); v != "" {
				vars = append(vars, v)
			}
		} else {
			vars = append(vars, EnvironmentVar(k))
		}
	}
	var servers []string
	for _, name := range vars {
		if v, _ := lookup(name); v != "" && !slices.Contains(servers, v) {
			servers = append(servers, v)
		}
	}
	return servers, nil
}

// ServerVar is the default server; ServerVar_<ENVIRONMENT> one
// environment's (EnvironmentVar).
const ServerVar = "DEVOPSY_SERVER"

// InstanceVar is the instance when the address names none.
const InstanceVar = "DEVOPSY_INSTANCE"

// EnvironmentVar is the variable naming an environment's server:
// DEVOPSY_SERVER_ and the name in upper case, anything but letters and
// digits as "_" (staging-eu: DEVOPSY_SERVER_STAGING_EU).
func EnvironmentVar(name string) string {
	suffix := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, name)
	return ServerVar + "_" + suffix
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
		return false, fmt.Sprintf("@%s is an alias (%s) without source: release and rollback need source: <the project's directory> on it", t.Alias, UserConfigFile())
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
	return false, fmt.Sprintf("@%s releases only from its source, %s (%s); here: %s", t.Alias, t.Source, UserConfigFile(), here)
}

// SourceDir is an alias's source, with "~/" expanded; "" when it
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
// <project>[-<instance>]-<environment>, "" when t has no project. Never
// from the server or the path.
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

// Targets lists what @ can name from projectDir ("" outside a project): the
// project's environments, then the user-level aliases (User set, Source and
// Address their to:), each by name. Patterns are left out. For completion
// and --debug.
func Targets(projectDir string) []*Target {
	c, err := LoadConfig(projectDir)
	if err != nil {
		return nil
	}
	var out []*Target
	for n, t := range c.Environments {
		if strings.Contains(n, "*") {
			continue
		}
		t.Name = n
		out = append(out, t)
	}
	for n, a := range c.Aliases {
		out = append(out, &Target{Name: n, User: true, Alias: n, Source: a.Source, Address: a.To})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].User != out[j].User {
			return !out[i].User
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Patterns lists the environment patterns of projectDir, sorted.
func Patterns(projectDir string) []string {
	c, err := LoadConfig(projectDir)
	if err != nil {
		return nil
	}
	var out []string
	for n := range c.Environments {
		if strings.Contains(n, "*") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Parts of an address: @[<server>:][<instance>/]<environment>.
type address struct {
	server, instance, env string
	// instanceSet is set when the address has a "/": an empty instance
	// then means none, whatever DEVOPSY_INSTANCE says.
	instanceSet bool
}

func parseAddress(s string) (address, error) {
	var a address
	rest := s
	if i := strings.Index(s, ":"); i >= 0 {
		a.server, rest = s[:i], s[i+1:]
		if a.server == "" {
			return a, fmt.Errorf("@%s: the server before : is empty", s)
		}
	}
	if j := strings.LastIndex(rest, "/"); j >= 0 {
		a.instance, a.instanceSet, rest = rest[:j], true, rest[j+1:]
		if a.instance != "" && !nameRe.MatchString(a.instance) {
			return a, fmt.Errorf("@%s: instance %q: lowercase letters, digits and -", s, a.instance)
		}
	}
	a.env = rest
	if !envName.MatchString(a.env) {
		return a, fmt.Errorf("@%s: not an address: @[<server>:][<instance>/]<environment>", s)
	}
	return a, nil
}

// LoadTarget resolves an address (what follows @) for a command.
//
// What the address leaves out comes from variables: the server from
// EnvironmentVar(environment), then ServerVar; the instance from
// InstanceVar. They are read from the caller's
// environment, then from projectEnv (the project's .devopsy/.env; nil for
// none), never for an alias.
func LoadTarget(projectDir, addr string, projectEnv func(string) (string, bool)) (*Target, error) {
	return loadTarget(projectDir, addr, projectEnv, true)
}

// DescribeTarget is LoadTarget for --debug: a missing server or required
// instance is shown, not an error.
func DescribeTarget(projectDir, addr string, projectEnv func(string) (string, bool)) (*Target, error) {
	return loadTarget(projectDir, addr, projectEnv, false)
}

func loadTarget(projectDir, addr string, projectEnv func(string) (string, bool), strict bool) (*Target, error) {
	c, err := LoadConfig(projectDir)
	if err != nil {
		return nil, err
	}
	a, err := parseAddress(addr)
	if err != nil {
		return nil, err
	}

	// A bare name: the project's environment of that exact name, else an
	// alias, else the project's patterns.
	var alias *Alias
	aliasName := ""
	bare := !strings.ContainsAny(addr, ":/")
	if _, exact := c.Environments[addr]; bare && !exact {
		if al, ok := c.Aliases[addr]; ok {
			alias, aliasName = &al, addr
		}
	}
	project := c.Project
	envs := c.Environments
	if alias != nil {
		if a, err = parseAddress(alias.To); err != nil {
			return nil, fmt.Errorf("alias %s: %w", aliasName, err)
		}
		projectEnv = nil
		project, envs = nil, nil
		switch {
		case alias.Source != "":
			src := &Target{Source: alias.Source}
			sc, err := LoadConfig(filepath.Join(src.SourceDir(), ".devopsy"))
			if err != nil {
				return nil, err
			}
			if sc.Project == nil || sc.Project.Name == "" {
				return nil, fmt.Errorf("alias %s: its source, %s, has no project: in .devopsy/config.yaml", aliasName, alias.Source)
			}
			project, envs = sc.Project, sc.Environments
		case alias.Project != "":
			if !nameRe.MatchString(alias.Project) {
				return nil, fmt.Errorf("alias %s: project: lowercase letters, digits and -", aliasName)
			}
			project = &Project{Name: alias.Project}
		default:
			return nil, fmt.Errorf("alias %s: needs source: (the project's checkout) or project: (its name)", aliasName)
		}
	}
	if project == nil || project.Name == "" {
		if projectDir == "" && alias == nil {
			names := make([]string, 0, len(c.Aliases))
			for n := range c.Aliases {
				names = append(names, n)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("@%s: not in a devopsy project, and no alias %q in %s (aliases: %s)", addr, addr, UserConfigFile(), strings.Join(names, ", "))
		}
		return nil, fmt.Errorf("set project: (its name on servers) in %s", filepath.Join(projectDir, ConfigFile))
	}

	// The environment: defined, or matched by a pattern. An alias without
	// source has no config: any environment, nothing to release.
	var t Target
	if envs != nil || alias == nil || alias.Source != "" {
		found, pattern, err := match(envs, a.env)
		if err != nil {
			return nil, err
		}
		if found == nil {
			names := make([]string, 0, len(envs))
			for n := range envs {
				names = append(names, n)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("no environment %q in %s (environments: %s)", a.env, filepath.Join(project.Dir, ConfigFile), strings.Join(names, ", "))
		}
		t = *found
		t.Pattern = pattern
	}
	t.Name = a.env
	t.Project = project
	defined := t.From
	t.From = map[string]string{}
	for k, v := range defined {
		t.From[k] = v
	}
	if alias != nil {
		t.User, t.Alias, t.Source = true, aliasName, alias.Source
	}

	lookup := func(k string) (string, string) {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			return v, k
		}
		if projectEnv != nil {
			if v, ok := projectEnv(k); ok && v != "" {
				return v, k + " in .devopsy/.env"
			}
		}
		return "", ""
	}

	// The instance.
	instance, from := a.instance, "the address"
	if !a.instanceSet {
		instance, from = lookup(InstanceVar)
		if instance != "" && !nameRe.MatchString(instance) {
			return nil, fmt.Errorf("%s=%s: lowercase letters, digits and -", InstanceVar, instance)
		}
	}
	switch {
	case instance != "" && project.Instances == InstancesNone:
		return nil, fmt.Errorf("%s takes no instance (instances: none), but %s names %s", project.Name, from, instance)
	case instance != "" && t.Path != "":
		return nil, fmt.Errorf("environment %s has its own path (%s), so it takes no instance", a.env, t.Path)
	case strict && instance == "" && project.Instances == InstancesRequired && t.Path == "":
		return nil, fmt.Errorf("%s needs an instance: devopsy @[<server>:]<instance>/%s, or %s", project.Name, a.env, InstanceVar)
	}
	t.Instance = instance
	if instance != "" {
		t.From["instance"] = from
	}

	// The server: the address, else the environment's variable, its
	// pattern's (pr-*: DEVOPSY_SERVER_PR), then DEVOPSY_SERVER.
	if a.server != "" {
		t.Host, t.From["server"] = a.server, "the address"
	} else {
		vars := []string{EnvironmentVar(a.env)}
		if pv := PatternVar(t.Pattern); pv != "" {
			vars = append(vars, pv)
		}
		for _, name := range append(vars, ServerVar) {
			if v, k := lookup(name); v != "" {
				t.Host, t.From["server"] = v, k
				break
			}
		}
	}
	if strings.Contains(t.Host, ":") {
		return nil, fmt.Errorf("server %q: no ':' in a server; use an ~/.ssh/config alias for ports", t.Host)
	}
	if strict && t.Host == "" {
		return nil, fmt.Errorf("no server for @%s: devopsy @<server>:%s, or %s (or %s)", addr, addr, ServerVar, EnvironmentVar(a.env))
	}

	if t.Path == "" {
		dir := project.Name
		t.Levels = []Level{{Name: "project", Dir: dir, Link: "project.env"}}
		if instance != "" {
			dir += "/" + instance
			t.Levels = append(t.Levels, Level{Name: "instance", Dir: dir, Link: "instance.env"})
		} else if !strict && project.Instances == InstancesRequired {
			dir += "/<instance>"
		}
		t.Path = dir + "/" + a.env
		t.From["path"] = "<project>[/<instance>]/<environment>, under the server's release root"
	}
	if strict {
		clean := path.Clean(t.Path)
		if clean == "/" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("environment %q: path must be a directory under the release root, or absolute (not /)", a.env)
		}
		t.Path = clean
	}

	if t.Releases != nil && t.Releases.Keep > 0 {
		t.Keep = t.Releases.Keep
	} else if project.Keep > 0 {
		t.Keep = project.Keep
		t.From["keep"] = "releases: in " + filepath.Join(project.Dir, ConfigFile)
	}
	switch t.Mode {
	case "":
		// Build works for every project, image only for those that never
		// build: a wrong build uploads extra files, a wrong image fails.
		t.Mode = ModeBuild
		t.From["mode"] = "devopsy's default"
	case ModeImage, ModeBuild:
	default:
		return nil, fmt.Errorf("environment %q: mode must be %q or %q", a.env, ModeImage, ModeBuild)
	}
	for k := range t.Env {
		if !varName.MatchString(k) {
			return nil, fmt.Errorf("environment %q: env: %q is not a variable name", a.env, k)
		}
	}
	t.Address = t.Host + ":"
	if t.Instance != "" {
		t.Address += t.Instance + "/"
	}
	t.Address += t.Name
	return &t, nil
}
