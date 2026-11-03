# Images

What a release does with its images once it has fetched them ([Fetching images](releases.md#fetching-images)), and how data limits which image runs on it.

## Each release keeps its images

Once fetched, every image the release
runs gets a second tag, `devopsy-<compose name>-<release id>` (a long
compose name shortened, a checksum added when a release names one
repository twice), without changing the names Compose runs
(`drupal11-prod-app`, `mariadb:11.8`), images pulled by digest included.
devopsy never moves a pulled name back: another environment, instance or
project may run it.

## Rollbacks bring back code

A rollback, or the restart after a
failed `run`, brings back the release's own images exactly (what it
built, from its tags, without building again), its compose files,
variables and commands. Any pulled image runs what the older release's
name gives now: a pin or a commit tag is the image it ran, a floating
tag its current, patched one. Data is never undone (that is for
snapshots), and a rollback the data's version does not allow is
refused (below). The rollback says each service that does not go back,
and why. Pin an image (a tag that never moves, or a digest) when you
want a rollback to take it back.

## What changed

Each release says which images changed since the
one it replaces, a pulled image by its version label
(`org.opencontainers.image.version`) when it has one, else by short
digest, a built one as rebuilt. In image mode it warns when an image
the project builds changed under the same name (tag it by commit), and
when one built over 30 days ago has not changed since the release
before: its base may have patches, so rebuild it with `--pull`.

## Images go with their releases

Pruning a release removes its tags.
An image no other release still tags, of any environment, instance or
project on the server, also loses the names that release ran that still
point to it (`mariadb:11.8.9`, `registry/app:<commit>`), so Docker
deletes it; then `docker image prune -f` removes what that left dangling
(server-wide: an untagged image built by hand goes too). Kept releases,
the ones a rollback goes to, keep theirs, and a name that moved on to a
newer image stays. Docker refuses a name a container uses, even a
stopped one (a `run` without `--rm`, a service dropped from the compose
file): it stays, recorded in the project's directory
(`.devopsy-images-left`), which `--releases` lists, and every later
pruning, `--releases remove` or `--destroy` of the same project retries
it, so it goes once its container does. A name another project's compose
file uses, with no container and no release of its own holding it, goes
too; that project pulls it again. Images are the server's, so removing
them needs the server's image lock alone, while fetches share it:
pruning never waits for it, and when another release holds it (devopsy
names it and for how long), skips this time, releases and images, and
the next release prunes them.

## Choosing tags

Float third-party images on the series upstream
patches (`mariadb:11.8`, `caddy:2-alpine`, `traefik:v3.7`): every release
pulls them, so fixes arrive with your next release and no commit. Never
`latest`, and never a major whose data or configuration format changes
(`mariadb:11`): a plain release would cross series. A series bump is a
higher `devopsy.data.version` for a service with data, from the same
variable as its tag (below). Tag your own images by commit
(image mode) or let devopsy build them (build mode). Pin a stateless image
whose upgrade you test by hand, by tag or by digest, when you want a
rollback to take it back. An environment nobody releases gets no patches:
release it on a schedule ([Environments nobody releases](servers.md#environments-nobody-releases)).

## Data and its images

A service with data (`devopsy.data=true`) changes its data in place: a
new image may upgrade it (MariaDB 11.8 to 12.1), and an older image may
then fail on it or damage it. devopsy cannot tell from an image what its
data needs, so every data service says it, with a label that is
required (a release refuses one without it, before anything changes):

- `devopsy.data.version=<version>`: its data's version, dotted numbers
  (`11`, `11.8`, `12.1.3`) compared numerically, missing parts as 0. You
  keep it up to date; best practice is the image's own version, from one
  variable:

  ```yaml
  image: mariadb:${MARIADB_VERSION:-11.8}
  labels:
    - devopsy.data.version=${MARIADB_VERSION:-11.8}
  ```

  and for a database the project builds (`FROM mariadb:${MARIADB_VERSION}`),
  the same build argument and label.
- `devopsy.data.version=any`: devopsy checks nothing, for data that does
  not depend on its image (an app's uploads), or whose versions the
  project handles itself.

devopsy keeps, per environment, what ran on each data service's data
(`.devopsy-data-images`, beside `shared/`): each run's version and, for
a pulled image, its tag (the part after the last `:`, the repository
never compared: a mirror is no change). Every place data meets an image
asks one rule against it: a release, a rollback, a sync, an import, a
`--no-code` restore. Against the data's version (the highest since its
history last started over):

| the activation's label | |
|---|---|
| higher | runs |
| the same, the same tag (a floating tag moving upstream, its patches) | runs |
| the same, another tag (a pulled image moved to another tag with the label left as it was: a pin bumped under a coarser label) | refused |
| the same, built (`build:`, in either mode: no tag is compared) | runs |
| lower | refused |
| `any` | runs, and its history starts over: a version set later is taken as it is |
| no version yet (a first release, after `any`, after an import of data of no known version) | runs, its label taken |

A refusal comes before anything is built, pulled or stopped (a
rollback's before its remote before and prepare steps; its local before
steps, which run before the server is asked, have run), and names
the versions or tags and the ways out: raise the label (not for a
rollback, whose code is the older release's), or `--accept-image
<service>` (comma separated, or repeated) on the command you ran
(`--release-sync`, `--reset`, with its `--from`), which runs it anyway, the
history of its data starting over from it. `--yes` never does: it only
skips prompts. `--accept-image` lets through only what was refused; a
service the rule allows runs as it would. `--release`, `--rollback`,
`--release-sync`, `--release-no-sync`, `--sync`, `--reset`, `--import`
and `--snapshots restore <id> --no-code` take it.

Within a version, the release's image is the project's claim: nothing
is held by digest, and a plain `up` follows the names, as for any
service. Accepted: a tag pushed again to an older image, or a stale
mirror, is the same tag, and devopsy does not see it.

## Rollback against restore

A rollback brings back code, never data:
a rollback past a data upgrade (to `mariadb:11.8` after a release of
`12.1`) is refused, naming a release that fixes forward, the release
snapshot of the older release's data ([Release snapshots](snapshots.md#release-snapshots)) and
`--accept-image`. `--snapshots restore <id>` brings data, code and
images back together, checking nothing: the snapshot's data and its
history come back as the pair they were, held as below. `--no-code`
brings back data alone, checked against the current release's labels
(or, for a held service, the image it runs: the snapshot's data is the
environment's own past), held as below against the restored history. A
built image it would need, gone (it builds nothing), refuses it before
anything stops, naming the full restore, which builds.

## Holds

A data service is held, run on another image than its
release's own, exactly when the rule would refuse its release's own
image against its data's history: on the image that history ends on,
by its digest when pulled, by its own name when built (the other
release's own name when it built what this one pulls). devopsy never
carries a hold from one activation to the next: it derives it again
from the history wherever it decides what a data service runs (every
activation, the restart after a failed run step, a full or `--no-code`
restore, a sync, an import, a reset), so a hold goes as soon as the
data allows the release's own image again, and nothing else keeps one.
A release or a rollback is refused or accepted before it runs, so it
holds nothing; the restart after a failed run step is the one place
that cannot refuse: it runs on its own, past the switch, so the
previous release runs on the image the failed one ran on the data,
said in the release's output and in `--status` (`held on`), until a
release fixes forward. The failed release's `--accept-image` never
reaches the restart: it accepted the new image, never the old one back
on the new data. A
full restore of a snapshot taken during such a hold holds the same way.
A rollback to a release that was held runs its own image, which stays on
the server through the hold, built again only when gone, and a `--reload` of one compares the
variables with its own image, not the held one. When the restart's
images cannot be restored at all (the server's images held by another
for 10 minutes, Docker failing), it is still held, a pulled one by its
digest, a built one by the bookmark of the release that built it; with no
bookmark, the release's output names it, with the release that fixes
forward. The same goes for a child after a
release that syncs whose run step failed before its database ran: its
data is then the parent's, and a database the parent builds ran on an
image only the parent's server has, so it runs an image of this server
that the data allows, the failed release's own usually; with none, the
previous release's own image runs on it, said, until a release fixes
forward.

## Syncs and imports

Syncs and imports check the receiving side's labels against the
source's data ([syncs](syncs.md), [`--export` and
`--import`](data.md#--export-and---import)): the receiving side always runs its release's own image on
the copy, a hold it had going with its old data, and its history
becomes the source's. One whose own built image is gone while held is
refused before anything stops: a copy builds nothing. A `custom` copy,
a sync's or an import's, is loaded through the image that runs, so the
data's files are that image's, not the source's version: it checks
nothing, and its history and hold stay as they were.

A copy brings data at the parent's version,
so before anything stops the child's side checks its own labels against
the parent's data, as a release checks its own (above),
for every service whose files the copy writes (`volumes`, `any`
included; one whose entries are all missing on the parent writes
nothing, and keeps the child's history). A `custom` copy checks nothing
and keeps the child's history and what it runs, as a `custom` import
does: its script loads the dump through the image that runs, so the
data's files are that image's, not the parent's version. A child below the parent's
version, or on another tag at it, is refused, every sync, unless `--sync
--accept-image <service>`: the way to try an upgrade path on the parent's
data, one command per sync, its history starting over. The
child always runs its own image on the copy; once the copy is complete it takes over
the parent's history of that data. A parent released between the sync's
start and its copy refuses the copy before stopping anything: sync
again.

An `--export` of a `volumes` service carries its data's history too (on
a target, as a sync's parent sends it; locally, its label's version and
tag alone), in the tar's first header, which `tar` itself skips. An
`--import` of it on a target checks the environment's labels against it
before anything stops, as a sync does, and takes it over once the data
is in; refused, `--import <service> --accept-image <service>` runs it
anyway, its history starting over. An import, a sync and a `--no-code`
restore all read a service's labels as they are now, and refuse one
whose image they name otherwise than its release fetched (a variable set
since) until a release fetches it. A tar without the header and a
`volumes` service's `--resolved` (unless the import took the history over
before what failed) start the service's history over, data of no known
version: what next runs on it says its version, the service's own label
if it runs once the import is in, else the next release's; locally there
is no history. A `custom` export is its script's output and carries none, and a
`custom` import, or its `--resolved`, checks nothing and leaves the
service's history and what it runs as they were: its script loads the
dump through the image that runs, so the data stays what that image
made it.
