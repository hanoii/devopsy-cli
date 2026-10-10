// Package cli decides what a devopsy invocation runs: a project's custom
// command or `docker compose`. It only builds a Plan; the caller executes it,
// which keeps this package testable.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/template"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"go.yaml.in/yaml/v3"
)

// ProjectDirName is the directory devopsy looks for.
const ProjectDirName = ".devopsy"

// ExitError carries an exit code and a message for the user.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }

// Plan is the process to replace devopsy with.
type Plan struct {
	// Path is the executable: a custom command, or "docker" looked up in PATH.
	Path string
	// Args includes argv[0].
	Args []string
	Env  []string
	// Notice is printed to stderr before executing, if not empty. Secrets
	// are masked.
	Notice string
	// Verbose lines are printed to stderr before Notice with -v.
	// Secrets are masked.
	Verbose []string
}

// Output is returned when devopsy only prints something to stdout.
type Output struct{ Text string }

func (o *Output) Error() string { return o.Text }

// Help is returned when devopsy only has to print usage.
type Help struct {
	Text string
	Code int
}

func (h *Help) Error() string { return h.Text }

// FindProjectDir walks up from dir until it finds a .devopsy directory.
func FindProjectDir(dir string) (string, error) {
	for {
		candidate := filepath.Join(dir, ProjectDirName)
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", &ExitError{Code: 100, Msg: ProjectDirName + "/ not found in this directory or any parent."}
		}
		dir = parent
	}
}

// Env is an ordered set of environment variables.
type Env struct {
	keys   []string
	values map[string]string
	// marked keys are the ones devopsy loaded or computed: what print-env
	// shows.
	marked map[string]bool
	// origin is the first dotenv file that defines a key, even when the
	// caller's environment set it first.
	origin map[string]string
	// caller is the environment devopsy was called with; first and defs are
	// every definition devopsy saw of a key, for TraceEnv: first the ones
	// that win over the caller's, defs the others, by precedence.
	caller map[string]string
	first  map[string][]EnvDef
	defs   map[string][]EnvDef
}

// The kinds of EnvDef.
const (
	EnvCaller   = "caller"
	EnvFile     = "file"
	EnvImported = "imported"
	EnvDevopsy  = "devopsy"
	EnvComputed = "computed"
)

// EnvDef is one place a variable is defined.
type EnvDef struct {
	Kind string
	// Source is the file (EnvFile, EnvImported, EnvComputed), or what
	// devopsy took the value from (EnvDevopsy).
	Source string
	Value  string
}

// EnvVar is a variable and its definitions by precedence: the first one is
// its value, the others are overridden. None: not set.
type EnvVar struct {
	Name string
	Defs []EnvDef
}

// trace returns k's definitions by precedence.
func (e *Env) trace(k string) []EnvDef {
	out := append([]EnvDef{}, e.first[k]...)
	if v, ok := e.caller[k]; ok {
		out = append(out, EnvDef{Kind: EnvCaller, Value: v})
	}
	return append(out, e.defs[k]...)
}

// NewEnv builds an Env from os.Environ-style entries.
func NewEnv(environ []string) *Env {
	e := &Env{values: map[string]string{}, marked: map[string]bool{}, origin: map[string]string{},
		caller: map[string]string{}, first: map[string][]EnvDef{}, defs: map[string][]EnvDef{}}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		e.Set(k, v)
		e.caller[k] = v
	}
	return e
}

// Lookup returns a variable and whether it is set.
func (e *Env) Lookup(k string) (string, bool) {
	v, ok := e.values[k]
	return v, ok
}

// Set sets a variable, keeping first-set order.
func (e *Env) Set(k, v string) {
	if _, ok := e.values[k]; !ok {
		e.keys = append(e.keys, k)
	}
	e.values[k] = v
}

// Mark records k as loaded or computed by devopsy.
func (e *Env) Mark(k string) { e.marked[k] = true }

