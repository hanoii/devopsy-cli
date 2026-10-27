# PLAN-reload: `devopsy @<target> --reload`

Branch `feat/reload` in devopsy-cli (`.worktrees/reload`), the same name
in devopsy-recipe-whoami and devopsy-recipe-drupal11 (they drop their
`reload` command), small doc changes in devopsy-traefik and
devopsy-recipe-registry, and the workspace (README conventions, roadmap).
This plan moves to `PLAN-reload.md` on the devopsy-cli branch, committed
as its parts are settled.

## Context

The templates reinvent "apply what changed without a release": whoami
and drupal11 ship an identical `reload` (`up -d --wait --no-build`),
Traefik's and the registry's `deploy` is what applies their variables,
and devopsy's own `--vars set`, `--vars-instance`, `--vars-project` and
`--inherit` leave the environment changed with no word to apply it
(compose's `restart` keeps the old environment). Imports resolve only at
a release, so a changed proxy fact needs a full release of every site.

Decision: one flag, `--reload`, no `--stop`/`--start` (compose's words
do the stop; a stop with a page is one template's policy, `maintenance
on|off`). A reload is the current release with its environment as it is
now (variables, imports, owners) and its `reload:` step run, under the
same guards as a release, never with new images: a variable that changes
what image a service would get needs a release, and the reload refuses
it. The step is the project's (`reload:` in config.yaml, beside
`destroy:`: a single command, `defaults:` and per environment, read on
the server from the deployed release's config); unset, devopsy's own
`compose up -d --wait --no-build --pull never` in the current release.
Not the run step (decided 2026-10-27): a sync reruns it because data
moved; here the data stays, and a run step is heavy (drush deploy) and
not promised idempotent. Traefik and the registry set `reload: deploy`
themselves; whoami and drupal11 set nothing.

## devopsy-cli

### The flag

- `cli.Flags`, Releases group: `--reload` scope `LocalOrTarget`
  ("the current release's reload step, with its variables and
  imports as they are now"), help in `cli.RemoteCommandHelp`,
  completion words (`internal/complete`).
- Dispatch in `cmd/devopsy/main.go`/`remote.go` beside `--sync`:
  `runReload(target)` opens one session (`ReloadScript`), logged into
  the current release's session log as a sync's is ("reloading"),
  `WatchEnv` armed, heartbeat, `LogSaveScript` after. Ctrl-C before the
  switch marker cancels (the watcher), after it says the step goes on
  (`WithSwitched`), as a release.
- Locally (`runLocalReload`, `release_local.go`): `defaults:`' `reload:`
  step in place, else the compose default, after `--missing-vars`; no
  owners step (`Plan.Starts` owns local starts).
- The step: `remote.Target.Reload` (string, like `Destroy`), in the
  `environments:`/`defaults:` key lists and errors, `defaults:` merging
  (`set("reload", ...)`), `ProjectSchema` (`TestSchemas`), README's
  config example (`reload: deploy  # optional: what --reload runs; default
  up -d --wait --no-build --pull never`). `--release-steps reload run`
  (`runReleaseSteps`) runs the deployed release's `reload:` if set, else
  `compose up -d --wait --no-build --pull never` through `devopsy --`
  (past project commands, as `fetchImages` does), so the server decides
  and the script has one call.

### `ReloadScript` (`internal/remote/reload.go`), in order

Modeled on `HolderScript`'s run part and `RestoreDataScript`.

1. `basePrelude`, `releaseImages`, `watchSession` (armed),
   `releaseLock` (fd 9, exclusive), `exec 1>&2`.
2. Refusals: a plain directory or no current ("nothing to reload:
   release first"); `recoverFetch`; `settleSync` then `markRefusal`
   (a partial sync, an import, a restore: "nothing reloaded", their
   fixes).
3. In `$base/current` (the release's `code/`): `--prepare-release`
   (imports resolved again into the current release's
   `devopsy-target.env`, the role checked; `cli.PrepareRelease` already
   rewrites after `ImportedMarker`), `--missing-vars`, `--check-names
   "$base"`, `--check-mounts`, and the new `--check-images` (below).
   Each failure: "nothing reloaded", exit 1. All read-only on the
   environment.
4. `waitBeat` (a session gone during the checks ends it, nothing
   changed), `trap '' INT TERM HUP PIPE`, `followOutput` into the
   current release's `ServerLogFile` (tag `reload`), after
   `SwitchMarker` so the session log cuts and anchors (`--log` shows the
   reload under its anchor).
5. `--mnt-owners` (a changed owner label is a variable's), then
   `with_tty devopsy --release-steps reload run`, `img ran`. A failed
   step: said with `--log`, exit its status, nothing restarted (the
   release is live; what the step started stays, as after `--sync`).

Needs: the script's flags are read from its text (`Needs`), and
`--check-images` is new, so an older server refuses the whole reload
before anything changes (`featureFlags`, `TestFeaturesCoverScripts`).

### `--check-images` (hidden, `cmd/devopsy/images.go` or `checkimages.go`)

The release runs the images it fetched; a reload must never pull or
build, and the run step is the project's, so the check is the guard.

- At the fetch, `images.List` reads each service's `build:` section from
  compose's config (it has the json already): record per built service
  `Build`: a sha256 of the interpolated section (context, dockerfile,
  target, args, every key) and per arg `KEY=<8 hex of sha256(value)>`,
  as one more column of `ListFile` (`entry.Build`; `readList`/`writeList`
  tolerate the column missing: a listing from before). Values never in
  clear: args can be secrets.
- `--check-images` in `current`: `List` now (compose config with the
  variables as they are) against `readList(rel)`: a pulled service whose
  `Ref` differs ("app's image is `app:v2` now, the release fetched
  `app:v1`: a reload runs the release's images"), a built one whose
  section hash differs, naming the args that changed (`APP_UID`) or
  "its build: section" when none did; a service in neither (added by a
  profile variable) likewise. Fix: `devopsy @<t> --release`. A listing
  without the column (an older release) checks references only, said at
  `-v`.
- The same call goes where the run step is rerun on possibly changed
  variables: the sync's holder before its prepare (an inherited variable
  can be a build arg: refuse before anything stops; `--sync` and a
  release that syncs read labels with the pending inherit file, so the
  check runs with `DEVOPSY_INTERNAL_INHERIT_FILE` exported too) and the
  `--no-code` restore after `--with-env` restores `shared/.env`, before
  its data step. One helper, one message.

### Hints

- `--vars set|unset` on a released environment: after the write, under
  the lock, the same two checks read-only in `current` (`--check-images`,
  `--missing-vars`; `VarsScript` grows the calls, their output parsed
  for the hint), and one info line with the follow-up that fits: "to
  apply it: `devopsy @<as typed> --reload`" (`targetRef`, "running
  containers keep their environment until recreated"); a build arg or an
  `image:` reference: "app's image is built with APP_UID: to apply it:
  `--release`"; a required variable unset: "X is required by
  compose.yaml: the next reload or release refuses until it is set".
  Never a refusal: a release applies any value. Nothing on a plain
  directory. `--vars-instance`, `--vars-project` and `--inherit`: the
  generic `--reload` line (several environments).
- The reload runs every guard itself all the same: the environment may
  have changed since, and values come from elsewhere than `--vars`.
- `--status` is unchanged (its image lines already say when a service
  runs another image than its label).

### Tests (every change carries its test)

- `internal/remote`: the script's shape (`sh -n`), `TestFeaturesCoverScripts`
  picks up `--check-images`, `TestUsage` for the help line.
- `cmd/devopsy` script tests against the fake docker: a reload runs the
  deployed `reload:` step in current with the lock, and the compose
  default (`up -d --wait --no-build --pull never`) without one; a
  `reload:` set only in the checkout is not what runs; refused on a marked
  environment (each mark kind), on a plain directory, with a missing
  variable, with a changed image reference, with a changed build arg
  (names the arg), with an unchanged one (goes through); `--vars set`'s
  hint in each of its three forms, and none on a plain directory; imports
  rewritten in `devopsy-target.env`; owners applied; the session gone
  during the checks cancels (nothing in the server log), gone during the
  run step goes on; the log joined under its anchor; a sync's holder
  refusing on a changed build arg before stopping anything; the local
  `--reload`.
- `internal/images`: the listing's new column round trip and `List`'s
  build hash (args hashed, order independent).
- e2e: none needed unless the session-gone case cannot be faked (the
  holder's tests fake it already).

### Docs

README: the flag in the flags list, a "Reload" subsection under
Releases (what it recomputes, what it refuses, that images need a
release), the Variables section pointing to it, config example unchanged.
AGENTS.md: a Reload section with the decisions above (why not the run
step, why a `reload:` step with a compose default, why `--check-images`
and where it also runs,
why no `--stop`). A `feat:` commit for the flag, `feat:` for the check
in the holder and the restore, `docs:`.

## Templates

- whoami, drupal11: remove `.devopsy/commands/reload`; README and
  comments say `devopsy @<target> --reload` after `--vars set`; no
  `reload:` key (the default is what their command did).
- traefik, registry: `reload: deploy` under `defaults:` (their deploy is
  what applies their variables, idempotent by design), README where it
  says to deploy after changing a setting or a password: `--reload`.
  Traefik's `restart` stays (it pulls: a different thing; a roadmap
  note).
- Workspace README, Conventions: one line ("Reload: `--reload` after
  changing variables; images only change with a release").

## Roadmap (workspace)

- devopsy-cli: compose's `up`/`start` words on a marked environment start
  the kept-down set: a guard on starting words in a marked release,
  like the bind-source start gate.
- devopsy-traefik: its `restart` pulls outside a release, the image held
  by no bookmark; replace with a scheduled release.

## Verification

- devopsy-cli: `go vet ./... && go test -count=1 ./...` locally and in
  Docker (Checks in AGENTS.md).
- By hand on an OrbStack machine (`devopsy-test-reload`): release
  whoami, `--vars set SITE_BASIC_AUTH=...`, `--reload`, the container
  recreated, `--log` shows the reload; drupal11 with `--vars set
  APP_UID=10002` refused naming `APP_UID`; a `--vars set` of an
  `image:` variable refused; a reload on a child after `--inherit`;
  Traefik's base domain changed, a site's `--reload` re-imports it
  (`--debug imports`). Delete the machine after.
- One review round after the build (as usual), fixes on the branch.
