---
name: devopsy
description: Working on a project that runs on devopsy (a .devopsy/ directory, or one to be released with devopsy): writing its compose file, Dockerfile and config.yaml the way devopsy expects, releasing it to servers and operating it there.
devopsy: {{VERSION}}
---

# devopsy

devopsy runs Docker Compose projects on plain servers: `devopsy` wraps
`docker compose` for a project with a `.devopsy/` directory, and
`devopsy @<target>` releases and runs it on a server over SSH. No control
plane: compose, environment variables and SSH.

## First

- This text comes from devopsy `{{VERSION}}`. If `devopsy --version` says
  another, run `devopsy --ai-skill` for the text that matches it.
- `devopsy --help` lists devopsy's own flags and where each applies
  (locally, on a target, on a server); most take `--help` for their own
  (`--release --help`, `--debug --help`). Bare `devopsy` in a project
  lists the project's own commands (`.devopsy/commands/`); a word is one
  of those, else a compose command (`devopsy up -d`), else an error.
- devopsy comes from https://github.com/hanoii/devopsy-cli (its README
  has the install line).
- Never touch a real server unless the user asks: a target's commands run
  there. Read-only ones are `--status`, bare `--releases`, `--log`,
  `--env`, `--vars` and `--vars get`, `--environments`, `--debug`.
- `devopsy --debug schema` documents every key of config.yaml, `--debug
  capabilities` the capabilities' contracts, `--debug env` where each
  variable comes from.

## A devopsy project

```
.devopsy/
  compose.yaml      the project; no name: (see below)
  config.yaml       its name on servers and its environments
  .env              local values and secrets, never committed
  mnt/<entry>/      data, bind-mounted as ./mnt/<entry>, one entry per concern
  commands/<name>   the project's own commands (## Description: line)
  capabilities/     env, status, shell, data: interfaces devopsy defines
  Dockerfile        in build mode, built on the server
```

A new project starts from a template, never from scratch: whoami (the
smallest: routing, certificates, releases), drupal11 (an app with a
database, built on the server, read-only at runtime), registry (a private
registry for image mode). They are GitHub template repositories under
https://github.com/hanoii/ (`devopsy-template-<name>`). An existing compose
project gets a `config.yaml` with `devopsy --init`. Sites route through the
server's own Traefik, devopsy-template-traefik, released once per server.

## compose.yaml

What devopsy requires:

- No `name:`. devopsy sets `COMPOSE_PROJECT_NAME` (locally `project:`,
  on servers `<project>[-<instance>]-<environment>`: each environment is
  its own compose project), which wins over a `name:`; leave it out so
  the file says what runs.
