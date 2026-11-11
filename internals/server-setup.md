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
- The release root and the tools are checked by one function each, `releaseRootCheck` and `toolsRefusal`, which `--server-setup`'s facts, the upload's `--server-ready` (stats and path lookups only, never Docker) and `--project-users add` (before it changes anything) all call.
- The release root is made when missing and given to the deploy user only when root owns it, never recursively; another user's, or one that is no directory, refuses.
- The mounts drop-in's want comes from the release root alone, the home included: `RequiresMountsFor` on `/` is `-.mount`, always there, and a want that moved with the mounts would remove the drop-in while a volume is not mounted.
- A root systemd reads otherwise is written escaped as systemd reads it back (`systemdPath`), and one a unit file cannot hold refuses.
- A live socket buffer is raised only when lower, never lowered; one that cannot be raised fails the second check, naming the reboot that loads the file.

## `daemon.json` and the record

- `daemon.json` is three parts, each the sub-object of its keys (`setup.Parts`), compared in canonical JSON (`setup.Canonical`): spacing and key order never read as a change.
- The merge keeps every other key as the file has it (`json.RawMessage`), puts the file only when the canonical form differs, and leaves a part devopsy stops wanting in the file; the merged file gets no header.
- A part's state comes from its observed value, the want, the record and what Docker runs with, by `setup.StateOf`'s rules in order, and what each command does with each state is `PartState.Upgrade` and `PartState.Setup`, which the facts carry (`setup.FactOf`), and what each shows `setup.ShowSetup` and `ShowInfo`: never a second judgment.
- A part the file does not set while Docker runs with a value of it other than Docker's default is set outside daemon.json (`setup.Outside`, docker.service's flags): left by every command with no question, said, no cause of pending, since dockerd refuses to start on a value both in a flag and in the file.
- A value someone else set is never put over unasked: `--server-setup` asks keep or replace, Enter and `--yes` keeping it.
- An answer acts only while the part still holds the value whose sha256 it names (`setup.Digest`): a part changed between the sessions is left unrecorded, said, and asked at the next run.
- `server.yaml`, beside `remote.UserConfigFile()`, is devopsy's record alone, never config, never in the schema, mode 0600, written by rename, its entries canonical JSON as quoted strings, so a comparison is exact text.
- The file is put first and the record written after, whatever the restart does next: every state a cut leaves reads in sync, which needs no record, and a restart not confirmed loses no answer.
- A server is set up by devopsy once `server.yaml` exists; the first apply writes it even with nothing to change.
- A `daemon.json` that is not a JSON object, or that the deploy user cannot read, refuses before anything changes, and is never rewritten.

## Root on the host

- Every root step on a host is one `hostRun`, declaring what it needs (a writable root, the host's network, `NET_ADMIN`, the pid and cgroup namespaces, AppArmor's profile off) and getting nothing more; `remote.EndUserArgs` is apart, as it runs devopsy itself.
- An apply is one root run of `SetupRootScript`, its arguments what to put, the root to make or give, the buffers to raise, and the restart or the marker; it prints a line per change, which the server's devopsy words.
- That run has the host's network and `NET_ADMIN`, for `sysctl`, and its pid namespace, for `systemctl`: systemd refuses a peer it cannot see.
- The root script's system paths sit under its host root argument, "" on a host (`remote.HostRootEnv` in tests), so its tests run it on a temporary directory; the release root is passed as the host names it.
- `daemon.json` is put through `hostPut` with `dockerd --validate` as its check: a refused file leaves the one there.

## Restarting Docker

- `--server-setup` offers a restart when the run changes `daemon.json` or Docker has yet to take it (`setup.Pending`), and the operator passes its answer as a condition (`--restart`, `--yes`'s `--restart-idle`, only with no container running): the apply judges it under its locks, restarting only when this run put `daemon.json` or something is pending then, else saying why not.
- A restart is refused, with nothing changed, while a release holds a lock in the deploy user's environments: the apply tries each `$base/.lock` before anything changes and holds them all until the restart is confirmed or given up; project users' trees are not covered, and the run says so.
- Docker is never restarted on a `daemon.json` dockerd refuses, whoever wrote it: it would stay down.
- The restart is queued with `--no-block`, `daemon-reload` first, and the apply goes by the root script's `queued` line, never by the exit of a container the restart may cut, whose client's words then go unsaid.
- A drop-in put without a restart is followed by a `daemon-reload`, failing quietly: else systemd says at each call that `docker.service` changed on disk.
- A restart is done once `docker.service`'s `InvocationID` changed and `docker info` shows the file's values, waited for through `boundedWait`, bounded (`restartWait`); one not confirmed in time (`remote.RestartWaitEnv` shortens it for tests) is an error naming the journal, the restart flag raised with the `InvocationID` read before the run.
- A change not restarted now raises the restart marker (`setup.RestartMarker`, with the `InvocationID` read before the run) and the system's flag, through `restart_flag`, only when the run changed `daemon.json`.
- systemctl refusing from the chroot never leads to a privileged container in the host's pid namespace (internals/door.md): the run marks the change and names the restart for the admin.

## Sessions

- `--server-setup` is two sessions as the deploy user: `facts`, read-only, no lock, no util image, carrying the apply's needs (`WithAlsoNeeds`) so an older server refuses before anything; then `apply`, under the apply lock (`setup.LockFile`, `boundedWait`), whenever there is something to put, an answer to record, a restart, or no record yet.
- A project user is refused through `askedAsProjectUser`, the same exit as `--ssh-keys --for`, and `refuseProjectUser` names the deploy user's address.
- Declining saves nothing; `--yes` goes through the same two sessions, and without a terminal a question fails naming `--yes`.
- `apply --upgrade` asks nothing and restarts nothing: a changed part is left and said, a missing release root said and never made, every failure a warning, exit 0.
- A server is one whose `server.yaml` exists, from its first `--server-setup` on, with no other sign: only there does `--upgrade` apply (`serverApply`), and `--server-info` warns where it is missing.
- `--server-info` shows the same facts and items as `--server-setup` (`setupItems`), never a second judgment.
