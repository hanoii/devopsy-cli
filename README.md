# devopsy-cli

`devopsy` is a small command-line tool for running Docker Compose projects,
both on your machine and on servers you own.

Compose is great for describing a project, but leaves you on your own once
it has to live on a server. devopsy tries to help with a few things there:

- **Releases:** ship a new version with one command, from your machine or
  CI, and roll back just as easily.
- **Environments:** production, staging or one per pull request, side by
  side on the same server without stepping on each other.
- **Variables per deployment:** set a value once for a project on a
  server, or only for one install or one environment; the most specific
  wins. Secrets never touch git or your shell history, and a release that
  is missing a required value stops before going live and tells you what
  to set.

Everything a project needs lives in a `.devopsy/` directory next to your
code, so every project works the same way.

## What it is not

- **Not a platform.** No control plane, dashboard, agent or daemon; nothing
  listens for webhooks. A server is Docker, SSH and this binary.
- **Not an orchestrator.** No Kubernetes, no Swarm, no failover: an
  environment is one compose project on one server.
- **Not a compose replacement.** It never generates or rewrites compose
  files; plain `docker compose` sees exactly what devopsy runs.
- **Not a local development tool like DDEV.** No per-framework setup, router
  or snapshots: it runs the compose project you write.
- **Not a build system.** Images are built however you like: on the server
  by compose, or in CI and pushed to a registry.

> [!NOTE]
> devopsy is none of these, but it should be possible to build any of them
> on top of it: its commands, `--debug` output and plain files are meant to
> be scripted, by you or with an AI agent.

## Who it is for

People and small teams who deploy different web applications, sometimes
with complex stacks and their own deploy strategies, to a few servers they
own, and want what a hosting platform gives (an environment per branch, releases and rollbacks, secrets
out of git, URLs and certificates) without running or paying for one.

It assumes some technical knowledge: a terminal, SSH, and the basics of
Docker. It is also a good way to learn how Docker and Docker Compose
deployments work: devopsy hides nothing, and every step it takes is plain
compose you can read and run yourself.

It grew out of hosting websites for clients: many projects, some installed
several times from one codebase, spread over a handful of servers, each
deployed by a different person or CI job. It fits when:

- you already describe your stack in compose, or are happy to;
- a project fits on one server per environment;
- a release that recreates its containers, for a few seconds, is fine;
- the people deploying have SSH access to the servers.

## Opinionated (as little as possible)

devopsy is small, but not neutral. Its goal is that every project is worked
on, released and operated the same way, so that moving between projects,
handing one over, or letting CI or an agent deploy it needs nothing to be
relearned:

- the same commands in every project: `devopsy up`, `devopsy @prod
  --release`, `--rollback`, `--log`, `--vars`, `--shell`;
- the same layout on every server: `<project>/<environment>/` with
  `releases/`, `current` and `shared/`, so data, secrets and release logs
  are always in the same place;
- the same names: compose project, containers and volumes are
  `<project>[-<instance>]-<environment>`.

To get there it makes choices for you:

- **Compose plus environment variables.** devopsy only adds variables, all
  visible with `devopsy --env`. Anything else is compose's.
- **Push over SSH, never pull.** The server needs no access to the
  repository: no deploy keys, tokens or webhooks. Locally devopsy needs
  `ssh`; on the server `devopsy`, `tar` and `flock`.
- **Releases are complete directories,** run through a `current` symlink;
  `shared/` keeps data and secrets across them. A release never changes a
  live tree in place.
- **Steps are explicit.** `--release` and `--rollback` refuse without the
  steps the environment names.
- **Project behavior lives in the project,** as commands in
  `.devopsy/commands/`, not as devopsy features.
- **Capabilities for common needs.** Some conveniences are useful to most
  projects but depend on each one to get right. devopsy defines them as
  small contracts, and a project that wants one implements it in
  `.devopsy/capabilities/`.
- **Deployment facts stay out of the project:** which servers and instances
  exist is decided by the command, variables or aliases, never the
  project's config.
