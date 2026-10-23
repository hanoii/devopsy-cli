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
own, and want what a hosting platform gives (an environment per branch,
releases and rollbacks, secrets out of git, URLs and certificates) without
running or paying for one.

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
  `ssh`; on the server `devopsy` and what Debian has: `tar`, GNU
  coreutils and util-linux (`flock`, `setsid`, `script`).
- **Releases are complete directories,** run through a `current` symlink;
  `shared/` keeps data and secrets across them. A release never changes a
  live tree in place.
- **Steps are explicit.** `--release` and `--rollback` refuse without the
  steps the environment names. A release's steps come from the checkout,
  which is what it uploads; a rollback's, the restart after a failed `run`
  and `--destroy`'s step come from the release they act on, its own
  `config.yaml` on the server.
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
- **Secrets stay out of sight:** the ones the project names are masked in
  output, and values never go in arguments.

Everything else is yours to make (an AI skill to help is coming soon).

## Templates

I created these templates as starting points, and as examples of how to work
with devopsy. Together they set up a stack for deploying complex web
applications, with an environment per branch or pull request, from your own
repository and in relatively little time.

They are GitHub template repositories: start a project from one ("Use this
template"), or sync it and keep it as a remote to pull its changes when you
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
binary (with `sudo` when its directory is not yours; on servers set up
with devopsy-server it is the deploy user's, so `devopsy @<server>
--upgrade` upgrades a server from your machine). At a terminal it
checks for a new release once a day in the background and says so; never in
CI or in what devopsy runs on servers, and `DEVOPSY_NO_UPDATE_CHECK=1` turns
it off.

To run a checkout's code instead, symlink `scripts/devopsy` into your `PATH`
(it rebuilds on every run, instantly when nothing changed; needs Go).

### Compose's help

`devopsy -- --help` is compose's own help as devopsy's (its flags go
before the command: `devopsy --profile tools run --rm jq`), and `devopsy
<command> --help` the help of one command.

### Shell completion

```sh
source <(devopsy --completion bash)   # ~/.bashrc
source <(devopsy --completion zsh)    # ~/.zshrc, after compinit
echo 'command -q devopsy; and devopsy --completion fish | source' > ~/.config/fish/completions/devopsy.fish
```

It completes targets (`@prod`, and environments after `@vm1:` or
`@vm1:b/`), aliases, then servers after `@` (`@vm1:`, from the `Host`
lines of `~/.ssh/config` and the files it includes), project commands with
their descriptions, devopsy's flags and, through compose's own completion,
compose's commands and services. It never connects to servers. The scripts
ask devopsy at every Tab, so they follow the installed version.

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
devopsy --release      # a release in place: defaults:' prepare, images, run
devopsy --destroy      # containers, volumes, built images and mnt's data, after asking
devopsy                # help: devopsy's own, the project's commands, the rest
devopsy --env          # the variables devopsy loads and computes, secrets masked (--show)
devopsy --shell [service] [-- command]   # a shell, or a command, in a container
devopsy -- <args>      # docker compose <args>, even if a project command has that name
devopsy -v deploy      # what devopsy found and runs (-vv: more)
```

**`devopsy --release`** without a target is a release in place, in the
local project, as a server runs one: the release steps under `defaults:`
in `config.yaml` (an error without them), `prepare`, the variables check,
the images (always as build mode: services with a `build:` are built with
`--pull`, the others pulled, since a local image-mode project has no tag
of its own in a registry), then `run`. No upload and no switch, so
nothing to go back to: a failure stops there. No `before` or `after`
steps: those are the operator's side of a deploy (pushing an image,
announcing it). A step whose command only runs on a target
(`## Run: target`) refuses before anything runs. So the project's
`deploy` only starts what is there, on a server and locally alike.

**`devopsy --destroy`** without a target starts over, after asking for
the project's name (`--yes` skips it): the `destroy:` step under
`defaults:` if there is one, then the containers, volumes and built
images down and `.devopsy/mnt` emptied, as a server's `--destroy` does.
The checkout stays.

A word is a project command if `.devopsy/commands/` has it, else a compose
command (devopsy asks compose which exist), else an error. Arguments starting
with a flag go to compose unchecked (`devopsy --profile x up`), except a
flag alone: compose's flags never stand alone, so one devopsy does not know
is an error, with the nearest flag of its own (`devopsy --ssh-host`: did
you mean `--shell-host`?), locally and with a target.

**Project commands:** any executable in `.devopsy/commands/`, run with the
project's `.env` loaded, `DEVOPSY_PROJECT_DIR` (the `.devopsy/` path) and
`DEVOPSY_COMMAND_NAME`. A `## Description:` line near the top shows in help.
Two more lines say where it runs:

- `## Run: local`: only on your machine (a build that pushes an image); on
  a server it fails. `## Run: target`: only through `devopsy @<target>`,
  on the server (a deploy); locally it fails. Without the line, where it is
  called: locally, or on the target with one.
- `## Needs: target` or `## Needs: server`, with `## Run: local`: it runs
  locally but takes an address, `devopsy @prod <command>` or
  `devopsy @vm1 <command>` (any target gives its server), and gets it as
  typed in `DEVOPSY_COMMAND_TARGET`, to call devopsy with: nothing else of
  the target's. As a release's `local:` step it gets the target's
  address.

Help shows them like a flag's scope (`(runs locally)` after a local
command that takes an address), completion offers each command where it
runs, and a release refuses steps that name one in the wrong place
(`local:` steps run locally, the rest on the server) before anything
changes. devopsy tells a server from your machine by the release's
`devopsy-target.env`, so `target` commands do not run in plain
directories.

```sh
#!/bin/sh
## Description: Copy a target's database locally
## Run: local
## Needs: target
devopsy @"$DEVOPSY_COMMAND_TARGET" drush sql-dump > .devopsy/mnt/dump.sql
devopsy drush sql-cli < .devopsy/mnt/dump.sql
```

A command can wrap a compose command of the same name: `commands/up` calling
`devopsy up` reaches compose, not itself.

**`.env`** is read with compose's own parser; your environment wins over it,
as in compose.

**Variables devopsy adds**, for compose files and commands:
`COMPOSE_PROJECT_NAME` (always set, see below), `DEVOPSY_PROJECT_DIR`, and
whatever the project's `env` capability computes (see Capabilities), which
is how projects derive values like their hosts.

To run plain compose with the same values: `devopsy --env --show > /tmp/env &&
docker compose -f .devopsy/compose.yaml --env-file /tmp/env config` (a file:
compose reads `--env-file` more than once).

**Project name:** devopsy always sets `COMPOSE_PROJECT_NAME`: the config's
`project:`, else compose's top-level `name:`, else the directory containing
`.devopsy/`, normalized; your own `COMPOSE_PROJECT_NAME` wins. On servers
it is always `<project>[-<instance>]-<environment>` (below), whatever
`name:` or your environment says: nothing of your local environment goes
to a server.

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

