# Config and targets

What a project says about itself in `.devopsy/config.yaml`, how an address
names an environment on a server, and where an environment's variables
come from.

## `.devopsy/config.yaml`

`devopsy --init` writes one. It asks for the project's name, and leaves
an existing file alone. `devopsy --debug schema` documents every key.

```yaml
project: shop                   # its name on servers: required
instances: true                 # optional: every target names an instance; default false: none does
releases: {keep: 5}             # optional: releases kept per environment (live ones)
secrets: [DB_PASSWORD, "*_TOKEN"]  # optional: variables whose values devopsy masks
public: [SORT_KEY]              # optional: secret-like names that are not secrets
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

Commit it: it holds no secret values and no servers. Unknown keys are
errors.

How the keys combine:

- Each environment takes `defaults:` and changes what it sets itself. In
  `env`, `""` sets an empty value and `~` removes a default; `inherit: ~`
  removes the list. `snapshots:` merges key by key: `true` and `false`
  only turn a kind on or off, `{keep: <n>}` only changes the count, and
  `~` goes back to the default.
- An environment key with `*` is a pattern, matching one or more name
  characters (`"*"` matches any name). An exact name wins over a pattern,
  then the pattern with the most literal characters.
- `.devopsy/config.local.yaml`, never committed or uploaded, is read over
  `config.yaml` on one machine. Its `defaults:` and each of its
  environments merge over the project's of the same name, the way
  `defaults:` merge into environments, so it changes a key and keeps the
  rest; `parent: ~` there removes a parent.

## Targets

```
devopsy @[<server>:][<instance>/]<environment> ...

@vm1:prod                     on vm1: an SSH destination or ~/.ssh/config alias
@vm1:b/prod                   instance b of the project, on vm1
@devopsy@vm1:5369:prod        as devopsy, on port 5369
@2001:db8::1:prod             an IPv6 address
@'[2001:db8::1]:5369:prod'    an IPv6 address with a port, in brackets
@prod                         server and instance from variables
```

The server ends at the last `:`, since the instance and environment
have none. An IPv6 address with a port needs its brackets, and zsh needs
them quoted, as the commands devopsy suggests write them.

What the address leaves out comes from your environment or the project's
`.devopsy/.env`, the most specific first:

```sh
DEVOPSY_SERVER_PR_12=...       # one environment's server (upper case, others as _)
DEVOPSY_SERVER_PR=...          # a pattern's: pr-* without the *
DEVOPSY_SERVER=vm1             # any environment's
DEVOPSY_INSTANCE=b
```

No server from any of them is an error. A server in a variable takes
the same forms, a port included (`DEVOPSY_SERVER=devopsy@vm1:5369`).

## Instances

Instances are opt in. In a project with `instances: true`, every target
names one, by the address, `DEVOPSY_INSTANCE` or an alias's `to:`. Any
other project refuses them, so a project's environments on a server all
have instances, or none do.

The project does not list its instances, and any name works (lowercase,
digits, `-`). The first release of an instance the server lacks asks
before creating it (`--release --yes` in CI).

## Where an environment lives

On the server an environment lives in
`<project>/[<instance>/]<environment>` under the server's release root
(Release settings, below). Its compose project, containers and volumes are
named `<project>[-<instance>]-<environment>`. That name is its
`COMPOSE_PROJECT_NAME`, which the templates build their URLs from.

## Parents

A child environment names its parent with `parent:`, a concrete
environment of the project other than itself. It is set on an
environment or a pattern, and not under `defaults:`. A child gets its
parent's data, including secrets and personal data, so the config
shows this where the child is defined.

The parent resolves like a target of its own, one level only: at the
child's instance, on the server its own variables name
(`DEVOPSY_SERVER_<PARENT>`, `DEVOPSY_SERVER`), not the child's server.
A guess could copy the wrong data, so a sync without one fails and says
which variable to set. `@vm2:develop` with `DEVOPSY_SERVER_PROD=vm1` syncs
from `vm1:prod`. Without the variable, an address does it for one
command: `devopsy @vm2:develop --release --from @vm1:prod`, and likewise
`--release-sync --from` and `--sync --from`.

Parents can chain (`pr-*` from `staging` from `prod`), but cannot form a
cycle. Two environments that were each other's parent could never be
released, since a child's first release syncs from a released parent.
`devopsy --debug environments` shows each parent resolved. What a sync
copies is in [Parent environments and syncs](syncs.md).

## Variables on a server

An environment's variables on a server come from these files, the nearest
first:

1. the environment's `shared/.env`;
2. a child's `shared/inherit.env`, what its parent's `inherit:` gave it;
3. the instance's `.env` (`<project>/<instance>/.env`);
4. the project's `.env` (`<project>/.env`);
5. a child's `shared/parent.env`, its copy of its parent's server's
   instance and project `.env`, when the parent is on another server;
6. the release's `devopsy-target.env`, written from the config, with
   `COMPOSE_PROJECT_NAME`, `DEVOPSY_PROJECT`, `DEVOPSY_INSTANCE`,
   `DEVOPSY_ENVIRONMENT`, `DEVOPSY_TARGET` and `DEVOPSY_RELEASE_COMMIT`
   (and a child's `DEVOPSY_PARENT` and `DEVOPSY_PARENT_TARGET`).

A rollback brings back its release's `devopsy-target.env`.

```sh
devopsy @vm1:prod --vars                       # KEY=value, secrets as *** (--show: all)
devopsy @vm1:prod --vars set DB_PASSWORD       # from your environment or .env, else a prompt or stdin
devopsy @vm1:prod --vars set --show LOG_LEVEL  # a visible prompt
devopsy @vm1:prod --vars get DB_PASSWORD       # whatever defines it, saying where
devopsy @vm1:prod --vars unset API_KEY
devopsy @vm1:b/prod --vars-instance set SITE_ID   # the instance's .env
devopsy @vm1 --vars-project set SHARED_KEY        # the project's .env on vm1
```

`--vars` lists the environment's own file. `--vars get` prints on stdout
the value the environment gets, wherever it is defined (inherited, the
instance's or the project's, from `devopsy-target.env`, imported or
computed, as `--env` shows it), and says where it comes from.

An edit changes only its flag's file, and devopsy says how the other
files affect it:

- When a set overrides a value from another file (the environment's own
  over an inherited value, the instance's over the project's), devopsy
  says so and gives the `unset` that brings the other value back.
- When a nearer file hides a set (the project's, under an environment's
  own value and its inherited one), devopsy names the files that hide it,
  for each environment it reaches. When an inherited value hides it, the
  warning gives the `--vars set` that gives the environment its own value,
  and how to drop the inherited one: remove it from the parent's
  `inherit:`, release the parent, then run `--inherit`.
- An `unset` says where the key's value now comes from, if it still has
  one.

Each server has its own instance and project `.env`, so two servers of one
project have separate files. `--vars-instance` and `--vars-project` edit
them through any environment on that server, or through the server alone
for the project's.

## Applying changes

Values travel over SSH's stdin, never as arguments. Running containers see
a change only once they are recreated. `--vars set` and `--vars unset` on
an environment always make the change, and say what applies it:

- `--reload` ([Reloads](releases.md#reloads));
- a release, when the variables now give a service another image than
  the release fetched;
- nothing yet, while a variable compose requires is unset, or while data
  a sync, an import or a restore did not complete is not settled.

## Release settings

Release settings belong to the server, in its deploy user's
`~/.config/devopsy/config.yaml`, so your machine and CI always agree:

```yaml
releases:
  root: /srv     # where releases live, never /; default: the deploy user's home
  keep: 2        # default for its projects (5)
  max_keep: 5    # no project keeps more (5)
