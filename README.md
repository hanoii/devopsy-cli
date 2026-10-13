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
with devopsy-server it is the deploy user's). At a terminal it
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
devopsy -v deploy      # what devopsy found and runs (-vv: more)
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
with what it links to, like `shared/.env`), `target.env` and what the
release imported into it, what devopsy sets itself and what the `env`
capability computed. Secrets are masked as above, unless `--show`. For a
target, the caller's environment is the server's SSH session plus the
`COMPOSE_PROJECT_NAME` your devopsy sends.

Help, `--debug`, devopsy's messages and Compose's output are in color on a
terminal, unless `NO_COLOR=1`, on servers too: devopsy tells the server
your terminal takes them (`DEVOPSY_COLOR=1`, which also forces them when
set by hand). Logs are saved without them.

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
    inherit: [SYNC_DATABASE]   # variables its children get from its server
  staging: {}
  develop:
    parent: prod                # a child: its first release syncs prod's data
  "pr-*":                       # a pattern: @pr-12, @pr-feature
    releases: {keep: 1}
    parent: prod
```

Commit it: no secrets, no servers. `.devopsy/config.local.yaml` (not
committed, never uploaded) is read over it for one machine. In `env`, `""`
sets an empty value and `~` removes a default; `parent: ~` and `inherit: ~`
remove theirs. An environment's `path:`
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

A child's **parent** (`parent:`, a concrete environment of the project)
resolves like a target of its own, one level only: at the child's instance
(none if the parent has a `path:`), on the server its variables name
(`DEVOPSY_SERVER_<PARENT>`, its pattern's, `DEVOPSY_SERVER`), never the
child's: a guess could copy the wrong data, so a sync without one fails
and says which variable to set. `@vm2:develop` with
`DEVOPSY_SERVER_PROD=vm1` syncs from `vm1:prod`. Parents can chain
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
devopsy @vm1:pr-12 --destroy     # run the destroy step, then remove the environment
devopsy @vm1:develop --sync     # replace a child's data with its parent's, run its run step
devopsy @vm1:develop --release-sync       # a release and the parent's data in one go (--release-no-sync: never)
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
- **A child's first release** syncs its parent's data between switching
  `current` and `run`, so `run` (`deploy`: database updates, cache clears)
  works on the parent's data and no empty site is ever served. A failed
  copy does not go live. `--release-sync` does that on any release,
  asking for the environment's name first when it already has one, as
  `--sync` does (`--yes` skips it); `--release-no-sync` never syncs, a
  child's first release included (see Parent environments and syncs).
- **Logs:** each release and rollback is recorded and saved on the server,
  in the release's directory (`releases/<id>/.devopsy-log`; a rollback's
  appended to the restored release's), failed ones included once the server
  was reached: devopsy's messages, the local steps run, and all the server
  printed, each line after its time in UTC (`2026-10-09 15:24:32`),
  progress displays reduced to their final state. `--log` shows the most recent, `--log <id>` one release's. Local steps'
  own output stays on your screen only, keeping their terminal.
- **Destroying:** `--destroy` runs the environment's `destroy:` step, one
  devopsy command, in the current release under the lock, then removes the
  directory (releases, `shared/` with its data and `.env`). The step is the
  project's: devopsy knows nothing of what it runs. Usually a command that
  runs `docker compose down --volumes` and removes what its containers own
  in `shared/mnt`, which the deploy user cannot. A failed step removes
  nothing; files left that the deploy user cannot remove fail the removal.
- **Local steps** get the environment's `env`, `DEVOPSY_TARGET` (the
  resolved target), `DEVOPSY_RELEASE_COMMIT` and, for a child,
  `DEVOPSY_PARENT` and `DEVOPSY_PARENT_TARGET`, never the server's `.env`.
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
directory away in a one-shot `init` service running as root. For the same
reason the environment's `destroy` step, not devopsy, removes what
containers own there. Back up `shared/.env` and `shared/mnt` (databases as
dumps).

**Parent environments and syncs.** A child (`develop: {parent: prod}`)
is a clone of its parent: its first release syncs the parent's data, and
`devopsy @<child> --sync` does it again later, then reruns the child's
`run` step. devopsy knows nothing of what the data is: services opt in
with a label, `devopsy.sync`, and devopsy copies the entries of
`.devopsy/mnt` those services bind-mount (`./mnt/files` gives `files`;
a file, `./mnt/app.ini`, itself, written in place so the container
mounting it sees the new content; `./mnt` itself, all of it), from the
parent's `shared/mnt` into the child's, with `rsync -a --delete
--numeric-ids`: the child's copy equals the parent's, owners and modes
included. Files a running service removes
during the copy (rsync's "vanished", its exit 24) are a notice, not a
failure: the rest is copied. `shared/.env` is never copied: the child
keeps its own secrets.

The label's values, on both sides:

| `devopsy.sync` | flush hook | rsync of its mounts | push capability |
| --------------- | ---------- | ------------------- | --------------- |
| `skip` (no label) | no | no | no |
| `live` | yes | yes | no |
| `pause` | yes | yes | no |
| `stop` | no | yes | no |
| `custom` | no | no | yes |

- `skip`, the default: the service is neither copied nor touched.
- `live`: nothing happens to the service; its mounts are copied.
- `pause`: frozen for the copy (the routed app): requests wait in its
  socket until it resumes, as long as nothing else happens on the host
  meanwhile (Traefik drops a paused container's router the next time a
  container starts, stops or reports its health, and a sync stops
  things: see devopsy-template-traefik's AGENTS.md). A service of the
  project's own on a lower-priority router answers meanwhile and waits
  for the app (devopsy-template-drupal11's `alwaysup`).
  That is holding, not consistency: `docker pause` flushes nothing, so
  never pause a database; stop it, or dump it.
- `stop`: stopped for the copy, a clean shutdown (a database's raw files);
  started again and waited for until healthy.
- `custom`: devopsy copies nothing and touches nothing; the parent's
  `push` capability copies it, after devopsy has copied the rest (a
  database to dump). It is required then.

`devopsy.sync.exclude=<entry>[,<entry>...]` on a service names entries of
`mnt` it mounts that are not copied through it anyway: an access log
next to the site's files (`./mnt/logs`), which a child does not want.
Another service mounting the same entry without excluding it still gets
it copied. A service mounting `./mnt` itself copies everything, the
entries of `skip` and `custom` services included, against their labels:
the parent says so before the copy, with the exclude label to write.
Mounting entries, one per concern, says who owns what.

A project without any labeled service syncs nothing ("nothing to
sync"), and the child's `run` step still runs.

Each side reads the labels from its own release, so they can differ: a
variable in the label (`devopsy.sync=${SYNC_DATABASE:-stop}`) that the
parent inherits to its children keeps them alike. What the parent copies
must not change under a running service of the child: before pausing
anything of its own, the parent looks at the child's services and refuses
when one that mounts a copied entry is still running there, labeled
`custom` or not at all (`pause` and `stop` have already paused or stopped
it; `live` on the child says a running copy is wanted). The child
restores its services, nothing was copied.

Order on each side: flush, pause, stop, copy, start (healthy), resume,
then the parent's `push` for the `custom` services. The `stop` services
go to one `docker compose stop`, and later one `start`, so Compose
orders them by `depends_on`: an app that depends on its database stops
first and starts last, its requests in flight finished before the
database goes (each service's `stop_grace_period`). The
child's services are paused and stopped before the parent's and resumed
after them. The child's side holds its release lock for the whole sync;
the parent's holds a shared one, so a release of the parent waits for the
copy, and a sync lock of its own, so syncs from one parent run one at
a time (each pauses and restores the parent's services). A lock held by
something else is announced ("waiting for the release running on
..."), then waited for up to 10 minutes, like every release lock.

The copy runs as root in `devopsy-rsync`, devopsy's own image (alpine
with rsync), built on each server the first time, so ownership and the
deploy user's permissions do not matter. **On one server**, one container
copies between the two directories, without SSH. **Across servers**, the
parent's server starts an rsync daemon (read-only, serving only the
entries being copied, no published port) and
reaches the child's over SSH, through a reverse forward, with your SSH
agent forwarded for that session only, and never on to the child; data
goes server to server, never through your machine. The sync's SSH
sessions send keepalives every 30 seconds, so a firewall does not drop a
quiet copy, and the parent gives up on a child it cannot reach, or that
refuses its key, in seconds rather than pausing its services for minutes.
So, across servers:

- an agent must be running where devopsy runs, with keys both servers
  accept: the parent's server reaches the child's through it (devopsy
  checks the agent has keys before anything starts, not which). In CI,
  `eval $(ssh-agent); ssh-add` with each server's CI key (devopsy-server
  generates one per server), instead of `ssh -i`;
- the parent's server can use every key in that agent for the session,
  not only the child's: with `prod` the parent of `staging` and `staging`
  the parent of `pr-*`, a sync of `pr-12` forwards the agent to
  staging's server, prod's key included. When servers trust each other
  less than that, load only the keys the sync needs, or constrain them
  to their path (OpenSSH 8.9 or later, hosts as in `known_hosts`):

  ```sh
  ssh-add -h vm1 ~/.ssh/vm1                # the parent's server, from here only
  ssh-add -h vm2 -h 'vm1>vm2' ~/.ssh/vm2   # the child's: from here and from vm1
  ```

- the child's server must be reachable from the parent's as `ssh -G`
  resolves it here (user, hostname, port; its host key is accepted on
  first use). A host behind a `ProxyCommand` or `ProxyJump` is refused: a
  relay through your machine is not supported yet;
- a CI key from devopsy-server needs `restrict,agent-forwarding,port-forwarding`
  in the deploy user's `authorized_keys` on both servers: the agent for the
  parent's session, the reverse forward for the child's. A plain `restrict`
  refuses both, and devopsy says which.

```sh
devopsy @develop --sync                         # asks for the environment's name
devopsy @develop --sync --yes                   # CI
devopsy @develop --sync --from b/prod           # another environment of the project
devopsy @develop --sync --yes -- --exclude=cache  # arguments for rsync
```

`--sync` asks you to type the environment's name, as `--destroy` does,
and so does `--release-sync` on a child already released; `--yes`
skips it. First-release syncs never ask. `--from` copies from another
environment of the project instead of the parent. What follows
`--` goes to rsync (as `DEVOPSY_SYNC_ARGS`; a sync capability's `push`
uses it as it sees fit).

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
again); on `--sync` and a later `--release-sync`, once the copy is in
and before the child's run step, so a failed copy leaves the child as it
was; `devopsy @<child> --inherit` does it alone, no data, no run step.
Names are printed, never values; keys the parent does not set are
listed. Not `DEVOPSY_*` or `COMPOSE_PROJECT_NAME`: those are the target's.

**The sync capability** (`.devopsy/capabilities/sync/`, see
Capabilities) adds to this, each action only where its file is: `flush`
on both sides, `prepare` and `finish` on the child's, around devopsy's own
handling, and `push` on the parent's for the `custom` services, like a
database to dump. A `push` alone, with no `custom` service, is not run
(devopsy says so). With its database labeled `custom`, a project's `push`
pipes a dump into the child's database, in short:

```sh
devopsy exec -T database sh -c 'mariadb-dump --single-transaction ...' </dev/null |
  devopsy --sync-exec exec -T database sh -c 'mariadb ...'