What a command produces goes to stdout (compose's output, `--env`,
`--vars`, lists, `--export`'s data); everything devopsy says goes to
stderr, every line starting with `devopsy:`, so it stands apart from what
compose, Docker and your commands print around it, locally and from a
server alike:

```
devopsy: releasing to vm1:prod (vm1:shop/prod): 5 files, 6.4 kB, image
devopsy: warning: uncommitted changes outside .devopsy/ are not released in image mode
devopsy: error: dev's parent, prod, has no server
devopsy: | devopsy reads a parent's server from variables only
devopsy: | to fix it:
devopsy: | $ devopsy @dev --sync --from @vm1:prod
devopsy: debug: ssh -T vm1
```

After the prefix comes the level: none for what devopsy does, `warning:`
when it goes on but something needs attention, `error:` when the command
fails, `debug:` (`-v`) and `trace:` (`-vv`) for what it runs. Details and
fixes follow on `| ` lines, a command to copy after `$ `. Questions start
with `devopsy:` too, and are only asked at a terminal: without one, a
command that would ask fails and says to add `--yes`.

Secrets are opt in: `secrets:` in `config.yaml` names the variables whose
values are secret (`[DB_PASSWORD, DATABASE_URL, "*_TOKEN"]`, `*` for any
characters), wherever they are set: `.env`, `shared/.env`, the caller's
environment. Name every secret a project uses, the ones it generates
included: nothing else is masked, and nothing guesses from names.

- `--env`, `--vars` and `--debug env` print those values as `***`, and
  `--show` prints them all. On a server the release's `config.yaml`
  decides; `--vars` also takes your checkout's, which may be newer.
- devopsy's own messages (what it runs, `-v` and `-vv`) mask them too,
  wherever they appear, when 8 characters or longer. Only the message is
  masked. Masking is a safety net: pass secrets through the environment,
  not arguments (`devopsy exec -T db sh -c 'MYSQL_PWD="$MARIADB_ROOT_PASSWORD"
  mariadb'`).
- What commands print is theirs: devopsy masks only its own lines.
- A name that looks like a secret's (`PASSWORD`, `TOKEN`, `KEY`, `SALT`,
  `AUTH`...) and is not listed stops `--release` (the compose files'
  variables and the target's `env:`) and `--vars set`: devopsy asks, and
  fails without a terminal or with `--yes`. List it in `secrets:`, or in
  `public:` when it is not a secret (`public: [SORT_KEY]`). The names only
  ask: they never mask.

`-v` (or `DEVOPSY_VERBOSE=1`) also prints the files loaded, what devopsy
runs (`running 'docker compose ...'`) and, for targets, each SSH session;
`-vv` (`DEVOPSY_VERBOSE=2`) also the script each session runs. It carries over to nested and server-side devopsy. A script
in a command's arguments (`devopsy run --rm tool sh -c '<several lines>'`)
shows as `<script: 12 lines>` in what devopsy says it runs, whole with
`-vv`.

```sh
devopsy --debug                       # versions, the project, environments, aliases, labels
devopsy --debug environments [name or target] [--yaml]   # computed, with each value's origin
devopsy --debug schema [--user]       # every config key, commented
devopsy --debug env [--show] [VAR...]  # each variable: what sets it, what that overrides
devopsy --debug labels | imports | capabilities
devopsy @vm1:prod --debug             # the same, as the server sees it
devopsy @vm1:prod --debug env         # the release's variables, traced there
```

`--debug env` traces the variables of `--env`, or the ones named: the
definition in use first, then every overridden one with its value, by
precedence. It tells the caller's environment, each `.env` file (on servers
with what it links to, like `shared/.env`), `devopsy-target.env` and what the
release imported into it, what devopsy sets itself and what the `env`
capability computed. Secrets are masked as above, unless `--show`. For a
target, the caller's environment is the server's SSH session plus the
target's `COMPOSE_PROJECT_NAME`, which devopsy sends.

Help, `--debug`, devopsy's messages and the lines a server sends are in
color on a terminal, unless `NO_COLOR=1`; `FORCE_COLOR=1` forces them
through a pipe or in CI (`0` or `false` do not), and `NO_COLOR` wins.
Releases and syncs run their sessions without a terminal, so their steps,
builds and run step get one on the server (`script`, from util-linux)
sized like yours, and a build's live progress shows as it would locally.
Logs are saved without any of it. Variables named `DEVOPSY_INTERNAL_*` are
how one devopsy tells another these things: never set or read them, they
change in any release.

## Servers

### Config: `.devopsy/config.yaml`

`devopsy --init` writes one (it asks for the project's name and never
touches an existing file); `devopsy --debug schema` documents every key.

```yaml
project: shop                   # its name on servers: required
instances: true                 # optional: every target names an instance; default false: none does
releases: {keep: 5}             # optional: releases kept per environment (live ones)
secrets: [DB_PASSWORD, "*_TOKEN"]  # optional: variables whose values devopsy masks
public: [SORT_KEY]              # optional: named like secrets, but not
defaults:                       # what every environment takes unless it sets its own
  mode: image                   # build (default) or image
  release: deploy               # what --release runs: required for it
  rollback: deploy              # what --rollback runs, as the restored release's config says: required there
  destroy: backup               # optional: what --destroy runs first, while everything is up
  env: {CERTRESOLVER: acmedns}  # per-environment variables, not secrets
environments:
  prod:
    env:
      DEVOPSY_DOMAINS: example.org www.example.org
    inherit: [DATA_DATABASE]   # variables its children get from its server
  staging: {}
  develop:
    parent: prod                # a child: its first release syncs prod's data
  "pr-*":                       # a pattern: @pr-12, @pr-feature
    releases: {keep: 1}
    parent: prod
```

Commit it: no secret values, no servers. `.devopsy/config.local.yaml` (not
committed, never uploaded) is read over it for one machine. In `env`, `""`
sets an empty value and `~` removes a default; `parent: ~` and `inherit: ~`
remove theirs. Patterns match one or
more name characters; an exact name wins, then the most literal characters;
`"*"` allows any. Unknown keys are errors.

### Targets

```
devopsy @[<server>:][<instance>/]<environment> ...

@vm1:prod                     on vm1: an SSH destination or ~/.ssh/config alias, no ":"
@vm1:b/prod                   instance b of the project, on vm1
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

A missing server is an error. **Instances** are opt in: a project with
`instances: true` has them, and every target names one (the address,
`DEVOPSY_INSTANCE` or an alias's `to:`); any other refuses them, so one
project's environments never mix both layouts on a server. They are never
listed in the project: any name works (lowercase, digits, `-`), and the
first release of one the server lacks asks before creating it (`--release
--yes` in CI).

On the server, an environment lives in `<project>/[<instance>/]<environment>`
under the server's release root, and its compose project, containers,
volumes are named `<project>[-<instance>]-<environment>`
(`COMPOSE_PROJECT_NAME`, from which templates build their URLs).

A child's **parent** (`parent:`, a concrete environment of the project)
resolves like a target of its own, one level only: at the child's
instance, on the server its variables name (`DEVOPSY_SERVER_<PARENT>`,
`DEVOPSY_SERVER`), never the child's: a guess could copy the wrong data,
so a sync without one fails and says which variable to set. `@vm2:develop`
with `DEVOPSY_SERVER_PROD=vm1` syncs from `vm1:prod`. Without the
variable, an address does it for one command: `devopsy @vm2:develop
--release --from @vm1:prod`, or `--release-sync --from`, `--sync --from`.
Parents can chain (`pr-*` from `staging` from `prod`), never in a cycle:
two environments that are each other's parent could never be released,
since a child's first release syncs from a released parent. `devopsy
--debug environments` shows it resolved.

### Releases

```sh
devopsy @vm1:prod --release      # upload, switch, run the release steps
devopsy @vm1:prod --rollback     # back to the previous release, run the rollback steps; the one left is marked rolled back
devopsy @vm1:prod --releases     # list them and their images, * marks the current one
devopsy @vm1:prod --releases remove failed   # remove releases (ids or failed) and the images only they hold
devopsy @vm1:prod --log          # what the last release or rollback printed (or --log <id>)
devopsy @vm1:pr-12 --destroy     # take it down and remove the environment, data included
devopsy @vm1:develop --sync     # replace a child's data with its parent's, run its run step
devopsy @vm1:develop --reset    # start a child over: all its data goes, then a sync as on its first release
devopsy @vm1:develop --release-sync       # a release and the parent's data in one go (--release-no-sync: never)
devopsy @vm1:prod --export database > db.sql   # a service's data, as its devopsy.data label says
devopsy @vm1:prod --export database | devopsy --import database   # prod's database locally
devopsy @vm1:prod --import database --resolved   # accept the data an import that failed left, start what it kept down
devopsy @vm1:prod --status       # the release there, a sync running into or from it, its containers, the project's own lines
devopsy @vm1:prod --env          # its variables there, env capability included, secrets masked
devopsy @vm1:prod --debug imports  # what devopsy sees there (capabilities, labels, imports)
devopsy @vm1:prod logs -f web    # any command, in the current release
devopsy --environments           # the project's environments on its servers: vm1:prod, vm1:b/prod...
devopsy @vm2 --environments      # on one server
```

`--environments` without a target asks each server `DEVOPSY_SERVER` and
the environments' `DEVOPSY_SERVER_<ENVIRONMENT>` name (the caller's
environment, then `.devopsy/.env`). It lists the project's layout only:
`<instance>/<environment>` with `instances: true`, else `<environment>`.

**Server commands** need only a server: `--shell-host`, `--ssh-keys`,
`--ssh-config`, `--environments`, `--vars-project` and `--upgrade`
(`--environments` and `--vars-project` with the checkout's project, or an
alias's). They take `@<server>`
alone, as well as any target, whose server they use (no instance
needed). A bare name is still the project's environment or an alias
first, as for every target, else a server; `@<server>:` says it is a
server when a name is both. Every other command needs an environment.

```sh
devopsy @vm1 --ssh-keys          # vm1's deploy user's keys, from anywhere
devopsy @prod --ssh-keys         # prod's server's, in the project
devopsy @vm1 --upgrade           # the server's devopsy, as the deploy user
```

Commands on a server run part of their work in the server's devopsy
(releases' images, for one), so it must be recent enough for yours: an
older one stops a command that needs what it lacks before anything
changes, says what that is, and says to run `devopsy @<target>
--upgrade`, which always works. Commands that need nothing new still
run. A sync across servers checks the child's devopsy against the
parent's the same way, before the parent stops anything.
`DEVOPSY_PROTOCOL_CHECK=warn`
turns that stop into a warning and goes on, at your own risk: for a
local build ahead of the servers, when what you run may not need what
the server's devopsy lacks.

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
    - local: notify      #   then locally
rollback: deploy         # only a run step: the same phases, in the restored release
```

