# Parent environments and syncs

A child (`develop: {parent: prod}`)
is a clone of its parent: its first release syncs the parent's data, and
`devopsy @<child> --sync` does it again later, then reruns the `run`
step of the child's deployed release (its own config, never the
checkout's). What moves is what the labels say:

- `volumes` services' entries go from the parent's `shared/mnt` into the
  child's: a file written in place, so the container mounting it sees
  the new content. Files a running service removes during the copy
  (rsync's "vanished", its exit 24) are a notice, not a failure: the
  rest is copied. An entry missing on the parent is skipped, said, and
  the child's is left as it is.
- `custom` services: the parent's `export <service>` goes into a file
  in the child's directory (`.devopsy-sync.stream.<service>.<sync id>`),
  and only once the export exited 0 does the child's `import <service>`
  read it, the child's service up (devopsy starts it, alone, unless it
  stopped it); the file goes afterwards. A cut export never reaches the
  import. One service after the other, in compose's `depends_on` order
  (dependencies first). A service the parent streams must be `custom` on
  the child too, and one the child streams must not be copied as files
  (`volumes`) on the parent, or the parent refuses before it stops
  anything. A label from an inherited
  variable (`devopsy.data.copy=${DATA_DATABASE:-volumes}`, `inherit:
  [DATA_DATABASE]`) follows the parent's in the same sync: change it on
  the parent, then `--sync`.

On the parent, the sync stops its `own` data services and its `always`
ones, with what depends on them; on the child, its `own`, `import` and
`always` ones, with what depends on them, for the whole sync. The child
stops its set before it knows what the parent copies: when the parent
has nothing to copy, its `import` services were down for a few seconds
for nothing. A stop word that differs between the sides (a child's own
`shared/.env`) is named.

A project without any service with data, or whose services have
nothing to copy, syncs nothing ("nothing to sync"): the parent's
`prepare` and `finish` do not run, and the child's `run` step still
runs. On a child whose data is from a sync that did not complete, it
completes nothing: the earlier copy's data is still there, and the sync
fails.

Each side reads the labels from its own release, so they can differ: a
variable in the label (`devopsy.data.copy=${DATA_DATABASE:-volumes}`) that the
parent inherits to its children keeps them alike. What the parent rsyncs
must not change under a running service of the child: before changing
anything of its own, the parent looks at the child's services and
refuses when one that mounts a copied entry is still running there,
copied as `custom` or `none`, or without data (one the child stopped is
not running; `volumes` on the child says a running copy is wanted). The child restores
its services, nothing was copied. A service the parent streams that the
child does not, or one the child streams that the parent copies as
files, is refused the same way (a label the child sets in its own
`shared/.env` can still differ).

## Order

Order on the child's server, under its release lock for the whole sync:
the capability's `prepare child`, its side's set stopped; then it waits
for the parent. On the parent's, under a shared hold of its release lock
(a release of the parent waits for the copy) and a sync lock of its own
(syncs from one parent run one at a time): `prepare parent`, its side's
set stopped, the rsync, each stream's export into the child's spool, its
services started, then the child's imports of the spools (an import
reads its spool, never the parent: the parent is not down through a
load), `finish parent go|abort`. Then the child's: its
services started, `finish child go`, and its `run` step. The child's
stopped services stay down through the imports, so an app never runs on
a half-imported database, and after a copy that did not complete too
(below). A child's first release creates, before `finish child go`, its
services with data and its side's set (an `import` app without data, an
unlabeled app depending on an `own` database), so the hook can sanitize
through them; one in a profile that is not up (`COMPOSE_PROFILES`) stays
uncreated, as the run step's `up` would leave it. A lock held by something else is announced ("waiting for the
release running on ..."), then waited for up to 10 minutes, like every
release lock.

## Where the parent's data clones by reflinks

