# Other features

Code: `cmd/devopsy/status.go`, `serverinfo.go` (and its `_linux.go`, `_darwin.go`), `sshkeys.go`, `probe.go`, `update.go`, `skill.go`; `internal/complete/`, `internal/skill/`, `internal/probe/`, `internal/update/`, `internal/cli/contexthash.go`, `internal/cli/capabilities.go`, `internal/remote/keys.go`, `internal/remote/sshconfig.go`, `internal/remote/serverinfo.go`, `internal/remote/status.go`.

## --status

- The facts are gathered where the environment is and formatted by the operator's devopsy (`statusReport`): locally in process, with a target by the hidden `--status-facts` in the current release (`remote.StatusFacts` as JSON, `--no-project` for the disks alone when no release is live), as `--server-info`'s.
- With a target, `StatusScript` prints header lines from the server's files (`remote.EnvStatus`, which `--server-info` runs per environment too, both read by `parseStatus`), then `---`, then the facts.
- The header holds the record, the live releases kept, the last completed sync from the child's `SyncedFile` (written by `finish go` once its hook accepted the copy), and a partial or running sync from `partialStatus`: "syncing" while a holder holds the child's lock, from its holding file on (`SyncRunning`), "source" while a push or an export holds the parent's sync lock (`SourceBusy`); `--releases` marks the current release SYNCING the same way.
- Every line is an entry (`remote.StatusEntry`: level, key, lines, remedies), the containers' table apart, and one formatter prints them all, keys aligned on the longest (`entryTable`, `--server-info`'s too): `--json` prints the same report, its lines and commands through `report.Copyable` too (`copyableEntries`).
- A command that acts on an entry is a fix (`report.Remedy`, its text optional, its commands: `Activating.Cut`, `ImportFixes`, `RestoreFixes`, `ResetFix`), never part of its line, naming the target as `remote.HintAddress` does: `--json` gives it as a field, and a script says the same fixes through `sayRemedies`, never lines of its own; a command that only shows more (`--log`, an environment's `--status` in `--server-info`, `--shell-host` to see what takes a disk) is a hint.
- A warning (an entry's, or a container's by `containerTrouble`, which `--server-info` uses too: restarting, paused, dead, unhealthy while running, exited on a code other than a stop's 0, 137 or 143) makes `--status` exit 3 (`statusWarned`), as `--server-info`.
- A one-off container (a compose run) is no service's: neither command lists or judges it.
- The status capability's lines are `<level>\t<key>\t<value>[\t<fix>]` (`parseStatusShow`, its levels `statusLevels`): one key on lines in a row is one entry at the highest of their levels, a line of another shape a warning naming it, a failure of `show` a warning, and the rest still shown.
- In a release `images.StatusLines` names the services held (`HeldLabel`, said), those running another image than the current release's `ComposeFile` gives them (another activation's container) and those running another image than their `devopsy.digest` label says (something outside the file for one it names an image for, a plain up that followed a moved name for a pulled one), both warned.
- The services with no container come from `compose config --services` run by the real docker (`realDocker`), never the door's, whose `config` makes the binds' entries.
- The disks are the release root's, Docker's data root's and containerd's (`docker info`, not through the door, where it is the admin's), the snapshot root's and /, with no reflink tried, judged by `judgeDisk`, which `--server-info` uses too; `DEVOPSY_INTERNAL_DISK_WARN=off` judges none, for tests.

## --server-info

- One session, `basePrelude` then the hidden `--server-facts` (a feature), which prints `remote.ServerInfo` as JSON, changes nothing and takes no lock (a temporary file in the release root for the reflink try, `FICLONE` as the deploy user, removed).
- The operator formats the facts and decides every warning (`serverInfoReport`: the thresholds, the version against its own, fixes naming the server as typed, `ServerRef`), so a judgment changes with the operator's devopsy alone; exit 3 when it warned (`serverInfoWarned`), for CI.
- Its lines are entries printed by `--status`'s formatter on stdout, each warning an entry of its own under the key of the line it is about, an environment's problems one warning with its `--status`, its lines `--status`'s warnings (`envState.entries`, which judges an environment's state for both); `--json` prints the facts and the entries (`serverReport`).
- Without a target it reads the Linux server it runs on in process (`serverFacts`), its fixes run there (no `--shell-host`, `--status` in the environment's `current`); on macOS it refuses: nothing marks a Mac a server, and the facts there were Docker's VM's.
- Kernel memory the kernel reserves outside any CMA area (Kexec HandOver's scratch on the 7.0 kernel) is `/proc/zoneinfo`'s `cma` less meminfo's `CmaTotal`, warned: the kernel's own allocations ran out with memory free; kdump's reservation (`/sys/kernel/kexec_crash_size`) is an info entry with its fix, no exit 3, since crash dumps are a choice.
- The server's time is taken last (`ServerInfo.Time`) and compared with the local clock at the facts' first byte (`firstWrite`), never at the session's end; an unsynchronized clock whose time service runs gets a fix naming a network's own time server, not `set-ntp`.
- Environments are the directories with `releases/` two or three levels under the release root, through links as the scripts reach them, never a cut `--destroy`'s `<base>.rm.<pid>`; their containers come from one `docker inspect` of all, read even when it fails, matched by compose name (`composeNameOf`).
- Facts agree with what the scripts read: roles from what `docker ps` lists, restarting and paused included (`RoleHoldersScript`, `cli.Running`), a `tail` that answers `--version` (`followOutput`), health only of a running container.
- Said, never warned: the classic image store, Docker's security options, and a sync or a snapshot under way; `EnvStatus`'s `busy` is ignored on a server without `flock`, where every lock reads held.
- AppArmor is warned about only as a kernel with it and no `apparmor_parser` (looked for in sbin, outside the deploy user's PATH): `docker info` lists AppArmor whenever the kernel has it, and Docker then loads its profile with the parser, without which no container starts.
- A full disk's fix follows what is on it (Docker's cache, snapshots); the no-reflinks fix names the server's `snapshots: {mirror: false}` first when it refuses every mirror.

## --ssh-keys

- `--ssh-keys` edits the deploy user's own `~/.ssh/authorized_keys`, or with `--for <project>` a project user's keys in the door's directory (`DoorKeysFile`), over the usual session as the deploy user: no root, both files that user's. A project user's last key may go, the deploy user's never.
- The same scripts edit both, the file a parameter (`keysPrelude`), and asked as a project user a `--for` script exits `AskedAsProjectUser` with the deploy user's name, for the hint: only the server knows it.
- Lines are parsed locally (`ParseAuthorizedKey`, the fingerprint from the blob, no ssh-keygen) and edited on the server under a lock next to the file, by blob: a comment never identifies a key.
- `add` refuses a duplicate (exit 3) and `remove` the last key (exit 4; commented-out keys are none, and devopsy refuses it locally before opening a session); `--restricted` on `add` writes `RestrictedKeyOptions`, the one place that line is written.
- `--ssh-keys-generate [<name>] [--out <file>]` is local and its own flag: `ssh-keygen`, nothing sent; the `.pub` is added per server.

## Completion and the skill

- Completion lives in Go and never connects to a server (`internal/complete`): the shell scripts only pass words to the hidden `--complete` and show what it prints (cobra's protocol), so compose's own completion passes through.
- After `@` and for `--from` it offers `~/.ssh/config`'s hosts as `@<host>:` (`sshHosts`), read in Go so every shell gets the same; bash splits words at `:`, so its script rebuilds them from `COMP_LINE`.
- A new subcommand's words go in `complete.go`, its help in `cli.RemoteCommandHelp`; `TestCompletionShells` runs the scripts in every installed shell (bash, macOS's bash 3.2, zsh with `_describe` stubbed, fish), and CI installs zsh and fish.
- The agent skill (`internal/skill/SKILL.md`, `devopsy --ai-skill`) is for using devopsy, AGENTS.md for developing it: what an agent needs to write a project's `.devopsy/`, release and operate it, pointing at `--help`, `--debug schema` and the templates rather than repeating them.
- The skill is embedded, so it describes the binary that prints it, its frontmatter carrying that version and the copy's checksum (`devopsy: <version> sha256:<hex>`, of the file with that line ending at its version; `stub` for `--stub`, the short form that sends the agent to the command).
- `Install` replaces only a copy so marked whose checksum still matches; an edited one, or one without a checksum, is the user's, left as it is and said with `rm` and the install again as its fix.
- The skill states what devopsy enforces as fact and what the templates do (10001, read-only, `alwaysup`) as what new projects should do.
- `TestContentNamesWhatExists` keeps the skill honest: every `--flag` it names is in `cli.Flags` or a flag's `Args`, every `devopsy.*` label is in the test's `publicLabels` (the docs', each checked against the code), and its config.yaml example loads.
- The skill installs into `.agents/skills/devopsy` in the project's directory (`FindProjectDir`), shared by Codex, Cursor and the skills installer, with a link from `.claude/skills/devopsy`, since Claude Code reads only its own directory; the link is relative from where the link really is (`EvalSymlinks` of both ends), a dangling one is replaced, anything else there is left and said.

## Capabilities and the environment

- Capabilities run in the project's own environment and session (`--capability <name> <action>`, hidden; executables in `.devopsy/capabilities/`), so no other project's variables leak in; the catalog (`cli.Capabilities`) lives with the code that calls it.
- devopsy always sets `COMPOSE_PROJECT_NAME` (locally the config's `project:`, else compose's `name:`, else the folder) and runs the `env` capability (`computeEnv`) before every command but help and completion; its output only fills unset variables, and `DEVOPSY_INTERNAL_ENV_COMPUTE` keeps a devopsy it calls from running it again.
- `--probe` is a tool with plain arguments, not a contract: it knows no proxy, and proxies print the `--probe` line to run.
- A server never drives local devopsy: devopsy execs ssh and cannot watch its output, and a server must not make a laptop act.

## SSH sessions

- Every session is `ssh [DEVOPSY_SSH_COMMAND's options] [--ssh-option's] [the session's own] [SSHOptions] -T|-t <host> 'sh -c <script>'` (`remote.SSHLog`), `-t` for any command at a terminal (a shell, a project or compose command, `--upgrade`), never for activations and holders (internals/sessions.md).
- `--ssh-option <key=value>` goes first, like `-v`, repeatable, `key=value` only (`cli.SSHOptionValue`), never another of ssh's arguments; `SSHCommand` adds it to every ssh, `ssh -G` included, so what devopsy resolves is what its sessions use, and it reaches nested devopsy calls through `remote.SSHOptionsEnv`, never a server.
- Every session passes `ForwardAgent=no` (`SSHOptions`), so an ssh config's `ForwardAgent yes` never forwards the operator's agent to a server's user: only a session's own option (a sync's push) or `--ssh-option` does.
- Every session passes `ClearAllForwardings=yes` (the operator's `LocalForward` lines clash between sessions), and every session refuses passwords (`PasswordAuthentication=no`, `KbdInteractiveAuthentication=no`, unless `DEVOPSY_SSH_ALLOW_PASSWORD`), never `BatchMode`, which would refuse a passphrase and a new host key too: a devopsy server takes keys only, so a prompt means a missing key, asked again per session of a release.
- Every session also bounds its connection (internals/sessions.md), and `DEVOPSY_SSH_COMMAND`'s options come first, so they win: ssh keeps the first value of each option.
- Connection sharing is the operator's SSH config, which `--ssh-config` suggests and never writes.
- Test fakes of ssh run their last argument.

## --context-hash

- `--context-hash` is a pure helper, devopsy builds nothing, and errors only ever go towards a new hash (a kept file costs a rebuild, a dropped one would reuse a stale image), hence Docker's own matcher and refusing what it cannot hash.
- It folds base images in, always (`baseImages`, `BaseDigest`): each external `FROM` (no stage, no `scratch`, global `ARG`s expanded with the build's args) by its registry's current digest (`docker buildx imagetools inspect`, no pull), one pinned by digest as its reference, and no answer fails the hash; `COPY --from` images are not covered.
- A registry request per base, and a hash that needs the network, are accepted.

## --upgrade

- `--upgrade` reads the same release files as `install.sh` (archive names without a version, `checksums.txt`); the daily check runs detached (`--upgrade-check`), and only release builds upgrade or check.
- To try them by hand, build a release build (`go build -ldflags "-X main.version=0.8.0" -o /tmp/devopsy ./cmd/devopsy`), then `/tmp/devopsy --upgrade`; the notice needs a terminal (`script -q out ./devopsy ps`).