- **One SSH session:** the remote steps share it, under the release lock.
  Hence local before steps first and local after steps last. Remote before
  steps run in the current release, with its commands, and none run on a
  first release.
- **Local steps** get the environment's `env`, `DEVOPSY_RELEASE_COMMIT`
  and, for a child, `DEVOPSY_PARENT` and `DEVOPSY_PARENT_TARGET`, never
  the server's `.env`. Never `DEVOPSY_TARGET`, `DEVOPSY_ENVIRONMENT` or
  `DEVOPSY_INSTANCE`: those mean a release on a server, where they come
  from `devopsy-target.env` (an `env` capability can tell local work
  from a release by them). A step that needs the address is a command
  with `## Needs: target`, which gets it in `DEVOPSY_COMMAND_TARGET`.
  Without steps, devopsy prints a starting point.
- **The variables check:** after `prepare`, devopsy checks that every
  variable compose requires (`${VAR:?message}`) is set there, env capability
  included. A missing one fails the release before it goes live, listing the
  variables and the `--vars set` line. Generate secrets in `prepare` so the
  check finds them, and name them in `secrets:`.
- **The names check:** containers of the release's compose project from
  another environment's directory fail the release before it goes live,
  after `prepare` and the variables check (compose cannot read its
  files while a variable they require is missing). Names are joined
  with `-`, so instance `a-b` of `c` and instance `a` of `b-c` are both
  `<project>-a-b-c`, and Compose would take the other's containers and
  volumes as its own. Rename an instance or environment.
- **The networks check:** a network the compose file names as its own
  (not `external`) that belongs to another compose project on the server
  fails the release before it goes live, after `prepare` and the
  variables check: Compose would silently share it, and the two
  projects' containers would reach each other by service name. Name a
  project's networks from `COMPOSE_PROJECT_NAME`; to reach another
  project, join its network as `external`.
- **The mounts check:** a writable bind mount in the release's tree, data
  or an owner with no entry of `mnt`, an entry with two owners, `./mnt`
  whole as data, a bind mount from below an owned entry, fail the
  release before it goes live, after `prepare` and the variables check
  ("Data and mounts", below).
- **The owners:** right after the switch, before a sync and the run
  step, the entries of `mnt` the labels own are created when missing and
  given to their owners (`devopsy.mnt.owner`, "Data and mounts"); a
  failure fails the release as its run step would.
- **Images:** after the checks, devopsy gets the release's images, by
  `mode:`, on the server and before it goes live: in build mode `docker
  compose pull --ignore-buildable` (services without a `build:`) then
  `docker compose build --pull` (the rest, base images refreshed); in
  image mode `docker compose pull`, every service with an `image:`,
  buildable ones included, so a tag missing from the registry fails the
  release there. Only the services Compose starts by default: other
  profiles get theirs when used. The run step's `up` then finds them all;
  a `deploy` needs no `build` or `pull` of its own. A failure leaves
  `current` as it was, its images included; what the failed release got
  is tagged as its own, so it goes with it. So does an interrupted one
  (Ctrl-C, a lost connection; see Dropped connections below): the build
  or pull stops, and the release is marked failed. A drop the server
  never hears of leaves it marked fetching until the next release,
  `--destroy` or `--releases remove` marks it failed.
- **When `run` fails,** devopsy first says what the release's containers
  were left as, into the release's log too, so `--log` shows why: each
  container's state and health, then, for every one neither running and
  healthy nor exited 0, its last health checks and its last 100 log
  lines. Then `current` goes back to the previous release, which
  is restarted with its own run step, from its own config (its `rollback:`
  run step, else its `release:` one), with its own images back, so its
  containers are the previous release's again. Data changes, like database
  updates, are the project's to undo. A first release has nothing to go
  back to: it takes down what it started (`down`, volumes and `mnt`
  kept), and `--status` says the release failed.
- **A child's first release** syncs its parent's data between switching
  `current` and `run`, so `run` (`deploy`: database updates, cache clears)
  works on the parent's data and no empty site is ever served. A failed
  copy does not go live. `--release-sync` does that on any release,
  asking for the environment's name first when it already has one, as
  `--sync` does (`--yes` skips it); `--release-no-sync` never syncs, a
  child's first release included (see Parent environments and syncs).
  `--from @<address>` syncs such a release from another environment of
  the project instead of the parent; a child's only, and only on a
  release that syncs.
- **Dropped connections:** before the switch, a session that drops (a
  cancelled CI job, a killed devopsy) cancels the release at once: the
  server notices and undoes what it fetched. A connection that dies
  silently (a laptop asleep or off the network) cancels it within about 30
  seconds: devopsy sends the server a heartbeat, and a silence counts as
  the session gone (so does devopsy itself stopped that long; 90 seconds
  for a release that syncs). Syncs and `--export` do the same; `--import`
  counts 2 minutes of silence.
  From the switch on, a release no longer depends on your connection: it
  finishes, or fails and brings the previous release back, on its own, and
  `--log` shows how. Ctrl-C before the switch cancels the release. After
  it, the release goes on to its end (the run step, pruning, the remote
  after steps): devopsy says so, at a terminal or in CI alike, and a
  second Ctrl-C only ends the session. To go back, `--rollback` once it
  ends. A release that syncs is the exception during its copy: Ctrl-C
  there aborts the sync, and the release goes back (Parent environments
  and syncs).
- **Logs:** each release and rollback is recorded on the server, in the
  release's directory (a rollback's and a sync's added to the release they
  go to), failed ones included once the server was reached, in two logs:
  `devopsy-log-session`, what your session said and saw up to the switch,
  or to the run step after a sync (devopsy's messages, the local steps
  run, the server's lines, the copy's), each line after its time in UTC
  (`2026-10-09 15:24:32`), and `devopsy-log-server`, what the server
  printed from there on, written there whatever happens to your
  connection. `--log` shows the two joined, progress displays reduced to
  their final state: the most recent, or `--log <id>` one release's. Local
  steps' own output stays on your screen only, keeping their terminal.
- **Rolled back:** `--rollback` marks the release it leaves (`ROLLED
  BACK` in `--releases`): like a failed one it never counts toward
  `keep` and a later rollback never goes back to it, so after a fix is
  released, the next rollback skips the bad release. Its log stays until
  pruning takes it.
- **Each release keeps its images:** once fetched, every image the release
  runs gets a second tag, `devopsy-<compose name>-<release id>` (a long
  compose name shortened, a checksum added when a release names one
  repository twice), without changing the names Compose runs
  (`drupal11-prod-app`, `mariadb:11`), images pulled by digest included. A
  rollback, or the restart after a failed `run`, points those names back
  at that release's tags: exactly the images it ran, floating tags
  included, without building or pulling. A release whose images are gone
  builds or pulls again. One exception: a shared name (`mariadb:11`) whose
  newer image another compose project on the server runs, any environment,
  instance or project, or a container outside Compose, stays on it, and
  the release says so: rolling back one environment never changes another.
  Pin such images (`mariadb:11.8.3`) for exact rollbacks. A floating tag's
  bookmark is what the name held when the release fetched: two releases
  pulling `mariadb:11` at the same moment may each bookmark the other's.
