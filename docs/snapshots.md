# Snapshots

A snapshot is a copy of an environment as it was at one moment: its data,
its code and the images that ran on them, kept on the server and brought
back whole or as data alone.

```sh
devopsy @vm1:prod --snapshot     # a snapshot: every service with data stopped for the moment of its copy, code and images kept
devopsy @vm1:prod --snapshots    # list them; restore, remove, export or import one
devopsy @vm1:prod --snapshots restore <id>   # its data, code and images back, as a new release (--no-code: the data alone)
devopsy @vm1:prod --snapshots export <id> | aws s3 cp - s3://backups/prod.tar   # a snapshot off the server
```

## What a snapshot holds

`devopsy @<target> --snapshot` takes one of the environment. It holds:

- the data of every service with data (`devopsy.data=true`): its entries
  of `mnt` and its named volumes, as files, whatever its copy label, less
  the excluded ones;
- a copy of the current release: its code, and its images bookmarked so
  they stay on the server;
- the history of the images that ran on the data;
- `shared/.env`, so an export carries the environment's secrets.

The data is kept as plain `tar.zst` files, one per entry or volume, which
can be restored by hand (`zstd -dc mnt.files.tar.zst | tar -x`). They hold
files, directories and symbolic links, wherever the links lead (an app's
absolute ones included), and sparse files with their holes. A restore
makes the links as they were and follows none, and refuses one that makes
a path a service bind-mounts lead out of its entry, since Docker would
follow it. FIFOs, sockets and device nodes are left out, since no restore
takes them, and hard links come back as files of their own.

## Taking one

