# Images

`internal/images/` holds the rules, behind the `Docker` interface, run on the server as the hidden `--images` (`cmd/devopsy/images.go`, `img` in the scripts); the scripts keep only what needs the shell, the locks and fetching at a terminal (`releaseImages`, `fetch_images`, `with_tty` in `internal/remote/remote.go`).

## Fetching

- Every image a release runs stays until its last release goes.
- A release fetches its images before the switch (`fetchImages`, through `devopsy --`, past project commands), in build mode `ProfilePull` (`--profile '*' pull --ignore-buildable`) then `build --pull`, in image mode `pull` then `ProfilePull`: a build or pull in the run step would run after the switch, and in a sync after the copy.
- Image mode's `pull` pulls buildable services with an `image:` too and fails on a missing tag.
- A rollback or a restart restores its images (`img restore`) and fetches only when an image it builds is gone.
- Every release pulls every profile's non-buildable services, pull only, while the listing and the bookmarks stay the default services': a profile-only tool pulled once on first use never gets a patch.
- A profile's buildable services are never built or pulled by a release, so a tool nobody pushed never fails one; a profile's non-buildable one that fails to pull fails it before the switch.
- Image mode asks the registry twice for its default third-party tags; accepted.
- A signal during `img names` stops the fetch before it pulls.

## The listing

