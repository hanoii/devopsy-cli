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

- **Compose plus environment variables.** devopsy never rewrites a
  project's compose files; everything it computes shows in `devopsy
  --env`. An activation adds one file of devopsy's own,
  `devopsy-compose.yaml` beside the release (`images.ComposeFile`),
  holding only facts of that activation (the image each service was
  given, an image held by digest), shown by `devopsy -- config`. Why: a
  project stays a plain compose project anyone can read and run, and
  devopsy adds no format of its own to learn or debug; the file exists
  because a hold by digest is the one fact compose can only take as a
  service's `image:` (decided 2026-10-23). Cost: what compose cannot
  express through variables or labels is a project's command or
  capability, never devopsy's; a plain `docker compose` on a server
  lacks the file. If a feature needs a project's choice in that file,
  it is the wrong feature.
- **Nothing is frozen before 1.0.0.** One person uses devopsy and runs
  every server, so anything may change when the change makes it better
  in the long run, the basics included (project discovery, `.env`
  loading with compose's parser and the caller's environment winning,
  `commands/` with `DEVOPSY_PROJECT_DIR` and `DEVOPSY_COMMAND_NAME`,
  the recursion guard, everything else passed to compose). Propose such
  changes rather than work around a design. No migration code: older
  files and syntax just stop working, with an error that says what to
  do; a breaking change is marked (`feat!`, `fix!`); a protocol bump
  or a new feature means upgrading the servers. A 1.0.0 is when
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
  and an older server refuses before anything changes what needs what
  it lacks (`Protocol`, features). Why: a laptop's checkout may be another branch
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
  roles, `devopsy.data`); capabilities are for behavior a label cannot
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
  (`data.go`), the owners of `mnt`'s entries and the mounts check
  (`mnt.go`), the watcher and the heartbeat (`watch.go`,
  `heartbeat.go`, Sessions, drops and signals), release logs
  (`releaselog.go`), `--releases` and `--images`; tests run the built
  binary against a fake `docker` and `ssh`.
- `internal/cli/`: finds the project, loads `.env` and decides what to
  run (`Build` returns a `Plan`; it only runs helpers, like the `env`
  capability, never the command); `cli.Flags`; roles (`roles.go`);
  capabilities; the `devopsy.data` labels and their stop sets (`sync.go`).
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

- **The server's devopsy must have what a script calls:** a number for
  broad changes, features for narrow ones, both reported by
  `--release-settings` (`protocol=`, `features=`) and checked by
  `basePrelude` before anything changes, read-only commands too, naming
  the upgrade. `remote.Protocol` (8) is for what every script depends
  on (the prelude, `--say`) or a format both sides share that no
  script's text shows: say what in its comment. Anything else is a
  feature: every devopsy flag a script calls (`featureFlags`; compose's
  flags, which a step or the operator's command may start with, as in
  `--profile web up`, are none: no devopsy reports them) and named
  behaviors of one that came later (`namedFeatures`, found by what the
  script calls). A script's needs are read from its own text (`Needs`,
  sent in front of it by `SSHStreams`), so they never drift, and
  `TestFeaturesCoverScripts` fails on a flag no feature lists. Why: one
  number gated every command, so a sync-only bump stopped releases and
  `--status` on servers that could run them; an older server now
  refuses only what needs what it lacks, by name. Across servers the
  parent's devopsy, which may be newer than the operator's, checks the
  child's the same way (`serverCheck`) on every session it opens there
  (`ChildCheck`, `childSSHArgs`), the first one (the look at the
  child's services) for the needs of all of them (`childNeeds`,
  `SyncNeedsEnv`): an import or the receive refusing on its own came
  after the parent's services stopped. `DEVOPSY_PROTOCOL_CHECK=warn`
  (`ProtocolCheckEnv`, exported to every session, so a push passes it
  on) makes either refusal a warning and goes on: the operator's own
  risk, for a local build ahead of the servers. A server with no
  `features=` is older than 8, so only the protocol's warning shows.
- **The data labels are a named feature, `data-labels`, not a
  protocol bump:** an older devopsy reads `devopsy.data` as no label (a
  parent's "nothing to sync" that a holder takes for `go`: a child's
  first release live on empty data), so every script that reads labels
  needs it: `--sync-push`, `--export`, `--import` (and `--resolved`
  through it), `--sync-services prepare|finish|import|list`, quoted or
  inside `with_tty`'s quotes (a project's remote step calling
  `--export` is gated, rightly). Not `settle`, which every activation
  calls (`partialGuard`) and which only warns on labels it cannot read.
  A bump would refuse every release, `--status` and `--log` on a server
  lacking only the labels. What refuses before anything changes: the
  parent's `CurrentExistsScript` carries the push's needs (`extra`,
  before the holder stops the child's set); the child's prints whether
  its server has the feature, and the operator refuses only a release
  that will sync (one that does not goes on); `childNeeds` always adds
  it, so the child's first session refuses for all of them. Not
  detected: a project command running `devopsy --export` itself (it
  fails loudly, without the upgrade line).
- **The owners in copies are a named feature, `mnt-owner`,** found by
  `--sync-push`, `--import` and `--sync-services list`: an older parent
  would rsync the parent's owners, an older child list none, an older
  server's import keep the tar's. `childNeeds` adds it as it does
  `data-labels`. A release's owners and mounts check are flags of their
  own (`--mnt-owners`, `--check-mounts`), so an activation on an older
  server refuses by them.
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
  checked before uploading:** the files first (`remote.Files`: build
  mode's list is `git ls-files`, so outside a repository it fails before
  any session), secret-like names
  (`confirmUnnamedSecrets`), a role another compose project holds
  (`RoleHoldersScript`), a new instance, which asks
  (`InstanceExistsScript`, `--yes`), and a child's sync (Syncs).
- **A release is a new directory, never a changed one:** `UploadScript`
  takes the release lock, says so (`LockedMarker`: devopsy sends the
  tar only then, Locks), extracts into `releases/<id>.tmp` through
  `--stdin-guard`, writes `PendingFile` and renames it into place. An
  existing id is refused: `mv` would put the new tree inside the old
  release, which would run again.
- **The server names a release** (hidden `--release-id`, under the
  lock; the upload prints it on stdout): a UTC second, or a second past
  the newest release there when that is not earlier
  (`remote.NextReleaseID`). Why: ids order releases (pruning,
  rollbacks, `--releases`), and an operator's clock (the id's source
  before) could order an older release after a newer one across
  laptops and CI, or give two releases one id in one second. The
  record (`RecordFile`) keeps the operator's time, for display only.
- **Files go with git's modes** (`Pack`: 0755 with any executable
  bit, else 0644), never the checkout's. Why: a checkout's modes are
  whatever wrote it, which git never shows (ddev's Mutagen writes 0600
  files to the host), and a 0600 Caddyfile bind-mounted into a container
  running as 10001 crash-looped it (decided 2026-10-22). Directories get
  the server's umask from `tar`.
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
   or import labels (`PrepareScript`, Roles, exports and imports).
6. `prepare` steps, then `--missing-vars` (`cli.MissingVars`: compose's
   `${VAR:?}`/`${VAR?}` left unset), then, releases only, the checks
   that read compose's configuration, which fails while a required
   variable is unset, so they come after what makes them (decided: before
   prepare, a first release whose prepare makes its secrets, drupal11's
   and the registry's, passed them unchecked): `--check-names`:
   containers of the same compose project from another directory
   (`cli.ProjectConflict`: `-`-joined names coincide, `a-b`/`c` and
   `a`/`b-c`), and a network the project names as its own that another
   compose project owns (`cli.NetworkConflicts`: Compose would silently
   share it; external networks are how a project joins another's);
   `--check-mounts` (`cli.CheckMounts`, Data, destroying and removing),
   which also readies `UtilImage` when an entry is owned, so a registry
   down refuses the release instead of failing it after its switch.
   Rollbacks run `--check-mounts restored` in the release they restore
   instead: no label refuses (unreadable ones warn), only what its
   `--mnt-owners restored` needs, the util image and no owned entry a
   link (review of 2026-10-23, round 2: an image pruned since failed the
   rollback after its switch).
7. The images (`fetch_release`, Images). `ServerLogFile` is created
   here, so a full disk fails the release before it goes live.
8. `PendingFile` removed, `make_current`, `--mnt-owners` (Data,
   destroying and removing; before the sync of a release that syncs,
   after `followOutput` otherwise, and in a rollback from the restored
   release's labels, `--mnt-owners restored`, where labels this devopsy
   cannot read only warn: a failure is the run step's), the sync of a
   release that syncs, `followOutput` (From the switch on), the run
   step.
9. A failed run or sync: `FailedFile`, what the release's containers
   were left as into the server's log (`failureReport`: each one's
   state and health, then the health checks and last logs of those
   neither running and healthy nor exited 0; decided: the restart
   recreates every container whose image or config changed, a tag
   rebuilt upstream included, so their evidence went with them and
   `--log` only said "unhealthy"), `current` back, the previous
   release restarted with its own images (`get_images`), owners
   (`--mnt-owners restored`, a failure only said) and run step
   (`--release-steps restart run`: its `rollback:`, else `release:`),
   except on a partial copy. A rollback whose run step fails marks the
   release it restored failed (decided: it did not come back up, so it
   is no rollback target until released again). A first release, with
   nothing to restart, takes down what it started (`firstDown`, after
   the report: `down --remove-orphans`, volumes and `mnt` kept, never a stop: `restart:
   always` would bring a container back with Docker, its bind mounts
   through the gone `current` made again as root's) and leaves nothing
   current; `--status` names it failed.
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
  write secrets too. A key's lines are all of its value, quoted over
  several lines, and `KEY: value`, which compose's parser reads as
  `KEY=value`. The result goes into place in one rename beside what a
  link points to, with the file's mode, so no command reads it empty or
  cut (a `cat` into it truncated it first); a file of another user's is
  written over in place. Inherited values are written under `umask 077`.

### Locks

- **The release lock** (`$base/.lock`, fd 9) is taken by every script
  that changes an environment, the upload too (`releaseLock`,
  `lockWait`: `-n`, then a "waiting for" line and `-w 600`, so a wait
  is never silent). Why the upload: `--destroy` never empties `$base`
  under it, and a second release's upload waits for the first's
  activation. A fetch's children inherit the lock, so a recovery never
  races a fetch still running.
- **What a session sends goes once its script holds the lock**
  (`remote.WithGate`, `lockedGate`): the upload, `--import`,
  `--destroy` and `--releases remove` print `LockedMarker` on stderr
  once they hold it (the removals once they hold the image lock too),
  and devopsy sends stdin only then (the tar, an import's data, or the
  `go` a removal reads through `--stdin-guard` before it starts); an
  import interrupted before drops what its pipe holds too. Why: a session cut
  while its script waited (Ctrl-C, a drop) left its tar in the server's
  pipe, and the script extracted it once the lock came, after devopsy
  had said nothing changed; a removal went on likewise. Now it finds an
  empty or silent stdin and stops. Activations need no gate: their
  watcher, started past the lock, reads a closed stdin at once. A
  sync's holder starts its watcher before the lock, armed only while it
  waits (nothing changed yet), and disarms it once it holds the lock: a
  session gone meanwhile ends it. With a heartbeat, the disarm waits for
  the session's next byte (`<armed>.next`), which devopsy sends at once
  on the holder's `BeatMarker` (`waitBeat`), so a live session goes on in a
  round trip and one that went silent during the wait ends it too,
  before anything stops, rather than once its quiet time comes after
  the lock. `--vars` edits and the inherit write
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

Every image a release runs stays until its last release goes. A
rollback runs exactly what the release built and what its changed names
pulled; a pulled data service only goes forward (below). The rules live in Go
(`internal/images`, whose comments say how each action decides), run
on the server as the hidden `--images` (`runImages`; `img` in the
scripts), under "What is deployed decides on the server"; they were
generated sh at first, where four review rounds found most of the
bugs. The scripts keep what needs the shell (`releaseImages`): the
locks, and fetching on a terminal (`fetch_images`, `with_tty`).

- **Fetching happens before the switch** (`fetchImages`, through
  `devopsy --`, past project commands): build mode `ProfilePull`
  (`--profile '*' pull --ignore-buildable`) then `build --pull`; image
  mode `pull`, which pulls buildable services with an `image:` too and
  fails on a missing tag, then `ProfilePull`. A rollback or a restart
  restores its images (`img restore`) and fetches only when an image
  it builds is gone.
- **Every release pulls every profile's non-buildable services,** pull
  only: the listing and the bookmarks stay the default
  services'. Why: a profile-only tool was pulled once, on first
  use, and its tag never got a patch (decided 2026-10-23). A profile's
  buildable services are never built or pulled by a release, so a tool
  nobody pushed never fails one; a non-buildable one that fails to pull
  fails it before the switch. Accepted: image mode asks the registry
  twice for its default third-party tags (Compose cannot pull those and
  the default buildable ones in one call; an unchanged image downloads
  nothing).
- **The listing is per service** (`devopsy-images`: name, image id,
  service, kind, registry digest, version label), from `compose config
  --format json` without profiles and without devopsy's own file
  (`NoReleaseComposeEnv`). Own or pulled is decided by compose and the
  mode (`Service.own`: a build in build mode, or `pull_policy: never`),
  never by the image: on Docker's containerd store built and loaded
  images carry `RepoDigests` too (checked 2026-10-23), and a pulled
  image's id is its index digest. The version is display only: no field
  is common to images.
- **Bookmarks are the reference count:** each release tags every image
  it runs as `<repo>:devopsy-<compose name>-<id>` (`bookmarkPrefix`: a
  long compose name is cut and hashed so the tag fits Docker's 128; a
  repository named twice adds a checksum of the ref), by image id, since
  another fetch may move the name meanwhile. Digests are bookmarked too
  (`prune -f` would delete an unused image pulled by digest only), never
  restored. `devopsy-images` lists `ref<tab>image id`: what a removal
  decides from even after a bookmark went.
- **Data only goes forward** (`history.go`, `record.go`,
  `restore.go`): a pulled data service (`KindData`: a `devopsy.data`
  label, any value, no `build:`, pulled) never runs an image older than
  the newest its data ran. No version ordering (no field is common to
  images, checked 2026-10-23): the environment's `HistoryFile`
  (`$base/.devopsy-data-images`) keeps every image that ran on each such
  service's data, appended only after something ran (`img ran` after the
  run step, failed or not, before a restart), never at `img record` (a
  release cancelled after it ran nothing). "Newer" means "ran later on
  this data". `img record` refuses an image the history moved past, or
  one it never ran under a changed name (`check`), unless `--yes`
  (`ImagesYesEnv`), whose `ForcedFile` starts the service's history over
  once it runs; refused, `putNamesBack` restores what `img names` saved
  before the fetch. A history is seeded from the running container when
  it has none (`seed`: an older devopsy's data, a label just added). Only
  the environment's own side writes it, under its release lock;
  `--destroy-data` removes it.
- **Holds, never retags of pulled names:** every activation writes
  `ComposeFile`: a `devopsy.digest` label per fetched service (the image
  it was given: compose recreates on a label change, so never a release
  id) and `image: <repo>@<digest>` for every pulled data service and for
  a rollback's changed name whose name moved on. `Restore(target,
  against)` decides per service from target's compose now (`List`, which
  never reads devopsy's own file: `NoReleaseComposeEnv`): data, the
  history's last (else its container's, else against's); a pulled name
  unchanged between the two releases, forward; a changed one, exact, held
  or pulled by digest; own images from their bookmarks, `ErrGone` (exit
  1) to build again. Why never a retag: `others()` let a rollback move a
  shared pin back while its other user was down, and a backward retag
  takes a database back on its upgraded data. `Undo` and `Recover` put
  back own names only (`RestoreOwn`). A re-listing that drops an image
  records its names as left (`keep`), or the names other releases
  counted on it to take would leak (found by `TestDataRandom`).
- **Compare names as Docker reports them:** Docker shows names short
  (`docker.io/library/nginx` is `nginx:latest`), so every comparison
  with a name Docker reports goes through `familiar`.
- **A release fetched again never removes names:** it untags the
  bookmarks its new listing no longer makes and records the names it
  dropped for the next pruning. Why: it runs under the shared lock,
  beside other fetches.
- **A restore never changes another environment:** pulled names never
  move back; an own name whose current image another compose project's
  container uses (`docker ps --filter ancestor=`, checked on Docker 29's
  containerd store), or that a bookmark none of this environment's
  releases made holds (exact names: `app-prod` never matches
  `app-prod-eu`), stays on the newer image, said. A held image must be
  there before the switch, pulled by its digest when gone, or the run
  step's `up` would pull after it.
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
  may hold the live release's images; then current's own names come
  back, never a pulled one (its data services are held anyway).
- **Which releases go** (`images/releases.go`): `ToPrune` (older than
  the `keep`-th live one; current and fetching ones stay), the rollback
  target and removals by id, unit-tested on directories.
- **`runImages` ignores SIGPIPE, SIGTERM, SIGINT and SIGHUP:** Go does
  not keep an inherited ignored SIGPIPE or SIGTERM, so the script's
  `trap ''` alone would let a gone session or a second TERM kill an
  undo halfway. Its steps are short and under the lock. `leftAdd`
  records before it says.
- **Tests:** an in-memory Docker (`images_test.go`, with a registry and
  containerd's digest names), `TestRandomHistory`, 200 random histories
  of two environments sharing `mariadb:11` and an alias, and
  `TestDataRandom`, 300 (`DEVOPSY_TEST_SEEDS` for more) of releases,
  refusals, `--yes`, failed runs, rollbacks, plain ups and pruning while
  the registry moves tags forward and back, checking that each database
  runs its data's newest image, that no rollback moves a pulled name and
  that nothing is left once all goes. Script tests call the real `--images`
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
- **The switch waits for a byte from the session:** with a heartbeat,
  the disarm takes the file to `.next`, prints `BeatMarker` and goes on
  once the watcher sees the session's next byte (`waitBeat`), which
  devopsy sends at once on the marker (`WithBeat`): a round trip. Why:
  the watcher starts past the lock and counts its quiet time from
  there, so a session that died silently during the lock wait or
  shortly before a quick fetch saw its release go live; now a silent
  session waits until the quiet time ends it, as gone, and nothing
  goes live. The holder past its lock wait does the same. Only a zero
  byte before any answer disarms: an answer that early is an abort,
  devopsy interrupted before a sync's marker, with the session's end
  right behind it (its bytes once disarmed a release that syncs, which
  then switched). `prelude` removes a watcher's file only once its
  script is gone (`kill -0` on the pid in its name): a holder arms its
  own before it waits for the lock. A watcher that never answers
  (killed outright) cannot hold a script: `waitBeat` gives up two beats
  past the longest quiet time, takes the file itself and goes the way
  of a session gone.
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
  readers, told by `HeartbeatEnv`, take a silence for EOF. A devopsy
  on a server beats as its own session does (`HeartbeatEvery` falls back
  to `HeartbeatEnv`), so the push's import on another server is counted
  in the beats it gets. A busy
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
  messages (`sshLost`) then mean what they say. An ssh killed by a
  signal devopsy did not send reads as 255 too (`SSHStreams`), and
  `DEVOPSY_SSH_COMMAND` runs through `exec`, so signals reach the
  command, never only a shell around it.

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
  a sync into a child remove anything in it, and only the owner label
  changes an owner.
- **`devopsy.mnt.owner=<uid>:<gid>` owns a service's writable entries**
  (`cli.MntOwnerLabel`, parsed with the data labels into `Owner` and
  `Writable`, every profile; `cli.MntOwners` maps entry to owner;
  `cmd/devopsy/mnt.go`). Why: `shared/mnt` starts empty and the deploy
  user's, Docker makes a missing bind source root's, the deploy user
  cannot chown, and devopsy copies owners exactly, so every template
  started as root for its mounts (an entrypoint's chown, an `init`
  service) and files from elsewhere (an import, a sync, an upload by
  hand) left a non-root app unable to write. Numbers only (names cannot
  be resolved from the host), from 0 to 4294967294 and normalized
  (`parseOwner`), so owners compare as numbers and every script and the
  child's list say them alike. Why the range: past a `uid_t`'s 32 bits,
  or 4294967295 (chown's -1), an owner passed the checks before the
  switch and failed in `find` and `chown` after it. A read-only mount claims nothing (a log
  reader); two owners on one entry are an error, an unlabeled writer of
  an owned entry is none (a tool sharing it). Explicit, never from
  `user:` (decided): changing that must not silently change the data's
  owner, and a default needs rules for names, root, images that drop
  privileges themselves and tools; a variable keeps the two alike.
  Rejected: a shared group with setgid directories and default ACLs
  (tar and rsync set owner and mode explicitly, a chmod sets the ACL
  mask), capability hooks (none runs after an `mnt` import, a plain
  `up` or an upload by hand), a root `init` service in every template.
- **Where owners are applied,** always as root in `UtilImage`, one
  container per owned entry that mounts that entry alone (`ownMnt`,
  `--mount`, its source a CSV field), `--network none`, `chown -h` in
  batches (`find -exec ... +`) and only what differs (`find \( ! -user
  -o ! -group \)`, `chownEntry`), only files, directories and links
  (`\( -type f -o -type d -o -type l \)`, in both looks: a device node
  given to the entry's owner, a block device of the server's disk, is
  its raw disk; review of 2026-10-23, round 4). Why one entry per container (decided,
  review of 2026-10-23, round 2): a directory the app swaps for a link
  during the scan can lead chown only into that entry or the throwaway
  container, never another entry or the host, which is what `-execdir`
  bought before at one chown per directory (measured on 20,200
  directories of 40,201 inodes in `devopsy-util:2`, container
  included: 15.3 s with `-execdir`, 0.34 s now). A missing entry is
  made by devopsy first, 0755 (`makeEntry`; one that is a link is
  refused there too). A scan that fails is looked at again, chowning
  nothing, and fails only when something is left with another owner or
  when that look fails itself (a directory find cannot read: review of
  2026-10-23, round 2, it passed silently), `-ignore_readdir_race` on
  both: the services may run meanwhile, and a file one removed after
  find saw it failed the release after its switch (review of
  2026-10-23; rsync's 24 is the same race). Applied: a release, right after
  `make_current` (it changes live data, so never before; `--mnt-owners`,
  a failure a failed run step, ignoring SIGTERM, SIGINT, SIGHUP and
  SIGPIPE as `runImages` does, since nothing interrupts after the
  switch and Go keeps no inherited ignored SIGTERM: review of
  2026-10-23, round 3), `--network none`, missing entries made
  0755 (only entries: a bind source below one is refused, below), and
  every owned entry scanned whole (decided: what devopsy did
  not write has the wrong owner deep under a correct top; measured 0.24
  s warm, 0.75 s cold for 500,000 files; fallback for huge trees: `-type
  d` only), and rollbacks and the restart after a failed run from the
  restored release's labels (`--mnt-owners restored`: labels this
  devopsy cannot read there, names from before a change, `./mnt` whole,
  a compose error, are a warning and nothing is given away, for the
  reason rollbacks never check roles or imports; review of 2026-10-23,
  a rollback to a release with `devopsy.sync` labels failed; a chown
  failing on labels read stays a failure); a sync's rsync, `--chown` per entry from
  the child's labels (Syncs); an `mnt` import, after its extracting
  containers (`extractEntries`, below) in the entries' own, never
  locally on macOS (review of 2026-10-23, round 2:
  as before `up`, and Docker Desktop's file sharing may refuse a
  chown); a reset, after `emptyMnt`;
  locally before compose's `up`, `start`, `restart`, `run`,
  `create`, `watch` and `scale` (review of 2026-10-23, round 3:
  `restart` starts containers, and Docker makes a missing entry root's)
  (`Plan.Starts`, only when the compose files' YAML has the label or an
  `include:` or `extends:` key, comments left out: no compose config for
  every command, and every template's comments say "included"; the
  project's own files, the ones `ComposeConfig` reads labels from, so a
  `-f` the command adds is half seen by neither; never for compose's
  `--dry-run`, which is compose's only where cobra parses it: before
  the command, after it up to a `--`, for `run` only before its service
  (past it the container's command: `run --rm app composer update
  --dry-run` starts app, review of 2026-10-23, round 4), never a flag's
  value (`commandValueFlags`, `dryRun`), and never in a release on a server, `cli.InRelease`, where
  `--mnt-owners` owns it: review of 2026-10-23, round 2, a rollback's
  plain `up` failed on labels `--mnt-owners restored` only warned about;
  labels that cannot be read are a warning and compose's command goes
  on, since a label devopsy reads must never stop it, and so is an
  owned entry that is a symbolic link, left as it is: review of
  2026-10-23, round 3, `makeEntry` refused it and every `up` failed
  before compose), only
  missing paths made and, on Linux, an entry whose top is wrong given
  away and scanned whole (one whose top is right is never scanned);
  macOS never checks (Docker Desktop and OrbStack let any container user
  write the Mac's files and show them the owner Docker asks for). Not a
  stream's import (it writes as the service's user), restores, restarts
  or reboots (owners persist). No flag to fix owners: a release does it.
- **The mounts check refuses before the switch** (`--check-mounts`,
  `cli.CheckMounts`, beside `--check-names` after the prepare steps and
  `--missing-vars`, on the server: labels can come from its variables):
  a writable bind from the project's tree outside `mnt` (`TreeBinds`:
  it goes with the next release), `devopsy.data=mnt` copying no entry (none mounted, or all
  excluded) or an owner of no writable entry (a sync said "nothing to
  sync" and a child's first release went live on empty data), two
  owners on one entry, `./mnt` whole mounted writable by any service
  (`WholeMnt`, below), a bind source below an owned entry
  (`Binds`), and an owned entry that is a symbolic link in `shared/mnt`
  (`ownedLinks`: Docker would mount what
  it points to, for the owners' step to give away; `makeEntry` refuses
  one too, wherever owners are given). With an entry owned it readies `UtilImage`
  (`ensureUtilImage`), so a registry down refuses the release rather
  than failing `--mnt-owners` after the switch. Not checkable, so
  documented: named volumes and absolute paths. It reads the labels
  exactly as `--mnt-owners` does after the switch (`cli.SyncServices`,
  the leftover `.devopsy/capabilities/sync/` included), so labels that
  step could not read refuse the release here (review of 2026-10-23,
  round 2: the check parsed compose's output itself, passed a leftover
  sync capability, and the release failed after its switch). A compose
  config that cannot be read (`cli.ErrComposeConfig`) is no refusal, as
  for `--check-names`: after `--missing-vars` that is a broken file,
  which the run step names.
  Each bind source counts as written and as where its links lead
  (`resolvePath`: `filepath.EvalSymlinks` where it exists, else its
  longest existing prefix, a link whose target is missing followed all
  the same), both for `Binds` (the lexical one too: the owner's link
  below its entry must not resolve the refusal away) and the resolved
  one alone for `TreeBinds` and `WholeMnt`. Why (review of 2026-10-23,
  round 3): the check was lexical, so a release-tree link to below an
  owned entry, or a `shared/mnt` link into the release, passed. Links
  into `shared/` lead out of the release tree, so they stay fine.
- **A bind source below an owned entry is refused** (decided, review
  of 2026-10-23, round 2), by any service, read-only too: the entry's
  owner (the app, writing it) can replace `storage/files` with a
  symbolic link, and Docker resolves a bind source on the host, so a
  link to `/` would mount the host's root into the next container
  started. The entry itself is a mount point no container can replace,
  and the check refuses one that is a link. Before: devopsy made such
  paths, its owner's (`OwnedPaths`, a directory at a time), which this
  replaced; only top-level owned entries are made and scanned now. A
  path below an entry nobody owns stays allowed (decided scope): such
  an entry is the deploy user's, written from the host, while an owned
  one is written by a container by design. Locally nothing refuses it.
- **`./mnt` whole is no entry** (decided): refused where labels are read
  as a `mnt` service's or an owned writable mount, writable on any other
  by the mounts check, and ignored read-only (a tool), so nothing
  downstream sees `.`. Why the check: such a service writes every entry,
  and once its mount was no entry the child guard no longer saw it
  (review of 2026-10-23: a sync rsynced under a running file manager).
  Why no entry: no project
  needs it, it gave every entry one owner and carried every service's
  data, and its special cases (a sync's notice, the module `mnt`, an
  import emptying every entry, links between entries allowed) caused a
  review bug: a tar member `../x` into `.` emptied a container's root.
  A tar carrying `./` itself is refused too.
- **`--destroy` removes nothing unless everything before succeeded**
  (`DestroyScript`, both locks; asks for the target's name unless
  `--yes`): the hidden `--destroy-data` (`runDestroyData`) in
  `current`, else the newest release: the environment's `destroy:` step
  if any (a hook, while everything is up), then `compose --profile '*'
  down --volumes --remove-orphans --rmi local` and `mnt` emptied as
  root in `UtilImage` (a plain `docker run`: `compose run` would bring
  the network back), a sync's leftover record's writers ended first
  (`endWriters`: nothing writes into `mnt` while it is emptied); then
  every release's images; then `removeBase`.
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
  exclusive on `SyncLockFile` so syncs from one parent serialize,
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
- **The gate:** the holder opens it before its marker (`SyncOpenFile`,
  named by the sync's id, which the push passes on as `SyncIDEnv`;
  inside, the parent's address and the id) and closes it (`rm`) right
  before deciding. Why the name: a write left from an earlier sync can
  never claim a newer gate, and the claim is the rename alone, with no
  check of the content before it that another gate could slip under. The child's
  side of the push claims it right before its first write
  (`claimWrite`, `ClaimWriteScript`: a rename to `SyncStartedFile`, one
  of the two wins; the same-server rsync, `SyncReceiveScript` across,
  an import in `syncImport`), so a push that outlived its connection
  writes nothing once the holder decided. The claimed file is the
  sync's record of its writers: each import appends a `WriterLine`
  (service, its devopsy's pid, the service's state before) before it
  writes, and the last write appends a `DoneLine` with the sync's id
  (the rsync's when no stream follows, else the last import's, `import
  <service> last`), on the child, never through another session, and
  never creating the file (`recordWrite`; the receive script checks the
  first line first). Across servers the
  receive script removes its rsync container once the holder decided
  (a watchdog on `SyncStartedFile`): after a silent cut between the
  servers the rsync would wait for TCP to give up, and sshd sends
  nothing on a quiet forward (rsync's `--timeout` counts its own
  keepalives into the forward's local socket, so it does not help).
- **The decision** (`syncBlock`), once the holder's read of the answer
  ends, however it ends: it takes the record in one move (`mv` to
  `SyncTakenFile`), so a writer appending after it finds no file and
  stops. Its own sync's done line is `go` (an `abort` after it too),
  taken without it `partial`, nothing taken on a marked environment
  `partial` (decided: nothing written leaves an earlier sync's partial
  copy as it was, even when the parent's labels cover nothing now, and
  only it: `finish partial` starts again what this sync stopped,
  `restoreOwnStops`, the record's `own <sync id> <name>` lines that
  `stopServices` writes, so what was down before stays down; none when
  its prepare failed before stopping, and never another sync's: a
  record of what was down before, written in prepare, missed a prepare
  failing earlier and started the import's set), else the answer; the
  taken record goes once finish is done.
- **No write outlives its sync** (`endWriters`, cmd/devopsy/writers.go):
  before a partial copy is marked (`finish partial`) or settled, what
  the record names is ended. A write runs on past its sync when its
  devopsy is killed outright, or inside a container, which never gets
  the signal (an import's load through `docker exec`, a one-off `run
  --rm`, devopsy's own rsync). So: the recorded imports' devopsy gets
  SIGTERM (only a pid `/proc` still shows as `--sync-services import`
  in the child's directory, `isSyncImport`: a pid reused after a reboot
  may be another environment's import; Linux only), the sync's own containers are removed (named
  `SyncContainerPrefix` + its id, never another sync's), the imported
  services are stopped (the only way to end what `docker exec` started
  in them) and, when they ran before the sync, recorded so a completing
  sync starts them, their one-off containers removed; then the imports
  are waited for (`writersWait`, 15 s) and killed. Decided (stop, not
  restart): the service holds a half-loaded database, and a partial
  copy keeps everything off its data.
- **A partial copy keeps everything off the data:** `--sync-services
  finish partial` leaves what the child's side stopped down (the
  record's `stop` lines) and writes
  `SyncIncompleteFile`; `finish go` removes it, and a failing `finish
  child go` writes it (a sync completes with its finish; the hook
  decides what stays up). While it is there `partialGuard` refuses a
  release that does not sync and a rollback, the restart after a failed
  release is skipped, `CurrentExistsScript` says `incomplete` so the
  operator refuses before uploading (`--release-no-sync` included: a
  sync is the fix, `--release-sync` when the sync itself needs a code
  fix), a push from it refuses (a chain), `--export` warns, and
  `--status` shows it. Labels bound the promise: only what the child's
  side stopped is kept off partial data.
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
  `partialGuard`, takes its record as the holder would, ends what it
  names (`endWriters`), and counts its data as not completed (marked,
  keeping a mark the holder wrote before dying, `finish child
  partial`); a sync's files while nothing holds the lock (`LockBusy`)
  are a leftover, a running sync is not.
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
  (SIGINT, SIGTERM in `runSyncSession`): before the marker devopsy writes
  `abort`, closes the holder's stdin and waits, as for a release (the
  watcher, armed during the lock wait or before a release's switch,
  ends it; past that the holder reads the abort after its prepare and
  restores in the session, so the operator sees how it ends), saying so
  after 5 seconds, and a second one ends the session (`endHolder`);
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
  own process group, `runDataHook`) and import get SIGTERM, spools go,
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

- **One set of labels for every copy** (`internal/cli/sync.go`):
  `devopsy.data=mnt|stream|none` (`cli.DataLabel`), `devopsy.data.exclude`
  and `devopsy.data.stop=never|own|import|always` (`DataStopLabel`),
  for syncs, `--export`/`--import` and later snapshots. Why: one boolean,
  `devopsy.sync.stop=true`, answered three questions (down while its own
  data is copied? while any data is imported? for every copy?), so
  `--import database` of a dump left the app serving during the load,
  and a child's app ran while its database was reloaded. `mnt`, not
  `rsync` (only the transport) or `files` (reads as `./mnt/files`).
- **Values:** no label is no data, like `none`; an empty value is an
  error (decided: a variable left unset by accident must fail, not stop
  copying; `none` turns copying off). `stop` defaults to `never` for
  both methods (every stop is visible in compose.yaml), applies to
  services without data too (an app needing `import`), and `never` is
  accepted explicitly (a variable sets it; it silences the dependents
  notice). `own` without any `devopsy.data` label is an error (it would
  never stop); with `none` it is ignored (a variable chose `none`).
  The labels before these, their old values (`rsync`, `files`, `skip`)
  and anything left in `.devopsy/capabilities/sync/` are errors naming
  what replaces them, only where labels are read (`parseSyncServices`,
  `SyncServices`), never in `Build`: a rollback to an older release, or
  the restart after a failed one, runs a plain `up`.
- **The stop set is a pure function** (`cli.DataStops`: the operation,
  `ExportOne`, `ImportOne`, `SyncParent`, `SyncChild`, and the services
  in, the set with each one's why out). The table in the README is the
  spec; `own` and `import` overlap only on the service's own import
  (decided: no `own,import`, a list later if a use appears), and the
  values are not a scale (`import` on a raw database exports it
  running, a corrupt copy). An operation over every service takes the
  union, under one stop and one start (decided over a per-service
  window: one record, one restore, Compose ordering one `stop`).
- **Dependents follow, everywhere devopsy stops something:** the
  services whose `depends_on` reaches a stopped one, transitively, every
  edge (`required: false`, and Compose's implicit `network_mode:
  service:`, `links`, `volumes_from`, which its config lists under
  `depends_on`: pinned by a fixture), computed by devopsy
  (`cli.WithDependents`), never left to Compose's `stop`: the record,
  the hooks' lists and the restore need the full set. The closure
  first, then only what runs is stopped and recorded, so a database
  down already still stops its running app, and a dependent down or in
  an inactive profile stays as it was. That includes the stops that end
  a cut write (`endWriters`, `endImport`). devopsy says what it stops
  and why (`cli.StoppingLine`). Rejected: stopping dependents of an
  imported service with no `import` value (implicit; an import of the
  app's own storage still needs a label).
- **One invariant for stream scripts:** a service in the set is down
  (`DEVOPSY_DATA_STOP` says so); a stream service not in it is up for
  its import (`upFor`, always `--no-deps` for `--import`: a retry's set
  is empty, since nothing runs, so "only with a set" would start a
  half-imported dependency; a sync keeps `!rec.first || len(rec.stops)
  > 0`). No exception for streams, a dependent's included: a script
  needing its server refuses.
- **A running dependent without a stop label is named** on `--import` and
  a child's prepare (`cli.UnlabeledDependents`, filtered by what runs):
  the worker writing to a streamed database is what authors miss. The
  child's prepare does not know what the parent copies, so it names the
  dependents of any of its data services (an approximation, decided:
  the list carries no `depends_on`).
- **The child stops before it knows what is copied** (accepted: knowing
  first costs a session per sync): with nothing to copy, its `import`
  services were down for seconds.
- **No `pause`:** `docker pause` flushes nothing (rsync sees what the
  kernel has, not a paused process's buffers), and Traefik 3.7 drops a
  paused container's router the next time it reads the containers
  (devopsy-traefik's AGENTS.md); a stopped routed container loses it at
  once (404). Consistency comes from a stop or a stream's dump.
- **`restart: always` defeats "stays down":** Docker starts such a
  container whenever its daemon starts, so a reboot brings a kept-down
  app back on half a database. A notice names one in a kept-down set
  (`restartAlwaysNotice`); the templates use `unless-stopped`.
- **`devopsy.data.exclude`** (`DataExcludeLabel`, entries of `mnt`,
  comma or space separated) drops entries from a mnt service's `Dirs`
  at parse time, so every consumer (copy, guard, list) agrees. On
  `stream` and `none` it is ignored, not validated, so a variable can
  flip the method and keep it. A mnt service's entry with whitespace in
  its name is refused: the child's list, exclude and rsync modules name
  entries split by spaces (decided over a new list format).
- **The child guard: each side reads its own labels, and the parent
  checks the child's before changing anything.** `--sync-push` asks the
  child for `--sync-services list` (through `--sync-exec`, one more
  session across servers) and refuses (`syncConflicts`) when a child
  service mounting a copied entry is running or restarting and not
  `mnt` there (one the child stopped is down already), and
  (`streamMismatches`) when a parent `stream` service is not `stream`
  on the child, whose import would refuse it after the rsync, or a
  child `stream` service is `mnt` on the parent. A stop word differing
  between the sides is named (`stopMismatches`). A label from an
  inherited variable cannot lag (the child reads the pending file,
  inherit below); one in the child's own `shared/.env` can still
  differ. The list (`runSyncServices`): per service `name`, `<method>+<stop
  word>`, state, entries, owners (`<entry>=<uid>:<gid>`, space
  separated, `listOwners`), tab separated, then `!import\t<service>` for
  an import's mark (`listMark`); only a devopsy with `data-labels` and
  `mnt-owner` reads it, from a child checked for both (`childNeeds`).
  The list refuses two owners on one entry.
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
  live service fails or copies inconsistent files); one boolean for the
  three questions; stopping others from inside the import script
  (devopsy would not record, restore or keep down what it stopped); a
  per-service stop window in syncs; `files` as the method's name; an
  empty `devopsy.data` to turn copying off; splitting the capability
  into `sync` (prepare, finish) and `data` (export, import).

### Order and state

- **The child's side** (`--sync-services prepare`): `settle`, the state
  record, an import's leftover writer ended (`endLeftoverImport`, when
  its mark is there: a sync never streams into a database still
  loading), the import check, the `prepare child` hook, then one
  `compose stop` of its set (`cli.DataStops`, `SyncChild`). Keep it one call: Compose orders
  them by `depends_on`, dependents first. `finish go|abort` starts them
  and waits until healthy, `partial` leaves them down; every spool
  left goes (an import removes its own; one left is an earlier sync's);
  then `finish child` with the word. Stopped
  services stay down through the imports: no app on a half-imported
  database.
- **The parent's side** (`runSyncPush`): the checks (labels, the
  child's import mark (below), the `export` script when a service is
  `stream`, the child guard, the util image) before anything, then `prepare parent`, on the same server a
  refusal when the child's gate is gone already (its holder decided),
  stop, rsync, streams, start (healthy), `finish parent go|abort`. A
  failing prepare aborts before its side stops anything.
- **Hooks run from Go** (`runDataHook`, `--capability data ...` in its
  own process group), only where their file is (`export` and `import`
  are required for a `stream` service), with that side's label lists
  and the operation's set (`cli.DataLists`, `dataHookEnv`:
  `DEVOPSY_DATA_STOP` from labels at prepare, export, import and
  `finish ... go|abort`, from the record at `finish child partial` and
  `settle`, what is down then) and the sync's addresses and arguments
  in their environment (README). One `data` capability, not a split:
  one concept, named like its labels, and snapshots will want
  `prepare` and `finish` too. The holder's scripts (`syncPrepare`,
  `syncFinish`) only call `--sync-services`. Hooks run in the
  foreground: what one leaves running in its group ends when it
  returns, said; what it starts outside its group (`setsid`, `docker
  exec -d`) is its own.
- **The state record exists before anything can fail:** stopping
  records what it stops in `$base/.devopsy-sync.services` (`syncState`,
  `stop <name>` lines; `startSyncState`: the child's prepare creates it
  before its hook, `stopServices` before its first `ps`). Why: no
  record means restore the side's whole set (a sync's; nothing for an
  import or `--resolved`, whose mark and record are written together),
  one down on purpose included. One left by an earlier sync or import
  that could not restore is kept and added to (`syncRecord`, rewritten
  whole, `writer <pgid>` lines kept), and what a sync stops itself is
  marked `own <sync id>`.
- **Restoring brings back only the recorded services,** whatever their
  labels say now (an inherited variable may have changed them since a
  cut sync), from container state (exited), so it also recovers after an interrupted sync, and a
  service down before the sync (a maintenance stop) stays down. It
  waits for every service it started to be running and healthy
  (`waitHealthy`: one gone fails), and removes the record once all are
  back. Service states come from one `docker compose ps --format json`
  (`serviceStates`, `servicesIn`), one-off containers left out
  (`serviceEntries`: a hook's `run --rm` still running would read as its
  service running, which then never started again); the sync tests'
  fake docker remembers stops and starts to answer it.
- **A child's first release gets its data services and its side's set
  before its finish hook:** a release with nothing current before it (`prepare
  first`, from the activation), or no container at all when prepare
  started, is a `first` line in the record (a stream's import creates
  its container before finish looks, and a first release killed
  before its `firstDown` may have left one; a later sync that finds
  containers drops it). Before
  the hook those services are created in one `up -d --wait`: the ones
  with data and the child side's set by labels, without the running
  filter (an unlabeled app following its `own` database, an `import`
  app without data), but not in a profile that is not up
  (`SyncService.Active`, `COMPOSE_PROFILES`: an `up` naming a service
  starts it whatever its profiles; a stream's import brings one up for
  itself only), since `up` creates dependencies but never
  dependents, and the hook sanitizes through them (drupal11's needs the app:
  without it, prod's passwords reached a child), and a stream imported
  then stays up. Only on go: an aborted first sync creates nothing.
  Never otherwise: a missing service was removed on purpose.

### Images with the data

- **The child takes the parent's data images with its data** (`img
  data`, `sync-check`, `sync-done`, `internal/images/sync.go`): the
  parent's pulled data services' images (their containers', else the
  history's last) and history are read in the preflight session
  (`ParentCheckScript`, `ParentData`), passed to the child's side
  (`images.SyncImagesEnv`) and their checksum to the push
  (`SyncImagesSumEnv`), which refuses when the parent's changed since
  (a release between the preflight and the push), before stopping
  anything. The holder checks before its prepare, a release that syncs
  at its record (in place of its own check for those services: an
  aborted sync brings the release before back on its own images): the
  parent's image or name passes; moved past, or a name the parent's
  data never ran, needs `--yes`, every sync. The parent's image is
  pulled by digest when missing and listed at once as a `sync:<service>`
  entry of the child's release (bookmarked, removed with it). On a
  complete copy (its done line, whatever the answer), `sync-done` holds
  the child on the parent's image and replaces its history with the
  parent's window (first line `sync <parent>`); failing, the child is
  marked (`finish`) and stays down. `restoreServices` makes again (`up
  --no-deps --wait`) a service whose container's `devopsy.digest` label
  differs from the release's `ComposeFile` (`images.Recreate`), before
  starting the rest: `compose start` would run the old image on the new
  data, the finish hook first. Only `mnt` copies: a stream goes through
  the child's own image. A complete `mnt` `--import` or `--resolved`
  starts the service's history over (`Imported`).

### Streams

- **An import only ever reads a complete export:** per `stream`
  service, in `depends_on` order over every service
  (`SyncStreamServices`: dependencies first, ties alphabetical, a cycle
  an error), the parent runs the capability's `export <service>`
  (`syncStream`) into the child's spool,
  `$base/.devopsy-sync.stream.<service>.<sync id>` (`remote.StreamSpool`:
  one per sync, so an export that outlived its push, killed outright,
  never writes into the next sync's): on the
  same server directly, across servers through `cat >` in an SSH
  session to the child (`exportInto`). Only once the export exited 0
  does the child's `--sync-services import <service>` run. Never a
  pipe into the import: a cut export never reaches it.
- **The import:** the service brought up (`upFor`, `--no-deps` but on
  a first release: Compose would start a dependency the sync stopped)
  unless the child's side stopped it (down again after if it was down), `import <service>` with the spool
  as stdin, the spool removed whatever happens. Its stdin is a pipe the
  push keeps open, so across servers the session's end, which a remote
  process never gets as a signal, cancels it too.
- **Every file a sync leaves in `$base` starts with `.devopsy-sync.`**

### Transport, SSH and environment

- **rsync always runs in `devopsy-util:<n>`** (`UtilImage`, now `:2`;
  built on each host from `UtilDockerfile` when missing; GNU tar for
  `--export` and `--import` too, GNU find and chown for owners), as
  root with `--numeric-ids`. Why: owners and modes survive, and the
  deploy user's permissions do not matter. Never devices, FIFOs or
  sockets (`--no-D`, taking back `-a`'s `-D`): as root rsync made them
  on the child's host, where bind mounts are not nodev, and `--chown`
  gave a block device to the entry's owner (review of 2026-10-23, round
  4), as an `mnt` import refuses them (`errMember`). Times are compared to the
  nanosecond (`RsyncCopy`, `--modify-window=-1`): to the second, rsync
  skipped a datadir's files of the same size that both sides wrote in
  the same second (the child stops right before the parent), and the
  child's database opened its old tablespaces under the parent's
  dictionary (found on a test server, 2026-10-23). An entry the child's labels
  own gets `--chown=<uid>:<gid>` (`syncRsync`, the receive script's
  options), from the child's list: the child's owner for every file
  written, whatever the parent's are (a parent whose files were wrong,
  an upload by hand there). Bump the tag whenever the Dockerfile's
  packages change: a host keeps an image of the old ones until it is a
  week old.
- **The util image floats on `alpine:3` and is rebuilt weekly**
  (`ensureUtilImage`, before every use; scripts call the hidden
  `--util-image`, a feature): older than `UtilImageMaxAge`, `build
  --pull --no-cache`, then the image it replaced removed without `-f`
  (a container still running it keeps it). Why: it runs as root on the
  data and serves rsync across servers, and was built once per host and
  never again (decided 2026-10-23: patches reach every floating image).
  A rebuild that fails warns and goes on with the image there (a
  registry out of reach never stops a sync, an import or `--destroy`);
  a missing one that fails to build fails, as before. Two at once on a
  host are harmless: a tag moves only when a build completes. One
  implementation, in Go: the receive script once built it in shell, and
  a weekly check there would need GNU `date` (macOS runs the scripts in
  tests); an older child refuses the sync, naming the upgrade.
- **Entries are the top-level entries of `mnt` that `rsync` services
  bind-mount** (`docker compose --profile '*' config`, lexical and
  resolved paths); one missing on the parent is
  skipped with a notice, the child's left as it is. An entry that is a
  file on the parent is copied as one, `--inplace`, so the container
  bind-mounting it sees the new content (a bind mount is the file's
  inode); across servers its module is `/mnt` filtered to it.
- **Across servers the parent's daemon serves only what is copied:**
  named with the sync's id and labeled with the parent's `mnt`, so the
  next push from the parent, holding its sync lock, removes one an
  earlier push left (its devopsy killed outright); one module per entry (`rsyncdConf`: the entry's name), so nothing else
  under `mnt` can be asked for.
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
  the release's:** `$base/.devopsy-sync.inherit.<sync id>.env`
  (`remote.SyncInheritPending`; the id chosen by the operator's devopsy,
  `newSyncID`, since the file is written before the holder runs), named by
  `DEVOPSY_INTERNAL_INHERIT_FILE` (exported by the holder, set by
  `--sync-exec`) for everything the sync runs on the child, and in a
  release that syncs exported by the activation from before its prepare
  steps, so the owners' step after the switch reads the labels with it
  too (an owner inherited is the new one in that release, the entries
  the sync does not copy included: `TestMntOwnersInherited`). Why: the
  child's labels follow an inherited variable in the same sync; written
  after the copy, they lagged one sync. `finish go` moves it into place
  (`mv -Tf`) before restoring services, the hook and the run step;
  anything else removes it. Rejected: writing `inherit.env` in place
  with a backup restored on abort, since a holder killed outright
  leaves the new file over the old data. One per sync: two syncs of a
  child at once (the second waiting for the lock, its file written)
  never read or remove each other's; finish removes another's only past
  `staleInherit` (an hour, longer than any wait for the lock).

### --reset

- **A sync that starts the child over** (`devopsy @<child> --reset`,
  `runSync` with reset): everything a sync checks, then the same two
  sessions; the push gets `--sync-push reset`, and passes
  `SyncResetEnv` on to the child's devopsy. Why: the way out of data
  nothing else fixes (a cut sync or import, a half-written entry the
  parent lacks, which `copiesService` refuses), and every refusal on a
  marked child lists it (`remote.ResetFix`).
- **The wipe is the copy's first write,** at whichever claim comes
  first (`claimWrite`'s first: the same-server rsync's, through
  `--sync-exec --sync-services wipe`; the receive script's, its
  `wipe`; a stream's import, in-process before its `upFor`), never in
  prepare: a push that refuses or never reaches the child leaves it as
  it was, and once claimed the existing rules hold (cut: partial,
  marked, down). `--sync-services wipe` (`wipeData`): an import's
  leftover containers, `compose --profile '*' down --volumes
  --remove-orphans` (no `--rmi`: the release's images stay),
  `emptyMnt` (shared with `--destroy-data`; in a container named by the
  sync, `<SyncContainerPrefix><id>-wipe`, so a holder deciding meanwhile
  removes it, and in `syncImport` after its writer line), and the record set to
  `first` with nothing stopped, so `finish go` creates the services
  with data and the child's set and the run step the rest. Its
  `shared/.env` and release stay.
- **What a reset skips and refuses:** the child guard
  (`syncConflicts`: nothing of the child runs under the copy) and the
  import mark's `copiesService` (it replaces everything); the stream
  mismatches stay (the child's import would refuse). A parent with no
  data refuses (it would only empty the child: `--destroy` does), and
  so does one whose labeled entries are all missing there with no
  stream (the rsync skips them, so nothing would claim and wipe). On
  an environment without a parent `syncPreflight` refuses, as for
  `--sync`.
- **A named feature, `reset`,** found by `--sync-push reset` and
  `--sync-services wipe`: an older parent would sync without wiping.

## --export and --import

One service's data to stdout or from stdin, moved the way its data
label says, with the sync's scripts. Code: `cmd/devopsy/data.go`,
`writers.go`, scope `[@t]`.

- **The label decides how, and it is the contract:** a service without
  data is refused. `stream`: the capability's `export` and `import`
  (`runDataHook`, the lists in their environment). `mnt`: a tar of
  the service's entries of `mnt` (exclude honored, file entries
  included) made in `UtilImage` as root with `--numeric-owner`. The
  set (`cli.DataStops`, `ExportOne` or `ImportOne`) is stopped around
  either and restored (`stopSet`), interrupts and a stop that failed
  included (it may have stopped them all the same) up to the first
  write. A `stream` import of a service not stopped brings it up alone
  and down again if it was down (`upFor`), an `up` that failed too. An
  owned entry it extracted goes to its owner before the import counts
  as complete, in a container of its own (`ownMnt`, named
  `<ImportContainerPrefix><compose name>-<pid>-owners-<n>`, so
  `importContainers` ends it with the extracting ones).
- **An mnt import checks the whole tar before changing anything**
  (`tarEntries`, read with `archive/tar`): a truncated tar fails, and
  so does one without its end-of-archive blocks, which every tar
  writer adds: `tar -t` takes a tar cut at a member boundary for
  complete, and the last member's own zeros (a sparse file, a
  database's pages) pass a check of the spool's last bytes; entries the
  service does not mount are refused, an excluded one by its label, and
  so is a member that reaches out of its entry (`tarMember`): a `..` or
  absolute path, a link out of `mnt`, a top-level entry that is no
  directory or file, a link into another entry, and `./`, `mnt` itself.
  Why: GNU tar extracted those through to another entry, refused them
  only once the entries were emptied, or (`../x` into `.`, before `.`
  went) emptied the container's root. Where a link leads is judged
  through the tar's own links, never lexically alone (`tarLinks.check`,
  after the whole tar is read): a symbolic link's target is walked
  component by component from its directory as the kernel resolves it,
  following every link of the tar on the way (40 at most, Linux's own
  limit), and refused when the way leaves its entry at any point (a `..`
  above it, `../storage/b` going through `mnt` included), as are a
  member below a link of the tar or sharing a link's name (a tar never
  writes them, and then every name is where it lands) and a hard link
  to a link or below one (GNU tar links the symbolic link itself, whose
  relative target then leads elsewhere: checked in `devopsy-util:2`).
  Why: `path.Clean` read `storage/w -> x/x/../../etc/shadow`, with
  `storage/x -> .`, as `storage/etc/shadow`, and on the server it was
  `/etc/shadow` (review of 2026-10-23, round 4). What the tar does not
  carry is a directory to the walk: the entry is emptied first. Any member but files,
  directories and links is refused too (`tarTypes`, `errMember`):
  GNU tar as root makes device nodes on the host, where bind mounts
  are not nodev, and the owners' step then gave one to the entry's
  owner, a block device of the server's disk the raw disk for the
  app's UID (review of 2026-10-23, round 3); FIFOs and anything else a
  tar of data never needs went with them. Then each carried entry is
  emptied, never removed, and extracted with `--overwrite`. Why never removed: a running
  container bind-mounts the directory's inode. Each entry in a util
  container of its own (`extractEntries`, `<ImportContainerPrefix><compose
  name>-<pid>-extract-<n>`), `--network none`, that mounts that entry
  alone (made first when missing, a directory or an empty file as the
  tar has it; one of the other kind refused before anything changes)
  and extracts only its members from the whole spool, by the names
  the archive gives it (`tarEntry.forms`: GNU tar matches
  `./storage/x` only by `./storage`, so member names must be plain
  paths, `././` and `//` refused). Why (decided, review of 2026-10-23,
  round 3): the service whose entry it is may run (`never` is the
  default stop), and its owner swapped a directory the extraction had
  just made for a link to `../<another entry>`, which GNU tar followed
  as root, planting files in another entry (demonstrated); here a link
  leads only into the entry or the throwaway container. Cost: the spool
  is read once per entry. An entry that is a
  symlink is refused both ways: tar carries only the link (an export
  that succeeded empty), and the image's `/mnt` never reaches its
  target.