- **Images go with their releases:** pruning a release removes its tags.
  An image no other release still tags, of any environment, instance or
  project on the server, also loses the names that release ran that still
  point to it (`mariadb:11.8.9`, `registry/app:<commit>`), so Docker
  deletes it; then `docker image prune -f` removes what that left dangling
  (server-wide: an untagged image built by hand goes too). Kept releases,
  the ones a rollback goes to, keep theirs, and a name that moved on to a
  newer image stays. Docker refuses a name a container uses, even a
  stopped one (a `run` without `--rm`, a service dropped from the compose
  file): it stays, recorded in the project's directory
  (`.devopsy-images-left`), which `--releases` lists, and every later
  pruning, `--releases remove` or `--destroy` of the same project retries
  it, so it goes once its container does. A name another project's compose
  file uses, with no container and no release of its own holding it, goes
  too; that project pulls it again. Images are the server's, so removing
  them needs the server's image lock alone, while fetches share it:
  pruning never waits for it, and when another release holds it (devopsy
  names it and for how long), skips this time, releases and images, and
  the next release prunes them.
- **Removing releases by hand:** `--releases remove <id>...` takes
  releases away as pruning does, images included; an id can be its start,
  and `failed` is every release that never went live (one cut off before
  its switch only when older than the current one: a newer one may be
  between its upload and its activation, so name it by id), whose run
  failed, or that a rollback left, ids alongside. The current release is
  refused, and every id is checked before anything goes. It asks first
  at a terminal, and takes `--yes` in scripts. Data in `shared/` is never
  touched.
- **Destroying:** `--destroy`, under the lock, in the current release
  (else the newest one): the environment's `destroy:` step if it has one
  (optional, one devopsy command, while everything still runs: a last
  dump), then `docker compose down --volumes --remove-orphans --rmi local`
  (every profile; the images built without an `image:` name go too), then
  `shared/mnt` emptied as root in `devopsy-util`, since its files belong
  to container users; then each release's images, as pruning removes them;
  then the directory (releases, `shared/` with its data and `.env`). It
  takes the server's image lock before anything goes, waiting up to 10
  minutes for releases fetching images, and fetches wait while it runs. A
  failure before the removal removes nothing. The project's and the
  instance's `.env` stay, shared by their environments; when none is left
  in their directory, `--purge` removes them with it (at a terminal
  devopsy asks; otherwise a warning says how), also after the environment
  is gone.

Two modes, `mode:` in the config:

- **build** mode (default) uploads the project as git sees it, uncommitted
  changes included, and builds on the server, before the release goes
  live. It needs a git repository: git's ignore rules decide what goes
  (`vendor/`, dumps and local secrets stay behind).
- **image** mode uploads `.devopsy/` only; images come from a registry. Tag
  them with the commit,
  `image: ghcr.io/me/app:${DEVOPSY_RELEASE_COMMIT:-local}`, so rollbacks
  run their own image. devopsy pulls them before the release
  goes live: build and push them first, like a local before step.

Image mode needs no git: without it the release just has no commit.
Either way files arrive with git's modes, whatever the checkout's: 0755
when executable, else 0644 (a checkout synced by ddev's Mutagen has 0600
files, which a container running as another user cannot read). A mode a
project needs is set in its image (`COPY --chmod`) or by its `deploy`.

A release says how much it uploads, shows the upload's progress on one
line while it runs (on a terminal only: CI gets none), then its id, size
and time, which the release log keeps. The server names it: its own UTC
second, always later than every release already there, so releases from
laptops and CI with different clocks stay in order.

On the server, an environment holds `releases/`, `current` and `shared/`.
Each release is a directory, `releases/<id>/`, with the project as
uploaded in `code/`, where `current` points and commands run, and
devopsy's own files beside it, never in the project's tree, so a build
context only ever sees the project: `devopsy-release.json` (commit,
branch, who), `devopsy-target.env` (its environment), `devopsy-images`
(its images), `devopsy-log-session` and `devopsy-log-server`,
`devopsy-pending` until it goes live, `devopsy-fetching` while it gets
its images, `devopsy-failed` when it never went live or its run failed,
and `devopsy-rolled-back` once a rollback left it.

Everything in `shared/` is linked into each release's `.devopsy/`: `.env`
for secrets and overrides, `mnt/` for data (bind mounts like `./mnt/data`),
anything else (a `compose.override.yaml`). Local `.env`, `mnt/` and override
files are never uploaded. Each release records its commit, branch and who
made it. A release that fails before going live stays, marked failed, a
first one too: `devopsy @<target> --destroy` removes an environment that
never went live, `--releases remove failed` the release alone.
One that fails in its `run` step leaves what the step started: a container
left restarting recreates its bind mounts under `current` as a directory
(Docker does, as root), and the next release refuses until it is taken
down and that directory removed, saying how.

**Data and mounts.** `.devopsy/mnt/` is devopsy's one data directory:
releases never upload it, and on servers it is a link to `shared/mnt`, one
per environment, so data outlives releases, rollbacks and pruning. Bind
mount its entries (`./mnt/<name>:/path`), one per concern (`storage`,
`database`, `logs`), never `./mnt` whole. The alternatives:

- **Named volumes** survive releases too (the compose project name stays
  the same) and take the ownership of the image's directory; `down
  --volumes` removes them. Harder to inspect and back up from the host.
- **Absolute paths** work, but devopsy knows nothing of them: keeping,
  backing up and removing them is the project's.

`shared/mnt` starts empty and belongs to the deploy user, Docker creates
a missing bind mount's directory as root, and devopsy copies owners
exactly (rsync and tar as root, numeric ids): a container running as
non-root from its first process could not write there, nor write what
an import or a sync brought in with another machine's owners. So a
service says who owns the entries it writes:

```yaml
services:
  app:
    user: "${APP_UID:-10001}:${APP_UID:-10001}"
    volumes: [./mnt/storage:/app/storage, ./mnt/logs:/app/logs]
    labels:
      devopsy.mnt.owner: "${APP_UID:-10001}:${APP_UID:-10001}"
