# Snapshots

`devopsy @<target> --snapshot` takes one of the
environment: the data of every service with data (`devopsy.data=true`:
its entries of `mnt` and named volumes, as files, whatever its copy
label, excluded ones left out), a copy of the current release (its code,
and its images bookmarked so they stay), the image history and
`shared/.env` (so an export carries the environment's secrets). Every
service with data, any service writing what the snapshot copies (a
worker writing the app's files, labeled or not), and what depends on
them stop for one moment, all together, as Platform.sh's backups do, while the compressed tars
(`zstd`) are written from the stopped data, then they start, waited for
until healthy: the downtime is the time the tars take. An environment
with `snapshots: {mirror: true}` keeps a mirror of its data instead
(`.mirror` among its snapshots, kept): the data is first copied into it
while everything runs, then again with them stopped, only what changed
since (the downtime), and the tars are made from the mirror afterwards,
nobody waiting. Worth it for data that stays put between the two
passes, like uploaded files; not for a busy database, whose written
tables change within minutes and are copied again whole (data on XFS,
whose reflinks make the copy instant, is the way there). Where the
data clones by reflinks (its entries and volumes on an XFS or btrfs
together with the environment's directory, Docker's volumes bound onto
it), a take needs neither: tried before
anything stops, the stopped pass clones the data (seconds, whatever its
size, a database included), the services start, and the tars are made
from the clone afterwards, nobody waiting; the clone then goes, and a
mirror left from before with it. `--snapshots` says how the last take
copied. A snapshot holds files, directories and symbolic links
(wherever they lead, an app's absolute ones included: a restore makes
them as they were and follows none, and refuses one that makes a path
a service bind-mounts lead out of its entry, which Docker would follow),
sparse files with their holes:
FIFOs, sockets and device nodes are left out (no restore takes them),
and hard links come back as files of their own. Plain
`tar.zst` files, one per entry or volume, restorable by hand
(`zstd -dc mnt.files.tar.zst | tar -x`). Other services run on (a
service that holds visitors, like drupal11's `alwaysup`, answers
meanwhile). Before the services start again, a Ctrl-C or a session gone
cancels it and starts them; past that it finishes on its own (`--log`
shows how). A take whose devopsy on the server is killed outright starts
what it stopped again at once and says so; one whose whole session went
(a reboot) leaves it for the next release, rollback, take, restore,
import, export or sync of the environment, which starts it first, and
`--status` names it meanwhile. An import or a restore killed that way
after its first write says so too, with the mark it left and the ways
out. Refused, nothing
stopped: an environment a sync, an import or a restore left incomplete
(a snapshot of it would restore as complete), nothing with data, a
service with data writing a bind from outside the project, one with
data and no entry or named volume (a database in an anonymous volume:
a snapshot would hold nothing of it), a bind source a link in its entry
leads out of it (the services could not start again), too little
space for the tars, and a mirror when it keeps one (measured first, a
tenth to spare).

## Where they go

Where they go: each environment's `snapshots/`, beside `shared/` (never
inside it: everything there is linked into each release), or the
server's `snapshots: root` (`<root>/<project>/[<instance>/]<environment>`),
a network volume say: the data at full speed on the local disk, the
backups off it. How many stay: the environment's `snapshots: manual:
{keep}` (3), at most the server's `snapshots: max_keep` (5); an
environment's `snapshots: manual: false` refuses `--snapshot` (pull
request environments, say).

## Release snapshots

With the environment's `snapshots: {release:
true}`, every release that replaces a current one first takes a
snapshot of the data as the release it replaces left it, right before
its switch: what a take stops stops (with a
mirror only for what changed while its images were fetched), the tars
are written (or, with a mirror, made from it afterwards, nobody
waiting), and the services stay stopped until the new release's run
step starts them under its code (what it leaves stopped, devopsy starts
after it, said; a restore's likewise). The downtime is that stop, the switch
and the `up`; without a mirror it includes the tars. A session gone or
a Ctrl-C before the switch cancels the release and starts them again;
a failure taking the snapshot (too little space, Docker) fails the
release before its switch. The snapshot belongs to the release before:
`--snapshots` lists it as `release`, beside that release's id, and it
goes once that release is gone, at the next snapshot taken or removed,
or past `snapshots: {release: {keep}}` (1, or `all`: every one whose
release is kept; a release tried again after a failure takes another,
and with `keep` 2 or more, or `all`, the one before still holds the
data from before the failed attempt). An
imported one is kept as a manual snapshot. Restoring it is a full return to
that release, code and data (a migration gone wrong, say). A restore
with release snapshots on takes one first too, of the data it replaces,
so a restore can be undone the same way. None is taken by a first
release, a rollback (it changes code, not data), on an environment a
sync, an import or a restore left incomplete (said), or with nothing
with data (said). `releases: {keep: 1}` with release snapshots on is an
error: the snapshot would go with its own release.

## Restoring

`--snapshots restore <id>` brings one back whole, as Platform.sh's
restores do: a new release made from the snapshot's copy (a
`--rollback` after it goes back to the release before), its steps the
restored release's `rollback:`, else `release:`; its tars checked whole
before anything stops (with a warning when their data may not fit
where it goes, sparse files counted whole: a disk filled midway leaves
the services down and the environment marked), then every service with data and its dependents
stopped, each entry and volume emptied and extracted, with their
history of what ran on them (nothing checked, no `--accept-image`: data
and its version come back together), the pulled images as their names
give them now, its own builds from the snapshot's bookmarks; then the
switch and the run step. Its
`devopsy-target.env` keeps this server's lines (the target, the imports:
an imported snapshot's would name another server). `--no-code` restores
the data alone under the current release and reruns its run step,
refused when its labels no longer fit the snapshot's (a service gone, or
an entry another service mounts now), or when its data's versions do not
allow the snapshot's data ([Data and its images](images.md#data-and-its-images); `--accept-image
<service>` runs it anyway): restore it whole then.
`shared/.env` comes back only with `--with-env`; without it, the keys
that differ are named. The prompt asks for the environment's full name
(`--yes` for CI). A restore completes once its run step went through.
One cut after its first write keeps what it stopped down, and one whose
run step fails takes down what that step started (current goes back,
the previous release not restarted on the snapshot's data), the
environment marked as an import's would be, until the same restore
again (it writes everything again), its data alone under the current
release (`--no-code`), another snapshot, `--destroy`, or on a child
`--reset` (a `--sync` too, when the parent copies every service of the
snapshot).

## Export and import

`--snapshots export <id>` writes a snapshot as one tar to stdout (`|
aws s3 cp - s3://...`: devopsy owns no storage of its own; takes,
releases and restores go on meanwhile, and a removal or pruning leaves
the snapshot being exported, said), and
`--snapshots import` stores one from stdin into the same environment,
on this server or another, checked whole (its tars as a restore checks
them, nothing but files and directories outside its code, its image
listing cut to the current release's services, each under that one's
repository), under a new id: on a new
server, release the project first, then import and restore it
(`--with-env` brings its secrets). Import only snapshots you trust: a
full restore runs the snapshot's code as the deploy user (its steps,
its compose file, its Dockerfiles), and `--with-env` applies its
variables to whatever code runs; a `--no-code` restore runs nothing of
it, its data checked as for any restore. Images stay out of it: a
restore pulls the pulled ones as their names give them now and builds
the built ones from the snapshot's code. `--snapshots remove <id>` removes one with the
images only it holds; `--destroy` removes them all, and its prompt says
how many.