- Each release lists its images per service in `devopsy-images` (`ListFile`: name, image id, service, kind, registry digest, version label, build key), from `compose config --format json` without profiles and without devopsy's own compose file (`NoReleaseComposeEnv`).
- Own or pulled comes from compose and the mode (`Service.own`, a build in build mode or `pull_policy: never`), never from `RepoDigests` alone: on Docker's containerd store built and loaded images carry `RepoDigests` too, and a pulled image's id is its index digest.
- A pulled image without a registry digest is restored as own: nothing could hold it.
- The version label is display only: no field is common to images.
- A built service's build key (`images.BuildKey`) hashes its interpolated `build:` section with paths relative to the release's code and links resolved, then a short hash per argument and never a value: compose prints paths absolute, the same code is reached by other paths (through `current`, a release root behind a link), and an argument can be a secret.
- A bookmark put back keeps the key its listing had (`Restore`'s retag, built under the old values); a listing after a fetch records today's key, whatever image id the build gave.
- In image mode a service with a `build:` section whose image id is the release before's gets a warning at the record (`stale`) once the image's `Created` is more than `StaleAfter` (30 days) ago, with the fix a rebuild with `build --pull`, then a release: its base may have patches.
- Compare names as Docker reports them, through `familiar` (`docker.io/library/nginx` is `nginx:latest`).

## Bookmarks

- Bookmarks are the reference count, each release tagging every image it runs as `<repo>:devopsy-<compose name>-<id>` (`bookmarkPrefix`, a long compose name cut and hashed to fit Docker's 128, a repository named twice adding a checksum of the ref) by image id: another fetch may move the name meanwhile.
- Digests are bookmarked too and never restored: `prune -f` would delete an unused image pulled by digest only.
- A release fetched again never removes names: it untags the bookmarks its new listing no longer makes and records the names it dropped for the next pruning, since it runs under the shared lock beside other fetches.
- A re-listing that drops an image records its names as left (`keep`): otherwise the names other releases counted on it to take leak.
- Snapshots' bookmarks count as the environment's own (`images.Set.own`, internals/snapshots.md).

## Data only goes forward

- A pulled data service (`KindData`: `devopsy.data=true` whatever its copy, no `build:`, pulled) never runs an image older than the newest its data ran (`history.go`, `record.go`, `restore.go`).
- There is no version ordering: "newer" means "ran later on this data", kept in the environment's `HistoryFile` (`$base/.devopsy-data-images`), one line per image that ran on each such service's data.
- The history is appended only after something ran (`img ran` after the run step, failed or not, before a restart), never at `img record`: a release cancelled after its record ran nothing.
- Every activation runs `img ran` on current before anything reads the history, and so does a settle once it started anything (`ranOnData`): a release killed past its switch never got to its own.
- A restore runs `img ran` only once `restored` started its set on the images it holds, and not when its run step failed: a database the step left as it was still ran the replaced data.
- `img record` refuses an image the history moved past, or one it never ran under a changed name (`check`), unless the operator names the service with `--accept-image` (`ImagesAcceptEnv`), never with `--yes`: CI passes `--yes` everywhere, which would skip every check.
- An accepted service's `ForcedFile` starts its history over once that image runs: the activation's `img ran` removes it, and a cut activation's `img ran ... cut` (`RanCut`) keeps each line until its service runs that image, so a database found still on the image before, stopped for a release snapshot, is left to the settle that makes it again.
- A built service whose data has a history is checked by name too: a switch between pulled and built needs acceptance, its own builds go through.
- A refused record puts back what `img names` saved before the fetch (`putNamesBack`).
- A history is seeded from the running container when it has none (`seed`): an older devopsy's data, a label just added.
- Only the environment's own side writes its history, under its release lock; `--destroy-data` removes it (`RemoveHistory`).

## Holds and restores

- Every activation writes `ComposeFile` with a `devopsy.digest` label per fetched service (`DigestLabel`, the image it was given, never a release id, since compose recreates on a label change) and `image: <repo>@<digest>` for every pulled data service, never another.
- `Restore(target, against, fetched)` decides per service from target's compose now (`List`, never reading devopsy's own file): a data service gets the history's last, else its container's, else against's; any other pulled image what target's name holds now, pulled when gone; an own image its bookmark, `ErrGone` (exit 1) to build again, also when the listing's build key differs from what `build:` gives now (`changed`, `rebuilt`).
- A rollback brings back code, never a changed name's exact image: a floating tag goes forward to the patched image.
- A rollback keeps a pulled data service of the release it leaves on its data's newest image, even where the target builds that service.
- Never retag a pulled name: a backward retag takes a database back on its upgraded data, and a rollback moving a shared pin back changes another environment.
- `Undo` and `Recover` put back own names only (`RestoreOwn`).
- A restore never changes another environment: pulled names never move back, and an own name whose current image another compose project's container uses (`docker ps --filter ancestor=`), or that a bookmark none of this environment's releases made holds (exact names, `app-prod` never matching `app-prod-eu`), stays on the newer image, said.
- A held image gone is pulled by its digest before the switch: the run step's `up` would pull it after.
- `img hold` (`HoldData`) holds an environment's data services on their data's newest image, for a restart whose `get_images` failed (internals/releases.md).
- `img undo` bookmarks a new release's fetch so it goes with the release, leaves a rollback's target alone (its names may hold the live release's images), then brings current's own names back, never a pulled one.

## Removals

- A removal (`Remove`) untags the going release's bookmarks, then decides per image from Docker's state:
  - held by another bookmark (never one the record holds), it stays, its names in repositories no holder is in recorded;
  - used by a container, its names are recorded, never stripped;
  - else its listed names and its other names in the same repositories go (`mariadb:11` with `mariadb:11.4`);
  - a name that moved on stays.
- Never `docker rmi -f`; a refusal is recorded: the classic store may delete an image a stopped container uses.
- Removals are idempotent, and one cut off anywhere is finished by the next; Docker failing (anything but a missing image) stops one with exit 1 and the release stays, `RemoveReleases` removing a directory only after its images, and `--destroy` then keeps the directory and says to run it again.
- No directory scanning, server-wide sweep or trash directory: a removal decides from its release's listing and Docker's state, and reads nothing above its project.
- What could not go is recorded in `images.LeftFile` (`<release root>/<project>/.devopsy-images-left`), listed by `--releases` and retried by `img prune` for this project's lines only, a line dropped once its name is gone, moved on or removed.
- The record is appended in one `O_APPEND` write (`leftAdd`, before it says), since two environments of a project record under the shared lock; only the retry, alone under the lock, rewrites it.
- `LeftoverScript` treats the record like the project's `.env`, and `--purge` says what it forgets (internals/data.md).
- `images/releases.go` decides which releases go, unit-tested on directories: `ToPrune` (older than the `keep`-th live one; current and fetching ones stay), `RollbackTarget`, `RemoveReleases` by id.

## On the server

- `runImages` ignores SIGPIPE, SIGTERM, SIGINT and SIGHUP: Go keeps no inherited ignored SIGPIPE or SIGTERM, so the script's `trap ''` alone would let a gone session or a second TERM kill an undo halfway.
- The history and the holds are a named feature, `data-images` (`DataImagesFeature`, internals/releases.md for what `childNeeds` carries), found by the `img` words they added (`names`, `record`, `restore`, `ran`, `hold`, `data`, `sync-check`, `sync-done`) and by `img`'s `--mode`, which an older `--images` refuses as usage after the locks: every activation, holder, parent check and removal refuses on an older server before anything changes.

## Tests

- An in-memory Docker (`fakeDocker` in `images_test.go`, with a registry and containerd's digest names); `TestRandomHistory` runs 200 random histories of two environments sharing a tag and an alias; `TestDataRandom` 300 (`DEVOPSY_TEST_SEEDS` for more) of releases, refusals, `--accept-image`, failed runs, rollbacks, plain ups and pruning while the registry moves tags forward and back, checking that each database runs its data's newest image, that no rollback moves a pulled name and that nothing is left once all goes.
- Script tests call the real `--images` (`realDevopsy`, built by `TestMain` in `internal/remote`) behind a fake devopsy and a file-per-tag docker.