// Marked returns the marked keys that are set, sorted.
func (e *Env) Marked() []string {
	var keys []string
	for k := range e.marked {
		if _, ok := e.values[k]; ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// Environ returns KEY=value entries.
func (e *Env) Environ() []string {
	out := make([]string, 0, len(e.keys))
	for _, k := range e.keys {
		out = append(out, k+"="+e.values[k])
	}
	return out
}

// LoadDotenv adds the variables of file that are not already set, so the
// caller's environment wins, as in compose. Values are parsed and
// interpolated by compose's own parser.
func LoadDotenv(env *Env, file string) error {
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	vars, err := dotenv.ReadFile(file, env.Lookup)
	if err != nil {
		return &ExitError{Code: 1, Msg: fmt.Sprintf("%s: %v", file, err)}
	}
	// A release's target.env ends with what it imported.
	imported := map[string]string{}
	if filepath.Base(file) == remote.TargetEnvFile {
		if data, err := os.ReadFile(file); err == nil {
			if _, after, ok := strings.Cut(string(data), ImportedMarker); ok {
				imported, _ = dotenv.UnmarshalWithLookup(after, nil)
			}
		}
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := env.Lookup(k); !ok {
			env.Set(k, vars[k])
		}
		env.Mark(k)
		kind := EnvFile
		if _, ok := imported[k]; ok {
			kind = EnvImported
		}
		env.defs[k] = append(env.defs[k], EnvDef{Kind: kind, Source: file, Value: vars[k]})
		if _, ok := env.origin[k]; !ok {
			env.origin[k] = file
		}
	}
	return nil
}

// DotenvLine formats KEY=value so compose's .env parser reads value back
// unchanged, on one line: single quotes, or double quotes with escapes when
// the value has a single quote or a newline.
func DotenvLine(k, v string) string {
	if !strings.ContainsAny(v, "'\n") {
		return k + "='" + v + "'"
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "\n", `\n`)
	return k + `="` + r.Replace(v) + `"`
}

var projectNameChars = regexp.MustCompile("[a-z0-9_-]")

// NormalizeProjectName is compose's own normalization (compose-go loader).
func NormalizeProjectName(s string) string {
	s = strings.ToLower(s)
	s = strings.Join(projectNameChars.FindAllString(s, -1), "")
	return strings.TrimLeft(s, "_-")
}

// TopLevelName returns a compose file's `name:`, uninterpolated, or "".
func TopLevelName(composeFile string) (string, error) {
	data, err := os.ReadFile(composeFile)
	if err != nil {
		return "", err
	}
	var doc struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", &ExitError{Code: 1, Msg: fmt.Sprintf("%s: %v", composeFile, err)}
	}
	return doc.Name, nil
}

// HasTopLevelName reports whether a compose file sets `name:`.
func HasTopLevelName(composeFile string) (bool, error) {
	name, err := TopLevelName(composeFile)
	return name != "", err
}

// CustomCommands lists the executable files in commands/.
func CustomCommands(projectDir string) []string {
	entries, err := os.ReadDir(filepath.Join(projectDir, "commands"))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if isExecutableFile(filepath.Join(projectDir, "commands", e.Name())) {
			names = append(names, e.Name())
		}
	}
	return names
}

func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// CommandDescription reads a command's `## Description:` line, as ddev does,
// from the top of the file.
func CommandDescription(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.SplitN(string(data), "\n", 40)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) > 15 && strings.EqualFold(line[:15], "## Description:") {
			return strings.TrimSpace(line[15:])
		}
	}
	return ""
}

// RemoteHelp describes `devopsy @<target>` commands.
const RemoteHelp = `Targets (devopsy @[<server>:][<instance>/]<environment> ..., or @<alias>):
  --release [--yes]      upload a new release, make it current, run its steps
  --rollback             back to the previous release, and run its steps
  --releases             the releases on the server
  --log [<id>]           what a release or rollback printed: the last, or one
  --shell [service] [-- command...]
                         a shell, or the command, in a container
  --shell-host           a shell on the server itself
  --vars [--project | --instance] [get | set | unset KEY...]
                         the server's variables (shared/.env)
  --env                  the variables devopsy loads and computes there
  --debug [env | capabilities | labels | imports]
                         what devopsy sees there
  --ssh-config           a ~/.ssh/config block for the target's host
  --destroy [--yes]      remove the environment, data included
  <command> [args]       a project or compose command, in the current release
  -- <args>              docker compose <args> there
  devopsy @<target> <flag> --help   details of each
`

