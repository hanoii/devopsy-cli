# Working locally

A project is a `.devopsy/` directory beside its code, and every devopsy command runs in it:

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

## A release in place

`devopsy --release` without a target is a release in place, in the
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

## Starting over

`devopsy --destroy` without a target starts over, after asking for
the project's name (`--yes` skips it): the `destroy:` step under
`defaults:` if there is one, then the containers, volumes and built
images down and `.devopsy/mnt` emptied, as a server's `--destroy` does.
The checkout stays.

## Words

A word is a project command if `.devopsy/commands/` has it, else a compose
command (devopsy asks compose which exist), else an error. Arguments starting
with a flag go to compose unchecked (`devopsy --profile x up`), except a
flag alone: compose's flags never stand alone, so one devopsy does not know
is an error, with the nearest flag of its own (`devopsy --ssh-host`: did
you mean `--shell-host`?), locally and with a target.

## Project commands

Any executable in `.devopsy/commands/` is a project command, run with the
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

## Variables

`.env` is read with compose's own parser; your environment wins over it,
as in compose.

Variables devopsy adds, for compose files and commands:
`COMPOSE_PROJECT_NAME` (always set, see below), `DEVOPSY_PROJECT_DIR`, and
whatever the project's `env` capability computes (see [the env capability](capabilities.md#env)), which
is how projects derive values like their hosts.

To run plain compose with the same values: `devopsy --env --show > /tmp/env &&
docker compose -f .devopsy/compose.yaml --env-file /tmp/env config` (a file:
compose reads `--env-file` more than once).

## Project name

devopsy always sets `COMPOSE_PROJECT_NAME`: the config's
`project:`, else compose's top-level `name:`, else the directory containing
`.devopsy/`, normalized; your own `COMPOSE_PROJECT_NAME` wins. On servers
it is always `<project>[-<instance>]-<environment>` ([Targets](config.md#where-an-environment-lives)), whatever
`name:` or your environment says: nothing of your local environment goes
to a server.

## Shells

`--shell` opens bash (or sh) in the service named, else the one labeled
`devopsy.shell=true`, else the only one running, as `devopsy.shell.user`
when set (for images whose entrypoint drops from root). Options go to
`docker compose exec` (`--user root` wins over the label); after `--`, a
command runs instead. A project needing more implements the `shell`
capability.

## Context hashes

`--context-hash [service]` prints a hash of what the service's image is
built from at `HEAD` (git-tracked context minus the dockerignore Docker
uses, the Dockerfile, the `build:` section, its base images), to reuse an
image across commits that do not change it: `tag=ctx-$(devopsy
--context-hash app)`. Each base image (a `FROM` that is no stage and not
`scratch`, its global `ARG`s expanded) counts by the digest its registry
gives now (`docker buildx imagetools inspect`, nothing pulled; a base
pinned by digest asks nothing), so a patched base makes a new hash: build
it with `--pull`, or Docker's cache keeps the old base. A registry that
does not answer fails the command, so the caller builds. Downloads during
the build are not covered. devopsy builds nothing itself.

## Output

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

## Secrets

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

## Verbose output and debugging

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

## Colors

Help, `--debug`, devopsy's messages and the lines a server sends are in
color on a terminal, unless `NO_COLOR=1`; `FORCE_COLOR=1` forces them
through a pipe or in CI (`0` or `false` do not), and `NO_COLOR` wins.
Releases and syncs run their sessions without a terminal, so their steps,
builds and run step get one on the server (`script`, from util-linux)
sized like yours, and a build's live progress shows as it would locally.
Logs are saved without any of it. Variables named `DEVOPSY_INTERNAL_*` are
how one devopsy tells another these things: never set or read them, they
change in any release.
