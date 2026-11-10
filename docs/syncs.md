# Parent environments and syncs

A child environment (`develop: {parent: prod}`, see
[Parents](config.md#parents)) is a clone of its parent. Its first
release syncs the parent's data. Later, `devopsy @<child> --sync` syncs
it again, then runs the `run` step of the child's deployed release
again, from the config stored with that release on the server, not your
checkout.

## Running a sync

```sh
devopsy @develop --sync                         # asks for the environment's name
devopsy @develop --sync --yes                   # CI
devopsy @develop --sync --from @b/prod          # another environment of the project
devopsy @develop --sync --yes --accept-image database  # below prod's version, on purpose
```

`--sync` asks you to type the environment's full name
(`<project>/[<instance>/]<environment>`, for example `shop/develop`), as
`--destroy` and `--import` do. So does `--release-sync` on a child
already released. `--yes` skips the prompt, and a first release's sync
never asks.

`--from` copies from another environment of the project instead of the
parent. It takes an address as you would type it after `devopsy`, with
or without its `@`.

A sync copies everything the labels say. Data that must never be copied
is left out by label (`devopsy.data.exclude`, [Data
labels](data.md#data-labels)); a sync has no options for it.

`--accept-image` lets a child run its image on the parent's data when
the version check refuses it, on purpose
([Syncs and imports](images.md#syncs-and-imports)).

## What moves

What moves is what the labels say ([Data labels](data.md#data-labels)):

- A `volumes` service's entries go from the parent's `shared/mnt` into
  the child's. A file entry is written in place, so the container
  mounting it sees the new content. Files that a running service removes
  during the copy (rsync's "vanished" files, its exit 24) are a notice,
  not a failure, and the rest is copied. An entry missing on the parent
  is skipped with a message, and the child's is left as it is. So is an
  entry or a volume the child's labels do not name (its release mounts
  another): a sync copies only what both sides name.
- A `custom` service's data is streamed. The parent's `export <service>`
  writes into a file in the child's directory
  (`.devopsy-sync.stream.<service>.<sync id>`). Only once the export has
  exited 0 does the child's `import <service>` read the file, with the
  child's service up: devopsy starts it alone, unless it stopped it. The
  file is removed afterwards. An export cut short never reaches the
  import. Services go one after the other, in compose's `depends_on`
  order, dependencies first.

The two sides' labels can differ ([Data labels](data.md#data-labels)).
To keep them alike, put a variable in the label and have the parent pass
it to its children (`devopsy.data.copy=${DATA_DATABASE:-volumes}` with
`inherit: [DATA_DATABASE]`). Such a label follows the parent's within
the same sync: change it on the parent, then run `--sync`. A label the
child sets in its `shared/.env` can still differ.

Before changing anything on its side, the parent checks the child's
services, and refuses when:

- a running child service mounts a copied entry and, on the child, is
  `custom` or `none`, or has no data. What the parent rsyncs must not
  change under it. A service the child stopped is not running, and
  `volumes` on the child means a copy while it runs is wanted.
- a service the parent streams is not `custom` on the child, or one the
  child streams is copied as files (`volumes`) on the parent.
- an entry the parent copies is of another kind on the child (a file
  where the parent's is a directory, or a link). `--reset` empties the
  child's entries first, so it takes such an entry.

The child then starts its services again; nothing was copied.

A project with no service with data, or whose services have nothing to
copy, syncs nothing ("nothing to sync"): the parent's `prepare` and
`finish` do not run, and the child's `run` step still runs. On a child
whose data comes from a sync that did not complete, such a sync fails,
because it completes nothing and the earlier copy's data is still there.

## What stops

For the whole sync, the parent stops its `own` services with data and
every `always` one. The child stops its `own` services with data and
every `import` and `always` one. Each side also stops what depends on
them ([`devopsy.data.stop`](data.md#devopsydatastop)). devopsy names a
stop word that differs between the two sides (set in a child's
`shared/.env`).

The child stops its set before the parent's side has said what it
copies. When the parent has nothing to copy, the child's `import`
services are down for a few seconds for nothing.

## Order

On the child's server, under the child's release lock for the whole
sync, the data capability's `prepare child` runs and the child side's
set is stopped. Then the child waits for the parent.

On the parent's server, the sync holds the parent's release lock shared,
which makes a release of the parent wait for the copy. It also holds a
sync lock, which makes syncs from one parent run one at a time. There:

1. `prepare parent` runs, then the parent side's set is stopped;
2. the rsync runs, then each stream's export writes into the child's
   spool;
3. the parent's services start again;
4. the child imports the spools. Each import reads its spool, so the
   parent is not down during a load;
5. `finish parent go` or `abort` runs.

Then, on the child's server, its services start again, `finish child go`
runs, and then its `run` step.

The child's stopped services stay down through the imports, so an app
never runs on a half-imported database. They also stay down after a copy
that did not complete (When a sync fails, below).

On a child's first release, before `finish child go`, devopsy creates
the child's services with data and its side's set (an `import` app
without data, an unlabeled app depending on an `own` database), so the
hook can sanitize the data through them. A service in a profile that is
not up (`COMPOSE_PROFILES`) is not created, as the run step's `up` would
leave it.

A lock held by something else is announced ("waiting for the release
running on ...") and then waited for up to 10 minutes, like every
release lock.

## When the parent's data clones by reflinks

When the parent's entries and volumes are on an XFS or btrfs filesystem
together with the environment's directory, as for snapshots, the
parent's side is down only for a clone, which takes seconds however
large the data is. Its set is stopped, its data is cloned into
`.devopsy-sync.clone.<id>` beside it, and its services start again. The
rsync then reads from the clone, which is removed afterwards. Across
servers it works the same way: the daemon serves the clone. devopsy
tests whether the data clones before it stops any service.

A `custom` service is exported before the services start again when the
parent's labels stop it, or when a service labeled `always` is stopped.
Otherwise the export runs after. When nothing that the rsync copies is
stopped (a database dumped, files under a running app), there is no
clone, and the rsync reads the live data.

An `--export` of a `volumes` service on a target clones the same way,
and makes its tar from the clone.

This has a cost on the parent. The pages a database writes while the
clone exists go to new places, so its files fragment, and they stay
fragmented after the clone is gone. On XFS, a database writing 200 pages
of 16 KiB while a sync's clone existed made some 400 extents, where a
file written in place keeps one. `xfs_fsr` on the database's files puts
them back together.

## How the data travels

The rsync runs as root in `devopsy.internal/util`, devopsy's image (alpine 3
with rsync and GNU tar, find and chown), so ownership and the deploy
user's permissions do not matter. The image is built on each server the
first time it is needed. Wherever it is used, an image a week old is
built again with its base pulled, so what runs as root on your data gets
alpine's fixes. A rebuild that fails (the registry out of reach) warns
and goes on with the image already there. It replaced `devopsy-rsync` and
`devopsy-util:1` to `:4`, which `docker image rm devopsy-rsync:2
devopsy-util:1 devopsy-util:2 devopsy-util:3 devopsy-util:4` removes.

On one server, a single container copies between the two directories,
without SSH, and a stream is written straight into the child's
directory. Two addresses are one server when ssh resolves them to the
same user, host name and port. They are also one server when they log
in as the same user and a marker devopsy writes on the parent's server,
with random content, reads back the same through the child's address.
So an environment addressed by a host name and another by the server's
IP address count as one server.

Across servers, the parent's server starts an rsync daemon for the
copy. It is read-only, serves only the entries being copied, and has
no network. For each entry and volume, the parent's server opens an SSH
session to the child's, and the child's rsync talks to the daemon over
that session itself: no port is opened on either server, and the
child's rsync has no network either. A stream is written to a file on
the child's server over a session of its own. Data goes from server to
server, never through your machine.

The parent's server connects with a key made for that sync, never with
your SSH agent ([CI](servers.md#ci)):

- devopsy makes the key pair on your machine for each sync and keeps it
  in memory. Once the child's side is ready, it authorizes the public
  half. The parent's side receives the private half through its SSH
  session's input, never on a command line, and keeps it in a file of
  its own until the push ends.
- On the child, the key runs only devopsy's command for that sync, and
  only while the sync is open there: no shell, no terminal, no agent,
  and no forwarding of any kind. The child builds every step from its
  own release and labels: the parent picks which of the sync's steps to
  run, only for names in the child's labels, and says which entries are
  files, which an entry the child has already decides. The key is
  removed once the child's side has the sync's result, and expires
  within two days in any case.
- The child's host keys come from the child's side too, over your own
  session, so the parent's first connection checks the child's host key
  instead of accepting whatever it is offered. A child whose `/etc/ssh`
  holds no `ssh_host_*_key.pub` is refused before anything stops.
- Whoever runs as the parent's user during a sync can read that key. It
  can only run that sync's steps on the child, and only until the sync
  ends.

The sync's SSH sessions, like every session devopsy opens, send
keepalives every 15 seconds, so a firewall does not drop a quiet copy.
If the child cannot be reached or refuses the key, the parent gives up
within 30 seconds instead of keeping its own services stopped for
minutes.

A sync across servers needs:

- The child's server reachable from the parent's at the user, hostname
  and port `ssh -G` gives on your machine. A host behind a
  `ProxyCommand` or `ProxyJump` is refused, since a relay through your
  machine is not supported. Two users on one machine count as two
  servers here.
- The child's sshd reading `expiry-time` in a key's options (OpenSSH
  7.7 or later).
- Both sides project users ([Servers](servers.md)), or neither: a
  project user would otherwise drive a copy the deploy user runs, or
  the other way round. devopsy refuses before anything stops, and
  `--export` piped into `--import` moves a service's data across through
  your machine instead.

## When a sync fails

A sync's output goes into the log of the release it synced into
(`--log`), after the release's own output. The parent's side always
starts its stopped services again.

The parent marks the child's data right before its first write into it
and again after its last. The child's side reads those marks to tell
what the copy did, however devopsy went away.

- Nothing written (a prepare or a check failed, the parent's stop
  failed, the first export failed with nothing to rsync): the child's
  stopped services come back and the run step is skipped. `finish child
  abort` runs, or `finish child partial` if an earlier sync or an import
  had left the data partial, which then stays partial and what was down
  for it stays down; only what this sync stopped comes back.
- Written in part (the rsync or an import failed or was cut, a Ctrl-C
  mid-copy, a lost connection): the child's data is part old and part
  the parent's, so its stopped services stay down, the run step is
  skipped, and `finish child partial` runs. First, devopsy stops
  anything still writing into the data. An import may still be running
  inside its service's container, as a load through `docker exec`, which
  a cut never stops. devopsy stops that service, which ends the import,
  and the next sync that completes starts it again. devopsy's copy
  containers are removed. A database copied raw would not start on such
  data, and devopsy says which entry was cut.

  Nothing runs on that data until a sync completes: releases and
  rollbacks refuse, the previous release is not restarted after a
  release that synced, a sync from the child refuses, and `--status`
  shows the mark. `--sync` (or `--release-sync`) completes it, or
  `--reset` starts it over.
- All of it (the copy completed, even if devopsy then went away or was
  interrupted): the sync goes on as after a success, with `finish child
  go` and then the run step. A sync completes only with its finish, so
  one whose `finish child go` fails leaves the child as after a sync
  that did not complete. The copy is in, but the project refused it, and
  `--status` and every refusal say so.

On a first release, a failure marks the release failed and leaves
nothing current, and the next release syncs again. A failed or
interrupted export imports nothing, and its file is removed.

Interrupting a sync:

- A Ctrl-C before the copy sends the child's server an abort, and
  devopsy waits, as for a release. The server finishes the step it is
  in, starts what it stopped and shows it. A second Ctrl-C leaves at
  once.
- A Ctrl-C during the copy ends devopsy's session to the parent's
  server, which stops the copy (an export too) and starts its services
  again. devopsy then has the child's server finish, as above.
- Once the copy is in and the run step is running, a Ctrl-C only prints
  a message, since the step belongs to the server. A second Ctrl-C ends
  your session while the step runs to its end there (`--log` shows how).

devopsy itself can go away mid-sync: killed, its connection lost, or
silent. The parent's side gives up after 30 seconds without devopsy's
heartbeat, the child's after 90. Each side finishes on its own and
writes what happened to `<environment>/.devopsy-sync.log` on its server.
When a child's side is killed before it settles the outcome, the next
sync, release or rollback to take its lock settles it as not completed.
A release or a rollback then refuses (a release that syncs counts as a
sync).

Starting services again means starting what the sync stopped, which is
recorded in `<environment>/.devopsy-sync.services` meanwhile. A service
that was down before the sync, stopped for maintenance for example,
stays down.

When a side's devopsy on the server is killed outright (the OOM killer,
`kill -9`), it starts nothing itself. What it stopped stays down until
the next release, sync, import, export or snapshot of that environment
starts it first. On a marked environment it stays down until the mark
goes. `--status` names it meanwhile.

The same applies to the services an `--export` stopped when it was
killed this way. It also applies to the services a release stopped for
its release snapshot, when the release was killed between its switch and
its run step. The release's services then start on the images that
release gives them.

Only what the child's side stopped is kept off partial data. A service
left running during the copy (a `volumes` one labeled `never`, a
`custom` one imported into while running) sees its data change.

## Starting a child over

`devopsy @develop --reset` takes the same options as `--sync`. It is a
sync that first throws away everything a sync would leave on the child:
every container of every profile, its named volumes and networks,
those only an older release named included, and every entry of
`shared/mnt`, including the ones no label copies (logs, an unlabeled
service's files).

Every data service's history of its images starts over with it ([Data
and its images](images.md#data-and-its-images)). What the copy writes
takes the parent's history, and a service the copy writes nothing for
(`copy=none`, or its entries all missing on the parent) has no version
until a release takes its label.

The parent's data then goes in as on a first release: the services with
data are created before `finish child go`, and the run step starts the
rest. The release, its images and the child's `shared/.env`, with its
secrets, stay.

From the wipe until its run step is done, no service of the child
answers. A service a sync keeps up to hold visitors goes with the rest,
so visitors get the proxy's errors meanwhile.

`--reset` fixes data nothing else can: a sync or an import that did not
complete, or a half-written entry the parent does not have.
Everything a sync checks runs first, and the child is wiped only at the
copy's first write, so a parent that refuses or cannot be reached leaves
the child as it was. A reset cut after that leaves the child down and
marked, as after a sync cut mid-copy, until a sync or a reset completes.

A reset whose parent has no data to copy is refused. Only children can
be reset: an environment without a parent starts over with `--destroy`
and a release. `--reset` asks for the environment's name, like `--sync`.

## Inherited variables

`inherit: [KEY, ...]` on the parent (or under `defaults:`) names
variables its children get, independently of what the labels say about
the data. What counts is the parent's current release. An `inherit:` in
a checkout changes nothing until the parent is released with it, so no
one syncing a child takes more of the parent's variables than the
parent's release gives.

The values are read from the parent's files on its server, nearest
first: its `shared/.env`, its `inherit.env` (what it inherited, which is
how a chain passes values down), its instance `.env`, its project `.env`
and its `parent.env`. They are written to the child's
`shared/inherit.env`, replacing the whole file each time.

The child reads that file below its `shared/.env` and above its
server's instance and project files ([Variables on a
server](config.md#variables-on-a-server)). A value the child sets itself
wins, and devopsy says so when one shadows an inherited key (`--vars
unset` it to inherit). No other environment on the child's server is
touched.

- On a first release this happens before the upload, so the variables
  check finds them. They stay if the sync fails, since the next release
  is a first one again.
- On `--sync` and a later `--release-sync` it also happens before the
  copy, into a pending file per sync
  (`<environment>/.devopsy-sync.inherit.<sync id>.env`). Everything the
  sync runs on the child reads that file in place of
  `shared/inherit.env`, labels included, which is how a label from an
  inherited variable follows the parent's in the same sync. Once the
  copy is in, the file moves into place, before the child's services
  start again and its run step runs. A failed copy removes it, leaving
  the child's variables as they were.

A parent whose `inherit:` names nothing leaves the child's
`shared/inherit.env` empty.

`devopsy @<child> --inherit` refreshes the variables alone, with no data
and no run step. It prints names, never values, and lists the keys the
parent does not set. `DEVOPSY_*` and `COMPOSE_PROJECT_NAME` are never
inherited, since they belong to the target.

## A parent on another server

Each server has its own instance and project `.env`, so a child on
another server would not see the parent's. It gets a copy of them
instead, `shared/parent.env`:

- with all their keys, each from the nearest file: the instance's over
  the project's, and both over the parent's `parent.env` down a chain;
- read below the instance and project files of the child's server, so
  what the child's server says wins;
- copied as written: a `${...}` in them resolves on the child, against
  its variables, as it would if those files were on its server, so
  nothing of the parent's `shared/.env` leaves through a reference.

Its first line names where it came from. Every sync writes it from its
source before the copy, in a pending file as for `inherit.env`, so a
failed sync leaves it as it was. A child's release that does not sync, a
`--reload` and an `--inherit` copy it again too, from the child's parent
(on the server you name for the parent then) or from the address a
`--from` gave. If the parent cannot be read then, devopsy warns and the
copy stays.

devopsy names any secret (`secrets:`) that is new in the copy. A value
that must stay on its server belongs in its environment's `shared/.env`,
which only `inherit:` copies, key by key.

On the parent's own server, its instance and project files are already
the child's, so the child gets only the parent's `parent.env`, down a
chain.

## Previews

A preview (for example one environment for each pull request) runs
code nobody has reviewed yet. Keep previews on a server of their own,
away from production, and release the project as a project user on
both servers ([project
users](servers.md#project-users-with-no-docker-socket)), since a sync
goes between two project users or two deploy users. `--project-users
add shop` makes `devopsy-shop` on each server, with its own keys there,
and each address names it ([Targets](config.md#targets)).

Let previews sync from a base environment on their own server, not
from production. A preview whose parent is production would stop
production's data services for each of its syncs, and production's
server would connect to the preview's to copy into it. The base's
parent is production, and you sync it yourself when you want newer
data in it:

```yaml
environments:
  prod: {}
  base: {parent: prod}       # on the preview server, synced by you
  "pr-*": {parent: base}     # @pr-12, @pr-feature, on the same server
```

`DEVOPSY_SERVER_PROD` names production's server, and
`DEVOPSY_SERVER_BASE` and `DEVOPSY_SERVER_PR` the preview server, each
with the project user (`devopsy-shop@vm2`).

The base and its previews run as one user on their server, so a
preview's code can read and change the base, its data and releases
included. The base keeps previews' syncs off production's server, not
the previews off the base: its next sync from production copies into
whatever a preview left there.

Previews get production's data, personal data included, unless the
base's `finish` step cleans it ([Order](#order)). They also
get its instance and project `.env` through the base's `parent.env`
([A parent on another server](#a-parent-on-another-server)), every key
of them, on the preview server: set a key on that server and the
previews use your value, but the copy still holds production's. So a
secret only production may know goes in production's `shared/.env`,
never in its instance or project `.env`. Nothing gets `shared/.env`
but what a parent's `inherit:` names, and a preview only if the base's
`inherit:` names it again.
