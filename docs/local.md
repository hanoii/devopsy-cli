# Working locally

A project is a `.devopsy/` directory beside its code. Every devopsy
command finds it from the directory you run it in:

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

## Words

A word after `devopsy` is a project command when `.devopsy/commands/`
has it, else a compose command (devopsy asks compose which ones exist),
else an error.

Arguments that start with a flag go to compose unchecked (`devopsy
--profile x up`). A flag on its own is different. Compose has none that
stand alone, so a lone flag devopsy does not know is an error, with
the nearest of its own flags as a hint (`devopsy --ssh-host`: did you
mean `--shell-host`?). This holds locally and with a target.

## Project commands

Any executable in `.devopsy/commands/` is a project command. It runs with
the project's `.env` loaded, `DEVOPSY_PROJECT_DIR` (the `.devopsy/` path)
and `DEVOPSY_COMMAND_NAME` set. A `## Description:` line near the top
shows in help, and two more lines say where it runs:

- `## Run: local` runs it only on your machine (for example a build that
  pushes an image); on a server it fails. `## Run: target` runs it only
  on the server, through `devopsy @<target>` (for example a deploy);
  locally it fails. Without the line it runs where it is called: locally,
  or on the target when one is given.
- `## Needs: target` or `## Needs: server`, with `## Run: local`, makes a
  local command that takes an address: `devopsy @prod <command>`, or
  `devopsy @vm1 <command>` for a server (any target gives its server).
  The command gets the address as typed in `DEVOPSY_COMMAND_TARGET`, to
  call devopsy with, and nothing else of the target's. As a release's
  `local:` step it gets the release's address.

```sh
#!/bin/sh
## Description: Copy a target's database locally
## Run: local
## Needs: target
devopsy @"$DEVOPSY_COMMAND_TARGET" drush sql-dump > .devopsy/mnt/dump.sql
devopsy drush sql-cli < .devopsy/mnt/dump.sql
```

Help shows where each command runs, as it shows a flag's scope (`(runs
locally)` after a local command that takes an address), and completion
offers each command only where it runs. A release refuses, before
anything changes, a step whose command runs in the wrong place (`local:`
steps run locally, the rest on the server). devopsy detects a release on
a server by its `devopsy-target.env`, so a `target` command does not run
in a plain directory on a server either.

A command can wrap the compose command of the same name: a `commands/up`
that calls `devopsy up` reaches compose, not itself.

## Variables

devopsy reads `.env` with compose's own parser, and your environment wins
over it, as in compose. It adds, for the compose files and commands:

- `COMPOSE_PROJECT_NAME`, always (Project name, below);
- `DEVOPSY_PROJECT_DIR`;
- `DEVOPSY_UID` and `DEVOPSY_PROJECT_UID`, the uids a server would give
  ([Variables on a server](config.md#variables-on-a-server)), here a hash
  of the local project name and of `config.yaml`'s `project:`. Yours win
  (`DEVOPSY_UID=$(id -u)` in `.devopsy/.env`): on Linux for a bind mount
  you edit that a container writes, or for rootless Docker or Podman,
  which run no uid above 65535;
- whatever the project's [env capability](capabilities.md#env) computes,
  which is how projects derive values like their hosts.

To run plain compose with the same values, write them to a file (compose
reads `--env-file` more than once, so a pipe does not work):

```sh
devopsy --env --show > /tmp/env && docker compose -f .devopsy/compose.yaml --env-file /tmp/env config
```

## Project name

devopsy always sets `COMPOSE_PROJECT_NAME`. Locally it is the config's
`project:`, else compose's top-level `name:`, else the name of the
directory holding `.devopsy/`, normalized; your own `COMPOSE_PROJECT_NAME`
wins over all three. On a server it is always the environment's compose
name ([Where an environment lives](config.md#where-an-environment-lives)),
regardless of `name:` or your environment, since nothing of your local
environment goes to a server.

## A release in place

`devopsy --release` without a target runs a release in the local project
the way a server runs one:

1. the release steps under `defaults:` in `config.yaml` (an error without
   them);
2. `prepare`;
3. the variables check;
4. the images, always as in build mode: services with a `build:` are built
   with `--pull`, the others pulled (a local image-mode project has no tag
   of its own in a registry);
5. `run`.

There is no upload and no switch, and a failure stops the release with
nothing to go back to. There are no `before` or `after` steps either,
since those are the operator's side of a deploy (pushing an image,
announcing it). A step whose command runs only on a target (`## Run:
target`) refuses before anything runs. Since step 4 gets the images, a
project's `deploy` only starts what is there, here as on a server.

## Starting over

`devopsy --destroy` without a target starts the local project over, as a
server's `--destroy` does. It asks for the project's name first (`--yes`
skips it), then runs the `destroy:` step under `defaults:` if there is
one, takes the containers, volumes and built images down, and empties
`.devopsy/mnt`. The checkout stays.

## Shells

`--shell` opens bash (or sh) in a container: of the service named, else
the one labeled `devopsy.shell=true`, else the only one running. It runs
as the user in `devopsy.shell.user` when that label is set (for images
whose entrypoint drops from root). Options go to `docker compose exec`,
and `--user root` wins over the label. After `--`, a command runs
instead of the shell. A project that needs more implements the
[shell capability](capabilities.md#shell).

## Context hashes

`--context-hash [service]` prints a hash of what the service's image is
built from at `HEAD`, to reuse an image across commits that do not
change it (`tag=ctx-$(devopsy --context-hash app)`). It covers:

- the build context as git tracks it, minus what Docker's dockerignore
  leaves out;
- the Dockerfile and the `build:` section;
- each base image (a `FROM` that is not a stage or `scratch`, its global
  `ARG`s expanded), by the digest its registry gives now (`docker buildx
  imagetools inspect`, nothing pulled; a base pinned by digest needs no
  request).

A patched base makes a new hash; build it with `--pull`, or Docker's
cache keeps the old base. A registry that does not answer fails
the command, so the caller builds. Downloads during the build are not
covered. devopsy builds nothing itself.

## Output

What a command produces goes to stdout: compose's output, `--env`,
`--vars`, lists, `--export`'s data. Everything devopsy says goes to
stderr, each line starting with `devopsy:`, which sets it apart from what
compose, Docker and your commands print around it, locally and from a
server:

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
fails, `debug:` (`-v`) and `trace:` (`-vv`) for what it runs. Details
follow on `| ` lines, then commands to copy after `$ `: the fixes under
`to fix it:`, numbered when there are several ways out, then hints,
worth knowing.

Questions start with `devopsy:` too, and are asked only at a terminal.
Without one, a command that would ask fails and says to add `--yes`.

## Secrets

Secrets are opt in. `secrets:` in `config.yaml` names the variables whose
values are secret (`[DB_PASSWORD, DATABASE_URL, "*_TOKEN"]`, `*` matching
any characters), wherever they are set: `.env`, `shared/.env` on a server,
the caller's environment. Name every secret the project uses, including
the ones it generates. Nothing else is masked, and nothing is guessed
from names.

- `--env`, `--vars` and `--debug env` print those values as `***`;
  `--show` prints them. On a server the release's `config.yaml` decides,
  and `--vars` also takes your checkout's, which may be newer.
- devopsy's own messages (what it runs, at `-v` and `-vv`) mask those
  values wherever they appear, when 8 characters or longer. Only the
  messages are masked, and what commands print is left as it is. Masking
  is a safety net: pass secrets through the environment, not as arguments
  (`devopsy exec -T db sh -c 'MYSQL_PWD="$MARIADB_ROOT_PASSWORD"
  mariadb'`).