```

Each side's database credentials stay its own: the dump runs with the
parent's, the import with the child's.

**Logs and failures:** a sync's output goes into the log of the release
it syncd into (`--log`), after the release's own. A failed copy restores
the paused and stopped services on both sides and skips the child's `run`
step; on a first release, the release is marked failed and nothing is
current, and the next release syncs again. The child's `mnt` may be
partly copied then; rsync picks up from there, and a database copied raw
is unusable until it does (devopsy says which entry was cut; its
container restarts in a loop, and the next sync stops it like a
running one). Interrupted (Ctrl-C) during the
copy, devopsy ends its session to the parent's server, which stops the
copy and restores its own services, then tells the child's to restore
its services. If devopsy itself goes away
mid-sync (killed, connection lost), each side restores its own
services and writes what happened to `<environment>/.devopsy-sync.log`
on its server. Restoring means what the sync paused or stopped
(`<environment>/.devopsy-sync.services` meanwhile): a service that was
down before it, stopped for maintenance, stays down.

**Variables on a server**, nearest first: the environment's `shared/.env`,
a child's `shared/inherit.env` (what its parent's `inherit:` gave it),
the instance's `.env` (`<project>/<instance>/.env`), the project's
(`<project>/.env`), then the release's `target.env`, written from the
config with `COMPOSE_PROJECT_NAME`, `DEVOPSY_PROJECT`, `DEVOPSY_INSTANCE`,
`DEVOPSY_ENVIRONMENT`, `DEVOPSY_TARGET` and `DEVOPSY_RELEASE_COMMIT` (and a
child's `DEVOPSY_PARENT` and `DEVOPSY_PARENT_TARGET`).
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

Its options come first, so they win over devopsy's own: ssh keeps the first
value of each. devopsy adds `-o ClearAllForwardings=yes` to every session:
it never needs the host's `LocalForward` lines, and each session would try
to bind them again while another holds them.

**SSH settings:** a release opens several SSH sessions (checks, upload,
activation, its log), each a new connection and handshake: noticeable
through a jump host or VPN. `devopsy @<target> --ssh-config` prints a
`Host` block for `~/.ssh/config`: where it connects (`Hostname`, `User`,
`Port`), then connection sharing (`ControlMaster`, `ControlPersist`) and
keepalives, commented out. It never connects.

**Keys on a server.** Who may log in as the deploy user is its
`~/.ssh/authorized_keys`, and `devopsy @<target> --ssh-keys` edits it over the
usual session, as that user, no root; any target on the server does. `--ssh-keys`
lists them (fingerprint, type, comment, options), `--ssh-keys add <file>`
authorizes a public key, `--ssh-keys remove <fingerprint or comment>` removes
one (never the last: it would lock everyone out). `add --restricted` is
for a machine's key: `restrict` (commands only, no shell) plus the agent
and port forwarding a sync across servers needs. Without a target,
`devopsy --ssh-keys generate [<name>]` makes an ed25519 pair in `~/.ssh`
(`devopsy-ci` by default), nothing sent anywhere. For CI: generate, `add`
the `.pub` on each server with `--restricted`, the private key as a
protected file variable, and in the job `eval "$(ssh-agent)"; ssh-add
"$DEVOPSY_SSH_KEY"` before `devopsy @prod --release`. Rotate by generating
another, adding it, then removing the old one. (devopsy-server's `ci-key`
step makes one on the server itself, for a setup without a laptop.)

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
- **sync:** hooks into a sync (Parent environments and syncs); each
  action runs only when its file is there, besides devopsy's own handling
  of the `devopsy.sync` labels.
  - `flush <service>` runs on each side, once per running service labeled
    `live` or `pause`, before it is paused or copied (child: before its
    services are paused; parent: before its own are paused and copied).
    Not for `stop`, `skip` or `custom`. A failure aborts the sync.
  - `prepare` runs on the child's server, in its current release under its
    release lock, before the copy, then devopsy flushes, pauses and stops
    the child's labeled services. A failure aborts the sync (`finish
    abort` runs).
  - `push` runs on the parent's server, in its current release, after
    devopsy has copied the services labeled `live`, `pause` and `stop`,
    when any is labeled `custom` (then it is required; without one it is
    not run). It gets `DEVOPSY_SYNC_CUSTOM` (those services, space
    separated), `DEVOPSY_SYNC_TO` (the child's server as
    `user@hostname`, empty on the same server), `DEVOPSY_SYNC_SSH_PORT`,
    `DEVOPSY_SYNC_TO_BASE` and `DEVOPSY_SYNC_TO_DIR` (the child's
    directory and its current `.devopsy/`), `DEVOPSY_SYNC_TO_PROJECT`,
    `DEVOPSY_SYNC_TO_TARGET`, `DEVOPSY_SYNC_FROM_TARGET` and
    `DEVOPSY_SYNC_ARGS` (what followed `--sync`'s `--`, its own to use).
    Two helpers do the transport: `devopsy --sync-rsync <entry>... [--
    <rsync args>]` copies entries of `mnt` as the generic path does, and
    `devopsy --sync-exec <devopsy args>` runs devopsy in the child's
    current release, as its project and with a clean environment (none of
    the parent's variables), stdin and stdout passed through. Exit 0 lets
    the child go on. On a child's first release none of its services has
    run yet: a `push` that needs one starts it first (`devopsy
    --sync-exec up -d --wait database`).
  - `finish go|abort` runs on the child's server after the copy (`go`) or
    a failure (`abort`), after devopsy has started, waited for and resumed
    the labeled services it paused or stopped. After `go` it is where a
    copy of production is sanitized (emails and passwords scrubbed, a
    login set for a child that had none: devopsy-template-drupal11's);
    then the release's `run` step runs. On a child's first release
    nothing of it exists yet, so devopsy creates the labeled services
    first (`up -d --wait`), as the run step's `up` would a moment later:
    the hook finds them running either way.
  - `flush` (on the child), `prepare` and `finish` get
    `DEVOPSY_SYNC_FROM_TARGET` and `DEVOPSY_SYNC_ARGS`.

## License

GPL-3.0. See [LICENSE](LICENSE).
