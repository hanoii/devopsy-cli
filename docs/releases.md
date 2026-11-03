# Releases

A release uploads the project into a new directory on the server, checks
it, gets its images, switches `current` to it and runs its steps. A
rollback switches back to an older release. The commands are listed in
[Servers](servers.md).

In order, a release:

1. runs its local `before` steps;
2. uploads the project into a new release directory;
3. runs its remote `before` steps in the current release, then its
   `prepare` steps in the new one;
4. checks it: variables, names, networks and mounts;
5. gets its images;
6. switches `current` to it, from where it no longer depends on your
   connection (a release that syncs, once its copy is done);
7. gives the entries of `mnt` to their owners;
8. on a child, syncs its parent's data, when it syncs;
9. runs its `run` step;
10. prunes old releases, then runs its remote, then its local `after`
    steps.

A failure up to the switch leaves `current` as it was.

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

`--release` and `--rollback` refuse without the steps the environment
names. When it names none, devopsy prints a starting point. A release's
steps come from your checkout, which is what it uploads. A rollback's
remote steps, the restart after a failed `run` and `--destroy`'s step
come from the release they act on, from its own `config.yaml` on the
server. A rollback's local `before` and `after` steps are your
checkout's.

The remote steps share one SSH session under the release lock, which is
why the local `before` steps run first and the local `after` steps last.
Remote `before` steps run in the current release, with its commands. A
first release has no current one, so none run.

