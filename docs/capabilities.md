# Capabilities

Interfaces devopsy defines and a project implements, as executables in
`.devopsy/capabilities/<name>/<action>`, run only by devopsy, in the
project's directory with its environment (`devopsy --debug capabilities`
lists them with their contracts):

## status

`show` prints the project's own lines under `devopsy
--status`, after the containers: its URL, a maintenance mode, a version,
one fact per line, with the project's environment loaded (the drupal11
template's). Locally and on servers (`devopsy @prod --status` runs it
in the current release there, after the release's record, the parent
and the last sync).

## env

`compute` runs before every devopsy command in the project (not
help or completion), locally and on servers, and prints `.env` lines;
devopsy sets those not set yet, at the lowest precedence, and `--env`
shows them. For values a project derives from others, like the site
templates' hosts and Traefik rule from `COMPOSE_PROJECT_NAME`, the
imported `PROXY_BASE_DOMAIN` and their own `PROXY_DOMAINS`. It
runs every time, so keep it fast; a devopsy it calls skips it. A failure
stops the command.

## shell

`open [service] [exec options...] [-- command...]` replaces
devopsy's own `--shell`, locally and on servers.

## data

The transport of the services copied as
`devopsy.data.copy=custom`,
in syncs and `--export` and `--import`, and hooks into syncs ([Parent
environments and syncs](syncs.md)). Which operation runs which action: `--export`
runs `export`, `--import` runs `import`, `--import --resolved` none; a
sync runs all four, `prepare` and `finish` only there. `prepare` and
`finish` run only when their file is there, besides devopsy's own
handling of the labels; `export` and `import` are required on their side
when a service is `custom` there, checked before anything stops. Hooks
run in the foreground: what one leaves running in the background ends
when it returns.
- `prepare parent|child` runs on each side, in its current release,
  before anything is stopped there: the child's under its release lock,
  before the parent's side starts; the parent's under a shared hold of
  its lock, and only when there is something to copy. A failure aborts
  the sync (`finish abort` runs).
- `export <service>` writes a `custom` service's data to stdout: on the
  parent's server, once per `custom` service in `depends_on` order,
  with what the sync stops there stopped (where the data clones by
  reflinks and nothing stopped is `custom` or `always`, after they
  start again from the clone, `DEVOPSY_DATA_STOP` then empty); and
  `devopsy --export`. stdin is empty. devopsy spools what it writes and
  imports it only once every export exited 0, the parent's services
  started again first. A service in `DEVOPSY_DATA_STOP` is stopped:
  reach it with `devopsy run --rm --no-deps -T <service> ...`, not
  `exec`, or refuse.
- `import <service>` reads that data from stdin, complete: on the
  child's server and in `devopsy --import`, its service up (devopsy
  brings it up alone first) unless it is in `DEVOPSY_DATA_STOP`.
- `finish parent|child go|abort|partial` runs on each side once the sync
  ended. The parent's gets `go` or `abort`, what the copy did, as the
  child's: `go` once it completed, even when a service of the parent's
  did not come back (that fails the command, said). Its data is never
  written, and its stopped services are started again. The child's says what the copy did
  to its data: `go`, complete; `abort`, nothing of the parent's written,
  on data that was complete; `partial`, written in part, or nothing
  written onto data an earlier sync, an import or a restore left
  incomplete ([When a sync fails](syncs.md#when-a-sync-fails)). After `go` and
  `abort` devopsy has started its stopped services and waited for them
  to be healthy; after `partial` they stay down, and the hook must not
  start them. On the child after `go` it is where a copy of production
  is sanitized (emails and passwords scrubbed, a login set for a child
  that had none: devopsy-template-drupal11's); then the release's `run`
  step runs. A failing `finish child go` fails the sync, and nothing
  runs on the child's data until a sync completes: the hook decides what
  stays up (drupal11's stops the app). devopsy restarts only what a sync
  stopped, so what the hook stopped is still down when the next sync's
  `finish child go` runs: the hook starts what it needs. On a child's first release
  nothing of it existed, so devopsy creates its services with data and
  its side's set first (`up -d --wait`), as the run step's `up` would a
  moment later: the hook finds them running either way. `finish child
  partial` also runs outside a sync, when a child's side was killed
  before deciding: in whatever takes its lock next, a sync, a release or
  a rollback (the last two then refuse).
- Every action gets its side's services by how a copy carries them,
  space separated: `DEVOPSY_DATA_VOLUMES`, `DEVOPSY_DATA_CUSTOM` (in
  `depends_on` order), and
  `DEVOPSY_DATA_STOP`, what the operation stops on its side by the
  labels (at `finish ... go|abort` back up by then; at `finish child
  partial`, what stays down); a stopped `custom` service is in both. In
  a sync also `DEVOPSY_SYNC_FROM_TARGET` and `DEVOPSY_SYNC_TO_TARGET`
  (the parent's and the child's addresses). The other `DEVOPSY_SYNC_*` variables
  a push sets are devopsy's own.

A database copied as `custom`, in short:

```sh
# capabilities/data/export: $1 is the service
exec devopsy exec -T "$1" sh -c 'mariadb-dump --single-transaction ...' </dev/null
# capabilities/data/import: the dump on stdin, complete
exec devopsy exec -T "$1" sh -c 'mariadb ...'
```

Each side's database credentials stay its own: the dump runs with the
parent's, the import with the child's. Whether the export exited 0 is
devopsy's to check, not the scripts'. A script whose service must run
refuses when it is in `DEVOPSY_DATA_STOP`.