// RemoteCommandHelp is the detailed help of each `devopsy @<target>`
// subcommand, shown by `devopsy @<target> <subcommand> --help`.
var RemoteCommandHelp = map[string]string{
	"--release": `Usage: devopsy @<target> --release [--yes]

Uploads the project to the target as a new release, makes it current and
runs the target's release steps from .devopsy/config.yaml (required), in
phases:

  environments:
    prod:
      release:
        before:                # before anything changes; a failure stops
          - local: image       #   here, on this machine, first
          - remote: maint on   #   on the server, in the current release
        prepare: [secrets]     # on the server, in the new release before it
                               # goes live (then: the variables check)
        run: deploy            # once it is current: the release itself
        after:                 # once live; a failure is only reported
          - remote: maint off  #   on the server first
          - local: notify      #   then here
      # or only the run step:
      # release: deploy

After prepare, devopsy checks that every variable compose requires
(${VAR:?message}) is set, with the env capability included; a missing one
fails the release before it goes live, listing what to set (--vars set).
When run fails, current goes back to the previous release, which is then
restarted with its own run step (the rollback's, else the release's).

Each step is a devopsy command line, split on spaces. The remote steps
share one SSH session, under the release lock: hence local before steps
first and local after steps last. Local steps get the target's env,
DEVOPSY_TARGET and DEVOPSY_RELEASE_COMMIT, never the server's shared/.env.
Remote before steps run in the current release, so they use its commands;
none run on a first release.

  - build mode (default): uploads the project as git sees it (tracked and
    untracked files, minus gitignored ones, uncommitted changes included).
  - image mode: uploads .devopsy/; images come from a registry.

The release links the server's shared/ (.env, mnt/...) and writes
.devopsy/target.env from the target's env in config.yaml, plus
DEVOPSY_RELEASE_COMMIT, the commit released, for image tags. It lands in
<project>[/<instance>]/<environment> under the server's release root (its
user-level config's releases: root, else the deploy user's home), unless
the target sets a path. It keeps the target's or project's releases: keep,
else the server's, at most the server's max_keep (all default 5).

The first release of an instance the server does not have yet asks before
creating it (a typo would otherwise make a new site); --yes skips that, for
CI.
`,
	"--rollback": `Usage: devopsy @<target> --rollback

Makes the release before the current one current again (skipping failed
ones) and runs the target's rollback steps from config.yaml (required), in
release's phases: before, prepare and the variables check in the restored
release (a failure changes nothing), run once it is current (a failure goes
back again), after. No upload, and no new imports: the restored release
keeps what it had. Usually the same run step as release:

  environments:
    prod:
      rollback: deploy

The run step runs in the restored release, so it must exist there.
Rolling back restores that release's files and target env, not data.
`,
	"--releases": `Usage: devopsy @<target> --releases

Lists the releases on the target, newest first: id, who made it, mode,
branch and commit (+dirty when made with uncommitted changes), and FAILED for
releases whose command failed. * marks the current one.
`,
	"--env": `Usage: devopsy @<target> --env

The variables devopsy loads and computes in the current release, as
devopsy --env prints them there: shared/.env, the instance's and project's
.env, target.env and the project's env capability (capabilities/env/compute,
from the release on the server, not your checkout). Secrets included.
`,
	"--debug": `Usage: devopsy @<target> --debug [env [--show] [VAR...] | capabilities | labels | imports]

devopsy --debug, run in the current release on the server: without a topic,
the server's devopsy and docker versions and the project; env, where each
variable of --env comes from there (shared/.env, the instance's and
project's .env, target.env and what it imported, the env capability) and
what each of those overrides, secrets masked unless --show; capabilities, the
release's capabilities and their contracts; labels, the devopsy labels in
its compose files; imports, the imports of every project running on that
host, marking those whose release holds an outdated value as STALE.
environments and schema are about this machine's config: run them without a
target.
`,
	"--log": `Usage: devopsy @<target> --log [<release id>]

Every release and rollback is recorded as it runs and saved on the server
in the release's directory (releases/<id>/.devopsy-log): devopsy's
messages, the local steps run and how they ended, and everything the server
printed (steps, compose, the variables check). A rollback's is appended to
the log of the release it restores. Saved even when it fails, once the
server was reached; terminal colors and progress redraws are dropped. Logs
go with their releases when they are pruned.

  devopsy @prod --log                   the most recent one
  devopsy @prod --log 20261009113901    that release's (an id, or its start;
                                        devopsy @prod --releases lists them)

Local steps' own output is not in it: it stays on your screen, which
keeps their terminal (docker's progress, colors).
`,
	"--shell": `Usage: devopsy @<target> --shell [service] [exec options...] [-- command...]

A shell in a container of the current release, as devopsy --shell runs it
locally: the project's shell capability (capabilities/shell/open) if it has
one; otherwise bash (or sh) in the service named, else the one labeled
devopsy.shell=true in compose.yaml, else the only one running, as the
service's user or its devopsy.shell.user label. Options go to docker
compose exec, like --user root. After --, a command runs instead of the
shell, directly as docker compose exec runs it: devopsy @<target> --shell
-- drush status.
`,
	"--destroy": `Usage: devopsy @<target> --destroy [--yes]

Removes the environment from the server, under the release lock: runs the
environment's destroy step (destroy: in config.yaml, required) in its
current release, then removes its directory (releases, shared/ with its
data and .env). The step is the project's: usually a command that runs
docker compose down --volumes and removes what its containers own in
shared/mnt, which the deploy user cannot. A failed step removes nothing.
Asks for the target's name, unless --yes (CI, like when a pull request
closes: devopsy @pr-123 --destroy --yes). The project's and the instance's
.env stay.
`,
	"--shell-host": `Usage: devopsy @<target> --shell-host

Opens your login shell on the target's host, over SSH, in the current
release, or in the target's path for a plain devopsy directory. devopsy and
docker compose work there as on any project. It never depends on the
project: the way in when something is broken. For a shell in a container:
devopsy @<target> --shell.
`,
	"--ssh-config": `Usage: devopsy @<target> --ssh-config

Prints a Host block for the target's host, for ~/.ssh/config: Hostname,
User and Port as the target and ssh -G resolve them, then suggestions,
commented out: connection sharing (one connection for all of a release's
sessions instead of a handshake each) and keepalives. It never connects.
`,
	"--vars": `Usage: devopsy @<target> --vars [--project | --instance] [get KEY | set [--show] KEY... | unset KEY...]

The target's variables on the server: shared/.env for an environment with
releases (it can be set before the first release), or .devopsy/.env for a
plain directory. These are its secrets and overrides, linked into every
release; config.yaml's env goes to target.env instead.

  --project           the project's .env on that server instead, shared by
                      all its environments there (<root>/<project>/.env)
  --instance          the instance's, shared by its environments
                      (<root>/<project>/<instance>/.env; @<server>:<instance>/<env>)

Nearest wins: the caller, the environment's, the instance's, the project's,
then target.env.

  --vars              the names, values hidden
  --vars get KEY      one value, on stdout
  --vars set KEY...   each value from your environment, else the project's
                      .env (not for aliases), else a hidden
                      prompt, else stdin (one key only). Existing keys are
                      replaced in place, new ones appended.
    --show            echo what you type at the prompt, for values that are
                      not secrets
  --vars unset KEY... remove them

Values never go in arguments: they travel on SSH's stdin, so they stay out
of ps, shell history and CI logs. Running containers keep their old values
until they are recreated, as by the next release; how to recreate them
without one depends on the project.

Examples:
  devopsy @prod --vars set REGISTRY_USER REGISTRY_PASSWORD   # copy from local .env
  printf '%s' "$TOKEN" | devopsy @prod --vars set CF_DNS_API_TOKEN
  devopsy @prod --vars set --show LOG_LEVEL                  # visible prompt
  devopsy @vm1-traefik --vars
`,
}

