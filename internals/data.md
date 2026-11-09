# Data, destroying and removing

Code: `cmd/devopsy/mnt.go` (owners, the start gate), `cli.MntOwners` and `cli.CheckMounts` in `internal/cli/sync.go`, `ensureUtilImage` in `cmd/devopsy/sync.go` and `UtilDockerfile` in `internal/remote/sync.go`, `runDestroyData` in `cmd/devopsy/release_local.go`, devopsy's containers in `cmd/devopsy/writers.go`, `cmd/devopsy/releases.go`, and `DestroyScript`, `LeftoverScript` and `ReleasesRemoveScript` in `internal/remote/remote.go`.

## mnt and owners

- `.devopsy/mnt/` is devopsy's one data directory: never uploaded, `shared/mnt` made by the upload and linked into each release like every `shared/` entry.
- What is in `mnt` and who owns it is the project's: only `--destroy`, an `--import` (its entries) and a sync into a child remove anything there, and only the owner label changes an owner.
- `devopsy.mnt.owner=user` or `=<uid>:<gid>` (`cli.MntOwnerLabel`) owns a service's writable entries of `mnt`, read with the data labels in every profile into `Owner` and `Writable`, `cli.MntOwners` mapping entry to owner: Docker makes a missing entry root's, the deploy user cannot chown, and an import, a sync, a restore or an upload by hand keeps the owners it came with, so without it a non-root app cannot write its entries short of a root step in the project (an entrypoint's chown, an init service).
- Owners are numbers only, 0 to 4294967294, normalized by `parseOwner` and compared as numbers: names cannot be resolved from the host, and past a `uid_t` or at 4294967295 (chown's -1) an owner passes the checks and fails `find` and `chown` after the switch.
- A read-only mount claims nothing, two owners on one entry are an error, and an unlabeled writer of an owned entry is none (a tool sharing it).
- The value `user` takes the service's own `user:` (`cli.OwnerUser`, `labelOwner`), which must be `<uid>:<gid>` in numbers: a name cannot be resolved from the host, and a uid alone leaves the group to the image.
- `user` is resolved where labels are parsed, so every consumer and the child's list see numbers, never the word.
- The label stays opt-in, never a default from `user:`: some images have no `user:` and drop privileges themselves, and a tool sharing a mount would claim it too.
- Owners are applied as root in `UtilImage`, one container per owned entry mounting that entry alone (`ownMnt`, `--mount` with its source as a CSV field), `--network none`, `chown -h` in batches of only what differs (`chownEntry`), only files, directories and links in the chown and in the look after it: one container per entry keeps a directory the app swaps for a link during the scan from leading the chown into another entry or the host, and a device node given to the owner would be the server's raw disk.
- A missing owned entry is made by devopsy first, 0755 (`makeEntry`); one that is a link is refused.
- A scan that fails is looked at again with `-ignore_readdir_race`, chowning nothing, and fails only when something is left with another owner or that look fails itself (a directory find cannot read): the services may run meanwhile, and a file removed between find and chown is no failure.
- Every owned entry is scanned whole on a server: what devopsy did not write can have the wrong owner deep under a correct top.
- On a server owners are given at these points:
  - by `--mnt-owners` in a release, right after `make_current` (it changes live data, so never before; a failure counts as a failed run step; signals ignored as `runImages` ignores them);
  - by `--mnt-owners restored` in a rollback and in the restart after a failed run, from the restored release's labels, where labels this devopsy cannot read, a compose error and `./mnt` whole only warn and nothing is given away, while a chown that fails on labels it read fails as a release's does;
  - by a sync's rsync, `--chown` per entry from the child's labels (internals/syncs.md);
  - by a `volumes` import and a snapshot's restore, after their extracting containers (internals/export-import.md, internals/snapshots.md);
  - by a reset, after `emptyMnt`.
- Locally, before compose's `up`, `start`, `restart`, `run`, `create`, `watch` and `scale` (`Plan.Starts`), devopsy makes the missing owned entries and, on Linux, gives away and scans whole an entry whose top has the wrong owner, never one whose top is right; macOS never checks, since Docker Desktop and OrbStack show containers the owner Docker asks for.
- `Plan.Starts` reads the labels from the project's own compose files (`ComposeConfig`), never a `-f` the command adds; never for a `--dry-run` where compose parses it (before the command, after it up to `--`, for `run` only before its service: `commandValueFlags`, `dryRun`); never in a release on a server (`cli.InRelease`), where `--mnt-owners` owns it, so a rollback's plain `up` never fails on labels `--mnt-owners restored` only warns about.
- Locally, labels that cannot be read and an owned entry that is a symbolic link are a warning and compose's command goes on: a label devopsy reads must never stop compose.
- No owners are fixed for a stream's import (it writes as the service's user), restarts or reboots (owners persist), and no flag fixes owners: a release does it.
- The owners in copies are a named feature, `mnt-owner` found by `--sync-push`, `--import` and `--sync-services list`: an older parent would rsync its own owners, an older child list none, an older server's import keep the tar's; a release's `--mnt-owners` and `--check-mounts` are flags of their own, so an activation on an older server refuses by them.

## The mounts check

- `--check-mounts` (`cli.CheckMounts`) runs on the server after the prepare steps and `--missing-vars`, beside `--check-names` (internals/releases.md), and refuses a release before its switch for:
  - a writable bind from the project's tree outside `mnt` (`TreeBinds`), which goes with the next release;
  - a service copied as `volumes` with no entry of `mnt` and no named volume, which a sync would take for nothing to copy;
  - an owner of no writable entry;
  - two owners on one entry;
  - `./mnt` whole mounted writable by any service (`WholeMnt`);
  - a bind source below an owned entry (`Binds`);
  - an owned entry that is a symbolic link in `shared/mnt` (`ownedLinks`);
  - a bind source below an entry that a link in the entry leads out of (`LeavingBinds`, the entry resolved first, so one on another disk is its own place), or that the deploy user cannot resolve (`HiddenBinds`, below).
- Labels are read exactly as `--mnt-owners` reads them after the switch (`cli.SyncServices`): a label that step cannot read refuses here, never after the switch.
- A compose config that cannot be read (`cli.ErrComposeConfig`) is no refusal, as for `--check-names`: after `--missing-vars` that is a broken file, which the run step names.
- Each bind source counts as written and as where its links lead (`resolvePath`: `filepath.EvalSymlinks` where it exists, else its longest existing prefix, a link whose target is missing followed all the same), both forms for `Binds` and the resolved one alone for `TreeBinds` and `WholeMnt`; links into `shared/` lead out of the release tree and stay fine.
- A bind source that reaches `mnt` through links (for example the tree's `files -> .devopsy/mnt/storage/files`) is judged by where it lands in `mnt`, before the links in `mnt` are followed (`throughMnt`).
- A bind source below an entry that the deploy user cannot resolve (a directory on the way it cannot search) is refused as one leading out of it is (`HiddenBinds`), in the leaving binds' words (`LeavingLines`, one place for the mounts check, the start gate and `CheckLeavingBinds`): Docker resolves it as root.
- With an entry owned the check readies `UtilImage` (`ensureUtilImage`), so a registry down refuses the release instead of failing `--mnt-owners` after its switch.
- Absolute bind paths cannot be checked; `docs/data.md` says so.
- `--check-mounts restored` (a rollback's, in the release it restores; the restart's after a failed run step, `restart`; a full restore's, `restored <snapshot>`, all but the entries the snapshot replaces) refuses no label, warning on ones it cannot read, and checks only what `--mnt-owners restored` needs: the leaving binds (`CheckLeavingBinds`), no owned entry a link and the util image readied (`checkOwned`).
- A `--no-code` restore checks the leaving binds in the entries it keeps (`check-tars`), and a sync's `finish go` before it starts anything, a copy that brings one kept as one its finish refused (`IncompleteFinish`).
- The restart after a failed run step runs `--check-mounts restored restart` first and keeps the previous release down when it refuses, saying what runs on from the failed release's code and the `--release` to run once the link or the util image is fixed.
- On a server nothing in a release starts while a bind source leads out of its entry, whatever brought the link (`cli.Plan.StartsInRelease`, `checkStartBinds`: in main before a project's command execs, and in `composeRun` for devopsy's own starts, compose's starting commands and `cp`, `--dry-run` aside), labels it cannot read only warning; the cost, one compose config read per start in a release, is accepted.
- What stops services to start them again runs the same check before it stops anything: a take, an export and a sync's push on the parent, only when something of their set runs (`gateBeforeStop`); a release snapshot's stopped pass, in the new release's code; an `--import` but for the entries its tar replaces; a sync's child side but under `--reset`, whose wipe removes the link; a restore in the entries it keeps (`check-tars`).
- The gate's message carries the links in its first line, which callers wrap.
- A bind source below an owned entry is refused by any service, read-only too: the entry's owner can replace a directory below it with a link, Docker resolves a bind source on the host, and a link to `/` would mount the host's root into the next container started; the entry itself is a mount point no container can replace.
- A path below an entry nobody owns stays allowed, but for one a link in the entry leads out of (`LeavingBinds`): such an entry is written from the host by the deploy user, an owned one by a container.
- Locally nothing refuses a bind source below an owned entry.
- `./mnt` whole is no entry: refused where labels are read as a data service's mount or an owned writable one, refused writable on any other service by the mounts check, and ignored read-only (a tool), so nothing downstream sees `.`; a tar carrying `./` itself is refused too.

## The util image

- `UtilImage` (`devopsy-util:4`, built on each host from `UtilDockerfile`, `alpine:3` with rsync, GNU tar, find, chown, zstd and setfacl) runs every step on the data as root: the owners, a sync's rsync, `--export`'s and `--import`'s tars, a snapshot's tars and `--destroy`'s emptying of `mnt`, so owners and modes survive and the deploy user's permissions do not matter.
- `ensureUtilImage` readies it before every use (the scripts through the hidden `--util-image`, a feature), built when missing and built again once older than `UtilImageMaxAge` (a week), `build --pull --no-cache`: it runs as root on the data and serves rsync across servers, so `alpine:3`'s patches must reach it.
- The image a rebuild replaced is removed without `-f`, so a container still running it keeps it.
- A rebuild that fails warns and goes on with the image there, since a registry out of reach must never stop a sync, an import or `--destroy`; a missing one that fails to build fails.
- Two rebuilds at once on a host are harmless: a build tags the image only once complete.
- The tag is bumped whenever `UtilDockerfile` changes, and a new tag is a named feature of its own (internals/snapshots.md's gates): a new tag is built at its first use on each host, where the old one would be rebuilt from the old Dockerfile only once a week old.
- One implementation, in Go, never in a script: a weekly check in shell needs GNU `date`, which the macOS test runs lack.

## devopsy's containers

- Every container devopsy starts in `UtilImage` carries `devopsy.env=<compose name>`, the environment whose data it works on, and `devopsy.task=<task>`, what it does (`remote.ContainerLabels`, `container` in `cmd/devopsy/writers.go`), never a `devopsy.data*` label, which are the project's.
- A container devopsy names is named `devopsy-<word>-<compose name>-<part>` (`newContainer`, the part what makes it unique while it runs: devopsy's pid, a sync's id), its compose name the one its label says: the door takes a name only of its project's own environments.
- The tasks (`remote.TaskOwners` and the rest):
  - `owners`, a release's or a local command's owners' step;
  - `import`, an import's or a restore's extraction and its owners';
  - `sync-<id>` (`remote.SyncTask`), what writes sync `<id>`'s copy into the child: its rsync, clone and owners' containers on one server, the receive's across, a reset's wipe and its owners';
  - `source`, the parent's side of a push's or an export's copy (the reflink check, the clone, its removal, the rsync daemon);
  - `snapshot`, `export` and `destroy`.
- What a killed devopsy left is found by these labels alone, never by a name, through one remover (`removeLeft(env, task)`, a `docker ps` filter on both): each guarantee is then a filter value, so a holder's can never match another sync's or the parent's clone, and an environment's never another environment's.
- Who removes which:
  - the child's holder (`endWriters`) its own `sync-<id>`, never another sync's and never the parent's clone of that very sync (`source`, the parent's env);
  - the next lock-taker (`endMarkedWriters`) an import's or a restore's `import` (the next import of that service, `--resolved`, a reset's wipe, the next restore on a restore's mark, a child's prepare on an import's or a restore's mark);
  - `--mnt-owners` the `owners` ones, before it scans;
  - the next holder of the parent's sync lock (a push, an export) the `source` ones;
  - `--destroy-data` every task of its environment, never another environment's, since it holds every lock.
- Locally only a failed import's end removes by label (`endImport`, `import`), never a local `--destroy` and never `owners`: nothing holds a lock there, and another terminal's owners' step may be running.
- A name stays only where it keeps a running container unique (`<prefix><compose name>-<pid>-<n>`, a sync's id), and `dockerRunFresh` (an import's, an owners' step's) and a snapshot check's `unzstd` remove a container of that very name first (`removeNamed`): a devopsy killed outright whose pid or snapshot this one repeats can have left it running, and `docker run` refuses the name.
- A container's own devopsy removes it by name when its context ends (`dockerRunIO`).
- Across servers the push's receive script starts the child's rsync, which the child's holder removes by its labels, so the labels are a named feature, `container-labels` (`remote.ContainerLabelsFeature`), found by `--sync-push`: an older parent would start it unlabeled, and its write could outlive the sync.

## Destroying and removing

- `--destroy` removes nothing unless everything before succeeded (`DestroyScript`, under the release and image locks; it asks for the target's full name unless `--yes`): the hidden `--destroy-data` (`runDestroyData`), in `current` else the newest release, runs the environment's `destroy:` step if any while everything is up, then `compose -p <name> down --volumes --remove-orphans --rmi local` with no `-f` (`composeDownProject`), since with files compose leaves the networks and volumes only an older release named, then empties `mnt` as root in `UtilImage` through a plain `docker run` (`compose run` would bring the network back), a sync's leftover record's writers ended first (`endWriters`) and every container of devopsy's the environment has removed, so nothing writes into `mnt` while it is emptied; then every release's images go; then `removeBase`.
- Prompts that destroy or replace data (`--destroy`, `--sync`, `--reset`, a snapshot's restore, `--import`, `--resolved`) ask for the target's full name (`confirmWord`, `Target.Path`: `<project>/[<instance>/]<environment>`), never the service, which the prompt's text names; a local `--destroy` asks for the project's name.
- Leftover levels are asked about, never removed silently: `purgeLeftovers` finds a level's directory left with only its `.env` (`LeftoverScript`) and removes it with `--purge` or a yes at a terminal, else warns with the command.
- `--releases remove` refuses all its words when one is wrong (`runReleases`): it lists first, resolves every word (`chooseReleases` takes an exact id, else a unique prefix, and `failed` as every failed or rolled-back release and an INCOMPLETE one older than current), refuses when one names nothing, several or the current release, then asks, `--yes` without a terminal.
- The server checks the words again (`ReleasesRemoveScript`, `--gone-ok` for `failed`, since a pruning may have taken one meanwhile) and runs `img prune` once everything went.
- Completion never offers release ids: they are the server's.
- A directory with `.devopsy/` and no `current` is a plain directory: commands run there, and releases refuse before creating anything.
- A `current` that is a directory (a failed release's containers restarting, Docker recreating their bind mounts as root) is refused by `prelude`, which says how to clean it up.
