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
devopsy help           # lists the custom commands
devopsy version        # devopsy's version, then compose's inside a project
devopsy print-env      # the variables devopsy loads and computes
```

### Custom commands

Any executable file in `.devopsy/commands/` becomes a command. It runs with
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
`devopsy print-env` prints what it loads and computes, in `.env` format, to
run plain compose with exactly the same values:

```sh
devopsy print-env > /tmp/devopsy.env
docker compose -f .devopsy/compose.yaml --env-file /tmp/devopsy.env config
```

Use a file: compose reads `--env-file` more than once, so `<(devopsy
print-env)` does not work.

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

Commit `targets.yaml`: CI deploys from it. It holds no secrets; to keep
server addresses out of the repository, use `~/.ssh/config` aliases as hosts.
`.devopsy/targets.local.yaml` (gitignore it) adds or replaces whole targets
for one machine, like a personal test server, and is never uploaded.

A target's `env` is written into each release as `.devopsy/target.env`, which
devopsy loads after `.env` and before the server-wide settings. So it applies
however devopsy runs on the server, and a rollback brings back that release's
values. Change it in `targets.yaml` and release again.

```sh
devopsy @prod release deploy   # upload a new release, run `devopsy deploy` there
devopsy @prod logs -f web      # any command runs in the current release
devopsy @prod releases         # list releases, * marks the current one
devopsy @prod rollback up -d   # back to the previous release, then `up -d`
```

`release` uploads the project as a new release, links the server's shared
files into it and makes it current. With a command, it runs `devopsy
<command>` there, and goes back to the previous release when that fails, with
the command's exit code. It keeps the last 5 releases.

- **image** mode uploads `.devopsy/` only: images come from a registry.
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