// DebugHelp is `devopsy --debug --help`.
const DebugHelp = `Usage: devopsy --debug [topic]

What devopsy sees and computes. Without a topic: versions, the project,
environments and aliases. Topics:

  environments [name] [--yaml]
                 environments and aliases with where each value comes from
                 (any address resolved)
  env [--show] [VAR...]
                 the variables devopsy loads and computes (or the ones
                 named), with every place that defines each: the one in
                 use first, then the overridden ones. Secrets masked
                 unless --show
  capabilities   the capabilities devopsy calls and their contracts
  labels         the devopsy labels in the compose files
  imports        the imports of every project running on this host
  schema [--user]
                 every config key, commented: the project's, or with --user
                 this machine's
`

// Usage is devopsy's help. projectDir is "" outside a project.
func Usage(projectDir string) string {
	var b strings.Builder
	b.WriteString(`devopsy: docker compose for projects with a .devopsy/ directory.

Usage:
  devopsy <command> [args...]     a project command, else docker compose's
  devopsy @<target> <command>     the same on a server
  devopsy [@<target>] --<flag>    devopsy's own (below)
  devopsy [@<target>] -- <args>   docker compose <args>, past project commands
  devopsy -v|-vv ...              any of these, verbose (below)

devopsy:
  -v, -vv                before anything else: also what devopsy found and
                         runs, and SSH sessions; -vv also their scripts
                         (DEVOPSY_VERBOSE=1 or 2 does the same)
  --version              devopsy's, docker's and docker compose's versions
  --upgrade [v]          replace devopsy with the latest release, or v
  --completion <shell>   shell completion for bash, zsh or fish
  --help, -h             this help

Project:
  --env                  the variables devopsy loads and computes (.env format)
  --shell [service] [exec options...] [-- command...]
                         a shell, or the command, in a container
  --environments [server]
                         the project's environments on its servers
  --init [project]       a new .devopsy/config.yaml
  --context-hash [service]
                         a hash of what the service's image is built from
  --debug [topic]        what devopsy sees and computes (--debug --help)

`)
	b.WriteString(RemoteHelp)
	b.WriteString(`
Tools:
  --probe [--ip <ip>] <host>...
                         DNS, certificate and HTTPS of hosts, from here
                         (--probe --help)

`)
	if projectDir == "" {
		b.WriteString("Not in a devopsy project: no .devopsy/ in this directory or above.\n\n")
	} else {
		cmds := CustomCommands(projectDir)
		fmt.Fprintf(&b, "Project commands (%s):\n", filepath.Join(projectDir, "commands"))
		if len(cmds) == 0 {
			b.WriteString("  none\n")
		}
		width := 0
		for _, c := range cmds {
			width = max(width, len(c))
		}
		for _, c := range cmds {
			desc := CommandDescription(filepath.Join(projectDir, "commands", c))
			fmt.Fprintf(&b, "  %-*s  %s\n", width, c, desc)
		}
		b.WriteString(`
Other words are docker compose's commands, with the project's files
(devopsy up -d, devopsy logs -f <service>...); anything else is an error.

`)
	}
	b.WriteString("More in the README: https://github.com/hanoii/devopsy-cli\n")
	return b.String()
}

