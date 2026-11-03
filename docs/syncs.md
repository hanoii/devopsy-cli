# Parent environments and syncs

A child environment (`develop: {parent: prod}`, see
[Parents](config.md#parents)) is a clone of its parent. Its first release
syncs the parent's data. Later, `devopsy @<child> --sync` syncs it again,
then runs the `run` step of the child's deployed release again, from that
release's own config on the server, never from your checkout.

## Running a sync

```sh
devopsy @develop --sync                         # asks for the environment's name
devopsy @develop --sync --yes                   # CI
devopsy @develop --sync --from @b/prod          # another environment of the project
devopsy @develop --sync --yes --accept-image database  # below prod's version, on purpose
```

`--sync` asks you to type the environment's full name
(`<project>/[<instance>/]<environment>`, for example `shop/develop`), as
`--destroy` and `--import` do. So does `--release-sync` on a child already
released. `--yes` skips the prompt, and a first release's sync never asks.

`--from` copies from another environment of the project instead of the
parent. It takes an address written as after `devopsy`, with its `@`,
though devopsy also takes it without one.

A sync copies everything the labels say. Data that must never be copied
is left out by label (`devopsy.data.exclude`, [Data
labels](data.md#data-labels)); a sync has no options for it.

`--accept-image` lets a child run its own image on the parent's data when
the version check refuses it, on purpose
([Syncs and imports](images.md#syncs-and-imports)).

## What moves

What moves is what the labels say ([Data labels](data.md#data-labels)):

- A `volumes` service's entries go from the parent's `shared/mnt` into the
  child's. A file entry is written in place, so the container mounting it
  sees the new content. Files that a running service removes during the
  copy (rsync's "vanished" files, its exit 24) are a notice, not a
  failure, and the rest is copied. An entry missing on the parent is
  skipped with a message, and the child's is left as it is.
- A `custom` service's data is streamed. The parent's `export <service>`
  writes into a file in the child's directory
  (`.devopsy-sync.stream.<service>.<sync id>`). Only once the export has
  exited 0 does the child's `import <service>` read the file, with the
  child's service up: devopsy starts it alone, unless it stopped it. The
  file is removed afterwards, so an export cut short never reaches the
  import. Services go one after the other, in compose's `depends_on`
  order, dependencies first.

Each side reads the labels from its own release, so they can differ. To
keep them alike, put a variable in the label and have the parent pass it
to its children (`devopsy.data.copy=${DATA_DATABASE:-volumes}` with
`inherit: [DATA_DATABASE]`). Such a label follows the parent's within the
same sync: change it on the parent, then run `--sync`. A label the child
sets in its own `shared/.env` can still differ.

Before changing anything of its own, the parent checks the child's
services, and refuses when:

- a child service that mounts a copied entry is still running and is
  copied there as `custom` or `none`, or has no data, since what the
  parent rsyncs must not change under it. A service the child stopped is
  not running, and `volumes` on the child means a copy while it runs is
  wanted.
- a service the parent streams is not `custom` on the child, or one the
  child streams is copied as files (`volumes`) on the parent.

The child then starts its services again; nothing was copied.

A project with no service with data, or whose services have nothing to
copy, syncs nothing ("nothing to sync"): the parent's `prepare` and
`finish` do not run, and the child's `run` step still runs. On a child
whose data comes from a sync that did not complete, such a sync fails,
since it completes nothing and the earlier copy's data is still there.

## What stops

For the whole sync, the parent stops its `own` services with data and
every `always` one. The child stops its `own` services with data and every
`import` and `always` one. Each side also stops what depends on them
([`devopsy.data.stop`](data.md#devopsydatastop)). devopsy names a stop
word that differs between the two sides (set in a child's own
`shared/.env`).

The child stops its set before it knows what the parent copies. When the
parent has nothing to copy, the child's `import` services were then down
for a few seconds for nothing.

## Order

On the child's server, under the child's release lock for the whole sync,
the data capability's `prepare child` runs and the child side's set is
stopped. Then the child waits for the parent.

On the parent's server, the sync holds the parent's release lock shared,
so a release of the parent waits for the copy, and a sync lock of its own,
so syncs from one parent run one at a time. There:

1. `prepare parent` runs, then the parent side's set is stopped;
2. the rsync runs, then each stream's export writes into the child's
   spool;
3. the parent's services start again;
4. the child imports the spools. An import reads its spool, never the
   parent, so the parent is not down through a load;
5. `finish parent go` or `abort` runs.

Then, on the child's server, its services start again, `finish child go`
runs, and then its `run` step.

The child's stopped services stay down through the imports, so an app
never runs on a half-imported database. They also stay down after a copy
that did not complete (When a sync fails, below).

On a child's first release, before `finish child go`, devopsy creates the
child's services with data and its side's set (an `import` app without
data, an unlabeled app depending on an `own` database), so the hook can
sanitize the data through them. A service in a profile that is not up
(`COMPOSE_PROFILES`) is not created, just as the run step's `up` would
leave it.

A lock held by something else is announced ("waiting for the release
running on ...") and then waited for up to 10 minutes, like every release
lock.

## When the parent's data clones by reflinks

When the parent's entries and volumes are on an XFS or btrfs filesystem
together with the environment's directory, as for snapshots, the parent's
side is down only for a clone, which takes seconds whatever the size. Its
set is stopped, its data is cloned into `.devopsy-sync.clone.<id>` beside
it, and its services start again. The rsync then reads from the clone,
which is removed afterwards. Across servers it works the same way: the
daemon serves the clone. devopsy tries whether the data clones before
anything stops.

A `custom` service that the parent's labels stop is exported before the
services start again, as the labels promise, and so is any `custom`
service while one labeled `always` is stopped. Otherwise the export runs
after. When nothing that the rsync copies is stopped (a database dumped,
files under a running app), there is no clone, and the rsync reads the
live data.

An `--export` of a `volumes` service on a target clones the same way, and
makes its tar from the clone.

The cost falls on the parent: the pages a database writes while the clone
exists go to new places, so its files fragment, and they stay fragmented
after the clone is gone. On XFS, 200 pages of 16 KiB written meanwhile
made some 400 extents, where a file written in place keeps one. `xfs_fsr`
on the database's files puts them back together.

## How the data travels

The rsync runs as root in `devopsy-util`, devopsy's own image (alpine 3
with rsync and GNU tar, find and chown), so ownership and the deploy
user's permissions do not matter. The image is built on each server the
first time it is needed. Wherever it is used, an image a week old is
built again with its base pulled, so what runs as root on your data gets
alpine's fixes. A rebuild that fails (the registry out of reach) warns
and goes on with the image already there. It replaced `devopsy-rsync`,
`devopsy-util:1` and `:2`, which `docker image rm devopsy-rsync:2
devopsy-util:1 devopsy-util:2` removes.

On one server, a single container copies between the two directories,
without SSH, and a stream is written directly into the child's directory.

Across servers, the parent's server starts an rsync daemon (read-only,
serving only the entries being copied, with no published port) and
reaches the child's server over SSH through a reverse forward. Your SSH
agent is forwarded for that session only, and never on to the child. A
stream also goes to the child's file through an SSH session from the
parent's server. Data goes from server to server, never through your
machine.

The sync's SSH sessions, like every session devopsy opens, send
keepalives every 15 seconds, so a firewall does not drop a quiet copy.
The parent gives up within 30 seconds on a child it cannot reach, or that
refuses its key, rather than keeping its services stopped for minutes.

This means, across servers:

- An agent must run where devopsy runs, with keys both servers accept,
  since the parent's server reaches the child's through it. Before
  anything starts, devopsy checks that the agent has keys, though not
  which ones. In CI, use `eval $(ssh-agent); ssh-add` with the key the
  servers accept, instead of `ssh -i`.
- The parent's server can use every key in that agent for the session,
  not only the child's. For example, with `prod` the parent of `staging`,
  and `staging` the parent of `pr-*`, a sync of `pr-12` forwards the
  agent to staging's server, prod's key included. When servers trust each
  other less than that, load only the keys the sync needs, or constrain
  them to their path (OpenSSH 8.9 or later, with hosts written as in
  `known_hosts`):

  ```sh
  ssh-add -h vm1 ~/.ssh/vm1                # the parent's server, from your machine only
  ssh-add -h vm2 -h 'vm1>vm2' ~/.ssh/vm2   # the child's: from your machine and from vm1
  ```

- The child's server must be reachable from the parent's as `ssh -G`
  resolves it locally (user, hostname, port; its host key is accepted on
  first use). A host behind a `ProxyCommand` or `ProxyJump` is refused,
  since a relay through your machine is not supported.
- A CI key needs `restrict,agent-forwarding,port-forwarding` in the
  deploy user's `authorized_keys` on both servers, which is what
  `--ssh-keys add --restricted` writes: agent forwarding for the parent's
  session, port forwarding for the reverse forward to the child's. A
  plain `restrict` refuses both, and devopsy says which one failed.

## When a sync fails

A sync's output goes into the log of the release it synced into
(`--log`), after the release's own output. The parent's side always
starts its stopped services again.

The child's side knows what the copy did to its data, whatever happened
to devopsy: the parent marks the child's data right before its first
write into it and again after its last, and the child goes by those
marks.

- Nothing written (a prepare or a check failed, the parent's stop failed,
  the first export failed with nothing to rsync): the child's stopped
  services come back, `finish child abort` runs, and the run step is
  skipped. If an earlier sync or an import had left the data partial, it
  stays partial and what was down for it stays down; only what this sync
  stopped comes back.
- Written in part (the rsync or an import failed or was cut, a Ctrl-C
  mid-copy, a lost connection): the child's data is part old and part the
  parent's, so its stopped services stay down, the run step is skipped,
  and `finish child partial` runs. First, devopsy stops anything still
  writing into the data. An import still running inside its service's
  container (a load through `docker exec`, which a cut never stops) ends
  with the service: devopsy stops the service, and the next sync that
  completes starts it again. devopsy's own copy containers are removed. A
  database copied raw would not start on such data anyway, and devopsy
  says which entry was cut.

  Nothing runs on that data until a sync completes: releases and
  rollbacks refuse, the previous release is not restarted after a
  release that synced, a sync from the child refuses, and `--status`
  says so. `--sync` (or `--release-sync`) completes it, or `--reset`
  starts it over.
- All of it (the copy completed, even if devopsy then went away or was
  interrupted): the sync goes on as after a success, with `finish child
  go` and then the run step. A sync completes only with its finish, so
  one whose `finish child go` fails leaves the child as after a sync that
  did not complete. The copy is in, but the project refused it, and
  `--status` and every refusal say so.

On a first release, a failure marks the release failed and leaves nothing
current, and the next release syncs again. A failed or interrupted export
imports nothing, and its file is removed.

Interrupting a sync:

- A Ctrl-C before the copy makes devopsy tell the child's server to
  abort, and wait, as a release does: the server finishes the step it is
  in, starts what it stopped and shows it. A second Ctrl-C leaves at
  once.
- A Ctrl-C during the copy ends devopsy's session to the parent's server,
  which stops the copy (an export too) and starts its own services again.
  devopsy then tells the child's server to finish as above.
- Once the copy is in and the run step is running, a Ctrl-C only prints a
  message, since the step belongs to the server. A second Ctrl-C ends
  your session while the step runs to its end there (`--log` shows how).

If devopsy itself goes away mid-sync (killed, its connection lost, or
silent: the parent's side gives up after 30 seconds without devopsy's
heartbeat, the child's after 90), each side finishes on its own and
writes what happened to `<environment>/.devopsy-sync.log` on its server.
When a child's side is killed before it decides, whatever takes its lock
next decides for it, as not completed: a sync, a release or a rollback. A
release or a rollback then refuses (a release that syncs counts as a
sync).

Starting services again means starting what the sync stopped, which is
recorded in `<environment>/.devopsy-sync.services` meanwhile. A service
that was down before the sync, stopped for maintenance for example, stays
down.

When a side's devopsy on the server is killed outright (the OOM killer,
`kill -9`), it starts nothing itself. What it stopped stays down until the
next release, sync, import, export or snapshot of that environment starts
it first, unless the environment is marked, in which case it stays down
until the mark goes. `--status` names it meanwhile. The same goes for what
an `--export` killed that way stopped, and for what a release killed
between its switch and its run step left stopped for its release
snapshot, which then starts on the images that release gives it.

Only what the child's side stopped is kept off partial data. A service
left running during the copy (a `volumes` one labeled `never`, a `custom`
one imported into while running) sees its data change, as it chose.

## Starting a child over

`devopsy @develop --reset` takes the same options as `--sync`. It is a
sync that first throws away everything a sync would leave on the child:
every container of every profile, its named volumes, and every entry of
`shared/mnt`, including the ones no label copies (logs, an unlabeled
service's files).

Every service's history of its images starts over with it ([Data and its
images](images.md#data-and-its-images)). What the copy writes takes the
parent's history, and a service the copy writes nothing for (`copy=none`,
or its entries all missing on the parent) has no version until a release
takes its label.

The parent's data then goes in as on a first release: the services with
data are created before `finish child go`, and the run step starts the
rest. The release, its images and the child's `shared/.env`, with its
secrets, stay.

Nothing of the child answers from the wipe until its run step is done. A
service a sync keeps up to hold visitors goes with the rest, so visitors
get the proxy's errors meanwhile.

`--reset` is the way out of data nothing else fixes: a sync or an import
that did not complete, or a half-written entry the parent does not have.
Everything a sync checks runs first, and the child is wiped only at the
copy's first write, so a parent that refuses or cannot be reached leaves
the child as it was. A reset cut after that leaves the child down and
marked, as after a sync cut mid-copy, until a sync or a reset completes.

A reset whose parent has no data to copy is refused. Only children can be
reset: an environment without a parent starts over with `--destroy` and a
release. `--reset` asks for the environment's name, like `--sync`.

## Inherited variables

`inherit: [KEY, ...]` on the parent (or under `defaults:`) names variables
its children get, whatever the labels say about the data. What counts is
the parent's current release: an `inherit:` in a checkout changes nothing
until the parent is released with it, so no one syncing a child takes
more of the parent's variables than the parent's own release gives.

The values are read from the parent's files on its server, nearest
first: its `shared/.env`, its own `inherit.env` (what it inherited, so a
chain passes values down), its instance `.env`, its project `.env` and its
`parent.env`. They are written to the child's `shared/inherit.env`,
replacing the whole file each time.

The child reads that file below its own `shared/.env` and above its
server's instance and project files ([Variables on a
server](config.md#variables-on-a-server)). A value the child sets itself
wins, and devopsy says so when one shadows an inherited key (`--vars
unset` it to inherit). No other environment on the child's server is
touched.

- On a first release this happens before the upload, so the variables
  check finds them. They stay if the sync fails, since the next release is
  a first one again.
- On `--sync` and a later `--release-sync` it also happens before the
  copy, into a pending file per sync
  (`<environment>/.devopsy-sync.inherit.<sync id>.env`). Everything the
  sync runs on the child reads that file in place of `shared/inherit.env`,
  labels included, which is how a label from an inherited variable
  follows the parent's in the same sync. Once the copy is in, the file
  moves into place, before the child's services start again and its run
  step runs. A failed copy removes it, leaving the child's variables as
  they were.

A parent whose `inherit:` names nothing leaves the child's
`shared/inherit.env` empty.

`devopsy @<child> --inherit` refreshes the variables alone, with no data
and no run step. It prints names, never values, and lists the keys the
parent does not set. `DEVOPSY_*` and `COMPOSE_PROJECT_NAME` are never
inherited, since they belong to the target.

## A parent on another server

Each server has its own instance and project `.env`, so a child on another
server would not see the parent's. It gets a copy of them instead,
`shared/parent.env`:

- with all their keys, each from the nearest file: the instance's over the
  project's, and both over the parent's own `parent.env` down a chain;
- read below the child's own server's instance and project files, so what
  the child's server says wins;
- copied as written: a `${...}` in them resolves on the child, against its
  own variables, as it would if those files were on its server, so nothing
  of the parent's `shared/.env` leaves through a reference.

Its first line names where it came from. Every sync writes it from its
source before the copy, in a pending file as for `inherit.env`, so a
failed sync leaves it as it was. A child's release that does not sync, a
`--reload` and an `--inherit` copy it again too, from the child's parent
(on the server you name for the parent then) or from the address a
`--from` gave. If the parent cannot be read then, devopsy warns and the
copy stays.

devopsy names any secret (`secrets:`) that is new in the copy. A value that
must stay on its server belongs in its environment's `shared/.env`, which
only `inherit:` copies, key by key.

On the parent's own server, its instance and project files are already
the child's, so the child gets only the parent's own `parent.env`, down a
chain.
