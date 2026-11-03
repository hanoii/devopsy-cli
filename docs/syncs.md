# Parent environments and syncs

A child environment (`develop: {parent: prod}`, see
[Parents](config.md#parents)) is a clone of its parent. Its first release
syncs the parent's data, and `devopsy @<child> --sync` does it again
later, then runs the `run` step of the child's deployed release again
(from its own config on the server, never your checkout's).

## Running a sync

```sh
devopsy @develop --sync                         # asks for the environment's name
devopsy @develop --sync --yes                   # CI
devopsy @develop --sync --from @b/prod          # another environment of the project
devopsy @develop --sync --yes --accept-image database  # below prod's version, on purpose
```

`--sync` asks you to type the environment's full name
(`<project>/[<instance>/]<environment>`, `shop/develop`), as `--destroy`
and `--import` do, and so does `--release-sync` on a child already
released; `--yes` skips it. A first release's sync never asks.

`--from` copies from another environment of the project instead of the
parent. It takes an address as after `devopsy`, with its `@` (devopsy
takes it without one too).

A sync copies everything the labels say. What must never be copied is
left out by label (`devopsy.data.exclude`,
[Data labels](data.md#data-labels)); a sync takes no options for it.

`--accept-image` lets a child run below its parent's data version on
purpose ([Syncs and imports](images.md#syncs-and-imports)).

## What moves

What moves is what the labels say ([Data labels](data.md#data-labels)):

- A `volumes` service's entries go from the parent's `shared/mnt` into the
  child's. A file entry is written in place, so the container mounting it
  sees the new content. Files a running service removes during the copy
  (rsync's "vanished", its exit 24) are a notice, not a failure: the rest
  is copied. An entry missing on the parent is skipped, said, and the
  child's is left as it is.
- A `custom` service's data is streamed. The parent's `export <service>`
  writes into a file in the child's directory
  (`.devopsy-sync.stream.<service>.<sync id>`), and only once the export
  exited 0 does the child's `import <service>` read it, with the child's
  service up (devopsy starts it, alone, unless it stopped it). The file
  goes afterwards, so a cut export never reaches the import. Services go
  one after the other, in compose's `depends_on` order, dependencies
  first.

Each side reads the labels from its own release, so they can differ. A
variable in a label that the parent passes to its children keeps them
alike (`devopsy.data.copy=${DATA_DATABASE:-volumes}` with `inherit:
[DATA_DATABASE]`), and such a label follows the parent's in the same sync:
change it on the parent, then `--sync`. A label the child sets in its own
`shared/.env` can still differ.

Before changing anything of its own, the parent looks at the child's
services, and refuses:

- when a child service that mounts a copied entry is still running and
  is copied there as `custom` or `none`, or has no data: what the parent
  rsyncs must not change under it. One the child stopped is not running,
  and `volumes` on the child says a running copy is wanted.
- when a service the parent streams is not `custom` on the child, or one
  the child streams is copied as files (`volumes`) on the parent.

The child then starts its services again; nothing was copied.

A project with no service with data, or whose services have nothing to
copy, syncs nothing ("nothing to sync"): the parent's `prepare` and
`finish` do not run, and the child's `run` step still runs. On a child
whose data comes from a sync that did not complete, such a sync completes
nothing, since the earlier copy's data is still there, and it fails.

## What stops

For the whole sync, the parent stops its `own` and `always` services with
data, and the child its `own`, `import` and `always` ones, each with what
depends on them ([`devopsy.data.stop`](data.md#devopsydatastop)). A stop
word that differs between the two sides (from a child's own
`shared/.env`) is named.

The child stops its set before it knows what the parent copies, so when
the parent has nothing to copy, the child's `import` services were down
for a few seconds for nothing.

## Order

On the child's server, under its release lock for the whole sync: the
data capability's `prepare child`, then its side's set stopped; then it
waits for the parent.

On the parent's server, under a shared hold of its release lock (a
release of the parent waits for the copy) and a sync lock of its own
(syncs from one parent run one at a time):

1. `prepare parent`, then its side's set stopped;
2. the rsync, then each stream's export into the child's spool;
3. its services started;
4. the child's imports of the spools: an import reads its spool, never
   the parent, so the parent is not down through a load;
5. `finish parent go` or `abort`.

Then on the child's: its services started, `finish child go`, and its
`run` step.

The child's stopped services stay down through the imports, so an app
never runs on a half-imported database, and they stay down after a copy
that did not complete too (When a sync fails, below).

On a child's first release, before `finish child go`, devopsy creates the
child's services with data and its side's set (an `import` app without
data, an unlabeled app depending on an `own` database), so the hook can
sanitize the data through them. One in a profile that is not up
(`COMPOSE_PROFILES`) stays uncreated, as the run step's `up` would leave
it.

A lock held by something else is announced ("waiting for the release
running on ..."), then waited for up to 10 minutes, like every release
lock.

## When the parent's data clones by reflinks

When the parent's entries and volumes are on an XFS or btrfs filesystem
together with the environment's directory, as for snapshots, its side is
down for a clone only: seconds, whatever the size. Its set is stopped, its
data cloned into `.devopsy-sync.clone.<id>` beside it, its services
started again, and the rsync reads from the clone, which then goes.
Across servers it is the same: the daemon serves the clone. Whether the
data clones is tried before anything stops.

A `custom` service the parent's labels stop, or any `custom` service
while one labeled `always` is stopped, is exported before the services
start again, as the labels promise; otherwise after. With nothing of what
the rsync copies stopped (a database dumped, files under a running app),
there is no clone, and the rsync reads the live data.

An `--export` of a `volumes` service on a target clones the same way,
and makes its tar from the clone.

The cost is the parent's: a database's pages written while the clone
exists go to new places, so its files fragment (on XFS, 200 pages of 16
KiB written meanwhile made some 400 extents, where a file written in
place keeps one), and stay so once the clone is gone. `xfs_fsr` on the
database's files puts them back together.

## How the data travels

The rsync runs as root in `devopsy-util`, devopsy's own image (alpine 3
with rsync and GNU tar, find and chown), built on each server the first
time it is needed, so ownership and the deploy user's permissions do not
matter. Wherever it is used, an image a week old is built again with its
base pulled, so what runs as root on your data gets alpine's fixes. A
rebuild that fails (the registry out of reach) warns and goes on with the
image there. It replaced `devopsy-rsync`, `devopsy-util:1` and `:2`, which
`docker image rm devopsy-rsync:2 devopsy-util:1 devopsy-util:2` removes.

On one server, one container copies between the two directories, without
SSH, and a stream is written into the child's directory directly.

Across servers, the parent's server starts an rsync daemon (read-only,
serving only the entries being copied, no published port) and reaches
the child's server over SSH, through a reverse forward, with your SSH
agent forwarded for that session only and never on to the child. A
stream goes to the child's file through an SSH session of the parent's
too. Data goes from server to server, never through your machine.

The sync's SSH sessions, like every session devopsy opens, send
keepalives every 15 seconds, so a firewall does not drop a quiet copy,
and the parent gives up within 30 seconds on a child it cannot reach, or
that refuses its key, rather than keeping its services stopped for
minutes.

So, across servers:

- An agent must run where devopsy runs, with keys both servers accept:
  the parent's server reaches the child's through it. devopsy checks
  that the agent has keys before anything starts, not which. In CI, use
  `eval $(ssh-agent); ssh-add` with the key the servers accept, instead of
  `ssh -i`.
- The parent's server can use every key in that agent for the session,
  not only the child's. With `prod` the parent of `staging`, and
  `staging` the parent of `pr-*`, a sync of `pr-12` forwards the agent to
  staging's server, prod's key included. When servers trust each other
  less than that, load only the keys the sync needs, or constrain them to
  their path (OpenSSH 8.9 or later, hosts as in `known_hosts`):

  ```sh
  ssh-add -h vm1 ~/.ssh/vm1                # the parent's server, from your machine only
  ssh-add -h vm2 -h 'vm1>vm2' ~/.ssh/vm2   # the child's: from your machine and from vm1
  ```

- The child's server must be reachable from the parent's as `ssh -G`
  resolves it locally (user, hostname, port; its host key is accepted on
  first use). A host behind a `ProxyCommand` or `ProxyJump` is refused: a
  relay through your machine is not supported yet.
- A CI key needs `restrict,agent-forwarding,port-forwarding` in the
  deploy user's `authorized_keys` on both servers, which is what
  `--ssh-keys add --restricted` writes: the agent for the parent's
  session, the reverse forward for the child's. A plain `restrict`
  refuses both, and devopsy says which.

## When a sync fails

A sync's output goes into the log of the release it synced into
(`--log`), after the release's own. The parent's side always starts its
stopped services again.

The child's side knows what the copy did to its data, whatever happened
to devopsy: the parent marks it right before its first write into the
child and after its last, and the child goes by that.

- Nothing written (a prepare or a check failed, the parent's stop failed,
  the first export failed with nothing to rsync): the child's stopped
  services come back, `finish child abort` runs, and the run step is
  skipped. On data an earlier sync or an import left partial, the data
  stays partial and what was down for it stays down; what this sync
  stopped comes back.
- Written in part (the rsync or an import failed or was cut, a Ctrl-C
  mid-copy, a connection lost): the child's data is part old, part the
  parent's, so its stopped services stay down, the run step is skipped,
  and `finish child partial` runs. First, nothing goes on writing into
  it: an import still running inside its service's container (a load
  through `docker exec`, which a cut never stops) ends with the service,
  which is stopped and started again by the next sync that completes, and
  devopsy's own copy containers go. A database copied raw would not start
  on such data anyway, and devopsy says which entry was cut.

  Nothing runs on that data until a sync completes: releases and
  rollbacks refuse, the previous release is not restarted after a
  release that synced, a sync from the child refuses, and `--status`
  says so. `--sync` (or `--release-sync`) completes it, or `--reset`
  starts it over.
- All of it (the copy completed, even if devopsy then went away or was
  interrupted): the sync goes on as after a success, `finish child go`,
  then the run step. A sync completes only with its finish: one whose
  `finish child go` fails leaves the child as after a sync that did not
  complete. The copy is in, but the project refused it, and `--status`
  and every refusal say so.

On a first release, a failure marks the release failed and leaves nothing
current, and the next release syncs again. A failed or interrupted
export imports nothing, and its file goes.

Interrupting a sync:

- With Ctrl-C before the copy, devopsy tells the child's server to abort
  and waits, as a release does: it finishes the step it is in, starts
  what it stopped and shows it. A second Ctrl-C leaves at once.
- With Ctrl-C during the copy, devopsy ends its session to the parent's
  server, which stops the copy (an export too) and starts its own
  services again, then tells the child's to finish as above.
- Once the copy is in and the run step runs, a Ctrl-C only says so: the
  step is the server's. A second Ctrl-C ends your session while the step
  ends there (`--log` shows how).

If devopsy itself goes away mid-sync (killed, its connection lost, or
silent: the parent's side gives up after 30 seconds without devopsy's
heartbeat, the child's after 90), each side finishes on its own and
writes what happened to `<environment>/.devopsy-sync.log` on its server.
A child's side killed before deciding is decided, as not completed, by
whatever takes its lock next: a sync, a release or a rollback; the last
two then refuse (a release that syncs counts as a sync).

Starting services again means what the sync stopped, recorded in
`<environment>/.devopsy-sync.services` meanwhile: a service that was down
before the sync, stopped for maintenance say, stays down.

A side whose devopsy on the server is killed outright (the OOM killer,
`kill -9`) starts nothing itself. What it stopped stays down until the
next release, sync, import, export or snapshot of that environment starts
it first (unless the environment is marked: then it stays down until the
mark goes), and `--status` names it meanwhile. The same goes for what an
`--export` killed that way stopped, and for what a release killed between
its switch and its run step left stopped for its release snapshot, which
then starts on the images that release gives it.

Only what the child's side stopped is kept off partial data. A service
left running during the copy (a `volumes` one labeled `never`, a `custom`
one imported into while running) sees its data change, as it chose.

## Starting a child over

`devopsy @develop --reset`, with the same options as `--sync`, is a sync
that first throws away everything a sync would leave on the child: every
container of every profile, its named volumes, and every entry of
`shared/mnt`, including the ones no label copies (logs, an unlabeled
service's files).

Every service's history of its images starts over with it
([Data and its images](images.md#data-and-its-images)): what the copy
writes takes the parent's history, and a service it writes nothing for
(`copy=none`, its entries all missing on the parent) has no version until
a release takes its label.

The parent's data then goes in as on a first release: the services with
data are created before `finish child go`, and the run step starts the
rest. The release, its images and the child's `shared/.env`, with its
secrets, stay.

Nothing of the child answers from the wipe until its run step is done. A
service a sync keeps up to hold visitors goes with the rest, so visitors
get the proxy's errors meanwhile.

`--reset` is the way out of data nothing else fixes: a sync or an import
that did not complete, a half-written entry the parent does not have.
Everything a sync checks comes first, and the child is wiped only at the
copy's first write, so a parent that refuses or cannot be reached leaves
it as it was. Cut after that, the child is down and marked, as after a
sync cut mid-copy, until a sync or a reset completes.

A parent with no data to copy refuses. Only children can be reset: an
environment without a parent starts over with `--destroy` and a release.
`--reset` asks for the environment's name, like `--sync`.

## Inherited variables

`inherit: [KEY, ...]` on the parent (or under `defaults:`) names variables
its children get, whatever the labels say about the data. What counts is
the parent's current release: a checkout's `inherit:` changes nothing
until the parent is released with it, so no one syncing a child takes
more of the parent's variables than the parent's own release gives.

The values are read from the parent's files on its server, nearest
first: its `shared/.env`, its own `inherit.env` (what it inherited, so a
chain passes values down), its instance `.env`, its project `.env` and its
`parent.env`. They are written as the child's `shared/inherit.env`, the
whole file each time, so a new sync replaces it.

The child reads that file below its own `shared/.env` and above its
server's instance and project files ([Variables on a
server](config.md#variables-on-a-server)). A value the child sets itself
wins, and devopsy says so when one shadows an inherited key (`--vars
unset` it to inherit). No other environment on the child's server is
touched.

- On a first release this happens before the upload, so the variables
  check finds them. They stay if the sync fails: the next release is a
  first one again.
- On `--sync` and a later `--release-sync` it happens before the copy
  too, into a pending file, one per sync
  (`<environment>/.devopsy-sync.inherit.<sync id>.env`), which everything
  the sync runs on the child reads in place of `shared/inherit.env`,
  labels included. So a label from an inherited variable follows the
  parent's in the same sync. Once the copy is in, the file moves into
  place, before the child's services start again and its run step runs.
  A failed copy removes it, leaving the child's variables as they were.

A parent whose `inherit:` names nothing leaves the child's
`shared/inherit.env` empty.

`devopsy @<child> --inherit` refreshes the variables alone, with no data
and no run step. Names are printed, never values, and keys the parent
does not set are listed. `DEVOPSY_*` and `COMPOSE_PROJECT_NAME` are never
inherited: they are the target's.

## A parent on another server

The instance's and the project's `.env` are each server's own, so a child
on another server would not see the parent's. It gets a copy of them
instead, `shared/parent.env`:

- all their keys, as written, each from the nearest file: the instance's
  over the project's, over the parent's own `parent.env` down a chain;
- read below the child's own server's instance and project files, so what
  the child's server says wins;
- as written: a `${...}` in them resolves on the child, against its own
  variables, as it would if those files were on its server, so nothing of
  the parent's `shared/.env` leaves through a reference.

Its first line names where it came from. Every sync writes it from its
source (before the copy, in a pending file, as for `inherit.env`, so a
failed sync leaves it as it was). A child's release that does not sync,
a `--reload` and an `--inherit` copy it again, from the child's parent
(on the server you name for it then) or from the address a `--from` gave.
A parent that cannot be read then is a warning, and the copy stays.

A secret (`secrets:`) new in the copy is named. A value that must stay on
its server belongs in its environment's `shared/.env`, which only
`inherit:` copies, key by key.

On the parent's own server, its instance and project files are already
the child's: the child then gets only the parent's own `parent.env`,
down a chain.
