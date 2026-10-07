# AGENTS.md

devopsy-cli is a Go command-line tool, `devopsy`, that wraps `docker compose`
for projects with a `.devopsy/` directory. Read `README.md` for the
user-facing behavior and keep it in sync with any change to it.

## Layout

- `cmd/devopsy/`: `main`, which executes what `internal/cli` plans, plus
  end-to-end tests that run the built binary against a fake `docker`.
- `internal/cli/`: finds the project, loads `.env` and decides what to run
  (`Build` returns a `Plan`; it never executes). Unit-tested.
- `internal/remote/`: `@target` support: targets.yaml, packing a release, and
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
  `--releases`, `--domains`, `--shell`, `--shell-host`, `--vars` (the first
  four were words until v0.12.0). A word is a project command, else a docker
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
  user-level target's `source:` checkout, from any directory).
  New built-ins and `@target` subcommands go in its lists too.
- `--upgrade` (internal/update) reads the same release files as `install.sh`:
  keep archive names and `checksums.txt` as they are. The daily release
  check runs in a detached `devopsy --upgrade-check`, because devopsy
  replaces itself with the command right away; only release builds (plain
  `X.Y.Z` versions) upgrade or check.
- Variable precedence: caller's environment, `.devopsy/.env` (on servers
  `shared/.env`), `.devopsy/target.env` (from targets.yaml). No server-wide
  layer: `/etc/devopsy/devopsy.env` existed until October 2026.
- `DEVOPSY_WILDCARD_DOMAIN` comes from the server's proxy: `--release` asks its
  `domains` capability (`wildcard-domain`, `WildcardDomainScript`) and writes
  it into the release's `target.env`, unless the target's environment
  already has the key (targets.yaml or `shared/.env`, even empty: no
  automatic URL). Written per release, so rollbacks keep what each release
  had. Asked at release, not on every command: devopsy would otherwise run
  the proxy's capability (a container or two) on each `up`.
- Wildcard host: `<name>.<DEVOPSY_WILDCARD_DOMAIN>`; without a domain,
  `<name>.localhost` locally and none in a release (`target.env` exists),
  where the environment only answers on `DEVOPSY_DOMAINS`.
  `DEVOPSY_HOST_RULE` stays unset without hosts, so labels' defaults apply.
- Targets come from the project's `targets.local.yaml`, then `targets.yaml`,
  then the user-level `~/.config/devopsy/targets.yaml` (never `~/.devopsy`:
  project discovery would take the home directory for a project). User-level
  targets refuse release and rollback unless their `source:` is the local
  project's directory (`ReleasesHere`, symlinks resolved): a user-level
  target belongs to no project, and a release from anywhere else would
  replace, say, a server's Traefik with whatever project it ran in.
  Hosts can come from variables, so
  public repositories need not name servers: `DEVOPSY_TARGET_HOST_<NAME>`
  replaces a target's host, `DEVOPSY_TARGET_HOST` fills in a missing one,
  from the caller's environment, then the project's `.env` (read for this
  only; user-level targets ignore it, so a project cannot redirect them).
  A target path without `current` but with `.devopsy/` is a plain
  directory: commands run there, release scripts refuse before creating
  anything.
- `defaults:` in a targets file (reserved, never a target) gives that
  file's targets their `mode`, `source`, steps and `env` unless they set
  their own; `env` merges key by key, `KEY: ~` removes a default and `""`
  stays an empty value (hence parsing nodes: a string map would make both
  ""). The project's defaults (targets.local.yaml's over targets.yaml's)
  and the user-level file's never mix. host and path are never defaults.
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
- `.devopsy/target.env` is written into each release from targets.yaml, so
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
  User-level targets never read values from a project's `.env`, as for hosts.
- Capabilities (`devopsy --capability <name> <action>`, hidden like
  `--complete`): interfaces devopsy defines and projects implement in
  `.devopsy/capabilities/<name>/<action>`, so devopsy asks a project without
  knowing its internals. devopsy runs them over their own SSH session, in
  the project's current release, so no other project's variables leak in
  (a nested devopsy would inherit the caller's project env and its
  recursion guard: rejected `-C <dir>` for that). Stderr shows only on
  failure. Contracts are versioned JSON, documented in README; unknown
  fields are ignored. Define a new one only when a second use needs it.
- `devopsy @target --domains`: the target's environment comes from the server
  (`print-env`, the old name of `--env`); what the proxy knows comes from
  its `domains` capability in `DEVOPSY_PROXY_DIR` (default `/srv/traefik`):
  routes, the resolver (shown only) and the issuing method (`http`,
  `dns-cname` with its record, `dns-api`), which next steps depend on.
  devopsy-cli knows no proxy: Traefik's API, ACME files and resolver names
  live in devopsy-traefik. DNS (through 1.1.1.1), certificates (a real TLS
  connection to the server per name, verified against system roots) and
  Cloudflare's proxy (its ranges, then a request through it) are checked
  locally. `--retry` calls `retry <project> <hosts>` and `--domains` calls
  `retry <project> --done` once every routed host has a valid certificate.
  When the target's path is `DEVOPSY_PROXY_DIR`, `--domains` checks every
  host the proxy reports (`facts --all`) as `server`.

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
a test project whose `.devopsy/targets.local.yaml` points at
`devopsy@devopsy-test@orb`. OrbStack's SSH needs no keys.

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