```

`devopsy.mnt.owner=<uid>:<gid>`, numbers from 0 to 4294967294 (names
cannot be resolved from the host; `010001` is `10001`), covers every entry of `mnt` the service mounts writable,
in any profile; a read-only mount (`:ro`, a log reader) claims none, and
an entry no service owns stays as it is, the deploy user's (right for
what is written from the host). It is never taken from `user:`, so
changing that never silently changes the data's owner: a variable keeps
the two alike. devopsy applies it, as root in `devopsy-util` without a
network, never following a symbolic link, each entry in a container
that mounts that entry alone, so a directory the app swaps for a link
meanwhile can lead nowhere else:

- **a release**, right after its switch (before a sync and the run
  step), and a rollback and the restart after a failed run, from the
  restored release's own labels: each owned entry missing is created
  (0755) and given to its owner, then every owned entry is scanned
  whole, so what devopsy did not write (an upload
  by hand, data from before the label) gets its owner too; a file a
  running service removes meanwhile is no failure. A failure fails the
  release as its run step would: the previous one comes back. In a
  rollback or a restart, labels that cannot be read (names from before
  a change, a compose error) are only a warning, nothing given away: a
  rollback never fails for its release's labels. Before its switch, a
  rollback makes sure `devopsy-util` is there when the release it
  restores owns entries, so an image pruned since fails it before
  anything changes;
- **a sync**: an entry the child owns lands as the child's owner, by the
  child's labels (`rsync --chown`), whatever the parent's files are;
- **an `--import`** of a `mnt` service: what it extracted goes to its
  owner before the import completes (locally on macOS nothing, as
  below: Docker Desktop's file sharing may refuse the chown);
- **a reset**, once it emptied `mnt`;
- **locally**, before `up`, `start`, `restart`, `run`, `create`,
  `watch` and `scale` (not with `--dry-run`): an owned entry missing is created,
  and on Linux an entry whose directory is not its owner's is given
  away, scanned whole; one whose directory is
  its owner's is never scanned, since this runs before every such
  command. On macOS, Docker Desktop and OrbStack let any container user
  write the Mac's files, so only the directories are made. Labels that
  cannot be read are only a warning: the command goes on, and compose
  says what is wrong with its files. So is an owned entry that is a
  symbolic link (an entry shared between worktrees): it is left as it
  is, and Docker mounts what it points to. The labels come from the project's
  compose files (`compose.yaml` and its override), never from one a
  command adds with `-f`. In a release on a server these commands do
  none of it: each release's switch did.

Two services whose owners differ on one entry are refused; one without
the label may write an owned entry (a tool sharing it). A stream's
import writes as the service's own user, and owners persist through
restarts and reboots.

Before a release goes live, after its `prepare` steps (which may make
the secrets its compose file requires), the mounts check refuses
(nothing changes):

- a writable bind mount from inside the project's tree outside `mnt`
  (`./src:/app`, `./cache:/cache` in `.devopsy/`): it lives in the
  release, and the next one replaces it. Read-only ones, configuration
  shipped with the release, are fine, and so are files from `shared/`
  linked into it;
- `devopsy.data=mnt` or `devopsy.mnt.owner` on a service that mounts no
  entry of `mnt` (writable, for the owner), or whose
  `devopsy.data.exclude` leaves all of them out: a sync would copy
  nothing of it, and a child's first release go live on empty data;
- one entry written by services with different owners;
- `./mnt` whole as a data or owned entry, refused wherever labels are
  read: it would carry every other service's data and give them one
  owner. Mounted writable by any other service too: it would write
  every entry, and no sync or import sees it there. Read-only (a tool
  reading every entry), devopsy ignores it;
- a bind mount from below an owned entry (`./mnt/storage/files` when
  `storage` is owned), by any service, read-only too: the entry's owner
  can replace that path with a symbolic link, and Docker resolves a
  bind mount's source on the host, so a link to `/` would mount the
  host's root. Bind the entry itself (`./mnt/storage:/app/storage`) and
  use the path below it inside the container;
- an owned entry that is a symbolic link in `shared/mnt`: Docker would
  mount what it points to, and devopsy give that to the owner.

Labels the owners' step after the switch could not read (a name from
before a change, a leftover `.devopsy/capabilities/sync/`) refuse the
release here too. With an entry owned, it also builds `devopsy-util`
when the server lacks it, so a registry that cannot be reached refuses
the release before it goes live.

Named volumes and absolute paths are not checked. `--destroy` empties
`shared/mnt` as root in `devopsy-util`. Back up `shared/.env` and
`shared/mnt` (databases as dumps).

**Data and its labels.** Everything devopsy does with data is an export
(a read) or an import (a replacement), of one service or of all of them
at once: `--export <service>` and `--import <service>` (below) move one;
a sync (a child's data from its parent's, below) exports every service
with data on the parent and imports each on the child. devopsy knows
nothing of what the data is: compose labels say it, three of them, read
on each side from its own release (and `devopsy.mnt.owner`, above, says
whose its files are).

`devopsy.data` says the service has data and how it moves:

- `devopsy.data=mnt`: the entries of `.devopsy/mnt` the service
  bind-mounts (`./mnt/files` gives `files`; a file, `./mnt/app.ini`,
  itself; never `./mnt` whole, refused). A sync rsyncs them (`rsync -a
  --delete --numeric-ids --modify-window=-1`, times compared to the
  nanosecond, the child's copy equal to the parent's,
  owners and modes included, but an entry the child owns, as its owner's:
  `devopsy.mnt.owner`, above); `--export` and `--import` move them as a
  tar. Both as root in `devopsy-util`.
- `devopsy.data=stream`: the data capability's `export <service>` writes
  it to stdout (a database dump), and its `import <service>` reads it
  from stdin (a load).
- `devopsy.data=none`, or no label: the service has no data, neither
  copied nor touched. An empty value is an error, not none: a variable
  left unset by accident (`devopsy.data=${DATA_DATABASE}`) must not stop
  copying silently; a variable turns a service's copying off with
  `none` (`devopsy.data=${DATA_DATABASE:-mnt}`, `DATA_DATABASE=none`).
  Any other value is an error, and so are the labels before these
  (`devopsy.sync...`), naming what replaces them.

`devopsy.data.exclude=<entry>[,<entry>...]` on a `mnt` service names
entries of `mnt` it mounts that are not copied through it anyway: an
access log next to the site's files (`./mnt/logs`). On `stream` and
`none` it is ignored, so a variable can flip the method and keep it.
The entries a `mnt` service mounts cannot have whitespace in their
names: copies refuse them.

`devopsy.data.stop` says when devopsy stops the service while data
moves:

- `never` (the default): it keeps running.
- `own`: while its own data is exported or imported.
- `import`: while any data is imported.
- `always`: while any data is exported or imported.

And a service that `depends_on` a stopped one stops with it (every
`depends_on`, `required: false` and Compose's implicit ones included): it
cannot work without it anyway. `own` and `import` are not nested, and
`always` is both: the values are not a scale. A database copied as files
labeled `import` (reading as "stronger than `own`") is tarred while it
runs on `--export`, which exits 0 with a corrupt copy. `own` without a
`devopsy.data` label is an error (it would never stop); with `none`
(from a variable) it is ignored.

| operation                       | `never` | `own` | `import` | `always` |
| ------------------------------- | ------- | ----- | -------- | -------- |
| its own data exported           | up      | down  | up       | down     |
| its own data imported           | up      | down  | down     | down     |
| another service's data imported | up      | up    | down     | down     |
| another service's data exported | up      | up    | up       | down     |

Recipes:

- A database copied as files (`mnt`): `own`, never `import`. Its
  `--export` stops its app too (a dependent), held by a project's own
  service if it has one (devopsy-template-drupal11's `alwaysup`).
- A database copied as a dump (`stream`): `never`: the script needs the
  server running.
- Anything that uses other services' data while it runs, an app, a
  worker, a cron container: `import`. Its own files (`mnt`) can be
  exported while it runs.
- An app keeping its own SQLite file in `mnt`: `mnt` with `own`, or
  `stream` with `import` and a script using `sqlite3 .backup`.
- A cache (Redis): nothing; flush it in the project's `import` script if
  stale entries matter.
- A service that holds visitors while the app is down: nothing, and it
  must never `depends_on` the app, or it stops with it.

An operation over every service (a sync's side) takes the union, under
one `docker compose stop` (Compose orders it, dependents first) and one
`start` afterwards, waited for until healthy: with two `own` databases,
each is down while the other moves too. Only what runs is stopped, and
devopsy says what and why before it does (`stopping database (own), app
(depends on database)`); a dependent already down, or in a profile that
is not up, stays as it was. On an import, and on a child's side of a
sync, a running service that depends on an imported one and carries no
`devopsy.data.stop` label is named: label it `import`, or `never` to
keep it running and silence it. Nothing is paused: Traefik drops a
paused container's router, and `docker pause` flushes nothing. A service
kept down (below) with `restart: always` is said: Docker starts it again
when its daemon starts, a reboot, on the data as it is; `unless-stopped`
keeps it down until devopsy starts it.

**Parent environments and syncs.** A child (`develop: {parent: prod}`)
is a clone of its parent: its first release syncs the parent's data, and
`devopsy @<child> --sync` does it again later, then reruns the `run`
step of the child's deployed release (its own config, never the
checkout's). What moves is what the labels say:

- `mnt` services' entries go from the parent's `shared/mnt` into the
  child's: a file written in place, so the container mounting it sees
  the new content. Files a running service removes during the copy
  (rsync's "vanished", its exit 24) are a notice, not a failure: the
  rest is copied. An entry missing on the parent is skipped, said, and
  the child's is left as it is.
- `stream` services: the parent's `export <service>` goes into a file
  in the child's directory (`.devopsy-sync.stream.<service>.<sync id>`),
  and only once the export exited 0 does the child's `import <service>`
  read it, the child's service up (devopsy starts it, alone, unless it
  stopped it); the file goes afterwards. A cut export never reaches the
  import. One service after the other, in compose's `depends_on` order
  (dependencies first). Both sides must label the service `stream`, or
  the parent refuses before it stops anything. A label from an inherited
  variable (`devopsy.data=${DATA_DATABASE:-mnt}`, `inherit:
  [DATA_DATABASE]`) follows the parent's in the same sync: change it on
  the parent, then `--sync`.

On the parent, the sync stops its `own` data services and its `always`
ones, with what depends on them; on the child, its `own`, `import` and
`always` ones, with what depends on them, for the whole sync. The child
stops its set before it knows what the parent copies: when the parent
has nothing to copy, its `import` services were down for a few seconds
for nothing. A stop word that differs between the sides (a child's own
`shared/.env`) is named.

A project without any service with data, or whose services have
nothing to copy, syncs nothing ("nothing to sync"): the parent's
`prepare` and `finish` do not run, and the child's `run` step still
runs. On a child whose data is from a sync that did not complete, it
completes nothing: the earlier copy's data is still there, and the sync
fails.

Each side reads the labels from its own release, so they can differ: a
variable in the label (`devopsy.data=${DATA_DATABASE:-mnt}`) that the
parent inherits to its children keeps them alike. What the parent rsyncs
must not change under a running service of the child: before changing
anything of its own, the parent looks at the child's services and
refuses when one that mounts a copied entry is still running there,
labeled `stream` or without data (one the child stopped is not running;
`mnt` on the child says a running copy is wanted). The child restores
its services, nothing was copied. A service streamed on one side and not
the other is refused the same way (a label the child sets in its own
`shared/.env` can still differ).

Order on the child's server, under its release lock for the whole sync:
the capability's `prepare child`, its side's set stopped; then it waits
for the parent. On the parent's, under a shared hold of its release lock
(a release of the parent waits for the copy) and a sync lock of its own
(syncs from one parent run one at a time): `prepare parent`, its side's
set stopped, the rsync, each stream (export, then the child's import),
its services started, `finish parent go|abort`. Then the child's: its
services started, `finish child go`, and its `run` step. The child's
stopped services stay down through the imports, so an app never runs on
a half-imported database, and after a copy that did not complete too
(below). A child's first release creates, before `finish child go`, its
services with data and its side's set (an `import` app without data, an
unlabeled app depending on an `own` database), so the hook can sanitize
through them; one in a profile that is not up (`COMPOSE_PROFILES`) stays
uncreated, as the run step's `up` would leave it. A lock held by something else is announced ("waiting for the
release running on ..."), then waited for up to 10 minutes, like every
release lock.

The rsync runs as root in `devopsy-util`, devopsy's own image (alpine with
rsync and GNU tar, find and chown; it replaced `devopsy-rsync`, which
`docker image rm devopsy-rsync:2` removes, and `devopsy-util:1`, which
`docker image rm devopsy-util:1` removes), built on each server the first time, so
ownership and the deploy user's permissions do not matter. **On one
server**, one container copies between the two directories, without SSH,
and a stream is written into the child's directory directly. **Across
servers**, the parent's server starts an rsync daemon (read-only, serving
only the entries being copied, no published port) and reaches the child's
over SSH, through a reverse forward, with your SSH agent forwarded for
that session only, and never on to the child; a stream goes to the child's
file through an SSH session of the parent's too. Data goes server to
server, never through your machine. The sync's SSH sessions send
keepalives every 15 seconds, so a firewall does not drop a quiet copy, and
the parent gives up on a child it cannot reach, or that refuses its key,
within 30 seconds rather than keeping its services stopped for minutes.
So, across servers:

- an agent must be running where devopsy runs, with keys both servers
  accept: the parent's server reaches the child's through it (devopsy
  checks the agent has keys before anything starts, not which). In CI,
  `eval $(ssh-agent); ssh-add` with the CI key the servers accept,
  instead of `ssh -i`;
- the parent's server can use every key in that agent for the session,
  not only the child's: with `prod` the parent of `staging` and `staging`
  the parent of `pr-*`, a sync of `pr-12` forwards the agent to
  staging's server, prod's key included. When servers trust each other
  less than that, load only the keys the sync needs, or constrain them
  to their path (OpenSSH 8.9 or later, hosts as in `known_hosts`):

  ```sh
  ssh-add -h vm1 ~/.ssh/vm1                # the parent's server, from your machine only
  ssh-add -h vm2 -h 'vm1>vm2' ~/.ssh/vm2   # the child's: from your machine and from vm1
  ```

- the child's server must be reachable from the parent's as `ssh -G`
  resolves it locally (user, hostname, port; its host key is accepted on
  first use). A host behind a `ProxyCommand` or `ProxyJump` is refused: a
  relay through your machine is not supported yet;
- a CI key needs `restrict,agent-forwarding,port-forwarding` in the deploy
  user's `authorized_keys` on both servers, what `--ssh-keys add
  --restricted` writes: the agent for the parent's session, the reverse
  forward for the child's. A plain `restrict` refuses both, and devopsy
  says which.

```sh
devopsy @develop --sync                         # asks for the environment's name
devopsy @develop --sync --yes                   # CI
devopsy @develop --sync --from @b/prod          # another environment of the project
devopsy @develop --sync --yes -- --exclude=cache  # arguments for rsync
```

`--sync` asks you to type the environment's full name
(`<project>/[<instance>/]<environment>`, `shop/develop`), as `--destroy`
and `--import` do,
and so does `--release-sync` on a child already released; `--yes`
skips it. First-release syncs never ask. `--from` copies from another
environment of the project instead of the parent: an address, as after
`devopsy`, with its `@` (devopsy takes it without one too). What follows
`--` goes to rsync, and to the data capability's actions as
`DEVOPSY_SYNC_ARGS`.

**Starting a child over:** `devopsy @develop --reset` (same options as
`--sync`) is a sync that first throws away everything a sync leaves on
the child: every container of every profile, its named volumes, and
every entry of `shared/mnt`, the ones no label copies included (logs,
an unlabeled service's files). The parent's data then goes in as on a
first release, the services with data are created before `finish
child go`, and the run step starts the rest. The release, its images
and the child's `shared/.env` (its secrets) stay. It is the way out of
data nothing else fixes: a sync or an import that did not complete, a
half-written entry the parent does not have. Everything a sync checks
comes first, and the child is wiped only at the copy's first write, so
a parent that refuses or cannot be reached leaves it as it was; cut
after that, the child is down and marked, as after a sync cut
mid-copy, until a sync or a reset completes. A parent with no data to
copy refuses. Children only: an environment without a parent starts
over with `--destroy` and a release. It asks for the environment's name,
like `--sync`.

**Inherited variables:** `inherit: [KEY, ...]` on the parent (or in
`defaults:`) names variables its children get, whatever the labels say
about the data: read from the parent's `shared/.env`, instance `.env` and
project `.env` on its server (nearest first) and written as the child's
`shared/inherit.env`, the whole file each time, so a resync replaces it.
The child reads it below its own `shared/.env` and above its server's
instance and project files: a value the child sets itself wins (devopsy
says so when one shadows an inherited key: `--vars unset` it to inherit),
and no other environment on the child's server is touched. On a first
release this happens before the upload, so the variables check finds
them (they stay if the sync fails: the next release is a first one
again). On `--sync` and a later `--release-sync` it happens before the
copy too, into a pending file (`<environment>/.devopsy-sync.inherit.env`)
that everything the sync runs on the child reads in place of
`shared/inherit.env`, labels included, so a label from an inherited
variable follows the parent's in the same sync; once the copy is in it
moves into place, before the child's services restart and its run step,
and a failed copy removes it, leaving the child's variables as they
were. `devopsy @<child> --inherit` refreshes the variables alone, no
data, no run step.
Names are printed, never values; keys the parent does not set are
listed. Not `DEVOPSY_*` or `COMPOSE_PROJECT_NAME`: those are the target's.

**The data capability** (`.devopsy/capabilities/data/`, see
Capabilities) adds to this: `export` and `import` for the `stream`
services, required then, in syncs and `--export` and `--import`; and in
syncs only, `prepare` and `finish` on both sides, each only where its
file is, around devopsy's own handling. A database labeled `stream`, in
short:

```sh
# capabilities/data/export: $1 is the service
exec devopsy exec -T "$1" sh -c 'mariadb-dump --single-transaction ...' </dev/null
# capabilities/data/import: the dump on stdin, complete
exec devopsy exec -T "$1" sh -c 'mariadb ...'
```

Each side's database credentials stay its own: the dump runs with the
parent's, the import with the child's. Whether the export exited 0 is
devopsy's to check, not the scripts'. A script whose service must run
refuses when it is in `DEVOPSY_DATA_STOP`.

**One service's data, anywhere:** `devopsy [@<target>] --export <service>`
writes a service's data to stdout, and `devopsy [@<target>] --import
<service>` replaces it with stdin, the way its label says: a `stream`
through the capability's `export` and `import`, a `mnt` service as a tar
of its entries of `mnt` (made and extracted as root in `devopsy-util`,
numeric owners and modes kept; an import empties each entry the tar
carries first, as a sync's `--delete` does, and extracts it in a
container without a network that mounts that entry alone, so a running
service that swaps a directory for a link meanwhile can lead the
extraction nowhere else; an entry its labels own is given to its owner
once extracted). What its stop labels say is
stopped around either (an export: the service if `own` or `always`, every
`always` one; an import: the service if `own`, `import` or `always`, every
`import` and `always` one; with what depends on them), and comes back
after. A `stream` service not stopped is brought up for its import, alone
(`--no-deps`), and down again after it if it was down. A service without
data is refused. Production's database and files on a laptop, with no
project command:

```sh
devopsy @prod --export database | devopsy --import database
devopsy @prod --export storage | devopsy --import storage
devopsy @prod --export database > database.sql     # a backup
devopsy @staging --import database < database.sql   # asks for staging's full name (--yes in CI)
```

`--import` spools stdin to a file first (`.devopsy-sync.stream.<service>.*`,
under `.devopsy/` locally, which releases never upload; next to the
environment on a server) and changes nothing until it is all there; a
truncated tar (cut anywhere, or missing its end-of-archive blocks),
or one carrying entries the service does not mount or excludes, is
refused, and so is one carrying `mnt` itself (`./`: make it with `tar -C
mnt -cf - <entry>...`), a member reaching out of its entry, or a device
node or a FIFO anywhere (as root, tar would make them on the server). On a target, `--export`
holds the release lock shared and the environment's sync lock (a sync from
it waits), `--import` the release lock, and asks for the environment's
full name on the terminal unless `--yes` (stdin is the data); one silent
for 2 minutes counts as cut. `--export` of data a sync or an import left
partial warns and goes on. A pipe cannot tell an export that failed
halfway from one that ended, so for what matters, go through a file and
check the export's exit code.

**An import that fails:** nothing is replaced until the data has fully
arrived; from the first replacement until the import completes, the
environment is marked and what devopsy stopped stays down.

- Before the import starts writing (devopsy asks Docker to start the
  hook, or the first container that empties and extracts), a failure, a
  Ctrl-C or a session gone restores what was stopped, and nothing is
  imported. On a target devopsy waits for a word from your session right
  before that write, so a session gone since the data came imports
  nothing.
- From then on, on a target, the import goes on whatever happens to your
  session (a Ctrl-C says so, a second one ends the session, and `--status`
  shows it), its output also in `<environment>/.devopsy-sync.log`. The
  environment is marked first, and what was stopped recorded, so a
  devopsy killed or a server rebooted mid-load leaves them too. A
  failure (a Docker error that started nothing included) ends whatever
  may still write and keeps it all down: what was stopped, the imported
  service and what depends on it. Nothing runs on that data: a release
  or a rollback refuses (a release before its upload), the restart after
  a failed release is skipped, `--status` says so and `--export` warns;
  only that service can be imported until one completes. The ways out:
  the import again from a complete file (`devopsy @prod --import
  database < database.sql`), which starts what the import kept down and
  only then removes the mark; on a child, a `--sync` that copies all of
  that service (every entry the child's service mounts: one the parent
  lacks would keep what the import half wrote, so the sync refuses
  first, naming it) or a `--reset`, which starts it over from its
  parent; or `--resolved`, below. The next import, or `--resolved`,
  first ends a load a killed devopsy left running. `--destroy` still
  works, its `destroy:` step running with what the import kept down
  still down.
- Locally there is no mark: the error says what it left down and prints
  `devopsy start ...` to bring it back.

When the dump itself cannot complete (an error near its end, every retry
the same), `devopsy @<target> --import <service> --resolved` accepts the
data as it is: what is missing does not matter, or you fixed it by hand
(`devopsy @<target> up -d --no-deps <service>`, then `--shell
<service>`). It reads no stdin and loads nothing; it asks for the
environment's full name, saying what it starts (`--yes` without a
terminal), starts what the import kept down, the service included, and
only once all of it is healthy removes the mark. A start that fails keeps
it: fix it and run it again. On a target only, and never over a sync's
mark: a sync's partial copy is completed only by a sync, which replaces
an import's mark when it is cut too. A `--release-sync` of a child marked
by an import whose parent does not copy that service fails at its push,
after the switch, and goes back: import or `--resolved` first. A project
command that runs `devopsy --export` itself, on a server whose devopsy is
older than the labels, fails without the upgrade line.

**Logs and failures:** a sync's output goes into the log of the release
it synced into (`--log`), after the release's own. The parent's side
always restores its stopped services. The child's side knows what the
copy did to its data, whatever happened to devopsy: the parent marks it
right before its first write into the child and after its last, and the
child goes by that:

- **Nothing written** (a prepare or a check failed, the parent's stop
  failed, the first export failed with nothing to rsync): its stopped
  services come back, `finish child abort`, and the run step is
  skipped. On data an earlier sync or an import left partial, it stays
  partial (below), and what was down for it stays down; what this sync
  stopped comes back.
- **Written in part** (the rsync or an import failed or was cut,
  Ctrl-C mid-copy, a connection lost): the child's data is part old, part
  the parent's, so its stopped services stay down and the run step is
  skipped, `finish child partial`. Nothing goes on writing into it
  first: an import still running inside its service's container (a
  load through `docker exec`, which a cut never stops) ends with the
  service, stopped and started again by the next sync that completes,
  and devopsy's own copy containers go. A database copied raw would not start
  on it anyway (devopsy says which entry was cut). Nothing runs on it
  until a sync completes: releases and rollbacks refuse, the previous
  release is not restarted after a release that synced, a sync from it
  refuses, and `--status` says so. `--sync` (or `--release-sync`)
  completes it, or `--reset` starts it over.
- **All of it** (the copy completed, even if devopsy then went away or
  was interrupted): it goes on as after a success, `finish child go`,
  then the run step. A sync completes with its finish: one whose `finish
  child go` fails leaves the child as one that did not complete: the
  copy is in, but the project refused it, and `--status` and every
  refusal say so.

On a first release a failure marks the release failed and leaves nothing
current, and the next release syncs again. A failed or interrupted export
imports nothing, and its file goes. Interrupted (Ctrl-C) before the copy,
devopsy tells the child's server to abort and waits, as a release does:
it finishes the step it is in, restores what it stopped and shows it, and
a second Ctrl-C leaves at once. Interrupted during the copy,
devopsy ends its session to the parent's server, which stops the copy (an
export too) and restores its own services, then tells the child's to
finish as above. Once the copy is in and the run step runs, a Ctrl-C only
says so: the step is the server's, and a second Ctrl-C ends your session
while the step ends there (`--log` shows how). If devopsy itself goes away
mid-sync (killed, connection lost, or silent: the parent's side gives up
after 30 seconds without devopsy's heartbeat, the child's after 90), each
side finishes on its own and writes what happened to
`<environment>/.devopsy-sync.log` on its server; a child's side killed
before deciding is decided, as not completed, by whatever takes its lock
next: a sync, a release or a rollback; the last two then refuse (a release
that syncs is a sync). Restoring means what the sync stopped
(`<environment>/.devopsy-sync.services` meanwhile): a service that was
down before it, stopped for maintenance, stays down. Only what the
child's side stopped is kept off partial data: a service left running
during the copy (a `mnt` one labeled `never`, a stream imported into a
running one) sees it change, as it chose.

**Variables on a server**, nearest first: the environment's `shared/.env`,
a child's `shared/inherit.env` (what its parent's `inherit:` gave it),
the instance's `.env` (`<project>/<instance>/.env`), the project's
(`<project>/.env`), then the release's `devopsy-target.env`, written from the
config with `COMPOSE_PROJECT_NAME`, `DEVOPSY_PROJECT`, `DEVOPSY_INSTANCE`,
`DEVOPSY_ENVIRONMENT`, `DEVOPSY_TARGET` and `DEVOPSY_RELEASE_COMMIT` (and a
child's `DEVOPSY_PARENT` and `DEVOPSY_PARENT_TARGET`).
Rollbacks bring back their release's `devopsy-target.env`.

```sh
devopsy @vm1:prod --vars                       # KEY=value, secrets as *** (--show: all)
devopsy @vm1:prod --vars set DB_PASSWORD       # from your environment or .env, else a prompt or stdin
devopsy @vm1:prod --vars set --show LOG_LEVEL  # a visible prompt
devopsy @vm1:prod --vars get DB_PASSWORD
devopsy @vm1:prod --vars unset API_KEY
devopsy @vm1:b/prod --vars-instance set SITE_ID   # the instance's .env
devopsy @vm1 --vars-project set SHARED_KEY        # the project's .env on vm1
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
`--shell-host` your login shell on the server, in the current release
(in the release root with `@<server>` alone), independent of the project
(the way in when it is broken). An environment's directory that is a plain
`.devopsy/` directory without releases runs commands in place and refuses
releases. Project commands come from the current release.