Local steps get the environment's `env`, `DEVOPSY_RELEASE_COMMIT` and,
for a child, `DEVOPSY_PARENT` and `DEVOPSY_PARENT_TARGET`, never the
server's `.env`. They never get `DEVOPSY_TARGET`, `DEVOPSY_ENVIRONMENT`
or `DEVOPSY_INSTANCE`: those mean a release on a server, where they come
from `devopsy-target.env`, and an `env` capability can tell local work
from a release by them. A step that needs the address is a command with
`## Needs: target`, which gets it in `DEVOPSY_COMMAND_TARGET`
([Project commands](local.md#project-commands)).

## Checks before the switch

After the `prepare` steps, which may make the secrets the compose files
require, devopsy runs these checks. Each one that fails fails the release
before it goes live.

- Variables: every variable compose requires (`${VAR:?message}`) must be
  set there, the env capability's included. The failure lists the missing
  variables and the `--vars set` line. Generate secrets in `prepare` so
  the check finds them, and name them in `secrets:`.
- Names: no containers of the release's compose project may come from
  another environment's directory. Names are joined with `-`, so instance
  `a-b` of `c` and instance `a` of `b-c` are both `<project>-a-b-c`, and
  Compose would take the other's containers and volumes as its own.
  Rename an instance or an environment.
- Networks: a network the compose file names as its own (not
  `external`) must not belong to another compose project on the server:
  Compose would silently share it, and the two projects' containers would
  reach each other by service name. Name a project's networks from
  `COMPOSE_PROJECT_NAME`; to reach another project, join its network as
  `external`.
- Mounts: no writable bind mount in the release's tree, no data or owner
  without an entry of `mnt`, no entry with two owners, no `./mnt` whole as
  data, no bind mount from below an owned entry
  ([The mounts check](data.md#the-mounts-check)).

The names, networks and mounts checks come after the variables check:
compose cannot read its files while a variable they require is missing.

## Fetching images

After the checks, devopsy gets the release's images on the server, by its
`mode:`, before it goes live:

- In build mode, `docker compose --profile '*' pull --ignore-buildable`
  for the services without a `build:`, then `docker compose build --pull`
  for the rest, their base images refreshed.
- In image mode, `docker compose pull` for every service with an
  `image:`, buildable ones included, so a tag missing from the registry
  fails the release there; then the same `--profile '*' pull
  --ignore-buildable`.

Every profile's services without a `build:` are pulled on every release,
so a tool run now and then (`devopsy --profile tools run --rm jq`) gets
its tag's fixes too, and one that fails to pull fails the release like
any other. They are only pulled, not kept as the release's own images
([Each release keeps its images](images.md#each-release-keeps-its-images)).
A profile's buildable services are built when they are used.

The run step's `up` then finds every image there, so a `deploy` needs no
`build` or `pull` of its own.

A failure leaves `current` as it was, its images included. What the
failed release got is tagged as its own, so it is removed with it. The
same goes for what an interrupted release got: a Ctrl-C or a lost
connection (Dropped connections, below) stops the build or pull, and the
release is marked failed. If the server never hears of the drop, the
release stays marked fetching until the next release, `--destroy` or
`--releases remove` marks it failed.

## Owners

Right after the switch, before a sync and the run step, the entries of
`mnt` that the labels own are created when missing and given to their
owners (`devopsy.mnt.owner`, [Owners](data.md#owners)). A failure there
fails the release as its run step would.

## When `run` fails

devopsy first reports the state the release's containers were left in,
in the release's log too, so `--log` shows why: each container's state
and health and, for every container neither running and healthy nor
exited 0, its last health checks and its last 100 log lines.

Then `current` goes back to the previous release, which is restarted with
its own run step from its own config (its `rollback:` run step, else its
`release:` one). It gets its images as a rollback does: what it built,
and a data service held on an image its data allows ([Rollbacks bring
back code](images.md#rollbacks-bring-back-code),
[Holds](images.md#holds)). Data changes, like database updates, are the
project's to undo.

- When one of the previous release's bind sources now leads out of its
  entry through a link, the previous release stays down, with a message,
  and what the failed run step started keeps running until a release goes
  live.
- A first release has nothing to go back to: it takes down what it
  started (`down`, volumes and `mnt` kept), and `--status` says the
  release failed.
- A run step that fails while Docker does not answer (the server
  shutting down, which stops the step, or the daemon gone) is not taken
  as a failure of the release: nothing goes back or restarts, since that
  could only fail too. The release is left as one cut off past its switch
  (below). `--status` names it, and the next `--release` finishes it once
  Docker is back.

A run step that fails leaves what it started. When a container is left
restarting, Docker recreates its bind mounts under `current` as
directories, as root. The next release then refuses until that container
is taken down and the directory removed, and says how.

## A child's first release

A child's first release syncs its parent's data between switching
`current` and `run`, so `run` (a `deploy`: database updates, cache clears)
works on the parent's data and no empty site is ever served. If the copy
fails, the release does not go live.

- `--release-sync` syncs on any release. On a child already released it
  asks for the environment's name first, as `--sync` does (`--yes` skips
  it).
- `--release-no-sync` never syncs, a child's first release included.
- `--from @<address>` syncs such a release from another environment of
  the project instead of the parent: on a child only, and only on a
  release that syncs.

What a sync copies is in [Parent environments and syncs](syncs.md).

## Dropped connections

Before the switch, a release depends on your session:

- A session that drops (a cancelled CI job, a killed devopsy) cancels the
  release at once: the server notices and undoes what it fetched.
- A connection that dies silently (a laptop asleep or off the network)
  cancels it within about 30 seconds: devopsy sends the server a
  heartbeat, and silence counts as the session gone, as does devopsy
  itself being stopped that long. A release that syncs waits 90 seconds.
  Syncs and `--export` do the same; `--import` waits for 2 minutes of
  silence.
- Ctrl-C cancels the release.

From the switch on, a release no longer depends on your connection: it
finishes, or fails and brings the previous release back, on its own, and
`--log` shows how. A Ctrl-C then does not stop it. The release goes on to
its end (the run step, pruning, the remote `after` steps) and devopsy
says so, at a terminal or in CI alike. A second Ctrl-C only ends your
session. To go back, run `--rollback` once the release has ended.

A release that syncs is the exception during its copy: a Ctrl-C there
aborts the sync, and the release goes back
([Parent environments and syncs](syncs.md)).

Only something that kills devopsy on the server outright (the OOM killer,
a reboot) stops a release past its switch. The release is then current,
but its run step may not have run. `--releases` marks it `CUT`, and
`--status` says so and names what finishes it:

- for a release, the release again, or on a child a `--sync`, which runs
  the release's run step again (the way out when a cut sync refuses the
  release);
- for a rollback, `--rollback` again, which finishes that rollback rather
  than going one release further back (a release instead leaves it as it
  is, marking the release it left rolled back);
- for a restore, the restore again, or on a child a `--sync`.

## Logs

Each release and rollback is recorded on the server, in the release's
directory, failed ones included as long as the server was reached. A
rollback's and a sync's logs are added to the release they go to. There
are two logs:

- `devopsy-log-session`: what your session said and saw up to the switch,
  or up to the run step after a sync: devopsy's messages, the local steps
  run, the server's lines, the copy's. Each line starts with its time in
  UTC (`2026-10-09 15:24:32`).
- `devopsy-log-server`: what the server printed from there on, written
  there whatever happens to your connection.

`--log` shows the two joined, with progress displays reduced to their
final state: the most recent release's, or one release's with `--log
<id>`.
Local steps' own output stays on your screen only, so they keep their
terminal.

## Rollbacks

`--rollback` goes back to the newest release older than the current one
that went live and was not rolled back, and runs its rollback steps
([Rollbacks bring back code](images.md#rollbacks-bring-back-code) says
what comes back). It marks the release it leaves, shown as `ROLLED BACK`
in `--releases`. Like a failed release, that release never counts toward
`keep`, and a later rollback never goes back to it, so once a fix is
released, the next rollback skips the bad release. Its log stays until
pruning removes it.

## Build and image modes

`mode:` in the config chooses how a release gets its images:

- Build mode, the default, uploads the project as git sees it,
  uncommitted changes included, and builds on the server before the
  release goes live. It needs a git repository: git's ignore rules decide
  what goes (`vendor/`, dumps and local secrets stay behind). A rollback
  runs the image its release built, built again from its code when a
  build argument changed since (`--vars set APP_UID`), before it goes
  live.
- Image mode uploads `.devopsy/` only, and the images come from a
  registry. devopsy pulls them before the release goes live, so build and
  push them first, as a local `before` step for example. Tag them with
  the commit, `image: ghcr.io/me/app:${DEVOPSY_RELEASE_COMMIT:-local}`, so
  a rollback runs its own image. Image mode needs no git: without it the
  release just has no commit.

Either way, files arrive with git's modes, whatever the checkout's: 0755
when executable, else 0644 (a checkout synced by ddev's Mutagen has 0600
files, which a container running as another user cannot read). A mode
the project needs is set in its image (`COPY --chmod`) or by its
`deploy`.

## Uploads

A release says how much it uploads, shows the upload's progress on one
line while it runs (at a terminal only: CI gets none), then its id, size
and time, which the release log keeps. The server names the release by
its own UTC second, always later than every release already there, so
releases from laptops and CI with different clocks stay in order.

## On the server

On the server an environment holds `releases/`, `current` and `shared/`.

Each release is a directory, `releases/<id>/`. The project as uploaded is
in its `code/`, where `current` points and commands run. devopsy's own
files sit beside it, never in the project's tree, so a build context only
ever sees the project:

- `devopsy-release.json`: the commit, branch and who made the release;
- `devopsy-target.env`: its environment;
- `devopsy-images`: its images, with each one's digest and version;
- `devopsy-compose.yaml` (below);
- `devopsy-log-session` and `devopsy-log-server`;
- `devopsy-pending` until it goes live, `devopsy-fetching` while it gets
  its images, `devopsy-failed` when it never went live or its run
  failed, and `devopsy-rolled-back` once a rollback left it.

Everything in `shared/` is linked into each release's `.devopsy/`: `.env`
for secrets and overrides, `mnt/` for data (bind mounts like
`./mnt/data`), and anything else you put there (a
`compose.override.yaml`). Your local `.env`, `mnt/` and override files
are never uploaded.

A release that fails before going live stays, marked failed, a first one
too. `devopsy @<target> --destroy` removes an environment that never went
live, and `--releases remove failed` the release alone.

`devopsy-compose.yaml` is devopsy's own compose file, the only one it
writes. It holds only facts of the release's last activation: the image
each service was given (a `devopsy.digest` label), and the data services
held on another image (a `devopsy.held` label, naming the image that
holds them: [Holds](images.md#holds)). devopsy runs compose with it
between the project's `compose.yaml` and your `compose.override.yaml`, so
your override still wins; `devopsy -- config` shows the result. Run
compose on a server through devopsy: a plain `docker compose up` lacks the
file, so it recreates every container once and goes by the images'
names.

## Removing releases by hand

`--releases remove <id>...` removes releases as pruning does, their
images included. A release can be named by the start of its id. `failed`
stands for every release that never went live, whose run failed, or that
a rollback left, and can be given beside ids. A release cut off before
its switch counts as failed only when it is older than the current one,
since a newer one may still be between its upload and its activation:
name that one by id.

The current release is refused, and every id is checked before anything
is removed. It asks first at a terminal, and takes `--yes` in scripts.
Data in `shared/` is never touched.

## Destroying

`--destroy` removes an environment, data included. Under the release
lock, in the current release (else the newest one), it runs:

1. the environment's `destroy:` step, if it has one: one devopsy command,
   run while everything still runs (a last dump, for example);
2. `docker compose down --volumes --remove-orphans --rmi local`, every
   profile, so the images built without an `image:` name go too;
3. `shared/mnt` emptied as root in `devopsy-util`, since its files belong
   to container users;
4. each release's images, as pruning removes them;
5. the environment's directory: its releases, and `shared/` with its data
   and `.env`.

It takes the server's image lock before removing anything, waiting up to
10 minutes for releases that are fetching images, and fetches wait while
it runs. A failure before the removal removes nothing.

The project's and the instance's `.env` stay, since their environments
share them. When no environment is left in their directory, `--purge`
removes them too (at a terminal devopsy asks; otherwise a warning says
how), even once the environment is already gone.

## Reloads

`devopsy @<target> --reload` applies what changed since the release
without another release: its variables (`--vars`, `--inherit`, an edit
of `shared/.env`) and the facts it imports (a proxy's base domain, which
every site imports again with its own reload, not a release).

It is one session under the release lock, in the current release, and it
changes nothing until everything is checked:

- it is refused on data a sync, an import or a restore did not complete,
  as a release is, and on a release cut off past its switch, saying what
  finishes it;
- the imports are read again, and compose's required variables, names
  and mounts checked as before a release's switch;
- every service's image is checked: a reload runs the images the release
  fetched and never builds or pulls one. It refuses a variable that now
  names another image, changes a build argument (named, never its value)
  or turns a profile on, and names `--release` as the way.

A lost session or a Ctrl-C before then changes nothing. From there on the
reload goes on whatever happens to the session (`--log` shows it): the
imports are put in place, the entries of `mnt` go to their owners (an
owner label may come from a variable), then the release's `reload:` step
runs, read from its own `config.yaml` on the server, as a rollback's
steps are:

```yaml
environments:
  prod:
    reload: deploy    # what applies this project's variables; it must be safe to run again
```

Without a `reload:` step, devopsy runs its own `up -d --wait --no-build
--pull never`, which recreates what changed. It never runs the run step
instead: a sync reruns that because its data moved, but a run step may do
far more than apply variables (a schema update). A reload that fails,
through its `reload:` step or devopsy's own `up`, leaves the release live
and restarts nothing.

Locally, `devopsy --reload` runs the `reload:` step under `defaults:` in
place (in a release on a server, its environment's), else the same `up`,
once compose's required variables are set.

A `--sync` and a `--snapshots restore --no-code` check the images the
same way before anything stops, since both run the current release's run
step on its images again. With `--with-env` the restore checks them under
the snapshot's variables, and when they fail it is refused, with the full
restore as the way. A full restore with `--with-env` reads the snapshot's
variables from its checks on, so its images are fetched for them.
