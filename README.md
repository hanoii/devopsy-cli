# devopsy-cli

A small wrapper around `docker compose` for projects that keep their
deployment in a `.devopsy/` directory. Run `devopsy` from anywhere inside the
project and it finds `.devopsy/`, loads its `.env` and runs compose with the
right files. Anything it doesn't know becomes a `docker compose` command.

It is a single static binary for Linux and macOS (amd64 and arm64), with no
dependencies besides Docker and the Compose plugin.

## Opinions

devopsy decides a few things for you, and the rest of this README follows
from them:

- **Compose plus environment variables.** devopsy never generates or
  rewrites compose files. It only adds variables, all visible with
  `devopsy --env`, so plain `docker compose` sees exactly what it sees.
  Configuration is environment variables, with a fixed precedence: caller,
  `.env` (on servers `shared/.env`), the target's `env`. Nothing
  server-wide: what describes a server, its host and public domain, belongs
  to each target.
- **An environment is a server path.** Each target is a directory on a
  server, so its own compose project, data and public URL. There is no
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
- **Steps are explicit.** `release` and `rollback` run the steps each target
  names in `targets.yaml`; without them they refuse, rather than upload a
  release nothing applies.
- **Project behavior lives in the project.** Anything that depends on a
  project's services, users or data is a command in its
  `.devopsy/commands/`, not a devopsy feature. devopsy does not build or
  push images either; it only helps decide when to (`--context-hash`).