**CI:** set `DEVOPSY_SSH_COMMAND` for SSH options, like git's
`GIT_SSH_COMMAND`:

```sh
DEVOPSY_SSH_COMMAND="ssh -i $KEY -o UserKnownHostsFile=$KNOWN_HOSTS" devopsy @vm1:prod --release
```

Its options come first, so they win over devopsy's own: ssh keeps the first
value of each. devopsy adds `-o ClearAllForwardings=yes` to every session:
it never needs the host's `LocalForward` lines, and each session would try
to bind them again while another holds them.

**SSH settings:** a release opens several SSH sessions (checks, upload,
activation, its log), each a new connection and handshake: noticeable
through a jump host or VPN. `devopsy @<server> --ssh-config` prints a
`Host` block for `~/.ssh/config`: where it connects (`Hostname`, `User`,
`Port`), then connection sharing (`ControlMaster`, `ControlPersist`) and
keepalives, commented out. It never connects.

**Keys on a server.** Who may log in as the deploy user is its
`~/.ssh/authorized_keys`, and `devopsy @<server> --ssh-keys` edits it over
the usual session, as that user, no root; any target on the server does
too. `--ssh-keys` lists them (fingerprint, type, comment, options),
`--ssh-keys add <file>` authorizes a public key, `--ssh-keys remove
<fingerprint or comment>` removes one (never the last: it would lock
everyone out). `add --restricted` is for a machine's key: `restrict`
(commands only, no shell) plus the agent and port forwarding a sync across
servers needs. Without a target, `devopsy --ssh-keys-generate [<name>]`
makes an ed25519 pair in `~/.ssh` (`devopsy-ci` by default; `--out <file>`
elsewhere), nothing sent anywhere. For CI: generate, `add` the `.pub` on
each server with `--restricted`, the private key as a protected file
variable, and in the job `eval "$(ssh-agent)"; ssh-add "$DEVOPSY_SSH_KEY"`
before `devopsy @prod --release`. Rotate by generating another, adding it,
then removing the old one.