Where the parent's data clones by reflinks (its entries and volumes
on an XFS or btrfs together with the environment's directory, as for
snapshots), what its side stops for the copy is down for a clone only,
seconds whatever the size: the parent's set stopped, its data cloned
into `.devopsy-sync.clone.<id>` beside it, its services started, and
the rsync made from the clone, which then goes (the same across
servers: the daemon serves the clone). Whether the data clones is tried
before anything stops. A `custom` service the parent's labels stop,
or any `custom` service while one labeled `always` is stopped, is
exported before they start, as the labels promise; otherwise after. With nothing of what the rsync copies
stopped (a database dumped, files under a running app) there is no
clone: the rsync reads the live data as before. An `--export` of a
`volumes` service on a target does the same, its tar from a clone. The
cost is the parent's: a database's pages written while the clone
exists go to new places, so its files fragment (measured on XFS: some
400 extents for 200 pages of 16 KiB written meanwhile, where a file
written in place keeps one), which stays once the clone is gone;
`xfs_fsr` on the database's files puts them back together.

## Transport

The rsync runs as root in `devopsy-util`, devopsy's own image (alpine 3
with rsync and GNU tar, find and chown; it replaced `devopsy-rsync` and
`devopsy-util:1` and `:2`, which `docker image rm devopsy-rsync:2
devopsy-util:1 devopsy-util:2` removes), built on each server the first time, so ownership and the
deploy user's permissions do not matter. Wherever it is used, an image a
week old is built again, its base pulled, so what runs as root on your
data gets alpine's fixes; a rebuild that fails (the registry out of
reach) warns and goes on with the image there. On one
server, one container copies between the two directories, without SSH,
and a stream is written into the child's directory directly. Across
servers, the parent's server starts an rsync daemon (read-only, serving
only the entries being copied, no published port) and reaches the child's
over SSH, through a reverse forward, with your SSH agent forwarded for
that session only, and never on to the child; a stream goes to the child's
file through an SSH session of the parent's too. Data goes server to
server, never through your machine. The sync's SSH sessions, like every
session devopsy opens, send keepalives every 15 seconds, so a firewall does not drop a quiet copy, and
the parent gives up on a child it cannot reach, or that refuses its key,
within 30 seconds rather than keeping its services stopped for minutes.
So, across servers:

- an agent must be running where devopsy runs, with keys both servers
  accept: the parent's server reaches the child's through it (devopsy
  checks the agent has keys before anything starts, not which). In CI,
  `eval $(ssh-agent); ssh-add` with the CI key the servers accept,
  instead of `ssh -i`;
- the parent's server can use every key in that agent for the session,
  not only the child's: with `prod` the parent of `staging` and `staging`
  the parent of `pr-*`, a sync of `pr-12` forwards the agent to
  staging's server, prod's key included. When servers trust each other
  less than that, load only the keys the sync needs, or constrain them
  to their path (OpenSSH 8.9 or later, hosts as in `known_hosts`):

  ```sh
  ssh-add -h vm1 ~/.ssh/vm1                # the parent's server, from your machine only
  ssh-add -h vm2 -h 'vm1>vm2' ~/.ssh/vm2   # the child's: from your machine and from vm1
  ```

- the child's server must be reachable from the parent's as `ssh -G`
  resolves it locally (user, hostname, port; its host key is accepted on
  first use). A host behind a `ProxyCommand` or `ProxyJump` is refused: a
  relay through your machine is not supported yet;