- A variable named like a secret (`PASSWORD`, `TOKEN`, `KEY`, `SALT`,
  `AUTH`...) and not listed stops `--release` (for the compose files'
  variables and the target's `env:`) and `--vars set`: devopsy asks
  first, and fails without a terminal or with `--yes`. List it in
  `secrets:`, or in `public:` when it is not a secret (`public:
  [SORT_KEY]`). These names only trigger the question; masking comes
  from `secrets:` alone.

## Verbose output and debugging

`-v` (or `DEVOPSY_VERBOSE=1`) also prints the files loaded, what devopsy
runs (`running 'docker compose ...'`) and, for targets, each SSH session.
`-vv` (`DEVOPSY_VERBOSE=2`) also prints the script each session runs.
Both carry over to nested devopsy and to devopsy on the server. A script
in a command's arguments (`devopsy run --rm tool sh -c '<several
lines>'`) shows as `<script: 12 lines>` in what devopsy says it runs, and
whole at `-vv`.

```sh
devopsy --debug                       # versions, the project, environments, aliases, labels
devopsy --debug environments [name or target] [--yaml]   # computed, with each value's origin
devopsy --debug schema [--user]       # every config key, commented
devopsy --debug env [--show] [VAR...]  # each variable: what sets it, what that overrides
devopsy --debug labels | imports | capabilities
devopsy @vm1:prod --debug             # the same, as the server sees it
devopsy @vm1:prod --debug env         # the release's variables, traced there
```

`--debug env` traces the variables `--env` shows, or the ones named: the
definition in use first, then every one it overrides, with its value, in
order of precedence. It names each source: the caller's environment,
each `.env` file (on a server with what it links to, like `shared/.env`),
`devopsy-target.env` and what the release imported into it, what devopsy
sets itself, and what the env capability computed. Secrets are masked as
above unless `--show`. For a target, the caller's environment is the
server's SSH session plus the target's `COMPOSE_PROJECT_NAME`, which
devopsy sends.

## Colors

Help, `--debug`, devopsy's messages and the lines a server sends are in
color on a terminal, unless `NO_COLOR=1`. `FORCE_COLOR=1` forces color
through a pipe or in CI (`0` and `false` do not), and `NO_COLOR` wins
over it.

Releases and syncs run their SSH sessions without a terminal, so devopsy
gives their steps, builds and run step one on the server (`script`, from
util-linux), sized like yours: a build's live progress shows as it would
locally. Logs are saved without the colors, and a progress display only
as its final state.

Variables named `DEVOPSY_INTERNAL_*` pass these settings from one devopsy
to another. Never set or read them, since any release may change them.
