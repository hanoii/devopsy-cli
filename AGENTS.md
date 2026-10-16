# AGENTS.md

devopsy-cli is `devopsy`, a Go CLI that runs Docker Compose projects with a
`.devopsy/` directory, locally and on servers over SSH. `README.md` is the
user-facing behavior: keep it in sync with every change. The workspace
(`../devopsy/README.md`, `../devopsy/ROADMAP.md`) describes how the
repositories fit together and the open ideas.

## Layout

- `cmd/devopsy/`: `main`, which executes what `internal/cli` plans, and
  end-to-end tests that run the built binary against a fake `docker`.
- `internal/cli/`: finds the project, loads `.env` and decides what to run
  (`Build` returns a `Plan`; it only runs helpers, like the `env`
  capability, never the command); roles, exports and imports
  (`roles.go`); capabilities.
- `internal/remote/`: config and targets (`config.go`), the documented
  schemas (`schema.go`), packing a release, and the POSIX `sh` scripts run on
  servers (checked with `sh -n` in tests; GNU coreutils, `mv -T`); a
  sync's sessions and scripts in `sync.go`.
- `cmd/devopsy/sync.go`: syncs, the operator's side (`runSync`,
  `runSyncSession`, `inheritVars`) and the hidden flags servers run;
  `cmd/devopsy/data.go`: `--export` and `--import`;
  `internal/cli/sync.go`: the `devopsy.sync` labels and `SyncServices`.
- `internal/complete/`: completion candidates, and the bash, zsh and fish
  scripts (embedded).
- `internal/probe/`, `internal/update/`: `--probe`, `--upgrade`.
- `internal/report/`: how devopsy reports a failure or a warning
  (`report.Fail`: `[error] <message>` in red, `report.Warn`: `[warning]
  <message>` in yellow, then the details, why it happens, one per line,
  then the fixes: "To fix it:" and the one, or "To fix it, do one of
  these:" and each numbered, a fix's commands on their own lines; plain,
  never wrapped or indented: a line is one logical line, a command to
  copy). `report.Error` carries details and fixes through wrapping; a
  command adds the fixes only it knows. The message reads as a sentence
  ("dev's parent, prod, has no server"). New so far only for a parent
  without a server; the other messages move over as they are touched.
- `install.sh` (POSIX `sh`, dash and busybox ash), `scripts/devopsy` (runs
  the checkout's code), `.goreleaser.yaml`, `.github/workflows/`.

## Rules

- **Compose plus environment variables.** devopsy never generates or
  rewrites compose files; everything it computes shows in `devopsy --env`.
  If a feature needs to change compose files, it is the wrong feature.
- **Compatibility of the basics:** project discovery, `.env` loading
  (compose's parser, caller's environment wins), commands in `commands/`
  with `DEVOPSY_PROJECT_DIR` and `DEVOPSY_COMMAND_NAME`, the recursion guard,
  everything else passed to compose untouched. No migration code for
  anything else: older files and syntax just stop working.
- devopsy replaces itself with what it runs (`syscall.Exec`): signals, the
  terminal and exit codes belong to that command.
- **Output:** stdout is the result (compose's output, `--env`, `--vars
  get`, `--context-hash`...), stderr is everything devopsy says: progress,
  notices and errors, the server scripts' `echo` lines included (`>&2`), so
  pipes stay clean and the two never interleave out of order. Anything that
  can contain arguments or scripts goes through `cli.Secrets` (values from
  `secrets:`). Scripts are for `-vv`: an SSH session's,
  and an argument of several lines in the "Running" notice (`Plan.running`),
  which otherwise only counts its lines. Failures with something to add
  (why, what to do) go through `internal/report`: one `[error]` or
  `[warning]` line, then why and the ways to fix it on their own lines,
  never packed into the message. Hints with an address use the resolved
  one (`t.Address`), so the command works when copied.
- **Words are never built-ins.** devopsy's features are flags or `@target`;
  a word is a project command, else a compose command (`ComposeCommands`:
  compose's own completion, cached, refreshed when a word is missing), else
  an error. The bare help says so in a line and stays devopsy's own:
  compose's help is `devopsy -- --help` and `devopsy <command> --help`,
  compose's output with `docker compose` read as `devopsy`, captured, not
  exec'd, no notice. Arguments starting with a flag go to compose unchecked
  (`--profile x up`), except a flag alone: compose's flags never stand
  alone, so a lone flag devopsy does not know is an error with the nearest
  flag as a hint (`cli.LoneUnknownFlag`, in `Build` and in `runRemote`
  before any connection, whatever the address); `--` is the escape hatch.
  Flags that need a target say so without one.
- **Two scopes:** a flag in `cli.ServerScope` (`--shell-host`,
  `--ssh-keys`, `--ssh-config`, `--environments`, `--vars-project`,
  `--upgrade`) needs only
  a server and resolves with `remote.ServerTarget`: `@<server>:` alone, a
  bare name that is no environment (exact or pattern) or alias, or any
  target leniently (no instance needed, a server is). A server named
  alone has no `Name`. Every other flag needs an environment, and
  `@<server>:` with one says so.
- **`cli.Flags` is every flag of devopsy's,** by task group, each with its
  scope (`Local`, `[@t]`, `@t`, `@s`, `[@s]`) and description: the help's
  list, completion's (`LocalFlags`, `RemoteFlags`), `RemoteOnly` and
  `ServerScope` all come from it, so a new flag is one entry there (a
  flag that differs locally and on a server, like `--upgrade`, has one per
  scope). Lines fit 80 columns (`TestUsage`).
- **Project commands say where they run** (`## Run: local|target`,
  `## Needs: target|server`, `cli.CommandRun`): help's prefix (its
  `Scope`), completion's filter (`CommandOffered`), and `CheckCommand` in
  `Build`, in `runRemote` before connecting (an older server's devopsy
  would run a local one) and on release steps (`checkStepScopes`). Where a
  command runs is decided by the directory, `cli.InRelease` (`target.env`
  present), never by a variable: a shell on the server or a plain `ssh`
  has none, and a variable would reach every child. `Needs:` takes
  `Run: local` (a server alone has no release to take a command from):
  `runRemote` resolves the address (`needsServer`: as a server's), then
  execs devopsy locally in the command's project (`runAddressed`) with the
  address as typed in `DEVOPSY_INTERNAL_COMMAND_TARGET`, which `Build`
  removes and turns into `DEVOPSY_COMMAND_TARGET` for that command only:
  every other project command gets neither, so an outer one's address
  never reaches an inner one. Never the target's variables: the command
  calls devopsy with the address.
- **Secrets are opt in:** `secrets:` in `config.yaml` (names, `*`
  patterns, `path.Match`) is the only rule, whatever set the value. No
  guessing from names (a regex went, October 2026), no "everything in
  `.env`", no mask-everything fallback. `cli.Secrets`: `Value` for
  listings (`--env`, `--vars`, `--debug env`: `***` whatever the length,
  `--show` unmasks), `Mask` for messages (values of 8 characters or more).
  On a server the release's config decides; `--vars` masks locally from
  the configs `VarsReadScript` sends (every `current` under a level's
  directory, read with the lenient `ConfigSecrets`) plus the checkout's.
  The secret-like names (`secretLike`, the old regex) only ask:
  `confirmUnnamedSecrets` before `--release` (`ComposeVariables` and the
  target's `env:`, locally, no session: names only in the server's files
  are left to `--vars set`) and on `--vars set`, a prompt at a terminal,
  else a failure, `--yes` included. `public:` lets a name through.
- **`@<target> <subcommand> --help` only prints help**
  (`cli.RemoteCommandHelp`, an entry per subcommand), and resolves the
  target leniently (`DescribeTarget`), so it never needs a server.
- **Small per-project choices are compose labels** (`devopsy.shell`,
  `devopsy.shell.user`, roles); capabilities are for behavior a label cannot
  express. `--shell-host` is never overridable: the way in when a project is
  broken.
- Few dependencies, standard library first. Linux and macOS only.
- **Color** only on a terminal and without `NO_COLOR` (`styleFor`), or
  forced (`cli.ColorForced`: `FORCE_COLOR`, not `0` or `false`, or the
  internal `cli.ColorEnv`). devopsy never exports `FORCE_COLOR`: in a
  session it would reach every tool there, which would then color what
  scripts capture. A session's lines are colored
  locally as they arrive (`remote.Colorize`, line-buffered, on the terminal
  writers only, the marker taken out first in a sync): `devopsy:` lines
  cyan, red when they report a failure, Compose's done steps green and
  the steps on the way dim, errors red; coloring on the server reaches
  nothing (plain progress, echoed lines). `remote.Color` also exports
  `DEVOPSY_INTERNAL_COLOR` into sessions, for the little the server colors itself.
  Release logs get the raw lines. Help is colored by layout (headings end
  with `:`, entries start with two to seven spaces, a scope `@t` magenta
  and `@s` yellow). A terminal is another thing:
  `remote.TTY` (`DEVOPSY_INTERNAL_TTY='<cols> <rows>'`, exported into
  sessions when stderr is one, colors or not, 80x24 when its size is
  unknown) lets a session without a tty (a
  sync's `-T` holder) run the run step under `script` (util-linux, like
  `flock`; `with_tty` in `basePrelude`, plain when `script` is missing or
  stdout is a tty already), so BuildKit draws its live progress through
  the pipe as it does on a `-t` release.
- Project-specific behavior belongs in projects' `.devopsy/commands/`.

## Config and targets

- Files: the project's `.devopsy/config.yaml` (`project:`, `instances:`,
  `secrets:`, `public:`, `releases: {keep}`, `defaults:`, `environments:`), its `config.local.yaml`
  over it, and the user's `~/.config/devopsy/config.yaml` (`aliases:`; on
  servers `releases:`). Never `~/.devopsy`: discovery would take the home
  directory for a project. Unknown keys are errors.
