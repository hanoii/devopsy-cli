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
	"slices"
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
	// NoticeFull replaces Notice with -vv, when Notice leaves a script out.
	NoticeFull string
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

// Unset removes a variable.
func (e *Env) Unset(k string) {
	if _, ok := e.values[k]; !ok {
		return
	}
	delete(e.values, k)
	delete(e.caller, k)
	e.keys = slices.DeleteFunc(e.keys, func(s string) bool { return s == k })
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
	return commandHeader(path, "## Description:")
}

// commandHeader reads a command's `## <Key>:` line from the top of the file.
func commandHeader(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.SplitN(string(data), "\n", 40)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) > len(key) && strings.EqualFold(line[:len(key)], key) {
			return strings.TrimSpace(line[len(key):])
		}
	}
	return ""
}

// RemoteCommandHelp is the detailed help of each `devopsy @<target>`
// subcommand, shown by `devopsy @<target> <subcommand> --help`.
var RemoteCommandHelp = map[string]string{
	"--sync": `Usage: devopsy @<target> --sync [--from @<target>] [--yes] [-- args...]

Replaces a child environment's data (shared/mnt, and what its services
hold) with its parent's, then runs its release's run step. A child is an
environment with parent: in config.yaml; its first release syncs on its
own, between switching current and the run step (--release-no-sync skips
that), so --sync is for resyncing later. Nothing goes through this
machine: the parent's server copies into the child's.

  environments:
    prod:
      inherit: [SYNC_DATABASE]   # variables every child gets
    dev:
      parent: prod

  --from @<target>  copy from another environment of the project instead
                    (@vm1:prod, @a/prod...), with its inherit:
  --yes             do not ask for the environment's name (CI)
  -- args...        DEVOPSY_SYNC_ARGS: rsync's arguments (--exclude=cache),
                    and the sync capability's to read

Services opt in, with labels on compose services, read by each side from
its own release:

  devopsy.sync=rsync   its entries of .devopsy/mnt rsynced (rsync -a
                       --delete --numeric-ids, as root in devopsy's own
                       image, devopsy-util, built on each server)
  devopsy.sync=stream  the parent's sync capability's export writes it to
                       stdout, into a file on the child's side; once the
                       export succeeded, the child's import reads it (a
                       database dumped and loaded)
  (no label)           neither copied nor touched
  devopsy.sync.stop=true
                       stopped on both sides for the whole sync, a clean
                       shutdown (a database's files); default false: a
                       running service is copied as it is
  devopsy.sync.rsync.exclude=<entry>[,<entry>...]
                       entries of mnt an rsync service mounts that are not
                       copied through it anyway (an access log)

What runs where:
  - the child's server, under its release lock: the prepare child hook,
    its stop=true services stopped, then it waits for the copy; after
    it, they start (and become healthy), the finish child hook runs,
    then the run step. A failed copy restores them and skips the run
    step.
  - the parent's server, under a shared hold of its lock (its releases
    wait): prepare parent, its stop=true services stopped, the rsync,
    each stream (export, then the child's import: its service brought
    up first unless stopped), its services started, finish parent.

Without any labeled service there is nothing to sync; the run step still
runs.

On one server, one container copies between the two directories. Across
servers, the parent's runs an rsync daemon for the copy and reaches the
child's over SSH with your agent, forwarded for that session only: an agent
must be running locally (in CI too), and the child's host must be reachable
from the parent's as ssh -G resolves it locally (no ProxyCommand or ProxyJump).
A CI key needs restrict,agent-forwarding,port-forwarding (--ssh-keys add --restricted).

The parent comes from the child's instance and the usual server variables
(DEVOPSY_SERVER_<PARENT>, DEVOPSY_SERVER), else the child's server. Before
copying, the variables the parent's inherit: names are copied from its
server's .env files into the child's, at the same level (shared/.env, the
instance's or the project's .env); names are printed, never values. On a
re-sync they go to a pending file the sync reads (labels from them follow
the parent's at once) and replace shared/inherit.env once the copy is in.

The sync capability (capabilities/sync): prepare and finish on both
sides, export on the parent's and import on the child's for the stream
services (devopsy --debug capabilities). Asks for the environment's name,
unless --yes. The log goes with the current release: devopsy @<target>
--log. One service alone, from anywhere to anywhere: --export, --import.
`,
	"--export": `Usage: devopsy [@<target>] --export <service>

Writes a service's data to stdout, the way its devopsy.sync label says;
everything devopsy says goes to stderr. With a target, in its current
release on the server, under a shared hold of its release lock (a
release waits) and its sync lock (a sync from it waits), the data coming
through SSH.

  devopsy.sync=stream   the sync capability's export <service> (a dump)
  devopsy.sync=rsync    a tar of the entries of .devopsy/mnt it mounts
                        (devopsy.sync.rsync.exclude honored), made as root
                        in devopsy-util with numeric owners and modes. A
                        service mounting ./mnt itself exports all of it,
                        other services' and excluded entries included
                        (devopsy says which)
  devopsy.sync.stop=true  stopped around it, started again afterwards

A service without the label is refused. --import reads what --export
writes, so data goes from one environment to another without a project
command:

  devopsy @prod --export database | devopsy --import database
  devopsy @prod --export storage > storage.tar
  devopsy --export database > database.sql
`,
	"--import": `Usage: devopsy [@<target>] --import <service> [--yes]

Replaces a service's data with stdin, what --export wrote, the way its
devopsy.sync label says. stdin goes to a file first (under .devopsy/
locally, next to the environment on a server); only once it is all there
does anything change, and the file is removed afterwards.

  devopsy.sync=stream   the sync capability's import <service> reads it,
                        with the service brought up first unless stopped
                        (and down again after it if it was down)
  devopsy.sync=rsync    a tar: each entry of mnt it carries is emptied,
                        then extracted as root in devopsy-util, owners and
                        modes as they were; the service may run meanwhile.
                        A truncated tar, or one carrying entries the
                        service does not mount, changes nothing; one of
                        ./mnt itself replaces all of mnt (devopsy says so)
  devopsy.sync.stop=true  stopped around it, started again afterwards

With a target, asks for the environment's name on the terminal first
(--yes skips it, and is required without a terminal), then imports under
its release lock; a session cut halfway imports nothing. A pipe cannot
tell an export that failed halfway from one that ended, so a stream
imports what it got (a tar is checked): when the export can fail, export
to a file, check its exit code, then import that.

  devopsy @prod --export database | devopsy --import database
  devopsy @staging --import database --yes < database.sql
`,
	"--inherit": `Usage: devopsy @<target> --inherit

Writes the variables the parent's inherit: names, read from the parent's
server, as the child's shared/inherit.env again: a resync of variables
without a copy of data and without the run step. A sync does the same
along with the data. The child's own shared/.env wins over the inherited
file; its server's instance and project .env come below it.

  environments:
    prod:
      inherit: [SYNC_DATABASE, DB_PASSWORD]
    dev:
      parent: prod
`,
	"--release": `Usage: devopsy @<target> --release [--from @<target>] [--yes]
       devopsy @<target> --release-sync [--from @<target>] [--yes]
       devopsy @<target> --release-no-sync
       devopsy --release

Uploads the project to the target as a new release, makes it current and
runs the target's release steps from .devopsy/config.yaml (required), in
phases:

  environments:
    prod:
      release:
        before:                # before anything changes; a failure stops
          - local: image       #   locally, first
          - remote: maint on   #   on the server, in the current release
        prepare: [secrets]     # on the server, in the new release before it
                               # goes live (then: the variables check)
        run: deploy            # once it is current: the release itself
        after:                 # once live; a failure is only reported
          - remote: maint off  #   on the server first
          - local: notify      #   then locally
      # or only the run step:
      # release: deploy

After prepare, devopsy checks that every variable compose requires
(${VAR:?message}) is set, with the env capability included; a missing one
fails the release before it goes live, listing what to set (--vars set).
Then devopsy gets the images, before the release goes live: in build mode
it pulls the services without a build and builds the others (--pull), in
image mode it pulls them all, so a tag missing from the registry fails
there. The run step only starts them. Each release
keeps its images under a tag of its own (devopsy-<compose name>-<id>), so
a rollback runs exactly what that release ran, without building or
pulling. When run fails, current goes back to the previous release, which
gets its own images back and is restarted with its own run step, from
its own config on the server (its rollback:, else its release:).

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
user-level config's releases: root, else the deploy user's home). It keeps the target's or project's releases: keep,
else the server's, at most the server's max_keep (all default 5),
counting only releases that went live: failed ones newer than the oldest
kept stay, with their logs.

Before anything runs, every variable the compose files use or the target's
env sets that is named like a secret (PASSWORD, TOKEN, KEY...) must be in
config.yaml's secrets: (masked) or public: (not a secret): otherwise
devopsy asks, and fails without a terminal or with --yes.

The first release of an instance the server does not have yet asks before
creating it (a typo would otherwise make a new site); --yes skips that, for
CI.

The first release of a child (an environment with parent:) syncs its
parent's data once current, before its run step, so the run step works on
that data and no empty site is served; a failed copy does not go live.
--release-sync does that on any release, asking for the environment's
name when it already has a current release (--yes skips it): new code
and fresh data with one run step. --release-no-sync never syncs, a
child's first release included (devopsy @<target> --sync --help).
--from @<target> copies from another environment of the project instead
of the parent, as --sync --from does: for a parent whose server only the
address names. Only on a release that syncs.

Without a target, --release runs in place, in the local project: the
release steps under defaults: in config.yaml, prepare, the variables
check, the images (always as build mode: what has a build is built, the
rest pulled), then run. No upload, no switch and nothing to go back to;
no before or after steps, which are the operator's side of a deploy.
`,
	"--rollback": `Usage: devopsy @<target> --rollback

Makes the release before the current one current again (skipping failed
ones) and runs its rollback steps, from the restored release's own
config.yaml on the server, not the checkout's (required there; the
checkout's local before and after steps run locally), in release's phases: before, prepare and the variables check in the restored
release (a failure changes nothing), run once it is current (a failure goes
back again), after. No upload, and no new imports: the restored release
keeps what it had. Usually the same run step as release:

  environments:
    prod:
      rollback: deploy

The steps and their commands are the restored release's, so they must
exist there.
Rolling back restores that release's files and target env, not data.
`,
	"--releases": `Usage: devopsy @<target> --releases

Lists the releases on the target, newest first: id, who made it, mode,
branch and commit (+dirty when made with uncommitted changes), and FAILED for
releases whose command failed. * marks the current one.
`,
	"--env": `Usage: devopsy @<target> --env [--show]

The variables devopsy loads and computes in the current release, as
devopsy --env prints them there: shared/.env, the instance's and project's
.env, target.env and the project's env capability (capabilities/env/compute,
from the release on the server, not your checkout). The values of the
variables the release's config.yaml names in secrets: print as ***, unless
--show.
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
       devopsy --destroy [--yes]

Removes the environment from the server, under the release lock, in its
current release: the environment's destroy step if it has one (destroy:
in config.yaml, optional: one devopsy command, while everything runs),
then docker compose down with volumes and the images built without an
image: name, then shared/mnt emptied as root in devopsy-util; then each
release's image tags, then its directory (releases, shared/ with its data
and .env). A failure before the removal removes nothing. Asks for the
target's name, unless --yes (CI, like when a pull request
closes: devopsy @pr-123 --destroy --yes). The project's and the instance's
.env stay.

Without a target, --destroy does the same in place, with the destroy
step under defaults:, asking for the project's name unless --yes:
containers, volumes, built images and .devopsy/mnt's data. The checkout
stays.
`,
	"--shell-host": `Usage: devopsy @<server> --shell-host
       devopsy @<target> --shell-host

Opens your login shell on the server, over SSH: in its release root for a
server named alone, else in the environment's current release, or its
directory for a plain devopsy directory. devopsy and docker compose work
there as on any project. It never depends on the project: the way in when
something is broken. For a shell in a container: devopsy @<target> --shell.
`,
	"--environments": `Usage: devopsy @<server> --environments
       devopsy --environments

The project's environments on the server, as addresses: <environment>, or
<instance>/<environment> for a project with instances: true. The project is
the checkout's, or an alias's. Without a target, on every server
DEVOPSY_SERVER and the environments' DEVOPSY_SERVER_<ENVIRONMENT> name (the
caller's environment, then .devopsy/.env).
`,
	"--upgrade": `Usage: devopsy @<server> --upgrade [vX.Y.Z]

Replaces the server's devopsy with the latest release, or vX.Y.Z, as the
deploy user, who owns it. devopsy --upgrade does the same locally.
`,
	"--ssh-config": `Usage: devopsy @<server> --ssh-config

Prints a Host block for the server, for ~/.ssh/config: Hostname,
User and Port as the target and ssh -G resolve them, then suggestions,
commented out: connection sharing (one connection for all of a release's
sessions instead of a handshake each) and keepalives. It never connects.
`,
	"--vars": `Usage: devopsy @<target> --vars [--show | get KEY | set [--show] KEY... | unset KEY...]
       devopsy @<target> --vars-instance [...]
       devopsy @<server> --vars-project [...]

The target's variables on the server: shared/.env for an environment with
releases (it can be set before the first release), or .devopsy/.env for a
plain directory. These are its secrets and overrides, linked into every
release; config.yaml's env goes to target.env instead.

  --vars-instance     the instance's .env instead, shared by its
                      environments (<root>/<project>/<instance>/.env)
  --vars-project      the project's .env on the server, shared by all its
                      environments there (<root>/<project>/.env); the
                      project is the checkout's or an alias's, so a server
                      alone does (@vm1)

Nearest wins: the caller, the environment's, the instance's, the project's,
then target.env.

  --vars [--show]     every variable, secrets (config.yaml's secrets:,
                      the release's or your checkout's) as *** unless
                      --show
  --vars get KEY      one value, on stdout
  --vars set KEY...   each value from your environment, else the project's
                      .env (not for aliases), else a hidden
                      prompt, else stdin (one key only). Existing keys are
                      replaced in place, new ones appended. A key named like
                      a secret that config.yaml's secrets: and public: do
                      not list asks first (fails without a terminal).
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
  devopsy @b/prod --vars-instance set SITE_ID                # the instance's
  devopsy @vm1 --vars-project set SHARED_KEY                 # the project's
`,
	"--status": `Usage: devopsy [@<target>] --status

What is running. In a project: its containers (every one, state and
health, as docker compose ps sees them), then what the project's status
capability prints (.devopsy/capabilities/status/show), its URL, say.
With a target: the current release there (id, commit, when, by whom, how
many releases are kept), the parent and the last sync for a child, then
the same from the current release on the server.

Examples:
  devopsy --status
  devopsy @prod --status
`,
	"--ssh-keys": `Usage: devopsy @<server> --ssh-keys [list | add <public key file> [--restricted] | remove <fingerprint or comment>]
       devopsy --ssh-keys-generate [<name>] [--out <file>]

The keys that may log in as the deploy user on the server: its
~/.ssh/authorized_keys, edited over the usual session as that user, no
root. Any target on the server does too; the keys are the server's.

  --ssh-keys                list them: fingerprint, type, comment, options
  --ssh-keys add <file>     authorize the public key in <file> (- for stdin)
    --restricted            for a machine's key, like CI's: restrict
                            (commands only, no shell, no X11), with
                            agent-forwarding and port-forwarding, which a
                            sync across servers needs. A person's key is
                            added plain.
  --ssh-keys remove <key>   remove the key with that fingerprint (a prefix
                            will do) or that exact comment. The last key is
                            never removed: it would lock everyone out.

--ssh-keys-generate, without a target, makes a local ed25519 pair with ssh-keygen,
~/.ssh/<name> and .pub (default devopsy-ci; --out another file), the
comment the name. Nothing is sent anywhere: add the .pub to each server.

For CI: generate one, add the .pub with --restricted, store the private key as
a protected file variable, and in the job load it into an agent (eval
"$(ssh-agent)"; ssh-add "$DEVOPSY_SSH_KEY") before devopsy @<target>
--release. Rotate by generating another, adding it, then removing the old
one.

Examples:
  devopsy @prod --ssh-keys
  devopsy --ssh-keys-generate ci-prod1
  devopsy @prod --ssh-keys add ~/.ssh/ci-prod1.pub --restricted
  devopsy @prod --ssh-keys add ~/.ssh/id_ed25519.pub
  devopsy @prod --ssh-keys remove ci-prod1
`,
}