- **Traefik only where it helps.** Releases and commands do not depend on
  a proxy. `DEVOPSY_HOST_RULE` is a Traefik rule as a plain variable, which
  projects are free to ignore. `@target domains` checks hosts from outside,
  where only the CLI is installed, and asks the server's proxy what it knows
  through a capability: the proxy's internals stay in
  [devopsy-traefik](https://github.com/hanoii/devopsy-traefik).
- **Capabilities are interfaces.** devopsy defines a few, like `domains`;
  a project implements one by shipping its scripts, and devopsy calls them.
  People never do, so they are never words.
- **Words are yours.** devopsy's own features are flags or `@target`, so a
  word is always a project command, then a compose command.
- **Secrets stay out of sight.** Values from `.env` and secret-named
  variables are masked in everything devopsy prints, and `--vars` sends
  values over SSH's stdin, never as arguments.

Other practices, like container UIDs, certificate resolvers or where data
lives, are recommendations: see the
[devopsy workspace](https://github.com/hanoii/devopsy) and its recipes.

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

It completes targets (`@prod`, user-level ones included, with their host and
path), the project's commands with their descriptions, devopsy's flags,
the commands after `@<target>` (`release`, `--vars set` with the keys of
the project's `.env`...) and, through docker compose's own completion,
compose's commands, flags and the project's services. After a project
command, it completes file names. On a target, services come from the local
compose files: completion never connects to servers.

Targets come first, the project's before user-level ones, then project
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
devopsy --context-hash [service]   # a hash of what the service's image is built from
devopsy --upgrade      # replace devopsy with the latest release
devopsy --completion fish   # a shell completion script (bash, zsh, fish)
devopsy -v deploy      # --verbose: also what devopsy found and runs
```

devopsy's own features are flags (`--help`, `--version`, `--env`,
`--context-hash`, `--upgrade`, `--verbose`, `--completion`) or start
with `@` (targets), so they never clash with words: a word is a project
command if `.devopsy/commands/` has it, else a docker compose command.
`devopsy version` is `docker compose version`.

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
- `DEVOPSY_PUBLIC_HOST`: `<project>.<DEVOPSY_PUBLIC_DOMAIN>`. The public
  domain describes the server, like the target's host, so it is set per
  target: in `targets.yaml`'s `env`, or, to keep it out of a public
  repository, in the server's `shared/.env` (`devopsy @prod --vars set
  --show DEVOPSY_PUBLIC_DOMAIN`, once per target; a local `.env` never
  reaches the server). Without one, `<project>.localhost`
  locally, and no public host in a release: the environment only answers on
  its `DEVOPSY_DOMAINS`. Set it yourself to override.
- `DEVOPSY_HOST_RULE`: a Traefik rule for the public host plus
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
or from CI. Define targets in `.devopsy/targets.yaml`:

```yaml
prod:
  host: devopsy@203.0.113.10    # any SSH destination or ~/.ssh/config alias
  path: /srv/myapp              # absolute, writable by that user
  mode: image                   # image (default) or build
  env:                          # per-target settings, not secrets
    DEVOPSY_PUBLIC_DOMAIN: vm1.example.com    # the server's: <project>.vm1.example.com
    DEVOPSY_DOMAINS: example.org www.example.org
  release:                      # what `release` runs (required for it)
    remote: deploy
  rollback:                     # what `rollback` runs (required for it)
    remote: deploy
staging:
  host: devopsy@203.0.113.10
  path: /srv/myapp-staging
  release:
    remote: deploy
```

Commit `targets.yaml`: CI deploys from it. It holds no secrets.
`.devopsy/targets.local.yaml` (gitignore it) adds or replaces whole targets
for one machine, like a personal test server, and is never uploaded.

To keep server addresses out of the repository, leave `host` out and set it
with variables, in your environment or `.devopsy/.env` (CI sets them in its
environment):

```sh
DEVOPSY_TARGET_HOST=devopsy@203.0.113.10        # targets without a host
DEVOPSY_TARGET_HOST_STAGING=devopsy@203.0.113.20  # replaces staging's host
```

The per-target variable is `DEVOPSY_TARGET_HOST_` and the target's name in
upper case, with anything but letters and digits as `_` (`staging-eu`:
`DEVOPSY_TARGET_HOST_STAGING_EU`). It replaces any host; `DEVOPSY_TARGET_HOST`
only fills in a missing one. `~/.ssh/config` aliases work as hosts too.

### User-level targets and plain directories

`~/.config/devopsy/targets.yaml` (or `$DEVOPSY_HOME/targets.yaml`) holds
targets you use from any directory, for running commands on servers. They
never `release` or `rollback`: those need a target the project defines. A
project's own targets win over user-level ones with the same name.

A target's path can also be a plain devopsy directory, without releases:
anything maintained in place, like a git clone.
Commands then run in the path itself, and `release`, `rollback` and
`releases` refuse. Together:

```yaml
# ~/.config/devopsy/targets.yaml
vm1-traefik:
  host: devopsy@203.0.113.10
  path: /srv/traefik
```

```sh
devopsy @vm1-traefik proxies add cloudflare   # from anywhere
devopsy @vm1-traefik logs -f traefik
```

A target's `env` is written into each release as `.devopsy/target.env`, which
devopsy loads after `.env`, so the server's `shared/.env` overrides it. So it applies
however devopsy runs on the server, and a rollback brings back that release's
values. Change it in `targets.yaml` and release again. When `compose.yaml` has
no `name:`, `target.env` also fixes `COMPOSE_PROJECT_NAME` to the target
directory's name, so devopsy run by hand on the server, in `current`, still
finds the project. From a git checkout, it also sets `DEVOPSY_RELEASE_COMMIT`
to the commit released (see image tags below).

```sh
devopsy @prod release          # upload a new release and run its steps
devopsy @prod logs -f web      # any command runs in the current release
devopsy @prod releases         # list releases, * marks the current one
devopsy @prod rollback         # back to the previous release and run its steps
devopsy @prod domains          # per host: DNS, challenge, certificate, next step
devopsy @prod --shell          # a shell on the server, in the current release
devopsy @prod --vars set KEY   # set a secret in the server's shared/.env
devopsy @prod release --help   # details of any of these
```

`--shell` opens your login shell on the server where commands run: the
current release, or the path itself for a plain directory. For a shell in a
container, use compose, `devopsy @prod exec <service> bash`, or a project
command that knows the service and user. `--shell` is built into the local
devopsy and only needs `sh` on the server, so it works with any target and
server version.

On a server, project commands come from the current release
(`current/.devopsy/commands/`), so a new or changed command arrives with the
next release.

### Release and rollback steps

`release` and `rollback` take no command: what they run belongs to the
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
`DEVOPSY_TARGET` and, for `release`, `DEVOPSY_RELEASE_COMMIT`, never the
server's `shared/.env`. They can still pass something to the remote step
with `devopsy @$DEVOPSY_TARGET --vars set`. A rollback's remote command runs
in the restored release, so it must exist there. Unknown keys are refused.
YAML anchors share steps between targets (`release: &steps ...`, then
`release: *steps`).

Without steps, `release` and `rollback` refuse and print a starting point:
`up -d --wait --remove-orphans --pull always` in image mode (`up` alone
would not pull a tag that moved), `--build` instead of `--pull always` in
build mode. A project usually wraps that in its own `deploy` command.

`release` uploads the project as a new release, links the server's shared
files into it, makes it current and runs the remote step there. It keeps the
last 5 releases.

- **image** mode uploads `.devopsy/` only: images come from a registry.
  Tag them with the commit, so each release and each rollback runs its own
  image (a fixed `:latest` would make a rollback pull the newest image
  again):

  ```yaml
  image: ghcr.io/me/app:${DEVOPSY_RELEASE_COMMIT:-local}
  ```

  CI builds and pushes `ghcr.io/me/app:$GITHUB_SHA`, then runs `devopsy @prod
  release` from the same checkout, whose remote step pulls and starts it; or
  a `before` step builds and pushes it. The image is the commit's: uncommitted changes outside
  `.devopsy/` are not in it, and `release` warns when there are some. Locally the variable is unset, so the tag is
  `local`. If the image was never pushed, the pull fails and the release goes
  back to the previous one.
- **build** mode uploads the whole project, as git sees it: tracked and
  untracked files, minus gitignored ones, with uncommitted changes. Compose
  then builds on the server.

`--vars` manages the target's variables on the server: `shared/.env`, or
`.devopsy/.env` for a plain directory. They are its secrets and overrides,
linked into every release, as opposed to `targets.yaml`'s `env`, which is
committed and written into each release as `target.env`.

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
prompt, for values that are not secrets; `--vars set --help` explains. User-level
targets only take values from your environment, never from a project's
`.env`.

On the server, the target path holds `releases/`, a `current` symlink and
`shared/`. Everything in `shared/` is linked into each release's `.devopsy/`.
`shared/.env` and `shared/mnt/` always exist: edit `.env` (secrets,
overrides) and run `devopsy @prod up -d` to apply it. Other files, like a
`compose.override.yaml`, are linked from the next release on. A local `.env`, `mnt/` and
`compose.override.yaml` are never uploaded. Commands run through `current`, so
bind mounts like `./mnt/data` keep pointing at `shared/mnt`.

`domains` checks the environment's public host and `DEVOPSY_DOMAINS`, as
the server computes them, from where you run it: DNS through 1.1.1.1, the
challenge CNAME when the certificate is issued through one, and the
certificate the server actually presents for each name, verified like a
browser would. What the server's proxy knows (which hosts it routes, how it
issues each certificate) comes from its `domains` capability (see
Capabilities), in `DEVOPSY_PROXY_DIR` on the target's server, by default
`/srv/traefik`. A domain behind Cloudflare's proxy
resolves to Cloudflare, so `domains` recognizes its ranges and requests the
site through the proxy instead, reporting Cloudflare's origin errors (521,
522, 525, 526) with what they mean. Any other answer, even the site's own
401, means the proxy reaches the server: without a valid certificate there,
its SSL mode is not Full (strict) and the certificate is still pending, which
the proxy requests once the site is routed. It ends each host with what to
do next, like the CNAME to create or "certificate ready: point its DNS at
...". `domains --retry` asks the proxy to request missing certificates
again (devopsy-traefik: a router file, no restart). Once every routed host
has a valid certificate, `domains` withdraws that request: the proxy keeps
and renews the certificates without it.

Run on the proxy's own target (its path is `DEVOPSY_PROXY_DIR`, like
`devopsy @vm1-traefik domains`), it checks the whole server: every host the
proxy routes, and wildcards (`*.<public domain>`) with their challenge
CNAME, through a name each one covers. `--retry` there covers wildcards too.

Several environments of one project live side by side as several targets,
each with its own path: its own compose project, containers, data and public
URL (`<target directory>.<server's public domain>`, printed after each
release).

Each release records its commit, branch, uncommitted changes and who made it,
shown by `releases`. When `compose.yaml` has no top-level `name:`, the project
is named after the target directory, here `myapp`.

The server needs `devopsy`, Docker, `tar` and `flock`;
[devopsy-server](https://github.com/hanoii/devopsy-server) sets that up. In CI,
set `DEVOPSY_SSH_COMMAND` to pass SSH options, like git's `GIT_SSH_COMMAND`:

```sh
DEVOPSY_SSH_COMMAND="ssh -i $DEVOPSY_SSH_KEY -o UserKnownHostsFile=$DEVOPSY_SSH_KNOWN_HOSTS" \
  devopsy @prod release
```

## Capabilities

A capability is an interface devopsy defines and a project implements, so
devopsy can ask a project for something without knowing how it works: the
server's proxy for what it knows about hosts, for example. A project
implements one with executables in `.devopsy/capabilities/<name>/<action>`
(POSIX `sh` advised). They are not commands: never in help or completion,
and only devopsy runs them, as `devopsy --capability <name> <action>
[args...]` on the server, in the project's directory (its current release),
with its environment loaded, over its own SSH session so no other project's
variables apply. What they print on stderr shows only when they fail. A
missing capability or action is an error naming where devopsy looked.

### domains

Implemented by the server's proxy (devopsy-traefik), used by `devopsy
@target domains`.

`facts <host>...` (or `facts --all`, every host the proxy routes) prints
one JSON document:

```json
{
  "version": 1,
  "ip": "203.0.113.10",
  "retries": ["shop"],
  "hosts": [
    {"host": "*.vm1.example.com", "probe": "devopsy-wildcard.vm1.example.com",
     "routed": true, "resolver": "acmedns", "method": "dns-cname",
     "record": {"name": "_acme-challenge.vm1.example.com", "target": "w.acme-vm1.example.com"}},
    {"host": "shop.vm1.example.com", "routed": true, "resolver": "letsencrypt1",
     "method": "http", "wildcard": "*.vm1.example.com"},
    {"host": "api.example.org", "routed": true, "resolver": "cloudflare", "method": "dns-api"},
    {"host": "old.example.org", "routed": false}
  ]
}
```

- `version`: 1. devopsy refuses a newer one, asking to upgrade devopsy.
  Unknown fields are ignored.
- `ip`: the server's public IPv4, for "point its DNS at ...".
- `retries`: names with a pending retry request.
- `routed`: the proxy has a route for the host. A host it does not list is
  not routed.
- `probe`: for a wildcard, a name it covers to look up and connect to.
- `resolver`: shown in the report, never acted on.
- `method`: how the certificate is issued. `http` (HTTP-01: the host's DNS
  must reach the server), `dns-cname` (DNS-01 through a delegated record,
  `record`, once the proxy knows it) or `dns-api` (DNS-01 through the DNS
  provider's API: nothing to create).
- `wildcard`: a wildcard certificate covering the host.

`retry <name> <host>...` asks the proxy to request those certificates
again, and `retry <name> --done` withdraws the request. `<name>` keeps
callers apart: devopsy passes the compose project name, or `server` on the
proxy's own target.

Exit status 0 on success; anything else is an error, its message on stderr.

## License

GPL-3.0. See [LICENSE](LICENSE).
