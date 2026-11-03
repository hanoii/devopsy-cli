# Capabilities

A capability is an interface devopsy defines and a project implements, as
executables in `.devopsy/capabilities/<name>/<action>`. Only devopsy runs
them, in the project's directory and with its environment. `devopsy
--debug capabilities` lists them with their contracts.

## status

`show` prints the project's own lines under `devopsy --status`, after the
containers: its URL, a maintenance mode, a version, one fact per line,
with the project's environment loaded (the drupal11 template has one). It
runs locally and on servers: `devopsy @prod --status` runs it in the
current release there, after the release's record, the parent and the
last sync.

## env

`compute` runs before every devopsy command in the project but help and
completion, locally and on servers, and prints `.env` lines. devopsy sets
those not set yet, at the lowest precedence, and `--env` shows them.

It is for values a project derives from others, like the site templates'
hosts and Traefik rule, from `COMPOSE_PROJECT_NAME`, the imported
`PROXY_BASE_DOMAIN` and their own `PROXY_DOMAINS`. It runs every time, so
keep it fast; a devopsy it calls skips it. A failure stops the command.

## shell

`open [service] [exec options...] [-- command...]` replaces devopsy's own
`--shell`, locally and on servers.

## data

The transport of the services copied as `devopsy.data.copy=custom`, in
syncs, `--export` and `--import`, and hooks into syncs
([Parent environments and syncs](syncs.md)).

Which operation runs which action:

| operation | actions |
| --- | --- |
| `--export` | `export` |
| `--import` | `import` |
| `--import --resolved` | none |
| a sync | `prepare`, `export`, `import`, `finish` |

`prepare` and `finish` run only when their file is there, besides
devopsy's own handling of the labels. `export` and `import` are required
on a side where a service is `custom`, checked before anything stops.
Hooks run in the foreground: what one leaves running in the background
ends when it returns.

### `prepare parent|child`

Runs on each side, in its current release, before anything is stopped
there: the child's under its release lock, before the parent's side
starts; the parent's under a shared hold of its release lock, and only
when there is something to copy. A failure aborts the sync, and `finish
abort` runs.

### `export <service>`

Writes a `custom` service's data to stdout, with stdin empty:

- in a sync, on the parent's server, once per `custom` service in
  `depends_on` order, with what the sync stops there stopped. Where the
  data clones by reflinks and nothing stopped is `custom` or `always`, it
  runs after the services start again from the clone, with
  `DEVOPSY_DATA_STOP` empty then.
- in `devopsy --export`.

devopsy spools what it writes, and imports it only once every export
exited 0, the parent's services started again first. A service in
`DEVOPSY_DATA_STOP` is stopped: reach it with `devopsy run --rm --no-deps
-T <service> ...`, not `exec`, or refuse.

### `import <service>`

Reads that data from stdin, complete: on the child's server in a sync,
and in `devopsy --import`. The service is up, devopsy bringing it up
alone first, unless it is in `DEVOPSY_DATA_STOP`.

### `finish parent|child go|abort|partial`

Runs on each side once the sync has ended.

The parent's gets `go` or `abort`, as the child's does, by what the copy
did: `go` once it completed, even when a service of the parent's did not
come back (that fails the command, said). The parent's data is never
written, and its stopped services are started again.

The child's says what the copy did to its data:

- `go`: complete;
- `abort`: nothing of the parent's written, on data that was complete;
- `partial`: written in part, or nothing written onto data an earlier
  sync, an import or a restore left incomplete
  ([When a sync fails](syncs.md#when-a-sync-fails)).

After `go` and `abort`, devopsy has started its stopped services and
waited for them to be healthy. After `partial` they stay down, and the
hook must not start them.

On the child, `finish child go` is where a copy of production is
sanitized (emails and passwords scrubbed, a login set for a child that had
none: devopsy-template-drupal11 does both); then the release's `run` step
runs.

- A failing `finish child go` fails the sync, and nothing runs on the
  child's data until a sync completes: the hook decides what stays up
  (drupal11's stops the app).
- devopsy starts again only what a sync stopped, so what the hook stopped
  is still down when the next sync's `finish child go` runs: the hook
  starts what it needs.
- On a child's first release nothing existed yet, so devopsy first
  creates its services with data and its side's set (`up -d --wait`), as
  the run step's `up` would a moment later: the hook finds them running
  either way.
- `finish child partial` also runs outside a sync, when a child's side
  was killed before deciding: in whatever takes its lock next, a sync, a
  release or a rollback (the last two then refuse).

### Variables

Every action gets its side's services, space separated, by how a copy
carries them:

- `DEVOPSY_DATA_VOLUMES`;
- `DEVOPSY_DATA_CUSTOM`, in `depends_on` order;
- `DEVOPSY_DATA_STOP`: what the operation stops on that side by the
  labels (back up by `finish ... go|abort`; at `finish child partial`,
  what stays down). A stopped `custom` service is in both.

In a sync they also get `DEVOPSY_SYNC_FROM_TARGET` and
`DEVOPSY_SYNC_TO_TARGET`, the parent's and the child's addresses. The
other `DEVOPSY_SYNC_*` variables a push sets are devopsy's own.

### A database copied as `custom`

```sh
# capabilities/data/export: $1 is the service
exec devopsy exec -T "$1" sh -c 'mariadb-dump --single-transaction ...' </dev/null
# capabilities/data/import: the dump on stdin, complete
exec devopsy exec -T "$1" sh -c 'mariadb ...'
```

Each side's database credentials stay its own: the dump runs with the
parent's, the import with the child's. Whether the export exited 0 is
devopsy's to check, not the scripts'. A script whose service must run
refuses when the service is in `DEVOPSY_DATA_STOP`.
