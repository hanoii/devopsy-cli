# Snapshots

A snapshot is a copy of an environment at one moment: its data, its code
and the images that ran on them. It is kept on the server, and it can be
brought back whole or as data alone.

```sh
devopsy @vm1:prod --snapshot     # a snapshot: every service with data stopped for the moment of its copy, code and images kept
devopsy @vm1:prod --snapshots    # list them; restore, remove, export or import one
devopsy @vm1:prod --snapshots restore <id>   # its data, code and images back, as a new release (--no-code: the data alone)
devopsy @vm1:prod --snapshots export <id> | aws s3 cp - s3://backups/prod.tar   # a snapshot off the server
```

## What a snapshot holds

`devopsy @<target> --snapshot` takes one of the environment. It holds:

- the data of every service with data (`devopsy.data=true`): its entries
  of `mnt` and its named volumes, as files, whatever its copy label,
  except the excluded ones;
- a copy of the current release: its code, and its images bookmarked so
  they stay on the server;
- the history of the images that ran on the data;
- `shared/.env`, so an export carries the environment's secrets.

The data is kept as plain `tar.zst` files, one per entry or volume, so it
can be restored by hand (`zstd -dc mnt.files.tar.zst | tar -x`). The tars
hold files, directories, symbolic links and sparse files with their
holes. Links are kept wherever they lead, an app's absolute ones
included. A restore makes the links as they were and follows none. It
refuses a link that would make a path a service bind-mounts lead out of
its entry, since Docker would follow it. FIFOs, sockets and device nodes
are left out, since no restore takes them, and hard links come back as
separate files.

## Taking one

A take stops, for one moment, every service with data, any service
writing what the snapshot copies (a worker writing the app's files,
labeled or not), and what depends on them. devopsy writes the compressed
tars (`zstd`) from the stopped data, then starts the services again and
waits until they are healthy. The downtime is the time the tars take.
Other services keep running: a service that holds visitors, like
drupal11's `alwaysup`, answers meanwhile.

There are two ways to make that moment shorter:

