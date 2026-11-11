# Sessions, drops and signals

How a release notices its operator gone, what a signal may stop, and how a release goes on past its switch whatever happens to its session. Code: `cmd/devopsy/watch.go`, `cmd/devopsy/heartbeat.go`, `internal/remote/heartbeat.go`, and `watchSession`, `with_tty` and `followOutput` in `internal/remote/remote.go`, and the signal traps there and in `internal/remote/activate.go`.

## The session

- Activations and holders are always `-T`, at a terminal too, with a pipe devopsy keeps open as stdin (`WatchEnv`) and a pseudo-terminal from `with_tty`: a `-t` session's hangup reaches only the shell, and dash runs a trap only once the command it waits for ends.
- `remote.TTY` is exported into every session as `DEVOPSY_INTERNAL_TTY='<cols> <rows>'` (`remote.TTYEnv`) when stderr is a terminal (80x24 when its size is unknown), and `with_tty` in `basePrelude` runs steps, the fetch and the run step under `script`, plainly when `script` is missing or stdout is a tty already: BuildKit draws its live progress and colors only to a terminal, and the session stays one path for a terminal and CI.
- Every session's script starts by exporting the operator's command as typed (`remote.CommandEnv`, each word as `QuoteWord` writes it) and the address of the target that session is on, as typed and as a release's hint names it (`AddressEnv`, `HintAddressEnv`, none for a server named alone), in `sessionExports`, and a devopsy running in a session passes the command on to the sessions it opens (`OperatorCommand`): a hint said on a server can only name what the operator typed if the session brings it.
- Activations and holders print on one stream (`exec 1>&2`): on two pipes a line on stdout could reach the log after the marker and be cut.
- Every session, and the parent's to a child, bounds its connection in one place (`connectOptions`, in `SSHOptions` and `ChildSSH`) with `ConnectTimeout=30`, a `ServerAliveInterval` of 3 beats and `ServerAliveCountMax=3`: a session left unbounded waits on a dead host until TCP gives up.
- A release is pending from its upload until right before `make_current`: one cut off anywhere before is never rolled back to nor kept, and `prelude` removes stale `releases/*.tmp` and `.devopsy-watch.*` under the lock.

## The watcher

- The watcher (`runWatchSession`, the hidden `--watch-session`; `watchSession` on the script side) reads the session's stdin in the background and, when it ends, takes the armed file (renamed to `.fired`) and sends SIGTERM to the script's group, or to the script alone when neither it nor its login shell leads one, so a fetch is undone through `fetch_sig` at once rather than at its next write.
- Armed for releases and rollbacks (`WatchEnv`), for a release that syncs always (its stdin is the answer's pipe), and for a sync's holder only while it waits for the release lock: past it a SIGTERM would stop it halfway through restoring.
- The script disarms the watcher by taking the file first (`mv` to `.off`) in `fetch_release`, under the image lock, before its last `fetch_sig` check: one rename decides between the watcher and the switch, so a SIGTERM never lands past a switch.
- With a heartbeat the disarm takes the file to `.next`, prints `BeatMarker` and goes on only once the watcher sees the session's next byte (`waitBeat`), which devopsy sends at once on the marker (`WithBeat`); the holder past its lock wait does the same: the watcher counts its quiet time from past the lock, so a session that died silently during the lock wait or before a quick fetch would see its release go live.
- Only a zero byte before any answer disarms: an answer that early is an abort, with the session's end right behind it.
- `prelude` removes a watcher's file only once its script is gone (`kill -0` on the pid in its name): a holder arms its own before it waits for the lock.
- `waitBeat` gives up two beats past the longest quiet time, takes the file itself and goes the way of a session gone: a watcher killed outright never answers and must not hold a script.
- `TestWaitBeatWithoutWatcher` takes 20 s by design, two beats past the longest quiet time: never shorten it.
- When the watcher won, the undo writes to `ServerLogFile` (a `cancel.<pid>` part, shown by `--log` at the end), never to the session and never through a signal: nobody reads the session, a write into its full buffers blocks with the lock held, and a signal sent before the watcher handled it would kill it.

## The heartbeat

- Every pipe devopsy keeps as a session's stdin (an activation, a holder, a push, the push's import, `--export`) gets a zero byte every 5 seconds (`HeartbeatEvery`), and the server's readers, told by `HeartbeatEnv`, take a silence for EOF: a connection that dies silently (a laptop asleep, a network gone) reaches the server only when TCP gives up, minutes to hours later, and devopsy-server never sets sshd's `ClientAliveInterval`.
- A devopsy on a server beats as its own session does (`HeartbeatEvery` falls back to `HeartbeatEnv`), so the push's import on another server is counted in the beats it gets.
- Quiet times count in beats and are ordered, `QuietBeats` (6, 30 seconds) for a push, an import, an export and a release before its switch, `HolderQuietBeats` (18, 90 seconds) for a holder or a release that syncs, behind an answer's fifo, so a push still copying is usually cancelled before its holder decides, the operator's ssh noticing in between; the order only shapes the messages, since a late write is settled by the record the holder takes and the writers it ends (`endWriters`, internals/syncs.md).
- Data carries no beat: the upload's tar through `--stdin-guard` and a server's `--import` are bounded by `DataQuietBeats` (24, 2 minutes), with or without a heartbeat, and a remote `--import` opens its session with the data's first byte, so a slow writer never trips it.
- Sync hooks never get `SyncWatchEnv` or `HeartbeatEnv`: a hook running `devopsy --export` would take its own `/dev/null` stdin for a session gone.

