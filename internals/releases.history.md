History, not rules: `internals/releases.md` wins where they differ.

## The gate

- One protocol number once gated every command: a bump for a sync-only change stopped releases and `--status` on servers that could still run them. The number now covers only what every script depends on, and anything else is a feature or a named feature, so an older server refuses only what needs what it lacks, by name.
- Across servers the parent's devopsy, which may be newer than the operator's, checks the child's; at first each session to the child checked only its own needs, so an import or the receive refused on its own, after the parent's services had stopped. The first session (the look at the child's services) now carries the needs of all of them.
- `data-copy` (2026-10-25) replaced `data-labels` (`devopsy.data=mnt|stream|none`, the method in one label) when the label split into `devopsy.data` and `devopsy.data.copy`. A protocol bump was rejected: it would have refused every release, `--status` and `--log` on a server lacking only the labels. A project's remote step calling `--export` is gated rightly; a project command running `devopsy --export` itself is not detected and fails loudly without the upgrade line.
- `mnt-owner` as a named feature: a release's owners and mounts check are flags of their own (`--mnt-owners`, `--check-mounts`), so an activation on an older server refuses by them; only the copies needed a name.
- `release-snapshots` and the first session: the activation's needs were once checked only by the activation, so a refusal there left a pending release and an instance's directory behind, after the instance's prompt and a child's inherited variables were written. They are now checked in the first session to the server (`remote.WithAlsoNeeds`); a release that opens no session before its upload runs its local before steps first, accepted rather than a session of its own for it.
- `data-images` is not in `childNeeds` because the parent's sessions to the child call none of the `img` words; the child's holder needs it itself.

## Steps

- Local steps never look like a release on a server (decided): the templates' `env` capabilities test `DEVOPSY_TARGET`, `DEVOPSY_ENVIRONMENT` and `DEVOPSY_INSTANCE` to tell a release from local work, and `DEVOPSY_INSTANCE` set would reach the step's own devopsy calls.
- Remote commands through `current` rather than a release path: compose stores bind-mount paths in containers, so containers made from a release path broke once that release was pruned.
- `mode:` defaults to `build` because it works for every project; a wrong `image` fails the release, which says so.
- Fetching before the switch, never in the run step: a build or pull there ran after the switch, and in a sync after the copy, so a failure left the new release live without its images.

## Upload

- The server names a release: the id's source was the operator's clock, which could order an older release after a newer one across laptops and CI, or give two releases one id in one second. Ids order pruning, rollbacks and `--releases`, so the server picks them (`remote.NextReleaseID`) and the record keeps the operator's time for display only.
- Files go with git's modes (decided 2026-10-22): a checkout's modes are whatever wrote it, which git never shows; ddev's Mutagen wrote 0600 files to the host, and a 0600 Caddyfile bind-mounted into a container running as UID 10001 crash-looped it.
- Full tar streams, no rsync, because macOS ships openrsync. The gzip's size is unknown until the end, so the progress line counts the project's bytes as `Pack` reads them, out of `remote.Size`.

## Activation

- Links into the release are relative (decided 2026-10-27): absolute ones broke every release when a server's release root moved.
- The checks that read compose's configuration come after the prepare steps (decided): before prepare, a first release whose prepare makes its secrets (the drupal11 and registry templates') passed them unchecked, since compose's config fails while a required variable is unset.
- Rollbacks run `--check-mounts restored` in the restored release (review of 2026-10-23, round 2): an image pruned since failed the rollback after its switch, so the util image is readied before it.
- `failureReport` (decided): the restart recreates every container whose image or config changed, a tag rebuilt upstream included, so their evidence went with them and `--log` only said "unhealthy". The report now writes each container's state and health, then the health checks and last logs of those neither running and healthy nor exited 0, before the restart.
- `img hold` when `get_images` fails in the restart (review after v0.52.2): the data services are still held on their data's newest image, their listing and bookmarks following, so a snapshot of that release holds what it runs.
- A rollback whose run step fails marks the release it restored failed (decided): it did not come back up, so it is no rollback target until released again.
- `firstDown` takes a failed first release down rather than stopping it: `restart: always` would bring a container back with Docker, its bind mounts through the gone `current` made again as root's.
- Docker not answering after a failed run step (found by hand on OrbStack, 2026-10-27): a machine restart stopped the run step with 143 and Docker with it; the shell, ignoring signals past the switch, took it for a failed run step, went back and restarted the previous release with no Docker, so current went back on the failed release's images with no mark saying so. The script now exits leaving `ActivatingFile` when `timeout 5 docker version` fails; the step's exit code was no test, since a project's own timeout or the OOM killer give 143 and 137 with Docker fine.
- `not_live` removes no directory: removing a failed first release's directory once took a second release uploaded behind it.

## On disk

- devopsy's files sit beside `code/` because inside the tree they were in every build context, and `COPY . .` missed the cache every release.
- IN PROGRESS by fd 7 on `PendingFile`: a pending release that is merely INCOMPLETE may be a newer one between its upload and its activation, so `remove failed` takes only one older than current.
- `devopsy-target.env` is written into each release so it applies however devopsy runs on the server and a rollback restores it.
- A target's `COMPOSE_PROJECT_NAME` is always its compose name: one exported for local work once put every environment under one compose project.
- The `--vars` edit's rename: a `cat` into the file truncated it first, so a command reading it meanwhile got it empty or cut.
- `--vars get` is the environment's value (decided 2026-10-27): a child's `get` said "not set" of a value `--env` showed, inherited. `--vars-instance get` and `--vars-project get` stay their file's because it reaches several environments and has no one value.
- A `set` warns with no list of unsets (decided with the user): the list was confusing and rarely what is wanted; only the `--vars set` on the environment and how an inherited value goes are named.
- The trace comes from `--env-trace` rather than a second reading of the files: a second reading was what `--debug env` already showed, done again and drifting from it.

## Locks

- The gate on what a session sends: a session cut while its script waited for the lock (Ctrl-C, a drop) left its tar in the server's pipe, and the script extracted it once the lock came, after devopsy had said nothing changed; a removal went on likewise. The scripts now print `LockedMarker` once they hold the lock and devopsy sends stdin only then.
- The holder's disarm waits for the session's next byte: a session that went silent during the lock wait was otherwise ended only once its quiet time came after the lock, with the child's set stopped meanwhile.
- Only the upload creates `$base`: an activation whose release went while it waited made the directory again.
- A lock file removed while awaited is opened again: two releases once held locks on different files. `removeBase` moves `$base` aside before removing it for the same reason.
- Pruning skipping a busy image lock is accepted: servers are not expected to be that busy, and the next release prunes.
- `--destroy` holding the image lock across its `destroy:` step is accepted: compose's `--rmi local` removes images too, so a slow step makes fetches on the server wait. Docker calls have no timeouts, so a hung daemon holds the lock until killed and waiters give up after 10 minutes.
- No update check under a lock: the detached `--upgrade-check` inherited fds 8 and 9 (compose has to keep them) and held the locks while it asked GitHub.