- Networks the project owns are named from its compose project
  (`${COMPOSE_PROJECT_NAME}_default`). To reach another project, join its
  network as `external` (the proxy's `traefik`); never a third, shared
  name: a release refuses a network another project owns.
- Data lives in `mnt` entries: `./mnt/<entry>` bind mounts, one per
  concern (`storage`, `database`, `logs`), never `./mnt` whole. On servers
  `mnt` is a link to the environment's `shared/mnt`, so releases and
  rollbacks keep the data. A service that writes an entry says who owns
  it, `devopsy.mnt.owner=<uid>:<gid>` (numbers; a variable keeps it in
  step with `user:`): devopsy creates the entry and gives it to the
  service on every release, so no container needs root to start. A
  release refuses, before anything goes live, a writable bind mount from
  the release's tree outside `mnt`, `./mnt` mounted whole and writable, an
  entry two services own differently, and a bind below an owned entry.
- Data labels, per service: `devopsy.data=true` says it has data
  (unlabeled or `false`: none); `devopsy.data.copy` how a copy carries it
  (`volumes`, the default: its `mnt` entries and named volumes as files;
  `custom`: the project's `data` capability exports and imports it, for a
  database dump; `none`: never copied); `devopsy.data.stop` when it is
  stopped (`own` while its own data moves, `import` while any data is
  imported, `always`, `never`, the default); `devopsy.data.exclude=<entry>,...`
  leaves entries out. They drive syncs from a parent environment,
  `--export` and `--import`; a snapshot takes every service with data,
  whatever its copy label, and stops them all for the copy. A database
  stops for a raw copy (`own`), an app while data is imported (`import`).
- Facts between projects: `devopsy.role=<name>` (one holder per host),
  `devopsy.export.<KEY>=<value>` on the running containers (visible to
  anyone with Docker access: never a secret), and
  `devopsy.import.<VAR>=<role or project>/<KEY>[?]`, copied into each
  release before it goes live (`?`: optional). Sites import the proxy's
  base domain: `devopsy.import.PROXY_BASE_DOMAIN=proxy/BASE_DOMAIN`.
- `devopsy.shell=true` names the service `--shell` opens, and
  `devopsy.shell.user` as whom.
- Images with data only go forward: a service with `devopsy.data=true`
  whose image is pulled is held by digest on the newest image its data
  ran; a series bump is `--release --accept-image <service>`, never
  `--yes` (which only skips prompts).

What the templates do, and new projects should: `user: "10001:10001"` (a
fixed non-root UID shared by all projects; never 1000, the deploy user, in
the docker group), `read_only: true` with tmpfs or an `mnt` entry for what
is written, `cap_drop: [ALL]`, `no-new-privileges`. Third-party images
float on a series (`mariadb:11.8`), never `latest`; in image mode the
project's own images are tagged with the commit
(`${DEVOPSY_RELEASE_COMMIT}`), never a fixed tag.

A site's routing is the project's, through labels on the `traefik`
network, as in whoami's compose.yaml: `traefik.enable=true`, a router rule
`${SITE_HOST_RULE:-...}` computed by the `env` capability from
`COMPOSE_PROJECT_NAME`, the imported `PROXY_BASE_DOMAIN` and the target's
`PROXY_DOMAINS`, and a certificate resolver only when the target sets
`PROXY_CERTRESOLVER` (`${PROXY_CERTRESOLVER:+traefik...certresolver=$PROXY_CERTRESOLVER}`).
Copy that block rather than writing routing anew. `PROXY_DOMAINS` holds
literal names, never a wildcard.

## Dockerfile

Build mode (the default) uploads the project and builds on the server, so
no registry is needed; image mode uploads `.devopsy/` only and pulls.
Either way the build or pull happens before the release goes live. An
image runs as its UID from the first process: `USER 10001`, files it
writes owned by it, no setuid binaries it does not need. Writable paths
are `mnt` entries or tmpfs. devopsy-template-drupal11's Dockerfile is the
reference.

## config.yaml

```yaml
project: shop                   # its name on servers: directories, compose names, URLs
secrets: [DB_PASSWORD, "*_TOKEN"]   # every secret, by name: masked, and never asked about
defaults:
  mode: build                   # or image
  release: {prepare: secrets, run: deploy}   # project commands: secrets before it goes live, deploy once current
  rollback: {prepare: secrets, run: deploy}
environments:
  prod:
    inherit: [DB_PASSWORD]      # what children get from prod
  staging: {}
  "pr-*":                       # a pattern: @pr-12
    parent: prod                # a child: its first release syncs prod's data
    releases: {keep: 1}
```

Servers and instances are never in it: the target names them. Release
steps run in phases (`before`, `prepare` on the server in the new
release, `run` once it is current, `after`), each a devopsy command line;
`destroy:` runs before `--destroy` while everything is up. Secrets are
generated in a `prepare` step (the templates' `secrets` command) so the
check of compose's required variables finds them; a value still missing
fails the release before the switch. A name that looks secret
(`PASSWORD`, `TOKEN`, `KEY`...) and is in neither `secrets:` nor
`public:` makes a release ask, so list every secret before releasing.
devopsy asks only at a terminal (`--destroy`, `--sync`, `--reset`, that
name): without one it fails and says to add `--yes`. Variables precedence, highest
first: the caller, the environment's `shared/.env` on the server, a
child's inherited values, the instance's and project's `.env` there, then
what `config.yaml` sets.

## Operating

- Locally, `devopsy up -d` and the rest of compose; `devopsy --release`
  runs the release steps in place (prepare, images, run) and
  `devopsy --destroy` takes it all down, `mnt` emptied.
- A target is `@[<server>:][<instance>/]<environment>`: `@vm1:prod`,
  `@prod` with `DEVOPSY_SERVER` in `.devopsy/.env` or CI's environment,
  or an alias from `~/.config/devopsy/config.yaml`.
- `devopsy @t --release` uploads a release, builds or pulls, switches
  `current` and runs the run step; a failure before the switch changes
  nothing live, one after switches back. `--rollback` goes back one;
  `--releases` lists; `--log` shows what the last one printed.
- A child (`parent:`) syncs its parent's data on its first release;
  `--sync` again; `--reset` wipes it and syncs anew. It gets the keys
  its parent's `inherit:` names, and, from a parent on another server,
  a copy of that server's instance and project variables
  (`shared/parent.env`): `--vars-project` and `--vars-instance` are
  per server.
  `devopsy @prod --export <service> | devopsy @staging --import <service>`
  moves one service's data; the same into a local project with
  `devopsy --import`.
- `--status`, `--shell [service] [-- cmd]`, `--vars [get|set|unset KEY]`
  (`get`: the value the environment gets and where it comes from; the
  list, `set` and `unset`: its `shared/.env`, warning of what a value
  overrides or what overrides it), `--env` (what devopsy loads).
- CI releases with a restricted key: `devopsy --ssh-keys-generate`, then
  `devopsy @t --ssh-keys add <key>.pub --restricted`; the job loads the
  private key into an agent and runs `devopsy @t --release`.
- Domains and certificates belong to the server's Traefik:
  `devopsy @<server's traefik> domains <compose project>` reports DNS,
  certificates and what to do next, and prints the `devopsy --probe`
  line that checks the same locally. A custom domain is a CNAME to the
  environment's wildcard URL.
- A server older than the operator's devopsy refuses before anything
  changes: `devopsy @<server> --upgrade`.
