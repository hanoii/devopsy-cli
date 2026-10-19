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
  util-linux and devopsy, never access to the repository, and nothing
  listens. Why: no control plane, no daemon, no webhooks, no deploy
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
  empty environment; moving the old one is by hand (done once,
  Traefik's `main` to `prod`, devopsy-traefik's AGENTS.md).
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

## Layout

- `cmd/devopsy/`: `main`, which executes what `internal/cli` plans, the
  operator's side of remote commands (`remote.go`), syncs (`sync.go`),
  `--export` and `--import` (`data.go`), the session watcher and the
  heartbeat (`watch.go`, `heartbeat.go`), and tests that run the built
  binary against a fake `docker` and `ssh`.
- `internal/cli/`: finds the project, loads `.env` and decides what to
  run (`Build` returns a `Plan`; it only runs helpers, like the `env`
  capability, never the command); `cli.Flags`; roles, exports and
  imports (`roles.go`); capabilities; the `devopsy.sync` labels
  (`sync.go`).
- `internal/remote/`: config and targets (`config.go`), the documented
  schemas (`schema.go`), packing a release, and the POSIX `sh` scripts
  run on servers (`remote.go`, a sync's in `sync.go`; checked with `sh
  -n` in tests; GNU coreutils, `mv -T`).
- `internal/images/`: the rules for releases' images on a server, behind
  a `Docker` interface, run there as the hidden `--images`.
- `internal/report/`: failures and warnings (Conventions, "Failures and
  warnings").
- `internal/complete/`: completion candidates, and the bash, zsh and
  fish scripts (embedded).
- `internal/probe/`, `internal/update/`: `--probe`, `--upgrade`.
- `e2e/`: end-to-end tests against servers in Docker (Checks).
- `install.sh` (POSIX `sh`, dash and busybox ash), `scripts/devopsy`
  (runs the checkout's code), `.goreleaser.yaml`, `.github/workflows/`.

## Conventions

How the code is written; the principles above say what it is for. Each:
the rule, why, and where it lives.

- **stdout is the result, stderr is devopsy talking.** The result:
  compose's output, `--env`, `--vars get`, `--context-hash`, `--export`;
  everything else (progress, notices, errors, the server scripts' `echo`
  lines, `>&2`) on stderr. Why: pipes stay clean, and the two never
  interleave out of order. Scripts show only at `-vv`: a session's, and
  an argument of several lines in the "Running" notice, which otherwise
  counts its lines (`Plan.running`).
- **Failures and warnings say why and how to fix it** (`internal/report`):
  `report.Fail` (`[error] <message>`, red) or `report.Warn` (`[warning]
  <message>`, yellow), then why, one detail per line, then "To fix it:"
  and the fix, or "To fix it, do one of these:" and each numbered; a
  fix's commands on their own lines, never wrapped or indented. The
  message reads as a sentence ("dev's parent, prod, has no server").
  Why: the fix is a command to copy, whole. `report.Error` carries
  details and fixes through wrapping; a command adds the fixes only it
  knows. Used so far for a parent without a server, a local `--release`
  without steps, `--releases remove` and `--destroy`'s leftovers: other
  messages move over as they are touched.
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
  what scripts capture. A session's lines are colored locally as they
  arrive (`remote.Colorize`, line-buffered, terminal writers only),
  since coloring on the server reaches nothing: `devopsy:` lines cyan,
  red for a failure, Compose's done steps green, steps on the way dim,
  errors red. `remote.Color` exports `DEVOPSY_INTERNAL_COLOR` for the
  little the server colors itself. Release logs get the raw lines. Help
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

## Config and targets

- Files: the project's `.devopsy/config.yaml` (`project:`,
  `instances:`, `secrets:`, `public:`, `releases: {keep}`, `defaults:`,
  `environments:`), its `config.local.yaml` over it, and the user's
  `~/.config/devopsy/config.yaml` (`aliases:`; on servers `releases:`).
  Never `~/.devopsy`: discovery would take the home directory for a
  project. Unknown keys are errors.
- `defaults:` gives environments `mode`, steps, `releases`, `parent`,
  `inherit` and `env`; `env` merges key by key, `KEY: ~` removes, `""`
  stays empty (hence parsing YAML nodes: `nullKeys`); `parent: ~` and
  `inherit: ~` remove theirs.
- **A target is an address,** `@[<server>:][<instance>/]<environment>`
  (`parseAddress`): the server ends at the first `:`, the instance
  before the last `/`, empty meaning none. Missing parts come from
  `DEVOPSY_SERVER_<ENVIRONMENT>`, the pattern's (`PatternVar`: `pr-*`
  gives `DEVOPSY_SERVER_PR`), `DEVOPSY_SERVER`, and `DEVOPSY_INSTANCE`:
  the caller's environment, then the project's `.env`, never for
  aliases. No server: error. `instances: true` makes every target name
  an instance (an alias's from its `to:` only); otherwise none may, so
  a project's environments never mix `<project>/<env>` and
  `<project>/<instance>/<env>` on a server, and `--environments` lists
  one layout. Explicit none (`@vm1:/prod`) only parses: it is the
  default.
- **Patterns:** environment keys with `*` (one or more name
  characters); exact name, then most literal characters; a tie is an
  error.
- **Aliases** (`{to, source}` or `{to, project}`): only a bare `@name`,
  and a project's exact environment wins. With `source:` its config
  gives the project, environments and steps, and releases run only from
  there (`ReleasesHere`, symlinks resolved): anywhere else a release
  would upload whatever project devopsy runs in.
- **Release settings belong to the server:** its deploy user's
  `releases:` (`root`, default home; `keep`, `max_keep`, default 5),
  read by the hidden `--release-settings` that every script asks first
  (`basePrelude`), so a laptop and CI resolve the same paths. A
  project's `keep` is capped by `max_keep`; `root` and `max_keep` in a
  project are errors.
- `--init` writes a new config and never touches an existing one.
  `--debug schema [--user]` prints `ProjectSchema` / `UserSchema`,
  documentation written as configs that parse (`TestSchemas`): a new
  key goes there too.

## Releases

### Steps

- `--release` and `--rollback` take no command: the environment's
  `release:`/`rollback:` (`remote.Steps`: before, prepare, run, after; a
  plain string is a run step alone) are required. Phases, not a free
  list, so the upload and the switch are never hidden in a step. The
  remote steps share one SSH session, which holds the release lock, so
  before is local steps then remote ones, after is remote then local,
  checked when the config is read. Local steps get the target's env,
  a child's `DEVOPSY_PARENT` and `DEVOPSY_PARENT_TARGET` (`parentEnv`)
  and the commit, never the server's `.env`, and never `DEVOPSY_TARGET`,
  `DEVOPSY_ENVIRONMENT` or `DEVOPSY_INSTANCE` (decided): set, they mean a
  release on a server, which the templates' `env` capabilities test,
  and `DEVOPSY_INSTANCE` would reach the step's own devopsy calls. A step
  that needs the address declares `## Needs: target`
  (`DEVOPSY_COMMAND_TARGET`).
- Remote commands run through the `current` symlink, never a release
  path: compose stores bind-mount paths in containers, and pruned
  releases would break them. The exception is `prepare`, in the new
  release before it is current: it must not start services.
- **Steps of what is deployed come from the server:** a rollback's
  (hidden `--release-steps rollback <phase> [<release>]`,
  `runReleaseSteps`), the restart after a failed run and `--destroy`'s
  step (`destroyStep`) are read from the release's own `config.yaml`,
  its environment from `devopsy-target.env` (`ReleaseEnvironment`),
  never the checkout's, which may have changed or be another branch. A
  release's come from the checkout: it is what is uploaded. A
  rollback's local before and after steps stay the checkout's.
- `mode:` defaults to `build`: it works for every project, where a
  wrong `image` fails the release. devopsy fetches the images itself,
  before the switch, so the run step only starts them: a build or pull
  there would run after the switch, and in a sync after the copy.
- `--release` without a target (`runLocalRelease`) is a release in place:
  `defaults:`' release steps (no environment is local), prepare,
  `--missing-vars`, the images always as build mode, run; no before or
  after steps, no switch. `--destroy` likewise (`runLocalDestroy`): after
  the project's name, `runDestroyData` in place, `defaults:`' destroy step
  if any; never the checkout. The other release flags stay target only.

### Upload

- Before uploading, locally or in short sessions: secret-like names
  (`confirmUnnamedSecrets`), a role another compose project holds
  (`RoleHoldersScript`), a new instance, which asks
  (`InstanceExistsScript`, `--yes`), and a child's sync (Syncs).
- `UploadScript` takes the release lock, extracts into
  `releases/<id>.tmp` through `--stdin-guard`, writes `PendingFile` and
  renames it into place. An existing id is refused: `mv` would put the
  new tree inside the old release, which would run again. Ids are UTC
  seconds (`NewRecord`), so tests releasing a target twice wait for a
  new second (`spaceReleases`).
- Full tar streams, no rsync (macOS ships openrsync). The progress line
  (`uploadProgress`) counts the project's bytes as `Pack` reads them,
  out of `remote.Size` (the gzip's size is unknown until the end); at a
  terminal only, never logged.
- The server needs `devopsy`, `tar`, `flock`, `setsid`, `script`
  (util-linux), GNU `tail` and `mkfifo`. Without `script` no live build
  progress; without `setsid` the run step runs plainly; without GNU
  `tail` only the traps protect a release after its switch. Locally
  only `ssh`.

### Activation

`ActivateScript`, one session, in order:

1. The release lock, the watcher (Sessions), `img recover` of a release
   left fetching.
2. A rollback picks its target (`img rollback-target`: the newest live
   release older than current). `partialGuard` refuses a rollback, and
   a release that does not sync, on a partial copy (Syncs).
3. The pending lock (fd 7), then remote before steps in `current`
   (none on a first release).
4. Links into the release: `shared/*`, `inherit.env`, and the project's
   and instance's `.env` (`project.env`, `instance.env`), even when
   missing, so one created later applies.
5. Releases only: `--prepare-release` when the compose files use role or
   import labels (`PrepareScript`); `--check-names "$base"`: containers of
   the same compose project from another directory (`cli.ProjectConflict`:
   `-`-joined names coincide, `a-b`/`c` and `a`/`b-c`), and a network the
   project names as its own that another compose project owns
   (`cli.NetworkConflicts`: Compose would silently share it; external
   networks are how a project joins another's; an unreadable config is
  skipped, the up will say).
6. `prepare` steps, then `--missing-vars` (`cli.MissingVars`:
   compose's `${VAR:?}`/`${VAR?}` left unset, env capability included).
7. The images (`fetch_release`). `ServerLogFile` is created before the
   switch, so a full disk fails the release before it goes live.
8. `PendingFile` removed, `make_current`, then the sync of a release
   that syncs, `followOutput` and the run step.
9. A failed run (or sync): `FailedFile`, `current` back to the previous
   release, restarted with its own images (`get_images`) and its own
   run step (`--release-steps restart run`: its config's `rollback:`,
   else `release:`), except on a partial copy. A rollback whose run
   step fails marks the release it restored failed (decided: it did not
   come back up, so it is no rollback target until released again).
10. A rollback marks the release it left (`RolledBackFile`).
11. Pruning to `keep` (`img to-prune`, `img remove-releases`, `img
    prune`), then remote after steps (a failure is reported, the
    release stays live).

A failure before the switch calls `not_live` (`NotLive`,
`RollbackNotLive`): nothing live changes, and a release is marked failed.
It never removes a directory: it once removed a failed first release's,
and with it a second release uploaded behind it. No prompts: what is
missing is listed with its `--vars set` line.

### Releases on disk

- `releases/<id>/code/` is the project as uploaded (`Pack` prefixes
  it), `current` points there, and devopsy's files sit beside it
  (`remote.CodeDir`, `RecordFile`, `TargetEnvFile`, `ImagesFile`,
  `LogFile`, `ServerLogFile`, `PendingFile`, `FetchingFile`,
  `FailedFile`, `RolledBackFile`), never in a build context (inside the
  tree, `COPY . .` missed the cache every release). Scripts keep `$rel`
  the release's directory, `$code` its tree, `$prev` `releases/<id>`.
  Go finds them with `remote.ReleaseFile` (symlinks resolved).
- A release is live when it is none of pending (upload to switch),
  fetching, failed or rolled back: what `keep` counts and a rollback
  goes to. Failed and rolled-back ones go once older than the oldest
  kept. A release that fails before going live stays, a first one
  included, until `--destroy` or `--releases remove failed`.
- `--releases` shows a pending release IN PROGRESS while its activation
  holds fd 7 on `PendingFile` (from when it holds the release lock to
  the switch; checked with `flock -n -s`, never waiting), else
  INCOMPLETE, which `remove failed` takes only when older than current:
  a newer one may be between its upload and its activation.
- `devopsy-target.env` (environment `env`, `COMPOSE_PROJECT_NAME`,
  overriding compose's `name:`, `DEVOPSY_PROJECT`, `DEVOPSY_INSTANCE`,
  `DEVOPSY_ENVIRONMENT`, `DEVOPSY_TARGET`, `DEVOPSY_RELEASE_COMMIT`, a
  child's `DEVOPSY_PARENT` and `DEVOPSY_PARENT_TARGET`) is written into
  each release, so it applies however devopsy runs on the server and
  rollbacks restore it. Precedence there: caller, `shared/.env`,
  `inherit.env`, `instance.env`, `project.env`, `devopsy-target.env`.
- Nothing of the local environment goes to a server: a target's
  `COMPOSE_PROJECT_NAME` is always its compose name (`projectName` in
  `runRemote`; one exported for local work put every environment under
  one compose project). What does go: values `--vars set KEY` reads on
  purpose, display settings (`DEVOPSY_INTERNAL_*`, `DEVOPSY_VERBOSE`),
  who released (`USER`, CI's actor) in the record.

### Locks

- **The release lock** (`$base/.lock`, fd 9): every script that changes an
  environment takes it (`releaseLock`, `lockWait`: `-n`, then a "waiting
  for" line and `-w 600`, so a wait is never silent). The upload too:
  `--destroy` never empties `$base` under an upload, and a second
  release's upload waits for the first's activation. Of the release
  scripts only the upload creates `$base`; outside them `--vars` edits
  and the inherit write do too (`internal/remote/vars.go`): an
  activation whose release went while it waited fails instead of making
  it again. A `.lock` removed while awaited is opened again (`[
  /dev/fd/9 -ef "$base/.lock" ]`, Linux only), or two releases would hold
  locks on different files; `removeBase` moves `$base` aside before `rm
  -rf`, so a session arriving meanwhile makes a new one. A fetch's
  children inherit the lock, so a recovery never races a fetch still
  running.
- **The image lock** (`ImagesLock` in the release root, fd 8, always
  taken after fd 9): images are the whole server's. Shared for
  fetches, exclusive for removals, which also need Docker to answer,
  else they count as busy (`il_busy` says which). Pruning skips when it
  is busy, directories included (servers are not expected to be that
  busy: revisit if skips show up); `--destroy` and `--releases remove`
  wait 10 minutes, then fail with 75. `--destroy` holds it across its
  `destroy:` step, since compose's `--rmi local` removes images too: a
  slow step makes fetches on the server wait (accepted). Docker calls
  have no timeouts: a hung daemon holds the lock until killed, and
  waiters give up after 10 minutes (accepted). The restart after a
  failed run waits too: on the new release's images it would run them
  under old code. Holders write `ImagesHolders/<pid>`, for messages
  only. Linux's `flock` lets new shared holders past a queued exclusive
  one.
- `basePrelude` exports `DEVOPSY_NO_UPDATE_CHECK=1`: the detached
  `--upgrade-check` would inherit fds 8 and 9 (compose has to keep
  them) and hold the locks while it asks GitHub.

### Images

The rules live in Go (`internal/images`), run on the server as the
hidden `--images` (`runImages`; `img` in the scripts): the server's
devopsy applies them, whichever laptop or CI wrote the script, since
the state is the whole server's (they were generated sh at first,
where four review rounds found most of the bugs). The scripts keep
what needs the shell (`releaseImages`): the locks, and fetching on a
terminal (`fetch_images`, `with_tty`). The package's comments say how
each action decides; these are the rules.

- **Fetching** (`fetchImages`, through `devopsy --`, past project
  commands): build mode `pull --ignore-buildable` then `build --pull`;
  image mode `pull`, which pulls buildable services with an `image:`
  too and fails on a missing tag. A rollback or a restart restores its
  bookmarks (`img restore`) and fetches only when it cannot.
- **Bookmarks are the reference count:** each release tags, by image id
  (another fetch may move the name meanwhile), every image it runs as
  `<repo>:devopsy-<compose name>-<id>` (`bookmarkPrefix`: a long compose
  name is cut and hashed so the tag fits Docker's 128; a repository named
  twice adds a checksum of the ref). Digests too (`prune -f` would delete
  an unused image pulled by digest), never restored. `devopsy-images`
  lists `ref<tab>image id`, what a removal decides from even after a
  bookmark went. Docker shows names short (`docker.io/library/nginx` is
  `nginx:latest`): every comparison with a name Docker reports goes
  through `familiar`.
- A release fetched again untags the bookmarks its new listing no
  longer makes and records the names it dropped for the next pruning:
  under the shared lock, beside other fetches, it never removes names.
- **A restore never changes another environment:** a name whose current
  image another compose project's container uses (`docker ps --filter
  ancestor=`, checked on Docker 29's containerd store), or a bookmark
  none of this environment's releases made holds (exact names: `app-prod`
  never matches `app-prod-eu`), stays on the newer image, said. A
  digest's bookmark must be there too, or the run step's `up` would
  pull after the switch.
- **A removal** untags the going release's bookmarks, then per image,
  from Docker's state: held by another bookmark, it stays (its names in
  repositories no holder is in, a mirror or a digest, are recorded, or
  they would outlive every release); used by a container, its names
  are recorded, never stripped (all but the last frees nothing and
  costs a pull); else its listed names and its other names in the same
  repositories go (`mariadb:11` and `mariadb:11.4`); a name that moved
  on stays. Never `-f`: a refusal is recorded (Docker 29's containerd
  store only untags with it, but the classic store, which servers
  upgraded from older Docker keep, may delete an image a stopped
  container uses; not checked). Idempotent: one cut off
  anywhere is finished by the next. Docker failing (anything but a
  missing image) stops it with exit 1 and the release stays, so the
  next removal finds its listing again (`RemoveReleases` removes a
  directory only after its images);
  `--destroy` then keeps the directory (the data is already gone) and
  says to run it again.
- No directory scanning, server-wide sweep or trash directory
  (decided): a removal decides from its release's listing and Docker's
  state, and reads nothing above its project.
- **The record** (`images.LeftFile`, `<release
  root>/<project>/.devopsy-images-left`) holds what could not go:
  `--releases` lists it, and `img prune` retries this project's lines
  only (decided), dropping one once its name is gone, moved on or
  removed. Appended in one `O_APPEND` write (two environments of a
  project record under the shared lock); only the retry, alone under
  the lock, rewrites it. `LeftoverScript` treats it like the project's
  `.env`, and `--purge` says what it forgets.
- **Undo** (`img undo`): a new release's fetch is bookmarked, so it
  goes with the release; a rollback's target is left alone (the names
  may hold the live release's images); then current's names come back.
- `images/releases.go` decides which releases pruning removes
  (`ToPrune`: older than the `keep`-th live one; current and fetching
  ones stay), the rollback target and removals by id, unit-tested on
  directories.
- **Signals:** `runImages` ignores SIGPIPE, SIGTERM, SIGINT and SIGHUP
  (its steps are short and under the lock): Go does not keep an
  inherited ignored SIGPIPE or SIGTERM, so the script's `trap ''` alone
  would let a gone session or a second TERM kill an undo halfway;
  `leftAdd` records before it says.
- **Tests:** an in-memory Docker (`images_test.go`) and
  `TestRandomHistory`, 200 random histories of two environments sharing
  `mariadb:11` and an alias, checking the rules above and that nothing
  is left once all goes. Script tests call the real `--images`
  (`realDevopsy`, built by `TestMain`) behind a fake devopsy and a
  file-per-tag docker.

### Sessions, drops and signals

- **Activations are always `-T`,** at a terminal too, with a pipe
  devopsy keeps open as stdin (`WatchEnv`); steps, the fetch and the
  run step get a pseudo-terminal from `with_tty`. One path for
  terminals and CI (decided): a `-t` session's hangup reached only the
  shell, and dash runs a trap only once the command it waits for ends
  (a build ran on, then compose hung on the gone terminal).
- **Before the switch, a dropped session cancels.** A release is
  pending from its upload until right before `make_current`: one cut
  off anywhere before is never rolled back to nor kept. `prelude`
  removes stale `releases/*.tmp` and `.devopsy-watch.*` under the lock.
- **The watcher** (`watchSession`, hidden `--watch-session`,
  `cmd/devopsy/watch.go`) reads the session's stdin in the background, in
  an activation once the release lock is held (a holder's before it). When
  stdin ends it takes the armed file (renamed to `.fired`) and sends
  SIGTERM to the script's group (when the script or its login shell leads
  one, as under sshd; else the script alone), so a fetch is undone at once
  through `fetch_sig`, not at the next write, a build later (without a
  watcher, as through OrbStack's own SSH, the fetch runs to its end and
  the script dies at its next write). The script
  disarms it by taking the file first (`mv` to `.off`) in `fetch_release`,
  under the image lock, before its last `fetch_sig` check: one rename
  wins, so a SIGTERM never lands past a switch. When the watcher won,
  nobody reads the session, so the undo writes to `ServerLogFile` (a
  `cancel.<pid>` part, which `--log` shows at the end): a write to a
  session whose buffers are full would block with the lock held. A file,
  not a signal, because a signal sent before the watcher could handle it
  would kill it. Armed: releases and rollbacks (`WatchEnv`), a release
  that syncs always (its stdin is the answer's pipe); never a sync's
  holder, which a SIGTERM would stop halfway through restoring.
- **The heartbeat** (`internal/remote/heartbeat.go`): a connection that
  dies silently (a laptop asleep, a network gone) is seen by the server
  only when TCP gives up (measured: 16 minutes with writes pending,
  hours for a quiet holder; devopsy-server never sets sshd's
  `ClientAliveInterval`, and the client's `ServerAlive*` only help the
  client). So every pipe devopsy keeps as a session's stdin (an
  activation, a holder, a push, the push's import, `--export`) gets a
  zero byte every 5 seconds, and the server's readers, told by
  `HeartbeatEnv`, take a silence for EOF: `QuietBeats` (30 seconds),
  `HolderQuietBeats` (90, behind an answer's fifo: a holder or a
  release that syncs), so a push still copying is cancelled before its
  holder decides, the operator's ssh noticing in between (one timeout
  for both let a holder restore while the push wrote into it). Data
  carries no beat: the upload's tar reads through `--stdin-guard`, a
  server's `--import` its stdin, both bounded by `DataQuietBeats` (2
  minutes, with or without a heartbeat: an export piped in may stop
  services first), and a remote `--import` opens its session with the
  data's first byte, so a slow writer never trips it. A busy operator
  (a laptop asleep, Ctrl-Z) cancels too, cleanly. Quiet times count in
  beats, so the e2e tests shorten them all. Sync hooks never get
  `SyncWatchEnv` or `HeartbeatEnv`: a hook running `devopsy --export`
  would take its own `/dev/null` stdin for a session gone.
- **SSH options:** `KeepAlive` (`ServerAliveInterval` 3 beats, 15
  seconds, `ServerAliveCountMax=3`) on activations, uploads and a
  sync's sessions; `Bounded` (`KeepAlive` plus `ConnectTimeout=30`) on
  a log's save, `--export` and `--import`, which otherwise waited
  minutes on a dead connection.
- **Ctrl-C:** devopsy takes it (`session` in `runRemote`; ssh ignores it).
  The first closes the pipe: before the switch the watcher cancels the
  release and devopsy waits (saying so after 5 seconds: the server takes
  the cancel once it holds the release lock); after it, devopsy says the
  release goes on, and a second ends the session; the log is saved either
  way. "After the switch" is `SSHStreams`' flag on `SwitchMarker`
  (`WithSwitched`), never the release log, which stops at its cap; a
  switch already under way when the pipe closed goes on, said. During the
  upload (`signal.NotifyContext`) the progress line goes and nothing
  changed.
- **An interrupted fetch:** a signal only sets `fetch_sig` (INT, TERM,
  HUP); the fetch's child gets it too and fails, and the same path undoes
  it under the image lock, with signals ignored from there on, so a second
  Ctrl-C cannot stop the undo; exit 130. PIPE is ignored from the fetch
  on: dash writes "Terminated" to the dead session. For a shell killed
  outright, `FetchingFile` marks a new release's fetch until a line says
  its images are ready, and `img recover` (by every activation,
  `--destroy` and `--releases remove`, after the lock) marks such a
  release failed, bookmarks its images and restores current's names; until
  then it counts as failed. Rollbacks never write the marker, which would
  mark a good release failed.
- **From the end of a fetch that went through, INT, TERM and HUP are
  ignored** to the script's end: a trapped signal still kills the
  foreground child (the `ln` and `mv` of `make_current`), which `set -e`
  turns into the script's end halfway through the switch. So nothing stops
  a release after its switch, the run step included (decided: stopping it
  restarted the previous release while `script`'s compose ran on, two
  `up`s on one project). `script` reads its signals through a signalfd,
  ignored ones too, and exits 0 when signaled (a restart's `get_images`
  took that for a fetch), so from there `with_tty` runs it under `setsid
  -w` (`tty_detach`).
- **255 is ssh's** for a connection that failed or was lost, and ssh
  passes a script's own status on, so activations and holders never
  exit 255 (`NoExit255`, `followOutput`'s trap: 254 instead; drush on a
  PHP error exits 255). The operator's lost-connection messages
  (`sshLost`) then mean what they say.
- **`remote.Protocol`** (4) is what the scripts need from the server's
  devopsy, reported by `--release-settings`: `basePrelude` refuses an
  older one before anything changes, for every command, read-only ones
  too, and names the upgrade. Raise it when a script starts calling
  something an older devopsy lacks, and say what in its comment.

### From the switch on

A release goes live, or fails and brings the previous one back, whatever
happens to its session. Right after `make_current` (after the sync, in a
release that syncs), `followOutput` sends the script's output to
`ServerLogFile` (the deploy user's alone, umask 077; both streams, each
activation's part after a `LogSection` line with its time and a tag) and a
GNU `tail -f` copies it to the session, so a session gone kills only the
tail, never the shell, compose mid-`up` or the switch back.
`SwitchMarker`, printed just before, tells the operator's devopsy to stop
logging the session and log `LogAnchor` there (`LineMarker` with `Cut`, on
logged sessions only: others pass through untouched, a shell's typing and
an export's data). Activations and holders print on one stream (`exec
1>&2`): on two pipes a line on stdout could reach the log after the marker
and be cut. The tail ends half a second after the shell (an EXIT trap), or
a second after a helper finds the shell gone without its trap: never `tail
--pid`, which takes an unreaped zombie for alive. A release that syncs and
a sync's holder follow from the run step: the sync has its own way through
a drop, and the copy's lines reach the session's log through the push.
Without GNU's `tail` (macOS, in tests) only the traps apply
(`TestAfterSwitchSurvives` skips).

### Logs

Two per release, never overlapping, neither rewritten, pruned with it:

- The session's (`LogFile`, `devopsy-log-session`, `releaseLog`):
  devopsy's messages, local steps' command lines and exit codes, and
  the sessions' output up to `SwitchMarker`, each line after its UTC
  time, cleaned (`cleanLines`: no terminal codes or empty lines, and
  only the last frame of a progress display: a `cursorUp` code
  supersedes the lines before it), saved by `LogSaveScript` once the
  server was reached. A rollback's goes to the restored release's log
  (its "rolling back to <id>" line), a sync's to the current one's
  ("syncing into"). Local steps' output is not piped: it would lose
  its terminal.
- The server's (`ServerLogFile`), raw, from each switch on.

`--log` (`LogReadScript`) joins them for display (`joinLogs`): each server
part after the session's anchor with its tag, cleaned the same way and
indented; never by time (two machines' clocks, lines of the same second).
Parts no anchor claims (an operator gone before saving) follow at the end
under their `LogSection` time, never under a later anchor.

### Data, destroying and removing

- `.devopsy/mnt/` is devopsy's one data directory (README, "Data and
  mounts"): never uploaded, `shared/mnt` created by the upload and linked
  like every `shared/` entry. Its contents and their ownership are the
  project's: only `--destroy`, an `--import` (its entries) and a sync into
  a child remove anything in it.
- `--destroy` (`DestroyScript`, both locks; asks for the target's name
  unless `--yes`): the hidden `--destroy-data` (`runDestroyData`) in
  `current`, else the newest release: the environment's `destroy:`
  step if any (a hook, while everything is up), then `compose --profile
  '*' down --volumes --remove-orphans --rmi local` and `mnt` emptied as
  root in `UtilImage` (a plain `docker run`: `compose run` would bring
  the network back); then every release's images; then `removeBase`.
  A failure before the removal removes nothing. Then `purgeLeftovers`:
  levels' directories left with only their `.env` (`LeftoverScript`),
  removed with `--purge` or a yes at a terminal, else a `report.Warn`
  with the command.
- `--releases remove <id|failed>... [--yes]` (`runReleases`): lists
  first, resolves every word (`chooseReleases`: exact id, else a unique
  prefix; `failed` is every failed or rolled-back release, and an
  INCOMPLETE one older than current) and refuses all when one names
  nothing, several or the current release, then asks, `--yes` without a
  terminal. On the server (`ReleasesRemoveScript`) the same checks
  again, `--gone-ok` for `failed` (a release pruning took meanwhile),
  and `img prune` once everything went. Completion never offers ids
  (the server's).
- An environment's directory with `.devopsy/` and no `current` is a
  plain directory: commands run there, releases refuse before creating
  anything. A `current` that is a directory (a failed release's
  containers restarting, Docker recreating their bind mounts as root)
  is refused by `prelude`, which says how to clean it up.
- `--vars`, `--vars-instance` and `--vars-project` (one file each, so
  flags, not options of `--vars`) send values over SSH's stdin, never
  as arguments; edits replace keys in place, under the release lock
  (deploy commands write secrets too).

### Syncs

- **Parents:** `parent:` names a concrete environment (a pattern is an
  error; one naming itself has none, so `defaults: {parent: prod}`
  works; a cycle is an error: neither could be released first, a first
  release syncing from a released parent). It resolves one level only
  (`ParentTarget`, whose own stays nil): at the child's instance, its
  server from the usual variables only, never the child's (a guess
  would copy the wrong data). Strict loading records an unresolvable
  parent in `ParentErr` instead of failing: only `--sync` and a
  `--release` that syncs fail with it, so every other command works on
  the child. `DescribeTarget` resolves it leniently.
- **When:** a child's first release (`CurrentExistsScript` prints
  `current` or `none`, then `incomplete` on a partial copy: one extra
  session on every release of a child), `--release-sync` (any release),
  never `--release-no-sync`, and `@<child> --sync`, which asks for the
  name like `--destroy` and reruns `run`. `--from <address>` (`@`
  optional) on `--release`, `--release-sync` and `--sync` copies from that
  address instead of `ParentTarget`, `ParentErr` ignored: a child's only,
  and on `--release` only a first one (a released child says to use
  `--release-sync`); `--release-no-sync --from` is an error. `failParent`
  lists the fixes: the variable, the same release with `--from`, and a
  release without the data then `--sync --from`.
- **Preflight:** everything that can fail (`syncPreflight`: a run step,
  the parent released, across servers a local agent that has keys
  (`agentHasKeys`, `ssh-add -l`: `SSH_AUTH_SOCK` set is no agent) and a
  child reachable without `ProxyCommand`/`ProxyJump`) is checked before
  the upload or the holder.
- **Two sessions,** both from the operator (`runSyncSession`): the
  holder on the child's server (`HolderScript`, or `ActivateScript` with
  `SyncOptions` between `make_current` and `run`) takes the release
  lock, runs prepare, and prints the marker (`SyncMarker`, `$base` and
  the sync's id: the operator does not know the release root), taken
  out by a `LineMarker` on stderr. Then the push on the parent's
  (`PushScript`: shared `flock -s` on its release lock, exclusive on
  `.sync-lock` so syncs from one parent serialize, `devopsy
  --sync-push`), with `ForwardAgent=yes` across servers. The push's
  exit gives `go` or `abort` to the holder; the holder's exit is the
  command's. Both ssh processes ignore SIGINT
  (`remote.WithoutInterrupt`: a terminal's Ctrl-C reaches the whole
  foreground group, and a `-T` ssh quits on it), so devopsy alone ends
  them, in order.
- **What the copy did decides, not the answer** (an answer lost after a
  complete copy once restored the child on the parent's data
  unsanitized, and one lost mid-copy ran it on a mix). The holder opens
  a gate before its marker (`SyncOpenFile`: the parent's address and
  the sync's id, which the push passes on as `SyncIDEnv`, so a write
  left from an earlier sync never claims a newer gate) and closes it
  (`rm`) right before deciding. The child's side of the push claims it
  right before its first write (`claimWrite`, `ClaimWriteScript`: a
  rename to `SyncStartedFile`, one of the two wins; the same-server
  rsync, `SyncReceiveScript` across, an import in `syncImport`), so a
  push that outlived its connection writes nothing once the holder
  decided, and writes `SyncDoneFile` after its last write (the rsync's
  when no stream follows, else the last import's, `import <service>
  last`), on the child, never through another session. The holder
  (`syncBlock`) decides once its read of the answer ends, however it
  ends: done is `go` (an `abort` after it too), started alone
  `partial`, neither the answer (`go` with nothing written clears an
  earlier mark too; `abort` on a marked environment stays `partial`);
  then removes both files.
- **The partial mark:** `--sync-services finish partial` leaves the
  `stop=true` services down and writes `SyncIncompleteFile`; `finish
  go` removes it, and a failing `finish child go` writes it (a sync
  completes with its finish; the hook decides what stays up). While it
  is there nothing runs on the data: `partialGuard` refuses a release
  that does not sync and a rollback, the restart after a failed release
  is skipped, `CurrentExistsScript` says `incomplete` so the operator
  refuses before uploading (`--release-no-sync` included: a sync is the
  fix, `--release-sync` when the sync itself needs a code fix), a push
  from it refuses (a chain), `--export` warns, `--status` shows it. A
  holder killed before deciding leaves the files: `--sync-services
  settle`, run by the next sync's prepare and by `partialGuard`, counts
  them as not completed (marked, `finish child partial`); a sync's
  files while nothing holds the lock (`LockBusy`) are a leftover, a
  running sync is not. A late write (an import running on in its
  container after the holder decided) is settled the same way. Labels
  bound the promise: only `stop=true` services are kept off partial
  data.
- **fd 3:** the holder reads devopsy's answer on fd 3, a fifo the
  watcher writes (`--answer-fd`; both ends opened before it starts, so
  no open ever waits), and gives every command `/dev/null` as stdin, so
  nothing it runs can eat or wait on the answer. Without one (devopsy
  interrupted, the connection lost), the holder writes its output to
  `$base/.devopsy-sync.log` instead (trimmed past `SyncLogMax` to
  `SyncLogKeep` lines, on both sides: a write to the dead session would
  kill what restores the services) and finishes as the files say. From
  prepare until the services are restored it ignores SIGPIPE, and it
  restores before it reports.
- **Interrupts** (SIGINT, SIGTERM in `runSyncSession`): before the
  marker devopsy ends the holder's session (`endHolder`) and waits for
  it: a release that syncs is cancelled by its watcher, a holder
  restores on its own. After it, the push's ssh gets SIGTERM
  (`remote.SSHContext`, SIGKILL and its output cut after
  `SSHKillDelay`) and is waited for, then the holder gets `abort` and
  finishes as the files say; devopsy exits 130, or 0 when the copy had
  completed and the holder went on. During the run step after `go` the
  first Ctrl-C only says the step goes on on the server; the second
  cuts the session (`followOutput`). The holder ending first (its
  connection dropped) is the same select: it restored the child on its
  way out, so the push is cancelled, nothing answered, and the sync
  fails.
- **The push watches its session:** its stdin is a pipe devopsy keeps
  open (`SyncWatchEnv`), so `--sync-push` sees the session's end (EOF,
  or a heartbeat's silence) at once, not at its next write, which a
  silent copy may not make for minutes. That, or SIGINT, SIGTERM,
  SIGHUP or SIGPIPE, cancels it: the same-host rsync container is
  removed (`dockerRun`: killing the docker client leaves the container
  copying), the `-R` session ends, a running export (its own process
  group, `runSyncHook`) and import get SIGTERM, spools go, no export
  starts, and the parent's services are restored. From then on `errOut`
  also appends to the parent's `$base/.devopsy-sync.log`
  (`quietWriter.keep`). What runs inside a container on its own (an
  import already in `docker exec`) runs to its end. Servers' commands
  write to `errOut`, never an `*os.File`, so a gone session cannot kill
  them with a broken pipe. The tests' fake ssh runs through dash on
  Linux, which never execs the last command of `sh -c`: cancelling
  closes the pipe, as sshd would, rather than signalling ssh.
- **Labels opt in:** `devopsy.sync=rsync|stream` (`cli.SyncLabel`); no
  label or an empty value is `skip` (neither copied nor touched), any
  other value an error (`live`, `pause`, `stop`, `custom` went, with
  `push` and `flush`: no migration). `devopsy.sync.stop=true|false`
  (`SyncStopLabel`, default false, ignored without `devopsy.sync`)
  stops the service on both sides for the whole sync, a stream
  included (on the parent, downtime for the copy: the project's choice,
  a stream without it the alternative). No `pause` (Gotchas).
  `devopsy.sync.rsync.exclude` (`SyncExcludeLabel`, entries of `mnt`,
  comma or space separated)
  drops entries from an rsync service's `Dirs` at parse time, so every
  consumer (copy, guard, list) agrees; on a `stream` service it is
  ignored, not validated, so a variable can flip `rsync`/`stream` and
  keep it. A `.` entry (`./mnt` itself) subsumes the rest, other
  services' entries included: `--sync-push` says so (`SyncWholeNotice`,
  yellow when the operator's terminal takes colors, `ColorEnv` passed
  with the push's variables), only a notice: it never excludes by
  itself. Nothing labeled, or nothing to copy (no
  entries, no streams): "nothing to sync", success, the parent's hooks
  skipped, and the run step still runs.
- **The child guard:** each side reads its own labels. Before the
  parent changes anything, `--sync-push` asks the child for
  `--sync-services list` (through `--sync-exec`, one more session across
  servers) and refuses (`syncConflicts`) when a child service mounting a
  copied entry is running or restarting and not `rsync` there (a
  `stop=true` one is stopped already), and (`streamMismatches`) when a
  parent `stream` service is not `stream` on the child, whose import
  would refuse it after the rsync, or a child `stream` service is
  `rsync` on the parent. A label from an inherited variable cannot lag
  (the child reads the pending file, inherit below); one in the
  child's own `shared/.env` can still differ. The list's columns: the
  service, its label (`rsync`, `stream` or `skip`, with `+stop`), its
  container's state (`none` without one), its entries of `mnt`.
- **Order, the child's side** (`--sync-services prepare`): `settle`,
  the state record, the import check, the `prepare child` hook, then
  one `compose stop` of its `stop=true` services (Compose orders them
  by `depends_on`, dependents first: keep it one call). `finish
  go|abort` starts them and waits until healthy, `partial` leaves them
  down; spools are removed unless `go`; then `finish child` with the
  word. Stopped services stay down through the imports (no app on a
  half-imported database).
- **Order, the parent's side** (`runSyncPush`): the checks (labels, the
  `export` script when a service is `stream`, the child guard, the util
  image) before anything, then `prepare parent`, on the same server a
  refusal when the child's gate is gone already (its holder decided),
  stop, rsync, streams, start (healthy), `finish parent go|abort`.
  A failing prepare aborts before its side stops anything.
- **Hooks** run from Go (`runSyncHook`, `--capability sync ...` in its
  own process group), only where their file is (`export` and `import`
  are required for a `stream` service), with `DEVOPSY_SYNC_RSYNC`,
  `DEVOPSY_SYNC_STREAM`, `DEVOPSY_SYNC_STOP` (`cli.SyncLists`, that
  side's labels) plus `DEVOPSY_SYNC_FROM_TARGET`,
  `DEVOPSY_SYNC_TO_TARGET` and `DEVOPSY_SYNC_ARGS`. The holder's
  scripts (`syncPrepare`, `syncFinish`) only call `--sync-services`.
- **The state record:** stopping records what it stops in
  `$base/.devopsy-sync.services` (`syncState`, `stop <name>` lines;
  `startSyncState`: the child's prepare creates it before its hook,
  `stopServices` before its first `ps`), because no record means
  restore every `stop=true` service, one down on purpose included. One
  left by an earlier sync that could not restore is kept and added to
  (`syncRecord`, rewritten whole). Restoring brings the recorded ones
  back from container state (exited), so it also recovers after an
  interrupted sync, and a service down before the sync (a maintenance
  stop) stays down. It waits for every service it started to be
  running and healthy (`waitHealthy`: one gone fails), and removes the
  record once all are back. Service states come from one `docker
  compose ps --format json` (`serviceStates`, `servicesIn`); the sync
  tests' fake docker remembers stops and starts to answer it.
- **A child's first release:** no container at all when prepare
  started is a `first` line in the record (a stream's import creates
  its container before finish looks; a later sync that finds
  containers drops it). Before the finish hook every labeled service is
  created in one `up -d --wait`, since the hook sanitizes through them
  (drupal11's needs the app: without it, prod's passwords reached a
  child), and a stream imported then stays up. Only on go: an aborted
  first sync creates nothing. Never otherwise: a missing service was
  removed on purpose.
- **Streams:** per `stream` service, in `depends_on` order over every
  service (`SyncStreamServices`: dependencies first, ties alphabetical,
  a cycle an error), the parent runs the capability's `export
  <service>` (`syncStream`) into the child's spool,
  `$base/.devopsy-sync.stream.<service>` (`remote.StreamSpool`; every
  file a sync leaves in `$base` starts with `.devopsy-sync.`): on the
  same server directly, across servers through `cat >` in an SSH
  session to the child (`exportInto`). Only once the export exited 0
  does the child's `--sync-services import <service>` run: the service
  brought up (`upFor`) unless `stop=true` (down again after if it was
  down), `import <service>` with the spool as stdin, the spool removed
  whatever happens. Never a pipe into the import: a cut export never
  reaches it. The import's stdin is a pipe the push keeps open, so
  across servers the session's end, which a remote process never gets
  as a signal, cancels it too.
- **Rejected with these labels** (October 2026): keeping a `push`
  script with helpers (a push only works from the parent's server to
  the child's, never for a file, a laptop or a relay; `export` and
  `import` split the same freedom and are reused by `--export` and
  `--import`); export and import commands in compose labels (scripts do
  not belong in labels, however short); `stop` as a value and a
  sub-label at once, or a hold sub-label for streams only (two
  orthogonal labels instead); a dump into an entry of `mnt` as
  devopsy's way (at rest on the parent, twice the disk, cleanup and
  completeness the project's again: still fine for a project that wants
  dumps on disk anyway); an `--export` that never stops a service (the
  scripts are written for the label: a `run --rm` export against a
  live service fails or copies inconsistent files).
- **Hidden flags** (dispatched in `main.go` before anything else):
  `--sync-push`, `--sync-exec <devopsy args>`, `--sync-services prepare
  | finish go|abort|partial | settle | import <service> [last] | list`.
  The first two need the push's `DEVOPSY_SYNC_TO_DIR` and
  `DEVOPSY_SYNC_TO_PROJECT`. `DEVOPSY_SYNC_ARGS` (space-joined, split
  like `StepArgs`) go to rsync, and to the hooks to read.
- **A clean environment:** on the same server the child's devopsy
  (`--sync-exec`) runs with `cleanEnv`: the machine's variables, the
  sync's own (addresses, arguments, id, watch, heartbeat, the pending
  inherit file through `childInheritEnv`), `DEVOPSY_VERBOSE`, Docker's and
  devopsy's settings, and the child's `COMPOSE_PROJECT_NAME`: the caller's
  environment wins over a project's `.env`, so the parent's secrets and
  project name would otherwise reach the child's commands. Across servers
  `SyncExecScript` exports the sync's own (`syncExecEnv`),
  `DEVOPSY_VERBOSE` comes through the call, and the rest is the clean
  login environment.
- **SSH between servers:** through `DEVOPSY_SSH_COMMAND` too, with
  `ChildSSH` on the parent's sessions to the child: `ForwardAgent=no` (the
  operator's agent reaches the parent, never the child, whatever the
  parent's ssh config says; the parent can use every key in it for the
  session, prod's included when a chain's middle environment is the
  parent, which only the operator's agent limits: README), `BatchMode=yes`
  (a refused key fails instead of prompting), `ConnectTimeout=30` (an
  unreachable child fails before the TCP timeout, the parent's services
  stopped meanwhile) and `StrictHostKeyChecking=accept-new`. The rsync
  session (`-R` to the parent's daemon, `--network host` on the child)
  passes `ExitOnForwardFailure=yes` and never `ClearAllForwardings`, which
  clears command-line forwardings too (`ssh_config(5)`); a taken port
  retries another (3 tries, 20000-60000). `--sync-exec`'s session and a
  stream's `cat` keep `ClearAllForwardings`. All of a sync's sessions pass
  `KeepAlive`: the holder is quiet for the whole copy. No agent on the
  parent with `DEVOPSY_SYNC_TO` set means a `restrict`ed key or sshd's
  `AllowAgentForwarding no`: say so. A `restrict`ed key also refuses the
  `-R` (`port-forwarding`), which sshd reports like a taken port: after
  the last try, say both.
- **Transport:** rsync always in `devopsy-util:<n>` (`UtilImage`, now
  `:1`; built on each host from `UtilDockerfile` when missing; GNU tar
  for `--export` and `--import` too), as root with `--numeric-ids`:
  owners and modes survive, the deploy user's permissions do not
  matter. Bump the tag whenever the Dockerfile changes: hosts never
  rebuild an image they have. Entries are the top-level entries of
  `mnt` that `rsync` services bind-mount (`docker compose --profile '*'
  config`, lexical and resolved paths; `.` subsumes the rest); one
  missing on the parent is skipped with a notice, the child's left as
  it is. An entry that is a file on the parent is copied as one
  (`--inplace`, so the container bind-mounting it sees the new content,
  a bind mount being the file's inode; across servers its module is
  `/mnt` filtered to it). Across servers the parent's daemon serves one
  module per entry being copied (`rsyncdConf`, `RsyncModule`: the
  entry's name, `mnt` for `.`), so nothing else under `mnt` can be asked
  for. rsync's exit 24 (files vanished during the copy) is a notice
  (`RsyncVanished`); any other failure aborts.
- **inherit:** `inheritVars` reads the parent's three files in one
  session (`InheritReadScript`), takes each key from the nearest level,
  and writes them all as the child's `shared/inherit.env`
  (`InheritWriteScript`, on stdin, under the child's lock, the whole
  file: a resync replaces it), which every release links and the loader
  reads below `.env`. Notices name the keys the child's own
  `shared/.env` shadows, never values. `--inherit` (`runInherit`) runs
  it alone. A sync inherits before its holder starts: a first release
  into `shared/inherit.env`, so `--missing-vars` sees them (kept on
  failure: the next release is first again); a re-sync into the pending
  `$base/.devopsy-sync.inherit.env` (`remote.SyncInheritPending`),
  which everything the sync runs on the child reads in place of the
  release's (`DEVOPSY_INTERNAL_INHERIT_FILE`, exported by the holder
  and set by `--sync-exec`), so the child's labels follow an inherited
  variable in the same sync (written after the copy, they lagged one
  sync). `finish go` moves it into place (`mv -Tf`) before restoring
  services, the hook and the run step; anything else removes it
  (rejected: writing `inherit.env` in place with a backup restored on
  abort, since a holder killed outright leaves the new file over the
  old data). A
  holder told none was written (`SyncOptions.Inherit`) removes a stale
  one first.

### --export and --import

- `--export <service>` and `--import <service> [--yes]`
  (`cmd/devopsy/data.go`, scope `[@t]`) move one labeled service's data
  the way its label says; an unlabeled service is refused. `stream`:
  the capability's `export` and `import` (`runSyncHook`, the lists in
  their environment). `rsync`: a tar of the service's entries of `mnt`
  (exclude honored, file entries included) made in `UtilImage` as root
  with `--numeric-owner`. An import lists it first (`tarEntries`: a
  truncated tar fails `tar -t` before anything changes, the spool must
  end with two zero blocks, which every tar writer adds and `tar -t`
  does not require, and entries the service does not mount are
  refused, an excluded one by its label), then empties each carried
  entry, never removing it (for `.`, every entry's contents and the
  files beside them: a running container bind-mounts the directory's
  inode), and extracts with `--overwrite`. `stop=true` stops the
  service around either and restores it (`stopFor`), interrupts
  included: the label is the contract. A `stream` import of a service
  not stopped brings it up and down again if it was down (`upFor`). A
  service mounting `./mnt` itself carries all of `mnt`, said
  (`wholeMntNotice`).
- stdout is the data, so `--export` refuses a terminal; `--import`
  refuses a terminal as stdin, spools it whole first (`importSpool`: in
  a release the sync's `$base/.devopsy-sync.stream.<service>`, locally
  a temporary `.devopsy/.devopsy-sync.stream.*`, which `excluded` keeps
  out of releases), refuses an empty one, and only then stops anything.
  The spool always goes.
- On a target the server runs the same code: `ExportScript` (shared
  release lock and exclusive `.sync-lock`, like the push: both stop and
  restore the same services; stdout through as it is, never `Colorize`d)
  and `ImportScript` (exclusive lock, `devopsy --import <service> --yes`).
  The local side asks for the environment's name on `/dev/tty`
  (`confirmImport`: stdin is the data; no terminal and no `--yes` fails),
  then sends stdin and, only after a clean EOF, an end line with a random
  nonce (`ImportEndEnv`): a session cut halfway reaches the server as a
  plain EOF, so the server requires the line and takes it off. A pipe from
  a failed export still looks complete: README says to go through a file
  for what matters.

## Roles, exports and imports

- Labels for facts shared between projects on a host (`roles.go`):
  `devopsy.role` (one compose project per host),
  `devopsy.export.<KEY>`, `devopsy.import.<VAR>=<role or
  project>/<KEY>[?]`. devopsy knows no role or key names; `proxy` and
  `WILDCARD_DOMAIN` are the Traefik template's convention. devopsy's
  core defines no proxy contract (a `domains` contract in the core was
  shaped by one pairing, the Traefik template and its projects).
- Imports resolve on the host at each release, never at runtime in a
  container: compose interpolates labels (a Traefik rule) before any
  container exists.
- `--prepare-release` checks the role and writes imports into the new
  release's `devopsy-target.env` (after `cli.ImportedMarker`) before it
  goes live; an import never overrides what the environment sets (even
  empty); an empty export is a value (else every project importing
  without `?` would fail under a proxy without a wildcard); rollbacks
  never check (it would fail a rollback exactly when one is needed,
  the proxy down). Exports
  are read from running containers (`Running`: one snapshot per
  lookup), waiting up to `WaitForExporters` (30 seconds) for required
  sources.
- `--debug imports` compares every running project's imports with its
  release's `devopsy-target.env` (found through compose's working_dir
  label).
- Roles are advisory: anyone in the docker group is root-equivalent.
  Checked only at releases, never on a local `up` (a test proxy beside
  a main one is normal locally), and two releases claiming one role on
  different targets at once are not locked against each other
  (accepted).

## Other features

- **`--status`** (`cmd/devopsy/status.go`): locally, `docker compose
  ps --all --format json` as a table, then the `status` capability's
  `show` with stdout through. With a target, `StatusScript` prints
  header lines (the release's record, releases kept: live ones, as
  `keep` counts them, the last copy that went through from the session
  logs' `SyncCopied` lines, a partial or running sync from
  `partialStatus`), `---`, then `devopsy --status` in the current
  release; the operator formats the header (`ago`) and prints the rest
  as it came.
- **`--ssh-keys`** (`cmd/devopsy/sshkeys.go`, scripts in
  `internal/remote/keys.go`) edits the deploy user's own
  `~/.ssh/authorized_keys` over the usual session: no root, so
  devopsy-cli stays out of host setup except for this file, which that
  user owns anyway. Lines are parsed locally (`ParseAuthorizedKey`:
  options, type, blob, comment; the SHA256 fingerprint is computed from
  the blob, no ssh-keygen needed) and edited on the server under a lock
  next to the file, by blob, so a comment never identifies a key. `add`
  refuses a duplicate (exit 3), `remove` the last key (exit 4;
  commented-out keys are none, and devopsy refuses locally first).
  `--restricted` on `add` is `RestrictedKeyOptions`, the one place that
  line is written. `--ssh-keys-generate [<name>] [--out <file>]` is
  local, its own flag: just `ssh-keygen`, nothing sent; the `.pub` is
  added per server.
- **Completion** lives in Go (`internal/complete`); the scripts only
  pass words to the hidden `--complete` and show what it prints,
  cobra's protocol (`value<TAB>description<TAB>group` lines, then
  `:<directive>`), so compose's own completion passes through. It never
  connects to servers; after `@` it offers `~/.ssh/config`'s hosts as
  `@<host>:` (`sshHosts`: `Host` lines and `Include`s, patterns
  skipped, read in Go, not delegated to the shells, so every shell gets
  the same), with no space when only those are left. `--from` offers
  the same addresses (`addresses`), aliases aside, always with their
  `@`. New flags go in `cli.Flags`, subcommands in its lists.
  `TestCompletionShells` runs the scripts in every installed shell
  (bash, macOS's bash 3.2, zsh with `_describe` stubbed, fish); CI
  installs zsh and fish. bash splits words at `:`, so its script
  rebuilds them from `COMP_LINE`.
- **`--debug`** explains what devopsy sees; targets carry `From` (each
  value's origin), and `Env` records every definition of a variable as
  it loads (`EnvDef`, `TraceEnv`: `--debug env`), so the trace is the
  loading itself, not a second implementation of its precedence. A new
  source of variables records its definitions too. The capability
  catalog (`cli.Capabilities`) lives with the code that calls it.
- **Capabilities** (`--capability <name> <action>`, hidden):
  executables in `.devopsy/capabilities/`, run in the project's own
  environment and SSH session, so no other project's variables leak in.
  Define one only when a second use needs it.
- **devopsy knows no URL.** It always sets `COMPOSE_PROJECT_NAME`
  (locally the config's `project:`, else compose's `name:`, else the
  folder) and runs the `env` capability (`computeEnv`) before every
  command but help and completion: projects derive hosts, rules and
  URLs themselves, from the name and imported facts. Its output only
  fills unset variables; `DEVOPSY_INTERNAL_ENV_COMPUTE` keeps a devopsy
  it calls from running it again.
- **`DEVOPSY_INTERNAL_*`** are devopsy talking to devopsy (`COLOR`,
  `TTY`, `ENV_COMPUTE`, `WATCH`, `SYNC_WATCH`, `SYNC_ID`, `HEARTBEAT`,
  `HEARTBEAT_EVERY`, `COMMAND_TARGET`, `IMPORT_END`, `INHERIT_FILE`):
  never documented as settings, free to change in any release. Settings
  people set and the contract projects read keep plain `DEVOPSY_*`
  names.
- **`--probe`** is a tool with plain arguments, not a contract: it
  knows no proxy. A server never drives local devopsy (devopsy execs
  ssh and cannot watch its output, and a server must not make a laptop
  act): proxies print the `--probe` line to run.
- **SSH:** every session is `ssh [DEVOPSY_SSH_COMMAND's options]
  [SSHOptions] -T|-t <host> 'sh -c <script>'` (`remote.SSHLog`), `-t` for
  any command at a terminal (a shell, a project or compose command,
  `--upgrade`; activations and holders never):
  `ClearAllForwardings=yes`, since
  the user's `LocalForward` lines clash between sessions. Anything
  faster (connection sharing) is the user's SSH config, which
  `--ssh-config` suggests and never writes. Test fakes of ssh run their last argument.
- **`--context-hash`** is a pure helper; devopsy builds nothing. Errors
  only ever go towards a new hash (a kept file costs a rebuild, a
  dropped one would reuse a stale image), hence Docker's own matcher
  and refusing what it cannot hash.
- **`--upgrade`** reads the same release files as `install.sh` (archive
  names without version, `checksums.txt`). The daily check runs
  detached (`--upgrade-check`), since devopsy execs right away; only
  release builds upgrade or check.

## Gotchas

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
- `docker pause` flushes nothing (rsync sees what the kernel has, not a
  paused process's buffers), and Traefik 3.7 drops a paused container's
  router the next time it reads the containers (devopsy-traefik's
  AGENTS.md); a stopped routed container loses it at once (404). Hence
  no `pause` label: `devopsy.sync.stop=true`, or a stream's dump.
- OrbStack machines are `ProxyCommand` hosts from macOS: syncs across
  servers need hosts reachable directly (`ssh -G` without a proxy),
  like `<machine>.orb.local` with a key of your own. Their
  `/etc/ssh/ssh_config.d/10-orbstack.conf` sets `IdentityAgent`, which
  hides a forwarded agent from the parent's `ssh`
  ("get_agent_identities: Permission denied"): remove it on test
  machines. OrbStack's own SSH also keeps stdin open and leads no
  process group: the watcher needs a real sshd (e2e).
- macOS hides what Linux servers do: dash as `sh` never execs the last
  command of `sh -c` (so a signal to it does not reach that command),
  and two releases can land in the same second.

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
last run that passed (`e2e.yml`); not part of a release yet. Drop tests
shorten the heartbeat (`E2E_BEAT`, default 1, passed as
`DEVOPSY_INTERNAL_HEARTBEAT_EVERY`: quiet times and the operator's
`KeepAlive` count in beats, which keeps their order); `E2E_BEAT=5`
runs everything with the real timeouts, and one test always does. The
test server's deploy user has `/bin/sh` (dash) as login shell, stricter
than devopsy-server's bash (it found the watcher signalling only the
script there). A scenario a review finds goes there.

```sh
go test -tags e2e -count=1 -v -timeout 25m ./e2e   # E2E_KEEP=1 keeps the containers
E2E_BEAT=5 go test -tags e2e -count=1 -v -timeout 40m ./e2e   # the real timeouts
```

What only a real setup shows (Traefik, certificates, the templates) is
still by hand: an OrbStack Debian 13 machine (`orb create
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
Conventional commits (`feat:`, `fix:`, `docs:`, `build:`...).
