# Snapshots

`cmd/devopsy/snapshot.go` (the steps, run on the server as the hidden `--snapshot-step <word>`, and the operator's `--snapshot` and `--snapshots`), `internal/remote/snapshot.go` (the scripts), the restore inside `activateScript` (`RestoreScript`), and the reflink helpers of `cmd/devopsy/clone.go`, shared with syncs.

## What a snapshot holds

- A snapshot is binary, always: every service with `devopsy.data=true`, whatever its copy label (`copy=none` included), its entries and named volumes as files minus `exclude`, and no `custom` script runs.
- A take is refused before anything stops when nothing has data, a data service writes a bind from outside the project (`cli.SyncService.OutsideBinds`), a service has data and no entry or named volume (a database in an anonymous volume would be held nowhere and restore as complete), the environment is marked, or the space is short (`du` in the util image, a tenth to spare, the tars and a mirror's missing part counted on the snapshot root's filesystem, where both live).
- A killed sync holder's files are settled into a mark before a take and before every restore (`settleSync`, as before an activation): a complete restore clears marks, so a snapshot of half-written data would come back as clean.
- The directory holds a release's copy, `code/` and its files without the logs, its images listing bookmarked as `s<id>` (`images.Set.idOf`, `BookmarkFrom`), the manifest with `format` first (a restore refuses a format it does not know), and `data/` (the tars, `shared.env`, `inherit.env`, `data-images`, and `images`, every pulled data service's image as `ParentData` lines, `SnapshotData`, whatever its copy).
- Everything in it is written under `umask 077`: it holds the secrets.
- `images.Set.own` counts snapshots' bookmarks as the environment's own (`TestSnapshotBookmarks`): otherwise a rollback of a built image after a snapshot leaves its name on the newer image.
- The scripts export `DEVOPSY_INTERNAL_SNAPSHOTS` and `DEVOPSY_INTERNAL_BASE` (`basePrelude`), which a `Set` reads when its own fields are empty.
- Only a renamed directory is a snapshot: a take writes into `<id>.tmp/` and renames it when complete.
- The manifest's `copy` records how the take copied (`reflink`, `mirror` or `none`), and `--snapshots` says it of the last take here, never of an imported one.
- The manifest's names are single names (`readManifest`), and a restore extracts a volume by the name compose gives its key in the restored code, never the manifest's: an imported manifest is anyone's, and a crafted one could name another project's volume, emptied as root.

## A take

