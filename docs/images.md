# Images

What a release does with its images once it has fetched them
([Fetching images](releases.md#fetching-images)), and how a service's
data limits which image may run on it.

## Each release keeps its images

Once fetched, every image a release runs gets a second tag for that
release, `devopsy-<compose name>-<release id>`. A long compose name is
shortened, and a checksum is added when a release names one repository
twice. Images pulled by digest are tagged too. The names Compose runs
(`drupal11-prod-app`, `mariadb:11.8`) don't change.

devopsy never moves a pulled name back to an older image, because another
environment, instance or project may be running it.

## What changed

Each release says which images changed since the one it replaces. A
pulled image shows its version label (`org.opencontainers.image.version`)
when it has one, else its short digest, and a built one shows as
rebuilt.

In image mode it also warns when an image the project builds changed under
the same name (tag it by commit instead), and when one built over 30 days
ago has not changed since the release before. Its base may have patches,
so rebuild it with `--pull`.

## Choosing tags

Float third-party images on the series their upstream patches
(`mariadb:11.8`, `caddy:2-alpine`, `traefik:v3.7`). Every release pulls
them again, and fixes arrive with your next release, without a commit.

Do not use `latest`, or a major version whose data or configuration
format changes within it (`mariadb:11`), or a plain release would cross a
series. To move a service with data to a new series, raise its
`devopsy.data.version`, set from the same variable as its tag (Data and
its images, below).

Tag your own images by commit (image mode), or let devopsy build them
(build mode).

An environment nobody releases gets no patches. Release it on a schedule
([Environments nobody releases](servers.md#environments-nobody-releases)).

## Rollbacks bring back code

A rollback, or the restart after a failed `run`, brings back the release's
images exactly (what it built, from its tags, without building again), its
compose files, its variables and its commands. It does not bring back
data; snapshots do that.

A pulled image runs whatever the older release's name points to now. A
pin or a commit tag gives the image the release ran, and a floating tag
its current, patched image. Pin a stateless image whose upgrades you
test by hand (a tag that never moves, or a digest) when you want a
rollback to take it back.

A rollback that the data's version does not allow is refused (Rollback
against restore, below). The rollback says which services do not go
back, and why.

## Images go with their releases

Pruning a release removes its tags. An image that no other release still
tags, of any environment, instance or project on the server, also loses
the names that release ran and that still point to it (`mariadb:11.8.9`,
`registry/app:<commit>`), and Docker deletes it. Then `docker image prune
-f` removes what that left dangling, server-wide, including an untagged
image built by hand.

Kept releases, the ones a rollback goes to, keep their images, and a name
that has moved on to a newer image stays.

Docker refuses to remove a name a container uses, even a stopped one (a
`run` without `--rm`, a service dropped from the compose file). Such a
name stays, recorded in the project's directory (`.devopsy-images-left`),
which `--releases` lists. Every later pruning, `--releases remove` or
`--destroy` of the same project tries it again, and it goes once its
container does. A name another project's compose file uses, with no
container and no release of its own holding it, goes too; that project
pulls it again.

Images belong to the whole server. Removing them takes the server's image
lock alone, while fetches share it. Pruning never waits for the lock: when
another release holds it (devopsy names that release and for how long),
pruning skips this time, releases and images alike, and the next release
prunes them.

## Data and its images

A service with data (`devopsy.data=true`) changes its data in place. A new
image may upgrade it (MariaDB 11.8 to 12.1), and an older image may then
fail on it or damage it. devopsy cannot tell from an image what its data
needs, so every service with data states it in a required label. A
release refuses a service with data that lacks one, before it changes
anything.

- `devopsy.data.version=<version>` is its data's version: dotted numbers
  (`11`, `11.8`, `12.1.3`), compared as numbers, with missing parts
  counting as 0. You keep it up to date. Use the image's version, from one
  variable:

  ```yaml
  image: mariadb:${MARIADB_VERSION:-11.8}
  labels:
    - devopsy.data.version=${MARIADB_VERSION:-11.8}
  ```

  For a database the project builds (`FROM mariadb:${MARIADB_VERSION}`),
  use the same build argument for the label.
- `devopsy.data.version=any` turns the check off, for data that does not
  depend on its image (an app's uploads), or whose versions the project
  handles itself.

devopsy keeps, per environment, what ran on each service's data, in
`.devopsy-data-images` beside `shared/`: each run's version and, for a
pulled image, its tag. The tag is the part after the last `:`. The
repository is not compared, so a mirror of the same image counts as no
change.

A release, a rollback, a sync, an import and a `--no-code` restore all
apply the same rule. The label of what is about to run is compared with
the data's version, the highest that ran on it since its history last
started over:

| the label of what runs | |
|---|---|
| higher | runs |
| the same, with the same tag (a floating tag that moved upstream: its patches) | runs |
| the same, with another tag (a pulled image moved to another tag, the label left as it was: a pin bumped under a coarser label) | refused |
| the same, built (`build:`, in either mode: no tag is compared) | runs |
| lower | refused |
| `any` | runs, and the history starts over: a version set later is taken as it is |
| no version yet (a first release, after `any`, after an import of data of no known version) | runs, and its label is taken |

A refusal comes before anything is built, pulled or stopped. For a
rollback that is before its remote `before` and `prepare` steps; its
local `before` steps run before the server is asked, so they have run.
The refusal names the versions or tags, and the ways past it:

- raise the label, unless it is a rollback, whose code is the older
  release's;
- add `--accept-image <service>` (comma separated, or repeated) to the
  command you ran (`--release-sync`, `--reset`, keeping its `--from`). It
  runs the image anyway, and the history of that data starts over from
  it.

`--accept-image` lets through only what was refused; a service the rule
allows runs as it would have. `--release`, `--rollback`, `--release-sync`,
`--release-no-sync`, `--sync`, `--reset`, `--import` and `--snapshots
restore <id> --no-code` take it. `--yes` never accepts an image: it only
skips prompts.

Within one version, devopsy trusts the image names. Nothing is held by
digest, and a plain `up` follows the names, as for any service. devopsy
does not notice a tag pushed again onto an older image, or a stale
mirror: both are still the same tag.

## Rollback against restore

Since a rollback brings back code alone, a rollback past a data upgrade
(back to `mariadb:11.8` after a release of `12.1`) is refused. The refusal
names three ways forward: a new release that fixes the problem, the
release snapshot of the older release's data ([Release
snapshots](snapshots.md#release-snapshots)), or `--accept-image`.

`--snapshots restore <id>` brings data, code and images back together and
checks nothing. The snapshot's data and its history come back as the pair
they were, held as the history says (Holds, below).

`--no-code` brings back the data alone. It is checked against the current
release's labels, or, for a service that is held, against the image it
runs, since the snapshot's data is an earlier state of the same
environment. Then it is held as the restored history says. When it would
need a built image that is gone, it is refused before any service stops,
since it builds nothing, and the refusal names the full restore, which
builds.

## Holds

A service with data is held, that is, run on an image other than the one
its release names, exactly when the rule would refuse the release's image
on its data. It is then held on the image its data's history ends on: by
digest when that image was pulled, and by name when it was built (the
name of the release that built it, when this release pulls the service
instead).

A hold is never carried over. devopsy works it out again from the
history each time it decides
what a service with data runs: every activation, the restart after a
failed run step, a full or a `--no-code` restore, a sync, an import, a
reset. A hold ends as soon as the data allows the release's image again.

A release or a rollback is refused or accepted before it runs, so it holds
nothing. The restart after a failed run step is the one place that cannot
refuse, since it runs on its own, past the switch. There the previous
release runs on the image the failed release ran on the data, until a new
release fixes the problem. The release's output and `--status` (`held
on`) say so. The failed release's `--accept-image` does not reach the
restart: it accepted the new image, and the restart would put the old one
back on the new data.

- A full restore of a snapshot taken during such a hold holds the same
  way.
- A rollback to a release that was held runs the image that release
  names. That image stays on the server through the hold, and is built
  again only when gone. A `--reload` of such a release compares the
  variables with the release's image, and ignores the held one.
- When the restart's images cannot be restored at all (the server's image
  lock held elsewhere for 10 minutes, Docker failing), the service is
  still held: a pulled image by its digest, a built one by the bookmark
  of the release that built it. Without a bookmark, the release's output
  names the service, and says a new release fixes it.
- A child after a release that syncs, whose run step failed before its
  database ran, holds the same way. Its data is then the parent's, and a
  database the parent builds ran on an image only the parent's server has.
  The child runs an image of its own server that the data allows, usually
  the failed release's. With none, the previous release's image runs on
  it, with a message, until a new release fixes the problem.

## Syncs and imports

A sync or an import brings data from somewhere else, at that source's
version ([Parent environments and syncs](syncs.md),
[`--export` and `--import`](data.md#--export-and---import)). For every
service whose files the copy writes (`volumes` ones, `any` included), the
receiving side checks its labels against the source's data first, as a
release does, while every service still runs.

The receiving side always runs the image its release names on the copy.
A hold it had goes with its old data, and once the copy is complete, its
history of that data becomes the source's. A service that is held, and
whose release's built image is gone, is refused while everything still
runs, since a copy builds nothing.

A `custom` copy, a sync's or an import's, checks nothing, and leaves the
service's history and what it runs as they were. Its script loads the
dump through the image that runs, so the data's files are that image's,
whatever version the source had. The same holds for a `custom`
`--resolved`.

For a sync:

- A service whose entries are all missing on the parent gets nothing
  written, and keeps the child's history.
- A child below the parent's version, or on another tag at the same
  version, is refused on every sync, unless `--sync --accept-image
  <service>`. That is how to try an upgrade path on the parent's data,
  one command per sync, the history starting over.
- A parent released between the sync's start and its copy refuses the
  copy before any service stops. Sync again.

For `--export` and `--import`:

- An `--export` of a `volumes` service carries its data's history in the
  tar's first header, which `tar` itself skips. On a target, that is the
  history as a sync's parent sends it; locally, its label's version and
  tag alone. A `custom` export is its script's output and carries none.
- An `--import` of a `volumes` service on a target checks the
  environment's labels against that history before any service stops, as
  a sync does, and takes it over once the data is in. When refused,
  `--import <service> --accept-image <service>` runs it anyway, the
  history starting over.
- A tar without the header, and a `volumes` service's `--resolved` (unless
  the import took the history over before what failed), start the
  service's history over, as data of no known version. What runs on it
  next sets its version: the service's label if it runs once the import
  is in, else the next release's. Locally there is no history.

An import, a sync and a `--no-code` restore read a service's labels as
they are now. They refuse a service whose image the labels now name
differently than its release fetched it (a variable set since), until a
release fetches it.