- `defaults:` gives environments `mode`, steps, `releases`, `parent`,
  `inherit` and `env`; `env` merges key by key, `KEY: ~` removes, `""` stays
  empty (hence parsing YAML nodes: `nullKeys`); `parent: ~` and `inherit: ~`
  remove theirs. `path` is never a default.
- **Deployment facts never live in a project's config:** servers
  (`server:` is an error) and instance names. They come from the target,
  variables or aliases.
- **A target is an address,** `@[<server>:][<instance>/]<environment>`
  (`parseAddress`): the server ends at the first `:`, the instance before
  the last `/`, empty meaning none. Missing parts come from
  `DEVOPSY_SERVER_<ENVIRONMENT>`, the pattern's (`PatternVar`: `pr-*` gives
  `DEVOPSY_SERVER_PR`), `DEVOPSY_SERVER`, and `DEVOPSY_INSTANCE`: the
  caller's environment, then the project's `.env`, never for aliases. No
  server: error. `instances: true` makes every target name an instance
  (an alias's from its `to:` only); otherwise none may, so a project's
  environments never mix `<project>/<env>` and `<project>/<instance>/<env>`
  on a server, and `--environments` lists one layout. Explicit none
  (`@vm1:/prod`) only parses: it is the default.
- **Names come only from project, instance and environment:** compose name
  `<project>[-<instance>]-<environment>`
  (`Target.ComposeName`), directory `<project>/[<instance>/]<environment>`
  under the server's release root, nothing else: `path:`, which moved
  the directory, went (October 2026; the release root is the server's
  `releases: root`). `project:` is
  required for anything remote, never taken from the checkout's folder.
- **Patterns:** environment keys with `*` (one or more name characters);
  exact name, then most literal characters; a tie is an error.
- **Aliases** (`{to, source}` or `{to, project}`): only a bare `@name`, and
  a project's exact environment wins. With `source:` its config gives the
  project, environments and steps, and releases run only from there
  (`ReleasesHere`, symlinks resolved): anywhere else a release would upload
  whatever project devopsy runs in.
- **Release settings belong to the server:** its deploy user's `releases:`
  (`root`, default home; `keep`, `max_keep`, default 5; `keep` counts
  releases that went live, failed ones only go once older than the oldest
  kept), read by the hidden
  `--release-settings` that every script asks first (`basePrelude`), so a
  laptop and CI resolve the same paths. A project's `keep` is capped by
  `max_keep`; `root` and `max_keep` in a project are errors.
- `--init` writes a new config and never touches an existing one.
  `--debug schema [--user]` prints `ProjectSchema` / `UserSchema`,
  documentation written as configs that parse (`TestSchemas`): a new key
  goes there too.

## Releases

- Remote commands run through the `current` symlink, never a release path:
  compose stores bind-mount paths in containers, and pruned releases would
  break them. The exception is `prepare`, in the new release before it is
  current: it must not start services.
- **Steps of what is deployed come from the server:** a rollback's
  (`--release-steps rollback <phase> [<release>]`, hidden,
  `runReleaseSteps`), the restart after a failed run and `--destroy`'s
  step (`destroyStep`) are read from the release's own `config.yaml`, its
  environment from `target.env` (`ReleaseEnvironment`), never the
  checkout's, which may have changed or be another branch. A release's
  come from the checkout: it is what is uploaded. A rollback's local
  before and after steps stay the checkout's.
- `--release` and `--rollback` take no command: the environment's
  `release:`/`rollback:` (`remote.Steps`: before, prepare, run, after; a
  plain string is run alone) are required. Phases, not a free list, so the
  upload and the switch are never hidden in a step. The remote steps share
  one SSH session, which holds the lock (a `flock`, `lockWait`: `-n`
  first, then a "waiting for" line and `-w 600`, so a wait is never
  silent): so before is local
  steps then remote ones, after is remote then local, checked when the
  config is read. Local steps get the target's env, `DEVOPSY_TARGET` (the
  resolved address) and the commit, never the server's `.env`.
