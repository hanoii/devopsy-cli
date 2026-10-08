# AGENTS.md

devopsy-cli is a Go command-line tool, `devopsy`, that wraps `docker compose`
for projects with a `.devopsy/` directory. Read `README.md` for the
user-facing behavior and keep it in sync with any change to it.

## Layout

- `cmd/devopsy/`: `main`, which executes what `internal/cli` plans, plus
  end-to-end tests that run the built binary against a fake `docker`.
- `internal/cli/`: finds the project, loads `.env` and decides what to run
  (`Build` returns a `Plan`; it never executes). Unit-tested.
- `internal/remote/`: `@target` support: config.yaml (`config.go`), packing a release, and
  the POSIX `sh` scripts run on the server over SSH. Scripts are checked with
  `sh -n` in tests; keep them POSIX and GNU coreutils based (`mv -T`).
- `internal/complete/`: shell completion: the candidates for the words
  after `devopsy`, and the bash, zsh and fish scripts (embedded) that ask
  for them.
- `install.sh`: POSIX `sh` installer that downloads a release.
- `scripts/devopsy`: runs devopsy built from the checkout; developers
  symlink it into their PATH.
- `.goreleaser.yaml` and `.github/workflows/`: CI and release builds.

## Rules

- Keep behavior compatible: project discovery, `.env` loading (compose's own
  parser, caller's environment wins), custom commands in `commands/` with
  `DEVOPSY_PROJECT_DIR` and `DEVOPSY_CLI_COMMAND`, the recursion guard, and
  passing everything else to `docker compose` untouched.
- devopsy replaces itself with the command it runs (`syscall.Exec`), so
  signals, the terminal and exit codes belong to that command.
- Messages go to stderr. Compose output stays on stdout. Anything printed
  that can contain arguments or scripts goes through `cli.Secrets` (values
  from `.env` and secret-named variables): the compose notice printed every
  command, passwords included, into terminals and CI logs.
- Built-ins are flags (`--help`, `--version`, `--env`, `--verbose`) or `@target`, never
  words: words are project commands, then docker compose commands, so nothing
  clashes (`devopsy version` once printed devopsy's version and then ran
  compose's). Never add a word built-in. Help (bare `devopsy`) lists
  built-ins, remote commands, project commands with their `## Description:`
  and the compose fallback. After `@target` too: `--release`, `--rollback`,
  `--releases`, `--shell`, `--shell-host`, `--vars`, `--instances`,
  `--destroy`. A word is a project command, else a docker
  compose command (`ComposeCommands`: compose's own completion, cached and
  refreshed when a word is missing, since compose's commands only change with
  its version), else an error, never compose's usage dump. Arguments starting
  with a flag go to compose unchecked: compose's global flags take values
  devopsy would have to know. `devopsy -- <args>` is the escape hatch:
  straight to compose, past project commands and the check. Flags that
  need a target say so without one.
- `--shell` opens a container shell: the project's `shell` capability, else
  bash or sh in the named service, the one labeled `devopsy.shell=true`, or
  the only running one, as its `devopsy.shell.user` label says if any.
  Small per-project choices are compose labels, read from the compose
  files: the capability is for behavior a label cannot express. `--shell-host` (the server's own shell) is never
  overridable: it is the way in when the project is broken.
- `devopsy @<target> <subcommand> --help` must only print help
  (`cli.RemoteCommandHelp`): before it existed, `--release --help` made a
  release and ran `devopsy --help` on the server. Keep a help entry for every
  server subcommand.
- Few dependencies; prefer the standard library. Linux and macOS only.
- Completion logic lives in Go (`internal/complete`), never in the shell
  scripts: they only pass words to the hidden `devopsy --complete` and show
  what it prints, cobra's protocol (`value<TAB>description` lines, then
  `:<directive>`). That lets docker compose's own completion (`docker
  __complete compose -f ...`, docker's CLI is cobra) pass through for
  compose's commands, flags and services. devopsy adds a third field, the
  group (target, user-target, project, devopsy, compose for the
  delegate's), which the zsh and fish scripts use to set them apart.
  Completion never connects to
  servers: after `@target`, services come from the local compose files (a
  an alias's `source:` checkout, from any directory).
  New built-ins and `@target` subcommands go in its lists too.
- Color only on a terminal and without `NO_COLOR` (`styleFor`); help is
  colored by its layout (headings end with `:`, entries start with two
  spaces), so keep that layout when editing help texts.
- `--debug [environments [name or address] [--yaml] | capabilities | labels | imports]` explains what devopsy
  sees and computes. Targets carry `From` (where each value came from) for
  it. The capability catalog it prints (`cli.Capabilities`) lives with the
  code that calls them: a new capability or action goes there and in
  README's contract.
- `--upgrade` (internal/update) reads the same release files as `install.sh`:
  keep archive names and `checksums.txt` as they are. The daily release
  check runs in a detached `devopsy --upgrade-check`, because devopsy
  replaces itself with the command right away; only release builds (plain
  `X.Y.Z` versions) upgrade or check.
- Variable precedence: caller's environment, `.devopsy/.env` (on servers
  `shared/.env`), then on servers `instance.env` and `project.env` (links
  to `<root>/<project>[/<instance>]/.env`, made by every release, even when
  the file does not exist yet), `.devopsy/target.env` (from config.yaml).
- Roles, exports and imports (`internal/cli/roles.go`): compose labels
  for facts shared between projects on a host. `devopsy.role` is a slot one
  compose project per host holds, `devopsy.export.<KEY>` a fact on running
  containers, `devopsy.import.<VAR>=<role or project>/<KEY>[?]` copies one
  into a release's `target.env`. devopsy knows no role or key names:
  `proxy` and `WILDCARD_DOMAIN` are devopsy-template-traefik's convention. The
  hidden `--prepare-release` checks the role and resolves imports on the
  server, in the new release before it becomes current (`PrepareScript`,
  only when the compose files mention the labels); an import never overrides what the release's
  environment sets, even empty; an empty export is a value; rollbacks never
  check. Read from running containers, not files: the exporter must run,
  and required sources are waited for (`WaitForExporters`, 30 s). Each
  lookup reads one snapshot of the host (`Running`: `docker ps` then
  `docker inspect`), so a restart in between cannot mix states.
  `--debug imports` compares every
  running project's imports (from its containers' labels) with its
  release's `target.env` (found through compose's working_dir label;
  imported lines follow `cli.ImportedMarker`). Roles are advisory: checked
  only at devopsy releases, never a security boundary (docker group is
  root-equivalent); pinning roles on the host is in ROADMAP. devopsy's
  core defines no proxy contract.
- Wildcard host: `<name>.<DEVOPSY_WILDCARD_DOMAIN>`; without a domain,
  `<name>.localhost` locally and none in a release (`target.env` exists),
  where the environment only answers on `DEVOPSY_DOMAINS`.
  `DEVOPSY_HOST_RULE` stays unset without hosts, so labels' defaults apply.
- Config (`internal/remote/config.go`): the project's `.devopsy/config.yaml`
  (`project:`, `instances:`, `releases: {keep}`, `defaults:`,
  `environments:`), its `config.local.yaml` over it, and the user-level
  `~/.config/devopsy/config.yaml` (`aliases:`; on servers `releases:`;
  never `~/.devopsy`: project discovery would take the home directory for a
  project). Unknown keys are errors, environments' too. Nothing else is
  read and there is no migration code: older files and syntax do nothing,
  and servers are moved by hand.
- A target is an address, `@[<server>:][<instance>/]<environment>`
  (`parseAddress`): the server ends at the first `:` (any SSH destination or
  ssh alias, no `:` inside), the instance before the last `/`, an empty one
  meaning none. What it leaves out comes from `DEVOPSY_SERVER_<ENVIRONMENT>`,
  the pattern's `DEVOPSY_SERVER_<PATTERN>` (`pr-*`: `DEVOPSY_SERVER_PR`,
  `PatternVar`), `DEVOPSY_SERVER` (server) and `DEVOPSY_INSTANCE`, from the
  caller's environment then the project's `.env` (never for aliases).
  Missing server: error; instance per `instances:` (`required`, `none`, or
  optional). The project's config never lists instances or servers
  (`server:` is an error): those are deployment facts, made in addresses,
  variables or aliases.
- Names come only from project, instance and environment: compose name and
  wildcard host `<project>[-<instance>]-<environment>`
  (`Target.ComposeName`), directory `<project>/[<instance>/]<environment>`
  under the server's release root. `path:` moves the directory only (then
  no instance and no shared `.env` levels). `project:` is required for
  anything remote, never the checkout's folder (template copies would
  install under the template's name).
- A first release of an instance the server lacks asks first
  (`InstanceExistsScript`); `--release --yes` skips it. Releases print the
  resolved target (`Target.Address`), which is also `DEVOPSY_TARGET` in
  `target.env` and local steps.
- Patterns: environment keys with `*` (one or more name characters); exact
  names win, then the most literal characters; equal: error. `Targets()`
  lists environments then aliases, `Patterns()` the patterns.
- `--init` writes a new `.devopsy/config.yaml` and never touches an
  existing one. `--debug schema [--user]` prints `remote.ProjectSchema` /
  `UserSchema`: documentation as a config that parses (`TestSchemas`), so a
  new key goes there too or the test fails to show it.
- Aliases (user config): `{to: <address>, source: <checkout>}`, or
  `project:` instead of source for commands only. Only a bare `@name` is an
  alias; an environment of that exact name wins. The source's config gives
  the project, environments and steps.
- Release settings are the machine's, never a project's or a laptop's: the
  deploy user's `releases:` (`root`, default home; `keep` and `max_keep`,
  default 5), read on the server by the hidden `--release-settings`, which
  every script asks first (`basePrelude`), so laptop and CI resolve the
  same paths. A project's `keep` (project or target) is capped at the
  server's `max_keep`; `root` and `max_keep` in a project's config are
  errors. On a laptop they only matter once `host: local` exists.
- `--destroy`: down with volumes in the current release, `shared/mnt`
  removed from a container (its files can belong to container users), then
  the directory; asks for the target's name unless `--yes`, refuses a plain
  directory and the root.
- Aliases refuse release and rollback unless their `source:` is the local
  project's directory (`ReleasesHere`, symlinks resolved): a release from
  anywhere else would replace, say, a server's Traefik with whatever project
  it ran in. A target path without `current` but with `.devopsy/` is a
  plain directory: commands run there, release scripts refuse before
  creating anything.
- `defaults:` in a project's config gives its environments their `mode`,
  steps, `releases` and `env` unless they set their own; `env` merges key
  by key, `KEY: ~` removes a default and `""` stays an empty value (hence
  parsing nodes: a string map would make both ""). config.local.yaml's
  defaults go over config.yaml's. server and path are never defaults.
- Releases write `COMPOSE_PROJECT_NAME` into `target.env` when compose.yaml has
  no name: devopsy run by hand in `current` named the project "current".
- Remote: only `ssh` locally, and `devopsy`, `tar` and `flock` on the server.
  No rsync: macOS ships openrsync. Releases are complete copies.
- Project-specific behavior belongs in a project's `.devopsy/commands/`, not
  here.
- `install.sh` stays POSIX `sh` (dash, busybox ash).

## Design decisions

The workspace README (`../devopsy/README.md` locally) describes how the repos
fit together, and `../devopsy/ROADMAP.md` the open ideas.

- devopsy is compose plus environment variables. It never generates or
  rewrites compose files: if a feature needs that, it is the wrong feature.
  Everything it computes is visible with `devopsy --env`.
- An environment is a target: a server path, so its own compose project,
  data and wildcard URL (`<project>.<DEVOPSY_WILDCARD_DOMAIN>`). Branches are
  only what gets released into one; no branch concept in the core.
- Remote commands run through the `current` symlink, never a release path:
  compose stores bind-mount paths in containers, and pruned releases would
  break them (for example after a reboot). `--release` switches `current`
  first, runs the remote step, and switches back when it fails.
- `--release` and `--rollback` take no command: each target's `release:` and
  `rollback:` (before, remote, after; `remote.Steps`) are required for them.
  A bare upload left `current` ahead of the running containers, for the next
  `up` or `reload` to half apply. Phases rather than a free list, so the
  upload is never hidden in a step and the order cannot break the lock: the
  lock is a `flock` inside the server script, one SSH session, so one remote
  command; several remote steps belong in a project command. Local steps
  get the target's env, never the server's `shared/.env`.
- A nested devopsy (`DEVOPSY_PROJECT_DIR` set: a project command or release
  step calling `devopsy @target`) ignores the inherited
  `COMPOSE_PROJECT_NAME`: it was the local project's, and a release from
  catalyze's `ship` once came up as a second stack on the same data.
- `mode:` defaults to `build` (before v0.15.0, `image`): build works for
  every project, image only for those that never build, and a wrong build
  costs a few uploaded files where a wrong image fails the release.
- Releases are full tar streams over SSH (catalyze, the largest project,
  compresses to about 3 MB). No rsync: macOS ships openrsync without the
  needed features.
- `.devopsy/target.env` is written into each release from config.yaml, so
  per-target values apply however devopsy runs on the server, and rollbacks
  restore them. `shared/.env` is always linked so server edits apply without
  a release. It also carries `DEVOPSY_RELEASE_COMMIT` (from git, unless the
  target's env sets it) for image tags: with a fixed tag, image-mode
  rollbacks would pull the newest image again.

- `devopsy --context-hash`: a pure helper, git and compose.yaml in, a hash
  out, so projects can reuse images by content (see README). devopsy does
  not build or push images: owning the build was deferred (ROADMAP,
  "Content-addressed images"). Errors only ever go towards a new hash: a
  file kept that Docker ignores costs a rebuild, a file dropped that Docker
  sends would reuse a stale image. Hence Docker's own matcher, and refusing
  what it cannot hash (`additional_contexts`, `build:` in an override).
- `devopsy @target --vars`: secrets go into `shared/.env` (or a plain
  directory's `.devopsy/.env`) over SSH's stdin, never as arguments. The
  scripts only need `sh`, `awk` and `flock` on the server, so any server
  version works; edits replace keys in place (appending reorders files) and
  take the release lock, since `deploy` commands write their own secrets.
  Aliases never read values from a project's `.env`, as for servers.
- Capabilities (`devopsy --capability <name> <action>`, hidden like
  `--complete`): interfaces devopsy defines and projects implement in
  `.devopsy/capabilities/<name>/<action>`, so devopsy asks a project without
  knowing its internals. devopsy runs them over their own SSH session, in
  the project's current release, so no other project's variables leak in
  (a nested devopsy would inherit the caller's project env and its
  recursion guard: rejected `-C <dir>` for that). Stderr shows only on
  failure. Contracts are versioned JSON, documented in README; unknown
  fields are ignored. Define a new one only when a second use needs it.
- `devopsy --probe [--ip <ip>] <host>...` (`internal/probe`): DNS through
  1.1.1.1, the certificate (a real handshake, system roots) and an HTTPS
  request, from where it runs. A tool with plain arguments, not a contract:
  it knows no proxy. It replaced `devopsy @target --domains` (until
  v0.17.0), whose proxy-specific half (routes, resolvers, CNAMEs, retries)
  is now devopsy-template-traefik's own `domains` command, which prints the
  `--probe` line to run. A server never drives local devopsy: devopsy execs
  ssh, so it cannot watch output, and a server must not make a laptop act.

## Gotchas

- Compose reads `--env-file` more than once: `<(devopsy --env)` does not
  work, a file does (observed, not confirmed in compose's source).
- `.env` values in double quotes are interpolated when loaded (compose's
  parser): `"$HOME"` becomes the value of HOME.
- GitHub has delivered the same tag push several times (v0.5.0: 4 release
  runs, v0.6.0: 3), and the runs raced on uploads ("already_exists"). The
  release workflow now queues runs per tag and GoReleaser replaces existing
  artifacts. If a release run still fails, check the release's assets and
  install it before assuming a broken release.
- Traefik picks up a new container a couple of seconds after `up` returns:
  wait before querying its API in tests.
- GitHub's `releases/latest` redirect, which `--upgrade` follows, can lag
  a new release by a few minutes depending on the edge: right after
  v0.11.0, one server upgraded and another still saw v0.10.0 (October
  2026). `devopsy --upgrade vX.Y.Z` installs a given release meanwhile.
- Go's HTTPS connection to github.com took 1.5 s where curl took 0.5 s
  (October 2026): too slow for a check before every command, hence the
  detached `--upgrade-check`.

## Checks

```sh
go vet ./... && go test -count=1 ./...
docker run --rm -v "$PWD:/mnt" -w /mnt koalaman/shellcheck:stable -s sh install.sh
```

End to end, against a real server: an OrbStack Debian 13 machine set up with
devopsy-server (see its AGENTS.md), a linux/arm64 build installed in it, and
a test project whose `.devopsy/config.local.yaml` points at
`devopsy@devopsy-test@orb`. OrbStack's SSH needs no keys.

Roles and imports, end to end on that machine: release devopsy-template-traefik
(its `config.local.yaml` pointing there too), then a template importing from
it (whoami, with `DEVOPSY_SERVER`), and check its `target.env`,
`devopsy @<target> --debug imports`, `devopsy @<traefik target> domains
<project>`, a target that sets the variable empty, a rollback, and a
release with Traefik stopped (fails after 30 s, `current` unchanged).
Delete the machine afterwards.

`--upgrade` and the release notice only work in release builds: test them
with a build that pretends to be older, against the real releases:

```sh
go build -ldflags "-X main.version=0.8.0" -o /tmp/devopsy ./cmd/devopsy
/tmp/devopsy --upgrade
```

The binary it installs is the real release, so it only knows the flags that
release had. The notice needs a terminal: `script -q out ./devopsy ps`.

## Releases

Tag `vX.Y.Z` on `main` and push the tag: the release workflow runs the tests
and GoReleaser publishes the binaries and `checksums.txt`. Archive names have
no version (`devopsy_linux_amd64.tar.gz`), which `install.sh` relies on.

## Commits

Conventional commits (`feat:`, `fix:`, `docs:`, `build:`...).