- **Words are yours.** devopsy's own features are flags or `@target`; a word
  is always a project command, then a compose command.
- **Secrets stay out of sight:** masked in output, never in arguments.

Everything else is yours to make (an AI skill to help is coming soon).

## Templates

I created these templates as starting points, and as examples of how to work
with devopsy. Together they set up a stack for deploying complex web
applications, with an environment per branch or pull request, from your own
repository and in relatively little time.

They are GitHub template repositories: start a project from one ("Use this
template"), or clone it and keep it as a remote to pull its changes when you
choose, then adapt it to your needs. They define one particular way of
doing things; devopsy enforces none of it, beyond the labels they use for
its features.

- [devopsy-template-traefik](https://github.com/hanoii/devopsy-template-traefik):
  the reverse proxy the others route through, one per server.
  - Automatic HTTPS certificates, including before DNS points to the server.
  - A wildcard domain, so every environment gets its own URL.
  - Real visitor IPs behind CDNs and other proxies.
  - A `domains` report: DNS, certificates and what to do next.
  - Cloudflare aware: its IP ranges, automatic DNS challenges with an API
    token, and proxied domains.
- [devopsy-template-whoami](https://github.com/hanoii/devopsy-template-whoami):
  the smallest project.
  - Try releases, rollbacks and per pull request environments.
  - Try domains and certificates end to end.
- [devopsy-template-drupal11](https://github.com/hanoii/devopsy-template-drupal11):
  a Drupal 11 site with MariaDB 11.
  - Built on the server: no registry needed.
  - Read-only code at runtime, and non-root containers.
  - Secrets generated on the first release.
  - Drush and a shell in the app container, as the app user.
- [devopsy-template-registry](https://github.com/hanoii/devopsy-template-registry):
  a private Docker registry with a user and password.
  - For projects that build images in CI and pull them on release.
  - A `credentials` command for `docker login`.

---

> [!WARNING]
> Work in progress. What follows is the longer explanation, from getting
> started to every feature in detail, and is still being rewritten.

## Getting started

1. Install devopsy on your machine (below).
2. Prepare a server: Docker, a `devopsy` deploy user in the docker group,
   and devopsy ([devopsy-server](https://github.com/hanoii/devopsy-server)
   does all three on Debian 13).
3. Release [devopsy-template-traefik](https://github.com/hanoii/devopsy-template-traefik)
   to it, if your projects serve websites.
4. Start a project from a template
   ([devopsy-template-whoami](https://github.com/hanoii/devopsy-template-whoami)
   is the smallest), or run `devopsy --init` in an existing compose project.
5. `devopsy up -d` locally, then `devopsy @<server>:prod --release`.

## Install

One static binary for Linux and macOS (amd64, arm64). It needs Docker
with the Compose plugin, and `ssh` for servers.

```sh
curl -fsSL https://raw.githubusercontent.com/hanoii/devopsy-cli/main/install.sh | sh
```

It installs the latest release, checksum verified, to `/usr/local/bin` when
it can, else `~/.local/bin` (`DEVOPSY_VERSION=v0.19.0` and
`DEVOPSY_INSTALL_DIR` choose). `devopsy --upgrade [vX.Y.Z]` replaces the
binary (with `sudo` when root owns it, as on servers). At a terminal it
checks for a new release once a day in the background and says so; never in
CI, and `DEVOPSY_NO_UPDATE_CHECK=1` turns it off.

To run a checkout's code instead, symlink `scripts/devopsy` into your `PATH`
(it rebuilds on every run, instantly when nothing changed; needs Go).

### Shell completion

```sh
source <(devopsy --completion bash)   # ~/.bashrc
source <(devopsy --completion zsh)    # ~/.zshrc, after compinit
echo 'command -q devopsy; and devopsy --completion fish | source' > ~/.config/fish/completions/devopsy.fish
```

It completes targets (`@prod`, and environments after `@vm1:` or `@vm1:b/`),
aliases, project commands with their descriptions, devopsy's flags and,
through compose's own completion, compose's commands and services. It never
connects to servers. The scripts ask devopsy at every Tab, so they follow
the installed version.

## Locally

```
my-project/.devopsy/
├── compose.yaml            # required
├── compose.override.yaml   # optional, usually not committed
├── .env                    # optional, not committed
├── config.yaml             # for servers: devopsy --init
└── commands/               # optional: executable project commands
```

```sh
devopsy up -d          # docker compose -f .devopsy/compose.yaml [-f ...override.yaml] up -d
devopsy deploy         # .devopsy/commands/deploy
devopsy                # help: devopsy's own, the project's commands, the rest
devopsy --env          # the variables devopsy loads and computes
devopsy --shell [service] [-- command]   # a shell, or a command, in a container
devopsy -- <args>      # docker compose <args>, even if a project command has that name
devopsy -v deploy      # --verbose: what devopsy found and runs
```

A word is a project command if `.devopsy/commands/` has it, else a compose
command (devopsy asks compose which exist), else an error. Arguments starting
with a flag go to compose unchecked.

**Project commands:** any executable in `.devopsy/commands/`, run with the
project's `.env` loaded, `DEVOPSY_PROJECT_DIR` (the `.devopsy/` path) and
`DEVOPSY_CLI_COMMAND`. A `## Description:` line near the top shows in help.
A command can wrap a compose command of the same name: `commands/up` calling
`devopsy up` reaches compose, not itself.

**`.env`** is read with compose's own parser; your environment wins over it,
as in compose.

**Variables devopsy adds**, for compose files and commands:
`COMPOSE_PROJECT_NAME` (always set, see below), `DEVOPSY_PROJECT_DIR`, and
whatever the project's `env` capability computes (see Capabilities), which
is how projects derive values like their hosts.

To run plain compose with the same values: `devopsy --env > /tmp/env &&
docker compose -f .devopsy/compose.yaml --env-file /tmp/env config` (a file:
compose reads `--env-file` more than once).

**Project name:** devopsy always sets `COMPOSE_PROJECT_NAME`: the config's
`project:`, else compose's top-level `name:`, else the directory containing
`.devopsy/`, normalized. On servers it is
`<project>[-<instance>]-<environment>` (below), whatever `name:` says. Your
own `COMPOSE_PROJECT_NAME` wins.

**`--shell`** opens bash (or sh) in the service named, else the one labeled
`devopsy.shell=true`, else the only one running, as `devopsy.shell.user`
when set (for images whose entrypoint drops from root). Options go to
`docker compose exec` (`--user root` wins over the label); after `--`, a
command runs instead. A project needing more implements the `shell`
capability.

**`--context-hash [service]`** prints a hash of what the service's image is
built from at `HEAD` (git-tracked context minus the dockerignore Docker
uses, the Dockerfile, the `build:` section), to reuse an image across
commits that do not change it: `tag=ctx-$(devopsy --context-hash app)`.
Base images and downloads are not covered: rebuild with `--pull`. devopsy
builds nothing itself.

### Output, secrets and debugging

devopsy prints what it runs to stderr, with secrets masked as `***`: every
value from the project's `.env` (on servers `shared/.env`) and every
variable named like a secret (`PASSWORD`, `TOKEN`, `KEY`...), 8 characters
or longer. Only the message is masked. Masking is a safety net: pass secrets
through the environment, not arguments (`devopsy exec -T db sh -c
'MYSQL_PWD="$MARIADB_ROOT_PASSWORD" mariadb'`).

`--verbose` (`-v`, or `DEVOPSY_VERBOSE=1`) also prints the files loaded and,
for targets, each SSH command and its script; it carries over to nested and
server-side devopsy.

```sh
devopsy --debug                       # versions, the project, environments, aliases, labels
devopsy --debug environments [name or target] [--yaml]   # computed, with each value's origin
devopsy --debug schema [--user]       # every config key, commented
devopsy --debug labels | imports | capabilities
devopsy @vm1:prod --debug             # the same, as the server sees it
```

Help and `--debug` are in color on a terminal, unless `NO_COLOR=1`.

## Servers

### Config: `.devopsy/config.yaml`

`devopsy --init` writes one (it asks for the project's name and never
touches an existing file); `devopsy --debug schema` documents every key.

```yaml
project: shop                   # its name on servers: required
instances: required             # optional: required, or none; unset: optional
releases: {keep: 5}             # optional: releases kept per environment
defaults:                       # what every environment takes unless it sets its own
  mode: image                   # build (default) or image
  release: deploy               # what --release runs: required for it
  rollback: deploy              # what --rollback runs: required for it
  destroy: destroy              # what --destroy runs: required for it
  env: {CERTRESOLVER: acmedns}  # per-environment variables, not secrets
environments:
  prod:
    env:
      DEVOPSY_DOMAINS: example.org www.example.org
  staging: {}
  "pr-*":                       # a pattern: @pr-12, @pr-feature
    releases: {keep: 1}
```

Commit it: no secrets, no servers. `.devopsy/config.local.yaml` (not
committed, never uploaded) is read over it for one machine. In `env`, `""`
sets an empty value and `~` removes a default. An environment's `path:`
moves its directory on the server, never its name. Patterns match one or
more name characters; an exact name wins, then the most literal characters;
`"*"` allows any. Unknown keys are errors.

### Targets

```
devopsy @[<server>:][<instance>/]<environment> ...

@vm1:prod                     on vm1: an SSH destination or ~/.ssh/config alias, no ":"
@vm1:b/prod                   instance b of the project, on vm1
@vm1:/prod                    no instance, whatever DEVOPSY_INSTANCE says
@prod                         server and instance from variables
```

What the target leaves out comes from your environment or the project's
`.devopsy/.env`, most specific first:

```sh
DEVOPSY_SERVER_PR_12=...       # one environment's server (upper case, others as _)
DEVOPSY_SERVER_PR=...          # a pattern's: pr-* without the *
DEVOPSY_SERVER=vm1             # any environment's
DEVOPSY_INSTANCE=b
```

A missing server is an error. **Instances** are never listed in the
project: any name works (lowercase, digits, `-`), and the first release of
one the server lacks asks before creating it (`--release --yes` in CI).
`instances: required` makes every target name one; `none` refuses them.

On the server, an environment lives in `<project>/[<instance>/]<environment>`
under the server's release root, and its compose project, containers,
volumes are named `<project>[-<instance>]-<environment>`
(`COMPOSE_PROJECT_NAME`, from which templates build their URLs).

### Releases

```sh
devopsy @vm1:prod --release      # upload, switch, run the release steps
devopsy @vm1:prod --rollback     # back to the previous release, run the rollback steps
devopsy @vm1:prod --releases     # list them, * marks the current one
devopsy @vm1:prod --log          # what the last release or rollback printed (or --log <id>)
devopsy @vm1:pr-12 --destroy     # run the destroy step, then remove the environment
devopsy @vm1:prod --env          # its variables there, env capability included
devopsy @vm1:prod --debug imports  # what devopsy sees there (capabilities, labels, imports)
devopsy @vm1:prod logs -f web    # any command, in the current release
devopsy --environments           # the project's environments on its servers: vm1:prod, vm1:b/prod...
devopsy --environments vm2       # on one server
```

`--environments` takes no target: without a server it asks each server
`DEVOPSY_SERVER` and the environments' `DEVOPSY_SERVER_<ENVIRONMENT>` name
(the caller's environment, then `.devopsy/.env`). Environments with an
explicit `path:` live elsewhere and are not listed.

Steps, in `release:` and `rollback:`, run in phases. Each is a devopsy
command line:

```yaml
release:
  before:                # before anything changes; a failure stops here
    - local: image       #   on this machine, first
    - remote: maint on   #   on the server, in the current release
  prepare: [secrets]     # on the server, in the new release, before it goes live
  run: deploy            # once it is current: the release itself
  after:                 # once live; a failure is only reported
    - remote: maint off  #   on the server, first
    - local: notify      #   then here
rollback: deploy         # only a run step: the same phases, in the restored release
```

- **The variables check:** after `prepare`, devopsy checks that every
  variable compose requires (`${VAR:?message}`) is set there, env capability
  included. A missing one fails the release before it goes live, listing the
  variables and the `--vars set` line. Generate secrets in `prepare` so the
  check finds them.
- **When `run` fails,** `current` goes back to the previous release, which is
  restarted with its own run step (the rollback's, else the release's), so
  its containers are the previous release's again. Data changes, like
  database updates, are the project's to undo.
- **One SSH session:** the remote steps share it, under the release lock.
  Hence local before steps first and local after steps last. Remote before
  steps run in the current release, with its commands, and none run on a
  first release.
- **Logs:** each release and rollback is recorded and saved on the server,
  in the release's directory (`releases/<id>/.devopsy-log`; a rollback's
  appended to the restored release's), failed ones included once the server
  was reached: devopsy's messages, the local steps run, and all the server
  printed. `--log` shows the most recent, `--log <id>` one release's. Local steps'
  own output stays on your screen only, keeping their terminal.
- **Destroying:** `--destroy` runs the environment's `destroy:` step, one
  devopsy command, in the current release under the lock, then removes the
  directory (releases, `shared/` with its data and `.env`). The step is the
  project's: devopsy knows nothing of what it runs. Usually a command that
  runs `docker compose down --volumes` and removes what its containers own
  in `shared/mnt`, which the deploy user cannot. A failed step removes
  nothing; files left that the deploy user cannot remove fail the removal.
- **Local steps** get the environment's `env`, `DEVOPSY_TARGET` (the
  resolved target) and `DEVOPSY_RELEASE_COMMIT`, never the server's `.env`.
  Without steps, devopsy prints a starting point.

- **build** mode (default) uploads the project as git sees it, uncommitted
  changes included, and builds on the server.
- **image** mode uploads `.devopsy/` only; images come from a registry. Tag
  them with the commit, `image: ghcr.io/me/app:${DEVOPSY_RELEASE_COMMIT:-local}`,
  so rollbacks run their own image.

On the server, an environment holds `releases/`, `current` and `shared/`.
Everything in `shared/` is linked into each release's `.devopsy/`: `.env`
for secrets and overrides, `mnt/` for data (bind mounts like `./mnt/data`),
anything else (a `compose.override.yaml`). Local `.env`, `mnt/` and override
files are never uploaded. Each release records its commit, branch and who
made it. A first release that fails before going live leaves nothing behind.

**Variables on a server**, nearest first: the environment's `shared/.env`,
the instance's `.env` (`<project>/<instance>/.env`), the project's
(`<project>/.env`), then the release's `target.env`, written from the
config with `COMPOSE_PROJECT_NAME`, `DEVOPSY_PROJECT`, `DEVOPSY_INSTANCE`,
`DEVOPSY_ENVIRONMENT`, `DEVOPSY_TARGET` and `DEVOPSY_RELEASE_COMMIT`.
Rollbacks bring back their release's `target.env`.

```sh
devopsy @vm1:prod --vars                       # names, values hidden
devopsy @vm1:prod --vars set DB_PASSWORD       # from your environment or .env, else a prompt or stdin
devopsy @vm1:prod --vars set --show LOG_LEVEL  # a visible prompt
devopsy @vm1:prod --vars get DB_PASSWORD
devopsy @vm1:prod --vars unset API_KEY
devopsy @vm1:b/prod --vars --instance set SITE_ID   # the instance's .env; --project: the project's
```

Values travel over SSH's stdin, never as arguments. Running containers see
changes when recreated (the next release, or the project's own `reload`).

**Release settings** belong to the server, in its deploy user's
`~/.config/devopsy/config.yaml`, so your machine and CI always agree:

```yaml
releases:
  root: /srv     # where releases live; default: the deploy user's home
  keep: 2        # default for its projects (5)
  max_keep: 5    # no project keeps more (5)
```

**On a server:** `--shell` opens a container shell as locally;
`--shell-host` your login shell on the server, in the current release,
independent of the project (the way in when it is broken). A target path
that is a plain `.devopsy/` directory without releases runs commands in
place and refuses releases. Project commands come from the current release.

**CI:** set `DEVOPSY_SSH_COMMAND` for SSH options, like git's
`GIT_SSH_COMMAND`:

```sh
DEVOPSY_SSH_COMMAND="ssh -i $KEY -o UserKnownHostsFile=$KNOWN_HOSTS" devopsy @vm1:prod --release
```

### Aliases

Your `~/.config/devopsy/config.yaml` (or `$DEVOPSY_HOME/config.yaml`) can
name targets you use from anywhere:

```yaml
aliases:
  vm1-traefik:  {source: ~/src/devopsy-template-traefik, to: "vm1:main"}
  confcatsdemo: {source: ~/src/catalyze, to: "vm1:confcatsdemo/prod"}
```

`source:` is the project's checkout: its config, and the only directory the
alias releases from. Without it, `project:` names the project, for commands
only. Inside a project its own environment of the same name wins; an alias
never reads a project's `.env`.

## Roles, exports and imports

Projects on one host share facts through compose labels. devopsy knows none
of the names; they are conventions between projects, like the Traefik
template's:

```yaml
# the proxy, on its traefik service
- devopsy.role=proxy
- devopsy.export.WILDCARD_DOMAIN=${DEVOPSY_PROXY_WILDCARD_DOMAIN:-}

# a project, on any service
- devopsy.import.DEVOPSY_WILDCARD_DOMAIN=proxy/WILDCARD_DOMAIN
```

- `devopsy.role=<name>`: only one compose project per host holds it; a
  release claiming a taken role fails before uploading anything.
- `devopsy.export.<KEY>=<value>`: a fact on the running containers (visible
  to anyone with Docker access: never secrets).
- `devopsy.import.<VAR>=<role or project>/<KEY>[?]`: at each release, before
  it goes live, the value is copied into its `target.env`, unless the
  environment already sets `<VAR>` (even empty, which opts out). A source
  not running is waited for 30 seconds; then the release fails, unless the
  import ends in `?`. An empty export is a value.

Each release keeps what it imported: `devopsy @vm1:prod --debug imports`
lists every project on that host and marks those with an outdated value as
STALE, to release again. Roles prevent mistakes, not attacks: anyone in the
docker group is root-equivalent.

## Probe

```sh
devopsy --probe [--ip <server ip>] <host>...
```

DNS through 1.1.1.1 (does it point at `--ip`, or at Cloudflare?), the
certificate the server presents, verified like a browser, and an HTTPS
request as visitors make it, with Cloudflare's 52x errors explained. Exits
1 on a problem, so it doubles as a smoke test. A proxy's own commands print
the line to run, like the Traefik template's `domains`.

## Capabilities

Interfaces devopsy defines and a project implements, as executables in
`.devopsy/capabilities/<name>/<action>`, run only by devopsy, in the
project's directory with its environment (`devopsy --debug capabilities`
lists them with their contracts):

- **env:** `compute` runs before every devopsy command in the project
  (not help or completion), locally and on servers, and prints `.env`
  lines; devopsy sets those not set yet, at the lowest precedence, and
  `--env` shows them. For values a project derives from others, like the
  site templates' hosts and Traefik rule from `COMPOSE_PROJECT_NAME`, the
  imported `DEVOPSY_WILDCARD_DOMAIN` and their own `DEVOPSY_DOMAINS`. It
  runs every time, so keep it fast; a devopsy it calls skips it
  (`DEVOPSY_ENV_COMPUTE` is set). A failure stops the command.
- **shell:** `open [service] [exec options...] [-- command...]` replaces
  devopsy's own `--shell`, locally and on servers.

## License

GPL-3.0. See [LICENSE](LICENSE).
