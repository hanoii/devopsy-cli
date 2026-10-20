# AGENTS.md

devopsy-cli is `devopsy`, a Go CLI that runs Docker Compose projects with a
`.devopsy/` directory, locally and on servers over SSH. `README.md` is the
user-facing behavior: keep it in sync with every change. devopsy-server
sets up hosts for it, and the devopsy-template-* repositories are
projects built on it.

This file holds the rules, the decisions with their reasons, and the
findings a later session must not have to rediscover. How a script or a
function works, line by line, is in its comments: keep the detail there.

## Principles

The few rules that shape every design. Each says what, why, what it
costs (accepted: do not "fix" it) and where it lives. A change that
conflicts with one is a design question for the user, not a detail.

- **Compose plus environment variables.** devopsy never generates or
  rewrites compose files; everything it computes shows in `devopsy
  --env`. Why: a project stays a plain compose project anyone can read
  and run, and devopsy adds no format of its own to learn or debug.
  Cost: what compose cannot express through variables or labels is a
  project's command or capability, never devopsy's. If a feature needs
  to change compose files, it is the wrong feature.
- **Nothing is frozen before 1.0.0.** One person uses devopsy and runs
  every server, so anything may change when the change makes it better
  in the long run, the basics included (project discovery, `.env`
  loading with compose's parser and the caller's environment winning,
  `commands/` with `DEVOPSY_PROJECT_DIR` and `DEVOPSY_COMMAND_NAME`,
  the recursion guard, everything else passed to compose). Propose such
  changes rather than work around a design. No migration code: older
  files and syntax just stop working, with an error that says what to
  do; a breaking change is marked (`feat!`, `fix!`); a protocol bump
  (`remote.Protocol`) means upgrading the servers. A 1.0.0 is when
  this gets a compatibility promise.
- **devopsy replaces itself with what it runs** (`syscall.Exec`). Why:
  signals, the terminal and exit codes belong to that command, so
  `devopsy up` behaves as `docker compose up` does, Ctrl-C included.
  Cost: nothing runs after the command (no cleanup, no notice): what
  must happen after is a detached helper (`--upgrade-check`) or a
  server script.
- **Push over SSH, never pull.** A release is a tar stream over `ssh`
  from wherever devopsy runs (a laptop, CI); the server needs Docker,
  devopsy and a few standard tools (Releases), never access to the
  repository, and nothing listens. Why: no control plane, no daemon, no webhooks, no deploy
  keys; one command from anywhere. Cost: a release needs the operator
  until its switch (Sessions, drops and signals), and syncs across
  servers only work through the operator's forwarded agent.
