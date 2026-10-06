# devopsy-cli

A small wrapper around `docker compose` for projects that keep their
deployment in a `.devopsy/` directory. Run `devopsy` from anywhere inside the
project and it finds `.devopsy/`, loads its `.env` and runs compose with the
right files. Anything it doesn't know becomes a `docker compose` command.

It is a single static binary for Linux and macOS (amd64 and arm64), with no
dependencies besides Docker and the Compose plugin.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/hanoii/devopsy-cli/main/install.sh | sh
```

It downloads the latest release for your platform, checks its checksum and
installs it to `/usr/local/bin` when it can, otherwise `~/.local/bin`. Set
`DEVOPSY_VERSION` to install a specific release, like `v0.1.0`, and
`DEVOPSY_INSTALL_DIR` to choose the directory. Run it again to upgrade.

Releases are on the [releases page](https://github.com/hanoii/devopsy-cli/releases).
To build from source: `go build -o devopsy ./cmd/devopsy`.

To always run a checkout's latest code, uncommitted changes included, symlink
`scripts/devopsy` into your `PATH` instead. It rebuilds on every run, which
Go's build cache makes instant when nothing changed, and needs Go:

```sh
ln -s "$PWD/scripts/devopsy" ~/.local/bin/devopsy
devopsy --version   # like v0.5.0-2-g1a2b3c4-dirty
```

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
devopsy --version      # devopsy's and docker compose's versions
devopsy --env          # the variables devopsy loads and computes
```

devopsy's own features are flags (`--help`, `--version`, `--env`) or start
with `@` (targets), so they never clash with words: a word is a project
command if `.devopsy/commands/` has it, else a docker compose command.
`devopsy version` is `docker compose version`.

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
- `DEVOPSY_PUBLIC_HOST`: `<project>.<DEVOPSY_PUBLIC_DOMAIN>` when the server
  has a public domain, else `<project>.localhost`. Set it yourself to
  override.
- `DEVOPSY_HOST_RULE`: a Traefik rule for the public host plus
  `DEVOPSY_DOMAINS`, a space or comma separated list you set per environment,
  usually in its `.env`. For example
  ``Host(`shop.vm1.example.com`) || Host(`example.org`)``.

A router label then needs no per-environment hosts. Defaults keep the file
usable with plain `docker compose`:

```yaml
labels:
  - traefik.enable=true
  - traefik.http.routers.${DEVOPSY_PROJECT_NAME:-app}.rule=${DEVOPSY_HOST_RULE:-Host(`app.localhost`)}
```

devopsy only adds environment variables; it never changes compose files.
`devopsy --env` prints what it loads and computes, in `.env` format, to
run plain compose with exactly the same values:

```sh
devopsy --env > /tmp/devopsy.env
docker compose -f .devopsy/compose.yaml --env-file /tmp/devopsy.env config
```

Use a file: compose reads `--env-file` more than once, so `<(devopsy
--env)` does not work.

On a server, `/etc/devopsy/devopsy.env` holds server-wide settings, like
`DEVOPSY_PUBLIC_DOMAIN`, written by devopsy-server. The project's `.env` and
your environment win over it. See devopsy-traefik for routing public URLs.

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
    DEVOPSY_DOMAINS: example.org www.example.org
staging:
  host: devopsy@203.0.113.10
  path: /srv/myapp-staging
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

A target's path can also be a plain devopsy directory, without releases: a
git clone like each server's `/srv/traefik`, or anything maintained in place.
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
devopsy loads after `.env` and before the server-wide settings. So it applies
however devopsy runs on the server, and a rollback brings back that release's
values. Change it in `targets.yaml` and release again. When `compose.yaml` has
no `name:`, `target.env` also fixes `COMPOSE_PROJECT_NAME` to the target
directory's name, so devopsy run by hand on the server, in `current`, still
finds the project. From a git checkout, it also sets `DEVOPSY_RELEASE_COMMIT`
to the commit released (see image tags below).

```sh
devopsy @prod release deploy   # upload a new release, run `devopsy deploy` there
devopsy @prod logs -f web      # any command runs in the current release
devopsy @prod releases         # list releases, * marks the current one
devopsy @prod rollback up -d   # back to the previous release, then `up -d`
devopsy @prod domains          # per host: DNS, challenge, certificate, next step
devopsy @prod --shell          # a shell on the server, in the current release
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
next release. `devopsy @prod release` alone uploads and switches without
running anything; running containers keep going, so use `release deploy`
when anything else changed.

`release` uploads the project as a new release, links the server's shared
files into it and makes it current. With a command, it runs `devopsy
<command>` there, and goes back to the previous release when that fails, with
the command's exit code. It keeps the last 5 releases.

- **image** mode uploads `.devopsy/` only: images come from a registry.
  Tag them with the commit, so each release and each rollback runs its own
  image (a fixed `:latest` would make a rollback pull the newest image
  again):

  ```yaml
  image: ghcr.io/me/app:${DEVOPSY_RELEASE_COMMIT:-local}
  ```

  CI builds and pushes `ghcr.io/me/app:$GITHUB_SHA`, then runs `devopsy @prod
  release deploy` from the same checkout; the project's `deploy` pulls and
  starts it. The image is the commit's: uncommitted changes outside
  `.devopsy/` are not in it, and `release` warns when there are some. Locally the variable is unset, so the tag is
  `local`. If the image was never pushed, the pull fails and the release goes
  back to the previous one.
- **build** mode uploads the whole project, as git sees it: tracked and
  untracked files, minus gitignored ones, with uncommitted changes. Compose
  then builds on the server.

On the server, the target path holds `releases/`, a `current` symlink and
`shared/`. Everything in `shared/` is linked into each release's `.devopsy/`.
`shared/.env` and `shared/mnt/` always exist: edit `.env` (secrets,
overrides) and run `devopsy @prod up -d` to apply it. Other files, like a
`compose.override.yaml`, are linked from the next release on. A local `.env`, `mnt/` and
`compose.override.yaml` are never uploaded. Commands run through `current`, so
bind mounts like `./mnt/data` keep pointing at `shared/mnt`.

`domains` checks the environment's public host and `DEVOPSY_DOMAINS` from
where you run it: DNS through 1.1.1.1, the acme-dns challenge CNAME when the
router uses `acmedns`, and the certificate the server actually presents for
each name, verified like a browser would. A domain behind Cloudflare's proxy
resolves to Cloudflare, so `domains` recognizes its ranges and requests the
site through the proxy instead, reporting Cloudflare's origin errors (521,
522, 525, 526) with what they mean. It ends each host with what to do
next, like the CNAME to create or "certificate ready: point its DNS at ...".
`domains --retry` asks Traefik to request missing certificates again, through
a small file in its dynamic configuration, without restarting it. Once every
routed host has a valid certificate, `domains` removes that file again:
Traefik keeps and renews the certificates without it.

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
  devopsy @prod release deploy
```

## License

GPL-3.0. See [LICENSE](LICENSE).