### Aliases

Your `~/.config/devopsy/config.yaml` (or `$DEVOPSY_HOME/config.yaml`) can
name targets you use from anywhere:

```yaml
aliases:
  vm1-traefik: {source: ~/src/devopsy-template-traefik, to: "vm1:prod"}
  shop-demo:   {source: ~/src/shop, to: "vm1:demo/prod"}
```

`source:` is the project's checkout: its config, and the only directory the
alias releases from. Without it, `project:` names the project, for commands
only. Inside a project its own environment of the same name wins; an alias
never reads a project's `.env`, and its instance is its `to:`'s, never
`DEVOPSY_INSTANCE`'s. With `project:`, a `to:` naming an instance is how
it knows the project has them.

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
  it goes live, the value is copied into its `devopsy-target.env`, unless the
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
request as visitors make it, with Cloudflare's 52x errors explained, and
its edge having no certificate for a name (a proxied second-level
subdomain, beyond its free certificate). Exits
1 on a problem, so it doubles as a smoke test. A proxy's own commands print
the line to run, like the Traefik template's `domains`.

## Capabilities

Interfaces devopsy defines and a project implements, as executables in
`.devopsy/capabilities/<name>/<action>`, run only by devopsy, in the
project's directory with its environment (`devopsy --debug capabilities`
lists them with their contracts):

