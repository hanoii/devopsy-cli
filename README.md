# devopsy-cli

`devopsy` is a small command-line tool for running Docker Compose projects,
both on your machine and on servers you own.

Compose describes a project but has nothing for running it on a server.
devopsy adds:

- Releases. One command ships a new version, from your machine or CI, and
  another rolls it back.
- Environments. Production, staging or one per pull request run side by
  side on one server, each with its own containers and volumes.
- Variables per deployment. A value can be set for a project on a
  server, for one install, or for one environment, and the most specific
  wins. Secrets stay out of git and your shell history. A release that is
  missing a required value stops before going live and names what to
  set.

Everything a project needs lives in a `.devopsy/` directory next to your
code, and every project works the same way.

## What it is not

devopsy has no control plane, dashboard, agent or daemon, and nothing
listens for webhooks. A server is Docker, SSH and this binary.

It does no orchestration: no Kubernetes, no Swarm, no failover. An
environment is one compose project on one server.

It does not replace compose or rewrite your compose files. Plain `docker
compose` sees what devopsy runs, apart from one file a release adds on a
server, `devopsy-compose.yaml` ([On the
server](docs/releases.md#on-the-server)).

It is not a local development tool like DDEV. It has no per-framework
setup or router, and runs the compose project you write.

It is not a build system. Images are built however you like: on the
server by compose, or in CI and pushed to a registry.

Any of these can be built on top of it. Its commands, `--debug` output and
plain files are meant to be scripted, by you or with an AI agent.

## Who it is for

People and small teams who deploy different web applications, sometimes
with complex stacks and their own deploy strategies, to a few servers they
own, and want what a hosting platform gives (an environment per branch,
releases and rollbacks, secrets out of git, URLs and certificates) without
running or paying for one.

It assumes some technical knowledge: a terminal, SSH, and the basics of
Docker. It can also teach how Docker and Docker Compose deployments work,
since every step it takes is plain compose you can read and run yourself.

It grew out of hosting websites for clients: many projects, some installed
several times from one codebase, spread over a handful of servers, each
deployed by a different person or CI job. It fits when:

- you already describe your stack in compose, or are happy to;
- a project fits on one server per environment;
- a release that recreates its containers, for a few seconds, is fine;
- the people deploying have SSH access to the servers.

## The choices it makes

devopsy aims for every project to be worked on, released and operated the
same way. Moving between projects, handing one over, or letting CI or an
agent deploy it then needs nothing relearned:

- the same commands in every project: `devopsy up`, `devopsy @prod
  --release`, `--rollback`, `--log`, `--vars`, `--shell`;
- the same layout on every server: `<project>/<environment>/` with
  `releases/`, `current` and `shared/`, with data, secrets and release
  logs always in the same place;
- the same names for compose projects, containers and volumes
  ([Where an environment lives](docs/config.md#where-an-environment-lives)).

For that, it decides some things for you:

- devopsy adds only environment variables to compose, all of them shown by
  `devopsy --env`. Everything else is compose's.
- Releases are pushed over SSH, never pulled. The server needs no access to the
  repository, and no deploy keys, tokens or webhooks. Locally devopsy
  needs `ssh`. On the server it needs `devopsy` and what Debian has:
  `tar`, GNU coreutils and util-linux (`flock`, `setsid`, `script`).
- A release is a complete directory, run through a `current` symlink, and
  `shared/` keeps data and secrets across releases. A release never
  changes a live tree in place ([On the
  server](docs/releases.md#on-the-server)).
- `--release` and `--rollback` run the steps the environment names, and
  refuse without them ([Steps](docs/releases.md#steps)).
- Project behavior lives in the project, as commands in
  `.devopsy/commands/` ([Project commands](docs/local.md#project-commands)).
- Conveniences most projects need, but each must get right for itself,
  are small contracts devopsy defines and a project implements in
  `.devopsy/capabilities/` ([Capabilities](docs/capabilities.md)).
- Which servers and instances exist is decided by the command, variables
  or aliases, and is kept out of the project's config
  ([Targets](docs/config.md#targets)).
- devopsy's own features are flags or `@target`. A word is a project
  command, else a compose command ([Words](docs/local.md#words)).
- The secrets a project names are masked in output, and their values are
  never passed as arguments ([Secrets](docs/local.md#secrets)).

Everything else is yours to make; `devopsy --ai-skill`
([Agents](docs/agents.md)) teaches an agent how.

## Templates

I created these templates as starting points, and as examples of how to work
with devopsy. Together they set up a stack for deploying complex web
applications, with an environment per branch or pull request, from your own
repository.

They are GitHub template repositories: start a project from one ("Use this
template"), or sync it and keep it as a remote to pull its changes when you
choose, then adapt it to your needs. They define one particular way of
doing things; devopsy enforces none of it, beyond the labels they use for
its features.

- [devopsy-template-traefik](https://github.com/hanoii/devopsy-template-traefik):
  the reverse proxy the others route through, one per server.
  - Automatic HTTPS certificates, including before DNS points to the server.
  - A base domain, so every environment gets its own URL.
  - Real visitor IPs behind CDNs and other proxies.
  - A `domains` report: DNS, certificates and what to do next.
  - Cloudflare support: its IP ranges, automatic DNS challenges with an API
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
binary, with `sudo` when its directory is not yours. On servers set up
with devopsy-server the directory is the deploy user's, and `devopsy
@<server> --upgrade` upgrades a server from your machine.

At a terminal, devopsy checks for a new release once a day in the
background and says when there is one. It does not check in CI or in what
devopsy runs on servers, and `DEVOPSY_NO_UPDATE_CHECK=1` turns the check
off.

To run a checkout's code beside the release, symlink `scripts/devopsy` into
your `PATH` as `devopsy-dev` (it rebuilds on every run, instantly when
nothing changed; needs Go). What it runs that calls `devopsy` runs the same
build.

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

## Documentation

- [Working locally](docs/local.md): commands, project commands, variables,
  output, secrets and debugging.
- [Config and targets](docs/config.md): `.devopsy/config.yaml`, addresses,
  instances, parents, variables on a server, release settings, aliases.
- [Releases](docs/releases.md): steps, the checks before going live,
  failures and rollbacks, dropped connections, logs, modes, reloads.
- [Images](docs/images.md): what each release keeps, tags, data versions
  and holds.
- [Data](docs/data.md): `.devopsy/mnt`, owners, the data labels,
  `--export` and `--import`.
- [Parent environments and syncs](docs/syncs.md): a child's data from its
  parent, `--reset`, inherited variables.
- [Snapshots](docs/snapshots.md): taking, restoring, exporting and
  importing them, release snapshots.
- [Servers](docs/servers.md): server commands, `--server-info`, CI, SSH
  and keys.
- [Server setup how-to](docs/server-setup-howto.md): the steps for what
  devopsy checks on a server and does not set up itself.
- [Codes](docs/codes.md): the codes of messages for a server's admin.
- [Project users](docs/project-users.md): a project with a Unix user of
  its own and no Docker socket, why and how.
- [Roles, exports and imports](docs/roles.md): facts shared between
  projects on a server, and `--probe`.
- [Capabilities](docs/capabilities.md): `status`, `env`, `shell` and `data`.
- [Agents](docs/agents.md): the skill that teaches an agent to work on a
  devopsy project.

## License

GPL-3.0. See [LICENSE](LICENSE).