- Order on the server (`ActivateScript`, one session): lock; remote before
  steps in `current` (skipped on a first release); link `shared/*` and the
  project's and instance's `.env` (`project.env`, `instance.env`, linked
  even when missing) into the release; `--prepare-release` when the compose
  files use role or import labels (`PrepareScript`, releases only);
  `--check-names "$base"` (`cli.ProjectConflict`: containers of the
  same compose project with a working directory outside `$base`, since
  `-`-joined names can coincide, `a-b`/`c` and `a`/`b-c`; then
  `cli.NetworkConflicts`: a network the compose config
  names as its own that another compose project's label owns fails the
  release, since Compose would just share it; external ones are how a
  project joins another's; an unreadable config is skipped, the up will
  say); `prepare` steps in the release; `--missing-vars` (`cli.MissingVars`:
  compose's `${VAR:?}`/`${VAR?}` left unset, env capability included); the
  images (`fetchImages`, by mode: build `pull --ignore-buildable` then
  `build --pull`, image `pull`, which pulls buildable services with an
  `image:` too and fails on a missing tag; through `devopsy --`, past
  project commands, under `with_tty`), bookmarked (`releaseImages`:
  `compose config --images` into `$rel/.devopsy-images`, each tagged
  `devopsy-<compose name>-<id>`, digests skipped; a rollback or restart
  restores those, all or nothing, else fetches; a fetch failure restores
  `$prev`'s; pruning untags); switch
  `current`; `run`; on failure switch back and restart the previous release
  with its own run step (`--release-steps restart run`: its own config's
  rollback:, else release:), its own images back first (`get_images`);
  prune to
  `keep`; remote after steps. A failure before the switch calls `not_live` (`NotLive`, `RollbackNotLive`): nothing live
  changes. No prompts: what is missing is listed with its `--vars set`
  line.
- Release logs (`releaseLog`): devopsy's messages, local steps' command
  lines and exit codes, and the upload and activation sessions' output
  (`SSHLog`, both streams), each line after its UTC time (`2006-01-02
  15:04:05`), without terminal codes, progress frames (a `cursorUp` code
  supersedes the n lines before it: BuildKit on a pseudo-terminal redraws
  its block every tick, only the last frame stays) or empty lines, saved by
  `LogSaveScript` in `releases/<id>/.devopsy-log` once the server was
  reached; a rollback's is appended to the restored release's, found from
  its script's "rolling back to <id>" line. Pruned with their releases.
  `--log` reads them (`LogReadScript`). Local
  steps' output is not piped: it would lose its terminal.
- Before uploading: a role taken by another compose project fails
  (`RoleHoldersScript`), and a new instance asks (`InstanceExistsScript`,
  `--yes`). A first release that fails before going live, with nothing in
  `shared/` yet, removes its directory.
- `target.env` is written into each release (environment `env`,
  `COMPOSE_PROJECT_NAME`, overriding compose's `name:`, `DEVOPSY_PROJECT`,
  `DEVOPSY_INSTANCE`, `DEVOPSY_ENVIRONMENT`, `DEVOPSY_TARGET`,
  `DEVOPSY_RELEASE_COMMIT`), so it applies however devopsy runs on the
  server and rollbacks restore it. Precedence there: caller, `shared/.env`,
  `inherit.env` (a child's, from its parent's `inherit:`), `instance.env`,
  `project.env`, `target.env`.
- A nested devopsy (`DEVOPSY_PROJECT_DIR` set) ignores the inherited
  `COMPOSE_PROJECT_NAME`: it is the local project's, not the target's.
- `mode:` defaults to `build`: it works for every project, where a wrong
  `image` fails the release. devopsy fetches the images itself, before the
  switch, so the run step only starts them: a build or pull there would
  run after the switch, and in a sync after the copy, where nothing had
  a terminal for it (the first release's `up` in `restoreServices` built
  the app in plain progress, then `deploy` built it again).
- `--release` without a target (`runLocalRelease`, its own `cli.Flags`
  entry with `Local` scope) is a release in place: `Config.Defaults`'
  release steps (no environment is local), prepare, `--missing-vars`, the
  images always as build mode, run; no before or after steps, no switch.
  `--destroy` likewise (`runLocalDestroy`): after the project's name,
  `runDestroyData` in place, `defaults:`' destroy step if any; never the
  checkout. The other release flags stay target only.
- Release ids are UTC seconds (`NewRecord`): `UploadScript` refuses an
  id that exists on the target (two releases of one target in the same
  second), since `mv` would put the new tree inside the old release and
  the old one would run again. Tests that release a target twice wait
  for a new second (`spaceReleases`).
- Releases are full tar streams (no rsync: macOS ships openrsync). The
  upload's progress (`uploadProgress`) counts the project's bytes as
  `Pack` reads them, out of `remote.Size`: through the pipe, what ssh
  has taken, not the gzip's size, which is unknown until the end. One
  line redrawn on stderr when it is a terminal, never logged; the log
  and CI get the summary line. Server
  needs: `devopsy`, `tar`, `flock` and `script` (util-linux; without the last, no live build progress on syncing releases); locally only `ssh`.
- `--vars`, `--vars-instance` and `--vars-project` (one file each, so
  flags, not options of `--vars`) send values over SSH's stdin, never as
  arguments; edits replace
  keys in place, under the release lock (deploy commands write secrets too).
- `--destroy` (`DestroyScript`, under the lock; asks for the target's
  name unless `--yes`): the hidden `--destroy-data [step]`
  (`runDestroyData`) in `current`, else the newest release: the
  environment's `destroy:` step if it has one (optional: a hook, run
  while everything is up), then `compose --profile '*' down --volumes
  --remove-orphans --rmi local` and `mnt` emptied as root in `UtilImage`
  (`find -delete`, a plain `docker run`: `compose run` would bring the
  network back); then every release's image bookmarks (`drop_images`);
  then `rm -rf`. A failure before the removal removes nothing. Until
  October 2026 the step was required and did all of it, which is why the
  templates had `destroy` commands: devopsy had no root container then.
- `.devopsy/mnt/` is devopsy's one data directory (README, "Data and
  mounts"): never uploaded, `shared/mnt` created by the upload and linked
  like every `shared/` entry. Its contents and their ownership are the
  project's; devopsy checks whether it is empty (`NotLive`), and only
  `--destroy` and an `--import` (its entries) remove anything in it.
- An environment's directory with `.devopsy/` and no `current` is a plain directory:
  commands run there, releases refuse before creating anything.

### Syncs

- **Parents:** `parent:` names a concrete environment (a pattern is an
  error; one naming itself has none, so `defaults: {parent: prod}` works;
  a cycle is an error: with mutual parents neither could be released
  first, a first release syncing from a released parent). It resolves one
  level only (`ParentTarget`, whose own stays nil): at the child's instance, its server from the usual variables
  only, never the child's (a guess would copy the wrong data). Strict loading
  records an unresolvable parent in `ParentErr` (`ParentTarget` nil)
  instead of failing: only `--sync` and a `--release` that syncs fail
  with it, so every other command works on the child. `DescribeTarget`
  resolves it leniently, maybe without a server. `target.env` and local steps get
  `DEVOPSY_PARENT` and `DEVOPSY_PARENT_TARGET`.
- **When:** a child's first release (`CurrentExistsScript` prints `current`
  or `none`, exit 0; one extra session on every release of a child),
  `--release-sync` (any release), never `--release-no-sync`, and `@<child>
  --sync`, which asks for the name like `--destroy` and reruns `run`.
  `--from <address>` (`@` optional) on `--release`, `--release-sync` and
  `--sync` copies from that address instead of `ParentTarget`, `ParentErr`
  ignored: a child's only (`parent:` required), and on `--release` only a
  first one (a released child says to use `--release-sync`);
  `--release-no-sync --from` is an error. `failParent` lists the fixes:
  the variable, the same release with `--from`, and a release without the
  data then `--sync --from`.
  Everything that can fail (`syncPreflight`: a run step, the parent
  released, across servers a local agent and a child reachable without
  `ProxyCommand`/`ProxyJump`) is checked before the upload or the holder.
