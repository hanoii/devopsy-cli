# Config and targets

What a project says about itself in `.devopsy/config.yaml`, how an address names an environment on a server, and where an environment's variables come from.

## `.devopsy/config.yaml`

`devopsy --init` writes one (it asks for the project's name and never
touches an existing file); `devopsy --debug schema` documents every key.

```yaml
project: shop                   # its name on servers: required
instances: true                 # optional: every target names an instance; default false: none does
releases: {keep: 5}             # optional: releases kept per environment (live ones)
secrets: [DB_PASSWORD, "*_TOKEN"]  # optional: variables whose values devopsy masks
public: [SORT_KEY]              # optional: named like secrets, but not
defaults:                       # what every environment takes unless it sets its own (never parent:)
  mode: image                   # build (default) or image
  release: deploy               # what --release runs: required for it
  rollback: deploy              # what --rollback runs, as the restored release's config says: required there
  destroy: backup               # optional: what --destroy runs first, while everything is up
  reload: deploy                # optional: what --reload runs; default: up -d --wait --no-build --pull never
  env: {PROXY_CERTRESOLVER: le-dns01-selfhosted}  # per-environment variables, not secrets
  snapshots: {manual: {keep: 3}, release: false, mirror: false}  # optional: manual ones kept (on, 3), release snapshots (off), a mirror (off)
environments:
  prod:
    env:
      PROXY_DOMAINS: example.org www.example.org
    inherit: [DATA_DATABASE]   # variables its children get from its server
  staging: {}
  develop:
    parent: prod                # a child: its first release syncs prod's data
  "pr-*":                       # a pattern: @pr-12, @pr-feature
    releases: {keep: 1}
    snapshots: {manual: false}  # no --snapshot here
    parent: prod
```

Commit it: no secret values, no servers. `.devopsy/config.local.yaml` (not
committed, never uploaded) is read over it for one machine: its `defaults:`
and each environment merge over the project's of the same name as
`defaults:` merge into environments, so it changes a key and keeps the
rest, and `parent: ~` there removes a parent. In `env`, `""`
sets an empty value and `~` removes a default; `inherit: ~` removes its
list. `snapshots:` merges key by key too: `true` and `false` only
turn a kind on or off, `{keep: <n>}` only counts, `~` goes back to the
default. Patterns match one or
more name characters; an exact name wins, then the most literal characters;
`"*"` allows any. Unknown keys are errors.

## Targets

