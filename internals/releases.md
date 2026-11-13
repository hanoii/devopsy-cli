# Releases

A release uploads the project as a new directory, then one activation session switches `current` to it; a rollback switches back to an older one. Code: `cmd/devopsy/remote.go` (the operator's side), `cmd/devopsy/release_local.go` (a release in place), `internal/remote/remote.go` (the scripts), `internal/remote/activate.go` (the activation, its steps in order in `activateScript`), `internal/remote/features.go` (the gate), `internal/remote/vars.go` (`--vars`).

## The gate

- The server's devopsy must have what a script calls: `--release-settings` reports `protocol=` and `features=`, and `basePrelude` checks them before anything changes, read-only commands too, naming the upgrade.
- `remote.Protocol` (8) covers only what every script depends on (the prelude, `--say`) or a format both sides share that no script's text shows, said in its comment; anything else is a feature: one number gating every command refuses releases and `--status` on servers that could run them.
- Every devopsy flag a script calls is a feature (`featureFlags`), and a behavior of a flag that came after it is a named feature found by what the script calls (`namedFeatures`); compose's flags a step or the operator's command starts with (for example `--profile web up`) are none.
- A script's needs are read from its own text (`Needs`, sent in front of it by `SSHStreams`), and `TestFeaturesCoverScripts` fails on a flag no feature lists: a need listed by hand drifts from the script.
- A new named feature is one entry in `namedFeatures` with the words that find it, and its area's rule says what finds it and what an older server would do without it: `data-copy` (internals/data-labels.md), `mnt-owner` (internals/data.md), `data-images` and `record-mode` (internals/images.md), `release-snapshots` (internals/snapshots.md), `export-images` (internals/export-import.md), `reset` (internals/syncs.md) and `container-labels` (internals/data.md).
- Across servers the child's devopsy builds its own side of every request of the parent's (`--sync-child`), so only the holder's needs are checked there, by its own prelude: every holder calls `--sync-key`, newer than any feature the child's side uses.
- The parent's `CurrentExistsScript` carries the push's needs (`extra`), and the child's prints whether its server has `data-copy`, so the operator refuses a release that will sync before its upload and lets one that does not go on.
- An activation's needs are checked in the first session to the server too (`remote.WithAlsoNeeds` on the role check, a new instance's, a child's current and the upload): refused at the activation alone, a pending release and an instance's directory are left behind.
- Only the plain activation's needs are checked so: a sync's are checked where it is decided, and a release that does not sync runs on a server lacking them.
- A release that opens no session before its upload runs its local before steps first; accepted.
- `DEVOPSY_PROTOCOL_CHECK=warn` (`ProtocolCheckEnv`, exported into every session so a push passes it on) turns either refusal into a warning, for a local build ahead of the servers.
- A server needs `devopsy`, `tar`, `flock`, `setsid`, `script` (util-linux), a `tail` that answers `--version` (GNU's or uutils') and `mkfifo`; locally only `ssh`.
- Without `script` there is no live build progress, without `setsid` the run step runs plainly, and without a `tail` that answers `--version` only the traps protect a release after its switch (internals/sessions.md).

## Steps

- `--release` and `--rollback` take no command; the environment's `release:` and `rollback:` are required (`remote.Steps`, with before, prepare, run and after phases, a plain string being a run step alone): the upload and the switch are never hidden in a step.
- The remote steps share the session that holds the release lock, so before is local steps then remote ones and after is remote then local, checked when the config is read.
- Local steps get the target's env, a child's `DEVOPSY_PARENT` and `DEVOPSY_PARENT_TARGET` (`parentEnv`) and the commit, never the server's `.env` nor `DEVOPSY_TARGET`, `DEVOPSY_ENVIRONMENT` or `DEVOPSY_INSTANCE`: set, those mean a release on a server, which the templates' `env` capabilities test, and `DEVOPSY_INSTANCE` would reach the step's own devopsy calls.
- A local step that needs the address declares `## Needs: target` and reads `DEVOPSY_COMMAND_TARGET`.
- Remote commands run through the `current` symlink, never a release path, except `prepare`, which runs in the new release before it is current and must not start services: compose stores bind-mount paths in containers, and a pruned release's path breaks them.
- A rollback's steps (`--release-steps rollback <phase> [<release>]`, `runReleaseSteps`), the restart after a failed run, the run step a `--sync` reruns (`--release-steps release run`), a reload's (`--release-steps reload run`), a parent's `inherit:` keys (`--release-inherit`) and `--destroy`'s step (`destroyStep`) come from the release's own `config.yaml` on the server, its environment from `devopsy-target.env` (`ReleaseEnvironment`); a release's steps come from the checkout, and a rollback's local before and after steps stay the checkout's.
- `mode:` defaults to `build`: it works for every project, where a wrong `image` fails the release.
- devopsy fetches the images itself before the switch (internals/images.md), so the run step only starts them: a build or pull in the run step runs after the switch, and in a sync after the copy.
- `--release` without a target is a release in place (`runLocalRelease`): `defaults:`' release steps, prepare, `--missing-vars`, the images as build mode and run, with no before or after steps and no switch.
- `--destroy` without a target (`runLocalDestroy`) asks for the project's name (`--yes` skips it), then runs `defaults:`' destroy step if any and `runDestroyData` in place, and never removes the checkout.
- The other release flags are target only.

## Upload

- Everything that can be checked locally or in a short session is checked before uploading: the files (`remote.Files`, build mode's list being `git ls-files`, so outside a repository it fails before any session), secret-like names (`confirmUnnamedSecrets`, internals/config.md), a role another compose project holds (`RoleHoldersScript`), a new instance, which asks (`InstanceExistsScript`, `--yes`), and a child's sync (internals/syncs.md).
- The upload's script calls `--server-ready` right after `basePrelude`, before `releasePrelude` takes the lock or makes `$base`: a server without the release root as the deploy user's, or `tar` and `flock`, or with Docker below its floor (internals/server-setup.md), refuses with nothing left, its fix `--server-setup` (the `docker` topic for Docker's); then it warns of the firewall (internals/server-setup.md); a project user checks nothing but the firewall, through the door, and no other session checks it.
- A release is a new directory, never a changed one: `UploadScript` takes the release lock, prints `LockedMarker`, extracts into `releases/<id>.tmp` through `--stdin-guard`, writes `PendingFile` and renames it into place; an existing id is refused, since `mv` would put the new tree inside the old release.
- The server names a release (the hidden `--release-id`, under the lock, printed on stdout by the upload), a UTC second, or a second past the newest release when that is not earlier (`remote.NextReleaseID`): ids order releases, and an operator's clock can order an older release after a newer one across laptops and CI, or give two releases one id.
- The record (`RecordFile`) keeps the operator's time, for display only.
- Files go with git's modes (`Pack`, 0755 with any executable bit, else 0644), never the checkout's, and directories get the server's umask from `tar`: a checkout's modes are whatever wrote it, and a 0600 file bind-mounted into a container running as another user fails it.
- A release is a full tar stream, never rsync: macOS ships openrsync.
- The progress line (`uploadProgress`, at a terminal only, never logged) counts the project's bytes as `Pack` reads them, against the total `remote.Size` gives: the gzip's size is unknown until the end.

## Activation

`ActivateScript`, one session, in this order; each area named holds the step's rules.

1. The release lock (Locks), the watcher (internals/sessions.md), `img recover` of a release left fetching.
2. A rollback's target is the newest live release older than current (`img rollback-target`); `partialGuard` refuses a rollback, and a release that does not sync, on a partial copy (internals/syncs.md).
3. The pending lock (fd 7), then remote before steps in `current`.
4. Links into the release (`shared/*`, `inherit.env`, `parent.env`, `project.env`, `instance.env`), made even when the target is missing, so one created later applies, and relative like `current`: an absolute link breaks every release when the release root moves.
5. Releases only: `--prepare-release` when the compose files use role or import labels (`PrepareScript`, internals/roles.md).
6. `prepare` steps, then `--missing-vars` (`cli.MissingVars`: the files as the server's compose reads them, `config -q`, and when they do not read, the required variables `config --variables` lists that are unset or empty, with compose's words), then, releases only, `--check-names` and `--check-mounts` (below).
7. The images (`fetch_release`, internals/images.md); `ServerLogFile` is created here: a full disk fails the release before it goes live.
8. `PendingFile` removed, `ActivatingFile` written (internals/sessions.md), `make_current`, `--mnt-owners` (before the sync of a release that syncs, after `followOutput` otherwise; in a rollback `--mnt-owners restored` from the restored release's labels, where unreadable labels only warn), the sync, `followOutput`, the run step.
9. A failed run or sync: `FailedFile`, `failureReport` into the server's log, `current` back, and the previous release restarted (below) except on a partial copy; a first release, with nothing to restart, is taken down (`firstDown`) and leaves nothing current.
10. A rollback marks the release it left (`RolledBackFile`).
11. Pruning to `keep` (`img to-prune`, `img remove-releases`), then remote after steps, whose failure is reported with the release live.

- `--missing-vars` goes by the server's compose alone, never devopsy's compose-go: the server's compose may be newer, and it is what runs the release.
- The variables a release reads before its upload (the `uids` feature's gate, secret-like names) come from compose-go's loader on the operator's machine (`cli.ComposeVariables`), what the files include and extend followed, never a Docker call: a release needs no Docker where it starts; what the loader cannot follow is said, and the files' own variables read alone.
- `--check-names` and `--check-mounts` read compose's configuration, which fails while a required variable is unset, so they run after the prepare steps and `--missing-vars`.
- `--check-names` refuses containers of the same compose project from another directory (`cli.ProjectConflict`, the `-`-joined names coinciding, `a-b`/`c` and `a`/`b-c`) and a network the project names as its own that another compose project owns (`cli.NetworkConflicts`), external networks being how a project joins another's: Compose would silently share it.
- `--check-mounts` (internals/data.md) readies `UtilImage` when an entry is owned, so a registry down refuses the release rather than failing it after its switch.
- A rollback runs `--check-mounts restored` in the release it restores instead (internals/data.md): only what its `--mnt-owners restored` needs.
- `failureReport` writes each container's state and health, then the health checks and last logs of those neither running and healthy nor exited 0, before the restart recreates them.
- The restart restores the previous release's images as a rollback's are (`get_images`; when that fails `img hold` writes what a file can of its holds, internals/images.md), gives its owners (`--mnt-owners restored`, a failure only said) and runs its run step (`--release-steps restart run`, its `rollback:`, else `release:`); a rollback whose run step fails marks the release it restored failed.
- `firstDown` is `down --remove-orphans` with volumes and `mnt` kept, never a stop: `restart: always` would bring a container back with its bind mounts through the gone `current` made again as root's.
- When Docker does not answer after a failed run step (`timeout 5 docker version`) the script exits leaving `ActivatingFile`, the release cut off past its switch, never going back, and the step's exit code is no test, since a project's own timeout or the OOM killer give 143 and 137 with Docker fine: a restart without Docker puts current back on the failed release's images with no mark saying so.
- A failure before the switch changes nothing live (`not_live`, `NotLive`, `RollbackNotLive`): the release is marked failed and no directory is removed, since removing a failed first release's directory takes a second release uploaded behind it.
- No prompts in an activation: what is missing is listed with its `--vars set` line.

## On disk

- `releases/<id>/code/` (`remote.CodeDir`) is the project as uploaded, `current` points there, and the release's own files (`RecordFile`, `TargetEnvFile`, `LogFile`, `PendingFile`, `FailedFile`) sit beside it, never in it: inside the tree they are in every build context, and `COPY . .` misses the cache every release.
- A release keeps its project's directory, `<root>/<project>`, closed to other users (`chmod 700` after its lock, `prelude`): a release root another user enters (a project user's, reaching its tree there) would otherwise show what the releases and their containers write; Docker, root, resolves a bind mount past it, and a container never walks the directories above its mounts.
- Scripts keep `$rel` the release's directory and `$code` its tree; Go finds them with `remote.ReleaseFile` (symlinks resolved).
- Live means none of pending (upload to switch), fetching, failed or rolled back: what `keep` counts and what a rollback goes to.
- Failed and rolled-back releases go once older than the oldest kept; one that fails before going live stays, a first one included, until `--destroy` or `--releases remove failed`.
- A pending release shows IN PROGRESS only while its activation holds fd 7 on `PendingFile` (`--releases` checks with `flock -n -s`, never waiting), else INCOMPLETE, which `remove failed` takes only when older than current: a newer one may be between its upload and its activation.
- `devopsy-target.env` is written into each release (the environment's `env`, `COMPOSE_PROJECT_NAME` overriding compose's `name:`, `DEVOPSY_PROJECT`, `DEVOPSY_INSTANCE`, `DEVOPSY_ENVIRONMENT`, `DEVOPSY_TARGET`, `DEVOPSY_RELEASE_COMMIT`, a child's `DEVOPSY_PARENT` and `DEVOPSY_PARENT_TARGET`): it applies however devopsy runs on the server, and a rollback restores it.
- Precedence on the server: the caller, `shared/.env`, `inherit.env`, `instance.env`, `project.env`, `parent.env`, `devopsy-target.env`.
- A target's `COMPOSE_PROJECT_NAME` is always its compose name (`projectName` in `runRemote`); what else goes from the operator is only what `--vars set KEY` reads on purpose, display settings (`DEVOPSY_INTERNAL_*`, `DEVOPSY_VERBOSE`) and who released (`USER`, CI's actor) in the record.

## Variables on the server

- Values reach the server's files over SSH's stdin, never as arguments: `--vars`, `--vars-instance` and `--vars-project` edit one file each, in place under the release lock, since a deploy's steps write secrets into them too.
- A key's lines are all of its value, quoted over several lines, and `KEY: value`, which compose's parser reads as `KEY=value`.
- The edited file goes into place in one rename beside what a link points to, keeping the file's mode, whoever owned the file: a command reading a file while it is written gets it empty or cut.
- Inherited values are written under `umask 077`.
- `--vars get` gives what the environment's commands get, whatever defines it, and says where from; `--vars-instance get` and `--vars-project get` give their file's value; the list stays the file `set` and `unset` edit, counting what reaches the environment from elsewhere.
- No flag edits `inherit.env`, the parent's, replaced whole on every sync and `--inherit`; the child's `shared/.env` is where an override goes.
- A `set` warns, never prompts or refuses: overriding is what levels are for, and CI sets without a terminal.
- Over a value it overrides now, the warning names it, with the `unset` that brings it back.
- For each environment its file reaches, the warning names every nearer file that hides it, with no list of unsets, and, when an inherited value hides it, the `--vars set` there (unless the environment's own file sets it already) and how the inherited value goes (the parent's `inherit:`, its release, then `--inherit`).
- The trace behind `get` and the warnings is the hidden `--env-trace` (`cli.EnvTrace`, a feature; `cli.TraceEnv` as JSON, what `--debug env` prints), never a second reading of the files, except before a first release, when `get` reads them nearest first; it runs in the release's `current` (a plain directory's `$base`) in the read's session, and after an edit in the edit's (`VarsTrace` lines, every `current` under a level's directory).
- `--debug env` is for internals, never a hint for an everyday question.

## Locks

- The release lock (`$base/.lock`, fd 9) is taken by every script that changes an environment, the upload included (`releaseLock`, `lockWait`: `-n`, then a "waiting for" line and `-w 600`), and a fetch's children inherit it, so a recovery never races a fetch still running; without the upload's, `--destroy` empties `$base` under an upload, and a second release's upload runs into the first's activation.
- What a session sends goes once its script holds the lock (`remote.WithGate`, `lockedGate`): the upload, `--import`, `--destroy` and `--releases remove` print `LockedMarker` on stderr once they hold it (the removals once they hold the image lock too), and devopsy sends stdin only then (the tar, an import's data, the `go` a removal reads through `--stdin-guard` before it starts); a session cut while its script waits leaves an empty or silent stdin, which stops the script, never data in the server's pipe that it acts on once the lock comes.
- Activations need no gate, since their watcher, started past the lock, reads a closed stdin at once; a sync's holder arms its watcher before the lock, disarms it once held and waits for the session's next byte (`waitBeat`, internals/sessions.md), so a session gone during the wait ends it before anything stops.
- `--vars` edits and the inherit write are gated too and end their data with `InputEnd` (`GatedInput`), which `gated_read` requires and takes off: the data may be empty.
- Only the upload creates `$base` among the release scripts (outside them, `--vars` edits and the inherit write do too): an activation whose release went while it waited fails instead of making it again.
- A lock file removed while awaited is opened again (`[ /dev/fd/9 -ef "$base/.lock" ]`, Linux only), and `removeBase` moves `$base` aside before `rm -rf`, so a session arriving meanwhile makes a new one: otherwise two releases hold locks on different files.
- The image lock (`ImagesLock` in the release root, fd 8, always taken after fd 9) is the release root's, every project of its user's: shared for fetches, exclusive for removals, which also need Docker to answer, else they count as busy (`il_busy` says which), and every take in Go goes through one helper that does the same, its wait said and bounded (`imagesLock`); holders write `ImagesHolders/<pid>` (`remote.HolderLine` for one in Go, `imagesLockAt`), for messages only; Linux's `flock` lets new shared holders past a queued exclusive one.
- A project user's lock is its own root's: what keeps its fetch from the deploy user's removals is the door's claim, which holds the deploy user's lock shared and the image by a claim hold (internals/door.md), and what keeps the others' fetches from its removals is the door, which removes only its own holds and build names.
- Pruning skips when the image lock is busy, directories included, and the next release prunes; `--destroy` and `--releases remove` wait 10 minutes, then fail with 75; the restart after a failed run waits too, since on the new release's images it would run them under old code.
- `--destroy` holds the image lock across its `destroy:` step (compose's `--rmi local` removes images too), and Docker calls have no timeouts, so a hung daemon holds the lock until killed and waiters give up after 10 minutes; accepted.
- `basePrelude` exports `DEVOPSY_NO_UPDATE_CHECK=1`: the detached `--upgrade-check` would inherit fds 8 and 9 and hold the locks while it asks GitHub.
