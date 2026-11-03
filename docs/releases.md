# Releases

A release uploads the project into a new directory on the server, checks it, gets its images, switches `current` to it and runs its steps; a rollback switches back to an older one. The commands are in [Servers](servers.md).

## Steps

Steps, in `release:` and `rollback:`, run in phases. Each is a devopsy
command line:

```yaml
release:
  before:                # before anything changes; a failure stops here
    - local: image       #   on this machine, first
    - remote: maint on   #   on the server, in the current release
  prepare: [secrets]     # on the server, in the new release, before it goes live
  run: deploy            # once it is current: the release itself
  after:                 # once live; a failure is only reported
    - remote: maint off  #   on the server, first
    - local: notify      #   then locally
rollback: deploy         # only a run step: the same phases, in the restored release
```

### One SSH session

The remote steps share it, under the release lock.
Hence local before steps first and local after steps last. Remote before
steps run in the current release, with its commands, and none run on a
first release.

### Local steps

Local steps get the environment's `env`, `DEVOPSY_RELEASE_COMMIT`
and, for a child, `DEVOPSY_PARENT` and `DEVOPSY_PARENT_TARGET`, never
the server's `.env`. Never `DEVOPSY_TARGET`, `DEVOPSY_ENVIRONMENT` or
`DEVOPSY_INSTANCE`: those mean a release on a server, where they come
from `devopsy-target.env` (an `env` capability can tell local work
from a release by them). A step that needs the address is a command
with `## Needs: target`, which gets it in `DEVOPSY_COMMAND_TARGET`.
Without steps, devopsy prints a starting point.

## Checks before the switch

### The variables check

After `prepare`, devopsy checks that every
variable compose requires (`${VAR:?message}`) is set there, env capability
included. A missing one fails the release before it goes live, listing the
variables and the `--vars set` line. Generate secrets in `prepare` so the
check finds them, and name them in `secrets:`.

### The names check

Containers of the release's compose project from
another environment's directory fail the release before it goes live,
after `prepare` and the variables check (compose cannot read its
files while a variable they require is missing). Names are joined
with `-`, so instance `a-b` of `c` and instance `a` of `b-c` are both
`<project>-a-b-c`, and Compose would take the other's containers and
volumes as its own. Rename an instance or environment.

### The networks check

A network the compose file names as its own
(not `external`) that belongs to another compose project on the server
fails the release before it goes live, after `prepare` and the
variables check: Compose would silently share it, and the two
projects' containers would reach each other by service name. Name a
project's networks from `COMPOSE_PROJECT_NAME`; to reach another
project, join its network as `external`.

### The mounts check