```
devopsy @[<server>:][<instance>/]<environment> ...

@vm1:prod                     on vm1: an SSH destination or ~/.ssh/config alias, no ":"
@vm1:b/prod                   instance b of the project, on vm1
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

A missing server is an error.

## Instances

Instances are opt in: a project with
`instances: true` has them, and every target names one (the address,
`DEVOPSY_INSTANCE` or an alias's `to:`); any other refuses them, so one
project's environments never mix both layouts on a server. They are never
listed in the project: any name works (lowercase, digits, `-`), and the
first release of one the server lacks asks before creating it (`--release
--yes` in CI).

## Where an environment lives

On the server, an environment lives in `<project>/[<instance>/]<environment>`
under the server's release root, and its compose project, containers,
volumes are named `<project>[-<instance>]-<environment>`
(`COMPOSE_PROJECT_NAME`, from which templates build their URLs).

## Parents

A child's parent (`parent:`, a concrete environment of the project,
never itself) is named per environment, a pattern included, never under
`defaults:`: a child gets its parent's data, secrets and personal data
included, so that shows where the child is defined. It
resolves like a target of its own, one level only: at the child's
instance, on the server its variables name (`DEVOPSY_SERVER_<PARENT>`,
`DEVOPSY_SERVER`), never the child's: a guess could copy the wrong data,
so a sync without one fails and says which variable to set. `@vm2:develop`
with `DEVOPSY_SERVER_PROD=vm1` syncs from `vm1:prod`. Without the
variable, an address does it for one command: `devopsy @vm2:develop
--release --from @vm1:prod`, or `--release-sync --from`, `--sync --from`.
Parents can chain (`pr-*` from `staging` from `prod`), never in a cycle:
two environments that are each other's parent could never be released,
since a child's first release syncs from a released parent. `devopsy
--debug environments` shows it resolved.

## Variables on a server

A server's variables, nearest first: the environment's `shared/.env`,
a child's `shared/inherit.env` (what its parent's `inherit:` gave it),
the instance's `.env` (`<project>/<instance>/.env`), the project's
(`<project>/.env`), a child's `shared/parent.env` (its copy of its
parent's server's instance and project `.env`, when that is another
server), then the release's `devopsy-target.env`, written from the
config with `COMPOSE_PROJECT_NAME`, `DEVOPSY_PROJECT`, `DEVOPSY_INSTANCE`,
`DEVOPSY_ENVIRONMENT`, `DEVOPSY_TARGET` and `DEVOPSY_RELEASE_COMMIT` (and a
child's `DEVOPSY_PARENT` and `DEVOPSY_PARENT_TARGET`).
Rollbacks bring back their release's `devopsy-target.env`.

```sh
devopsy @vm1:prod --vars                       # KEY=value, secrets as *** (--show: all)
devopsy @vm1:prod --vars set DB_PASSWORD       # from your environment or .env, else a prompt or stdin
devopsy @vm1:prod --vars set --show LOG_LEVEL  # a visible prompt
devopsy @vm1:prod --vars get DB_PASSWORD       # whatever defines it, saying where
devopsy @vm1:prod --vars unset API_KEY
devopsy @vm1:b/prod --vars-instance set SITE_ID   # the instance's .env
devopsy @vm1 --vars-project set SHARED_KEY        # the project's .env on vm1
```

`--vars` lists the environment's own file, and `get` gives the value the
environment gets (inherited, the instance's or the project's, from
`devopsy-target.env`, imported or computed: what `--env` shows), on
stdout, saying where it comes from. Edits stay in their flag's file:
a set over another value warns that it overrides it now (the
environment's own over an inherited value, the instance's over the
project's), and a set that a nearer file hides warns, for each
environment it reaches, which files do (the project's under an
environment's own and its inherited one). The first comes with the
`unset` that brings the other value back; the second, when an inherited
value hides it, with the `--vars set` that gives the environment its
own and how the inherited value goes (dropped from the parent's
`inherit:`, released, then `--inherit`). An `unset` says the value left, if any.

The instance's and the project's `.env` are that server's: two servers
of one project each have their own, which `--vars-instance` and
`--vars-project` edit through any of its environments (or the server
alone, for the project's).

## Applying changes

Values travel over SSH's stdin, never as arguments. Running containers see
changes when recreated: `--vars set` and `unset` on an environment say
what applies them, never refusing one: `--reload`; a release, when the
variables now give a service another image than the release fetched;
nothing until a variable compose requires is set again, or until data
a sync, an import or a restore did not complete is settled.

## Release settings

Release settings belong to the server, in its deploy user's
`~/.config/devopsy/config.yaml`, so your machine and CI always agree:

```yaml
releases:
  root: /srv     # where releases live; default: the deploy user's home
  keep: 2        # default for its projects (5)
  max_keep: 5    # no project keeps more (5)
snapshots:
  root: /mnt/backups   # where snapshots (and mirrors) go; default: each environment's snapshots/
  max_keep: 5          # no environment keeps more manual snapshots (5)
  mirror: false        # no environment keeps a mirror here, whatever it asks (default: each decides)
```

Moving a server's release root: stop each environment (`devopsy down`
in its `current`), `mv` the projects' directories under the new root,
change `root:`, then start each again (its `deploy`, or a release).
Containers keep the paths their bind mounts had, so they must be made
again; everything in the directories is linked relatively.

Changing a server's `snapshots: root` leaves the snapshots taken before
where they are, unseen by `--snapshots`, pruning and `--destroy`: move
each environment's directory under the new root first (their images'
bookmarks go with them).

## Aliases

Your `~/.config/devopsy/config.yaml` (or `$DEVOPSY_HOME/config.yaml`) can
name targets you use from anywhere:

```yaml
aliases:
  vm1-traefik: {source: ~/src/devopsy-template-traefik, to: "vm1:prod"}
  shop-demo:   {source: ~/src/shop, to: "vm1:demo/prod"}
```

`source:` is the project's checkout: its config, and the only directory the
alias releases from. Without it, `project:` names the project, for commands
only. Inside a project its own environment of the same name wins; an alias
never reads a project's `.env`, and its instance is its `to:`'s, never
`DEVOPSY_INSTANCE`'s. With `project:`, a `to:` naming an instance is how
it knows the project has them.