- **stdout is the data:** `--export` refuses a terminal; `--import`
  refuses a terminal as stdin, spools it whole first (`importSpool`: in
  a release a temporary `$base/.devopsy-sync.stream.<service>.*`,
  locally a temporary `.devopsy/.devopsy-sync.stream.*`, which `excluded` keeps
  out of releases), refuses an empty one, and only then stops anything.
  The spool always goes, and one a devopsy killed outright left goes
  with the next import of its service or `--resolved` (`removeSpools`:
  nothing else would remove a dump-sized file on an environment never
  synced).
- **The first write** is devopsy asking Docker to start the writer (the
  import hook, or the util container): checked against its context
  right before; from then on the import counts as written, a Docker
  error that started nothing included. Before it a failure restores
  the set; from it a failure ends the write (`endImport`, with the
  closure: one-offs, the project's `ImportContainerPrefix` containers,
  the service and its dependents stopped) and keeps the set down, the
  imported service stopped and recorded too when it ran before (one
  rule with a sync's `endWriters`, "stop, not restart"). Locally no
  mark and no record (a release in place has nothing to guard): the
  error names what it left down and `devopsy start`. Decided over
  restoring after a failure, with or without a mark: drupal11's import
  drops the database first, so a cut dump served an empty site.
- **On a target, the mark and the record go first** ("on a target" is
  `cli.InRelease`): the record (`stopSet` with record, the sync's
  `syncState`) at the stop, before stopping (a devopsy killed between
  the stop and the byte leaves it, and the next import, `--resolved`
  or sync restores it; a restore before the first write puts it back as
  it was); the mark (`IncompleteImport`, its second field the service:
  `markIncomplete`'s `from`) right before the writer; a `writer <pgid>`
  line once the hook runs. A complete import restores the record's
  `stop` lines (`restoreServices`, not `stopSet`'s set, empty on a
  retry) and only once all are healthy removes the mark (still its own:
  `clearImportMark`) and the record; a start that fails keeps both. On
  a complete retry `upFor`'s `down()` is skipped for a service in the
  record. Rejected: a mark written only on failure (a devopsy killed or
  a server rebooted mid-load would leave none).
- **The byte before the first write:** the local side sends the data,
  the end line with a nonce (`ImportEndEnv`; a session cut halfway
  reaches the server as a plain EOF) and keeps stdin open; the server
  finds the line inside the stream (`spoolUntil`), prints
  `remote.ImportMarker` and waits, bounded (`dataQuiet`), for one byte
  (`awaitByte`), which the operator's devopsy sends on the marker unless
  interrupted. Why: once the end line came nothing reads stdin, and a
  session lost between the spool and the first write is only noticed
  through SIGPIPE, asynchronously. No byte: restored, nothing imported.
  From the byte on nothing from the session interrupts (the rule after
  a release's switch): `signal.Ignore` of SIGPIPE, SIGHUP and SIGINT
  (only dropping `interruptible`'s `Notify` would let Go's runtime kill
  devopsy on the next write to the gone fd 2), `errOut.keep` to
  `$base/.devopsy-sync.log`, the import's own result deciding. A
  Ctrl-C after the marker says the import goes on, a second ends the
  session. Rejected: a session lost during the load cancelling it.
- **The writer does not hold the release lock:** devopsy sets fds 8 and
  9 close-on-exec (`syscall.CloseOnExec`) so the hook and the util
  container's client get neither; devopsy keeps them to its end. Why:
  after a SIGKILLed devopsy the orphaned load held the lock to its end,
  every lock-taker waited 600 s and failed. Unlike a fetch's children,
  an import's leftover writer is ended explicitly by the next
  lock-taker (`endLeftoverImport`: the record's writer groups, SIGTERM
  while `/proc` shows the data capability's import started with this
  environment's `COMPOSE_PROJECT_NAME`, `isDataImport`, then SIGKILL;
  the import containers; `endImport`, recording the imported service
  only when it ran before the import: a writer line ends ` down` when
  `upFor` brought it up), run by an import of that service,
  `--resolved` and a child's prepare; `--destroy-data` removes the
  containers too (not Compose's: `down --remove-orphans` leaves them),
  and an owners' step's (`ownersContainers`).
- **Containers named after devopsy's pid** (an import's extracting and
  owners' ones, a release's and a local command's owners' ones,
  `<ownersPrefix><compose name>-<pid>-<n>`) are started through
  `dockerRunFresh`, which removes a container of that very name first:
  only a devopsy killed outright whose pid this one reuses can have
  left one, still running (`--rm` removes it once it ends), and docker
  run would refuse the name (review of 2026-10-23, round 3). On a
  server the next lock-taker removes the rest: an import's by
  `importContainers` (above), a release's owners' by
  `ownersContainers` in `--mnt-owners` before it scans, and both in
  `--destroy-data`. Locally nothing holds a lock, so only the name's
  own is removed: another terminal's owners' step may be running.
- **While an import's mark is there, only its service can be
  imported** (`ImportScript`, in shell under the lock after its
  `settle`, before devopsy reads stdin, and again in Go), and any
  import refuses on a sync's mark. Why: two partial services would need
  a rule for every combination, and an import's mark over a sync's
  would lift the guard off its partial copy. A sync's mark replaces an
  import's, never the reverse (`markIncomplete`: `keep` keeps a sync's
  only; the holder's `copy` line writes over it; `finish partial`
  keeps any mark when nothing was written), so from then on only a sync
  clears it. A sync clears an import's mark only by copying that
  service: the push refuses before stopping anything on the parent
  otherwise (`copiesService`: a stream, or for a mnt service every
  entry the child's service mounts, from the child's list, copied and
  present on the parent: with any one, a sync completed over a
  half-written entry the parent lacks and cleared the mark), and a sync
  whose parent writes nothing stays partial. Every
  reader of the mark branches on its code (`markRefusal`,
  `partialGuard`, the restart skip, `PushScript`, `syncBlock`,
  `--status`, `--export`'s warning, `syncPreflight`, the operator's
  release check, `UploadScript`) and says "the import of <service> did
  not complete" with its ways out (`remote.ImportFixes`), `--sync` only
  on a child: the real user is a prod without a parent.
- **"Importing" is the holding file with the lock held**
  (`SyncImportingFile`, written by `ImportScript` once it holds the
  lock, removed on its way out; `ImportRunning`), never `LockBusy`
  alone: any holder of the lock would read as an import. Never in a
  script holding the lock itself, where `LockBusy` is always true:
  holding it, it knows none runs, so `releaseLock` removes a holding
  file a shell killed outright left (the import's and a sync holder's
  alike), which would read as running whenever anything holds the lock.
- **A release of a marked environment refuses before its upload,
  without a new session:** `UploadScript` checks the mark under the
  lock before `LockedMarker`, so no tar is sent and no release is left;
  only what `partialGuard` would (a release that syncs goes on). Rejected:
  one more session per release (accepted cost: its local `before` steps
  run first). Children keep `CurrentExistsScript`, which prints the
  mark's line (`remote.ParseCurrent`), "incomplete" alone for a killed
  holder's files.
- **`--import <service> --resolved`** (`runResolved`,
  `runRemoteResolved`): the way out when the dump itself cannot
  complete. A read-only session (`ResolvedCheckScript`, never waiting on
  the lock) refuses there (no mark, another service's, a sync's, a
  killed holder's sync files, which make it a sync's, an import
  running); a prompt names what starts (the record's `stop` lines,
  the imported service only when it ran before) and asks for the full
  name; a second session under the lock settles a killed holder's sync
  (`settleLeftover`, as `ImportScript` does), checks again, ends a
  leftover writer, gives a `mnt` service's owned entries to their
  owners (`resolvedOwners`, named as an import's owners' containers:
  review of 2026-10-23, round 3, it started the services on the tar
  maker's owners), starts the record and only then
  removes the mark and the record. On the server it is `devopsy
  --import <service> --resolved --yes` (`ResolvedScript`), so the
  `data-labels` regexp gates it. Not locally (decided): nothing there
  tells a failed import's stops from deliberate ones. Rejected names:
  `--accept`, `--force` (`mariadb --force`), `--continue`, `--fake`,
  `--clear`, `--repair`.
- **Prompts ask for the full name** (`confirmWord`: `t.Path`,
  `<project>/[<instance>/]<environment>`), never the service, which the
  prompt's text names: `--destroy`, `--sync`, `--import`, `--resolved`;
  a local `--destroy` keeps the project's name.
- **The restore's compose calls run in their own process group**
  (`compose`, not `composeContext`): a terminal's Ctrl-C reaches the
  whole foreground group, and the larger sets would be left down
  without a word.
- **On a target the server runs the same code:** `ExportScript` (shared
  release lock and exclusive `SyncLockFile`, like the push: both stop and
  restore services; stdout through as it is, never colorized) and
  `ImportScript` (exclusive lock, `devopsy --import <service> --yes`,
  not exec'd: its holding file goes on its way out). The local side
  asks on `/dev/tty` (`confirmImport`: stdin is the data; no terminal
  and no `--yes` fails).

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
  `SyncCopied` lines, a partial or running sync from `partialStatus`:
  "syncing" while a holder holds the child's lock, from its holding file
  on, its prepare included, `SyncRunning`; "source" while a push or an
  export holds the parent's sync lock, `SourceBusy`; `--releases` marks
  the current release SYNCING the same way),
  `---`, then `devopsy --status` in the current release; the operator
  formats the header and prints the rest as it came. In a release,
  `images.StatusLines`: the services held by digest, and those running
  another image than their `devopsy.digest` label says (a plain up
  followed a moved name).
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
  `HEARTBEAT_EVERY`, `COMMAND_TARGET`, `IMPORT_END`, `INHERIT_FILE`,
  `SYNC_NEEDS`):
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
- **`--context-hash` folds base images in, always** (`baseImages`,
  `BaseDigest`): each external `FROM` (no stage, no `scratch`; global
  `ARG`s expanded with the build's args) by its registry's current
  digest (`docker buildx imagetools inspect`, no pull); one pinned by
  digest is its reference; no answer fails the hash. Why: a hash that
  ignored bases reused `ctx-<hash>` images forever, so patched bases
  never arrived (decided 2026-10-23, no opt out). Cost: a registry
  request per base, and a hash needs the network. `COPY --from` images
  are not covered.
- **`--upgrade` reads the same release files as `install.sh`** (archive
  names without version, `checksums.txt`). The daily check runs
  detached (`--upgrade-check`), since devopsy execs right away; only
  release builds upgrade or check.

## Gotchas

Findings that are not one area's.

- Compose recreates a container when a label's value changes (its
  config hash covers labels) and when its `image:` reference changes,
  even to the same image by digest; a plain `up` follows a name moved to
  another image; `pull_policy: never` makes `compose pull` skip the
  service; `compose start` starts the old container whatever the files
  now say. Checked on Docker 29.9 with the containerd store (2026-10-23).
- On Docker's containerd store an image's id is its index digest for a
  pulled image, every image (built and loaded too) has `RepoDigests`, a
  pull by digest leaves a name `docker image prune` never removes, and
  `docker rmi <repo>@<digest>` removes every name of that repository on
  the image, bookmarks included.

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
  command of `sh -c` (so a signal to it does not reach that command).
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
go test -tags e2e -count=1 -v -timeout 40m ./e2e   # E2E_KEEP=1 keeps the containers
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