- **Two sessions,** both from the operator (`runSyncSession`): the holder
  on the child's server (`HolderScript`, or `ActivateScript` with
  `SyncOptions` between `make_current` and `run`) takes the release lock,
  runs prepare, and prints the marker with its directory
  (`SyncMarker` + `$base`: the operator does not know the release root),
  swallowed by `markerWriter`. Then the push on the parent's (`PushScript`:
  shared `flock -s` on its release lock, exclusive on `.sync-lock` so
  syncs from one parent serialize, `devopsy --sync-push`), with
  `ForwardAgent=yes` across servers. The push's exit gives `go` or `abort`
  to the holder; the holder's exit is the command's.
- **fd 3:** the holder keeps devopsy's answer on fd 3 and gives every
  command `/dev/null` as stdin, so nothing it runs can eat or wait on the
  answer. No answer (devopsy interrupted, connection lost): the holder
  sends its output to `$base/.devopsy-sync.log` (a write to the dead
  session would kill what restores the services) and restores. From
  prepare until the services are restored the holder ignores SIGPIPE
  (`trap '' PIPE`, reset before `run`) and restores before it reports, so
  a session gone during prepare cannot leave them stopped either.
- **Interrupts** (SIGINT, SIGTERM in `runSyncSession`): before the marker
  devopsy just exits, and the holder restores on its own. After it, the
  push session's ssh gets SIGTERM (`remote.SSHContext`, SIGKILL and its
  output cut after `SSHKillDelay`) and is waited for, then the holder gets
  `abort`, and devopsy exits 130. After `go`, during the run step, the
  first Ctrl-C only says the step goes on on the server (cutting the
  session would fail it and bring the previous release back); the
  second cuts it. The push session's stdin is a pipe
  devopsy keeps open (`SyncWatchEnv` says so: an older devopsy's
  `/dev/null` would read as a session gone at once), so the parent's
  `--sync-push` sees EOF as soon as the session ends, not at its next
  write, which a silent copy may not make for minutes (seen on OrbStack:
  the parent stayed paused, then ran the old push capability into the
  aborted child); cancelling closes the pipe's write end at once, as
  sshd closes the remote stdin when the connection goes (the tests' fake
  ssh runs through dash on Linux, which never execs the last command of
  `sh -c`, so a signal to it does not reach the push). EOF or a signal
  cancels it: the same-host rsync
  container is removed (`dockerRun`: killing the docker client leaves the
  container copying), the `-R` session ends (`SSHCommandContext`), the
  parent's services are restored, and no export starts; one running, in
  its own process group (`runSyncHook`), gets SIGTERM, a pipeline's
  commands included, its spool removed (same server) or left to the
  child's abort, and the import, if running, gets SIGTERM and removes
  its spool. From the cancellation on, `errOut` also
  appends to the parent's `$base/.devopsy-sync.log` (`quietWriter.keep`,
  the holder's file on the child's side), after a timestamped line. What runs inside a container on its own
  (an import already under way in `docker exec`) runs to its end. The
  holder ending first (its server's connection dropped) is a case of the
  same select: it restored the child on its way out, so the push is
  cancelled the same way, nothing is answered, and the sync fails.
- **Labels opt in:** `devopsy.sync=rsync|stream` (`cli.SyncLabel`), no
  label or an empty value is `skip` (neither copied nor touched), any
  other value an error (no migration: `live`, `pause`, `stop`, `custom`
  went in October 2026 with the `push` and `flush` actions, and
  `--sync-rsync`). `devopsy.sync.stop=true|false` (`SyncStopLabel`,
  default false, ignored without `devopsy.sync`) stops the service on
  both sides for the whole sync, a stream included. `pause` is gone for
  good: Traefik drops a paused container's router, and `docker pause`
  flushes nothing. `devopsy.sync.rsync.exclude` (`SyncExcludeLabel`,
  entries of `mnt`, comma or space separated) drops entries from an
  rsync service's `Dirs` at parse time, so every consumer (copy, guard,
  list) agrees; on a `stream` service it is ignored, not validated, so a
  variable can flip `rsync`/`stream` and keep it. A `.` entry (`./mnt`
  itself) subsumes the rest, the unlabeled and `stream` services'
  entries too: `--sync-push` says so (`SyncWholeNotice`, in yellow when
  the operator's terminal takes colors, `ColorEnv` passed with the
  push's variables), never excludes by itself. No labeled service:
  "nothing to sync", success, and the run step still runs. Each side
  reads its own labels; before the parent changes anything,
  `--sync-push` always asks the child for `--sync-services list`
  (through `--sync-exec`, one more session across servers) and refuses
  (`syncConflicts`) when a child service mounting a copied entry is
  running and not `rsync` there (a `stop=true` one is stopped already):
  a `stream` or unlabeled service the copy would change under; and
  (`streamMismatches`) when a parent `stream` service is not `stream` on
  the child, whose import would refuse it after the rsync, or a child
  `stream` service is `rsync` on the parent (a label from an inherited
  variable cannot lag: the child reads the pending file, see inherit;
  one in the child's own `shared/.env` can still differ). The
  list's second column is the label, `rsync`, `stream` or `skip`, with
  `+stop`; the third the container's state (`none` without one); a child
  devopsy that prints no state is not checked.
- **Streams:** per `stream` service, in `depends_on` order over every
  service (`SyncStreamServices`: dependencies first, ties alphabetical,
  a cycle an error), the parent runs the capability's `export <service>`
  (`syncStream`) with stdout into the child's spool,
  `$base/.devopsy-sync.stream.<service>` (`remote.StreamSpool`; every
  file a sync leaves in `$base` starts with `.devopsy-sync.`): on the same
  server the file opened under `DEVOPSY_SYNC_TO_BASE`, across servers
  `cat >` in an SSH session to the child (`exportInto`: `ChildSSH`,
  `KeepAlive`, `ClearAllForwardings`), and only once the export exited 0
  (and the cat) `--sync-exec --sync-services import <service>`, which
  brings the child's service up (`up -d --wait`, `upFor`) unless it is
  `stop=true`, and down again afterwards if it was down (kept up on a
  first release, no containers at all, for the finish hook), runs
  `import <service>` with the spool as stdin, and removes the spool
  whatever happens. The import's stdin is a pipe the push keeps open
  until the import ends or the sync is cancelled, with `SyncWatchEnv`
  (through `--sync-exec`: `cleanEnvKeys`, `syncExecEnv`), so the import
  watches it (`watchSession`): across servers the session's end, which
  a remote process never gets as a signal, cancels its hook's process
  group and removes the spool; on the same server it also gets SIGTERM. Never a pipe into the import: a cut
  export never reaches it, which every old `push` had to check itself.
  A failed export's spool goes at once on the same server, by the
  child's `finish abort` (`$base/.devopsy-sync.stream.*`) across servers.
- **Who stops what:** the child's `stop=true` services by the holder,
  for the whole sync (`--sync-services prepare`: the import check, the
  `prepare child` hook, then one `compose stop`, so Compose orders them
  by `depends_on`, dependents first: keep it one call; `--sync-services
  finish go|abort` starts them and waits until healthy, removes the
  spools on abort, then runs `finish child go|abort`). The child's
  stopped services stay down through the imports (no app on a
  half-imported database). The parent's inside `--sync-push`
  (`runSyncPush`): the checks (labels, the `export` script when a
  service is `stream`, the child guard, the util image) before
  anything, then `prepare parent`, stop, rsync, streams, start (healthy),
  `finish parent go|abort`. Hooks run from Go (`runSyncHook`, `--capability
  sync ...` in its own process group), only where their file is, with
  `DEVOPSY_SYNC_RSYNC`, `DEVOPSY_SYNC_STREAM`, `DEVOPSY_SYNC_STOP`
  (`cli.SyncLists`, that side's labels) plus the sync's
  `DEVOPSY_SYNC_FROM_TARGET`, `DEVOPSY_SYNC_TO_TARGET` and
  `DEVOPSY_SYNC_ARGS`; the holder's scripts (`syncPrepare`,
  `syncFinish`) only call `--sync-services`. A failing prepare aborts
  before its side stops anything; `finish abort` runs. Stopping records
  what it stops in `$base/.devopsy-sync.services` (`syncState`, `stop
  <name>` lines). The record exists before anything can fail
  (`startSyncState`: the child's prepare creates it before the import
  check and the hook, `stopServices` before its first `ps`), because no
  record means restore every `stop=true` service, a service down on
  purpose included; one left by an earlier sync that could not restore
  is kept and added to. Restoring brings the recorded ones back, from
  container state (exited), so it also recovers after an interrupted
  sync, and a service down before the sync (a maintenance stop) stays
  down. A child's first release (no container at all when prepare
  started: a `first` line in the record, since a stream's import creates
  its container before finish looks) gets every labeled service created
  in one `up -d --wait` before the finish hook, which sanitizes through
  them (drupal11's needs the app: without it, prod's passwords reached a
  child), and a stream imported then stays up; never otherwise, since a
  missing one was removed on purpose. A restore that brought everything
  back removes the record.
  `--sync-push` and an import cancel on SIGINT, SIGTERM, SIGHUP, SIGPIPE
  and restore; servers' commands write to `errOut`, never an `*os.File`,
  so a gone session cannot kill them with a broken pipe.
- **Hidden flags** (dispatched in `main.go` before anything else):
  `--sync-push`, `--sync-exec <devopsy args>`, `--sync-services prepare |
  finish go|abort | import <service> | list`. The first two need the
  push's `DEVOPSY_SYNC_TO_DIR` and `DEVOPSY_SYNC_TO_PROJECT`.
  `DEVOPSY_SYNC_ARGS` (space-joined, split like `StepArgs`) go to rsync,
  and to the hooks to read.
- **Clean environment:** on the same server the child's devopsy
  (`--sync-exec`) runs with `cleanEnv`: the machine's variables, the
  sync's addresses and arguments (`DEVOPSY_SYNC_FROM_TARGET`,
  `DEVOPSY_SYNC_TO_TARGET`, `DEVOPSY_SYNC_ARGS`, for the import; across
  servers `SyncExecScript` exports them) and the child's
  `COMPOSE_PROJECT_NAME` only. The caller's environment wins over
  a project's `.env`, so the parent's secrets and project name would leak
  into the child's commands. Over SSH the login environment is clean.
