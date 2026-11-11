# Server setup

`--server-setup` keeps what devopsy needs of a server as devopsy wants it. Code: `internal/setup/` (the item model, touching nothing: parts, states, the merge, the record, the volume candidates, the files' texts), `cmd/devopsy/serversetup.go` (the operator's side), `serversetup_step.go` (the server's, `--server-setup-step`), `hostroot.go` (`hostRun`), `internal/remote/serversetup.go` (`SetupRootScript`).

## Items

- An item is a want, a check and, for what devopsy sets, an apply: the required ones (the release root, `tar` and `flock`, Docker) refuse what depends on them before anything changes, and the tunables (`daemon.json`'s parts, the socket buffers, the mounts drop-in) are applied.
- devopsy checks and never installs: packages, Docker, the deploy user and devopsy itself are devopsy-server's setup.sh's.
- systemd is assumed, not a distribution: the one convention used is the system's restart flag (`remote.RebootRequired`).
- Every state comes from the server's devopsy, which applies it (`readSetup`, `setup.Facts`); the operator only words them and asks.
- Every item is idempotent: a second run in a row changes nothing and restarts nothing.
- After an apply every item's check runs again, and what was applied and does not read in sync is an error naming the item.
- A file devopsy writes whole sits at a path of its own and starts with `setup.Header`; devopsy writes or removes no other file, and none until its want is retired.
- The release root is never asked or saved by devopsy: it comes from `releases: root`, else the home, and `/` is refused by `remote.ReleaseSettings` in every command.
- The release root and the tools are checked by one function each, `releaseRootCheck` and `toolsRefusal`, which `--server-setup`'s facts, the upload's `--server-ready` (stats and path lookups only, never Docker) and `--project-users add` (its first check) all call.
- The tools are one list, `serverTools`, the required ones marked: the setup's refusal reads it, and `--server-info` says each tool from it once.
- The release root has one form, its links followed by what they say as far as its parts are there, the rest as written (`resolveRoot`): a root behind a link, made or not, its volume mounted or not, gives one want.
- The release root is made when missing and given to the deploy user only when root owns it, never recursively; another user's, or one that is no directory, refuses.
- The mounts drop-in's want comes from the release root alone, in its one form, the home included: `RequiresMountsFor` on `/` is `-.mount`, always there, and a want that moved with the mounts would remove the drop-in while a volume is not mounted.
- A root systemd reads otherwise is written escaped as systemd reads it back (`systemdPath`), and one a unit file cannot hold refuses.
- A live socket buffer is raised only when lower, never lowered; one that cannot be raised fails the second check, naming the reboot that loads the file.

## `daemon.json` and the record

- `daemon.json` is three parts, each the sub-object of its keys (`setup.Parts`), compared in canonical JSON (`setup.Canonical`): spacing and key order never read as a change.
- The merge keeps every other key as the file has it (`json.RawMessage`), puts the file only when the canonical form differs, and leaves a part devopsy stops wanting in the file; the merged file gets no header and keeps its mode and owner (`hostPut`'s `keep:`), 644 root's only for a file devopsy makes.
- A part's state comes from its observed value, the want, the record and the daemon's flags, by `setup.StateOf`'s rules in order, and what each command does with each state is `PartState.Upgrade` and `PartState.Setup`, which the facts carry (`setup.FactOf`), and what each shows `setup.ShowSetup` and `ShowInfo`: never a second judgment.
- The daemon's flags are a fact, read from the unit's command (`systemctl show -p ExecStart`, `setup.DaemonCommand`, `DaemonFlags`), never inferred from what docker info shows: the unit's command is what Docker starts with next, beside the file.
- A part one of whose keys a flag sets is the flags' (`setup.Outside`), whole: never put, never asked, said, out of pending and of the restart's confirmation, since dockerd refuses to start on a key both in a flag and in the file.
- A `--config-file` of the unit's naming another file refuses the Docker item: `daemon.json` is not what that Docker reads.
- dockerd's check of `daemon.json` runs with the unit's flags, so it checks what Docker starts with.
- A value someone else set is never put over unasked: `--server-setup` asks keep or replace, Enter and `--yes` keeping it.
- An answer acts only while the part still holds the value whose sha256 it names (`setup.Digest`): a part changed between the sessions is left unrecorded, said, and asked at the next run.
- `server.yaml`, beside `remote.UserConfigFile()`, is devopsy's record alone, never config, never in the schema, mode 0600, written by rename, its entries canonical JSON as quoted strings, so a comparison is exact text.
- The file is put first and the record written after, whatever the restart does next: every state a cut leaves reads in sync, which needs no record, and a restart not confirmed loses no answer.
- A server is set up by devopsy once `server.yaml` exists; the first apply writes it even with nothing to change.
- A `daemon.json` devopsy cannot judge (not a JSON object, a value of the wrong type, unreadable by the deploy user) has none of its parts judged (`judgeDaemon`): `--server-setup` refuses, `--upgrade` warns, and no command rewrites it.
- Docker's facts and `daemon.json` are read once for `--server-info`'s facts and the setup's (`readHostDocker`), and each fact is worded once for every command (`refusalWords`, `daemonWords`, `rootWords`).

## Root on the host

- Every root step on a host is one `hostRun`, declaring what it needs (a writable root, the host's network, `NET_ADMIN`, the pid and cgroup namespaces, AppArmor's profile off) and getting nothing more; `remote.EndUserArgs` is apart, as it runs devopsy itself.
- An apply is one root run of `SetupRootScript`, its arguments what to put, the root to make or give, the buffers to raise, and the restart or the marker; it prints a line per change, which the server's devopsy words.
- That run has the host's network and `NET_ADMIN`, for `sysctl`, and its pid namespace, for `systemctl`: systemd refuses a peer it cannot see.
- The root script's system paths sit under its host root argument, "" on a host (`remote.HostRootEnv` in tests), so its tests run it on a temporary directory; the release root is passed as the host names it.
- `daemon.json` is put through `hostPut` with `dockerd --validate` as its check: a refused file leaves the one there.

## Restarting Docker

- `--server-setup` offers a restart when the run changes `daemon.json` or Docker has yet to take it (`setup.Pending`), and the operator passes its answer as a condition (`--restart`, `--yes`'s `--restart-idle`, only with no container running): the apply judges it under its locks, restarting only when this run put `daemon.json` or something is pending then, else saying why not.
- A restart is refused, with nothing changed, while a release holds a lock in the deploy user's environments: the apply tries each `$base/.lock` before anything changes and holds them all until the restart is confirmed or given up, walking only the deploy user's own projects (`deployEnvBases`, `ownProjects`, `--server-info`'s filter): a project user's tree is never walked or locked, being that user's to change, and the run says its releases are not covered.
- Docker is never restarted on a `daemon.json` dockerd refuses, whoever wrote it: it would stay down.
- The restart is queued with `--no-block`, `daemon-reload` first, after the root run writes the marker with the `InvocationID` read just before the run: a restart that comes ends pending by its new id, the marker left to be read so (`/run` empties at a boot), and one that does not leaves Docker pending.
- A root run that fails once its restart was asked (its `asked` line, or the marker it wrote) goes on to the bounded wait, which tells whether the restart came: a restart cuts the run's container, and its client's words then go unsaid.
- A drop-in put without a restart is followed by a `daemon-reload`, failing quietly: else systemd says at each call that `docker.service` changed on disk.
- A restart is done once `docker.service`'s `InvocationID` changed and `docker info` shows the file's values, those set outside it aside, waited for through `boundedWait`, bounded (`restartWait`); one not confirmed in time (`remote.RestartWaitEnv` shortens it for tests) is an error naming the journal.
- The system's restart flag after a restart not confirmed is raised then, by a second root run, never before the restart, which would ask a reboot for nothing; with Docker down that run cannot start, and the error says nothing was flagged.
- A change not restarted now raises the restart marker (`setup.RestartMarker`, with the `InvocationID` read just before the run, the one source of it) and the system's flag, through `restart_flag`, only when the run changed `daemon.json`.
- systemctl refusing from the chroot never leads to a privileged container in the host's pid namespace (internals/door.md): the run marks the change and names the restart for the admin.

## Sessions

- `--server-setup` is two sessions as the deploy user: `facts`, read-only, no lock, no util image, carrying the apply's needs (`WithAlsoNeeds`) so an older server refuses before anything; then `apply`, under the apply lock (`setup.LockFile`, `boundedWait`), whenever there is something to put, an answer to record, a restart, or no record yet.
- A project user is refused through `askedAsProjectUser`, the same exit as `--ssh-keys --for`, and `refuseProjectUser` names the deploy user's address.
- Declining saves nothing; `--yes` goes through the same two sessions, and without a terminal a question fails naming `--yes`.
- `apply --upgrade` asks nothing and restarts nothing: a changed part is left and said, a missing release root said and never made, every failure a warning, exit 0.
- A server is one whose `server.yaml` exists, from its first `--server-setup` on, with no other sign: only there does `--upgrade` apply (`serverApply`), and `--server-info` warns where it is missing.
- `--server-info` shows the same facts and items as `--server-setup` (`setupItems`), never a second judgment.
