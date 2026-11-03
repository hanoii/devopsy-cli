# Data

Where an environment's data lives, who owns its files, and how its labels say what a copy carries and what stops meanwhile.

## Data and mounts

`.devopsy/mnt/` is devopsy's one data directory:
releases never upload it, and on servers it is a link to `shared/mnt`, one
per environment, so data outlives releases, rollbacks and pruning. Bind
mount its entries (`./mnt/<name>:/path`), one per concern (`storage`,
`database`, `logs`), never `./mnt` whole. The alternatives:

- Named volumes survive releases too (the compose project name stays
  the same) and take the ownership of the image's directory; `down
  --volumes` removes them. Harder to inspect and back up from the host.
- Absolute paths work, but devopsy knows nothing of them: keeping,
  backing up and removing them is the project's.

## Owners

`shared/mnt` starts empty and belongs to the deploy user, Docker creates
a missing bind mount's directory as root, and devopsy copies owners
exactly (rsync and tar as root, numeric ids): a container running as
non-root from its first process could not write there, nor write what
an import or a sync brought in with another machine's owners. So a
service says who owns the entries it writes:

```yaml
services:
  app:
    user: "${APP_UID:-10001}:${APP_UID:-10001}"
    volumes: [./mnt/storage:/app/storage, ./mnt/logs:/app/logs]
    labels:
      devopsy.mnt.owner: user