- Where the data clones by reflinks (its entries and volumes on an XFS or
  btrfs filesystem together with the environment's directory, with
  Docker's volumes bound onto it), the stopped pass only clones the data.
  That takes seconds whatever the size, a database included. The services
  start again, and the tars are made from the clone afterwards, while
  nobody waits. Then the clone is removed, and so is any mirror left from
  before. devopsy tries this before anything stops, whatever the
  settings.
- An environment with `snapshots: {mirror: true}` keeps a mirror of its
  data (`.mirror` among its snapshots, kept between takes). The data is
  first copied into it while everything runs, then copied again with the
  services stopped, this time only what changed. That second copy is the
  downtime, and the tars are made from the mirror afterwards, while
  nobody waits. A mirror
  suits data that stays put between the two passes, like uploaded files.
  It does not suit a busy database, whose written tables change within
  minutes and are copied again whole; use reflinks there.

`--snapshots` says how the last take copied.

Until the services start again, a Ctrl-C or a lost session cancels the
take and starts them. After that, the take finishes on its own, and
`--log` shows how. If the take's devopsy on the server is killed
outright, the take starts what it stopped at once and says so. If the
whole session goes (a reboot), the next release, rollback, take, restore,
import, export or sync of the environment starts those services first,
and `--status` names them meanwhile. An import or a restore killed that
way after its first write also says so, with the mark it left and the
ways out.

A take is refused, with nothing stopped, when:

- a sync, an import or a restore left the environment incomplete, since a
  snapshot of it would restore as complete;
- the environment has no service with data;
- a service with data writes a bind mount from outside the project;
- a service with data has no entry or named volume (a database in an
  anonymous volume), so the snapshot would hold nothing of it;
- a symbolic link in an entry makes a bind source lead out of it, so the
  services could not start again;
- there is too little space for the tars, and for the mirror when the
  environment keeps one (measured first, with a tenth to spare).

## Where they go, and how many stay

Snapshots go in each environment's `snapshots/`, beside `shared/`, never
inside it, since everything in `shared/` is linked into each release. The
server's `snapshots: root` can put them elsewhere, under
`<root>/<project>/[<instance>/]<environment>` ([Release
settings](config.md#release-settings)): on a network volume, for example,
so the data runs at full speed on the local disk and the backups live off
it.

The environment's `snapshots: manual: {keep}` says how many stay (3 by
default), up to the server's `snapshots: max_keep` (5 by default). An
environment with `snapshots: manual: false` refuses `--snapshot`, which
suits pull request environments, for example.

## Release snapshots

With the environment's `snapshots: {release: true}`, every release that
replaces a current one first takes a snapshot, right before its switch,
of the data as the replaced release left it:

- what a take stops is stopped (with a mirror, its second pass copies only
  what changed while the release fetched its images);
- the tars are written, or, with a mirror, made from it afterwards, while
  nobody waits;
- the services stay stopped until the new release's run step starts them
  under its code. devopsy starts whatever the run step leaves stopped, and
  says so. A restore's run step works the same way.

The downtime is that stop, the switch and the `up`; without a mirror it
also includes writing the tars. A lost session or a Ctrl-C before the
switch cancels the release and starts the services again. A failure
taking the snapshot (too little space, a Docker error) fails the release
before its switch.

The snapshot belongs to the release before: `--snapshots` lists it as
`release`, beside that release's id. Once that release is gone, the
snapshot is removed at the next snapshot taken or removed. It is also
removed past `snapshots: {release: {keep}}`, which is 1, or `all` to keep
one for every release still kept. A release tried again after a failure
takes another snapshot, so with `keep` 2 or more, or `all`, the earlier
one still holds the data from before the failed attempt. An imported
release snapshot is kept as a manual one.

Restoring a release snapshot is a full return to that release, code and
data, after a migration gone wrong for example. With release snapshots
on, a restore also takes one first, of the data it replaces, so a restore
can be undone the same way.

No release snapshot is taken by a first release, by a rollback (it
changes code, not data), on an environment that a sync, an import or a
restore left incomplete, or on one with no service with data. devopsy
says so in the last two cases. `releases: {keep: 1}` with release
snapshots on is an error, since the snapshot would go with its own
release.

## Restoring

`--snapshots restore <id>` brings a snapshot back whole, as a new release
made from the snapshot's copy. A `--rollback` after it goes back to the
release before. The restore runs the restored release's `rollback:`
steps, else its `release:` steps. In order:

1. the tars are checked whole before anything stops. A warning says when
   their data may not fit where it goes (sparse files counted at full
   size), since a disk filled midway leaves the services down and the
   environment marked;
2. the images are fetched: the pulled ones by their names, as whatever
   the names point to now, and the snapshot's own builds from its
   bookmarks;
3. every service with data and its dependents stop;
4. each entry and volume is emptied and extracted, with its history of the
   images that ran on it. Nothing is checked and `--accept-image` plays no
   part, since the data and its version come back together;
5. the switch, and the run step.

The restored release's `devopsy-target.env` keeps this server's lines
(the target, the imports), since an imported snapshot's would name
another server.

`--no-code` restores the data alone under the current release, then runs
its run step again. It is refused when the current labels no longer fit
the snapshot's (a service gone, or an entry another service mounts now),
or when the data's versions do not allow the snapshot's data ([Data and
its images](images.md#data-and-its-images); `--accept-image <service>`
runs it anyway). Restore the snapshot whole in that case.

`shared/.env` comes back only with `--with-env`; without it, devopsy
names the keys that differ. The prompt asks for the environment's full
name (`--yes` in CI).

A restore completes once its run step has gone through. A restore cut
after its first write keeps down what it stopped. When its run step
fails, devopsy takes down what that step started and switches `current`
back, without restarting the previous release on the snapshot's data.
Either way the environment is marked, as after a failed import, until one
of these:

- the same restore again, which writes everything again;
- its data alone under the current release (`--no-code`);
- another snapshot restored;
- `--destroy`;
- on a child, `--reset`, or a `--sync` when the parent copies every
  service of the snapshot.

## Export and import

`--snapshots export <id>` writes a snapshot as one tar to stdout (`| aws
s3 cp - s3://...`), since devopsy keeps no storage of its own. Takes,
releases and restores go on meanwhile, and a removal or a pruning leaves
the snapshot being exported in place, with a message.

`--snapshots import` stores a snapshot from stdin into the same
environment, on this server or another, under a new id. It is checked
whole first: its tars as a restore checks them, nothing but files and
directories outside its code, and its list of images cut down to the
current release's services, each under that service's repository. To
move an environment to a new server, release the project there first,
then import the snapshot and restore it (`--with-env` brings its
secrets).

Import only snapshots you trust. A full restore runs the snapshot's code
as the deploy user (its steps, its compose file, its Dockerfiles), and
`--with-env` applies its variables to whatever code runs. A `--no-code`
restore runs none of its code, and its data is checked as in any restore.

Images stay out of an export: a restore pulls the pulled ones by their
names, as whatever the names point to now, and builds the built ones
from the snapshot's code.

## Removing them

`--snapshots remove <id>` removes one, with the images only it holds.
`--destroy` removes them all, and its prompt says how many.