A writable bind mount in the release's tree, data
or an owner with no entry of `mnt`, an entry with two owners, `./mnt`
whole as data, a bind mount from below an owned entry, fail the
release before it goes live, after `prepare` and the variables check
([The mounts check](data.md#the-mounts-check)).

## Owners

Right after the switch, before a sync and the run
step, the entries of `mnt` the labels own are created when missing and
given to their owners (`devopsy.mnt.owner`, [Owners](data.md#owners)); a
failure fails the release as its run step would.

## Fetching images

After the checks, devopsy gets the release's images, by
`mode:`, on the server and before it goes live: in build mode `docker
compose --profile '*' pull --ignore-buildable` (services without a
`build:`) then `docker compose build --pull` (the rest, base images
refreshed); in image mode `docker compose pull`, every service with an
`image:`, buildable ones included, so a tag missing from the registry
fails the release there, then the same `--profile '*' pull
--ignore-buildable`. Every profile's services without a `build:` are
pulled on every release, so a tool run now and then (`devopsy
--profile tools run --rm jq`) gets its tag's fixes too, and one that
fails to pull fails the release like any other; they are only pulled,
not kept as the release's own images ([Each release keeps its images](images.md#each-release-keeps-its-images)), and a profile's
buildable services are built when used. The run step's `up` then finds them all;
a `deploy` needs no `build` or `pull` of its own. A failure leaves
`current` as it was, its images included; what the failed release got
is tagged as its own, so it goes with it. So does an interrupted one
(Ctrl-C, a lost connection; see [Dropped connections](#dropped-connections) below): the build
or pull stops, and the release is marked failed. A drop the server
never hears of leaves it marked fetching until the next release,
`--destroy` or `--releases remove` marks it failed.

## When `run` fails

When `run` fails, devopsy first says what the release's containers
were left as, into the release's log too, so `--log` shows why: each
container's state and health, then, for every one neither running and
healthy nor exited 0, its last health checks and its last 100 log
lines. Then `current` goes back to the previous release, which
is restarted (unless one of its bind sources now leads out of its
entry through a link: then it stays down, what the failed run step
started running on, said, until a release goes live) with its own run step, from its own config (its `rollback:`
run step, else its `release:` one), with its images as a rollback gets
them ([Rollbacks bring back code](images.md#rollbacks-bring-back-code), [Holds](images.md#holds): what it built back, a data service held on an image its
data allows). Data changes, like database updates, are the project's
to undo. A first release has nothing to go
back to: it takes down what it started (`down`, volumes and `mnt`
kept), and `--status` says the release failed. A run step that fails
while Docker does not answer (the server shutting down, which stops
the step, or the daemon gone) is no failure of the release's: nothing
goes back or restarts, which could only fail too, and the release is
left as one cut off past its switch, which `--status` names and the
next `--release` finishes once Docker is back.

## A child's first release

A child's first release syncs its parent's data between switching
`current` and `run`, so `run` (`deploy`: database updates, cache clears)
works on the parent's data and no empty site is ever served. A failed
copy does not go live. `--release-sync` does that on any release,
asking for the environment's name first when it already has one, as
`--sync` does (`--yes` skips it); `--release-no-sync` never syncs, a
child's first release included (see [Parent environments and syncs](syncs.md)).
`--from @<address>` syncs such a release from another environment of
the project instead of the parent; a child's only, and only on a
release that syncs.

## Dropped connections

Before the switch, a session that drops (a
cancelled CI job, a killed devopsy) cancels the release at once: the
server notices and undoes what it fetched. A connection that dies
silently (a laptop asleep or off the network) cancels it within about 30
seconds: devopsy sends the server a heartbeat, and a silence counts as
the session gone (so does devopsy itself stopped that long; 90 seconds
for a release that syncs). Syncs and `--export` do the same; `--import`
counts 2 minutes of silence.
From the switch on, a release no longer depends on your connection: it
finishes, or fails and brings the previous release back, on its own, and
`--log` shows how. Ctrl-C before the switch cancels the release. After
it, the release goes on to its end (the run step, pruning, the remote
after steps): devopsy says so, at a terminal or in CI alike, and a
second Ctrl-C only ends the session. To go back, `--rollback` once it
ends. A release that syncs is the exception during its copy: Ctrl-C
there aborts the sync, and the release goes back ([Parent environments
and syncs](syncs.md)). Only something that kills devopsy on the server outright
(the OOM killer, a reboot) stops it there: then its release is
current, but its run step may not have run. `--releases` marks it
`CUT`, and `--status` says so with what finishes it: the release
again (a child's `--sync`, which runs the release's run step again,
finishes it too, and is the way when a cut sync refuses the release); for a rollback, `--rollback`
again, which finishes that rollback rather than going one release
further back (a release instead leaves it as it is, the release it
left marked rolled back); for a restore, the restore again, or a
child's `--sync`.

## Logs

Each release and rollback is recorded on the server, in the
release's directory (a rollback's and a sync's added to the release they
go to), failed ones included once the server was reached, in two logs:
`devopsy-log-session`, what your session said and saw up to the switch,
or to the run step after a sync (devopsy's messages, the local steps
run, the server's lines, the copy's), each line after its time in UTC
(`2026-10-09 15:24:32`), and `devopsy-log-server`, what the server
printed from there on, written there whatever happens to your
connection. `--log` shows the two joined, progress displays reduced to
their final state: the most recent, or `--log <id>` one release's. Local
steps' own output stays on your screen only, keeping their terminal.

## Rolled back

`--rollback` marks the release it leaves (`ROLLED
BACK` in `--releases`): like a failed one it never counts toward
`keep` and a later rollback never goes back to it, so after a fix is
released, the next rollback skips the bad release. Its log stays until
pruning takes it.

## Removing releases by hand

`--releases remove <id>...` takes
releases away as pruning does, images included; an id can be its start,
and `failed` is every release that never went live (one cut off before
its switch only when older than the current one: a newer one may be
between its upload and its activation, so name it by id), whose run
failed, or that a rollback left, ids alongside. The current release is
refused, and every id is checked before anything goes. It asks first
at a terminal, and takes `--yes` in scripts. Data in `shared/` is never
touched.

## Destroying

`--destroy`, under the lock, in the current release
(else the newest one): the environment's `destroy:` step if it has one
(optional, one devopsy command, while everything still runs: a last
dump), then `docker compose down --volumes --remove-orphans --rmi local`
(every profile; the images built without an `image:` name go too), then
`shared/mnt` emptied as root in `devopsy-util`, since its files belong
to container users; then each release's images, as pruning removes them;
then the directory (releases, `shared/` with its data and `.env`). It
takes the server's image lock before anything goes, waiting up to 10
minutes for releases fetching images, and fetches wait while it runs. A
failure before the removal removes nothing. The project's and the
instance's `.env` stay, shared by their environments; when none is left
in their directory, `--purge` removes them with it (at a terminal
devopsy asks; otherwise a warning says how), also after the environment
is gone.

## Build and image modes

Two modes, `mode:` in the config:

- build mode (default) uploads the project as git sees it, uncommitted
  changes included, and builds on the server, before the release goes
  live. It needs a git repository: git's ignore rules decide what goes
  (`vendor/`, dumps and local secrets stay behind).
- image mode uploads `.devopsy/` only; images come from a registry. Tag
  them with the commit,
  `image: ghcr.io/me/app:${DEVOPSY_RELEASE_COMMIT:-local}`, so rollbacks
  run their own image. In build mode a rollback runs the image its
  release built, built again from its code when a build argument changed
  since (`--vars set APP_UID`), before it goes live. devopsy pulls them before the release
  goes live: build and push them first, like a local before step.

Image mode needs no git: without it the release just has no commit.
Either way files arrive with git's modes, whatever the checkout's: 0755
when executable, else 0644 (a checkout synced by ddev's Mutagen has 0600
files, which a container running as another user cannot read). A mode a
project needs is set in its image (`COPY --chmod`) or by its `deploy`.

## Uploads

A release says how much it uploads, shows the upload's progress on one
line while it runs (on a terminal only: CI gets none), then its id, size
and time, which the release log keeps. The server names it: its own UTC
second, always later than every release already there, so releases from
laptops and CI with different clocks stay in order.

## On the server

On the server, an environment holds `releases/`, `current` and `shared/`.
Each release is a directory, `releases/<id>/`, with the project as
uploaded in `code/`, where `current` points and commands run, and
devopsy's own files beside it, never in the project's tree, so a build
context only ever sees the project: `devopsy-release.json` (commit,
branch, who), `devopsy-target.env` (its environment), `devopsy-images`
(its images, with each one's digest and version), `devopsy-compose.yaml`
(below), `devopsy-log-session` and `devopsy-log-server`,
`devopsy-pending` until it goes live, `devopsy-fetching` while it gets
its images, `devopsy-failed` when it never went live or its run failed,
and `devopsy-rolled-back` once a rollback left it.

Everything in `shared/` is linked into each release's `.devopsy/`: `.env`
for secrets and overrides, `mnt/` for data (bind mounts like `./mnt/data`),
anything else (a `compose.override.yaml`). Local `.env`, `mnt/` and override
files are never uploaded. Each release records its commit, branch and who
made it. A release that fails before going live stays, marked failed, a
first one too: `devopsy @<target> --destroy` removes an environment that
never went live, `--releases remove failed` the release alone.
`devopsy-compose.yaml` is devopsy's own compose file, the only one it
writes, holding only facts of the release's last activation: the image
each service was given (a `devopsy.digest` label) and the data
services held on another image (a `devopsy.held` label, the image it
holds them by: [Holds](images.md#holds)). devopsy runs compose with it between the project's
`compose.yaml` and your `compose.override.yaml`, so your override still
wins; `devopsy -- config` shows the result. Run compose on a server
through devopsy: a plain `docker compose up` lacks it, recreates every
container once and follows the names.
One that fails in its `run` step leaves what the step started: a container
left restarting recreates its bind mounts under `current` as a directory
(Docker does, as root), and the next release refuses until it is taken
down and that directory removed, saying how.

## Reloads

`devopsy @<target> --reload` applies what changed since the
release without another one: its variables (`--vars`, `--inherit`, an
edit of `shared/.env`) and the facts it imports (a proxy's base domain:
every site re-imports with its own reload, not a release). It is one
session under the release lock, in the current release, and changes
nothing until everything is checked: refused on data a sync, an import
or a restore did not complete (as a release is), then the imports read
again, compose's required variables, names and mounts checked as before
a release's switch, and every service's image: a reload runs the images
the release fetched, never building or pulling one, so a variable that
now names another image, changes a build argument (named, never its
value) or turns a profile on is refused, `--release` as the way. A
session gone or a Ctrl-C until then changes nothing. From there it goes
on whatever happens to the session (`--log` shows it): the imports in
place, the entries of `mnt` to their owners (an owner label may be a
variable), then the release's `reload:` step, from its own
`config.yaml` on the server like a rollback's:

```yaml
environments:
  prod:
    reload: deploy    # what applies this project's variables; it must be safe to run again
```

Without one, devopsy's own `up -d --wait --no-build --pull never`, which
recreates what changed. Never the run step on its own: a sync reruns it
because its data moved, but a run step may do far more than apply
variables (a schema update). A step that fails leaves the release live
and restarts nothing. A release cut off past its switch is refused, with
what finishes it. Locally, `devopsy --reload` runs `defaults:`'
`reload:` step in place (in a release on a server, its environment's),
else the same up, once compose's required variables are set. A `--sync` and a `--snapshots restore --no-code`
check the images the same way before anything stops: both run the
current release's run step on its images again (`--with-env`: under the
snapshot's variables, refused then with the full restore as the way).
A full restore `--with-env` reads the snapshot's variables from its
checks on, so its images are fetched for them.