- **SSH between servers:** through `DEVOPSY_SSH_COMMAND` too, with
  `ChildSSH` on the parent's sessions to the child: `ForwardAgent=no`
  (the operator's agent reaches the parent, never the child, whatever
  the parent's ssh config says; the parent can use every key in it for
  the session, prod's included when a chain's middle environment is the
  parent, which only the operator's agent limits: fewer keys, or
  `ssh-add -h` constraints, README), `BatchMode=yes` (a refused key fails
  instead of prompting), `ConnectTimeout=30` (an unreachable child fails
  before the TCP timeout, with the parent's services stopped meanwhile)
  and `StrictHostKeyChecking=accept-new`. The rsync session (`-R` to the
  parent's daemon, `--network host` on the child) passes
  `ExitOnForwardFailure=yes` and never `ClearAllForwardings`, which clears
  command-line forwardings too (`ssh_config(5)`); a taken port retries
  another (3 tries, 20000-60000). `--sync-exec`'s session keeps
  `ClearAllForwardings`, and so does a stream's `cat` session. All the
  sync's sessions (holder, push, `-R`, `--sync-exec`, `cat`) pass `KeepAlive` (`ServerAliveInterval=30`,
  `ServerAliveCountMax=4`): the holder is quiet for the whole copy, and a
  dropped one aborts mid-copy. The preflight asks the local agent for its
  keys (`agentHasKeys`, `ssh-add -l`: `SSH_AUTH_SOCK` set is not an
  agent), so an empty or dead one fails before any session. No agent on
  the parent with `DEVOPSY_SYNC_TO` set means a `restrict`ed key or
  sshd's `AllowAgentForwarding no`: say so. A `restrict`ed key also refuses
  the `-R` (`port-forwarding`), which sshd reports like a taken port: after
  the last try, say both.
- **Transport:** rsync always in `devopsy-util:<n>` (`UtilImage`, which was
  `devopsy-rsync` until October 2026; built
  on each host from `UtilDockerfile` when missing; tar for `--export` and
  `--import` too, GNU's), as root with
  `--numeric-ids`: owners and modes survive, the deploy user's permissions
  do not matter. Across servers the parent's daemon serves one module per
  entry being copied (`rsyncdConf`, `RsyncModule`: the entry's name, `mnt`
  for `.`), so nothing else under `mnt` can be asked for. rsync's exit 24 (files vanished during the copy, a
  running service's temporary files) is a notice on both paths
  (`RsyncVanished`), any other failure aborts. Bump the tag whenever the
  Dockerfile changes (the alpine version or a package included: `:2`
  added tar): hosts never rebuild an image they have. Entries are
  the top-level entries of `mnt` that `rsync` services bind-mount
  (`docker compose --profile '*' config`, lexical and resolved paths;
  `.` subsumes the rest). An entry that is a file on the parent is copied
  as one (`files` in `syncRsync`: no trailing slash, into `mnt`,
  `--inplace` so the container bind-mounting it sees the new content,
  a bind mount being the file's inode; across servers its module is
  `/mnt` filtered to it, a module being a directory).
- **inherit:** `inheritVars` reads the parent's three files in one session
  (`InheritReadScript`), takes each key from the nearest level, and writes
  them all as the child's `shared/inherit.env` (`InheritWriteScript`,
  values on stdin, under the child's lock, the whole file: a resync
  replaces it), which `shared/*` linking puts in every release and the
  loader reads below `.env`. The script prints the keys the child's own
  `shared/.env` also sets, for the notice. `--inherit` (`runInherit`)
  runs it alone, both sides released. A sync inherits before its holder
  starts (`runSync`; for releases after local before steps and before
  the upload): a first release (`syncJob.first`) into
  `shared/inherit.env`, so `--missing-vars` sees them, kept on failure
  (the next release is first again); a re-sync (`--sync`,
  `--release-sync` of a released child) into the pending
  `$base/.devopsy-sync.inherit.env` (`remote.SyncInheritPending`,
  `InheritWriteScript`'s `pending`). The holder exports
  `DEVOPSY_INTERNAL_INHERIT_FILE` (`remote.InheritPendingEnv`) naming it
  for everything it runs, and `--sync-exec` sets it for the child's
  devopsy from `DEVOPSY_SYNC_TO_BASE` (`childInheritEnv`; never for the
  parent's own reads); the loader reads that file in place of the
  release's `inherit.env`, same precedence, when it exists. So the
  child's labels follow an inherited variable in the same sync, where
  writing after the copy made them lag one sync. `sync_finish go` moves
  it over `shared/inherit.env` (`mv -Tf`, linked into the release if the
  link was missing) before restoring services, `finish child go` and the
  run step; `sync_finish abort` removes it, so a failed copy writes
  nothing. A holder told the operator wrote none
  (`SyncOptions.Inherit` false: no `inherit:` on the parent, a first
  release) removes a stale one, left by a holder that died, before
  reading anything. Notices name keys, never values.

### --export and --import

- `--export <service>` and `--import <service> [--yes]` (`cmd/devopsy/data.go`,
  scope `[@t]`) move one labeled service's data the way its label says;
  an unlabeled service is refused. `stream`: the capability's `export`
  and `import` (`runSyncHook`, the lists in their environment). `rsync`:
  a tar of the service's entries of `mnt` (exclude honored, file entries
  included) made in `UtilImage` as root with `--numeric-owner`, and on
  import listed first (`tarEntries`: a truncated tar fails `tar -t`
  before anything changes, and entries the service does not mount are
  refused, an excluded one by its label; and the spool must end with two
  zero blocks, which every tar writer adds and `tar -t` does not
  require, so a tar cut at a member boundary is refused too), then each
  carried entry emptied, never removed (a running
  container bind-mounts its inode), and extracted with `--overwrite`
  (files in place). `stop=true` stops the service around either and
  restores it (`stopFor`), interrupts included: the label is the
  contract, every caller obeys it. A `stream` import of a service not
  stopped brings it up and down again if it was down (`upFor`). A
  service mounting `./mnt` itself carries all of `mnt`: a notice names
  the other services' and excluded entries (`wholeMntNotice`).
- stdout is the data, so `--export` refuses a terminal and says
  everything on stderr; `--import` refuses a terminal as stdin and spools
  it whole first (`importSpool`: in a release `$base/.devopsy-sync.stream.<service>`,
  the sync's file; locally a temporary `.devopsy/.devopsy-sync.stream.*`,
  which `excluded` keeps out of releases), refuses an empty one, and only
  then stops anything. The spool always goes.
- On a target the server runs the same code: `ExportScript` (shared
  release lock and exclusive `.sync-lock`, like the push: both stop and
  restore the same services, `devopsy --export` in `current`, stdout
  through as it is: never `Colorize`d) and `ImportScript` (exclusive
  lock, `devopsy --import <service> --yes`). The local side asks for the
  environment's name on `/dev/tty` (`confirmImport`: stdin is the data;
  no terminal and no `--yes` fails), then sends stdin and, only after a
  clean EOF, an end line with a random nonce (`ImportEndEnv`,
  `DEVOPSY_INTERNAL_IMPORT_END`): a session cut halfway reaches the
  server as a plain EOF, so the server's `--import` requires the line at
  the end and takes it off. A pipe from a failed export still looks
  complete: README says to go through a file for what matters.

## Roles, exports and imports

- Labels for facts shared between projects on a host (`roles.go`):
  `devopsy.role` (one compose project per host), `devopsy.export.<KEY>`,
  `devopsy.import.<VAR>=<role or project>/<KEY>[?]`. devopsy knows no role
  or key names; `proxy` and `WILDCARD_DOMAIN` are the Traefik template's
  convention. devopsy's core defines no proxy contract.
- `--prepare-release` checks the role and writes imports into the new
  release's `target.env` (after `cli.ImportedMarker`) before it goes live;
  an import never overrides what the environment sets (even empty); an
  empty export is a value; rollbacks never check. Exports are read from
  running containers (`Running`: one snapshot per lookup), waiting up to
  `WaitForExporters` for required sources.
- `--debug imports` compares every running project's imports with its
  release's `target.env` (found through compose's working_dir label).
