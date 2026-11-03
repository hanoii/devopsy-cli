# devopsy-cli

`devopsy` is a small command-line tool for running Docker Compose projects,
both on your machine and on servers you own.

Compose is great for describing a project, but leaves you on your own once
it has to live on a server. devopsy tries to help with a few things there:

- Releases: ship a new version with one command, from your machine or
  CI, and roll back just as easily.
- Environments: production, staging or one per pull request, side by
  side on the same server without stepping on each other.
- Variables per deployment: set a value once for a project on a
  server, or only for one install or one environment; the most specific
  wins. Secrets never touch git or your shell history, and a release that
  is missing a required value stops before going live and tells you what
  to set.

Everything a project needs lives in a `.devopsy/` directory next to your
code, so every project works the same way.

## What it is not

- Not a platform. No control plane, dashboard, agent or daemon; nothing
  listens for webhooks. A server is Docker, SSH and this binary.
- Not an orchestrator. No Kubernetes, no Swarm, no failover: an
  environment is one compose project on one server.
- Not a compose replacement. It never rewrites your compose files:
  plain `docker compose` sees what devopsy runs, but for the one file of
  devopsy's own a release adds on a server (`devopsy-compose.yaml`: the
  image each service was given, and the data services held on another
  image than their release's own).
- Not a local development tool like DDEV. No per-framework setup or
  router: it runs the compose project you write.
- Not a build system. Images are built however you like: on the server
  by compose, or in CI and pushed to a registry.

devopsy is none of these, but any of them can be built on top of it: its
commands, `--debug` output and plain files are meant to be scripted, by
you or with an AI agent.

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

- Compose plus environment variables. devopsy only adds variables, all
  visible with `devopsy --env`. Anything else is compose's.
- Push over SSH, never pull. The server needs no access to the
  repository: no deploy keys, tokens or webhooks. Locally devopsy needs
  `ssh`; on the server `devopsy` and what Debian has: `tar`, GNU
  coreutils and util-linux (`flock`, `setsid`, `script`).
- Releases are complete directories, run through a `current` symlink;
  `shared/` keeps data and secrets across them. A release never changes a
  live tree in place.
- Steps are explicit. `--release` and `--rollback` refuse without the
  steps the environment names. A release's steps come from the checkout,
  which is what it uploads; a rollback's remote steps, the restart after
  a failed `run` and `--destroy`'s step come from the release they act
  on, its own `config.yaml` on the server.
- Project behavior lives in the project, as commands in
  `.devopsy/commands/`, not as devopsy features.
- Capabilities for common needs. Some conveniences are useful to most
  projects but depend on each one to get right. devopsy defines them as
  small contracts, and a project that wants one implements it in
  `.devopsy/capabilities/`.
- Deployment facts stay out of the project: which servers and instances
  exist is decided by the command, variables or aliases, never the
  project's config.
- Words are yours. devopsy's own features are flags or `@target`; a word
  is always a project command, then a compose command.
- Secrets stay out of sight: the ones the project names are masked in
  output, and values never go in arguments.

Everything else is yours to make; `devopsy --ai-skill` ([Agents](docs/agents.md))
teaches an agent how.

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
  - A base domain, so every environment gets its own URL.
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
- [Roles, exports and imports](docs/roles.md): facts shared between
  projects on a server, and `--probe`.
- [Capabilities](docs/capabilities.md): `status`, `env`, `shell` and `data`.
- [Agents](docs/agents.md): the skill that teaches an agent to work on a
  devopsy project.

## License

GPL-3.0. See [LICENSE](LICENSE).