- **status:** `show` prints the project's own lines under `devopsy
  --status`, after the containers: its URL, a maintenance mode, a version,
  one fact per line, with the project's environment loaded (the drupal11
  template's). Locally and on servers (`devopsy @prod --status` runs it
  in the current release there, after the release's record, the parent
  and the last sync).
- **env:** `compute` runs before every devopsy command in the project (not
  help or completion), locally and on servers, and prints `.env` lines;
  devopsy sets those not set yet, at the lowest precedence, and `--env`
  shows them. For values a project derives from others, like the site
  templates' hosts and Traefik rule from `COMPOSE_PROJECT_NAME`, the
  imported `DEVOPSY_WILDCARD_DOMAIN` and their own `DEVOPSY_DOMAINS`. It
  runs every time, so keep it fast; a devopsy it calls skips it. A failure
  stops the command.
- **shell:** `open [service] [exec options...] [-- command...]` replaces
  devopsy's own `--shell`, locally and on servers.
- **data:** the transport of the services labeled `devopsy.data=stream`,
  in syncs and `--export` and `--import`, and hooks into syncs (Parent
  environments and syncs). Which operation runs which action: `--export`
  runs `export`, `--import` runs `import`, `--import --resolved` none; a
  sync runs all four, `prepare` and `finish` only there. `prepare` and
  `finish` run only when their file is there, besides devopsy's own
  handling of the labels; `export` and `import` are required on their side
  when a service is `stream` there, checked before anything stops. Hooks
  run in the foreground: what one leaves running in the background ends
  when it returns. A capability left in `.devopsy/capabilities/sync/` is
  an error naming this one.
  - `prepare parent|child` runs on each side, in its current release,
    before anything is stopped there: the child's under its release lock,
    before the parent's side starts; the parent's under a shared hold of
    its lock, and only when there is something to copy. A failure aborts
    the sync (`finish abort` runs).
  - `export <service>` writes a `stream` service's data to stdout: on the
    parent's server, once what the sync stops there is stopped and the
    rsync is done, once per `stream` service in `depends_on` order; and
    `devopsy --export`. stdin is empty. devopsy spools what it writes and
    imports it only once it exited 0. A service in `DEVOPSY_DATA_STOP` is
    stopped: reach it with `devopsy run --rm --no-deps -T <service> ...`,
    not `exec`, or refuse.
  - `import <service>` reads that data from stdin, complete: on the
    child's server and in `devopsy --import`, its service up (devopsy
    brings it up alone first) unless it is in `DEVOPSY_DATA_STOP`.
  - `finish parent|child go|abort|partial` runs on each side once the sync
    ended. The parent's gets `go` or `abort`, what the copy did, as the
    child's: `go` once it completed, even when a service of the parent's
    did not come back (that fails the command, said). Its data is never
    written, and its stopped services are started again. The child's says what the copy did
    to its data: `go`, complete; `abort`, nothing of the parent's written;
    `partial`, written in part (Logs and failures, above). After `go` and
    `abort` devopsy has started its stopped services and waited for them
    to be healthy; after `partial` they stay down, and the hook must not
    start them. On the child after `go` it is where a copy of production
    is sanitized (emails and passwords scrubbed, a login set for a child
    that had none: devopsy-template-drupal11's); then the release's `run`
    step runs. A failing `finish child go` fails the sync, and nothing
    runs on the child's data until a sync completes: the hook decides what
    stays up (drupal11's stops the app). devopsy restarts only what a sync
    stopped, so what the hook stopped is still down when the next sync's
    `finish child go` runs: the hook starts what it needs. On a child's first release
    nothing of it existed, so devopsy creates its services with data and
    its side's set first (`up -d --wait`), as the run step's `up` would a
    moment later: the hook finds them running either way. `finish child
    partial` also runs outside a sync, when a child's side was killed
    before deciding: in whatever takes its lock next, a sync, a release or
    a rollback (the last two then refuse).
  - Every action gets its side's services by label, space separated:
    `DEVOPSY_DATA_MNT`, `DEVOPSY_DATA_STREAM` (in `depends_on` order), and
    `DEVOPSY_DATA_STOP`, what the operation stops on its side by the
    labels (at `finish ... go|abort` back up by then; at `finish child
    partial`, what stays down); a stopped `stream` service is in both. In
    a sync also `DEVOPSY_SYNC_FROM_TARGET` and `DEVOPSY_SYNC_TO_TARGET`
    (the parent's and the child's addresses) and `DEVOPSY_SYNC_ARGS`
    (what followed `--sync`'s `--`). The other `DEVOPSY_SYNC_*` variables
    a push sets are devopsy's own.

## License

GPL-3.0. See [LICENSE](LICENSE).
