# Capabilities

A capability is an interface devopsy defines and a project implements, as
executables in `.devopsy/capabilities/<name>/<action>`. Only devopsy runs
them, in the project's directory and with its environment. `devopsy
--debug capabilities` lists them with their contracts.

## status

`show` prints the project's own lines under `devopsy --status`, after the
containers: its URL, a maintenance mode, a version, one fact per line. It
runs with the project's environment loaded (the drupal11 template has
one), locally and on servers: `devopsy @prod --status` runs it in the
current release there, after the release's record, the parent and the
last sync.

## env

`compute` prints `.env` lines. It runs before every devopsy command in the
project except help and completion, locally and on servers. devopsy sets
the variables it prints that are not set yet, at the lowest precedence,
and `--env` shows them.

It is for values a project derives from others. The site templates, for
example, derive their hosts and Traefik rule from `COMPOSE_PROJECT_NAME`,
the imported `PROXY_BASE_DOMAIN` and their own `PROXY_DOMAINS`. It runs
every time, so keep it fast; a devopsy it calls skips it. A failure stops
the command.

## shell

`open [service] [exec options...] [-- command...]` replaces devopsy's own
`--shell`, locally and on servers.

## data

The data capability carries the data of services copied as
`devopsy.data.copy=custom`, in syncs, `--export` and `--import`. It also
has hooks into syncs ([Parent environments and syncs](syncs.md)).

Which operation runs which action:

| operation | actions |
| --- | --- |
| `--export` | `export` |
| `--import` | `import` |
| `--import --resolved` | none |
| a sync | `prepare`, `export`, `import`, `finish` |

`prepare` and `finish` run only when their file exists, in addition to
devopsy's own handling of the labels. `export` and `import` are required
on any side where a service is `custom`, and devopsy checks for them
before anything stops. Hooks run in the foreground: anything a hook
leaves running in the background ends when it returns.

### `prepare parent|child`

Runs on each side, in its current release, before anything is stopped
there. The child's runs under the child's release lock, before the
parent's side starts. The parent's runs under a shared hold of the
parent's release lock, and only when there is something to copy. If
either fails, the sync is aborted and `finish abort` runs.

### `export <service>`

Writes a `custom` service's data to stdout, with stdin empty. It runs:

- in a sync, on the parent's server, once for each `custom` service in
  `depends_on` order, while what the sync stops there is stopped. Where
  the data clones by reflinks and none of the stopped services is
  `custom` or `always`, it runs after the services have started again
  from the clone, and `DEVOPSY_DATA_STOP` is then empty.
- in `devopsy --export`.

In a sync, devopsy spools what the exports write. It imports nothing
until every export has exited 0, and it starts the parent's services
again before it imports. A service listed in `DEVOPSY_DATA_STOP` is
stopped ([Variables](#variables)).

### `import <service>`

Reads that data from stdin, complete: on the child's server in a sync,
and in `devopsy --import`. Unless the service is in `DEVOPSY_DATA_STOP`,
it is up when the import runs: devopsy first brings it up alone.

### `finish parent|child go|abort|partial`

Runs on each side once the sync has ended.

The parent's gets `go` or `abort`, like the child's, depending on what the
copy did. It gets `go` once the copy completed, even when one of the
parent's services did not come back, which fails the command with a
message. A sync never writes the parent's data, and the parent's stopped
services are started again.

The child's says what the copy did to the child's data:

- `go`: the copy is complete;
- `abort`: nothing of the parent's was written, and the data was complete
  before;
- `partial`: the data was written in part, or nothing was written onto
  data that an earlier sync, an import or a restore had left incomplete
  ([When a sync fails](syncs.md#when-a-sync-fails)).

By `go` and `abort`, devopsy has already started its stopped services
and waited for them to be healthy. After `partial` they stay down, and
the hook must not start them.

On the child, `finish child go` is where a copy of production is
sanitized: emails and passwords scrubbed, a login set for a child that
had none (devopsy-template-drupal11 does both). The release's `run` step
runs after it.

- If `finish child go` fails, the sync fails, and nothing runs on the
  child's data until a sync completes. The hook decides what stays up
  (drupal11's stops the app).
- devopsy starts again only what a sync stopped. Whatever the hook stopped
  is still down when the next sync's `finish child go` runs, so the hook
  starts what it needs.
- On a child's first release nothing exists yet, so devopsy first creates
  the services with data and the child side's stop set (`up -d --wait`),
  as the run step's `up` would a moment later. The hook finds them
  running on a first release too.
- `finish child partial` also runs outside a sync, when a child's side was
  killed before it could decide. It runs in the next operation that takes
  the lock, a sync, a release or a rollback (the last two then refuse).

### Variables

Every action gets its side's services, space separated, grouped by how a
copy carries them:

- `DEVOPSY_DATA_VOLUMES`;
- `DEVOPSY_DATA_CUSTOM`, in `depends_on` order;
- `DEVOPSY_DATA_STOP`: what the operation stops on that side by the
  labels. They are up again by `finish ... go|abort`, and at `finish
  child partial` they are what stays down. A stopped `custom` service is
  in both of the last two. A script reaches a stopped service with
  `devopsy run --rm --no-deps -T <service> ...` instead of `exec`, and a
  script whose service must be running refuses.

In a sync, the actions also get `DEVOPSY_SYNC_FROM_TARGET` and
`DEVOPSY_SYNC_TO_TARGET`, the parent's and the child's addresses. The
other `DEVOPSY_SYNC_*` variables a push sets are for devopsy's own use.

### A database copied as `custom`

```sh
# capabilities/data/export: $1 is the service
exec devopsy exec -T "$1" sh -c 'mariadb-dump --single-transaction ...' </dev/null
# capabilities/data/import: the dump on stdin, complete
exec devopsy exec -T "$1" sh -c 'mariadb ...'
```

Each side's database credentials stay its own: the dump runs with the
parent's, the import with the child's. devopsy checks that the export
exited 0, so the scripts need not.
