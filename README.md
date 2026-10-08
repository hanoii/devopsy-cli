# devopsy-cli

A small wrapper around `docker compose` for projects that keep their
deployment in a `.devopsy/` directory. Run `devopsy` from anywhere inside the
project and it finds `.devopsy/`, loads its `.env` and runs compose with the
right files. Other words are `docker compose` commands.

It is a single static binary for Linux and macOS (amd64 and arm64), with no
dependencies besides Docker and the Compose plugin.

## What this is not

- **Not a platform.** No control plane, dashboard, agent or daemon on
  servers; nothing listens for webhooks. A server is Docker, SSH and this
  binary.
- **Not an orchestrator.** No Kubernetes, no Swarm, no scheduling across
  hosts: an environment is one compose project on one server.
- **Not a replacement for compose.** It never generates or rewrites compose
  files; plain `docker compose` sees exactly what devopsy runs.
- **Not a local development tool like DDEV.** No per-framework setup, no
  router or database snapshots of its own: it runs the compose project you
  write, the same way locally and on servers.
- **Not a build system, and not tied to a proxy.** Images are built however
  you like; Traefik is one template, not part of devopsy.

## What this is for

Hosting compose projects on plain servers you own, from your machine or CI:

- **Releases over SSH:** each one a complete directory, switched in at the
  end and switched back when its steps fail; `--rollback` to the previous
  one. Data and secrets live outside releases.
- **Several environments per server:** `prod`, `staging`, `pr-123`, each its
  own compose project, data and URL; one codebase installed several times
  (instances) for multi-site setups.
- **The same commands locally and remotely:** `devopsy up`, `devopsy @prod
  logs`, project commands in `.devopsy/commands/`, a shell in any container.
- **Small teams and agencies** running many sites on a few cheap servers,
  with nothing to operate besides the servers themselves.