- a CI key needs `restrict,agent-forwarding,port-forwarding` in the deploy
  user's `authorized_keys` on both servers, what `--ssh-keys add
  --restricted` writes: the agent for the parent's session, the reverse
  forward for the child's. A plain `restrict` refuses both, and devopsy
  says which.

## Running a sync

```sh
devopsy @develop --sync                         # asks for the environment's name
devopsy @develop --sync --yes                   # CI
devopsy @develop --sync --from @b/prod          # another environment of the project
devopsy @develop --sync --yes --accept-image database  # below prod's version, on purpose
```

`--sync` asks you to type the environment's full name
(`<project>/[<instance>/]<environment>`, `shop/develop`), as `--destroy`
and `--import` do,
and so does `--release-sync` on a child already released; `--yes`
skips it. First-release syncs never ask. `--from` copies from another
environment of the project instead of the parent: an address, as after
`devopsy`, with its `@` (devopsy takes it without one too). A sync copies
everything the labels say; what must never be copied is excluded by label
(`devopsy.data.exclude`).

## Starting a child over

`devopsy @develop --reset` (same options as
`--sync`) is a sync that first throws away everything a sync leaves on
the child: every container of every profile, its named volumes, and
every entry of `shared/mnt`, the ones no label copies included (logs,
an unlabeled service's files). Every data service's history of its
images starts over with it ([Data and its images](images.md#data-and-its-images)): what the copy writes
takes the parent's, and one it writes nothing for (`copy=none`, its
entries all missing on the parent) has no version until a release
adopts its label. The parent's data then goes in as on a
first release, the services with data are created before `finish
child go`, and the run step starts the rest. The release, its images
and the child's `shared/.env` (its secrets) stay. Nothing of the child
answers from the wipe until its run step is done: a service a sync
keeps up to hold visitors goes with the rest, so they get the proxy's
errors meanwhile. It is the way out of
data nothing else fixes: a sync or an import that did not complete, a
half-written entry the parent does not have. Everything a sync checks
comes first, and the child is wiped only at the copy's first write, so
a parent that refuses or cannot be reached leaves it as it was; cut
after that, the child is down and marked, as after a sync cut
mid-copy, until a sync or a reset completes. A parent with no data to
copy refuses. Children only: an environment without a parent starts
over with `--destroy` and a release. It asks for the environment's name,
like `--sync`.

## Inherited variables

`inherit: [KEY, ...]` on the parent (or in
`defaults:`) names variables its children get, whatever the labels say
about the data, as the parent's current release says: a checkout's
`inherit:` changes nothing until the parent is released with it, so no
one syncing a child takes more of the parent's variables than its own
release gives. They are read from the parent's `shared/.env`, its own
`inherit.env` (what it inherited, so a chain passes values down), its
instance `.env`, its project `.env` and its `parent.env` on its server
(nearest first) and written as the child's
`shared/inherit.env`, the whole file each time, so a resync replaces it.
The child reads it below its own `shared/.env` and above its server's
instance and project files: a value the child sets itself wins (devopsy
says so when one shadows an inherited key: `--vars unset` it to inherit),
and no other environment on the child's server is touched. On a first
release this happens before the upload, so the variables check finds
them (they stay if the sync fails: the next release is a first one
again). On `--sync` and a later `--release-sync` it happens before the
copy too, into a pending file (`<environment>/.devopsy-sync.inherit.<sync id>.env`,
one per sync)
that everything the sync runs on the child reads in place of
`shared/inherit.env`, labels included, so a label from an inherited
variable follows the parent's in the same sync; once the copy is in it
moves into place, before the child's services restart and its run step,
and a failed copy removes it, leaving the child's variables as they
were. A parent whose `inherit:` names nothing leaves the child's
`shared/inherit.env` empty.

## A parent on another server

The instance's and the project's `.env`
are each server's own, so a child elsewhere would not see the parent's.
It gets a copy of them instead, `shared/parent.env`: all of their keys
as written, each from the nearest (its instance's over its project's,
over its own `parent.env` down a chain), read below the child's own
server's instance and project files, so what the child's server says
wins. As written: a `${...}` in them resolves on the child, against
its own variables, as it would were those files on its server, so
nothing of the parent's `shared/.env` leaves through a reference. Its
first line names where it came from: every sync writes it from its
source (before the copy in a pending file, as `inherit.env`, so a
failed sync leaves it as it was), and a child's release that does not
sync, `--reload` and `--inherit` copy it again, from the child's parent
(as you name its server then) or the address a `--from` gave. A parent
that cannot be read then is a warning, and the copy stays. A secret (`secrets:`) new in the copy is
named: a value that must stay on a server belongs in its environment's
`shared/.env`, which only `inherit:` copies, key by key. On the
parent's own server its instance and project files are the child's
already: the child gets only the parent's own `parent.env`, down a
chain.

`devopsy @<child> --inherit` refreshes the variables alone, no
data, no run step.
Names are printed, never values; keys the parent does not set are
listed. Not `DEVOPSY_*` or `COMPOSE_PROJECT_NAME`: those are the target's.

## Logs and failures

A sync's output goes into the log of the release
it synced into (`--log`), after the release's own. The parent's side
always restores its stopped services. The child's side knows what the
copy did to its data, whatever happened to devopsy: the parent marks it
right before its first write into the child and after its last, and the
child goes by that:

- Nothing written (a prepare or a check failed, the parent's stop
  failed, the first export failed with nothing to rsync): its stopped
  services come back, `finish child abort`, and the run step is
  skipped. On data an earlier sync or an import left partial, it stays
  partial (below), and what was down for it stays down; what this sync
  stopped comes back.
- Written in part (the rsync or an import failed or was cut,
  Ctrl-C mid-copy, a connection lost): the child's data is part old, part
  the parent's, so its stopped services stay down and the run step is
  skipped, `finish child partial`. Nothing goes on writing into it
  first: an import still running inside its service's container (a
  load through `docker exec`, which a cut never stops) ends with the
  service, stopped and started again by the next sync that completes,
  and devopsy's own copy containers go. A database copied raw would not start
  on it anyway (devopsy says which entry was cut). Nothing runs on it
  until a sync completes: releases and rollbacks refuse, the previous
  release is not restarted after a release that synced, a sync from it
  refuses, and `--status` says so. `--sync` (or `--release-sync`)
  completes it, or `--reset` starts it over.
- All of it (the copy completed, even if devopsy then went away or
  was interrupted): it goes on as after a success, `finish child go`,
  then the run step. A sync completes with its finish: one whose `finish
  child go` fails leaves the child as one that did not complete: the
  copy is in, but the project refused it, and `--status` and every
  refusal say so.

On a first release a failure marks the release failed and leaves nothing
current, and the next release syncs again. A failed or interrupted export
imports nothing, and its file goes. Interrupted (Ctrl-C) before the copy,
devopsy tells the child's server to abort and waits, as a release does:
it finishes the step it is in, restores what it stopped and shows it, and
a second Ctrl-C leaves at once. Interrupted during the copy,
devopsy ends its session to the parent's server, which stops the copy (an
export too) and restores its own services, then tells the child's to
finish as above. Once the copy is in and the run step runs, a Ctrl-C only
says so: the step is the server's, and a second Ctrl-C ends your session
while the step ends there (`--log` shows how). If devopsy itself goes away
mid-sync (killed, connection lost, or silent: the parent's side gives up
after 30 seconds without devopsy's heartbeat, the child's after 90), each
side finishes on its own and writes what happened to
`<environment>/.devopsy-sync.log` on its server; a child's side killed
before deciding is decided, as not completed, by whatever takes its lock
next: a sync, a release or a rollback; the last two then refuse (a release
that syncs is a sync). Restoring means what the sync stopped
(`<environment>/.devopsy-sync.services` meanwhile): a service that was
down before it, stopped for maintenance, stays down. A side whose devopsy
on the server is killed outright (the OOM killer, `kill -9`) restores
nothing itself: what it stopped stays down until the next release, sync,
import, export or snapshot of that environment starts it first (one not
marked: on a mark it stays down until the mark goes), and `--status`
names it meanwhile; so does what an `--export` killed that way stopped,
and what a release killed between its switch and its run step left
stopped for its release snapshot, which then starts on the images that
release gives it. Only what the
child's side stopped is kept off partial data: a service left running
during the copy (a `volumes` one labeled `never`, a `custom` one imported
into while running) sees it change, as it chose.