- The tars are made while the set is stopped unless the data clones by reflinks or a mirror is kept, the downtime their time, then the manifest and the rename.
- The tars (`dataTars`, `tarInto`) hold only files, directories and links (`find ... | tar --null --no-recursion -T -`), hard links as files of their own (`--hard-dereference`), holes kept (`--sparse`, and `RsyncCopy`'s `--sparse` for the mirror and syncs, since holes written as zeros take more disk than the source), one `.tar.zst` per entry or volume with its members named plainly as the extraction matches them, `zstd -T0` under `nice` in the util image, written into the snapshot's directory mounted into the container and given to the deploy user, never through the container's stdout: Docker's stream is many times slower than a file.
- The tars' sources are mounted read-only where a mirror has them (`/m/mnt`, `/m/volumes/<key>`), so the tars are the same with or without one.
- One tar per entry, never one for all: an extracting container reads only its own.
- Each step runs in the release that is current when it starts (a take's steps and an import's `cd "$base/current"` in their subshell): the first pass holds no release lock, and a release switching current meanwhile would leave the stopped pass on the old release's labels with the new one's code.
- The set is stopped as a sync stops it (`stopSetFor`), every service with data, any service writing what is copied with or without a label (for a take a writable mount of a copied entry or any mount of a copied volume, for a restore any service of the current release mounting what it writes), and their dependents, in one stop and one start, recorded as `snapshot <id> <name>` lines (`readSyncRecord` keeps them as their own kind).
- A take whose stopped pass is killed outright starts them from its script at once, the locks still held; a take gone whole leaves them to the next settle (`settleStops`, internals/syncs.md), which starts only those the current release has and none on a marked environment.
- Every stopped pass holds the release lock exclusively, so a `snapshot` line a settle sees is a leftover by construction.
- A take's session is an activation's (`liveSession` with its words): before the end of the stopped pass a signal or a session gone cancels it and the services start again (each step runs in the background, the shell forwarding the signal, since the watcher signals the script alone where it leads no group); past it the take goes on, its output in the current release's server log (`followOutput`, tag `snapshot.<id>`).
- What stops services to start them again is gated before it stops anything (`gateBeforeStop`, internals/data.md).

## Reflinks and mirrors

- Reflinks come first, whatever the setting (`takeCopy`, over `clone.go`'s `reflinks`, `cloneData` and `removeClone`): before anything stops, `reflinks` clones one file of each entry and volume into `$base/snapshots/.clone` with `cp --reflink=always`, as root in the util image readied first, and removes it; where all clone, the stopped pass clones the data there (`cloneData`, `cp -a --reflink=always`, marked `.devopsy-complete` with the id), the tars are made from the clone afterwards (`snapshotTar`), and the clone goes once the snapshot is renamed (`removeClone`, as root).
- The clone lives on the release root's filesystem, never the snapshot root's: reflinks never cross filesystems.
- A clone that fails though the check passed falls back to the tars from the stopped data: a filesystem that refuses one file must not fail every take there.
- A cut take with a marked clone is finished from it; a clone without its `.tmp`, and a check's `.reflink.*` left by a kill, go with the leftovers; `--destroy` removes `.clone` as root.
- A take that clones removes a leftover mirror.
- No reflinked copy is kept past the take: a database writing over one fragments its files.
- With `snapshots: {mirror: true}` a take runs two rsync passes into a kept mirror, `.mirror` among the environment's snapshots under the snapshot root (`mnt/`, `volumes/`, root's), pass 1 while everything runs, pass 2 stopped, the services started, then the tars from it (`snapshotTar`, `tarInto`) into `<id>.tmp/`.
- The server's `snapshots: {mirror: false}` wins over the environment's (`takeMirror`): a take asking for one goes on without it, warned.
- A take without a mirror removes one left from before (`removeMirror`, as root), said.
- The mirror's rsync is the sync's (`rsyncEntry`, `rsyncVolume`: `--modify-window=-1`, `--no-D`, exit 24 a notice).
- The stopped pass of a mirror take ends by writing `.mirror/.devopsy-complete` with the id, and a take without a mirror writes `devopsy-tarred` into its `.tmp` once all its tars are there: a mirror take's tars come after its manifest, so the files alone never tell a complete `.tmp` from a cut one.
- A holder of the snapshot lock finishes a `.tmp` whose id `.devopsy-complete` names, with its manifest and listing there, from the mirror (`leftovers`); renames one with its manifest, listing and `devopsy-tarred`; removes any other `.tmp` with its bookmarks, and a killed import's `.import.*` with it.

## Locks

- One order everywhere: release (fd 9), sync (fd 5 in the snapshot scripts, where fd 8 is the image lock's as in `image_lock`; `syncSourceLock` takes it on 8), snapshot (`.devopsy-snapshot.lock`, fd 6), image; none is waited for while a later one is held.
- A take holds the snapshot lock alone for pass 1, the release, sync and snapshot locks for the stopped pass, then the snapshot lock alone for the tars: a release never waits minutes for a tar.
- A restore holds the snapshot lock from before it reads the snapshot to its end, and pruning keeps the snapshot a restore's mark names: a take's prune once removed the snapshot a restore was reading.
- `--destroy` takes all four.
- `--status` says "snapshot" while a take holds its holding file and the lock (`SnapshotRunning`), never on the file alone, and every other holder of the lock removes a holding file a killed take left (`snapshotLock`).
- A release snapshot writes the holding file once it has its id, its detached tar keeping the lock to its end: `--snapshots` otherwise lists it INCOMPLETE meanwhile.
- The unit tests' fake `flock` locks nothing unless `FAKE_REAL_FLOCK` names the real one.

## A full restore

- A full restore is a release (`RestoreScript`, `activateScript` in rollback mode with `RestoreOptions`): a new release with the server's id, made from the snapshot's copy (`make-release`), its images restored from its bookmarks as a rollback's (`img restore`, built again when gone), its steps the restored release's `rollback:` else `release:` (`--release-steps restart`), and `--check-mounts restored`.
- `make-release` takes the snapshot's code and, of the release's files, only the pending file and `devopsy-target.env`, never a mark the snapshot carries; the target file keeps the current release's target lines and imports (`mergeTargetEnv`), and the record names the snapshot.
- The tars are checked whole before anything stops (`check-tars`): `zstd -dc` in the util image into a FIFO beside the snapshots, never its stdout (`unzstd`), into `tarEntriesIn`, the reader holding a writer of its own so it never waits on a container that never opened it, a container of the check's name removed first (a check killed outright leaves one blocked on the FIFO; every check holds the snapshot lock), and Docker's failure the one said.
- The sizes are summed for a warning when the data may not fit (`restoreSpace`: per filesystem, more than its free space plus what the restore replaces there, from `df` and `du` in the util image, existing volumes only), a warning, never a refusal.
- The checks are an import's (internals/export-import.md) but a symbolic link's target, which is the data's own wherever it leads, and but a link that makes a bind source of the code that will run lead out of its entry, walked through the tar's own links as the host resolves them in mnt (`linkAtBind`, `tarLinks.walkMnt`), checked in that code (`$code` for a full restore, current for `--no-code`); every release and rollback refuses such a bind too (`LeavingBinds`, internals/data.md), since the data outlives the code.
- Each tar is one entry or volume of the manifest's kind, restored only where the restored code's services with data have it, an entry of another kind than its tar or a symlink refused as an import refuses them.
- On a marked environment a restore refuses when any entry or volume there now of a service with data, current or restored, is absent from the snapshot: the restore clears the mark, so it must write all of each, as a sync does.
- A data image gone is pulled by its digest in the service's own repository as the snapshot's listing names it (`SnapshotPull`, `ownHolds`), never from a repository an imported record names, under the image lock, shared, and bookmarked as the snapshot's at once: another environment's removal must never strip it before the data step holds it.
- The data is written between the images and the switch (`restore-data`), past the point of no return: `followOutput` and its marker come right before it, so a failure before it undoes the images' names as a failure before the switch does.
- `restore-data` stops the set, writes a restore mark right before the first write, extracts each entry and volume with the import's own code (`extractEntry`, containers named with `ImportContainerPrefix`), and holds the data services on the snapshot's images (`ImportDone`).
- The mark and the record of what it stopped go, the record whole, only once the run step went through (`restored`): cleared by the data step, a shell killed before the run step leaves the set down unmarked.
- A run step that fails takes down what it started (`firstDown`) before current goes back, and the restart skips the marked environment (`RestoreFixes`, naming `--no-code`): the failed step's services must not run on the restored data.
- A full restore never marks the release it leaves as rolled back: a rollback after it goes back there.
- `--with-env` brings `shared/.env` and `inherit.env` back; without it the keys that differ are said.

## `--no-code`

- `--no-code` (`RestoreDataScript`) restores the data under the current release, never as an activation: the watcher, the locks, `check-fit`, the tars checked, `waitBeat`, then the data step and the deployed run step again, then `restored`, the output followed from the disarm on.
- `check-fit` refuses, naming each mismatch, when the current labels do not fit the manifest (a service gone or without data, an entry or volume no longer its own or another's now) and, on a marked environment, when a current data service or any of its entries and volumes is absent from the snapshot.
- A `--no-code` run step that fails takes down what it started (`firstDown`), as a full restore's does.

## The restore mark

- A restore's mark (`remote.IncompleteRestore`, its `From` the snapshot) is branched on by every reader of marks (`markRefusal`, the restart skip, `syncBlock`, `ImportScript`, the operator's release check, the parent check, `--status`, `--export`'s warning, `--import`, and `--resolved`, which refuses it as for imports only), with `RestoreFixes`.
- The snapshot a restore's mark names stays until the mark goes.
- A sync clears it only by copying every service of the snapshot, all of each: the child's list prints a `!restore` line per service from the manifest, and the push checks each with `copiesService`, refusing before it stops anything otherwise.
- A cut copy over a restore's mark stays partial and the mark stays the restore's: a sync's copy mark over it would let the next plain sync clear it.
- Every sync needs `restoreMarkNeed` of the parent's devopsy (`syncPreflight`), so a parent that cannot read the mark refuses before anything changes.

## Export and import

- An export is one outer tar of the snapshot's directory on stdout, and an import reads one from stdin.
- An export holds its snapshot's directory locked (fd 4, shared, taken under the snapshot lock, which goes before the tar), and a removal or pruning skips a snapshot it finds locked (`removeSnapshotDir`): nothing waits for an export, which lasts as long as its pipe.
- A release that waits for the sync or snapshot lock in vain fails through `not_live` (`lockWaitOr`), never a bare `exit 75` leaving it pending.
- An import is gated (`lockedGate`, `--stdin-guard`), never `--import`'s marker and byte (storing a snapshot stops nothing), and extracted beside the snapshots (`receive`: GNU tar, the same stream read by `archive/tar` to its end-of-archive blocks, since GNU tar takes a stream cut at a member's boundary for complete; modes as the tar has them, `-p`, never the import's umask 077, which would strip the code's, which a restore runs).
- Outside its `code/` an imported snapshot holds nothing but files and directories: a link among its own files would have the import read and write another snapshot's manifest.
- An imported snapshot is checked whole as a restore checks (`imported`), its tars only (no pull, no space warning), and its `code/` is trusted by design (a full restore runs it).
- Its image listing is cut to the current release's services, each kept under that service's repository here, and its image ids cleared (`images.FilterImported`): the other server's images are bookmarked by them, and a crafted listing could have the server pull and tag another repository's image, and a removal strip names of images nothing else held.
- An imported snapshot is renamed under a new id, its images bookmarked where the server has them, a release snapshot made a manual one (its release is not here, and pruning would take it for one whose release went), and its manifest marked `imported`.
- Images stay out of exports: a member of the outer tar needs its size first, so `docker save` would spool gigabytes on the server; a restore pulls the pulled ones by digest and builds the built ones from the snapshot's code.

## Keep and pruning

- The environment's `snapshots: manual: {keep}` (3) is sent by the operator (`DEVOPSY_INTERNAL_SNAPSHOT_KEEP`, and its `mirror:` as `DEVOPSY_INTERNAL_SNAPSHOT_MIRROR`), at most the server's `snapshots: max_keep` (5).
- Pruning runs after each tar, under the snapshot lock, the image lock taken only when free (else the next take prunes).
- `--snapshots remove` waits for the image lock, and refuses the snapshot a restore's mark names.
- A changed `snapshots: root` leaves the old snapshots unseen, accepted: finding them would need a sweep of Docker's bookmarks, which removals never do.
- `--destroy` removes every snapshot, bookmarks first, the mirror as root, the snapshot root's directory too, and says how many.

## Release snapshots

- `snapshots: {release: true|{enabled, keep}}` (off; keep 1 or `all`, `KeepAll`; sent as `DEVOPSY_INTERNAL_SNAPSHOT_RELEASE_KEEP`) makes an activation replacing a current release, and a restore, full or `--no-code`, take a snapshot of the data it replaces, from the release replaced in `$base/current`; rollbacks take none.
- The checkout's config decides, as for a release's `keep`.
- `releaseSnapshotSetup` runs before the fetch: a killed holder's files settled (`settleSync`, since a release that syncs runs no `partialGuard`), the server's image lock, record and mode exported (the bookmarks and the detached pruning hold the lock), the effective releases `keep` checked (1 would prune the snapshot with its own release; `releases: keep: 1` with them on is a config error too), the sync and snapshot locks (release, sync, snapshot, then the fetch's image lock), `release-check` (the leftovers first, as a take's; exit 3 skips it, said, on a marked environment or with nothing with data; else the space check), the id, the server log made, and `not_live`'s hook, so that every failure before the switch starts the record's `snapshot` lines again, the write after the fetch included.
- Inside `fetch_release`, before its disarm, so that a session gone or a signal still cancels: the mirror's first pass beside the fetch (a background job, waited for), then `release-stopped`, which leaves the set stopped for the run step's `up` to start under the new code and names it in the new release's `devopsy-snapshot-stopped`, then `release-tar` detached (`setsid`, `nice`, its output to the release's server log, fd 6 alone kept).
- `release-tar` makes the mirror's tars and, once the activation's pid is gone, prunes both kinds and every release snapshot whose release is gone (`pruneGone`, which `--snapshots remove` runs too), waiting up to 10 minutes for the image lock the activation held; it keeps the snapshot lock to its end, so a take or a removal meanwhile waits.
- Every release snapshot counts towards `keep`, and a release tried again keeps its own: the one before a failed run step holds the only copy of the data from before it.
- Only a holder of the snapshot lock prunes: a release removed by anything else (pruning, `--releases remove`, release snapshots since turned off) leaves its snapshot to the next take or removal, since an activation calling a snapshot step would make every release need a server with snapshots.
- At the switch `release-record live` turns the `snapshot` lines into the release's `stop` lines, kept down by a mark and by a settle inside its own activation (`HeldStopsEnv`; any other settle starts them as leftovers, internals/syncs.md).
- Once something started them, the run step or the restart of the release before, `release-record done` removes those lines, first starting what of them the run step left exited (`startLeftDown`, said; a run step need not `up` every service), one whose container another image than the release's holds made again (`images.Recreate`, as `restoreServices` does), a start that fails only warning, the release live; a cut sync's copy or a failed restore keeps them down, the record saying so.
- A restore's data step turns them into its own once its mark is written, kept down under it; a mark that cannot be written leaves them the snapshot's, which the hook starts.
- A `--no-code` restore traps INT, TERM and HUP around its release snapshot, so a cancel starts the set again (`rd_unsnap`) rather than killing the shell.
- Without a mirror or reflinks the stopped pass writes the tars, so a release is down for them: `mirror: true` or a reflinking filesystem is what large data needs.

## Gates

- The hidden `--snapshot-step` is a feature flag, so an older server refuses every snapshot script before anything changes.
- Release snapshots are a named feature, `release-snapshots`, found by `--snapshot-step release-` (`ReleaseSnapshotsFeature`): a server with manual snapshots only refuses a release with them on once its activation starts, after the upload, accepted, since an activation's needs are checked by its own session and the upload's by the upload's (internals/releases.md).
- What came with release snapshots is under the same name: the mounts check's words after `restored` (`restart`, a restore's snapshot) and `devopsy-util:3` in a script's text, so every activation needs it; a new `UtilImage` tag (internals/data.md) is a named feature of its own, since a devopsy without the tag never readies it.

## Tests

- The scripts run against the fake docker in `cmd/devopsy/snapshot_test.go` (take, interrupt, keep, list, remove, restore, a cut restore, `--no-code`, leftover lines, the mark's readers, a sync over a restore mark, export and import, `--destroy`, without a mirror, release snapshots and their cases, a restore's), the images' bookmarks in `internal/images/snapshot_test.go`, and end to end a take and a full restore with real Docker, a snapshot moved to another server with a mirror, release snapshots both ways and reflinks on an XFS loop device (`e2e/snapshot_test.go`, `TestSnapshotReflinkE2E`).
- The fake docker clones by reflinks only under `FAKE_REFLINK=1` (and fails the clone under `FAIL_CLONE=1`); the e2e image has `xfsprogs` for the loop device.
