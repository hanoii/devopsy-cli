# Data

Where an environment's data lives, who owns its files, how labels say what
a copy carries and what stops meanwhile, and how one service's data moves
with `--export` and `--import`.

## Where data lives

`.devopsy/mnt/` is devopsy's one data directory. Releases never upload it,
and on a server it is a link to `shared/mnt`, one per environment, so data
outlives releases, rollbacks and pruning. Bind-mount its entries
(`./mnt/<name>:/path`), one per concern (`storage`, `database`, `logs`),
never `./mnt` whole.

The alternatives:

- Named volumes survive releases too, since the compose project name stays
  the same, and take the ownership of the image's directory. `down
  --volumes` removes them. They are harder to inspect and back up from the
  host.
- Absolute paths work, but devopsy does not manage them. Keeping, backing
  up and removing them is the project's job.

`--destroy` empties `shared/mnt` as root in `devopsy.internal/util`, and forgets
the history of the images that ran on the data. Back up `shared/.env`,
`shared/mnt` (databases as dumps) and `.devopsy-data-images` beside them.

## Data labels

Everything devopsy does with data is an export (a read) or an import (a
replacement), of one service or of all of them at once. `--export
<service>` and `--import <service>` move one (below). A sync, which
copies a child's data from its parent's ([Parent environments and
syncs](syncs.md)), exports every service with data on the parent and
imports each on the child.

Compose labels tell devopsy what the data is. Each side of a copy reads
the labels from its own release, so the two sides' labels can differ.

### `devopsy.data`

`devopsy.data=true` says the service has data. `false`, or no label, says
it has none, and devopsy neither copies nor touches it. An empty value is
an error rather than `false`, so a variable left unset by accident
(`devopsy.data=${HAS_DATA}`) cannot silently stop the copying. Any other
value is an error too.

A service with data must also say its data's version,
`devopsy.data.version` ([Data and its images](images.md#data-and-its-images)).

### `devopsy.data.copy`

How a copy to another environment (a sync, `--export`, `--import`)
carries the data:

- `volumes`, the default: the entries of `.devopsy/mnt` the service
  bind-mounts (`./mnt/files` gives `files`; a file, `./mnt/app.ini`, is
  its own entry; `./mnt` whole is refused), and its named volumes.
- `custom`: the data capability's `export <service>` writes the data to
  stdout (a database dump) and its `import <service>` reads it from stdin
  (a load), and devopsy carries the bytes between them
  ([the data capability](capabilities.md#data)).
- `none`: no copy carries it. A sync leaves the child's data as it is,
  and `--export` and `--import` refuse the service. It is still the
  service's data: its version is checked, and `--reset` empties it with
  the rest, its history starting over. A variable can turn a service's
  copying off this way (`devopsy.data.copy=${DATA_DATABASE:-volumes}`,
  `DATA_DATABASE=none`).

An empty `devopsy.data.copy` is an error. On a service without data the
label is ignored, so a variable can flip `devopsy.data` and leave it in
place.

A `volumes` copy:

- A sync copies the entries with rsync (`rsync -a --no-D --delete
  --numeric-ids --modify-window=-1`, times compared to the nanosecond).
  The child's copy then equals the parent's, with its modes, and its
  owners but devopsy's own uids, which take the child's ([Owners in
  copies](#owners-in-copies)), and an entry the child owns, which gets
  the child's owner (Owners, below). `--export` and `--import` move them
  as a tar. Both run as root in `devopsy.internal/util`.
- Device nodes, FIFOs and sockets are never copied, nor given to an
  owner. As root, rsync and tar would make them on the server, where a
  block device given to the app's UID is its raw disk.
- Named volumes (a database's datadir in `dbdata:/var/lib/mysql`) are
  copied by their compose key, into the other environment's volume of
  the same key (`<project>_<key>`, the name compose gives it). devopsy
  creates that volume when it is missing, with its driver, options and
  labels, as compose would; one with options (a share's, its password
  among them) through the project's own compose, which also creates its
  services' containers, stopped, so the options never show on a command
  line. In a tar, a volume sits at the top level
  under its key, beside the entries.
- A volume keeps the owners it is copied with (its image's users), but
  devopsy's own uids, which take the receiving side's ([Owners in
  copies](#owners-in-copies)); `devopsy.mnt.owner` owns only its root
  (Owners, below).
- On a service with data, devopsy refuses an `external:` volume and one
  with a fixed `name:` (another project's, or every environment's), and
  a volume key named like an entry of `mnt` that a service with data
  mounts.

### `devopsy.data.exclude`

`devopsy.data.exclude=<entry>[,<entry>...]`, on a service with data,
names entries of `mnt` or named volumes (by key) that it mounts but that
are not to be copied through it: an access log next to the site's files
(`./mnt/logs`), a cache in a volume. On a service without data it is
ignored.

The entries a service with data mounts, and its named volumes' keys,
cannot contain whitespace, start with a dash or be `!` or `(`, and
neither can an entry a `devopsy.mnt.owner` owns: releases, copies and
snapshots refuse them.

### `devopsy.data.copy.reflink`

`devopsy.data.copy.reflink=true` on a child's service copied as `volumes`
clones its entries and volumes from the parent's by reflinks, when the
sync runs on one server whose filesystem has them (XFS, btrfs). The
child's blocks are the parent's until either side writes, so the copy is
instant and takes next to no room.

The child's labels decide; the parent's are ignored, and so is the label
on a service without data or with another copy. An empty value is an
error.

Use it for mostly static files, added to and rarely rewritten, like
uploads. Do not use it for a database. A database writes its pages in
place, so each page it writes while a child shares the file lands
somewhere else, and the parent's files fragment for good. On XFS, a
database writing 4,000 pages of 16 KiB into a file a child shared made
some 5,600 extents, where a file written in place keeps one; `xfs_fsr`
puts them back together.

How the clone works:

- devopsy first tries one file. Where nothing clones (another
  filesystem), everything is rsynced instead, with a message, and the
  child's entries are not emptied first.
- Otherwise the child's entry is emptied, never removed, then cloned, and
  an owned one is given to its owner.
- An entry that is a symbolic link on either side, and one holding device
  nodes, FIFOs or sockets (which no copy carries), is rsynced instead.
- Across servers everything is rsynced.

### `devopsy.data.stop`

When devopsy stops the service while data moves:

- `never`, the default: it keeps running;
- `own`: while its own data is exported or imported;
- `import`: while any data is imported;
- `always`: while any data is exported or imported.

| operation                       | `never` | `own` | `import` | `always` |
| ------------------------------- | ------- | ----- | -------- | -------- |
| its own data exported           | up      | down  | up       | down     |
| its own data imported           | up      | down  | down     | down     |
| another service's data imported | up      | up    | down     | down     |
| another service's data exported | up      | up    | up       | down     |

A service that `depends_on` a stopped one stops with it, through every
kind of `depends_on`, including `required: false` and Compose's implicit
ones, since it cannot work without it.

The values do not form a scale. `own` and `import` do not contain each
other, and `always` is both. A database copied as files and labeled
`import`, which reads as "stronger than `own`", is tarred while it runs
on `--export`, which then exits 0 with a corrupt copy.

`own` without a `devopsy.data` label is an error, because it would never
stop anything. With `devopsy.data=false` or a copy of `none` (from a
variable), it is ignored, as no copy moves that data.

Recipes:

- A database copied as files (`volumes`): `own`, never `import`. Its
  `--export` stops its app too, as a dependent, and a project's own
  service can hold visitors meanwhile (devopsy-template-drupal11's
  `alwaysup`).
- A database copied as a dump (`custom`): `never`, since the script needs
  the server running.
- Anything that uses other services' data while it runs (an app, a
  worker, a cron container): `import`. Its own files (`volumes`) can be
  exported while it runs.
- An app keeping its own SQLite file in `mnt`: `volumes` with `own`, or
  `custom` with `import` and a script using `sqlite3 .backup`.
- A cache (Redis): nothing. Flush it in the project's `import` script if
  stale entries matter.
- A service that holds visitors while the app is down: nothing, and it
  must never `depends_on` the app, or it stops with it.

How the stops happen:

- An operation over every service (a sync's side) takes the union of
  what each needs, under one `docker compose stop` (Compose orders it,
  dependents first) and one `start` afterwards, waited for until healthy.
  With two `own` databases, each is down while the other moves too.
- Only what runs is stopped, and devopsy says what and why before it does
  (`stopping database (own), app (depends on database)`). A dependent
  already down, or in a profile that is not up, stays as it was.
- On an import, and on a child's side of a sync, a running service that
  depends on an imported one and has no `devopsy.data.stop` label is
  named: label it `import`, or `never` to keep it running and silence the
  notice.
- Services are stopped, not paused. Traefik drops a paused container's
  router, and `docker pause` flushes nothing.
- A service kept down (by an import that failed, below, or a sync:
  [When a sync fails](syncs.md#when-a-sync-fails)) that has `restart:
  always` is named: Docker starts it again when its daemon starts, after a
  reboot, on the data as it is. `unless-stopped` keeps it down until
  devopsy starts it.

## Owners

A container that runs as non-root from its first process cannot write
into `mnt` on its own, nor write what an import or a sync brings in.
`shared/mnt` starts empty and belongs to the deploy user, Docker creates
a missing bind mount's directory as root, and devopsy copies owners
exactly (rsync and tar as root, numeric ids), which gives what an import
or a sync brings another machine's owners. A service declares who owns
the entries it writes:

```yaml
services:
  app:
    user: "${DEVOPSY_UID:?}:${DEVOPSY_UID:?}"
    volumes: [./mnt/storage:/app/storage, ./mnt/logs:/app/logs]
    labels:
      devopsy.mnt.owner: user
```

- `devopsy.mnt.owner=user` takes the service's own `user:`, which must
  then be `<uid>:<gid>` in numbers, as compose interpolates it. No
  `user:`, a name, or a uid alone (Docker would take the group from the
  image) is an error wherever the labels are read, so the app and its
  data can never drift apart.
- `devopsy.mnt.owner=<uid>:<gid>` names the owner itself, for an image
  that drops privileges on its own and has no `user:`. That uid is the
  same in every environment, so the service shares it with every other
  environment's: a `user:` of `DEVOPSY_UID` keeps environments apart
  where the image allows it.

Either way the owner is given in numbers, from 0 to 4294967294, since
names cannot be resolved from the host; `010001` is `10001`.

The label covers every entry of `mnt` the service mounts writable, in any
profile. A read-only mount (`:ro`, a log reader) claims none. An entry no
service owns stays as it is, the deploy user's, which is right for what
is written from the host. Without the label nothing is owned, even when
the service has a `user:`. Otherwise an image with no `user:`, or tools
sharing a mount, would each claim it.

Two services with different owners on one entry are refused. A service
without the label may write an owned entry (a tool sharing it).

The label also owns the named volumes the service mounts writable, their
root alone: devopsy makes such a volume when it is missing and gives its
root to the owner, and what the service writes there is the service's
own. Docker fills an empty volume from the image when a container first
mounts it, and gives its root the owner of the image's directory there,
so every service's mount of an owned volume takes `nocopy`, read-only
mounts and services without the label included:

```yaml
services:
  database:
    user: "${DEVOPSY_UID:?}:${DEVOPSY_UID:?}"
    volumes: ["dbdata:/var/lib/mysql:nocopy"]
    labels:
      devopsy.mnt.owner: user
volumes:
  dbdata: {}
```

The long syntax says it as `volume: {nocopy: true}`. A mount of an owned
volume without it is refused, before a release goes live and locally
before a start. What the image ships at that path is not copied into the
volume then; a database makes its own. An owned volume that is
`external:` or has a fixed `name:` is refused (another project's, or
every environment's), and so are two services with different owners on
one volume. A volume whose driver refuses the chown (a share's may) fails
the step that gives it.

devopsy gives the entries away as root in `devopsy.internal/util`, without a
network and never following a symbolic link. Each entry is done in a
container that mounts that entry alone, so a directory the app swaps for
a link meanwhile cannot lead anywhere else. Only files, directories and
links are given away. A device node, a FIFO or a socket keeps its owner,
the host's root, since devopsy never makes one there.

An owner that changes (another `user:`, another uid, a label changed)
stops what runs on it first. Before devopsy gives away an entry whose
directory has another owner, or an owned volume whose root has another,
it stops the running services that mount it and those that depend on
them, saying so (`stopping database (its data changes owner), app
(depends on database)`), and the run step, or the local command, starts
them again. A service left running as the old owner would lose its own
files under it: MariaDB's shutdown, for one, cannot remove its `tc.log`
then, and it refuses the next start. An entry or a root that is its
owner's already stops nothing.

Where owners are applied:

- A release, right after its switch, before a sync and the run step; a
  rollback, and the restart after a failed run, from the restored
  release's own labels. Each owned entry that is missing is created
  (0755) and given to its owner, then every owned entry is scanned whole,
  so what devopsy did not write (an upload by hand, data from before the
  label) gets its owner too. A file a running service removes meanwhile
  is no failure. Each owned named volume, made first when missing, has
  its root given again, which also mends one Docker made before. A
  failure fails the release as its run step would, and the previous one
  comes back.
  - In a rollback or a restart, labels that cannot be read (names from
    before a change, a compose error) are only a warning and nothing is
    given away: a rollback never fails for its release's labels.
  - Before its switch, a rollback makes sure `devopsy.internal/util` is there when
    the release it restores owns entries, so an image pruned since fails
    it before anything changes.
- A sync: an entry the child owns lands with the child's owner, by the
  child's labels (`rsync --chown`), regardless of the owners on the
  parent's files.
- An `--import` of a `volumes` service: what it extracted goes to its
  owner before the import completes; locally on macOS, nothing, since
  Docker Desktop's file sharing may refuse the chown.
- A reset, once it has emptied `mnt`, its owned volumes' roots too.
- Locally, before `up`, `start`, `restart`, `run`, `create`, `watch` and
  `scale`, but not with compose's `--dry-run`. A `--dry-run` after
  `run`'s service belongs to the container's command, and that run does
  start, so it gets the owners. A missing owned entry is created. On
  Linux, an entry whose directory is not its owner's is given away and
  scanned whole; one whose directory is its owner's is never scanned,
  since this runs before every such command. On macOS, Docker Desktop
  and OrbStack let any container user write the Mac's files, so only the
  directories are made. An owned named volume is made when missing and
  its root given, on macOS as on Linux: a volume is Docker's own, and
  nothing fakes its owners.
  - Labels that cannot be read are only a warning: the command goes on,
    and compose says what is wrong with its files. So is an owned entry
    that is a symbolic link (an entry shared between worktrees): it is
    left as it is, and Docker mounts what it points to. A mount of an
    owned volume without `nocopy` stops the command: the start would
    fill the volume and leave its root the image's.
  - The labels come from the project's compose files (`compose.yaml` and
    its override), never from a file a command adds with `-f`, nor from
    `COMPOSE_FILE`, which compose ignores for every devopsy command since
    devopsy names its files with `-f`.
  - In a release on a server these commands do none of this: each
    release's switch did.

A `custom` import writes as the service's own user. Owners persist
through restarts and reboots.

### Owners in copies

Data copied from another environment carries that environment's uids
([uids](servers.md#uids)), which no container of the receiving one runs
as. The receiving side of every copy, a sync by rsync or by reflinks, an
`--import` and a snapshot's restore, gives each file owned by an
environment's uid (2,000,000,000 to 2,099,999,999) its own
`DEVOPSY_UID`, and each one owned by a project's (1,900,000,000 to
1,999,999,999) its own `DEVOPSY_PROJECT_UID`, the group the same way and
apart from the owner. Every other owner stays as it is: root, an image's
own users, anything below those ranges. The range alone says the kind,
so nothing of the sending side's numbers is carried, a tar made by hand
or by an older devopsy is mapped alike, and data left on an old uid
takes the right one at its next copy.

- A sync's rsync maps them as it writes (`--usermap` and `--groupmap`);
  an entry the child owns gets the child's owner instead (`--chown`).
- A clone by reflinks, an import's extraction and a restore's are
  mapped right after, in the same container (`find` and `chown -h`),
  only the files whose owner differs: `chown` clears a setuid or setgid
  bit on a file it changes. A restore of the environment's own data
  takes one pass over it, changing nothing.
- Locally on macOS an import maps its volumes, not its entries: Docker
  Desktop and OrbStack show bind mounts' files as any owner.

## The mounts check

Before a release goes live, after its `prepare` steps (which may make the
secrets its compose file requires), the mounts check refuses the release,
with nothing changed, for:

- A writable bind mount from inside the project's tree, outside `mnt`
  (`./src:/app`, `./cache:/cache` in `.devopsy/`): it lives in the
  release, and the next release replaces it. Read-only ones,
  configuration shipped with the release, are fine, and so are files from
  `shared/` linked into it.
- Data copied as `volumes` (`devopsy.data=true`, the default copy) on a
  service that mounts no entry of `mnt` and no named volume, or whose
  `devopsy.data.exclude` leaves all of them out; and `devopsy.mnt.owner`
  on a service that mounts no entry of `mnt` or named volume writable. A
  sync would copy nothing of it, and a child's first release would go
  live on empty data.
- One entry, or one named volume, written by services with different
  owners.
- A mount of an owned named volume without `nocopy`, and an owned volume
  that is `external:` or has a fixed `name:` (Owners, above).
- `./mnt` whole as a data or owned entry, refused wherever labels are
  read: it would carry every other service's data and give them one
  owner. Mounted writable by any other service it is refused too: that
  service would write every entry, where no sync or import sees it.
  Mounted read-only (a tool reading every entry), devopsy ignores it.
- A bind mount from below an owned entry (`./mnt/storage/files` when
  `storage` is owned), by any service, read-only too. The entry's owner
  can replace that path with a symbolic link, and Docker resolves a bind
  mount's source on the host, so a link to `/` would mount the host's
  root. Bind the entry itself (`./mnt/storage:/app/storage`) and use the
  path below it inside the container.
- An owned entry that is a symbolic link in `shared/mnt`: Docker would
  mount what it points to, and devopsy would give that to the owner.
- A bind mount from below an entry, when a symbolic link in the entry
  leads out of it (`storage/public -> /`, made by hand or by a restored
  snapshot): Docker would mount that part of the host. A link within the
  entry, and an entry that is itself a link (to another disk), are fine.

That last check also runs outside releases. Rollbacks and restores refuse
such a link, and a sync whose copy brings one starts nothing on it. On a
server, nothing in a release starts while one is there (an `up`, a
`start`, a `run`, a `cp`, devopsy's own starts), however it got there.
What stops services in order to start them again refuses before it stops
them: a snapshot, an export, a sync from the environment or into it
(except a `--reset`), and an `--import` for the entries its tar does not
replace.

A bind mount's source is checked both as written and as where its
symbolic links lead, since Docker mounts the latter. A link in the
release to below an owned entry, or one in `shared/mnt` into the release,
is refused as what it leads to. Links into `shared/`, which is outside the
release, are fine.

Labels that the owners' step after the switch could not read (a value it
does not know) refuse the release here too. With an entry owned, the
check also builds `devopsy.internal/util` when the server lacks it, so a registry
that cannot be reached refuses the release before it goes live.

Absolute paths are not checked. A service with data's named volumes are
its data (Data labels, above).

## `--export` and `--import`

`devopsy [@<target>] --export <service>` writes a service's data to
stdout, and `devopsy [@<target>] --import <service>` replaces it with
stdin, the way its copy label says. Production's database and files on a
laptop, with no project command:

```sh
devopsy @prod --export database | devopsy --import database
devopsy @prod --export storage | devopsy --import storage
devopsy @prod --export database > database.sql     # a backup
devopsy @staging --import database < database.sql   # asks for staging's full name (--yes in CI)
```

- A `custom` service goes through the data capability's `export` and
  `import`.
- A `volumes` service goes as a tar of its entries of `mnt` and its named
  volumes, made and extracted as root in `devopsy.internal/util`, numeric owners
  and modes kept. An import empties each entry the tar carries first, as
  a sync's `--delete` does, and extracts it in a container without a
  network that mounts that entry alone, so a running service that swaps a
  directory for a link meanwhile cannot lead the extraction anywhere else.
  Volumes go the same way, created first when missing. An entry its
  labels own is given to its owner once extracted.
- A service without data, or whose copy is `none`, is refused.

Around either, devopsy stops what the stop labels say, with what depends
on it, and starts it again after:

- an export stops the service if it is `own` or `always`, and every
  `always` one;
- an import stops the service if it is `own`, `import` or `always`, and
  every `import` and `always` one.

A `custom` service that is not stopped is brought up for its import,
alone (`--no-deps`), and down again afterwards if it was down.

`--import` spools stdin to a file first, and changes nothing until it is
all there: `.devopsy-sync.stream.<service>.*`, under `.devopsy/` locally
(which releases never upload), beside the environment on a server. It
refuses a tar that is:

- truncated, cut anywhere or missing its end-of-archive blocks;
- carrying entries the service does not mount or excludes, or `mnt`
  itself (`./`: make the tar with `tar -C mnt -cf - <entry>...`);
- carrying a member that reaches out of its entry: by its path, a hard
  link, or a member below a link;
- carrying a link that makes a service's bind source lead out of its
  entry, judged by where it leads through the tar's other links as the
  host would resolve it, since Docker follows it there. Any other link
  belongs to the data, wherever it points, and is kept as a sync would
  copy it.
- carrying a device node or a FIFO anywhere: as root, tar would make
  them on the server. `--export` leaves them out.

On a target:

- `--export` holds the release lock shared and the environment's sync
  lock, so a sync from it waits; `--import` holds the release lock.
- `--import` asks for the environment's full name on the terminal, since
  stdin is the data, unless `--yes`. An import silent for 2 minutes
  counts as cut.
- `--export` of data that a sync or an import left partial warns, and
  goes on.

A pipe cannot tell an export that failed halfway from one that ended. For
data that matters, go through a file and check the export's exit code.

## An import that fails

Nothing is replaced until the data has fully arrived. From the first
replacement until the import completes, the environment is marked, and
what devopsy stopped stays down.

Until the import starts writing (when devopsy asks Docker to start the
hook, or starts the first container that empties and extracts), a
failure, a Ctrl-C or a lost session starts again what was stopped, and
nothing is imported. On a target, devopsy waits for your session to
confirm right before that write, so a session lost after the data arrived
imports nothing.

From then on, on a target, the import keeps going if your session is
cut. A Ctrl-C only prints a message, a second one ends the session, and
`--status` shows the import. Its output also goes to
`<environment>/.devopsy-sync.log`. The environment is marked first, and
what was stopped is recorded, so a devopsy killed or a server rebooted
mid-load leaves both behind too.

A failure, including a Docker error that started nothing, ends whatever
may still write and keeps everything down: what was stopped, the
imported service and what depends on it. Nothing runs on that data: a
release or a rollback refuses (a release before its upload), the restart
after a failed release is skipped, `--status` says so, `--export` warns,
and only that service can be imported until an import completes.

To clear the mark:

- the import again from a complete file (`devopsy @prod --import
  database < database.sql`), which starts what the import kept down and
  only then removes the mark;
- on a child, a `--sync` that copies all of that service, every entry the
  child's service mounts (one the parent lacks would keep what the import
  half wrote, so the sync refuses first, naming it), or a `--reset`,
  which starts it over from its parent;
- `--resolved`, below.

The next import, or `--resolved`, first ends a load that a killed devopsy
left running. `--destroy` still works: its `destroy:` step runs with what
the import kept down still down.

Locally there is no mark: the error says what it left down, and prints the
`devopsy start ...` that brings it back.

### `--resolved`

When the dump itself cannot complete (an error near its end, the same on
every retry), `devopsy @<target> --import <service> --resolved` accepts
the data as it is: what is missing does not matter, or you fixed it by
hand (`devopsy @<target> up -d --no-deps <service>`, then `--shell
<service>`).

It reads no stdin and loads nothing. It asks for the environment's full
name, saying what it will start (`--yes` without a terminal). Then it
gives the entries a `volumes` service's labels own to their owners, as
the import would have, starts what the import kept down, the service
included, and removes the mark only once all of it is healthy. A start
that fails keeps the mark, and so does a failure to work out what runs on
the data (a held image given back with the registry out of reach): fix
it and run it again.

It works on a target only, and never over a sync's mark: only a sync
completes a sync's partial copy, and a sync that is cut too replaces an
import's mark.

A `--release-sync` of a child marked by an import, whose parent does not
copy that service, fails at its push, after the switch, and goes back:
import or `--resolved` first.

A project command that runs `devopsy --export` itself, on a server whose
devopsy is older than the labels, fails without the upgrade line.