To start: install it (below), then take a template, like
[devopsy-template-whoami](https://github.com/hanoii/devopsy-template-whoami)
for the smallest project and
[devopsy-template-traefik](https://github.com/hanoii/devopsy-template-traefik)
for the server's proxy, or add `.devopsy/compose.yaml` and
`.devopsy/config.yaml` to your own project (Remote targets, below).

## Opinions

devopsy decides a few things for you, and the rest of this README follows
from them:

- **Compose plus environment variables.** devopsy never generates or
  rewrites compose files. It only adds variables, all visible with
  `devopsy --env`, so plain `docker compose` sees exactly what it sees.
  Configuration is environment variables, with a fixed precedence: caller,
  `.env` (on servers `shared/.env`), the target's `env`. Nothing
  server-wide: the server's wildcard domain comes from its proxy at each
  release, and each target can override it.
- **An environment is a server path.** Each target is a directory on a
  server, so its own compose project, data and wildcard URL. There is no
  branch concept: a branch is only what you release into a target. Hence
  released projects' compose files have no `name:`; devopsy names the
  project after the path.
- **Push over SSH, never pull.** A release is uploaded from your machine or
  CI. The server needs no access to the repository and nothing listens for
  webhooks. Locally devopsy only needs `ssh`; on the server, `devopsy`,
  `tar` and `flock`.
- **Releases are complete directories.** Each one is a full copy, and
  `current` switches to it at the end, then back if the release steps fail.
  Secrets and data live in `shared/`, outside releases, so rollbacks keep
  them. Remote commands run through `current`.
- **Steps are explicit.** `--release` and `--rollback` run the steps each target
  names in `config.yaml`; without them they refuse, rather than upload a
  release nothing applies.
- **Project behavior lives in the project.** Anything that depends on a
  project's services, users or data is a command in its
  `.devopsy/commands/`, not a devopsy feature. devopsy does not build or
  push images either; it only helps decide when to (`--context-hash`).
- **Traefik only where it helps.** Releases and commands do not depend on
  a proxy. `DEVOPSY_HOST_RULE` is a Traefik rule as a plain variable, which
  projects are free to ignore. devopsy knows no proxy: projects that deliver
  to one speak its language (labels, its network), the proxy owns routing
  and certificates, with its own commands
  ([devopsy-template-traefik](https://github.com/hanoii/devopsy-template-traefik)), and the
  facts it shares reach projects through generic labels (roles, exports,
  imports).
- **Capabilities are interfaces.** devopsy defines few (`shell`); a project
  implements one by shipping its scripts, and devopsy calls them. People
  never do, so they are never words.
- **Words are yours.** devopsy's own features are flags or `@target`, so a
  word is always a project command, then a compose command.
- **Secrets stay out of sight.** Values from `.env` and secret-named
  variables are masked in everything devopsy prints, and `--vars` sends
  values over SSH's stdin, never as arguments.

Other practices, like container UIDs, certificate resolvers or where data
lives, are recommendations: see the
[devopsy workspace](https://github.com/hanoii/devopsy) and its templates.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/hanoii/devopsy-cli/main/install.sh | sh
```

It downloads the latest release for your platform, checks its checksum and
installs it to `/usr/local/bin` when it can, otherwise `~/.local/bin`. Set
`DEVOPSY_VERSION` to install a specific release, like `v0.1.0`, and
`DEVOPSY_INSTALL_DIR` to choose the directory.

To upgrade, run `devopsy --upgrade` (`sudo devopsy --upgrade` when root owns
the binary, as on servers), or `devopsy --upgrade v0.9.0` for a specific
release, older ones included. It checks the checksum like `install.sh` and
replaces the binary in one step. At a terminal, devopsy checks for a new
release once a day, in the background so no command waits, and says when
there is one from then on; never in CI, and `DEVOPSY_NO_UPDATE_CHECK=1`
turns it off. Builds from
source neither upgrade nor check.

Releases are on the [releases page](https://github.com/hanoii/devopsy-cli/releases).
To build from source: `go build -o devopsy ./cmd/devopsy`.

To always run a checkout's latest code, uncommitted changes included, symlink
`scripts/devopsy` into your `PATH` instead. It rebuilds on every run, which
Go's build cache makes instant when nothing changed, and needs Go:

```sh
ln -s "$PWD/scripts/devopsy" ~/.local/bin/devopsy
devopsy --version   # like v0.5.0-2-g1a2b3c4-dirty
```

### Shell completion

`devopsy --completion <shell>` prints a completion script for bash, zsh or
fish. Load it from your shell's startup file:

```sh
source <(devopsy --completion bash)   # ~/.bashrc (bash 4 or later)
source <(devopsy --completion zsh)    # ~/.zshrc, after compinit
```

In fish, add a one-line completion file. Fish loads it at the first Tab after
`devopsy` in each shell, so shells start no slower, and each shell gets the
installed devopsy's script:

```fish
echo 'command -q devopsy; and devopsy --completion fish | source' > ~/.config/fish/completions/devopsy.fish
```

It completes targets (`@prod`, the project's environments, and aliases
with what they stand for), the project's commands with their descriptions, devopsy's flags,
the commands after `@<target>` (`--release`, `--vars set` with the keys of
the project's `.env`...) and, through docker compose's own completion,
compose's commands, flags and the project's services. After a project
command, it completes file names. On a target, services come from the local
compose files: completion never connects to servers.

Targets come first, environments before aliases, then project
commands, devopsy's own commands and docker compose's. zsh lists each in its
own group (unless your `group-name` and `format` styles say otherwise), fish
describes project commands as `project: ...` and compose's as `docker
compose: ...`, and bash (4.4 or later) keeps the order but shows names only.

The scripts are small and ask devopsy (`devopsy --complete <words>`) at every
Tab, so candidates follow the installed devopsy right away, even in open
shells. A new release can still change how a script shows them: loaded as
above, new shells get it, and `devopsy --completion fish | source` (or the
`source` line for bash and zsh) updates an open one. A script saved to a file
(`devopsy --completion fish > ...`) only changes when you save it again.

## Project layout

```
my-project/
└── .devopsy/
    ├── compose.yaml            # required
    ├── compose.override.yaml   # optional, usually not committed
    ├── .env                    # optional, usually not committed
    └── commands/               # optional, executable scripts
        └── deploy
```

## Usage

```sh
devopsy up -d          # docker compose -f .devopsy/compose.yaml [-f .devopsy/compose.override.yaml] up -d
devopsy logs -f web
devopsy deploy         # runs .devopsy/commands/deploy if it exists
devopsy                # help: built-ins, the project's commands, and the rest
devopsy --version      # devopsy's, docker's and docker compose's versions
devopsy --env          # the variables devopsy loads and computes
devopsy --shell [service]   # a shell in a container
devopsy --context-hash [service]   # a hash of what the service's image is built from
devopsy --debug        # what devopsy sees: versions, project, targets, capabilities, labels
devopsy --upgrade      # replace devopsy with the latest release
devopsy --completion fish   # a shell completion script (bash, zsh, fish)
devopsy -v deploy      # --verbose: also what devopsy found and runs
```

devopsy's own features are flags (`--help`, `--version`, `--env`,
`--shell`, `--context-hash`, `--upgrade`, `--verbose`, `--completion`, and
after a target `--release` and the rest) or start with `@` (targets), so
they never clash with words: a word is a project command if
`.devopsy/commands/` has it, else a docker compose command, else an error
naming the project's commands. `devopsy version` is `docker compose
version`. devopsy learns compose's commands from compose itself and caches
them, so a new compose's new commands just work. Arguments that start with
a flag (`devopsy --profile tools up`) go to compose unchecked, and `--` is
the escape hatch: `devopsy -- <args>` (or `devopsy @prod -- <args>`) is
`docker compose <args>` with the project's files, even when a project
command has that name or devopsy does not know the word.

### Debugging

`devopsy --debug` summarizes what devopsy sees: its version and path,
docker's and compose's, the project and its files, commands,
environments, aliases, implemented capabilities and `devopsy.*` labels.
Topics go deeper:

```sh
devopsy --debug environments [name or address]   # each as computed, where each value comes from
devopsy --debug environments --yaml   # the same as plain YAML: defaults merged, servers resolved
devopsy --debug capabilities     # what devopsy calls, each action's contract, what this project implements
devopsy --debug labels           # the labels devopsy reads, the project's, roles on this host
devopsy --debug imports          # every running project's imports against its release (STALE ones)
devopsy --debug schema           # every key of .devopsy/config.yaml, commented (--user: the user config)
devopsy @prod --debug            # the same, as the server sees it
```

`environments` shows every value with its origin: the config file,
`defaults in` a file, the address, a variable like `DEVOPSY_SERVER`, or
devopsy's default; any address resolves (`devopsy --debug environments
vm1:b/prod`).

Help and `--debug` are in color on a terminal; `NO_COLOR=1` turns it off,
as it does for devopsy's other messages.

### Output and secrets

Before running docker compose, devopsy prints the command (`Running 'docker
compose ...'...`) to stderr. Secrets in it are masked as `***`: every value
from the project's `.env` (on servers `shared/.env`), even when the caller's
environment overrides it, and every variable named like a secret (`PASSWORD`,
`SECRET`, `TOKEN`, `SALT`, `AUTH`, `KEY`...), as CI sets them. Values shorter
than 8 characters are left alone. Only the message is masked: the command
gets its arguments unchanged.

Masking is a safety net, not a reason to pass secrets as arguments: they still
show in `ps` and shell history. Expand them inside the container instead,
where compose already set them:

```sh
devopsy exec -T database sh -c 'MYSQL_PWD="$MARIADB_ROOT_PASSWORD" mariadb -uroot'
```

`--verbose` (or `-v`, before anything else) also prints the project
directory, the env files loaded and the project command run, and for
`@target`, each SSH command with the script it runs, secrets masked the same
way. `DEVOPSY_VERBOSE=1` (or `true`) does the same; the flag sets it, so
devopsy called from project commands and devopsy on the server are verbose
too.

### Custom commands

Any executable file in `.devopsy/commands/` becomes a command. A
`## Description:` line near its top is shown by `devopsy`, as in ddev:

```sh
#!/bin/sh
## Description: Pull images and roll out
```

 It runs with
the project's `.env` loaded and these variables set:

- `DEVOPSY_PROJECT_DIR`: absolute path to `.devopsy/`.
- `DEVOPSY_CLI_COMMAND`: the command's name.

A command can override a compose command and still call it. A
`commands/up` that runs `devopsy up "$@"` reaches `docker compose up`, not
itself.

### .env

`.devopsy/.env` is the file compose reads for variable substitution, and
devopsy also loads it for custom commands, with compose's own parser, so both
see the same values. Variables already set in your environment win over
`.env`, as they do in compose.

### Variables for compose files

devopsy also sets, for compose files and custom commands:

- `DEVOPSY_PROJECT_NAME`: the compose project name.
- `DEVOPSY_WILDCARD_HOST`: `<project>.<DEVOPSY_WILDCARD_DOMAIN>`. The
  wildcard domain is the server's, like `vm1.example.com`, with a wildcard
  DNS record (and usually a wildcard certificate) pointing at it, never a
  site of its own: when it is there, each environment gets an automatic
  subdomain of it, next to its own `DEVOPSY_DOMAINS`. A project gets it from
  the server's proxy by importing it (see Roles, exports and imports): each
  release writes it into its `target.env`, so nothing needs setting. A target
  overrides it with its own value, or turns the automatic URL off with an
  empty one: `devopsy @prod --vars set --show DEVOPSY_WILDCARD_DOMAIN` (empty:
  just Enter), applied by the next `up` or `reload`; `--vars unset` goes
  back to the proxy's. Without one, `<project>.localhost` locally, and no
  wildcard host in a release: the environment only answers on its
  `DEVOPSY_DOMAINS`. Set `DEVOPSY_WILDCARD_HOST` yourself to override the
  whole host.
- `DEVOPSY_HOST_RULE`: a Traefik rule for the wildcard host plus
  `DEVOPSY_DOMAINS`, a space or comma separated list you set per environment,
  usually in its `.env`. For example
  ``Host(`shop.vm1.example.com`) || Host(`example.org`)``. Unset when there
  are no hosts at all, so a label's own default applies.

A router label then needs no per-environment hosts. Defaults keep the file
usable with plain `docker compose`:

```yaml
labels:
  - traefik.enable=true
  - traefik.http.routers.${DEVOPSY_PROJECT_NAME:-app}.rule=${DEVOPSY_HOST_RULE:-HostRegexp(`^app\.localhost$`)}
```

The fallback is a `HostRegexp`: Traefik requests no certificate for it, so
an environment without hosts (`DEVOPSY_HOST_RULE` unset) logs no ACME
errors, while plain compose still answers on `app.localhost`.

devopsy only adds environment variables; it never changes compose files.
`devopsy --env` prints what it loads and computes, in `.env` format, to
run plain compose with exactly the same values:

```sh
devopsy --env > /tmp/devopsy.env
docker compose -f .devopsy/compose.yaml --env-file /tmp/devopsy.env config
```

Use a file: compose reads `--env-file` more than once, so `<(devopsy
--env)` does not work.

### Context hash

`devopsy --context-hash [service]` prints a hash of what the service's image
is built from at the git commit `HEAD`, like Platform's tree id: build an
image only when the hash is new, and reuse it across commits that only touch
other files (`.devopsy/`, `.ddev/`...). The service defaults to the only one
with a `build:` section. It covers:

- the files git tracks in the build context, minus what the dockerignore
  Docker uses leaves out (`<Dockerfile>.dockerignore` next to the
  Dockerfile, else `.dockerignore` in the context), matched with Docker's own
  rules (moby's patternmatcher);
- the Dockerfile, even when it is ignored or outside the context;
- the `build:` section, interpolated, so build args count by value.

Not covered: base images, images in `COPY --from` and anything the build
downloads: rebuild with `--pull` for those. Neither are uncommitted or
untracked files: build from a git export of `HEAD` (`git archive`), or the
hash does not describe the image. `additional_contexts` and a `build:` in
`compose.override.yaml` are refused. devopsy builds nothing itself; a
project's own command decides, for example:

```sh
tag=ctx-$(devopsy --context-hash app)
docker buildx imagetools inspect "$image:$tag" >/dev/null 2>&1 || build_and_push
```

### Project name

Set a top-level `name:` in `compose.yaml`. Without one, devopsy uses the name
of the directory containing `.devopsy/`, normalized as compose does
(`My Proj` becomes `myproj`). Otherwise compose would call every project
`devopsy`. `COMPOSE_PROJECT_NAME` still overrides both.

## Remote targets

`devopsy @<target> ...` runs devopsy on a server over SSH, from your machine
or from CI. A target is an address: which server, which instance of the
project, which environment.

```
@[<server>:][<instance>/]<environment>

@prod                         environment; server (and instance) from variables or config
@vm1:prod                     on vm1: any SSH destination or ~/.ssh/config alias, no ":"
@vm1:confcatsdemo/prod        an instance of the project, on vm1
@vm1:/prod                    explicitly no instance, whatever DEVOPSY_INSTANCE says
@devopsy@203.0.113.10:prod    a raw SSH destination: the server ends at the first ":"
```

The project's devopsy config, `.devopsy/config.yaml`, names the project and
defines its environments:

```yaml
project: shop                   # its name on servers: required for @target
defaults:                       # what every environment takes unless it sets its own
  mode: image                   # build (default) or image
  release: {remote: deploy}     # what --release runs (required for it)
  rollback: {remote: deploy}    # what --rollback runs (required for it)
environments:
  prod:
    env:                        # per-environment settings, not secrets
      DEVOPSY_DOMAINS: example.org www.example.org
  staging: {}
  "pr-*":                       # any matching name: @pr-123, @pr-feature
    releases: {keep: 1}
```

`devopsy --init` writes a starting `config.yaml` (it asks for the project's
name; it never touches an existing one), and `devopsy --debug schema` prints
every key, commented. Commit `config.yaml`: CI deploys from it. It holds no secrets and no
servers. `.devopsy/config.local.yaml` (gitignore it) is read over it for
one machine: top-level keys replace, `defaults` merge, an environment
replaces the same-named one; it is never uploaded.

What the address leaves out comes from variables, in your environment or the
project's `.devopsy/.env` (CI sets them in its environment):

```sh
DEVOPSY_SERVER=vm1                       # the server
DEVOPSY_SERVER_STAGING=devopsy@203.0.113.20   # one environment's (upper case, other characters as _)
DEVOPSY_SERVER_PR=pr-box                 # a pattern's: pr-* without the *
DEVOPSY_INSTANCE=confcatsdemo            # the instance
```

The most specific wins: the environment's (`DEVOPSY_SERVER_PR_12`), its
pattern's, then `DEVOPSY_SERVER`. A project's config never names servers
(nor instances): where things run is a deployment decision, made in the
address, these variables, or an alias. No server anywhere is an error
naming the ways to give one.

On the server, each environment lives in `<project>/[<instance>/]<environment>`
under the server's release root (below): `shop/prod`, `shop/pr-123`. Its
compose project, and so its containers, volumes and wildcard URL, is
`<project>[-<instance>]-<environment>` (`shop-prod`): from project, instance
and environment only, never the server. An environment's `path:` moves its
directory (relative to the root, or absolute), never its name.

`defaults:` gives each environment its `mode`, `release`, `rollback`,
`releases` and `env`, unless it sets its own: `env` merges key by key (the
environment's value wins), the rest replaces whole. In `env`, `""` sets an
empty value and `~` (null) removes a default. `server` and `path` stay each
environment's.

```yaml
defaults:
  env:
    CERTRESOLVER: acmedns
environments:
  demo:
    env:
      DEVOPSY_WILDCARD_DOMAIN: ""   # set and empty: no wildcard URL
      CERTRESOLVER: ~               # not set: the label's own default
```

Environment names with `*` are patterns, one or more name characters:
`@pr-123` uses an environment named `pr-123`, else the matching pattern
with the most literal characters (two equally specific ones are an error);
`"*"` allows any name. The name is the concrete one everywhere: path,
compose name, `DEVOPSY_ENVIRONMENT`. Pull request environments are then a
CI job: `devopsy @pr-$PR --release` when one opens or changes, `devopsy
@pr-$PR --destroy --yes` when it closes.

### Instances

One project can be installed several times on a server, each install an
instance with its own environments: a second copy of a site, or one
codebase serving several sites, each maybe on another server. Instances
are never listed in the project: any name (lowercase letters, digits, `-`)
works, from the address or `DEVOPSY_INSTANCE`.

```sh
devopsy @vm1:b/prod --release                     # shop/b/prod, compose name shop-b-prod
DEVOPSY_INSTANCE=b devopsy @vm1:prod --release    # the same
devopsy @vm1:prod --instances                     # the instances on vm1
```

The first release of an instance the server does not have yet asks before
creating it, as a typo would otherwise create a new site; `--release
--yes` skips the question (CI). `instances: required` in `config.yaml`
makes every remote command name one, so a multi-site project never
releases as itself by accident; `instances: none` refuses them. Without
either, instances are optional. An environment with its own `path:` takes
none.

### Variables on servers

A target's `env` is written into each release as `.devopsy/target.env`. The
server adds, nearest first:

- the environment's `shared/.env`, linked into every release (secrets,
  overrides): `devopsy @prod --vars set KEY`;
- the instance's `.env`, `<project>/<instance>/.env`, shared by its
  environments: `--vars --instance`;
- the project's `.env`, `<project>/.env`, shared by all its environments on
  that server: `--vars --project`.

Then `target.env`. So a release applies however devopsy runs on the server,
a rollback brings back that release's `target.env`, and edits to a `.env`
apply without a release (to running containers when recreated). Each
release's `target.env` also has `COMPOSE_PROJECT_NAME` (unless
`compose.yaml` has a `name:`), `DEVOPSY_PROJECT`, `DEVOPSY_INSTANCE`,
`DEVOPSY_ENVIRONMENT` (the target) and, from a git checkout,
`DEVOPSY_RELEASE_COMMIT` (see image tags below).

### Release settings of a server

Where releases live and how many stay is the server's decision, in the
deploy user's own `~/.config/devopsy/config.yaml` there, read by devopsy on
the server, so your laptop and CI always agree:

```yaml
# on the server: ~/.config/devopsy/config.yaml
releases:
  root: /srv          # default: the deploy user's home
  keep: 2             # default for its projects; default 5
  max_keep: 5         # no project keeps more; default 5
```

A project sets its own `keep` (`releases: {keep: 3}` in `config.yaml`, or
per target), at most the server's `max_keep`. A project's config cannot set
`root` or `max_keep`: a repository never decides a server's layout. The
same file on your laptop only matters for releases landing on it, none
today. `devopsy --debug` shows this machine's settings; `devopsy @prod
--debug` the server's.

`devopsy @pr-123 --destroy` removes an environment: compose down with its
volumes, then its directory, data and `shared/.env` included. It asks for
the target's name, unless `--yes`.

### Aliases and plain directories

`~/.config/devopsy/config.yaml` (or `$DEVOPSY_HOME/config.yaml`) holds
aliases: shortcuts for targets you use from any directory. `to:` is an
address; `source:` is the local checkout of its project, which gives the
project's name, environments and release steps, and the only directory it
releases from (anywhere else, a release would upload whatever project you
stand in). `~/` is your home directory. Without `source:`, `project:` names
the project, for running commands only.

```yaml
# ~/.config/devopsy/config.yaml
aliases:
  vm1-traefik:  {source: ~/src/devopsy-template-traefik, to: "vm1:main"}
  confcatsdemo: {source: ~/src/catalyze, to: "vm1:confcatsdemo/prod"}
```

Only a bare name can be an alias: inside a project, its environment of that
exact name wins, then an alias, then the project's patterns. Parts `to:`
leaves out come from your environment's variables, never a project's
`.env`. With `source:`, completion also knows the source's commands and
services, from any directory.

A target's path can also be a plain devopsy directory, without releases:
anything maintained in place, like a git clone. Commands then run in the
path itself, and `--release`, `--rollback` and `--releases` refuse.

```sh
devopsy @vm1-traefik proxies add cloudflare   # from anywhere
devopsy @prod --release          # upload a new release and run its steps
devopsy @prod logs -f web        # any command runs in the current release
devopsy @prod --releases         # list releases, * marks the current one
devopsy @prod --rollback         # back to the previous release and run its steps
devopsy @prod --shell            # a shell in a container, like devopsy --shell
devopsy @prod --shell-host       # a shell on the server itself
devopsy @prod --vars set KEY     # set a secret in the server's shared/.env
devopsy @prod --release --help   # details of any of these
devopsy --release                # without a target: says it needs one
```

`--shell [service] [exec options]` opens a shell in a container, locally or
(`@prod --shell`) in the current release: bash, or sh where the image has no
bash, as the service's user (`user:` in compose). The service is the one
you name, else the one labeled `devopsy.shell=true` in `compose.yaml`, else
the only one running:

```yaml
services:
  app:
    labels:
      - devopsy.shell=true       # the default service
      - devopsy.shell.user=app   # its shell's user, when not the service's own
```

`devopsy.shell.user` is for images that start as root and drop to another
user in their entrypoint, which `docker compose exec` skips. Options go to
`docker compose exec`, like `devopsy --shell app --user root`, which wins
over the label. After `--`, a command runs instead of the shell, in the same
service as the same user, directly as `docker compose exec` runs it, and
without a terminal when there is none (pipes, CI):

```sh
devopsy --shell -- drush status             # the default service, its label user
devopsy @prod --shell db --user root -- ls /
``` A project that needs more (a login script, another
program) implements the `shell` capability instead (see Capabilities).

`--shell-host` opens your login shell on the server itself, where commands
run: the current release, or the path itself for a plain directory. It never
depends on the project, so it is the way in when something is broken: it is
built into the local devopsy and only needs `sh` on the server.

On a server, project commands come from the current release
(`current/.devopsy/commands/`), so a new or changed command arrives with the
next release.

### Release and rollback steps

`--release` and `--rollback` take no command: what they run belongs to the
project, in each target's `release:` and `rollback:`, and both are required
for their command. An upload that started nothing would leave `current`
ahead of the running containers, for the next command on that target to
half apply. Three phases, so the upload always happens at the same point:

```yaml
prod:
  release:
    before: [image]    # local devopsy commands, in order, before anything
                       # touches the server; a failure stops there
    remote: deploy     # one devopsy command on the server, in the new
                       # release, under the release lock; a failure makes
                       # the previous release current again
    after: [notify]    # local devopsy commands once it is live; a failure
                       # is reported, nothing is undone
  rollback:
    remote: deploy     # the same phases, after switching back
```

Each step is a devopsy command line, split on spaces (no quoting); `before`
and `after` take one or a list. There is one remote command: several remote
steps belong in a project command, which can also handle a partial failure.
Local steps run in the local project with the target's `env`,
`DEVOPSY_TARGET` and, for `--release`, `DEVOPSY_RELEASE_COMMIT`, never the
server's `shared/.env`. They can still pass something to the remote step
with `devopsy @$DEVOPSY_TARGET --vars set`. A rollback's remote command runs
in the restored release, so it must exist there. Unknown keys are refused.
`defaults:` shares steps and the rest between targets (see Remote
targets).

Without steps, `--release` and `--rollback` refuse and print a starting point:
`up -d --wait --remove-orphans --pull always` in image mode (`up` alone
would not pull a tag that moved), `--build` instead of `--pull always` in
build mode. A project usually wraps that in its own `deploy` command.

`--release` uploads the project as a new release, links the server's shared
files into it, makes it current and runs the remote step there. It keeps the
last 5 releases.

- **build** mode, the default, uploads the whole project, as git sees it:
  tracked and untracked files, minus gitignored ones, with uncommitted
  changes. Compose then builds on the server. It works for every project:
  one that only pulls images merely uploads a few more files.
- **image** mode uploads `.devopsy/` only: images come from a registry.
  Tag them with the commit, so each release and each rollback runs its own
  image (a fixed `:latest` would make a rollback pull the newest image
  again):

  ```yaml
  image: ghcr.io/me/app:${DEVOPSY_RELEASE_COMMIT:-local}
  ```

  CI builds and pushes `ghcr.io/me/app:$GITHUB_SHA`, then runs `devopsy @prod
  --release` from the same checkout, whose remote step pulls and starts it; or
  a `before` step builds and pushes it. The image is the commit's: uncommitted changes outside
  `.devopsy/` are not in it, and `--release` warns when there are some. Locally the variable is unset, so the tag is
  `local`. If the image was never pushed, the pull fails and the release goes
  back to the previous one. Projects that never build, like the server's
  Traefik, use it too, to upload only `.devopsy/`.

`--vars` manages the target's variables on the server: `shared/.env`, or
`.devopsy/.env` for a plain directory. They are its secrets and overrides,
linked into every release, as opposed to `config.yaml`'s `env`, which is
committed and written into each release as `target.env`. `--vars --project`
and `--vars --instance` manage the `.env` shared by the project's, or the
instance's, environments on that server.

```sh
devopsy @prod --vars                         # names, values hidden
devopsy @prod --vars set DB_PASSWORD API_KEY # from your environment or .env, else a hidden prompt
printf '%s' "$TOKEN" | devopsy @prod --vars set TOKEN   # or stdin, for one key
devopsy @prod --vars set --show LOG_LEVEL    # a visible prompt, for values that are not secrets
devopsy @prod --vars get DB_PASSWORD
devopsy @prod --vars unset API_KEY
```

Values never go in arguments: `set` sends them over SSH's stdin, so they
stay out of `ps`, shell history and CI logs, and in CI it copies a CI
variable to the server by name. Existing keys are replaced in place. It
works before the first release, so secrets can be in place for the first
deploy, and it waits for a running release. Running containers keep their
old values until they are recreated, as by the next release; without one,
the project's own way (a `reload` command, `up -d`...). `--show` echoes the
prompt, for values that are not secrets; `--vars set --help` explains.
Aliases only take values from your environment, never from a project's
`.env`.

On the server, the target path holds `releases/`, a `current` symlink and
`shared/`. Everything in `shared/` is linked into each release's `.devopsy/`.
`shared/.env` and `shared/mnt/` always exist: edit `.env` (secrets,
overrides) and run `devopsy @prod up -d` to apply it. Other files, like a
`compose.override.yaml`, are linked from the next release on. A local `.env`, `mnt/` and
`compose.override.yaml` are never uploaded. Commands run through `current`, so
bind mounts like `./mnt/data` keep pointing at `shared/mnt`.

Several environments of one project live side by side as several targets,
each with its own directory: its own compose project, containers, data and
public URL (`<project>[-<instance>]-<target>.<server's wildcard domain>`,
printed after each release).

Each release records its commit, branch, uncommitted changes and who made it,
shown by `--releases`.

The server needs `devopsy`, Docker, `tar` and `flock`;
[devopsy-server](https://github.com/hanoii/devopsy-server) sets that up. In CI,
set `DEVOPSY_SSH_COMMAND` to pass SSH options, like git's `GIT_SSH_COMMAND`:

```sh
DEVOPSY_SSH_COMMAND="ssh -i $DEVOPSY_SSH_KEY -o UserKnownHostsFile=$DEVOPSY_SSH_KNOWN_HOSTS" \
  devopsy @prod --release
```

## Roles, exports and imports

Projects on one host share facts through compose labels, which devopsy
reads like `devopsy.shell`. devopsy knows none of the names: they are a
convention between the projects, like devopsy-template-traefik's `proxy` role and
its `WILDCARD_DOMAIN` export.

```yaml
# the server's proxy (devopsy-template-traefik), on its traefik service
labels:
  - devopsy.role=proxy
  - devopsy.export.WILDCARD_DOMAIN=${DEVOPSY_PROXY_WILDCARD_DOMAIN:-}

# a project, on any service
labels:
  - devopsy.import.DEVOPSY_WILDCARD_DOMAIN=proxy/WILDCARD_DOMAIN
```

- `devopsy.role=<name>`: a slot only one compose project per host holds.
  Lowercase letters, digits, `-` and `_`.
- `devopsy.export.<KEY>=<value>`: a fact on the project's running
  containers. Compose fills in variables at `up`, and Docker keeps the
  value on the container, so exports change with the exporter's own `up`.
  Labels are visible to anyone with Docker access: facts, never secrets.
- `devopsy.import.<VAR>=<role or compose project>/<KEY>`, optionally ending
  in `?`: copied into each release's `target.env`.

At `--release`, before the new release becomes current, devopsy on the
server checks the project's role is not held by another compose project
there, and resolves each import: unless the release's environment already
has `<VAR>` (config.yaml or a `.env` on the server, even empty), it finds the
running compose project holding that role (else of that name), reads its
export and writes `<VAR>` into `target.env`. An export that is there but
empty is a value (`''`); one with newlines or control characters is
refused. A required source that is not running is waited for up to 30
seconds, as while the proxy restarts; then nothing running, or no such
export, fails the release, unless the import ends in `?`. A failed check
leaves `current` alone. The values stay with each release: a rollback restores what its
release had, and a later `devopsy up` on the server sees them.

So a project that expects a proxy imports from it, one that does not
imports nothing, and a target opts out of a value it would import by
setting it, for example `DEVOPSY_WILDCARD_DOMAIN: ""` in `config.yaml` or
`devopsy @prod --vars set --show DEVOPSY_WILDCARD_DOMAIN` (empty).

`devopsy --debug labels` (locally, or `devopsy @prod --debug labels` on a
server) shows the roles held on the host with their exports, and what each
of the project's imports resolves to now and in the current release.
Because each release keeps what it imported, a changed export (a new
wildcard domain, say) only reaches a project with its next release:
`devopsy @prod --debug imports` lists every project running on that host
with its imports, and marks those whose release has an outdated value as
STALE, to release again.

Rollbacks and plain `up` never check anything. The server's devopsy must be
v0.17.0 or newer for projects with these labels (a release says so);
projects without them release with any. Order when moving a server to them:
upgrade its devopsy, release the exporter (devopsy-template-traefik), then the
projects importing from it.

Roles are advisory, not a security boundary: they are only checked when
devopsy releases a project, against running containers, so a role is free
while its holder is stopped, and anyone in the docker group (root-equivalent)
can run a container claiming any role or exporting any value. They prevent
mistakes, like two proxies on one host. Separate servers separate clients.

## Probe

```sh
devopsy --probe [--ip <server ip>] <host>...
```

Checks hosts from where you run it, as visitors reach them: DNS through
1.1.1.1, IPv4 and IPv6 (and whether it points at `--ip`, either kind, or at
a CDN proxy: Cloudflare's IPv4 ranges are recognized), the certificate the server presents (at `--ip`,
else where the host resolves), verified like a browser would, and an HTTPS
request through what DNS returns, with Cloudflare's origin errors (521,
522, 525, 526) explained. Exits 1 when a host has a problem, so it also
works as a smoke test after a release. A wildcard cannot be probed: name a
host it covers.

It knows no proxy, and takes plain arguments: a proxy's commands print the
line to run, like devopsy-template-traefik's `domains`, which reports what the proxy
knows (routes, resolvers, the CNAME to create) from the server.

## Capabilities

A capability is an interface devopsy defines and a project implements, so
devopsy can ask a project for something without knowing how it works. A project
implements one with executables in `.devopsy/capabilities/<name>/<action>`
(POSIX `sh` advised). They are not commands: never in help or completion,
and only devopsy runs them, as `devopsy --capability <name> <action>
[args...]` on the server, in the project's directory (its current release),
with its environment loaded, over its own SSH session so no other project's
variables apply. What they print on stderr shows only when they fail. A
missing capability or action is an error naming where devopsy looked.

### shell

Implemented by any project that wants its own `--shell`. `open [service]
[exec options...] [-- command...]`, with the arguments `--shell` got, opens
an interactive shell, or runs the command after `--`, however the project
needs: devopsy runs it in place of its own
(`exec`, the terminal attached), locally or on the server for
`@<target> --shell`. Its exit status is the shell's.

## License

GPL-3.0. See [LICENSE](LICENSE).