snapshots:
  root: /mnt/backups   # where snapshots (and mirrors) go; default: each environment's snapshots/
  max_keep: 5          # no environment keeps more manual snapshots (5)
  mirror: false        # no environment keeps a mirror here, whatever it asks (default: each decides)
firewall:
  metadata: block      # containers kept off the cloud's metadata services (block, the default), or not (allow)
```

`firewall: metadata:` is the server's, applied by its next
`--server-setup` ([Servers](servers.md#--server-setup)): `block` keeps
containers on Docker's bridges (`docker0`, `br-*`) off the cloud's
metadata services, never one on the host's network, on macvlan or
ipvlan, or on a bridge named otherwise; `allow` lets them reach them,
and is refused where `door:` is set or the door's firewall is loaded,
since project users' containers never do.

The release root must be a directory of the deploy user's: a release
refuses one that is missing, or root's and empty, as `/srv` is on a new
server, until `devopsy @<server> --server-setup` makes it or gives it to
the deploy user, and one root owns that holds files, one another user
owns or one that is no directory until `root:` names another
([Servers](servers.md#--server-setup)).

To move a server's release root:

1. stop each environment (`devopsy down` in its `current`);
2. `mv` the projects' directories under the new root, a directory of the
   deploy user's;
3. change `root:`;
4. run `--server-setup` again, which makes Docker wait at boot for the
   new root's mounts instead of the old one's;
5. start each environment again (its `deploy`, or a release).

Containers keep the paths their bind mounts had, so they must be created
again; everything inside the directories is linked relatively.

Changing a server's `snapshots: root` leaves the snapshots taken before
where they are, unseen by `--snapshots`, pruning and `--destroy`. Move
each environment's snapshot directory under the new root first; their
images' bookmarks go with them.

## Aliases

Your `~/.config/devopsy/config.yaml` (or `$DEVOPSY_HOME/config.yaml`) can
name targets you use from anywhere:

```yaml
aliases:
  vm1-traefik: {source: ~/src/devopsy-template-traefik, to: "vm1:prod"}
  shop-demo:   {source: ~/src/shop, to: "vm1:demo/prod"}
```

`source:` is the project's checkout: the alias uses its config, and
releases only from that directory. Without `source:`, `project:` names
the project, and the alias works for commands only.

Inside a project, an environment of the same name wins over an alias. An
alias does not read a project's `.env`, and its instance comes from its
`to:`, not from `DEVOPSY_INSTANCE`. With `project:`, a `to:` that names an
instance marks the project as having instances.