// loadedProject is a project with its environment loaded.
type loadedProject struct {
	dir, composeFile, dotenvFile string
	env                          *Env
	verbose                      []string
}

// loadProject finds the project from cwd and loads its environment over the
// caller's.
func loadProject(cwd string, environ []string) (*loadedProject, error) {
	projectDir, err := FindProjectDir(cwd)
	if err != nil {
		return nil, err
	}
	composeFile := filepath.Join(projectDir, "compose.yaml")
	if _, err := os.Stat(composeFile); err != nil {
		return nil, &ExitError{Code: 1, Msg: composeFile + " not found."}
	}

	env := NewEnv(environ)
	env.Set("DEVOPSY_PROJECT_DIR", projectDir)
	env.first["DEVOPSY_PROJECT_DIR"] = []EnvDef{{Kind: EnvDevopsy, Source: "where it found " + ProjectDirName + "/", Value: projectDir}}
	verbose := []string{"devopsy: project " + projectDir}
	load := func(file string) error {
		if err := LoadDotenv(env, file); err != nil {
			return err
		}
		if _, err := os.Stat(file); err == nil {
			verbose = append(verbose, "devopsy: loaded "+file)
		}
		return nil
	}
	dotenvFile := filepath.Join(projectDir, ".env")
	if err := load(dotenvFile); err != nil {
		return nil, err
	}
	// On servers, the instance's and the project's .env, shared by their
	// environments: links releases get, nearest first.
	for _, shared := range []string{"instance.env", "project.env"} {
		if err := load(filepath.Join(projectDir, shared)); err != nil {
			return nil, err
		}
	}
	// Per-target settings, written into each release by `devopsy @target
	// --release` from config.yaml. Below .env, so a server can override them.
	targetEnv := filepath.Join(projectDir, "target.env")
	if err := load(targetEnv); err != nil {
		return nil, err
	}
	// COMPOSE_PROJECT_NAME is always set, so commands and compose agree on
	// it: releases write it into target.env (<project>[-<instance>]-<environment>);
	// locally it is the config's project, else compose's name:, else the
	// directory containing .devopsy (compose would take .devopsy itself).
	if name, _ := env.Lookup("COMPOSE_PROJECT_NAME"); name == "" {
		project, err := remote.LoadProject(projectDir)
		if err != nil {
			return nil, &ExitError{Code: 1, Msg: err.Error()}
		}
		from := "project: in " + filepath.Join(projectDir, remote.ConfigFile)
		if project != nil && project.Name != "" {
			name = project.Name
		} else {
			from = "name: in " + composeFile
			raw, err := TopLevelName(composeFile)
			if err != nil {
				return nil, err
			}
			if name, err = template.Substitute(raw, env.Lookup); err != nil {
				return nil, &ExitError{Code: 1, Msg: fmt.Sprintf("%s: name: %v", composeFile, err)}
			}
			if name == "" {
				name = filepath.Base(filepath.Dir(projectDir))
				from = "the name of the directory with " + ProjectDirName + "/"
			}
		}
		env.Set("COMPOSE_PROJECT_NAME", NormalizeProjectName(name))
		env.first["COMPOSE_PROJECT_NAME"] = []EnvDef{{Kind: EnvDevopsy, Source: from, Value: NormalizeProjectName(name)}}
	}
	env.Mark("COMPOSE_PROJECT_NAME")
	env.Mark("DEVOPSY_PROJECT_DIR")
	return &loadedProject{dir: projectDir, composeFile: composeFile, dotenvFile: dotenvFile, env: env, verbose: verbose}, nil
}