func init() {
	// The three releases share one explanation.
	RemoteCommandHelp["--release-sync"] = RemoteCommandHelp["--release"]
	RemoteCommandHelp["--release-no-sync"] = RemoteCommandHelp["--release"]
	RemoteCommandHelp["--vars-project"] = RemoteCommandHelp["--vars"]
	RemoteCommandHelp["--ssh-keys-generate"] = RemoteCommandHelp["--ssh-keys"]
	RemoteCommandHelp["--vars-instance"] = RemoteCommandHelp["--vars"]
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
  devopsy [@<target>] <command> [args...]   a project command, else compose's
  devopsy [@<target>] --<flag>              devopsy's own, below
  devopsy [@<target>] -- <args>             docker compose <args>, always

Where each runs:
    @t   on a target: @[<server>:][<instance>/]<environment>, or an alias
    @s   on a server: @<server> (@<server>: when a name is ambiguous), or any
         target's
  [@t]   optional: without it, on your machine, in the project you are in

`)
	if projectDir == "" {
		b.WriteString("Not in a devopsy project: no .devopsy/ in this directory or above.\n\n")
	} else {
		cmds := CustomCommands(projectDir)
		fmt.Fprintf(&b, "Project commands (%s):\n", filepath.Join(projectDir, "commands"))
		if len(cmds) == 0 {
			helpLine(&b, "", "none", "")
		}
		for _, c := range cmds {
			path := filepath.Join(projectDir, "commands", c)
			run, _ := ReadCommandRun(path)
			typed := c
			if run.RunsLocal() {
				// The prefix is an address: say it still runs locally.
				typed += " " + RunsLocallyNote
			}
			helpLine(&b, run.Scope().prefix(), typed, CommandDescription(path))
		}
		b.WriteString(`Any other word is docker compose's, with the project's files: devopsy up -d,
devopsy logs -f <service>; anything else is an error. Compose's own help, as
devopsy's: devopsy -- --help, devopsy <command> --help.

`)
	}
	b.WriteString(flagsHelp())
	b.WriteString("Details of each: devopsy [@<target>] <flag> --help\n")
	b.WriteString("More in the README: https://github.com/hanoii/devopsy-cli\n")
	return b.String()
}

// loadedProject is a project with its environment loaded.
type loadedProject struct {
	dir, composeFile, dotenvFile string
	env                          *Env
	verbose                      []string
	// secrets are config.yaml's secrets: names.
	secrets []string
	// address is a local command's, when the devopsy that read it gave one.
	address   string
	addressed bool
}

// newSecrets masks the project's secrets as loaded.
func (p *loadedProject) newSecrets() *Secrets { return NewSecrets(p.env, p.secrets) }

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
	// The address of a local command that needs one, from the devopsy
	// that read it: for that command only, never its children.
	address, addressed := env.Lookup(commandTargetInternal)

	env.Unset(commandTargetInternal)
	env.Unset(CommandTargetEnv)
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
	// On servers, a child's inherited variables (shared/inherit.env, below
	// its own .env), then the instance's and the project's .env, shared by
	// their environments: links releases get, nearest first.
	for _, shared := range []string{remote.InheritFile, "instance.env", "project.env"} {
		file := filepath.Join(projectDir, shared)
		// During a re-sync on the child, its pending inherited variables
		// stand in for inherit.env (the labels the sync reads come from
		// them), when the sync wrote any.
		if p, _ := env.Lookup(remote.InheritPendingEnv); shared == remote.InheritFile && p != "" {
			if _, err := os.Stat(p); err == nil {
				file = p
			}
		}
		if err := load(file); err != nil {
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
	project, err := remote.LoadProject(projectDir)
	if err != nil {
		return nil, &ExitError{Code: 1, Msg: err.Error()}
	}
	var secrets []string
	if project != nil {
		secrets = project.Secrets
	}
	if name, _ := env.Lookup("COMPOSE_PROJECT_NAME"); name == "" {
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
	return &loadedProject{dir: projectDir, composeFile: composeFile, dotenvFile: dotenvFile, env: env, verbose: verbose, secrets: secrets, address: address, addressed: addressed}, nil
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
	secrets := p.newSecrets()
	var vars []EnvVar
	for _, k := range names {
		defs := p.env.trace(k)
		// Overridden values are secrets like the ones in use.
		for _, d := range defs {
			if secrets.Named(k) {
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
	projectDir, env, verbose := p.dir, p.env, p.verbose

	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return nil, &Help{Text: Usage(projectDir), Code: 0}
	}
	if err := p.computeEnv(); err != nil {
		return nil, err
	}
	verbose = p.verbose
	switch args[0] {
	case "--env":
		secrets := p.newSecrets()
		for _, a := range args[1:] {
			if a != "--show" {
				return nil, &ExitError{Code: 1, Msg: "usage: devopsy --env [--show]"}
			}
			secrets = nil
		}
		var b strings.Builder
		for _, k := range env.Marked() {
			v, _ := env.Lookup(k)
			// Masked before quoting: quoting can escape a secret out of sight.
			b.WriteString(DotenvLine(k, secrets.Value(k, v)) + "\n")
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
		secrets := p.newSecrets()
		plan.running(secrets)
		plan.Verbose = maskAll(secrets, verbose)
		return plan, nil
	}

	// A custom command can call `devopsy <same name>` to reach the compose
	// command it wraps, without recursing into itself.
	current, _ := env.Lookup("DEVOPSY_COMMAND_NAME")
	custom := filepath.Join(projectDir, "commands", args[0])
	secrets := p.newSecrets()
	if current != args[0] && isExecutableFile(custom) {
		where := Bare
		switch {
		case InRelease(projectDir):
			where = OnServer
		case p.addressed:
			where = Addressed
		}
		run, err := CheckCommand(projectDir, args[0], where)
		if err != nil {
			return nil, &ExitError{Code: 1, Msg: err.Error()}
		}
		if where == Addressed && run.Needs != "" {
			env.Set(CommandTargetEnv, p.address)
		}
		env.Set("DEVOPSY_COMMAND_NAME", args[0])
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
	// global ones, like --profile) go to compose unchecked, except a flag
	// alone: compose's never stand alone, so it is devopsy's or a typo.
	if !strings.HasPrefix(args[0], "-") && !isComposeCommand(args[0], env.Environ()) {
		return nil, &ExitError{Code: 1, Msg: unknownWord(args[0], projectDir)}
	}
	if msg := LoneUnknownFlag(args); msg != "" {
		return nil, &ExitError{Code: 1, Msg: msg}
	}

	plan := p.compose(args)
	plan.running(secrets)
	plan.Verbose = maskAll(secrets, verbose)
	return plan, nil
}

// running sets the plan's notice: the command line, with an argument of
// several lines (a script, as in `run ... sh -c '<script>'`) only counted;
// -vv shows it whole, as it does the scripts of SSH sessions.
func (plan *Plan) running(secrets *Secrets) {
	short := make([]string, len(plan.Args))
	script := false
	for i, a := range plan.Args {
		short[i] = a
		if n := strings.Count(strings.TrimSpace(a), "\n"); n > 0 {
			short[i] = fmt.Sprintf("<script: %d lines>", n+1)
			script = true
		}
	}
	plan.Notice = secrets.Mask(fmt.Sprintf("Running '%s'...", strings.Join(short, " ")))
	if script {
		plan.NoticeFull = secrets.Mask(fmt.Sprintf("Running '%s'...", strings.Join(plan.Args, " ")))
	}
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
	secrets := p.newSecrets()
	return &Plan{
		Path:    file,
		Args:    cmdArgs,
		Env:     p.env.Environ(),
		Verbose: maskAll(secrets, append(p.verbose, "devopsy: running '"+strings.Join(cmdArgs, " ")+"'")),
	}, nil
}

// ColorEnv set means the output ends on a terminal that takes colors,
// even through a pipe: devopsy on a server, whose session has none, so
// its messages and Compose's output (--ansi always) come colored to the
// operator. Internal: devopsy exports it into every session when it
// colors locally; what a person sets is FORCE_COLOR (ColorForced).
const ColorEnv = "DEVOPSY_INTERNAL_COLOR"

// ColorForced reports whether colors are wanted without a terminal:
// ColorEnv, or FORCE_COLOR set to anything but 0 or false (chalk's
// convention, honored by many tools). NO_COLOR, checked by the callers,
// wins over both.
func ColorForced(lookup func(string) (string, bool)) bool {
	if v, _ := lookup(ColorEnv); v != "" {
		return true
	}
	v, _ := lookup("FORCE_COLOR")
	return v != "" && v != "0" && !strings.EqualFold(v, "false")
}

// compose plans `docker compose` with the project's files and args.
func (p *loadedProject) compose(args []string) *Plan {
	composeArgs := []string{"docker", "compose"}
	if noColor, _ := p.env.Lookup("NO_COLOR"); noColor == "" && ColorForced(p.env.Lookup) {
		composeArgs = append(composeArgs, "--ansi", "always")
	}
	composeArgs = append(composeArgs, "-f", p.composeFile)
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