## Ctrl-C and signals

- devopsy takes Ctrl-C itself (`session` in `runRemote`; ssh ignores it): the first closes the pipe, and before the switch the watcher cancels the release while devopsy waits, saying so after 5 seconds; after the switch devopsy says the release goes on, and a second Ctrl-C ends the session; the log is saved either way.
- "After the switch" is `SSHStreams`' one flag (`WithLive`), set on `SwitchMarker` or on the disarm's byte answered, never the release log, which stops at its cap: from the byte the script goes on to its switch on its own, so a Ctrl-C or a session lost between the two says the release goes on, never that nothing went live.
- During the upload (`signal.NotifyContext`) a Ctrl-C ends the progress line, and nothing changed.
- A signal during the fetch only sets `fetch_sig` (INT, TERM, HUP); the fetch's child gets it too and fails, and the same path undoes it under the image lock with signals ignored from there on, exit 130, so a second Ctrl-C cannot stop the undo.
- PIPE is ignored from the fetch on: dash writes "Terminated" to the dead session.
- `FetchingFile` marks a new release's fetch until a line says its images are ready, and until then the release counts as failed.
- `img recover` (`recoverFetch`), run after the lock by every activation, a sync's holder, a `--no-code` restore, `--destroy` and `--releases remove`, marks such a release failed and bookmarks its images by its fetch's tags.
- A rollback never writes `FetchingFile`: it would mark a good release failed.
- From the end of a fetch that went through, INT, TERM and HUP are ignored to the script's end, the run step included: a trapped signal still kills the foreground child (the `ln` and `mv` of `make_current`), which `set -e` turns into the script's end halfway through the switch.
- `script` gets its own session past a fetch (`setsid -w`, `tty_detach` in `with_tty`): it reads its signals through a signalfd, ignored ones too, and exits 0 when signaled, which a restart's `get_images` would take for a fetch.
- What kills the shell past the switch leaves `ActivatingFile` (`$base/.devopsy-activating`: id, kind, the release replaced, a restore's snapshot), written right before the pending file goes and `make_current`, and removed wherever the activation ends, its failure paths included.
- An `ActivatingFile` naming current while nothing holds the release lock is an activation cut off past its switch (the OOM killer, a reboot), whose run step may not have run; one naming another release was cut before its switch and is still pending.
- `--releases` marks a cut activation `CUT`, and `--status` says so with what finishes it, the words and the fix in one place (`remote.Activating`'s `Cut`, as `ImportFixes` and `RestoreFixes` for the marks); nothing finishes it by itself.
- A `--rollback` finishes a cut rollback (its target current; the release it left, `rb_from`, is what it marks and goes back to; `$prev` stays current until the switch), never going one further back; a release or a restore over it marks that release rolled back, as the rollback would have; a sync's run step clears a cut release's or full restore's mark (the run step they skipped); any activation replaces it.
- A script's own status passes through ssh, so activations and holders never exit 255 (`NoExit255`, `followOutput`'s trap, 254 instead): 255 is ssh's.
- ssh exits 255 for a connection that failed or was lost and for a script killed outright (SIGKILL, the OOM killer), so every message on `sshLost` says both in one wording (`lostSays`), only what holds for both, with what shows it; a script that runs devopsy as its child sees the kill and says it itself in the same words (`killedLine`, `killedSays`).
- A step a cancel interrupts on the server says so (`interrupted`), since what ran under its context (docker, zstd, rsync) is killed with it and says only "signal: killed".
- The operator's words name what runs (`releaseWords`: release, rollback, restore; `goesOnSays` past the point of no return).
- An ssh killed by a signal devopsy did not send reads as 255 too (`SSHStreams`), and `DEVOPSY_SSH_COMMAND` runs through `exec`, so signals reach the command, never only a shell around it.

## From the switch on

- Right after `make_current` (after the sync, in a release that syncs) `followOutput` sends the script's output, both streams, to `ServerLogFile` (the deploy user's alone, umask 077; each activation's part after a `LogSection` line with its time and a tag) and a GNU `tail -f` copies it to the session: a session gone then kills only the tail, never the shell, compose mid-`up` or the switch back.
- `SwitchMarker`, printed just before, tells the operator's devopsy to stop logging the session and log `LogAnchor` there (`LineMarker` with `Cut`, on logged sessions only; a shell's typing and an export's data pass through untouched).
- The tail ends by traps, never `tail --pid`: half a second after the shell (an EXIT trap), or a second after a helper finds the shell gone without its trap; `tail --pid` takes an unreaped zombie for alive.
- A release that syncs and a sync's holder follow from the run step: the sync has its own way through a drop (internals/syncs.md), and the copy's lines reach the session's log through the push.
- Without GNU `tail` (macOS, in tests) only the traps apply (`TestAfterSwitchSurvives` skips).