// TraceEnv explains the project's environment as every devopsy command
// gets it, for `devopsy --debug env`: the variables devopsy loads and
// computes (what --env prints), or the ones named, each with every place
// that defines it. The Secrets mask what devopsy never prints.
func TraceEnv(cwd string, environ, names []string) (string, []EnvVar, *Secrets, error) {
	p, err := loadProject(cwd, environ)
	if err != nil {
		return "", nil, nil, err
	}
	if err := p.computeEnv(); err != nil {
		return "", nil, nil, err
	}
	if len(names) == 0 {
		names = p.env.Marked()
	}
	secrets := NewSecrets(p.env, p.dotenvFile)
	var vars []EnvVar
	for _, k := range names {
		defs := p.env.trace(k)
		// Overridden values are secrets like the ones in use.
		for _, d := range defs {
			if d.Source == p.dotenvFile || secretName.MatchString(k) {
				secrets.Add(d.Value)
			}
		}
		vars = append(vars, EnvVar{Name: k, Defs: defs})
	}
	return p.dir, vars, secrets, nil
}

// Build decides what to run for args (without argv[0]), from cwd and the
// caller's environment.
func Build(cwd string, args []string, environ []string) (*Plan, error) {
	p, err := loadProject(cwd, environ)
	if err != nil {
		return nil, err
	}
	projectDir, env, verbose, dotenvFile := p.dir, p.env, p.verbose, p.dotenvFile

	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return nil, &Help{Text: Usage(projectDir), Code: 0}
	}
	if err := p.computeEnv(); err != nil {
		return nil, err
	}
	verbose = p.verbose
	switch args[0] {
	case "--env":
		var b strings.Builder
		for _, k := range env.Marked() {
			v, _ := env.Lookup(k)
			b.WriteString(DotenvLine(k, v) + "\n")
		}
		return nil, &Output{Text: b.String()}
	case "--context-hash":
		if len(args) > 2 {
			return nil, &ExitError{Code: 1, Msg: "usage: devopsy --context-hash [service]"}
		}
		service := ""
		if len(args) == 2 {
			service = args[1]
		}
		hash, err := ContextHash(p, service)
		if err != nil {
			return nil, &ExitError{Code: 1, Msg: "--context-hash: " + err.Error()}
		}
		return nil, &Output{Text: hash + "\n"}
	case "--capability":
		return capability(p, args[1:])
	case "--missing-vars":
		return nil, missingVars(p)
	case "--shell":
		return shell(p, args[1:])
	case "--":
		// The escape hatch: straight to docker compose, past project
		// commands and the check for compose's commands.
		plan := p.compose(args[1:])
		secrets := NewSecrets(env, dotenvFile)
		plan.Notice = secrets.Mask(fmt.Sprintf("Running '%s'...", strings.Join(plan.Args, " ")))
		plan.Verbose = maskAll(secrets, verbose)
		return plan, nil
	}

	// A custom command can call `devopsy <same name>` to reach the compose
	// command it wraps, without recursing into itself.
	current, _ := env.Lookup("DEVOPSY_CLI_COMMAND")
	custom := filepath.Join(projectDir, "commands", args[0])
	secrets := NewSecrets(env, dotenvFile)
	if current != args[0] && isExecutableFile(custom) {
		env.Set("DEVOPSY_CLI_COMMAND", args[0])
		cmdArgs := append([]string{custom}, args[1:]...)
		return &Plan{
			Path:    custom,
			Args:    cmdArgs,
			Env:     env.Environ(),
			Verbose: maskAll(secrets, append(verbose, "devopsy: running '"+strings.Join(cmdArgs, " ")+"'")),
		}, nil
	}

	// A word is a project command, else a docker compose command, else a
	// mistake: say so, rather than compose's usage. Flags first (compose's
	// global ones, like --profile) go to compose unchecked.
	if !strings.HasPrefix(args[0], "-") && !isComposeCommand(args[0], env.Environ()) {
		return nil, &ExitError{Code: 1, Msg: unknownWord(args[0], projectDir)}
	}

	plan := p.compose(args)
	plan.Notice = secrets.Mask(fmt.Sprintf("Running '%s'...", strings.Join(plan.Args, " ")))
	plan.Verbose = maskAll(secrets, verbose)
	return plan, nil
}

