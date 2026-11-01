History, not rules: `internals/sessions.md` wins where they differ.

## The session

- Activations always `-T` (decided), one path for terminals and CI: a `-t` session's hangup reached only the shell, and dash runs a trap only once the command it waits for ends, so a build ran on and then compose hung on the gone terminal.
- The pseudo-terminal from `with_tty` rather than a `-t` session: BuildKit draws its live progress and colors through the pipe only under a terminal, and a `-t` session would have made the session a second path for terminals.
- `KeepAlive` and `Bounded`: a log's save, `--export` and `--import` once waited minutes on a dead connection before ssh gave up.

## The watcher

- Without a watcher (OrbStack's own SSH, which keeps stdin open and leads no process group) the fetch runs to its end; the watcher needs a real sshd, which is why the end-to-end tests run one.
- The switch waits for a byte from the session: the watcher starts past the lock and counts its quiet time from there, so a session that died silently during the lock wait, or shortly before a quick fetch, saw its release go live. Now a silent session waits until the quiet time ends it, as gone, and nothing goes live.
- Only a zero byte before any answer disarms: an answer's bytes once disarmed a release that syncs, which then switched, when devopsy had been interrupted before the sync's marker and sent its abort with the session's end right behind it.
- The undo writes to a file rather than the session or a signal: nobody reads the session once the watcher won, a write to one whose buffers are full would block with the lock held, and a signal sent before the watcher could handle it would kill it.

## The heartbeat

- Measured: a laptop asleep or a network gone is seen by the server only when TCP gives up, 16 minutes with writes pending and hours for a quiet holder. devopsy-server never sets sshd's `ClientAliveInterval`, and the client's `ServerAlive*` only help the client, hence the zero byte every 5 seconds on every pipe devopsy keeps open.
- A busy operator (a laptop asleep, Ctrl-Z) is taken for gone and cancels, cleanly; accepted.
- The quiet times are ordered because one timeout for both let a holder restore while the push still wrote into it.
- `DataQuietBeats` is 2 minutes with or without a heartbeat because an export piped into an import may stop services first and write nothing meanwhile.

## Ctrl-C and signals

- "After the switch" includes the disarm's byte answered (review round 9): a Ctrl-C or a session lost between the beat and `SwitchMarker` said nothing went live while the script went on to its switch on its own. A switch already under way when the pipe closed goes on, said.
- PIPE is ignored from the fetch on because dash wrote "Terminated" to the dead session and died of it.
- `img recover` by every lock-taker (review of 2026-10-25): a sync's holder and a `--no-code` restore ran current's images without recovering a release left fetching.
- Signals are ignored from the end of a fetch (decided): stopping the run step restarted the previous release while `script`'s compose ran on, two `up`s on one project.
- `ActivatingFile` (decided 2026-10-27): nothing finishes a cut activation by itself, since a run step started by whatever takes the lock next would surprise more than help. A release or a restore over a cut rollback marks the release it left rolled back (review round 9): it stayed live before and became the next rollback's target.
- `script` in a session of its own (`setsid -w`): it reads the terminal's signals through a signalfd, ignored ones too, and exits 0 when signaled, which a restart's `get_images` took for a fetch.
- One wording for a connection lost and a devopsy killed (decided 2026-10-27, over scripts that never exec): ssh exits 255 for both, so every message says both and only what holds for both; "once the server notices" was dropped because after a kill it is the next lock-taker that settles. drush on a PHP error exits 255, which is why scripts never exit 255 of their own.
- A step interrupted by a cancel says so because what ran under its context (docker, zstd, rsync) said only "signal: killed", which read as a broken snapshot.

## From the switch on

- The output goes to a file and a tail copies it to the session: before, a session gone killed the shell, compose mid-`up` or the switch back; now it kills only the tail.
- One stream (`exec 1>&2`): on two pipes a line on stdout reached the log after the marker and was cut.
- The tail ends by traps rather than `tail --pid` because `tail --pid` takes an unreaped zombie for alive.