Every service with data, any service writing what the snapshot copies (a
worker writing the app's files, labeled or not), and what depends on them
stop together, for one moment. The compressed tars (`zstd`) are written
from the stopped data, then the services start again and are waited for
until healthy. The downtime is the time the tars take. Other services run
on: a service that holds visitors, like drupal11's `alwaysup`, answers
meanwhile.

There are two ways to make that moment shorter:

- Where the data clones by reflinks (its entries and volumes on an XFS or
  btrfs filesystem together with the environment's directory, Docker's
  volumes bound onto it), the stopped pass only clones the data: seconds,
  whatever its size, a database included. The services start again, and
  the tars are made from the clone afterwards, with nobody waiting; then
  the clone goes, and a mirror left from before with it. devopsy tries
  this before anything stops, whatever the settings.
- An environment with `snapshots: {mirror: true}` keeps a mirror of its
  data (`.mirror` among its snapshots, kept between takes). The data is
  first copied into it while everything runs, then again with the
  services stopped, only what changed since: that second copy is the
  downtime. The tars are made from the mirror afterwards, with nobody
  waiting. It is worth it for data that stays put between the two passes,
  like uploaded files, not for a busy database, whose written tables
  change within minutes and are copied again whole; reflinks are the way
  there.

`--snapshots` says how the last take copied.

Until the services start again, a Ctrl-C or a session gone cancels the
take and starts them. Past that it finishes on its own (`--log` shows
how). A take whose devopsy on the server is killed outright starts what it
stopped at once, and says so. One whose whole session went (a reboot)
leaves it to the next release, rollback, take, restore, import, export or
sync of the environment, which starts it first, and `--status` names it
meanwhile. An import or a restore killed that way after its first write
says so too, with the mark it left and the ways out.

A take is refused, with nothing stopped, for:

- an environment a sync, an import or a restore left incomplete: a
  snapshot of it would restore as complete;
- an environment with nothing with data;
- a service with data writing a bind mount from outside the project;
- a service with data and no entry or named volume (a database in an
  anonymous volume): the snapshot would hold nothing of it;
- a bind source that a link in its entry leads out of it: the services
  could not start again;
- too little space for the tars, and for the mirror when it keeps one
  (measured first, with a tenth to spare).

## Where they go, and how many stay

Snapshots go in each environment's `snapshots/`, beside `shared/` (never
inside it: everything there is linked into each release), or under the
server's `snapshots: root` (`<root>/<project>/[<instance>/]<environment>`,
[Release settings](config.md#release-settings)), a network volume say, so
the data runs at full speed on the local disk and the backups live off it.

The environment's `snapshots: manual: {keep}` (3) says how many stay, at
most the server's `snapshots: max_keep` (5). An environment's
`snapshots: manual: false` refuses `--snapshot` (pull request
environments, say).

## Release snapshots

With the environment's `snapshots: {release: true}`, every release that
replaces a current one first takes a snapshot of the data as the release
it replaces left it, right before its switch:

- what a take stops is stopped (with a mirror, its second pass copies
  only what changed while the release fetched its images);
- the tars are written, or, with a mirror, made from it afterwards, with
  nobody waiting;
- the services stay stopped until the new release's run step starts them
  under its code. What the run step leaves stopped, devopsy starts after
  it, and says so; a restore's run step likewise.

The downtime is that stop, the switch and the `up`; without a mirror it
includes the tars. A session gone or a Ctrl-C before the switch cancels
the release and starts the services again. A failure taking the snapshot
(too little space, Docker) fails the release before its switch.

The snapshot belongs to the release before: `--snapshots` lists it as
`release`, beside that release's id. It goes once that release is gone,
at the next snapshot taken or removed, or past `snapshots: {release:
{keep}}`: 1, or `all` for every one whose release is kept. A release tried
again after a failure takes another, so with `keep` 2 or more, or `all`,
the one before still holds the data from before the failed attempt. An
imported release snapshot is kept as a manual one.

Restoring it is a full return to that release, code and data: after a
migration gone wrong, say. A restore with release snapshots on takes one
first too, of the data it replaces, so a restore can be undone the same
way.

None is taken by a first release, by a rollback (it changes code, not
data), on an environment a sync, an import or a restore left incomplete
(said), or with nothing with data (said). `releases: {keep: 1}` with
release snapshots on is an error: the snapshot would go with its own
release.

## Restoring

`--snapshots restore <id>` brings a snapshot back whole, as a new release
made from the snapshot's copy. A `--rollback` after it goes back to the
release before. Its steps are the restored release's `rollback:`, else
`release:`. In order:

1. the tars are checked whole before anything stops, with a warning when
   their data may not fit where it goes (sparse files counted whole: a
   disk filled midway leaves the services down and the environment
   marked);
2. the images: the pulled ones as their names give them now, the
   snapshot's own builds from its bookmarks;
3. every service with data and its dependents stop;
4. each entry and volume is emptied and extracted, with its history of
   what ran on it: nothing is checked and `--accept-image` plays no part,
   since data and its version come back together;
5. the switch, and the run step.

The restored release's `devopsy-target.env` keeps this server's lines
(the target, the imports), since an imported snapshot's would name
another server.

`--no-code` restores the data alone under the current release, and runs
its run step again. It is refused when the current labels no longer fit
the snapshot's (a service gone, or an entry another service mounts now),
or when the data's versions do not allow the snapshot's data
([Data and its images](images.md#data-and-its-images); `--accept-image
<service>` runs it anyway): restore it whole then.

`shared/.env` comes back only with `--with-env`; without it, the keys
that differ are named. The prompt asks for the environment's full name
(`--yes` in CI).

A restore completes once its run step went through. One cut after its
first write keeps what it stopped down. One whose run step fails takes
down what that step started, and `current` goes back without the
previous release being restarted on the snapshot's data. Either way the
environment is marked, as after an import that failed, until one of:

- the same restore again, which writes everything again;
- its data alone under the current release (`--no-code`);
- another snapshot restored;
- `--destroy`;
- on a child, `--reset`, or a `--sync` when the parent copies every
  service of the snapshot.

## Export and import

`--snapshots export <id>` writes a snapshot as one tar to stdout (`| aws
s3 cp - s3://...`): devopsy keeps no storage of its own. Takes, releases
and restores go on meanwhile, and a removal or pruning leaves the
snapshot being exported, said.

`--snapshots import` stores one from stdin into the same environment, on
this server or another, under a new id. It is checked whole first: its
tars as a restore checks them, nothing but files and directories outside
its code, and its image listing cut to the current release's services,
each under that service's repository. To bring an environment to a new
server, release the project there first, then import the snapshot and
restore it (`--with-env` brings its secrets).

Import only snapshots you trust. A full restore runs the snapshot's code
as the deploy user (its steps, its compose file, its Dockerfiles), and
`--with-env` applies its variables to whatever code runs. A `--no-code`
restore runs nothing of it, and its data is checked as for any restore.

Images stay out of an export: a restore pulls the pulled ones as their
names give them now, and builds the built ones from the snapshot's code.

## Removing them

`--snapshots remove <id>` removes one, with the images only it holds.
`--destroy` removes them all, and its prompt says how many.