// capabilityName is a capability's or action's name: a file name, never a
// path.
var capabilityName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// capability plans `devopsy --capability <name> <action> [args...]`: the
// executable capabilities/<name>/<action>, with the project's environment.
// devopsy calls it, people never do: hidden from help and completion.
func capability(p *loadedProject, args []string) (*Plan, error) {
	if len(args) < 2 {
		return nil, &ExitError{Code: 1, Msg: "usage: devopsy --capability <name> <action> [args...]"}
	}
	name, action := args[0], args[1]
	if !capabilityName.MatchString(name) || !capabilityName.MatchString(action) {
		return nil, &ExitError{Code: 1, Msg: fmt.Sprintf("--capability: not a capability and action: %q %q", name, action)}
	}
	file := filepath.Join(p.dir, "capabilities", name, action)
	if !isExecutableFile(file) {
		return nil, &ExitError{Code: 1, Msg: fmt.Sprintf("%s does not provide the %s capability's %s action (no executable %s)", filepath.Dir(p.dir), name, action, file)}
	}
	cmdArgs := append([]string{file}, args[2:]...)
	secrets := NewSecrets(p.env, p.dotenvFile)
	return &Plan{
		Path:    file,
		Args:    cmdArgs,
		Env:     p.env.Environ(),
		Verbose: maskAll(secrets, append(p.verbose, "devopsy: running '"+strings.Join(cmdArgs, " ")+"'")),
	}, nil
}

// compose plans `docker compose` with the project's files and args.
func (p *loadedProject) compose(args []string) *Plan {
	composeArgs := []string{"docker", "compose", "-f", p.composeFile}
	for _, name := range []string{"compose.override.yaml", "compose.override.yml"} {
		override := filepath.Join(p.dir, name)
		if fi, err := os.Stat(override); err == nil && fi.Mode().IsRegular() {
			composeArgs = append(composeArgs, "-f", override)
			break
		}
	}
	return &Plan{Path: "docker", Args: append(composeArgs, args...), Env: p.env.Environ()}
}

// Compose plans `docker compose` args with the project's files and
// environment, never a project command, and prints nothing: for shell
// completion.
func Compose(cwd string, args []string, environ []string) (*Plan, error) {
	p, err := loadProject(cwd, environ)
	if err != nil {
		return nil, err
	}
	return p.compose(args), nil
}

func maskAll(s *Secrets, lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = s.Mask(l)
	}
	return out
}

// Fprint writes msg to w, in color when color is set.
func Fprint(w io.Writer, color string, msg string, useColor bool) {
	if useColor && color != "" {
		fmt.Fprintf(w, "%s%s\033[0m\n", color, msg)
		return
	}
	fmt.Fprintln(w, msg)
}
