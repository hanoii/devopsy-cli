# AGENTS.md

devopsy-cli is `devopsy`, a Go CLI that runs Docker Compose projects with a
`.devopsy/` directory, locally and on servers over SSH. `README.md` is the
user-facing behavior: keep it in sync with every change. The workspace
(`../devopsy/README.md`, `../devopsy/ROADMAP.md`) describes how the
repositories fit together and the open ideas.

## Layout

- `cmd/devopsy/`: `main`, which executes what `internal/cli` plans, and
  end-to-end tests that run the built binary against a fake `docker`.
- `internal/cli/`: finds the project, loads `.env` and decides what to run
  (`Build` returns a `Plan`; it only runs helpers, like the `env`
  capability, never the command); roles, exports and imports
  (`roles.go`); capabilities.
- `internal/remote/`: config and targets (`config.go`), the documented
  schemas (`schema.go`), packing a release, and the POSIX `sh` scripts run on
  servers (checked with `sh -n` in tests; GNU coreutils, `mv -T`).
- `internal/complete/`: completion candidates, and the bash, zsh and fish
  scripts (embedded).
- `internal/probe/`, `internal/update/`: `--probe`, `--upgrade`.
- `install.sh` (POSIX `sh`, dash and busybox ash), `scripts/devopsy` (runs
  the checkout's code), `.goreleaser.yaml`, `.github/workflows/`.

## Rules

- **Compose plus environment variables.** devopsy never generates or
  rewrites compose files; everything it computes shows in `devopsy --env`.
  If a feature needs to change compose files, it is the wrong feature.
- **Compatibility of the basics:** project discovery, `.env` loading
  (compose's parser, caller's environment wins), commands in `commands/`
  with `DEVOPSY_PROJECT_DIR` and `DEVOPSY_CLI_COMMAND`, the recursion guard,
  everything else passed to compose untouched. No migration code for
  anything else: older files and syntax just stop working.
- devopsy replaces itself with what it runs (`syscall.Exec`): signals, the
  terminal and exit codes belong to that command.
- **Output:** stdout is the result (compose's output, `--env`, `--vars
  get`, `--context-hash`...), stderr is everything devopsy says: progress,
  notices and errors, the server scripts' `echo` lines included (`>&2`), so
  pipes stay clean and the two never interleave out of order. Anything that
  can contain arguments or scripts goes through `cli.Secrets` (values from
  `.env`, secret-named variables).
- **Words are never built-ins.** devopsy's features are flags or `@target`;
  a word is a project command, else a compose command (`ComposeCommands`:
  compose's own completion, cached, refreshed when a word is missing), else
  an error. Arguments starting with a flag go to compose unchecked; `--` is
  the escape hatch. Flags that need a target say so without one.
- **`@<target> <subcommand> --help` only prints help**
  (`cli.RemoteCommandHelp`, an entry per subcommand), and resolves the
  target leniently (`DescribeTarget`), so it never needs a server.
- **Small per-project choices are compose labels** (`devopsy.shell`,
  `devopsy.shell.user`, roles); capabilities are for behavior a label cannot
  express. `--shell-host` is never overridable: the way in when a project is
  broken.
- Few dependencies, standard library first. Linux and macOS only.
- **Color** only on a terminal and without `NO_COLOR` (`styleFor`); help is
  colored by layout (headings end with `:`, entries start with two spaces).
- Project-specific behavior belongs in projects' `.devopsy/commands/`.

## Config and targets

- Files: the project's `.devopsy/config.yaml` (`project:`, `instances:`,
  `releases: {keep}`, `defaults:`, `environments:`), its `config.local.yaml`
  over it, and the user's `~/.config/devopsy/config.yaml` (`aliases:`; on
  servers `releases:`). Never `~/.devopsy`: discovery would take the home
  directory for a project. Unknown keys are errors.
- `defaults:` gives environments `mode`, steps, `releases` and `env`; `env`
  merges key by key, `KEY: ~` removes, `""` stays empty (hence parsing YAML
  nodes). `path` is never a default.
- **Deployment facts never live in a project's config:** servers
  (`server:` is an error) and instance names. They come from the target,
  variables or aliases.
- **A target is an address,** `@[<server>:][<instance>/]<environment>`
  (`parseAddress`): the server ends at the first `:`, the instance before
  the last `/`, empty meaning none. Missing parts come from
  `DEVOPSY_SERVER_<ENVIRONMENT>`, the pattern's (`PatternVar`: `pr-*` gives
  `DEVOPSY_SERVER_PR`), `DEVOPSY_SERVER`, and `DEVOPSY_INSTANCE`: the
  caller's environment, then the project's `.env`, never for aliases. No
  server: error. `instances:` is `required`, `none` or unset (optional).
- **Names come only from project, instance and environment:** compose name
  `<project>[-<instance>]-<environment>`
  (`Target.ComposeName`), directory `<project>/[<instance>/]<environment>`
  under the server's release root. `path:` moves the directory only (and
  then takes no instance and no shared `.env` levels). `project:` is
  required for anything remote, never taken from the checkout's folder.
- **Patterns:** environment keys with `*` (one or more name characters);
  exact name, then most literal characters; a tie is an error.
- **Aliases** (`{to, source}` or `{to, project}`): only a bare `@name`, and
  a project's exact environment wins. With `source:` its config gives the
  project, environments and steps, and releases run only from there
  (`ReleasesHere`, symlinks resolved): anywhere else a release would upload
  whatever project devopsy runs in.
- **Release settings belong to the server:** its deploy user's `releases:`
  (`root`, default home; `keep`, `max_keep`, default 5), read by the hidden
  `--release-settings` that every script asks first (`basePrelude`), so a
  laptop and CI resolve the same paths. A project's `keep` is capped by
  `max_keep`; `root` and `max_keep` in a project are errors.
- `--init` writes a new config and never touches an existing one.
  `--debug schema [--user]` prints `ProjectSchema` / `UserSchema`,
  documentation written as configs that parse (`TestSchemas`): a new key
  goes there too.

## Releases

- Remote commands run through the `current` symlink, never a release path:
  compose stores bind-mount paths in containers, and pruned releases would
  break them. The exception is `prepare`, in the new release before it is
  current: it must not start services.
- `--release` and `--rollback` take no command: the environment's
  `release:`/`rollback:` (`remote.Steps`: before, prepare, run, after; a
  plain string is run alone) are required. Phases, not a free list, so the
  upload and the switch are never hidden in a step. The remote steps share
  one SSH session, which holds the lock (a `flock`): so before is local
  steps then remote ones, after is remote then local, checked when the
  config is read. Local steps get the target's env, `DEVOPSY_TARGET` (the
  resolved address) and the commit, never the server's `.env`.
- Order on the server (`ActivateScript`, one session): lock; remote before
  steps in `current` (skipped on a first release); link `shared/*` and the
  project's and instance's `.env` (`project.env`, `instance.env`, linked
  even when missing) into the release; `--prepare-release` when the compose
  files use role or import labels (`PrepareScript`, releases only);
  `prepare` steps in the release; `--missing-vars` (`cli.MissingVars`:
  compose's `${VAR:?}`/`${VAR?}` left unset, env capability included); switch
  `current`; `run`; on failure switch back and restart the previous release
  with its own run step (`Phases.Restart`: the rollback's, else the
  release's); prune to `keep`; remote after steps. A failure before the
  switch calls `not_live` (`NotLive`, `RollbackNotLive`): nothing live
  changes. No prompts: what is missing is listed with its `--vars set`
  line.
- Release logs (`releaseLog`): devopsy's messages, local steps' command
  lines and exit codes, and the upload and activation sessions' output
  (`SSHLog`, both streams), each line after its UTC time (`2006-01-02
  15:04:05`), without terminal codes, progress frames or empty lines, saved by
  `LogSaveScript` in `releases/<id>/.devopsy-log` once the server was
  reached; a rollback's is appended to the restored release's, found from
  its script's "rolling back to <id>" line. Pruned with their releases.
  `--log` reads them (`LogReadScript`). Local
  steps' output is not piped: it would lose its terminal.
- Before uploading: a role taken by another compose project fails
  (`RoleHoldersScript`), and a new instance asks (`InstanceExistsScript`,
  `--yes`). A first release that fails before going live, with nothing in
  `shared/` yet, removes its directory.
- `target.env` is written into each release (environment `env`,
  `COMPOSE_PROJECT_NAME`, overriding compose's `name:`, `DEVOPSY_PROJECT`,
  `DEVOPSY_INSTANCE`, `DEVOPSY_ENVIRONMENT`, `DEVOPSY_TARGET`,
  `DEVOPSY_RELEASE_COMMIT`), so it applies however devopsy runs on the
  server and rollbacks restore it. Precedence there: caller, `shared/.env`,
  `instance.env`, `project.env`, `target.env`.
- A nested devopsy (`DEVOPSY_PROJECT_DIR` set) ignores the inherited
  `COMPOSE_PROJECT_NAME`: it is the local project's, not the target's.
- `mode:` defaults to `build`: it works for every project, where a wrong
  `image` fails the release.
- Releases are full tar streams (no rsync: macOS ships openrsync). Server
  needs: `devopsy`, `tar`, `flock`; locally only `ssh`.
- `--vars` sends values over SSH's stdin, never as arguments; edits replace
  keys in place, under the release lock (deploy commands write secrets too).
- `--destroy`: the environment's `destroy:` step (one command, required)
  in the current release under the lock, then `rm -rf` of the directory;
  asks for the target's name unless `--yes`. devopsy knows nothing of
  containers, volumes or `shared/mnt`: taking them down, and removing files
  container users own, is the project's step. A failed step removes nothing.
- `.devopsy/mnt/` is devopsy's one data directory (README, "Data and
  mounts"): never uploaded, `shared/mnt` created by the upload and linked
  like every `shared/` entry. Its contents and their ownership are the
  project's; devopsy only checks whether it is empty (`NotLive`), never
  removes anything in it.
- A target path with `.devopsy/` and no `current` is a plain directory:
  commands run there, releases refuse before creating anything.

## Roles, exports and imports

- Labels for facts shared between projects on a host (`roles.go`):
  `devopsy.role` (one compose project per host), `devopsy.export.<KEY>`,
  `devopsy.import.<VAR>=<role or project>/<KEY>[?]`. devopsy knows no role
  or key names; `proxy` and `WILDCARD_DOMAIN` are the Traefik template's
  convention. devopsy's core defines no proxy contract.
- `--prepare-release` checks the role and writes imports into the new
  release's `target.env` (after `cli.ImportedMarker`) before it goes live;
  an import never overrides what the environment sets (even empty); an
  empty export is a value; rollbacks never check. Exports are read from
  running containers (`Running`: one snapshot per lookup), waiting up to
  `WaitForExporters` for required sources.
- `--debug imports` compares every running project's imports with its
  release's `target.env` (found through compose's working_dir label).
- Roles are advisory: anyone in the docker group is root-equivalent.

## Other features

- **Completion** lives in Go (`internal/complete`); the scripts only pass
  words to the hidden `--complete` and show what it prints, cobra's
  protocol (`value<TAB>description<TAB>group` lines, then `:<directive>`),
  so compose's own completion passes through. It never connects to
  servers. New flags and subcommands go in its lists. `TestCompletionShells`
  runs the scripts in every installed shell (bash, macOS's bash 3.2, zsh
  with `_describe` stubbed, fish); CI installs zsh and fish. bash splits
  words at `:`, so its script rebuilds them from `COMP_LINE`.
- **`--debug`** explains what devopsy sees; targets carry `From` (each
  value's origin). The capability catalog (`cli.Capabilities`) lives with
  the code that calls it.
- **Capabilities** (`--capability <name> <action>`, hidden): executables in
  `.devopsy/capabilities/`, run in the project's own environment and SSH
  session, so no other project's variables leak in. Define one only when a
  second use needs it.
- **devopsy knows no URL.** It always sets `COMPOSE_PROJECT_NAME` (locally
  the config's `project:`, else compose's `name:`, else the folder) and
  runs the `env` capability (`computeEnv`) before every command but help
  and completion: projects derive hosts, rules and URLs themselves, from
  the name and imported facts. Its output only fills unset variables;
  `DEVOPSY_ENV_COMPUTE` keeps a devopsy it calls from running it again.
- **`--probe`** is a tool with plain arguments, not a contract: it knows no
  proxy. A server never drives local devopsy (devopsy execs ssh and cannot
  watch its output, and a server must not make a laptop act): proxies print
  the `--probe` line to run.
- **SSH:** every session is `ssh [DEVOPSY_SSH_COMMAND's options]
  [SSHOptions] -T|-t <host> 'sh -c <script>'` (`remote.SSHLog`):
  `ClearAllForwardings=yes`, since the user's `LocalForward` lines clash
  between sessions. Anything faster (connection sharing) is the user's SSH
  config, which `--ssh-config` suggests and never writes; built-in sharing
  is in ROADMAP.md. Test fakes of ssh run their last argument.
- **`--context-hash`** is a pure helper; devopsy builds nothing. Errors only
  ever go towards a new hash (a kept file costs a rebuild, a dropped one
  would reuse a stale image), hence Docker's own matcher and refusing what
  it cannot hash.
- **`--upgrade`** reads the same release files as `install.sh` (archive
  names without version, `checksums.txt`). The daily check runs detached
  (`--upgrade-check`), since devopsy execs right away; only release builds
  upgrade or check.

## Gotchas

- Compose reads `--env-file` more than once: `<(devopsy --env)` fails, a
  file works.
- `.env` values in double quotes are interpolated by compose's parser.
- GitHub can deliver a tag push several times: the release workflow queues
  runs per tag and GoReleaser replaces assets. If a run fails, check the
  release's assets before assuming it broke.
- GitHub's `releases/latest` can lag a new release by minutes: `devopsy
  --upgrade vX.Y.Z` installs one directly.
- Go's HTTPS to github.com is slow (about 1.5 s), hence the detached check.
- Traefik picks up a new container a few seconds after `up` returns.

## Checks

```sh
go vet ./... && go test -count=1 ./...
docker run --rm -v "$PWD:/mnt" -w /mnt koalaman/shellcheck:stable -s sh install.sh
```

End to end: an OrbStack Debian 13 machine (`orb create debian:trixie
devopsy-test`) set up with devopsy-server, a linux/arm64 build installed in
it, and targets like `@devopsy@devopsy-test@orb:prod` (OrbStack's SSH needs
no keys). Release the Traefik template there, then a template importing
from it, and check `target.env`, `--debug imports`, the Traefik `domains`
command, an instance, a pattern environment and `--destroy`, a rollback,
and a release with Traefik stopped. Delete the machine afterwards.

`--upgrade` and the release notice only work in release builds: `go build
-ldflags "-X main.version=0.8.0" -o /tmp/devopsy ./cmd/devopsy`, then
`/tmp/devopsy --upgrade`. The notice needs a terminal (`script -q out
./devopsy ps`).

## Releases and commits

Tag `vX.Y.Z` on `main` and push the tag: the release workflow tests, and
GoReleaser publishes the binaries and `checksums.txt`. Conventional commits
(`feat:`, `fix:`, `docs:`, `build:`...).