- **Names come only from project, instance and environment:** compose
  name `<project>[-<instance>]-<environment>` (`Target.ComposeName`),
  directory `<project>/[<instance>/]<environment>` under the server's
  release root (`releases: root`), nothing else: not a `path:` key
  (there was one, it went), not compose's `name:` (overridden through
  `COMPOSE_PROJECT_NAME`, `devopsy-target.env`), not the checkout's
  folder (`project:` is required for anything remote), not the local
  shell's `COMPOSE_PROJECT_NAME` ("Nothing of the local environment
  goes to a server", Releases on disk). Why: the same target lands in
  the same place from any laptop or CI, two environments never share
  containers, volumes, networks or a URL, and everything per
  environment (syncs, images' bookmarks, `--destroy`, `--status`) finds
  its things by that name alone, with no registry of environments. The
  `-` join can collide (`a-b` + `c`, `a` + `b-c`): `--check-names`
  refuses then (Activation). Cost: renaming any of the three is a new,
  empty environment; moving the old one is by hand.
- **Deployment facts belong to targets, never to a project's config:**
  servers (`server:` is an error) and instance names come from the
  address, variables or aliases (Config and targets); a server's own
  settings (`releases:`) are its deploy user's (Release settings), and
  there is no server-wide variable layer (`/etc/devopsy/devopsy.env`
  went, and a file the proxy's release would write was rejected): what
  a server knows is its targets' (`--vars`) or an export of the project
  that owns it (Roles, exports and imports). Why: one checkout releases
  to any server, and nothing hidden on a host changes what a project
  sees. Cost: CI and laptops name the server (`DEVOPSY_SERVER`, the
  address).
- **What is deployed decides on the server.** A rollback's and a
  restart's steps come from the release's own config there (Steps), the
  rules for images run as the server's devopsy (`--images`, Images),
  and a protocol number makes an older server refuse before anything
  changes (`Protocol`). Why: a laptop's checkout may be another branch
  or another version; the server's state is the truth. Cost: every
  server's devopsy must keep up (`devopsy @<server> --upgrade`).
- **Before the switch anything cancels; after it nothing interrupts.**
  A drop, a Ctrl-C, a failure or a silence before `make_current`
  cancels the release and nothing live changes (`not_live`, the
  watcher, the heartbeat); from the switch on the release finishes, or
  fails and brings the previous one back, on its own (Sessions, drops
  and signals; From the switch on). A sync goes by what its copy wrote
  (Syncs). Why: a release is either the old one or the new one, never
  half, whatever happens to the operator. Cost: a stuck run step can
  only be stopped on the server.
- **Check everything before changing anything; leave every state
  visible and recoverable.** Preflights run before the upload, before
  a service stops, before the switch. What a cut-off session leaves is
  marked (pending, fetching, failed, the partial-sync mark), shown
  (`--releases`, `--status`) and settled by whatever takes the lock
  next; removals resume where they were cut. Why: the operator's
  connection, a laptop or CI can vanish at any point, and a server must
  never need someone to log in and guess. Cost: marker files and the
  code that reads them.
- **The server never waits on the operator for long.** A session's end,
  clean or silent, is noticed within seconds (`--watch-session`, the
  heartbeat: 30 s, 90 s for a sync's holder, 2 min for data), so a dead
  session never holds a lock or keeps a service down; lock waits are
  announced and bounded (`lockWait`). Cost: an operator frozen past the
  quiet time (a laptop asleep, Ctrl-Z) cancels, cleanly.
- **Words are never built-ins.** devopsy's own features are flags or
  `@target`; a word is a project command, else a compose command
  (`ComposeCommands`: compose's own completion, cached, refreshed when
  a word is missing), else an error. Why: a project command or a new
  compose command never clashes with devopsy, and `devopsy <anything
  compose takes>` keeps working. Cost: devopsy's features read as flags
  (`--release`, `--sync`), never as subcommands. Arguments starting
  with a flag go to compose unchecked (`--profile x up`), except a flag
  alone, which compose never has: an unknown lone flag is an error with
  the nearest flag as a hint (`cli.LoneUnknownFlag`, in `Build` and in
  `runRemote` before any connection); `--` is the escape hatch. The
  bare help is devopsy's own; compose's is `devopsy -- --help` and
  `devopsy <command> --help` (captured, `docker compose` read as
  `devopsy`).
- **Every change carries its test.** A fix comes with a test that
  fails without it (run it against the code before the fix, and say
  so); a feature with tests of what it promises; and a behavior a
  change touches that has no test gets one then. Unit tests first: Go
  tests, the scripts run against fake `docker` and `ssh` (`cmd/devopsy`,
  `internal/remote`); an end-to-end test (`e2e/`, Checks) only for what
  needs real sshd, Docker, the network, a terminal or timing. One test
  per behavior: extend the test that covers it before adding another,
  and never test the same thing at both levels. Why: the reviews of
  October 2026 found bugs in code that worked when written and broke
  under a later change nobody re-tested; a test is what keeps a fix
  fixed. Cost: fakes grow with the code (extend them, never weaken a
  test to pass), and the e2e suite takes minutes.
- **Projects own policy; devopsy owns mechanics.** devopsy knows no
  URL, proxy or application: projects compute hosts (the `env`
  capability), sanitize copies (`finish`), hold visitors during a
  deploy, and put project-specific behavior in `.devopsy/commands/`.
  Small per-project choices are compose labels (`devopsy.shell`,
  roles, `devopsy.sync`); capabilities are for behavior a label cannot
  express, and a new one only when a second use needs it. Why: devopsy
  stays small and every project can differ. Cost: templates carry the
  conventions (devopsy-template-*), and devopsy only reports facts to
  them (the `finish` word, exports). `--shell-host` is never
  overridable: the way in when a project is broken.


## Conventions

How the code is written; the principles above say what it is for. Each:
the rule, why, and where it lives.

- **stdout is the result, stderr is devopsy talking.** The result:
  compose's output, help, `--env`, `--vars get`, lists, `--context-hash`,
  `--export`, never through `report`; everything devopsy says (progress,
  notices, errors, prompts, the scripts' lines) on stderr. Why: pipes
  stay clean, and the two never interleave out of order. Scripts show
  only at `-vv`: a session's, and an argument of several lines in the
  "running" notice, which otherwise counts its lines (`Plan.running`).
- **Everything devopsy says goes through `internal/report`,** one line
  format: `devopsy: ` on every line (`report.Prefix`), then the level's
  tag (none for info, `warning: `, `error: `, `debug: ` at `-v`,
  `trace: ` at `-vv`), details and fixes on `devopsy: | ` lines, a
  command to copy after `$ ` (`report.Fix(text, cmds...)`): "to fix
  it:" and the fix, or "to fix it, do one of these:" and each numbered.
  Why: each line says by itself that it is devopsy's, among compose's,
  Docker's and the steps' output, so coloring, logs and grep never need
  the line before; the format changes in one place. `error` means the
  command fails (whatever goes on is a `warning`); the caller writes the
  words (lowercase after the prefix, names keep their case, no final
  period, one message and its details rather than sentences in a row),
  `report` everything else, never rewriting text. `report.Failure`
  carries details and fixes through wrapping (`report.Fail(err,
  more...)`); a command adds the fixes only it knows. Prompts too
  (`Confirm`, `Input`, `ConfirmName`, `Prompt`, bold cyan), only at a
  terminal, else the command fails with `--yes` as the fix. Scripts say
  things through the hidden `devopsy --say <level> <message>
  [<detail>...]` (Protocol 5), except `basePrelude`'s checks of the
  server's devopsy and the `--ssh-keys` scripts, which need nothing of
  it: a plain `echo` of a line built from `report` (`echoLine`).
  `--say` fails when its write does, as `echo` does: the release relies
  on that before its switch. `TestNothingSaysAround` fails on a
  `"devopsy: "` literal or a stderr `Fprint` anywhere else (help
  excepted). Rejected: `slog` (key=value attributes, not a message with
  details and fixes), indented continuation lines (ambiguous with
  compose's), times on lines (the logs have them; a total, when one is
  wanted, from the local devopsy's own clock, in the message).
- **Hints name the address as typed** (the alias, else the resolved
  address: `targetRef`, `hintAddress` in scripts). Why: the command
  works when copied, and an alias with `source:` releases only from its
  checkout.
- **Every flag is one entry in `cli.Flags`,** by task group, with its
  scope (`Local`, `[@t]`, `@t`, `@s`, `[@s]`) and description. Why: the
  help, completion (`LocalFlags`, `RemoteFlags`), `RemoteOnly` and
  `ServerScope` all come from it, so they never disagree; a flag that
  differs locally and on a server (`--upgrade`) has one entry per
  scope. Lines fit 80 columns (`TestUsage`).
- **A flag needs a server or an environment.** `cli.ServerScope`
  (`--shell-host`, `--ssh-keys`, `--ssh-config`, `--environments`,
  `--vars-project`, `--upgrade`) needs only a server
  (`remote.ServerTarget`: `@<server>:` alone, a bare name that is no
  environment or alias, or any target leniently); a server named alone
  has no `Name`. Every other flag needs an environment, and says so
  given `@<server>:`. Why: server tasks work before any project is
  released there.
- **Project commands say where they run** (`## Run: local|target`, `##
  Needs: target|server`, `cli.CommandRun`), checked in `Build`, in
  `runRemote` before connecting (an older server's devopsy would run a
  local one) and on release steps (`checkStepScopes`); help and
  completion show it. Where a command runs is decided by the directory
  (`cli.InRelease`: `devopsy-target.env` beside its `code/`), never by
  a variable. Why: a shell on the server or a plain `ssh` has none, and
  a variable would reach every child. A `Needs:` command runs locally
  (`Run: local`), in its project (`runAddressed`), with the address as
  typed in `DEVOPSY_COMMAND_TARGET` for that command only (passed as
  `DEVOPSY_INTERNAL_COMMAND_TARGET`), so an outer command's address
  never reaches an inner one; never the target's variables: the command
  calls devopsy with the address.
- **Secrets are opt in:** `secrets:` in `config.yaml` (names, `*`
  patterns) is the only rule, whatever set the value. Why: guessing
  from names (a regex did, and went) masked the wrong things and missed
  others; no "everything in `.env`", no mask-everything fallback.
  `cli.Secrets`: `Value` for listings (`--env`, `--vars`, `--debug
  env`: `***`, `--show` unmasks), `Mask` for messages (values of 8
  characters or more). On a server the release's config decides;
  `--vars` masks locally from the configs `VarsReadScript` sends plus
  the checkout's. Secret-like names (`secretLike`) only ask: before
  `--release` (`confirmUnnamedSecrets`, locally, no session: names only
  in the server's files are left to `--vars set`) and on `--vars set`;
  a prompt at a terminal, else a failure, `--yes` included. `public:`
  lets a name through.
- **`@<target> <subcommand> --help` only prints help**
  (`cli.RemoteCommandHelp`), resolving the target leniently
  (`DescribeTarget`). Why: help never needs a server.
- **Few dependencies, standard library first. Linux and macOS only.**
- **Color only on a terminal** without `NO_COLOR` (`styleFor`), or
  forced (`cli.ColorForced`: `FORCE_COLOR` not `0` or `false`, or the
  internal `cli.ColorEnv`). devopsy never exports `FORCE_COLOR`. Why:
  in a session it would reach every tool there, which would then color
  what scripts capture. Colors go by what a line starts with, never its
  words (`report.Paint`): devopsy's errors red, warnings yellow, info
  cyan, debug, trace and the `| ` bar dim, a `$ ` command bold;
  Compose's done steps green, steps on the way dim, another tool's
  errors red. A session's lines are colored locally as they arrive
  (`report.Colorize`, line-buffered, terminal writers only), lines
  colored already left as they are; `remote.Color` exports
  `DEVOPSY_INTERNAL_COLOR`, so devopsy on the server colors its own the
  same way. Release logs get the raw lines. Help
  is colored by layout (headings end with `:`, entries start with two
  to seven spaces, `@t` magenta, `@s` yellow).
- **A pseudo-terminal on the server, never a `-t` session** for
  activations and holders: `remote.TTY` (`DEVOPSY_INTERNAL_TTY='<cols>
  <rows>'`, exported when stderr is a terminal; 80x24 when its size is
  unknown) makes steps, the fetch and the run step run under `script`
  (`with_tty` in `basePrelude`; plain when `script` is missing or stdout
  is a tty already). Why: BuildKit draws its live progress and colors
  through the pipe, while the session stays one path for a terminal and
  CI (Sessions, drops and signals).

## Layout

The section that covers each part is in parentheses.

- `cmd/devopsy/`: `main`, which executes what `internal/cli` plans, and
  the operator's side of remote commands (`remote.go`, Releases; a
  local release in `release_local.go`), syncs and the servers'
  `--sync-*` flags (`sync.go`, Syncs), `--export` and `--import`
  (`data.go`), the watcher and the heartbeat (`watch.go`,
  `heartbeat.go`, Sessions, drops and signals), release logs
  (`releaselog.go`), `--releases` and `--images`; tests run the built
  binary against a fake `docker` and `ssh`.
- `internal/cli/`: finds the project, loads `.env` and decides what to
  run (`Build` returns a `Plan`; it only runs helpers, like the `env`
  capability, never the command); `cli.Flags`; roles (`roles.go`);
  capabilities; the `devopsy.sync` labels (`sync.go`).
- `internal/remote/`: config and targets (`config.go`), the schemas
  (`schema.go`), packing a release, and the POSIX `sh` scripts run on
  servers (`remote.go`, a sync's in `sync.go`; checked with `sh -n` in
  tests; GNU coreutils, `mv -T`).
- `internal/images/` (Images, behind a `Docker` interface),
  `internal/report/` (Conventions: the format, `Paint`, `Colorize`,
  prompts), `internal/complete/` (completion
  and the embedded bash, zsh and fish scripts), `internal/probe/`,
  `internal/update/`.
- `e2e/`: end-to-end tests against servers in Docker (Checks).
- `install.sh` (POSIX `sh`, dash and busybox ash), `scripts/devopsy`
  (runs the checkout's code), `.goreleaser.yaml`, `.github/workflows/`.

## Config and targets

Where a target's names, server and settings come from. Governed by
"Names come only from project, instance and environment" and
"Deployment facts belong to targets".

- **Three files, and unknown keys are errors:** the project's
  `.devopsy/config.yaml` (`project:`, `instances:`, `secrets:`,
  `public:`, `releases: {keep}`, `defaults:`, `environments:`), its
  `config.local.yaml` over it, and the user's
  `~/.config/devopsy/config.yaml` (`aliases:`; on servers `releases:`).
  Never `~/.devopsy`: discovery would take the home directory for a
  project. Where: `config.go`.
- **`defaults:` merges, `~` removes:** it gives environments `mode`,
  steps, `releases`, `parent`, `inherit` and `env`; `env` merges key by
  key, `KEY: ~` removes, `""` stays empty, and `parent: ~` and
  `inherit: ~` remove theirs. Why YAML nodes are parsed: a plain decode
  cannot tell `~` from absent (`nullKeys`).
- **A target is an address,** `@[<server>:][<instance>/]<environment>`
  (`parseAddress`): the server ends at the first `:`, the instance
  before the last `/`, empty meaning none. Missing parts come from
  `DEVOPSY_SERVER_<ENVIRONMENT>`, the pattern's (`PatternVar`: `pr-*`
  gives `DEVOPSY_SERVER_PR`), `DEVOPSY_SERVER`, and `DEVOPSY_INSTANCE`:
  the caller's environment, then the project's `.env`, never for
  aliases. No server is an error. Explicit none (`@vm1:/prod`) only
  parses: it is the default.
- **Instances are all or nothing:** `instances: true` makes every
  target name one (an alias's from its `to:` only), otherwise none may.
  Why: a project's environments never mix `<project>/<env>` and
  `<project>/<instance>/<env>` on a server, so `--environments` lists
  one layout.
- **Patterns:** environment keys with `*` (one or more name
  characters); the exact name wins, then the most literal characters; a
  tie is an error.
- **Aliases** (`{to, source}` or `{to, project}`) match only a bare
  `@name`, and a project's exact environment wins. With `source:` its
  config gives the project, environments and steps, and releases run
  only from there (`ReleasesHere`, symlinks resolved). Why: anywhere
  else a release would upload whatever project devopsy runs in.
- **Release settings belong to the server:** its deploy user's
  `releases:` (`root`, default home; `keep`, `max_keep`, default 5),
  read by the hidden `--release-settings` that every script asks first
  (`basePrelude`). Why: a laptop and CI resolve the same paths. A
  project's `keep` is capped by `max_keep`; `root` and `max_keep` in a
  project are errors.
- **The schemas are documentation that parses:** `--debug schema
  [--user]` prints `ProjectSchema` / `UserSchema`, checked by
  `TestSchemas`: a new key goes there too. `--init` writes a new config
  and never touches an existing one.

## Releases

A release uploads the project as a new directory, then one activation
session switches `current` to it; a rollback switches back to an older
one. Governed by "Push over SSH", "What is deployed decides on the
server", "Before the switch anything cancels" and "Check everything
before changing anything".

- **The server's devopsy must speak the scripts' protocol**
  (`remote.Protocol`, 6, reported by `--release-settings`):
  `basePrelude` refuses an older one before anything changes, for every
  command, read-only ones too, and names the upgrade. Raise it when a
  script starts calling something an older devopsy lacks, and say what
  in its comment. `DEVOPSY_PROTOCOL_CHECK=warn` (`ProtocolCheckEnv`,
  passed to every session) makes it a warning and goes on: the
  operator's own risk, for a local build ahead of the servers.
- **What the server needs:** `devopsy`, `tar`, `flock`, `setsid`,
  `script` (util-linux), GNU `tail` and `mkfifo`; locally only `ssh`.
  Without `script` there is no live build progress; without `setsid`
  the run step runs plainly; without GNU `tail` only the traps protect
  a release after its switch (From the switch on).

### Steps

- **Phases, not a free list:** `--release` and `--rollback` take no
  command; the environment's `release:`/`rollback:` are required
  (`remote.Steps`: before, prepare, run, after; a plain string is a run
  step alone). Why: the upload and the switch are never hidden in a
  step. The remote steps share the one session that holds the release
  lock, so before is local steps then remote ones, after is remote then
  local, checked when the config is read.
- **Local steps never look like a release on a server** (decided): they
  get the target's env, a child's `DEVOPSY_PARENT` and
  `DEVOPSY_PARENT_TARGET` (`parentEnv`) and the commit, never the
  server's `.env`, and never `DEVOPSY_TARGET`, `DEVOPSY_ENVIRONMENT` or
  `DEVOPSY_INSTANCE`. Why: set, those mean a release on a server, which
  the templates' `env` capabilities test, and `DEVOPSY_INSTANCE` would
  reach the step's own devopsy calls. A step that needs the address
  declares `## Needs: target` (`DEVOPSY_COMMAND_TARGET`).
- **Remote commands run through the `current` symlink, never a release
  path.** Why: compose stores bind-mount paths in containers, and
  pruned releases would break them. The exception is `prepare`, in the
  new release before it is current: it must not start services.
- **Steps of what is deployed come from the server** (principle "What
  is deployed decides on the server"): a rollback's (hidden
  `--release-steps rollback <phase> [<release>]`, `runReleaseSteps`),
  the restart after a failed run, the run step a `--sync` reruns
  (`--release-steps release run`) and `--destroy`'s step
  (`destroyStep`) are read from the release's own `config.yaml`, its environment from
  `devopsy-target.env` (`ReleaseEnvironment`). A release's steps come
  from the checkout, which is what is uploaded; a rollback's local
  before and after steps stay the checkout's.
- **`mode:` defaults to `build`.** Why: it works for every project,
  where a wrong `image` fails the release. Either way devopsy fetches
  the images itself, before the switch (Images), so the run step only
  starts them: a build or pull there would run after the switch, and in
  a sync after the copy.
- **`--release` without a target is a release in place**
  (`runLocalRelease`): `defaults:`' release steps (no environment is
  local), prepare, `--missing-vars`, the images always as build mode,
  run; no before or after steps, no switch. `--destroy` likewise
  (`runLocalDestroy`): after the project's name, `runDestroyData` in
  place, `defaults:`' destroy step if any; never the checkout. The
  other release flags stay target only.

### Upload

- **Everything that can be checked locally or in a short session is
  checked before uploading:** secret-like names
  (`confirmUnnamedSecrets`), a role another compose project holds
  (`RoleHoldersScript`), a new instance, which asks
  (`InstanceExistsScript`, `--yes`), and a child's sync (Syncs).
- **A release is a new directory, never a changed one:** `UploadScript`
  takes the release lock, says so (`LockedMarker`: devopsy sends the
  tar only then, Locks), extracts into `releases/<id>.tmp` through
  `--stdin-guard`, writes `PendingFile` and renames it into place. An
  existing id is refused: `mv` would put the new tree inside the old
  release, which would run again. Ids are UTC seconds (`NewRecord`), so
  tests that release a target twice wait for a new second
  (`spaceReleases`).
- **Full tar streams, no rsync:** macOS ships openrsync. The progress
  line (`uploadProgress`, at a terminal only, never logged) counts the
  project's bytes as `Pack` reads them, out of `remote.Size`: the
  gzip's size is unknown until the end.

### Activation

`ActivateScript`, one session, in order (the sections named hold each
step's rules):

1. The release lock (Locks), the watcher (Sessions, drops and signals),
   `img recover` of a release left fetching.
2. A rollback's target (`img rollback-target`: the newest live release
   older than current); `partialGuard` refuses a rollback, and a
   release that does not sync, on a partial copy (Syncs).
3. The pending lock (fd 7), then remote before steps in `current`.
4. Links into the release: `shared/*`, `inherit.env`, `project.env` and
   `instance.env`, even when missing, so one created later applies.
5. Releases only: `--prepare-release` when the compose files use role
   or import labels (`PrepareScript`, Roles, exports and imports);
   `--check-names`: containers of the same compose project from another
   directory (`cli.ProjectConflict`: `-`-joined names coincide,
   `a-b`/`c` and `a`/`b-c`), and a network the project names as its
   own that another compose project owns (`cli.NetworkConflicts`:
   Compose would silently share it; external networks are how a project
   joins another's).
6. `prepare` steps, then `--missing-vars` (`cli.MissingVars`: compose's
   `${VAR:?}`/`${VAR?}` left unset).
7. The images (`fetch_release`, Images). `ServerLogFile` is created
   here, so a full disk fails the release before it goes live.
8. `PendingFile` removed, `make_current`, the sync of a release that
   syncs, `followOutput` (From the switch on), the run step.
9. A failed run or sync: `FailedFile`, `current` back, the previous
   release restarted with its own images (`get_images`) and run step
   (`--release-steps restart run`: its `rollback:`, else `release:`),
   except on a partial copy. A rollback whose run step fails marks the
   release it restored failed (decided: it did not come back up, so it
   is no rollback target until released again).
10. A rollback marks the release it left (`RolledBackFile`).
11. Pruning to `keep` (`img to-prune`, `img remove-releases`), then
    remote after steps (a failure is reported, the release stays
    live).

- **A failure before the switch changes nothing live** (`not_live`,
  `NotLive`, `RollbackNotLive`): the release is marked failed, and no
  directory is removed. Why: removing a failed first release's
  directory once took a second release uploaded behind it.
- **No prompts in an activation:** what is missing is listed with its
  `--vars set` line.

### Releases on disk

- **devopsy's files sit beside the tree, never in it:**
  `releases/<id>/code/` (`remote.CodeDir`) is the project as uploaded,
  `current` points there, and the release's files (`RecordFile`,
  `TargetEnvFile`, `LogFile`, `PendingFile`, `FailedFile`...) sit beside
  it. Why: inside the tree they were in every build context, and `COPY
  . .` missed the cache every release. Scripts keep `$rel` the
  release's directory, `$code` its tree; Go finds them with
  `remote.ReleaseFile` (symlinks resolved).
- **Live means none of pending (upload to switch), fetching, failed or
  rolled back:** what `keep` counts and a rollback goes to. Failed and
  rolled-back releases go once older than the oldest kept. One that
  fails before going live stays, a first one included, until
  `--destroy` or `--releases remove failed`.
- **A pending release shows IN PROGRESS only while its activation runs**
  (it holds fd 7 on `PendingFile` from the release lock to the switch;
  `--releases` checks with `flock -n -s`, never waiting), else
  INCOMPLETE, which `remove failed` takes only when older than current.
  Why: a newer one may be between its upload and its activation.
- **`devopsy-target.env` is written into each release** (the
  environment's `env`, `COMPOSE_PROJECT_NAME`, overriding compose's
  `name:`, `DEVOPSY_PROJECT`, `DEVOPSY_INSTANCE`,
  `DEVOPSY_ENVIRONMENT`, `DEVOPSY_TARGET`, `DEVOPSY_RELEASE_COMMIT`, a
  child's `DEVOPSY_PARENT` and `DEVOPSY_PARENT_TARGET`). Why: it
  applies however devopsy runs on the server, and rollbacks restore it.
  Precedence there: caller, `shared/.env`, `inherit.env`,
  `instance.env`, `project.env`, `devopsy-target.env`.
- **Nothing of the local environment goes to a server:** a target's
  `COMPOSE_PROJECT_NAME` is always its compose name (`projectName` in
  `runRemote`). Why: one exported for local work put every environment
  under one compose project. What does go: values `--vars set KEY`
  reads on purpose, display settings (`DEVOPSY_INTERNAL_*`,
  `DEVOPSY_VERBOSE`), who released (`USER`, CI's actor) in the record.
- **Values reach the server's files over SSH's stdin, never as
  arguments:** `--vars`, `--vars-instance` and `--vars-project` (one
  file each, so flags, not options of `--vars`; `internal/remote/vars.go`)
  edit keys in place under the release lock, since deploy commands
  write secrets too.

### Locks

- **The release lock** (`$base/.lock`, fd 9) is taken by every script
  that changes an environment, the upload too (`releaseLock`,
  `lockWait`: `-n`, then a "waiting for" line and `-w 600`, so a wait
  is never silent). Why the upload: `--destroy` never empties `$base`
  under it, and a second release's upload waits for the first's
  activation. A fetch's children inherit the lock, so a recovery never
  races a fetch still running.
- **What a session sends goes once its script holds the lock**
  (`remote.WithGate`, `lockedGate`): the upload, `--destroy` and
  `--releases remove` print `LockedMarker` on stderr once they hold it
  (the removals once they hold the image lock too), and devopsy sends
  stdin only then (the tar, or the `go` a removal reads through
  `--stdin-guard` before it starts). Why: a session cut
  while its script waited (Ctrl-C, a drop) left its tar in the server's
  pipe, and the script extracted it once the lock came, after devopsy
  had said nothing changed; a removal went on likewise. Now it finds an
  empty or silent stdin and stops. Activations need no gate: their
  watcher, started past the lock, reads a closed stdin at once. A
  sync's holder starts its watcher before the lock, armed only while it
  waits (nothing changed yet), and disarms it once it holds the lock: a
  session gone meanwhile ends it. `--vars` edits and the inherit write
  are gated too; their data may be empty, so it ends with `InputEnd`
  (`GatedInput`), which `gated_read` requires and takes off.
- **Only the upload creates `$base`** among the release scripts
  (outside them `--vars` edits and the inherit write do too). Why: an
  activation whose release went while it waited fails instead of making
  it again.
- **A lock file removed while awaited is opened again** (`[ /dev/fd/9
  -ef "$base/.lock" ]`, Linux only), or two releases would hold locks
  on different files; likewise `removeBase` moves `$base` aside before
  `rm -rf`, so a session arriving meanwhile makes a new one.
- **The image lock** (`ImagesLock` in the release root, fd 8, always
  taken after fd 9): images are the whole server's. Shared for fetches,
  exclusive for removals, which also need Docker to answer, else they
  count as busy (`il_busy` says which). Holders write
  `ImagesHolders/<pid>`, for messages only. Linux's `flock` lets new
  shared holders past a queued exclusive one.
- **Who waits for the image lock:** pruning skips when it is busy,
  directories included, and the next release prunes (accepted: servers
  are not expected to be that busy); `--destroy` and `--releases
  remove` wait 10 minutes, then fail with 75; the restart after a
  failed run waits too, since on the new release's images it would run
  them under old code.
- **Accepted costs of the image lock:** `--destroy` holds it across its
  `destroy:` step, since compose's `--rmi local` removes images too, so
  a slow step makes fetches on the server wait. Docker calls have no
  timeouts: a hung daemon holds the lock until killed, and waiters give
  up after 10 minutes.
- **No update check under a lock:** `basePrelude` exports
  `DEVOPSY_NO_UPDATE_CHECK=1`. Why: the detached `--upgrade-check`
  would inherit fds 8 and 9 (compose has to keep them) and hold the
  locks while it asks GitHub.

### Images

Every image a release runs stays until its last release goes, so a
rollback runs exactly what it ran. The rules live in Go
(`internal/images`, whose comments say how each action decides), run
on the server as the hidden `--images` (`runImages`; `img` in the
scripts), under "What is deployed decides on the server"; they were
generated sh at first, where four review rounds found most of the
bugs. The scripts keep what needs the shell (`releaseImages`): the
locks, and fetching on a terminal (`fetch_images`, `with_tty`).

- **Fetching happens before the switch** (`fetchImages`, through
  `devopsy --`, past project commands): build mode `pull
  --ignore-buildable` then `build --pull`; image mode `pull`, which
  pulls buildable services with an `image:` too and fails on a missing
  tag. A rollback or a restart restores its bookmarks (`img restore`)
  and fetches only when it cannot.
- **Bookmarks are the reference count:** each release tags every image
  it runs as `<repo>:devopsy-<compose name>-<id>` (`bookmarkPrefix`: a
  long compose name is cut and hashed so the tag fits Docker's 128; a
  repository named twice adds a checksum of the ref), by image id, since
  another fetch may move the name meanwhile. Digests are bookmarked too
  (`prune -f` would delete an unused image pulled by digest only), never
  restored. `devopsy-images` lists `ref<tab>image id`: what a removal
  decides from even after a bookmark went.
- **Compare names as Docker reports them:** Docker shows names short
  (`docker.io/library/nginx` is `nginx:latest`), so every comparison
  with a name Docker reports goes through `familiar`.
- **A release fetched again never removes names:** it untags the
  bookmarks its new listing no longer makes and records the names it
  dropped for the next pruning. Why: it runs under the shared lock,
  beside other fetches.
- **A restore never changes another environment:** a name whose current
  image another compose project's container uses (`docker ps --filter
  ancestor=`, checked on Docker 29's containerd store), or that a
  bookmark none of this environment's releases made holds (exact names:
  `app-prod` never matches `app-prod-eu`), stays on the newer image,
  said. A digest's bookmark must be there too, or the run step's `up`
  would pull after the switch.
- **A removal frees only what nothing else holds:** it untags the going
  release's bookmarks, then per image, from Docker's state: held by
  another bookmark, it stays (its names in repositories no holder is
  in, a mirror or a digest, are recorded, or they would outlive every
  release); used by a container, its names are recorded, never stripped
  (all but the last frees nothing and costs a pull); else its listed
  names and its other names in the same repositories go (`mariadb:11`
  and `mariadb:11.4`); a name that moved on stays.
- **Never `docker rmi -f`:** a refusal is recorded. Why: Docker 29's
  containerd store only untags with it, but the classic store, which
  servers upgraded from older Docker keep, may delete an image a stopped
  container uses (not checked).
- **Removals are idempotent and keep the release on a Docker failure:**
  one cut off anywhere is finished by the next. Docker failing (anything
  but a missing image) stops it with exit 1 and the release stays, so
  the next removal finds its listing again (`RemoveReleases` removes a
  directory only after its images); `--destroy` then keeps the
  directory (the data is already gone) and says to run it again.
- **No directory scanning, server-wide sweep or trash directory**
  (decided): a removal decides from its release's listing and Docker's
  state, and reads nothing above its project.
- **The record holds what could not go** (`images.LeftFile`, `<release
  root>/<project>/.devopsy-images-left`): `--releases` lists it, and
  `img prune` retries this project's lines only (decided), dropping one
  once its name is gone, moved on or removed. Appended in one
  `O_APPEND` write, since two environments of a project record under
  the shared lock; only the retry, alone under the lock, rewrites it.
  `LeftoverScript` treats it like the project's `.env`, and `--purge`
  says what it forgets.
- **Undo** (`img undo`): a new release's fetch is bookmarked, so it goes
  with the release; a rollback's target is left alone, since the names
  may hold the live release's images; then current's names come back.
- **Which releases go** (`images/releases.go`): `ToPrune` (older than
  the `keep`-th live one; current and fetching ones stay), the rollback
  target and removals by id, unit-tested on directories.
- **`runImages` ignores SIGPIPE, SIGTERM, SIGINT and SIGHUP:** Go does
  not keep an inherited ignored SIGPIPE or SIGTERM, so the script's
  `trap ''` alone would let a gone session or a second TERM kill an
  undo halfway. Its steps are short and under the lock. `leftAdd`
  records before it says.
- **Tests:** an in-memory Docker (`images_test.go`) and
  `TestRandomHistory`, 200 random histories of two environments sharing
  `mariadb:11` and an alias, checking the rules above and that nothing
  is left once all goes. Script tests call the real `--images`
  (`realDevopsy`, built by `TestMain`) behind a fake devopsy and a
  file-per-tag docker.

### Sessions, drops and signals

How a release notices its operator gone, and what a signal may stop.
Governed by "Before the switch anything cancels; after it nothing
interrupts" and "The server never waits on the operator for long".

- **Activations are always `-T`,** at a terminal too, with a pipe
  devopsy keeps open as stdin (`WatchEnv`), and a pseudo-terminal from
  `with_tty` (Conventions). Decided, one path for terminals and CI: a
  `-t` session's hangup reached only the shell, and dash runs a trap
  only once the command it waits for ends (seen: a build ran on, then
  compose hung on the gone terminal).
- **A release is pending from its upload until right before
  `make_current`:** one cut off anywhere before is never rolled back to
  nor kept. `prelude` removes stale `releases/*.tmp` and
  `.devopsy-watch.*` under the lock.
- **The watcher turns a session's end into a cancel at once**
  (`runWatchSession`, hidden `--watch-session`; the script side is
  `watchSession` in `internal/remote/remote.go`): it reads the session's
  stdin in the background, and when stdin ends takes the armed file
  (renamed to `.fired`) and sends SIGTERM to the script's group (or the
  script alone when neither it nor its login shell leads one), so a
  fetch is undone through `fetch_sig` at once, not at its next write, a
  build later. Without a watcher (OrbStack's own SSH) the fetch runs to
  its end. Armed: releases and rollbacks (`WatchEnv`), a release that
  syncs always (its stdin is the answer's pipe); a sync's holder only
  while it waits for the release lock, since past it a SIGTERM would
  stop it halfway through restoring.
- **One rename decides between the watcher and the switch:** the script
  disarms the watcher by taking the file first (`mv` to `.off`) in
  `fetch_release`, under the image lock, before its last `fetch_sig`
  check, so a SIGTERM never lands past a switch.
- **When the watcher won, the undo writes to a file,** `ServerLogFile`
  (a `cancel.<pid>` part, which `--log` shows at the end): nobody reads
  the session, and a write to one whose buffers are full would block
  with the lock held. A file, not a signal: one sent before the watcher
  could handle it would kill it.
- **The heartbeat catches a connection that dies silently**
  (`internal/remote/heartbeat.go`): a laptop asleep or a network gone
  is seen by the server only when TCP gives up (measured: 16 minutes
  with writes pending, hours for a quiet holder; devopsy-server never
  sets sshd's `ClientAliveInterval`, and the client's `ServerAlive*`
  only help the client). So every pipe devopsy keeps as a session's
  stdin (an activation, a holder, a push, the push's import,
  `--export`) gets a zero byte every 5 seconds, and the server's
  readers, told by `HeartbeatEnv`, take a silence for EOF. A busy
  operator (a laptop asleep, Ctrl-Z) cancels too, cleanly (accepted).
- **Quiet times are ordered:** `QuietBeats` (30 seconds),
  `HolderQuietBeats` (90, behind an answer's fifo: a holder or a
  release that syncs), so a push still copying is cancelled before its
  holder decides, the operator's ssh noticing in between. Why: one
  timeout for both let a holder restore while the push wrote into it.
  They count in beats, so the e2e tests shorten them all.
- **Data carries no beat:** the upload's tar reads through
  `--stdin-guard`, a server's `--import` its stdin, both bounded by
  `DataQuietBeats` (2 minutes, with or without a heartbeat: an export
  piped in may stop services first), and a remote `--import` opens its
  session with the data's first byte, so a slow writer never trips it.
- **Sync hooks never get `SyncWatchEnv` or `HeartbeatEnv`:** a hook
  running `devopsy --export` would take its own `/dev/null` stdin for a
  session gone.
- **SSH options:** `KeepAlive` (`ServerAliveInterval` 3 beats, 15
  seconds, `ServerAliveCountMax=3`) on activations, uploads and a
  sync's sessions; `Bounded` (`KeepAlive` plus `ConnectTimeout=30`) on
  a log's save, `--export` and `--import`, which otherwise waited
  minutes on a dead connection.
- **devopsy takes Ctrl-C itself** (`session` in `runRemote`; ssh ignores
  it). The first closes the pipe: before the switch the watcher cancels
  the release and devopsy waits (saying so after 5 seconds: the server
  takes the cancel once it holds the release lock); after it, devopsy
  says the release goes on, and a second ends the session; the log is
  saved either way. "After the switch" is `SSHStreams`' flag on
  `SwitchMarker` (`WithSwitched`), never the release log, which stops
  at its cap; a switch already under way when the pipe closed goes on,
  said. During the upload (`signal.NotifyContext`) the progress line
  goes and nothing changed.
- **An interrupted fetch is undone under the lock, once:** a signal
  only sets `fetch_sig` (INT, TERM, HUP); the fetch's child gets it too
  and fails, and the same path undoes it under the image lock, with
  signals ignored from there on, so a second Ctrl-C cannot stop the
  undo; exit 130. PIPE is ignored from the fetch on: dash writes
  "Terminated" to the dead session.
- **A shell killed outright is recovered by the next lock-taker:**
  `FetchingFile` marks a new release's fetch until a line says its
  images are ready, and `img recover` (by every activation, `--destroy`
  and `--releases remove`, after the lock) marks such a release failed,
  bookmarks its images and restores current's names; until then it
  counts as failed. Rollbacks never write the marker, which would mark
  a good release failed.
- **From the end of a fetch that went through, INT, TERM and HUP are
  ignored** to the script's end. Why: a trapped signal still kills the
  foreground child (the `ln` and `mv` of `make_current`), which `set
  -e` turns into the script's end halfway through the switch. So
  nothing stops a release after its switch, the run step included
  (decided: stopping it restarted the previous release while
  `script`'s compose ran on, two `up`s on one project).
- **`script` gets its own session past a fetch** (`setsid -w`,
  `tty_detach` in `with_tty`): it reads its signals through a signalfd,
  ignored ones too, and exits 0 when signaled (a restart's `get_images`
  took that for a fetch).
- **255 is ssh's:** ssh exits 255 for a connection that failed or was
  lost, and passes a script's own status on, so activations and holders
  never exit 255 (`NoExit255`, `followOutput`'s trap: 254 instead;
  drush on a PHP error exits 255). The operator's lost-connection
  messages (`sshLost`) then mean what they say.

### From the switch on

A release goes live, or fails and brings the previous one back,
whatever happens to its session (principle "Before the switch anything
cancels; after it nothing interrupts").

- **After the switch the output goes to a file, and a tail copies it to
  the session:** right after `make_current` (after the sync, in a
  release that syncs), `followOutput` sends the script's output to
  `ServerLogFile` (the deploy user's alone, umask 077; both streams,
  each activation's part after a `LogSection` line with its time and a
  tag), and a GNU `tail -f` copies it to the session. Why: a session
  gone kills only the tail, never the shell, compose mid-`up` or the
  switch back.
- **`SwitchMarker` cuts the session's log:** printed just before, it
  tells the operator's devopsy to stop logging the session and log
  `LogAnchor` there (`LineMarker` with `Cut`, on logged sessions only:
  others pass through untouched, a shell's typing and an export's
  data).
- **Activations and holders print on one stream** (`exec 1>&2`). Why:
  on two pipes a line on stdout could reach the log after the marker
  and be cut.
- **The tail ends by traps, never `tail --pid`:** half a second after
  the shell (an EXIT trap), or a second after a helper finds the shell
  gone without its trap. Why: `tail --pid` takes an unreaped zombie for
  alive.
- **A release that syncs, and a sync's holder, follow from the run
  step:** the sync has its own way through a drop (Syncs), and the
  copy's lines reach the session's log through the push.
- Without GNU's `tail` (macOS, in tests) only the traps apply
  (`TestAfterSwitchSurvives` skips).

### Logs

Two per release, never overlapping, neither rewritten, pruned with it.

- **The session's log** (`LogFile`, `devopsy-log-session`,
  `releaseLog`, `cmd/devopsy/releaselog.go`): devopsy's messages, local
  steps' command lines and exit codes, and the sessions' output up to
  `SwitchMarker`, each line after its UTC time, cleaned (`cleanLines`:
  no terminal codes or empty lines, and only the last frame of a
  progress display: a `cursorUp` code supersedes the lines before it),
  saved by `LogSaveScript` once the server was reached. A rollback's
  goes to the restored release's log (its "rolling back to <id>" line),
  a sync's to the current one's ("syncing into").
- **Local steps' output is not piped** into it: it would lose its
  terminal.
- **The server's log** (`ServerLogFile`), raw, from each switch on.
- **`--log` joins them by anchors, never by time** (`LogReadScript`,
  `joinLogs`): each server part goes after the session's anchor with
  its tag, cleaned the same way and indented. Why not time: two
  machines' clocks, and lines of the same second. Parts no anchor
  claims (an operator gone before saving) follow at the end under their
  `LogSection` time, never under a later anchor.

### Data, destroying and removing

- **`.devopsy/mnt/` is devopsy's one data directory** (README, "Data
  and mounts"): never uploaded, `shared/mnt` created by the upload and
  linked like every `shared/` entry. Its contents and their ownership
  are the project's: only `--destroy`, an `--import` (its entries) and
  a sync into a child remove anything in it.
- **`--destroy` removes nothing unless everything before succeeded**
  (`DestroyScript`, both locks; asks for the target's name unless
  `--yes`): the hidden `--destroy-data` (`runDestroyData`) in
  `current`, else the newest release: the environment's `destroy:` step
  if any (a hook, while everything is up), then `compose --profile '*'
  down --volumes --remove-orphans --rmi local` and `mnt` emptied as
  root in `UtilImage` (a plain `docker run`: `compose run` would bring
  the network back); then every release's images; then `removeBase`.
- **Leftover levels are asked about, never removed silently:**
  `purgeLeftovers` finds levels' directories left with only their
  `.env` (`LeftoverScript`) and removes them with `--purge` or a yes at
  a terminal, else a `report.Warn` with the command.
- **`--releases remove` refuses all words when one is wrong**
  (`runReleases`, `cmd/devopsy/releases.go`): it lists first, resolves
  every word (`chooseReleases`: exact id, else a unique prefix;
  `failed` is every failed or rolled-back release, and an INCOMPLETE
  one older than current) and refuses when one names nothing, several
  or the current release, then asks, `--yes` without a terminal. The
  server (`ReleasesRemoveScript`) checks again, with `--gone-ok` for
  `failed` (a release pruning took meanwhile), and runs `img prune`
  once everything went. Completion never offers ids: they are the
  server's.
- **A directory with `.devopsy/` and no `current` is a plain
  directory:** commands run there, releases refuse before creating
  anything. A `current` that is a directory (a failed release's
  containers restarting, Docker recreating their bind mounts as root)
  is refused by `prelude`, which says how to clean it up.

## Syncs

A child environment (`parent:`) gets its parent's data through two
sessions from the operator, a holder on the child and a push on the
parent; labels on each side's services say what moves. Governed by
"Before the switch anything cancels" (a sync goes by what its copy
wrote), "Check everything before changing anything" and "Projects own
policy". Code: `cmd/devopsy/sync.go`, `internal/remote/sync.go`,
`internal/cli/sync.go`.

### Parents and when

- **`parent:` names a concrete environment:** a pattern is an error;
  one naming itself has none, so `defaults: {parent: prod}` works; a
  cycle is an error, since neither could be released first (a first
  release syncs from a released parent).
- **A parent resolves one level only, its server never from the
  child's** (`ParentTarget`, whose own stays nil): at the child's
  instance, its server from the usual variables only. Why: a guess
  would copy the wrong data.
- **An unresolvable parent fails only what syncs:** strict loading
  records it in `ParentErr` instead of failing, so every other command
  works on the child; only `--sync` and a `--release` that syncs fail
  with it (`failParent` lists the fixes: the variable, the same release
  with `--from`, and a release without the data then `--sync --from`).
  `DescribeTarget` resolves it leniently.
- **When:** a child's first release (`CurrentExistsScript` prints
  `current` or `none`, then `incomplete` on a partial copy: one extra
  session on every release of a child), `--release-sync` (any
  release), never `--release-no-sync`, and `@<child> --sync`, which
  asks for the name like `--destroy` and reruns the deployed release's
  `run` (`--release-steps release run`, never the checkout's).
- **`--from <address>`** (`@` optional) on `--release`, `--release-sync`
  and `--sync` copies from that address instead of `ParentTarget`,
  `ParentErr` ignored: a child's only, and on `--release` only a first
  one (a released child says to use `--release-sync`);
  `--release-no-sync --from` is an error.
- **Everything that can fail is checked before the upload or the
  holder** (`syncPreflight`): a run step, the parent released, and
  across servers a local agent that has keys (`agentHasKeys`, `ssh-add
  -l`: `SSH_AUTH_SOCK` set is no agent) and a child reachable without
  `ProxyCommand`/`ProxyJump`.

### Sessions and the decision

- **Two sessions, both from the operator** (`runSyncSession`): the
  holder on the child's server (`HolderScript`, or `ActivateScript`
  with `SyncOptions` between `make_current` and `run`) takes the
  release lock, runs prepare, and prints the marker (`SyncMarker`,
  `$base` and the sync's id: the operator does not know the release
  root), taken out by a `LineMarker` on stderr. Then the push on the
  parent's (`PushScript`: shared `flock -s` on its release lock,
  exclusive on `.sync-lock` so syncs from one parent serialize,
  `devopsy --sync-push`), with `ForwardAgent=yes` across servers. The
  push's exit gives `go` or `abort` to the holder; the holder's exit is
  the command's, but a push that failed after a copy that completed
  (the parent's services not back) fails it too.
- **Both ssh processes ignore SIGINT** (`remote.WithoutInterrupt`): a
  terminal's Ctrl-C reaches the whole foreground group, and a `-T` ssh
  quits on it, so devopsy alone ends them, in order.
- **What the copy did decides, not the answer.** Why: an answer lost
  after a complete copy once restored the child on the parent's data
  unsanitized, and one lost mid-copy ran it on a mix.
- **The gate:** the holder opens it before its marker (`SyncOpenFile`:
  the parent's address and the sync's id, which the push passes on as
  `SyncIDEnv`, so a write left from an earlier sync never claims a
  newer gate) and closes it (`rm`) right before deciding. The child's
  side of the push claims it right before its first write
  (`claimWrite`, `ClaimWriteScript`: a rename to `SyncStartedFile`, one
  of the two wins; the same-server rsync, `SyncReceiveScript` across,
  an import in `syncImport`), so a push that outlived its connection
  writes nothing once the holder decided. It writes `SyncDoneFile`,
  holding the sync's id, after its last write (the rsync's when no
  stream follows, else the last import's, `import <service> last`), on
  the child, never through another session. Across servers the
  receive script removes its rsync container once the holder decided
  (a watchdog on `SyncStartedFile`): after a silent cut between the
  servers the rsync would wait for TCP to give up, and sshd sends
  nothing on a quiet forward (rsync's `--timeout` counts its own
  keepalives into the forward's local socket, so it does not help).
- **The decision** (`syncBlock`), once the holder's read of the answer
  ends, however it ends: its own sync's done is `go` (an `abort` after
  it too), started alone or another sync's done (a push that outlived
  its holder wrote meanwhile) `partial`, neither on a marked
  environment `partial` (decided: nothing written leaves an earlier
  sync's partial copy as it was, even when the parent's labels cover
  nothing now), else the answer; then both files go.
- **A partial copy keeps everything off the data:** `--sync-services
  finish partial` leaves the `stop=true` services down and writes
  `SyncIncompleteFile`; `finish go` removes it, and a failing `finish
  child go` writes it (a sync completes with its finish; the hook
  decides what stays up). While it is there `partialGuard` refuses a
  release that does not sync and a rollback, the restart after a failed
  release is skipped, `CurrentExistsScript` says `incomplete` so the
  operator refuses before uploading (`--release-no-sync` included: a
  sync is the fix, `--release-sync` when the sync itself needs a code
  fix), a push from it refuses (a chain), `--export` warns, and
  `--status` shows it. Labels bound the promise: only `stop=true`
  services are kept off partial data.
- **The mark says why** (`remote.IncompleteLine`: since, parent, code,
  why, tab separated, one line: shell reads it with `cut`, no JSON):
  `copy` (cut), `write` (another sync's push wrote after its holder
  decided), `settled` (a holder killed before deciding), `finish` (a
  complete copy the project's `finish child go` refused: the sync's
  contract is that it completes only once its finish accepts the
  copy). A later mark keeps the first one's time and says its own why;
  the refusals and `--status` print the why.
- **A holder killed before deciding is settled by the next lock-taker:**
  `--sync-services settle`, run by the next sync's prepare and by
  `partialGuard`, counts its files as not completed (marked, `finish
  child partial`); a sync's files while nothing holds the lock
  (`LockBusy`) are a leftover, a running sync is not. A late write (an
  import running on in its container after the holder decided) is
  settled the same way.
- **The answer comes on fd 3, never stdin:** the holder reads it from a
  fifo the watcher writes (`--answer-fd`; both ends opened before the
  watcher starts, so no open ever waits) and gives every command
  `/dev/null` as stdin, so nothing it runs can eat or wait on the
  answer.
- **Without an answer, the holder writes to a file:** devopsy
  interrupted or the connection lost, its output goes to
  `$base/.devopsy-sync.log` (trimmed past `SyncLogMax` to `SyncLogKeep`
  lines, on both sides) and it finishes as the files say. Why: a write
  to the dead session would kill what restores the services. From
  prepare until the services are restored it ignores SIGPIPE, and it
  restores before it reports.
- **An interrupt stops the push first, then lets the holder decide**
  (SIGINT, SIGTERM in `runSyncSession`): before the marker devopsy ends
  the holder's session (`endHolder`) and waits for it (a release that
  syncs is cancelled by its watcher, a holder restores on its own);
  after it, the push's ssh gets SIGTERM (`remote.SSHContext`, SIGKILL
  after `SSHKillDelay`) and is waited for, then the holder gets `abort`
  and finishes as the files say: exit 130, or 0 when the copy had
  completed and the holder went on. After `go`, during the run step,
  the first Ctrl-C only says the step goes on on the server, and a
  second cuts the session. The holder ending first (its connection
  dropped) restored the child on its way out, so the push is cancelled
  the same way and the sync fails.
- **The push watches its session:** its stdin is a pipe devopsy keeps
  open (`SyncWatchEnv`, Go's `watchSession`), so `--sync-push` sees the
  session's end (EOF, or a heartbeat's silence) at once, not at its
  next write, which a silent copy may not make for minutes. That, or
  SIGINT, SIGTERM, SIGHUP or SIGPIPE, cancels it: the same-host rsync
  container is removed (`dockerRun`: killing the docker client leaves
  the container copying), the `-R` session ends, a running export (its
  own process group, `runSyncHook`) and import get SIGTERM, spools go,
  no export starts, and the parent's services are restored. From then
  on `errOut` also appends to the parent's `$base/.devopsy-sync.log`
  (`quietWriter.keep`). What runs inside a container on its own (an
  import already in `docker exec`) runs to its end.
- **Servers' commands write to `errOut`, never an `*os.File`:** a gone
  session cannot kill them with a broken pipe.
- **Test fakes:** the fake ssh runs through dash on Linux, which never
  execs the last command of `sh -c`, so cancelling closes the pipe, as
  sshd would, rather than signalling ssh.

### Labels and the child guard

- **Labels opt in:** `devopsy.sync=rsync|stream` (`cli.SyncLabel`); no
  label or an empty value is `skip` (neither copied nor touched), any
  other value an error (`live`, `pause`, `stop`, `custom` went, with
  `push` and `flush`: no migration). Nothing labeled, or nothing to
  copy (no entries, no streams): "nothing to sync", success, the
  parent's hooks skipped, and the run step still runs, unless the child
  is marked (it stays so).
- **`devopsy.sync.stop=true|false`** (`SyncStopLabel`, default false,
  ignored without `devopsy.sync`) stops the service on both sides for
  the whole sync, a stream included. On the parent that is downtime for
  the copy: the project's choice, a stream without it the alternative.
- **No `pause`:** `docker pause` flushes nothing (rsync sees what the
  kernel has, not a paused process's buffers), and Traefik 3.7 drops a
  paused container's router the next time it reads the containers
  (devopsy-traefik's AGENTS.md); a stopped routed container loses it at
  once (404). Consistency comes from `stop=true` or a stream's dump.
- **`devopsy.sync.rsync.exclude`** (`SyncExcludeLabel`, entries of
  `mnt`, comma or space separated) drops entries from an rsync
  service's `Dirs` at parse time, so every consumer (copy, guard, list)
  agrees. On a `stream` service it is ignored, not validated, so a
  variable can flip `rsync`/`stream` and keep it.
- **A `.` entry (`./mnt` itself) subsumes the rest,** other services'
  entries included: `--sync-push` says so (`SyncWholeNotice`, yellow
  when the operator's terminal takes colors, `ColorEnv` passed with the
  push's variables), only a notice: it never excludes by itself.
- **The child guard: each side reads its own labels, and the parent
  checks the child's before changing anything.** `--sync-push` asks the
  child for `--sync-services list` (through `--sync-exec`, one more
  session across servers) and refuses (`syncConflicts`) when a child
  service mounting a copied entry is running or restarting and not
  `rsync` there (a `stop=true` one is stopped already), and
  (`streamMismatches`) when a parent `stream` service is not `stream`
  on the child, whose import would refuse it after the rsync, or a
  child `stream` service is `rsync` on the parent. A label from an
  inherited variable cannot lag (the child reads the pending file,
  inherit below); one in the child's own `shared/.env` can still
  differ. The list's format is on `runSyncServices`.
- **Rejected for these labels:** a `push` script with helpers (it only
  works from the parent's server to the child's, never for a file, a
  laptop or a relay; `export` and `import` give the same freedom and
  serve `--export` and `--import` too); export and import commands in
  compose labels (scripts do not belong in labels); `stop` as a value
  and a sub-label at once, or a hold sub-label for streams only (two
  orthogonal labels instead); a dump into an entry of `mnt` as
  devopsy's way (at rest on the parent, twice the disk, cleanup and
  completeness the project's again; fine for a project that wants
  dumps on disk anyway); an `--export` that never stops a service (the
  scripts are written for the label: a `run --rm` export against a
  live service fails or copies inconsistent files).

### Order and state

- **The child's side** (`--sync-services prepare`): `settle`, the state
  record, the import check, the `prepare child` hook, then one `compose
  stop` of its `stop=true` services. Keep it one call: Compose orders
  them by `depends_on`, dependents first. `finish go|abort` starts them
  and waits until healthy, `partial` leaves them down; spools are
  removed unless `go`; then `finish child` with the word. Stopped
  services stay down through the imports: no app on a half-imported
  database.
- **The parent's side** (`runSyncPush`): the checks (labels, the
  `export` script when a service is `stream`, the child guard, the util
  image) before anything, then `prepare parent`, on the same server a
  refusal when the child's gate is gone already (its holder decided),
  stop, rsync, streams, start (healthy), `finish parent go|abort`. A
  failing prepare aborts before its side stops anything.
- **Hooks run from Go** (`runSyncHook`, `--capability sync ...` in its
  own process group), only where their file is (`export` and `import`
  are required for a `stream` service), with that side's label lists
  (`cli.SyncLists`) and the sync's addresses and arguments in their
  environment (README). The holder's scripts (`syncPrepare`,
  `syncFinish`) only call `--sync-services`.
- **The state record exists before anything can fail:** stopping
  records what it stops in `$base/.devopsy-sync.services` (`syncState`,
  `stop <name>` lines; `startSyncState`: the child's prepare creates it
  before its hook, `stopServices` before its first `ps`). Why: no
  record means restore every `stop=true` service, one down on purpose
  included. One left by an earlier sync that could not restore is kept
  and added to (`syncRecord`, rewritten whole).
- **Restoring brings back only the recorded services,** whatever their
  labels say now (an inherited variable may have changed them since a
  cut sync), from container state (exited), so it also recovers after an interrupted sync, and a
  service down before the sync (a maintenance stop) stays down. It
  waits for every service it started to be running and healthy
  (`waitHealthy`: one gone fails), and removes the record once all are
  back. Service states come from one `docker compose ps --format json`
  (`serviceStates`, `servicesIn`); the sync tests' fake docker remembers
  stops and starts to answer it.
- **A child's first release gets every labeled service before its
  finish hook:** a release with nothing current before it (`prepare
  first`, from the activation), or no container at all when prepare
  started, is a `first` line in the record (a stream's import creates
  its container before finish looks, and a first release that failed
  may have left one; a later sync that finds containers drops it). Before
  the hook every labeled service is created in one `up -d --wait`,
  since the hook sanitizes through them (drupal11's needs the app:
  without it, prod's passwords reached a child), and a stream imported
  then stays up. Only on go: an aborted first sync creates nothing.
  Never otherwise: a missing service was removed on purpose.

### Streams

- **An import only ever reads a complete export:** per `stream`
  service, in `depends_on` order over every service
  (`SyncStreamServices`: dependencies first, ties alphabetical, a cycle
  an error), the parent runs the capability's `export <service>`
  (`syncStream`) into the child's spool,
  `$base/.devopsy-sync.stream.<service>` (`remote.StreamSpool`): on the
  same server directly, across servers through `cat >` in an SSH
  session to the child (`exportInto`). Only once the export exited 0
  does the child's `--sync-services import <service>` run. Never a
  pipe into the import: a cut export never reaches it.
- **The import:** the service brought up (`upFor`, `--no-deps` but on
  a first release: Compose would start a dependency the sync stopped)
  unless `stop=true` (down again after if it was down), `import <service>` with the spool
  as stdin, the spool removed whatever happens. Its stdin is a pipe the
  push keeps open, so across servers the session's end, which a remote
  process never gets as a signal, cancels it too.
- **Every file a sync leaves in `$base` starts with `.devopsy-sync.`**

### Transport, SSH and environment

- **rsync always runs in `devopsy-util:<n>`** (`UtilImage`, now `:1`;
  built on each host from `UtilDockerfile` when missing; GNU tar for
  `--export` and `--import` too), as root with `--numeric-ids`. Why:
  owners and modes survive, and the deploy user's permissions do not
  matter. Bump the tag whenever the Dockerfile changes: hosts never
  rebuild an image they have.
- **Entries are the top-level entries of `mnt` that `rsync` services
  bind-mount** (`docker compose --profile '*' config`, lexical and
  resolved paths; `.` subsumes the rest); one missing on the parent is
  skipped with a notice, the child's left as it is. An entry that is a
  file on the parent is copied as one, `--inplace`, so the container
  bind-mounting it sees the new content (a bind mount is the file's
  inode); across servers its module is `/mnt` filtered to it.
- **Across servers the parent's daemon serves only what is copied:**
  one module per entry (`rsyncdConf`, `RsyncModule`: the entry's name,
  `mnt` for `.`), so nothing else under `mnt` can be asked for.
- **rsync's exit 24** (files vanished during the copy) is a notice
  (`RsyncVanished`); any other failure aborts.
- **The child's sessions from the parent never get the operator's
  agent** (`ChildSSH`, through `DEVOPSY_SSH_COMMAND` too):
  `ForwardAgent=no`, whatever the parent's ssh config says. The parent
  can use every key in the agent for the session, prod's included when
  a chain's middle environment is the parent, which only the
  operator's agent limits (README). Also `BatchMode=yes` (a refused key
  fails instead of prompting), `ConnectTimeout=30` (an unreachable
  child fails before the TCP timeout, with the parent's services
  stopped meanwhile) and `StrictHostKeyChecking=accept-new`.
- **The rsync session keeps its forwarding:** `-R` to the parent's
  daemon, `--network host` on the child, `ExitOnForwardFailure=yes`,
  and never `ClearAllForwardings`, which clears command-line
  forwardings too (`ssh_config(5)`); a taken port retries another (3
  tries, 20000-60000). `--sync-exec`'s session and a stream's `cat`
  keep `ClearAllForwardings`. All of a sync's sessions pass
  `KeepAlive`: the holder is quiet for the whole copy.
- **Say what a refusal means:** no agent on the parent with
  `DEVOPSY_SYNC_TO` set means a `restrict`ed key or sshd's
  `AllowAgentForwarding no`. A `restrict`ed key also refuses the `-R`
  (`port-forwarding`), which sshd reports like a taken port: after the
  last try, say both.
- **The child's devopsy never sees the parent's environment:** on the
  same server `--sync-exec` runs it with `cleanEnv` (`cleanEnvKeys`:
  the machine's variables, the sync's own, Docker's and devopsy's
  settings, and the child's `COMPOSE_PROJECT_NAME`). Why: the caller's
  environment wins over a project's `.env`, so the parent's secrets and
  project name would reach the child's commands. Across servers
  `SyncExecScript` exports the sync's own (`syncExecEnv`) into the
  clean login environment.
- **Hidden flags** (dispatched in `main.go` before anything else):
  `--sync-push`, `--sync-exec <devopsy args>` (both need the push's
  `DEVOPSY_SYNC_TO_DIR` and `DEVOPSY_SYNC_TO_PROJECT`) and
  `--sync-services prepare [first] | finish go|abort|partial | settle |
  import <service> [last] | list` (`runSyncServices`). `DEVOPSY_SYNC_ARGS`
  (space-joined, split like `StepArgs`) go to rsync, and to the hooks
  to read.

### inherit

- **A parent's `inherit:` keys become the child's
  `shared/inherit.env`, whole:** `inheritVars` reads the parent's three
  files in one session (`InheritReadScript`), takes each key from the
  nearest level, and writes them all (`InheritWriteScript`, on stdin,
  under the child's lock: a resync replaces the file), which every
  release links and the loader reads below `.env`. Notices name the
  keys the child's own `shared/.env` shadows, never values. `--inherit`
  (`runInherit`) runs it alone.
- **A first release inherits into `shared/inherit.env` before the
  upload,** so `--missing-vars` sees the values; kept on failure, since
  the next release is first again.
- **A re-sync inherits into a pending file the sync reads in place of
  the release's:** `$base/.devopsy-sync.inherit.env`
  (`remote.SyncInheritPending`), named by
  `DEVOPSY_INTERNAL_INHERIT_FILE` (exported by the holder, set by
  `--sync-exec`) for everything the sync runs on the child. Why: the
  child's labels follow an inherited variable in the same sync; written
  after the copy, they lagged one sync. `finish go` moves it into place
  (`mv -Tf`) before restoring services, the hook and the run step;
  anything else removes it. Rejected: writing `inherit.env` in place
  with a backup restored on abort, since a holder killed outright
  leaves the new file over the old data. A holder told none was written
  (`SyncOptions.Inherit`) removes a stale one first.

## --export and --import

One labeled service's data to stdout or from stdin, moved the way its
sync label says, with the sync's scripts. Code:
`cmd/devopsy/data.go`, scope `[@t]`.

- **The label decides how, and it is the contract:** an unlabeled
  service is refused. `stream`: the capability's `export` and `import`
  (`runSyncHook`, the lists in their environment). `rsync`: a tar of
  the service's entries of `mnt` (exclude honored, file entries
  included) made in `UtilImage` as root with `--numeric-owner`.
  `stop=true` stops the service around either and restores it
  (`stopFor`), interrupts included. A `stream` import of a service not
  stopped brings it up and down again if it was down (`upFor`). A
  service mounting `./mnt` itself carries all of `mnt`, said
  (`wholeMntNotice`).
- **An rsync import checks the whole tar before changing anything**
  (`tarEntries`, read with `archive/tar`): a truncated tar fails, and
  so does one without its end-of-archive blocks, which every tar
  writer adds: `tar -t` takes a tar cut at a member boundary for
  complete, and the last member's own zeros (a sparse file, a
  database's pages) pass a check of the spool's last bytes; entries the service does not mount are refused, an
  excluded one by its label. Then each carried entry is emptied, never
  removed (for `.`, every entry's contents and the files beside them),
  and extracted with `--overwrite`. Why never removed: a running
  container bind-mounts the directory's inode.
- **stdout is the data:** `--export` refuses a terminal; `--import`
  refuses a terminal as stdin, spools it whole first (`importSpool`: in
  a release the sync's `$base/.devopsy-sync.stream.<service>`, locally
  a temporary `.devopsy/.devopsy-sync.stream.*`, which `excluded` keeps
  out of releases), refuses an empty one, and only then stops anything.
  The spool always goes.
- **On a target the server runs the same code:** `ExportScript` (shared
  release lock and exclusive `.sync-lock`, like the push: both stop and
  restore the same services; stdout through as it is, never
  colorized) and `ImportScript` (exclusive lock, `devopsy --import
  <service> --yes`). The local side asks for the environment's name on
  `/dev/tty` (`confirmImport`: stdin is the data; no terminal and no
  `--yes` fails).
- **A remote import needs an end line to count as complete:** the local
  side sends stdin and, only after a clean EOF, an end line with a
  random nonce (`ImportEndEnv`); the server requires the line and takes
  it off. Why: a session cut halfway reaches the server as a plain EOF.
  A pipe from a failed export still looks complete: README says to go
  through a file for what matters.

## Roles, exports and imports

Facts shared between projects on a host, through labels on running
containers (`internal/cli/roles.go`). Governed by "Projects own
policy": devopsy knows no role or key names (`proxy` and
`WILDCARD_DOMAIN` are the Traefik template's convention), and its core
defines no proxy contract (a `domains` contract in the core was shaped
by one pairing, the Traefik template and its projects).

- **The labels:** `devopsy.role` (one compose project per host),
  `devopsy.export.<KEY>`, `devopsy.import.<VAR>=<role or
  project>/<KEY>[?]`.
- **Imports resolve on the host at each release, never at runtime in a
  container.** Why: compose interpolates labels (a Traefik rule) before
  any container exists.
- **`--prepare-release` checks the role and writes the imports before
  the release goes live,** into its `devopsy-target.env` after
  `cli.ImportedMarker`. Exports are read from running containers
  (`Running`: one snapshot per lookup), waiting up to
  `WaitForExporters` (30 seconds) for required sources.
- **An import never overrides what the environment sets,** even empty.
- **An empty export is a value.** Why: else every project importing
  without `?` would fail under a proxy without a wildcard.
- **Rollbacks never check roles or imports.** Why: it would fail a
  rollback exactly when one is needed, the proxy down.
- **Roles are advisory** (accepted): anyone in the docker group is
  root-equivalent. They are checked only at releases, never on a local
  `up` (a test proxy beside a main one is normal locally), and two
  releases claiming one role on different targets at once are not
  locked against each other.
- `--debug imports` compares every running project's imports with its
  release's `devopsy-target.env` (found through compose's working_dir
  label).

## Other features

Smaller features, each with the rule an agent must keep.

- **`--status`** (`cmd/devopsy/status.go`): locally, the containers as
  a table, then the `status` capability's `show`. With a target,
  `StatusScript` prints header lines from the server's files (the
  record, live releases kept, the last copy from the session logs'
  `SyncCopied` lines, a partial or running sync from `partialStatus`),
  `---`, then `devopsy --status` in the current release; the operator
  formats the header and prints the rest as it came.
- **`--ssh-keys` edits only the deploy user's own
  `~/.ssh/authorized_keys`** (`cmd/devopsy/sshkeys.go`,
  `internal/remote/keys.go`), over the usual session. Why: no root, so
  devopsy-cli stays out of host setup except for this file, which that
  user owns anyway. Lines are parsed locally (`ParseAuthorizedKey`,
  the fingerprint computed from the blob, no ssh-keygen needed) and
  edited on the server under a lock next to the file, by blob: a
  comment never identifies a key. `add` refuses a duplicate (exit 3),
  `remove` the last key (exit 4; commented-out keys are none, and
  devopsy refuses locally first). `--restricted` on `add` is
  `RestrictedKeyOptions`, the one place that line is written.
  `--ssh-keys-generate [<name>] [--out <file>]` is local, its own flag:
  just `ssh-keygen`, nothing sent; the `.pub` is added per server.
- **Completion lives in Go and never connects to servers**
  (`internal/complete`): the scripts only pass words to the hidden
  `--complete` and show what it prints (cobra's protocol), so compose's
  own completion passes through. After `@` and for `--from` it offers
  `~/.ssh/config`'s hosts as `@<host>:` (`sshHosts`), read in Go, not
  delegated to the shells, so every shell gets the same. bash splits
  words at `:`, so its script rebuilds them from `COMP_LINE`. A new
  subcommand's words go in `complete.go`, its help in
  `cli.RemoteCommandHelp`. `TestCompletionShells` runs the scripts in
  every installed shell (bash, macOS's bash 3.2, zsh with `_describe`
  stubbed, fish); CI installs zsh and fish.
- **`--debug` traces the loading itself, never a second implementation
  of it:** targets carry `From` (each value's origin), and `Env`
  records every definition of a variable as it loads (`EnvDef`,
  `TraceEnv`: `--debug env`). A new source of variables records its
  definitions too. The capability catalog (`cli.Capabilities`) lives
  with the code that calls it.
- **Capabilities run in the project's own environment and session**
  (`--capability <name> <action>`, hidden; executables in
  `.devopsy/capabilities/`), so no other project's variables leak in.
- **devopsy knows no URL:** it always sets `COMPOSE_PROJECT_NAME`
  (locally the config's `project:`, else compose's `name:`, else the
  folder) and runs the `env` capability (`computeEnv`) before every
  command but help and completion; projects derive hosts, rules and
  URLs from the name and imported facts. Its output only fills unset
  variables; `DEVOPSY_INTERNAL_ENV_COMPUTE` keeps a devopsy it calls
  from running it again.
- **`DEVOPSY_INTERNAL_*` are devopsy talking to devopsy** (`COLOR`,
  `TTY`, `ENV_COMPUTE`, `WATCH`, `SYNC_WATCH`, `SYNC_ID`, `HEARTBEAT`,
  `HEARTBEAT_EVERY`, `COMMAND_TARGET`, `IMPORT_END`, `INHERIT_FILE`):
  never documented as settings, free to change in any release. Settings
  people set and the contract projects read keep plain `DEVOPSY_*`
  names.
- **`--probe` is a tool with plain arguments, not a contract:** it
  knows no proxy. A server never drives local devopsy (devopsy execs
  ssh and cannot watch its output, and a server must not make a laptop
  act): proxies print the `--probe` line to run.
- **Every session is `ssh [DEVOPSY_SSH_COMMAND's options] [SSHOptions]
  -T|-t <host> 'sh -c <script>'`** (`remote.SSHLog`), `-t` for any
  command at a terminal (a shell, a project or compose command,
  `--upgrade`; activations and holders never), with
  `ClearAllForwardings=yes`, since the user's `LocalForward` lines
  clash between sessions. Connection sharing is the user's SSH config,
  which `--ssh-config` suggests and never writes. Test fakes of ssh run
  their last argument.
- **`--context-hash` is a pure helper; devopsy builds nothing.** Errors
  only ever go towards a new hash (a kept file costs a rebuild, a
  dropped one would reuse a stale image), hence Docker's own matcher
  and refusing what it cannot hash.
- **`--upgrade` reads the same release files as `install.sh`** (archive
  names without version, `checksums.txt`). The daily check runs
  detached (`--upgrade-check`), since devopsy execs right away; only
  release builds upgrade or check.

## Gotchas

Findings that are not one area's.

- Compose reads `--env-file` more than once: `<(devopsy --env)` fails,
  a file works.
- `.env` values in double quotes are interpolated by compose's parser.
- GitHub can deliver a tag push several times: the release workflow
  queues runs per tag and GoReleaser replaces assets. If a run fails,
  check the release's assets before assuming it broke.
- GitHub's `releases/latest` can lag a new release by minutes: `devopsy
  --upgrade vX.Y.Z` installs one directly.
- Go's HTTPS to github.com is slow (about 1.5 s), hence the detached
  check.
- Traefik picks up a new container a few seconds after `up` returns.
- macOS hides what Linux servers do: dash as `sh` never execs the last
  command of `sh -c` (so a signal to it does not reach that command),
  and two releases can land in the same second.
- OrbStack machines are `ProxyCommand` hosts from macOS: syncs across
  servers need hosts reachable directly (`ssh -G` without a proxy),
  like `<machine>.orb.local` with a key of your own. Their
  `/etc/ssh/ssh_config.d/10-orbstack.conf` sets `IdentityAgent`, which
  hides a forwarded agent from the parent's `ssh`
  ("get_agent_identities: Permission denied"): remove it on test
  machines. OrbStack's own SSH also keeps stdin open and leads no
  process group: the watcher needs a real sshd (e2e).

## Checks

```sh
go vet ./... && go test -count=1 ./...
docker run --rm -v "$PWD:/mnt" -w /mnt koalaman/shellcheck:stable -s sh install.sh
```

Before tagging, run the tests on Linux too: CI does, and macOS hides
differences that matter there (Gotchas). The caches keep reruns fast:

```sh
docker run --rm -v "$PWD:/src" -w /src -e GOFLAGS=-buildvcs=false \
  -v devopsy-gocache:/root/.cache -v devopsy-gomod:/go/pkg/mod \
  golang:1.26-trixie go test -count=1 ./...
```

End to end, in Docker (`e2e/`, behind the `e2e` build tag): privileged
Debian 13 containers with their own Docker and sshd as servers, an
operator container, devopsy built from the checkout in each; the tests
drive them with `docker exec` and check from outside. Locally with any
Docker, on GitHub on demand and nightly when `main` moved since the
last run that passed (`e2e.yml`); the release workflow does not run
them. Drop tests shorten the heartbeat (`E2E_BEAT`, default 1, passed
as `DEVOPSY_INTERNAL_HEARTBEAT_EVERY`: quiet times and the operator's
`KeepAlive` count in beats, which keeps their order); `E2E_BEAT=5`
runs everything with the real timeouts, and one test always does. The
test server's deploy user has `/bin/sh` (dash) as login shell, stricter
than devopsy-server's bash (it found the watcher signalling only the
script there). A scenario a review finds goes there, unless a unit
test can cover it (Principles, "Every change carries its test").

```sh
go test -tags e2e -count=1 -v -timeout 25m ./e2e   # E2E_KEEP=1 keeps the containers
E2E_BEAT=5 go test -tags e2e -count=1 -v -timeout 40m ./e2e   # the real timeouts
```

What only a real setup shows (Traefik, certificates, the templates) is
checked by hand: an OrbStack Debian 13 machine (`orb create
debian:trixie devopsy-test`) set up with devopsy-server, a linux/arm64
build installed in it, and targets like
`@devopsy@devopsy-test@orb:prod` (OrbStack's SSH needs no keys).
Release the Traefik template there, then a template importing from it,
and check `devopsy-target.env`, `--debug imports`, the Traefik
`domains` command, an instance, a pattern environment and `--destroy`,
a rollback, and a release with Traefik stopped. Delete the machine
afterwards.

`--upgrade` and the release notice only work in release builds: `go
build -ldflags "-X main.version=0.8.0" -o /tmp/devopsy ./cmd/devopsy`,
then `/tmp/devopsy --upgrade`. The notice needs a terminal (`script -q
out ./devopsy ps`).

## Releases and commits

Tag `vX.Y.Z` on `main` and push the tag: the release workflow tests,
and GoReleaser publishes the binaries and `checksums.txt`.
Conventional commits (`feat:`, `fix:`, `docs:`, `build:`...), with no
attribution trailers: no `Co-Authored-By`, no session links.