```

`devopsy.mnt.owner=user` takes the service's own `user:`, which must then
be `<uid>:<gid>` in numbers, as compose interpolates it: no `user:`, a
name or a uid alone (Docker would take the group from the image) is an
error wherever the labels are read, so the app and its data can never
drift apart. `devopsy.mnt.owner=<uid>:<gid>` names the owner itself, for
an image that drops privileges on its own and has no `user:`. Either
way numbers from 0 to 4294967294 (names cannot be resolved from the
host; `010001` is `10001`). The label covers every entry of `mnt` the
service mounts writable, in any profile; a read-only mount (`:ro`, a log
reader) claims none, and an entry no service owns stays as it is, the
deploy user's (right for what is written from the host). Without the
label nothing is owned, whatever `user:` says: an image with none, or
tools sharing a mount, would each claim it. devopsy applies it, as root in `devopsy-util` without a
network, never following a symbolic link, each entry in a container
that mounts that entry alone, so a directory the app swaps for a link
meanwhile can lead nowhere else. Only files, directories and links are
given away: a device node, a FIFO or a socket keeps its owner (devopsy
never makes one there, so it is the host's root's):

- a release, right after its switch (before a sync and the run
  step), and a rollback and the restart after a failed run, from the
  restored release's own labels: each owned entry missing is created
  (0755) and given to its owner, then every owned entry is scanned
  whole, so what devopsy did not write (an upload
  by hand, data from before the label) gets its owner too; a file a
  running service removes meanwhile is no failure. A failure fails the
  release as its run step would: the previous one comes back. In a
  rollback or a restart, labels that cannot be read (names from before
  a change, a compose error) are only a warning, nothing given away: a
  rollback never fails for its release's labels. Before its switch, a
  rollback makes sure `devopsy-util` is there when the release it
  restores owns entries, so an image pruned since fails it before
  anything changes;
- a sync: an entry the child owns lands as the child's owner, by the
  child's labels (`rsync --chown`), whatever the parent's files are;
- an `--import` of a `volumes` service: what it extracted goes to its
  owner before the import completes (locally on macOS nothing, as
  below: Docker Desktop's file sharing may refuse the chown);
- a reset, once it emptied `mnt`;
- locally, before `up`, `start`, `restart`, `run`, `create`,
  `watch` and `scale` (not with compose's `--dry-run`; one after `run`'s
  service is the container's command's, which starts): an owned entry missing is created,
  and on Linux an entry whose directory is not its owner's is given
  away, scanned whole; one whose directory is
  its owner's is never scanned, since this runs before every such
  command. On macOS, Docker Desktop and OrbStack let any container user
  write the Mac's files, so only the directories are made. Labels that
  cannot be read are only a warning: the command goes on, and compose
  says what is wrong with its files. So is an owned entry that is a
  symbolic link (an entry shared between worktrees): it is left as it
  is, and Docker mounts what it points to. The labels come from the project's
  compose files (`compose.yaml` and its override), never from one a
  command adds with `-f`, nor `COMPOSE_FILE`'s, which compose ignores
  for every devopsy command, since devopsy names its files with `-f`. In a release on a server these commands do
  none of it: each release's switch did.

Two services whose owners differ on one entry are refused; one without
the label may write an owned entry (a tool sharing it). A `custom`
import writes as the service's own user, and owners persist through
restarts and reboots.

## The mounts check

Before a release goes live, after its `prepare` steps (which may make
the secrets its compose file requires), the mounts check refuses
(nothing changes):

- a writable bind mount from inside the project's tree outside `mnt`
  (`./src:/app`, `./cache:/cache` in `.devopsy/`): it lives in the
  release, and the next one replaces it. Read-only ones, configuration
  shipped with the release, are fine, and so are files from `shared/`
  linked into it;
- data copied as `volumes` (`devopsy.data=true`, the default copy) on
  a service that mounts no entry of `mnt` and no named volume, or whose
  `devopsy.data.exclude` leaves all of them out, and `devopsy.mnt.owner`
  on one that mounts no entry of `mnt` writable: a sync would copy
  nothing of it, and a child's first release go live on empty data;
- one entry written by services with different owners;
- `./mnt` whole as a data or owned entry, refused wherever labels are
  read: it would carry every other service's data and give them one
  owner. Mounted writable by any other service too: it would write
  every entry, and no sync or import sees it there. Read-only (a tool
  reading every entry), devopsy ignores it;
- a bind mount from below an owned entry (`./mnt/storage/files` when
  `storage` is owned), by any service, read-only too: the entry's owner
  can replace that path with a symbolic link, and Docker resolves a
  bind mount's source on the host, so a link to `/` would mount the
  host's root. Bind the entry itself (`./mnt/storage:/app/storage`) and
  use the path below it inside the container;
- an owned entry that is a symbolic link in `shared/mnt`: Docker would
  mount what it points to, and devopsy give that to the owner;
- a bind mount from below an entry that a symbolic link in the entry
  leads out of it (`storage/public -> /`, made by hand or by a restored
  snapshot), rollbacks and restores too, and a sync whose copy brings
  one starts nothing on it: Docker would mount that of the host. On a
  server nothing in a release starts while one is there (an `up`, a
  `start`, a `run`, a `cp`, devopsy's own starts), whatever brought
  it, and what stops services to start them again (a snapshot, an
  export, a sync from the environment or into it but a `--reset`, an
  `--import` for the entries its tar does not replace) refuses before
  stopping them. A link
  within the entry, and an entry that is itself a link (on another
  disk), are fine.

A bind mount's source counts as written and as where its symbolic links
lead, since Docker mounts that: a link in the release to below an owned
entry, or one in `shared/mnt` into the release, is refused as what it
leads to (links into `shared/`, which is outside the release, are fine).

Labels the owners' step after the switch could not read (a value it
does not know) refuse the release here too. With an entry owned, it also builds `devopsy-util`
when the server lacks it, so a registry that cannot be reached refuses
the release before it goes live.

Absolute paths are not checked (a service with data's named volumes are
its data, below). `--destroy` empties
`shared/mnt` as root in `devopsy-util`, and forgets the history of the
images that ran on the data. Back up `shared/.env`, `shared/mnt`
(databases as dumps) and `.devopsy-data-images` beside them.

## Data and its labels

Everything devopsy does with data is an export
(a read) or an import (a replacement), of one service or of all of them
at once: `--export <service>` and `--import <service>` (below) move one;
a sync (a child's data from its parent's, [syncs](syncs.md)) exports every service
with data on the parent and imports each on the child. devopsy knows
nothing of what the data is: compose labels say it, three of them, read
on each side from its own release (and `devopsy.mnt.owner`, above, says
whose its files are).

`devopsy.data=true` says the service has data; `false`, or no label,
that it has none, neither copied nor touched. An empty value is an
error, not `false`: a variable left unset by accident
(`devopsy.data=${HAS_DATA}`) must not stop copying silently. Any other
value is an error. A service with data also says its data's version,
`devopsy.data.version` ([Data and its images](images.md#data-and-its-images)).

`devopsy.data.copy` says how a copy to another environment (a sync,
`--export`, `--import`) carries it:

- `volumes` (the default): the entries of `.devopsy/mnt` the service
  bind-mounts (`./mnt/files` gives `files`; a file, `./mnt/app.ini`,
  itself; never `./mnt` whole, refused). A sync rsyncs them (`rsync -a
  --no-D --delete --numeric-ids --modify-window=-1`, times compared to
  the nanosecond, the child's copy equal to the parent's, owners and
  modes included, but an entry the child owns, as its owner's:
  `devopsy.mnt.owner`, above); `--export` and `--import` move them as a
  tar. Both as root in `devopsy-util`. Device nodes, FIFOs and sockets
  are never copied, nor given to an owner: as root, rsync and tar would
  make them on the server, where a block device given to the app's UID
  is its raw disk. Its named volumes too (a database's datadir in
  `dbdata:/var/lib/mysql`), each by its compose key: copied into the
  other environment's volume of that key (`<project>_<key>`, the name
  compose gives it), which devopsy makes when missing as compose would
  (its driver, options and labels), and carried in a tar at its top
  level by the key, beside the entries. A volume keeps the owners it is
  copied with (its image's users; `devopsy.mnt.owner` owns entries of
  `mnt`). Refused on a service with data: an `external:` volume or one
  with a fixed `name:` (another project's, or every environment's), and
  a key named like an entry of `mnt` a service with data mounts.
- `custom`: the data capability's `export <service>` writes it to
  stdout (a database dump), and its `import <service>` reads it from
  stdin (a load), devopsy carrying the bytes.
- `none`: no copy carries it: a sync leaves the child's as it is, and
  `--export` and `--import` refuse it. It is still the service's data:
  its version is checked ([Data and its images](images.md#data-and-its-images)), and `--reset` empties it
  with the rest, its history starting over. A variable turns a service's copying off this way
  (`devopsy.data.copy=${DATA_DATABASE:-volumes}`, `DATA_DATABASE=none`).

An empty `devopsy.data.copy` is an error too; on a service without data
it is ignored, so a variable can flip `devopsy.data` and keep it.

`devopsy.data.exclude=<entry>[,<entry>...]` on a service with data names
entries of `mnt` or named volumes (by key) it mounts that are not
copied through it anyway: an access log next to the site's files
(`./mnt/logs`), a cache in a volume. On a service
without data it is ignored. The entries a service with data mounts,
and its named volumes' keys, cannot have whitespace in their names,
start with a dash or be `!` or `(`, nor an entry a `devopsy.mnt.owner`
owns: releases,
copies and snapshots refuse them.

`devopsy.data.copy.reflink=true` on a child's service copied as
`volumes` has its entries and volumes cloned from the parent's by
reflinks, where a sync runs on one server and its filesystem has them
(XFS, btrfs): its blocks are the parent's until either side writes, so
the copy is instant and the child takes next to no room. The child's
labels decide; the parent's own are ignored, as is the label on a
service without data or with another copy, and an empty value is an
error. For mostly static files, added to and rarely rewritten, like
uploads: never a database, which writes its pages in place, each page
it writes while a child shares it then landing elsewhere, so the
parent's files fragment for good (measured on XFS: some 5,600 extents
for 4,000 pages of 16 KiB written, where a file written in place keeps
one; `xfs_fsr` puts them back together). Whether the data clones into
the child is tried first, on one file: where nothing clones (another
filesystem) everything is rsynced, said, the child's entries untouched.
Otherwise the child's entry is emptied, never removed, then cloned, and
an owned one given to its owner (`devopsy.mnt.owner`). An entry that is
a symbolic link on either side, and one holding device nodes, FIFOs or
sockets (which no copy carries), is rsynced instead. Across servers it
is rsynced as any.

`devopsy.data.stop` says when devopsy stops the service while data
moves:

- `never` (the default): it keeps running.
- `own`: while its own data is exported or imported.
- `import`: while any data is imported.
- `always`: while any data is exported or imported.

And a service that `depends_on` a stopped one stops with it (every
`depends_on`, `required: false` and Compose's implicit ones included): it
cannot work without it anyway. `own` and `import` are not nested, and
`always` is both: the values are not a scale. A database copied as files
labeled `import` (reading as "stronger than `own`") is tarred while it
runs on `--export`, which exits 0 with a corrupt copy. `own` without a
`devopsy.data` label is an error (it would never stop); with `false` or a
copy of `none` (from a variable) it is ignored: no copy moves its data.

| operation                       | `never` | `own` | `import` | `always` |
| ------------------------------- | ------- | ----- | -------- | -------- |
| its own data exported           | up      | down  | up       | down     |
| its own data imported           | up      | down  | down     | down     |
| another service's data imported | up      | up    | down     | down     |
| another service's data exported | up      | up    | up       | down     |

Recipes:

- A database copied as files (`volumes`): `own`, never `import`. Its
  `--export` stops its app too (a dependent), held by a project's own
  service if it has one (devopsy-template-drupal11's `alwaysup`).
- A database copied as a dump (`custom`): `never`: the script needs the
  server running.
- Anything that uses other services' data while it runs, an app, a
  worker, a cron container: `import`. Its own files (`volumes`) can be
  exported while it runs.
- An app keeping its own SQLite file in `mnt`: `volumes` with `own`, or
  `custom` with `import` and a script using `sqlite3 .backup`.
- A cache (Redis): nothing; flush it in the project's `import` script if
  stale entries matter.
- A service that holds visitors while the app is down: nothing, and it
  must never `depends_on` the app, or it stops with it.

An operation over every service (a sync's side) takes the union, under
one `docker compose stop` (Compose orders it, dependents first) and one
`start` afterwards, waited for until healthy: with two `own` databases,
each is down while the other moves too. Only what runs is stopped, and
devopsy says what and why before it does (`stopping database (own), app
(depends on database)`); a dependent already down, or in a profile that
is not up, stays as it was. On an import, and on a child's side of a
sync, a running service that depends on an imported one and carries no
`devopsy.data.stop` label is named: label it `import`, or `never` to
keep it running and silence it. Nothing is paused: Traefik drops a
paused container's router, and `docker pause` flushes nothing. A service
kept down (by an import below, or a sync: [Logs and failures](syncs.md#logs-and-failures)) with `restart: always` is said: Docker starts it again
when its daemon starts, a reboot, on the data as it is; `unless-stopped`
keeps it down until devopsy starts it.

## `--export` and `--import`

`devopsy [@<target>] --export <service>`
writes a service's data to stdout, and `devopsy [@<target>] --import
<service>` replaces it with stdin, the way its copy label says: a
`custom` one through the capability's `export` and `import`, a `volumes`
one as a tar of its entries of `mnt` and its named volumes (made and
extracted as root in `devopsy-util`,
numeric owners and modes kept; an import empties each entry the tar
carries first, as a sync's `--delete` does, and extracts it in a
container without a network that mounts that entry alone, so a running
service that swaps a directory for a link meanwhile can lead the
extraction nowhere else; a volume the same way, made first when
missing; an entry its labels own is given to its owner once
extracted). What its stop labels say is
stopped around either (an export: the service if `own` or `always`, every
`always` one; an import: the service if `own`, `import` or `always`, every
`import` and `always` one; with what depends on them), and comes back
after. A `custom` service not stopped is brought up for its import, alone
(`--no-deps`), and down again after it if it was down. A service without
data, or whose copy is `none`, is refused. Production's database and files on a laptop, with no
project command:

```sh
devopsy @prod --export database | devopsy --import database
devopsy @prod --export storage | devopsy --import storage
devopsy @prod --export database > database.sql     # a backup
devopsy @staging --import database < database.sql   # asks for staging's full name (--yes in CI)
```

`--import` spools stdin to a file first (`.devopsy-sync.stream.<service>.*`,
under `.devopsy/` locally, which releases never upload; next to the
environment on a server) and changes nothing until it is all there; a
truncated tar (cut anywhere, or missing its end-of-archive blocks),
or one carrying entries the service does not mount or excludes, is
refused, and so is one carrying `mnt` itself (`./`: make it with `tar -C
mnt -cf - <entry>...`), a member reaching out of its entry (a path, a
hard link, a member below a link), a link that makes a bind source of a
service lead out of its entry (judged by where it leads through the
tar's other links, as the host would resolve it: Docker follows it
there; any other link is the data's own, wherever it points, as a sync
copies it), or a device node or a FIFO anywhere (as root, tar would make
them on the server; `--export` leaves them out). On a target, `--export`
holds the release lock shared and the environment's sync lock (a sync from
it waits), `--import` the release lock, and asks for the environment's
full name on the terminal unless `--yes` (stdin is the data); one silent
for 2 minutes counts as cut. `--export` of data a sync or an import left
partial warns and goes on. A pipe cannot tell an export that failed
halfway from one that ended, so for what matters, go through a file and
check the export's exit code.

## An import that fails

Nothing is replaced until the data has fully
arrived; from the first replacement until the import completes, the
environment is marked and what devopsy stopped stays down.

- Before the import starts writing (devopsy asks Docker to start the
  hook, or the first container that empties and extracts), a failure, a
  Ctrl-C or a session gone restores what was stopped, and nothing is
  imported. On a target devopsy waits for a word from your session right
  before that write, so a session gone since the data came imports
  nothing.
- From then on, on a target, the import goes on whatever happens to your
  session (a Ctrl-C says so, a second one ends the session, and `--status`
  shows it), its output also in `<environment>/.devopsy-sync.log`. The
  environment is marked first, and what was stopped recorded, so a
  devopsy killed or a server rebooted mid-load leaves them too. A
  failure (a Docker error that started nothing included) ends whatever
  may still write and keeps it all down: what was stopped, the imported
  service and what depends on it. Nothing runs on that data: a release
  or a rollback refuses (a release before its upload), the restart after
  a failed release is skipped, `--status` says so and `--export` warns;
  only that service can be imported until one completes. The ways out:
  the import again from a complete file (`devopsy @prod --import
  database < database.sql`), which starts what the import kept down and
  only then removes the mark; on a child, a `--sync` that copies all of
  that service (every entry the child's service mounts: one the parent
  lacks would keep what the import half wrote, so the sync refuses
  first, naming it) or a `--reset`, which starts it over from its
  parent; or `--resolved`, below. The next import, or `--resolved`,
  first ends a load a killed devopsy left running. `--destroy` still
  works, its `destroy:` step running with what the import kept down
  still down.
- Locally there is no mark: the error says what it left down and prints
  `devopsy start ...` to bring it back.

When the dump itself cannot complete (an error near its end, every retry
the same), `devopsy @<target> --import <service> --resolved` accepts the
data as it is: what is missing does not matter, or you fixed it by hand
(`devopsy @<target> up -d --no-deps <service>`, then `--shell
<service>`). It reads no stdin and loads nothing; it asks for the
environment's full name, saying what it starts (`--yes` without a
terminal), gives the entries a `volumes` service's labels own to their
owners (as the import would have), starts what the import kept down,
the service included, and only once all of it is healthy removes the
mark. A start that fails keeps
it, and so does a failure to derive what runs on the data (a held image
given back with the registry out of reach): fix it and run it again. On a target only, and never over a sync's
mark: a sync's partial copy is completed only by a sync, which replaces
an import's mark when it is cut too. A `--release-sync` of a child marked
by an import whose parent does not copy that service fails at its push,
after the switch, and goes back: import or `--resolved` first. A project
command that runs `devopsy --export` itself, on a server whose devopsy is
older than the labels, fails without the upgrade line.