- Roles are advisory: anyone in the docker group is root-equivalent.

## Other features

- **`--status`** (`cmd/devopsy/status.go`): locally, `docker compose ps
  --all --format json` as a table, then the `status` capability's `show`
  with stdout through. With a target, `StatusScript` prints header lines
  (the release's record, releases kept, the last sync from the logs'
  "sync of" and "Syncing @" lines), `---`, then `devopsy --status` in
  the current release; the operator formats the header (`ago`) and
  prints the rest as it came. A server's devopsy without `--status`
  fails there, which the output shows.
- **`--ssh-keys`** (`cmd/devopsy/sshkeys.go`, scripts in `internal/remote/keys.go`)
  edits the deploy user's own `~/.ssh/authorized_keys` over the usual
  session: no root, so devopsy-cli stays out of host setup except for
  this file, which that user owns anyway. Lines are parsed locally
  (`ParseAuthorizedKey`: options, type, blob, comment; the SHA256
  fingerprint is computed from the blob, no ssh-keygen needed) and
  edited on the server under a lock next to the file, by blob, so a
  comment is never what identifies a key. `add` refuses a duplicate
  (exit 3), `remove` the last key (exit 4). `--ssh-keys-generate` is local,
  its own flag (it never takes a target): just
  `ssh-keygen` into `~/.ssh/<name>`, nothing sent; the `.pub` is added
  per server. `--restricted` on `add` is `RestrictedKeyOptions`, the one
  place that line is written (devopsy-server's `ci-key` step went).
- **Completion** lives in Go (`internal/complete`); the scripts only pass
  words to the hidden `--complete` and show what it prints, cobra's
  protocol (`value<TAB>description<TAB>group` lines, then `:<directive>`),
  so compose's own completion passes through. It never connects to
  servers; after `@` it offers `~/.ssh/config`'s hosts as `@<host>:`
  (`sshHosts`: `Host` lines and `Include`s, patterns skipped, read in Go
  like the shells' own ssh completion, not delegated to them, so every
  shell gets the same), with no space when only those are left. `--from` offers the same
  addresses (`addresses`), aliases aside, always with their `@`; devopsy
  takes one without it too. New flags go in `cli.Flags`, subcommands in its lists. `TestCompletionShells`
  runs the scripts in every installed shell (bash, macOS's bash 3.2, zsh
  with `_describe` stubbed, fish); CI installs zsh and fish. bash splits
  words at `:`, so its script rebuilds them from `COMP_LINE`.
- **`--debug`** explains what devopsy sees; targets carry `From` (each
  value's origin), and `Env` records every definition of a variable as it
  loads (`EnvDef`, `TraceEnv`: `--debug env`), so the trace is the loading
  itself, not a second implementation of its precedence. A new source of
  variables records its definitions too. The capability catalog
  (`cli.Capabilities`) lives with the code that calls it.
- **Capabilities** (`--capability <name> <action>`, hidden): executables in
  `.devopsy/capabilities/`, run in the project's own environment and SSH
  session, so no other project's variables leak in. Define one only when a
  second use needs it.
- **devopsy knows no URL.** It always sets `COMPOSE_PROJECT_NAME` (locally
  the config's `project:`, else compose's `name:`, else the folder) and
  runs the `env` capability (`computeEnv`) before every command but help
  and completion: projects derive hosts, rules and URLs themselves, from
  the name and imported facts. Its output only fills unset variables;
  `DEVOPSY_INTERNAL_ENV_COMPUTE` keeps a devopsy it calls from running it
  again.
- **`DEVOPSY_INTERNAL_*`** are devopsy talking to devopsy (`COLOR`, `TTY`,
  `ENV_COMPUTE`, `SYNC_WATCH`, `COMMAND_TARGET`, `IMPORT_END`,
  `INHERIT_FILE`): never documented as settings, free to
  change in any release. Settings people set and the contract projects
  read keep plain `DEVOPSY_*` names.
- **`--probe`** is a tool with plain arguments, not a contract: it knows no
  proxy. A server never drives local devopsy (devopsy execs ssh and cannot
  watch its output, and a server must not make a laptop act): proxies print
  the `--probe` line to run.
- **SSH:** every session is `ssh [DEVOPSY_SSH_COMMAND's options]
  [SSHOptions] -T|-t <host> 'sh -c <script>'` (`remote.SSHLog`):
  `ClearAllForwardings=yes`, since the user's `LocalForward` lines clash
  between sessions. Anything faster (connection sharing) is the user's SSH
  config, which `--ssh-config` suggests and never writes; built-in sharing
  is in ROADMAP.md. Test fakes of ssh run their last argument.
- **`--context-hash`** is a pure helper; devopsy builds nothing. Errors only
  ever go towards a new hash (a kept file costs a rebuild, a dropped one
  would reuse a stale image), hence Docker's own matcher and refusing what
  it cannot hash.
- **`--upgrade`** reads the same release files as `install.sh` (archive
  names without version, `checksums.txt`). The daily check runs detached
  (`--upgrade-check`), since devopsy execs right away; only release builds
  upgrade or check.

## Gotchas

- Compose reads `--env-file` more than once: `<(devopsy --env)` fails, a
  file works.
- `.env` values in double quotes are interpolated by compose's parser.
- GitHub can deliver a tag push several times: the release workflow queues
  runs per tag and GoReleaser replaces assets. If a run fails, check the
  release's assets before assuming it broke.
- GitHub's `releases/latest` can lag a new release by minutes: `devopsy
  --upgrade vX.Y.Z` installs one directly.
- Go's HTTPS to github.com is slow (about 1.5 s), hence the detached check.
- Traefik picks up a new container a few seconds after `up` returns.
- `docker pause` flushes nothing: rsync sees what the kernel has, not what
  sits in a paused process's buffers. Hence no `pause` label any more:
  `devopsy.sync.stop=true` (or a stream's dump) gives consistency.
- A stopped routed container loses its Traefik router (404); a paused one
  keeps it only until Traefik reads the containers again (any start, die
  or health event on the host): Traefik 3.7 filters paused containers as
  non-running (devopsy-traefik's AGENTS.md).
- OrbStack machines are `ProxyCommand` hosts from macOS: syncs across
  servers need hosts reachable directly (`ssh -G` without a proxy), like
  `<machine>.orb.local` with a key of your own. Their
  `/etc/ssh/ssh_config.d/10-orbstack.conf` sets `IdentityAgent`, which
  hides a forwarded agent from the parent's `ssh` ("get_agent_identities:
  Permission denied"): remove it on test machines.

## Checks

```sh
go vet ./... && go test -count=1 ./...
docker run --rm -v "$PWD:/mnt" -w /mnt koalaman/shellcheck:stable -s sh install.sh
```

Before tagging, run the tests on Linux too: CI does, and macOS hides
differences that matter there (dash as `sh`, which never execs the last
command of `sh -c`; timing, like two releases in the same second). The
caches keep reruns fast:

```sh
docker run --rm -v "$PWD:/src" -w /src -e GOFLAGS=-buildvcs=false \
  -v devopsy-gocache:/root/.cache -v devopsy-gomod:/go/pkg/mod \
  golang:1.26-trixie go test -count=1 ./...
```

End to end: an OrbStack Debian 13 machine (`orb create debian:trixie
devopsy-test`) set up with devopsy-server, a linux/arm64 build installed in
it, and targets like `@devopsy@devopsy-test@orb:prod` (OrbStack's SSH needs
no keys). Release the Traefik template there, then a template importing
from it, and check `target.env`, `--debug imports`, the Traefik `domains`
command, an instance, a pattern environment and `--destroy`, a rollback,
and a release with Traefik stopped. Delete the machine afterwards.

`--upgrade` and the release notice only work in release builds: `go build
-ldflags "-X main.version=0.8.0" -o /tmp/devopsy ./cmd/devopsy`, then
`/tmp/devopsy --upgrade`. The notice needs a terminal (`script -q out
./devopsy ps`).

## Releases and commits

Tag `vX.Y.Z` on `main` and push the tag: the release workflow tests, and
GoReleaser publishes the binaries and `checksums.txt`. Conventional commits
(`feat:`, `fix:`, `docs:`, `build:`...).
