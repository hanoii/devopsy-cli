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
  `ssh`; on the server `devopsy`, `tar`, `flock` and `script`.
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
CI, and `DEVOPSY_NO_UPDATE_CHECK=1` turns it off.

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

It completes targets (`@prod`, and environments after `@vm1:` or `@vm1:b/`),
aliases, then servers after `@` (`@vm1:`, from the `Host` lines of
`~/.ssh/config` and the files it includes), project commands with their
descriptions, devopsy's flags and, through compose's own completion,
compose's commands and services. It never connects to servers. The scripts ask devopsy at every Tab, so they follow
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
command that takes an address), completion offers each command where it runs, and
a release refuses steps that name one in the wrong place (`local:` steps
run locally, the rest on the server) before anything changes. devopsy tells a
server from your machine by the release's `devopsy-target.env`, so `target`
commands do not run in plain directories.

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

`-v` (or `DEVOPSY_VERBOSE=1`) also prints the files loaded and, for
targets, each SSH session; `-vv` (`DEVOPSY_VERBOSE=2`) also the script each
session runs. It carries over to nested and server-side devopsy. A script
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
`COMPOSE_PROJECT_NAME` your devopsy sends.

Help, `--debug`, devopsy's messages and the lines a server sends are in
color on a terminal, unless `NO_COLOR=1`; `FORCE_COLOR=1` forces them
through a pipe or in CI (`0` or `false` do not), and `NO_COLOR` wins.
A release that syncs runs its session without a terminal, so its run step
gets one on the server (`script`, from util-linux) sized like yours, and a
build's live progress looks as it does on a plain release. Logs are saved
without any of it. Variables named `DEVOPSY_INTERNAL_*` are how one devopsy
tells another these things: never set or read them, they change in any
release.

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
    inherit: [SYNC_DATABASE]   # variables its children get from its server
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
resolves like a target of its own, one level only: at the child's instance,
on the server its variables name
(`DEVOPSY_SERVER_<PARENT>`, its pattern's, `DEVOPSY_SERVER`), never the
child's: a guess could copy the wrong data, so a sync without one fails
and says which variable to set. `@vm2:develop` with
`DEVOPSY_SERVER_PROD=vm1` syncs from `vm1:prod`. Without the variable,
an address does it for one command: `devopsy @vm2:develop --release
--from @vm1:prod`, or `--release-sync --from`, `--sync --from`. Parents can chain
(`pr-*` from `staging` from `prod`), never in a cycle: two environments
that are each other's parent could never be released, since a child's
first release syncs from a released parent. `devopsy --debug
environments` shows it resolved.

### Releases

```sh
devopsy @vm1:prod --release      # upload, switch, run the release steps
devopsy @vm1:prod --rollback     # back to the previous release, run the rollback steps
devopsy @vm1:prod --releases     # list them, * marks the current one
devopsy @vm1:prod --log          # what the last release or rollback printed (or --log <id>)
devopsy @vm1:pr-12 --destroy     # take it down and remove the environment, data included
devopsy @vm1:develop --sync     # replace a child's data with its parent's, run its run step
devopsy @vm1:develop --release-sync       # a release and the parent's data in one go (--release-no-sync: never)
devopsy @vm1:prod --export database > db.sql   # a service's data, as its devopsy.sync label says
devopsy @vm1:prod --export database | devopsy --import database   # prod's database locally
devopsy @vm1:prod --status       # the release there, its containers, the project's own lines
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

- **The variables check:** after `prepare`, devopsy checks that every
  variable compose requires (`${VAR:?message}`) is set there, env capability
  included. A missing one fails the release before it goes live, listing the
  variables and the `--vars set` line. Generate secrets in `prepare` so the
  check finds them, and name them in `secrets:`.
- **Images:** after the checks, devopsy gets the release's images, by
  `mode:`, on the server and before it goes live: in build mode `docker
  compose pull --ignore-buildable` (services without a `build:`) then
  `docker compose build --pull` (the rest, base images refreshed); in
  image mode `docker compose pull`, every service with an `image:`,
  buildable ones included, so a tag missing from the registry fails the
  release there. Only the services Compose starts by default: other
  profiles get theirs when used. The run step's `up` then finds them all;
  a `deploy` needs no `build` or `pull` of its own. A failure leaves
  `current` as it was, its images included.
- **Each release keeps its images:** once fetched, every image the release
  runs gets a second tag, `devopsy-<compose name>-<release id>`, without
  changing the names Compose runs (`drupal11-prod-app`, `mariadb:11`),
  images pulled by digest included. A release naming one repository
  twice (`postgres:15` and `postgres:16`) gets a tag per name, ending in
  a checksum of it. Docker's tags end at 128 characters: a compose name
  longer than 94 is cut to 85, followed by 8 characters of its hash. A
  rollback, or the restart after a failed `run`, points those names back
  at that release's tags: exactly the images it ran, floating tags
  included, without building or pulling. A release without them (pruned
  images, a release older than this) builds or pulls again.
- **Images go with their releases:** pruning a release removes its tags.
  An image no other release still tags, of any environment, instance or
  project on the server, also loses the names that release ran that still
  point to it (`mariadb:11.8.9`, `registry/app:<commit>`), so Docker
  deletes it; then `docker image prune -f` removes what that left
  dangling (server-wide: an untagged image built by hand goes too). Kept
  releases, the ones a rollback goes to, keep theirs, and a name that
  moved on to a newer image stays. Docker refuses a name a container
  uses, even a stopped one: it stays, and the release says so. A name
  another project on the server runs by the same name, with no release
  of its own holding it, goes too; that project pulls it again. Images
  are the server's, so removing them takes the server's image lock
  (`.devopsy-images.lock` in the release root) alone, while fetching
  shares it: pruning never waits for it, and when another release holds
  it (naming who and for how long), skips this time, releases and
  images, and the next release prunes them.
- **The names check:** containers of the release's compose project from
  another environment's directory fail the release before it goes live.
  Names are joined with `-`, so instance `a-b` of `c` and instance `a` of
  `b-c` are both `<project>-a-b-c`, and Compose would take the other's
  containers and volumes as its own. Rename an instance or environment.
- **The networks check:** a network the compose file names as its own
  (not `external`) that belongs to another compose project on the server
  fails the release before it goes live: Compose would silently share it,
  and the two projects' containers would reach each other by service
  name. Name a project's networks from `COMPOSE_PROJECT_NAME`; to reach
  another project, join its network as `external`.
- **When `run` fails,** `current` goes back to the previous release, which is
  restarted with its own run step, from its own config (its `rollback:`
  run step, else its `release:` one), with its own images back, so its containers are the previous release's
  again. Data changes, like
  database updates, are the project's to undo.
- **One SSH session:** the remote steps share it, under the release lock.
  Hence local before steps first and local after steps last. Remote before
  steps run in the current release, with its commands, and none run on a
  first release.
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
- **Logs:** each release and rollback is recorded and saved on the server,
  in the release's directory (`releases/<id>/devopsy-log`; a rollback's
  appended to the restored release's), failed ones included once the server
  was reached: devopsy's messages, the local steps run, and all the server
  printed, each line after its time in UTC (`2026-10-09 15:24:32`),
  progress displays reduced to their final state. `--log` shows the most recent, `--log <id>` one release's. Local steps'
  own output stays on your screen only, keeping their terminal.
- **Destroying:** `--destroy`, under the lock, in the current release:
  the environment's `destroy:` step if it has one (optional, one devopsy
  command, while everything still runs: a last dump), then `docker compose
  down --volumes --remove-orphans --rmi local` (every profile; the images
  built without an `image:` name go too), then `shared/mnt` emptied as
  root in `devopsy-util`, since its files belong to container users; then
  each release's images, as pruning removes them; then the directory
  (releases, `shared/` with its data and `.env`). It takes the server's
  image lock before anything goes, waiting up to 10 minutes for releases
  fetching images, and fetches wait while it runs. A failure before the
  removal removes nothing.
  The project's and the instance's `.env` stay, shared by their
  environments; when none is left in their directory, `--purge` removes
  them with it (at a terminal devopsy asks; otherwise a warning says how),
  also after the environment is gone.
- **Local steps** get the environment's `env`, `DEVOPSY_TARGET` (the
  resolved target), `DEVOPSY_RELEASE_COMMIT` and, for a child,
  `DEVOPSY_PARENT` and `DEVOPSY_PARENT_TARGET`, never the server's `.env`.
  Without steps, devopsy prints a starting point.

- **build** mode (default) uploads the project as git sees it, uncommitted
  changes included, and builds on the server, before the release goes
  live.
- **image** mode uploads `.devopsy/` only; images come from a registry. Tag
  them with the commit, `image: ghcr.io/me/app:${DEVOPSY_RELEASE_COMMIT:-local}`,
  so rollbacks run their own image. devopsy pulls them before the release
  goes live: build and push them first, like a local before step.

A release says how much it uploads, shows the upload's progress on one
line while it runs (on a terminal only: CI gets none), then its size and
time, which the release log keeps.

On the server, an environment holds `releases/`, `current` and `shared/`.
Each release is a directory, `releases/<id>/`, with the project as
uploaded in `code/`, where `current` points and commands run, and
devopsy's own files beside it, never in the project's tree, so a build
context only ever sees the project: `devopsy-release.json` (commit,
branch, who), `devopsy-target.env` (its environment), `devopsy-images`
(its images), `devopsy-log`, and `devopsy-failed` when it never went live
or its run failed. Releases from before October 2026 kept these inside the
tree: rollbacks cannot go back to them, and their image tags stay until
removed by hand (`docker image rm`).

Everything in `shared/` is linked into each release's `.devopsy/`: `.env`
for secrets and overrides, `mnt/` for data (bind mounts like `./mnt/data`),
anything else (a `compose.override.yaml`). Local `.env`, `mnt/` and override
files are never uploaded. Each release records its commit, branch and who
made it. A first release that fails before going live leaves nothing behind.
One that fails in its `run` step leaves what the step started: a container
left restarting recreates its bind mounts under `current` as a directory
(Docker does, as root), and the next release refuses until it is taken
down and that directory removed, saying how.

**Data and mounts.** `.devopsy/mnt/` is devopsy's one data directory:
releases never upload it, and on servers it is a link to `shared/mnt`, one
per environment, so data outlives releases, rollbacks and pruning. Bind
mount from it (`./mnt/<name>:/path`). A bind mount from anywhere else in
`.devopsy/` lives in the release: the next release replaces it, and your
local copy is uploaded with each release. The alternatives:

- **Named volumes** survive releases too (the compose project name stays
  the same) and take the ownership of the image's directory; `down
  --volumes` removes them. Harder to inspect and back up from the host.
- **Absolute paths** work, but devopsy knows nothing of them: keeping,
  backing up and removing them is the project's.

`shared/mnt` starts empty and belongs to the deploy user, and Docker
creates a missing bind-mount directory as root. A container running as
non-root from the start cannot write there, and the deploy user cannot give
it away: start as root and fix ownership in the entrypoint, or give the
directory away in a one-shot `init` service running as root. `--destroy`
empties it as root in `devopsy-util`. Back up `shared/.env` and `shared/mnt` (databases as
dumps).

**Parent environments and syncs.** A child (`develop: {parent: prod}`)
is a clone of its parent: its first release syncs the parent's data, and
`devopsy @<child> --sync` does it again later, then reruns the child's
`run` step. devopsy knows nothing of what the data is: services opt in
with a label, `devopsy.sync`, which says how their data moves:

- `devopsy.sync=rsync`: devopsy copies the entries of `.devopsy/mnt` the
  service bind-mounts (`./mnt/files` gives `files`; a file,
  `./mnt/app.ini`, itself, written in place so the container mounting it
  sees the new content; `./mnt` itself, all of it), from the parent's
  `shared/mnt` into the child's, with `rsync -a --delete --numeric-ids`:
  the child's copy equals the parent's, owners and modes included. Files
  a running service removes during the copy (rsync's "vanished", its exit
  24) are a notice, not a failure: the rest is copied.
- `devopsy.sync=stream`: the parent's sync capability `export <service>`
  writes the service's data to stdout (a database dump), devopsy spools
  it into a file in the child's directory (`.devopsy-sync.stream.<service>`),
  and only once the export exited 0 does the child's `import <service>`
  read it from stdin (a load into the child's database), the child's
  service brought up first; the file goes afterwards. A cut export never
  reaches the import. One service after the other, in compose's
  `depends_on` order (dependencies first). `--export` and `--import`
  (below) use the same scripts, but through a pipe
  (`devopsy @a --export db | devopsy @b --import db`) a failed export
  looks like a short one and its import gets what came: when the export
  can fail, go through a file and check its exit code first. Both sides
  must label the service `stream`, or the parent refuses before anything
  stops. A label from an inherited variable
  (`devopsy.sync=${SYNC_DATABASE:-rsync}`, `inherit: [SYNC_DATABASE]`)
  follows the parent's in the same sync: change it on the parent, then
  `--sync`.
- no label: the service is neither copied nor touched. Any other value
  is an error.

A second label says what happens to the service meanwhile:
`devopsy.sync.stop=true` stops it on both sides for the whole sync (one
`docker compose stop`, so Compose orders them by `depends_on`, dependents
first; one `start` afterwards, waited for until healthy), for a clean
shutdown: a database's raw files rsynced, an app that must not run on a
half-imported database. The default, `false`, leaves it running: an
`rsync` service's files are copied as they are, which is the project's
call (fine for uploaded files, not for a running database's datadir); a
`stream` service is what its export and import talk to. Nothing is paused:
Traefik drops a paused container's router, and `docker pause` flushes
nothing.

| labels | copied | meanwhile |
| ------ | ------ | --------- |
| (none) | no | untouched |
| `devopsy.sync=rsync` | its `mnt` entries, rsynced | runs |
| `devopsy.sync=rsync` + `devopsy.sync.stop=true` | its `mnt` entries, rsynced | stopped |
| `devopsy.sync=stream` | the capability's `export` and `import` | runs |
| `devopsy.sync=stream` + `devopsy.sync.stop=true` | the same, the scripts using `run --rm` | stopped |

`devopsy.sync.rsync.exclude=<entry>[,<entry>...]` on an `rsync` service
names entries of `mnt` it mounts that are not copied through it anyway:
an access log next to the site's files (`./mnt/logs`), which a child does
not want. On a `stream` service it is ignored, so a variable can flip a
service between `rsync` and `stream` and keep it. Another service
mounting the same entry without excluding it still gets it copied. A
service mounting `./mnt` itself copies everything, the entries of
unlabeled and `stream` services included, against their labels: the
parent says so before the copy, with the exclude label to write.
Mounting entries, one per concern, says who owns what. `shared/.env` is
never copied: the child keeps its own secrets.

A project without any labeled service syncs nothing ("nothing to
sync"), and the child's `run` step still runs.

Each side reads the labels from its own release, so they can differ: a
variable in the label (`devopsy.sync=${SYNC_DATABASE:-rsync}`) that the
parent inherits to its children keeps them alike. What the parent rsyncs
must not change under a running service of the child: before changing
anything of its own, the parent looks at the child's services and
refuses when one that mounts a copied entry is still running there,
labeled `stream` or not at all (`devopsy.sync.stop=true` has already
stopped it; `rsync` on the child says a running copy is wanted). The
child restores its services, nothing was copied. A service streamed on
one side and not the other is refused the same way (a label the child
sets in its own `shared/.env` can still differ).

Order on the child's server, under its release lock for the whole sync:
the capability's `prepare child`, its `stop=true` services stopped; then
it waits for the parent. On the parent's, under a shared hold of its
release lock (a release of the parent waits for the copy) and a sync
lock of its own (syncs from one parent run one at a time): `prepare
parent`, its `stop=true` services stopped, the rsync, each stream
(export, then the child's import), its services started, `finish parent
go|abort`. Then the child's: its services started, `finish child
go|abort`, and after `go` its `run` step. The child's stopped services
stay down through the imports, so an app never runs on a half-imported
database. A lock held by something else is announced ("waiting for the
release running on ..."), then waited for up to 10 minutes, like every
release lock.

The rsync runs as root in `devopsy-util` (until October 2026
`devopsy-rsync`: remove that one by hand, `docker image rm
devopsy-rsync:2`), devopsy's own image (alpine
with rsync and tar), built on each server the first time, so ownership
and the deploy user's permissions do not matter. **On one server**, one
container copies between the two directories, without SSH, and a stream
is written into the child's directory directly. **Across servers**, the
parent's server starts an rsync daemon (read-only, serving only the
entries being copied, no published port) and reaches the child's over
SSH, through a reverse forward, with your SSH agent forwarded for that
session only, and never on to the child; a stream goes to the child's
file through an SSH session of the parent's too. Data goes server to
server, never through your machine. The sync's SSH sessions send
keepalives every 30 seconds, so a firewall does not drop a quiet copy,
and the parent gives up on a child it cannot reach, or that refuses its
key, in seconds rather than keeping its services stopped for minutes.
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

`--sync` asks you to type the environment's name, as `--destroy` does,
and so does `--release-sync` on a child already released; `--yes`
skips it. First-release syncs never ask. `--from` copies from another
environment of the project instead of the parent: an address, as after
`devopsy`, with its `@` (devopsy takes it without one too). What follows
`--` goes to rsync, and to the sync capability's actions as
`DEVOPSY_SYNC_ARGS`.

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

**The sync capability** (`.devopsy/capabilities/sync/`, see
Capabilities) adds to this: `prepare` and `finish` on both sides, each
only where its file is, around devopsy's own handling, and `export` and
`import` for the `stream` services, required then. A database labeled
`stream`, in short:

```sh
# capabilities/sync/export, on the parent: $1 is the service
exec devopsy exec -T "$1" sh -c 'mariadb-dump --single-transaction ...' </dev/null
# capabilities/sync/import, on the child: the dump on stdin, complete
exec devopsy exec -T "$1" sh -c 'mariadb ...'
```

Each side's database credentials stay its own: the dump runs with the
parent's, the import with the child's. Whether the export exited 0 is
devopsy's to check, not the scripts'.

**One service's data, anywhere:** `devopsy [@<target>] --export
<service>` writes a labeled service's data to stdout, and `devopsy
[@<target>] --import <service>` replaces it with stdin, the way its label
says: a `stream` through the capability's `export` and `import`, an
`rsync` service as a tar of its entries of `mnt` (made and extracted as
root in `devopsy-util`, numeric owners and modes kept; an import empties
each entry the tar carries first, as a sync's `--delete` does, and leaves
the service running unless it is stopped), `devopsy.sync.stop=true`
stopping it around either. A `stream` service that is not stopped is
brought up for its import, and down again after it if it was down. Production's database and files on a laptop,
with no project command:

```sh
devopsy @prod --export database | devopsy --import database
devopsy @prod --export storage | devopsy --import storage
devopsy @prod --export database > database.sql     # a backup
devopsy @staging --import database < database.sql   # asks for staging's name (--yes in CI)
```

`--import` spools stdin to a file first (`.devopsy-sync.stream.<service>`,
under `.devopsy/` locally, which releases never upload; next to the
environment on a server) and changes nothing until it is all there; a
truncated tar (`tar -t` fails, or the end-of-archive blocks are missing),
or one carrying entries the service does not mount or excludes, is
refused. A service mounting `./mnt` itself exports, and imports, all of
`mnt`: devopsy names what else that includes. On a target, `--export`
holds the release lock shared and the environment's sync lock (a sync
from it waits), `--import` the release lock, and asks for the environment's name on the terminal unless
`--yes` (stdin is the data); a session cut halfway imports nothing. A
pipe cannot tell an export that failed halfway from one that ended, so
for what matters, go through a file and check the export's exit code.

**Logs and failures:** a sync's output goes into the log of the release
it syncd into (`--log`), after the release's own. A failed copy restores
the stopped services on both sides and skips the child's `run` step; on
a first release, the release is marked failed and nothing is current,
and the next release syncs again. The child's `mnt` may be partly copied
then; rsync picks up from there, and a database copied raw is unusable
until it does (devopsy says which entry was cut; its container restarts
in a loop, and the next sync stops it like a running one). A failed or
interrupted export imports nothing, and its file goes. Interrupted
(Ctrl-C) during the copy, devopsy ends its session to the parent's
server, which stops the copy (an export too) and restores its own
services, then tells the child's to restore its services. Once the copy
is in and the run step runs, a Ctrl-C only says so: the step is the
server's, and cutting the session would fail it and bring the previous
release back, which a second Ctrl-C does. If devopsy itself goes away
mid-sync (killed, connection lost), each side restores its own
services and writes what happened to `<environment>/.devopsy-sync.log`
on its server. Restoring means what the sync stopped
(`<environment>/.devopsy-sync.services` meanwhile): a service that was
down before it, stopped for maintenance, stays down.

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
`~/.ssh/authorized_keys`, and `devopsy @<server> --ssh-keys` edits it over the
usual session, as that user, no root; any target on the server does too. `--ssh-keys`
lists them (fingerprint, type, comment, options), `--ssh-keys add <file>`
authorizes a public key, `--ssh-keys remove <fingerprint or comment>` removes
one (never the last: it would lock everyone out). `add --restricted` is
for a machine's key: `restrict` (commands only, no shell) plus the agent
and port forwarding a sync across servers needs. Without a target,
`devopsy --ssh-keys-generate [<name>]` makes an ed25519 pair in `~/.ssh`
(`devopsy-ci` by default), nothing sent anywhere. For CI: generate, `add`
the `.pub` on each server with `--restricted`, the private key as a
protected file variable, and in the job `eval "$(ssh-agent)"; ssh-add
"$DEVOPSY_SSH_KEY"` before `devopsy @prod --release`. Rotate by generating
another, adding it, then removing the old one.

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
- **env:** `compute` runs before every devopsy command in the project
  (not help or completion), locally and on servers, and prints `.env`
  lines; devopsy sets those not set yet, at the lowest precedence, and
  `--env` shows them. For values a project derives from others, like the
  site templates' hosts and Traefik rule from `COMPOSE_PROJECT_NAME`, the
  imported `DEVOPSY_WILDCARD_DOMAIN` and their own `DEVOPSY_DOMAINS`. It
  runs every time, so keep it fast; a devopsy it calls skips it. A failure stops the command.
- **shell:** `open [service] [exec options...] [-- command...]` replaces
  devopsy's own `--shell`, locally and on servers.
- **sync:** hooks into a sync (Parent environments and syncs), and the
  transport of the services labeled `devopsy.sync=stream`, there and in
  `--export` and `--import`. `prepare` and `finish` run only when their
  file is there, besides devopsy's own handling of the labels; `export`
  and `import` are required on their side when a service is `stream`
  there, checked before anything stops.
  - `prepare parent|child` runs on each side, in its current release,
    before anything is stopped there: the child's under its release lock,
    before the parent's side starts; the parent's under a shared hold of
    its lock. A failure aborts the sync (`finish abort` runs).
  - `export <service>` writes a `stream` service's data to stdout: on the
    parent's server, once its `stop=true` services are stopped and the
    rsync is done, once per `stream` service in `depends_on` order; and
    `devopsy --export`. stdin is empty. devopsy spools what it writes and
    imports it only once it exited 0. A service in `DEVOPSY_SYNC_STOP` is
    stopped: reach it with `devopsy run --rm --no-deps -T <service> ...`,
    not `exec`.
  - `import <service>` reads that data from stdin, complete: on the
    child's server, its service brought up first (`up -d --wait`) unless
    stopped; and `devopsy --import`.
  - `finish parent|child go|abort` runs on each side after the copy
    (`go`) or a failure (`abort`), once devopsy has started that side's
    stopped services and waited for them to be healthy. On the child
    after `go` it is where a copy of production is sanitized (emails and
    passwords scrubbed, a login set for a child that had none:
    devopsy-template-drupal11's); then the release's `run` step runs. On
    a child's first release nothing of it existed, so devopsy creates
    every labeled service first (`up -d --wait`), as the run step's `up`
    would a moment later: the hook finds them running either way.
  - Every action gets its side's services by label, space separated:
    `DEVOPSY_SYNC_RSYNC`, `DEVOPSY_SYNC_STREAM` (in `depends_on` order)
    and `DEVOPSY_SYNC_STOP` (a stopped `stream` service is in both); in a
    sync also `DEVOPSY_SYNC_FROM_TARGET` and `DEVOPSY_SYNC_TO_TARGET`
    (the parent's and the child's addresses) and `DEVOPSY_SYNC_ARGS`
    (what followed `--sync`'s `--`). The other `DEVOPSY_SYNC_*` variables
    a push sets are devopsy's own.

## License

GPL-3.0. See [LICENSE](LICENSE).
