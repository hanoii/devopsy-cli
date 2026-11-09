# Images

`internal/images/` holds the rules, behind the `Docker` interface, run on the server as the hidden `--images` (`cmd/devopsy/images.go`, `img` in the scripts); the scripts keep only what needs the shell, the locks and fetching at a terminal (`releaseImages`, `fetch_images`, `with_tty` in `internal/remote/remote.go`).

## Fetching

- Every image a release runs stays until its last release goes.
- A release fetches its images before the switch (`fetchImages`, through `devopsy --`, past project commands), by its own mode (`img mode`, its record's), in build mode `ProfilePull` (`--profile '*' pull --ignore-buildable`) then `build --pull`, in image mode `pull` then `ProfilePull`: a build or pull in the run step would run after the switch, and in a sync after the copy.
- Image mode's `pull` pulls buildable services with an `image:` too and fails on a missing tag.
- A rollback or a restart restores its images (`img restore`) and fetches, by the restored release's mode, only when an image it builds is gone.
- Every release pulls every profile's non-buildable services, pull only, while the listing and the bookmarks stay the default services': a profile-only tool pulled once on first use never gets a patch.
- A profile's buildable services are never built or pulled by a release, so a tool nobody pushed never fails one; a profile's non-buildable one that fails to pull fails it before the switch.
- Image mode asks the registry twice for its default third-party tags; accepted.

## The listing

- Each release lists its images per service in `devopsy-images` (`ListFile`: name, image id, service, kind, registry digest, version label, build key), from `compose config --format json` without profiles and without devopsy's own compose file (`NoReleaseComposeEnv`).
- Own or pulled comes from compose and the mode of the release listed (`Service.own`, a build in build mode or `pull_policy: never`), never from `RepoDigests` alone: on Docker's containerd store built and loaded images carry `RepoDigests` too, and a pulled image's id is its index digest.
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

## Data follows its version

- Every service with `devopsy.data=true` says its data's version (`devopsy.data.version`, `DataVersionLabel`, internals/data-labels.md): devopsy cannot tell what an image does to its data, so it compares what the project says.
- One rule (`rule`, `history.go`) judges a version and a tag against the data's window at every place data meets an image: a higher version runs, a lower one is refused, the same one runs unless pulled under another tag than the window's newest line at that version, `any` and a window with no version run unchecked.
- The window is the history since its last start (`window`): every line but `Ran` starts it over, and its version is the highest of its lines.
- A service with `build:` has no tag check (`Service.tag`): image mode's tag is usually the commit's, new every release.
- The history is the environment's `HistoryFile` (`$base/.devopsy-data-images`), appended only after something ran (`img ran` after the run step, failed or not, before a restart), never at `img record`: a release cancelled after its record ran nothing.
- Every activation runs `img ran` on current before anything reads the history, and so does a settle once it started anything (`ranOnData`): a release killed past its switch never got to its own.
- `Ran` records a container only once it ran the image its activation gave it (its `devopsy.digest` label against `ComposeFile`), and never one down that last started before the data landed (`ranBefore`).
- A restore runs `img ran` only once `restored` started its set, and not when its run step failed: a database the step left as it was still ran the replaced data.
- A release is checked before anything is built or pulled (`img check`, `Check`), so a refused one fetches nothing and moves no names.
- A rollback is checked twice, leniently at `img rollback-target` before its before and prepare steps (`CheckRollback`, a config that cannot be read refusing nothing) and for real at `img restore`.
- `--accept-image <service>` (`ImagesAcceptEnv`) forces only what the rule refused (`judge`), never `--yes`, which CI passes everywhere.
- An accepted service's `ForcedFile` starts its history over once that image runs: the activation's `img ran` removes it, and a cut activation's `img ran ... cut` (`RanCut`) keeps each line until its service runs that image.
- A refused rollback names the newest release snapshot whose data allows what it refused (`snapshotFix`).
- Only the environment's own side writes its history, under its release lock; `--destroy-data` removes it (`RemoveHistory`).
- The rule and its lines are a named feature, `data-version` (`DataVersionFeature`), found by the `img` words and flags that carry them and by `--export`, `--import` and `--check-images --restore`.

## Holds and restores

- Every activation writes `ComposeFile` with a `devopsy.digest` label per fetched service (`DigestLabel`, the image it was given, never a release id, since compose recreates on a label change).
- `ComposeFile` pins every pulled service not held to its listing's digest, `image:` as `<ref>@<digest>` (`Pin`, the tag kept for `docker ps`), and a hold stays `repo@digest`: a name resolved again after the fetch is anyone's on the server to move (a project user's build of a registry's name), and the pin's string never changes for the same image, which would recreate.
- A pulled service's `image:` in `ComposeFile` is its pin, never an own image by its bookmark (`derive` reads that for `KindOwn` only).
- A data service is held, on another image than its release's own, exactly when the rule refuses its own label and tag against the window its data has then, on the image that window ends on (`holdsFor`, `holdLine`, `hold.go`).
- A hold is never carried, every writer of `ComposeFile` deriving it again from the history (`Record`, `Restore`, `LastHold`, `derive`): carried holds go stale on the path that forgets them.
- A held entry records its release's own image (`entry.OwnRef`, `ownOf`), which every derivation judges, `ReloadCheck` compares the variables with and a hold that ends gives back.
- A pulled image is held by its registry digest (`image:` in `ComposeFile`, `HeldLabel`), in the service's own repository when the service pulls, and pulled when gone before the switch.
- Every own image runs by its release's bookmark, held or not (`resolveBookmarks`, `image:` in `ComposeFile`), a held one by the holding release's own bookmark of it in the repository it was built under, never another release's, which goes with that release: the project's name is every environment's to move (an `image:` beside `build:`, a branch's or a pull request's environment), and an id is no name a build can tag.
- devopsy never moves a project's own name: what an environment runs never depends on where another left it.
- A build-mode fetch runs with its own file, `FetchFile` (`img fetch-file`, `WriteFetch`, selected by `FetchComposeEnv`, never `ComposeFile` then), giving each service whose image is one the release builds, every profile's, a fetch tag (`fetchTag`, `<repo>:devopsy-<compose name>-<id>-f<cksum>`): the build lands under a name only that fetch writes, and `ComposeFile`, the live release's at a restart, is never written before or during a fetch.
- What a fetch built is read by its fetch tag alone (`entries`, `Restore --fetched`), never the project's name, which another environment's fetch may move meanwhile: a record whose tag is missing fails (`errFetchedNone`). Adopting it bookmarks the image by id, then removes the fetch tags and the file (`dropFetch`), as a failed or cut fetch's `img undo`, `img recover` and every restore do.
- A service of no entry whose `image:` is exactly an own image's (one under a profile sharing a build) runs that image's bookmark too (`resolveBookmarks` through `ListAll`), since devopsy's builds write no project name it could run by.
- `img fetch-file` warns of a built service whose Dockerfile reads exactly another service's image by its name (a `FROM`, a `COPY --from`, a `RUN --mount` `from=`), not a build context (`fromOtherBuilds`), skipping what it cannot read for certain (an `${ARG}`, a stage, an inline Dockerfile): that name holds another environment's build or none. A warning, never a refusal: it reads Dockerfiles by a heuristic.
- An own image's bookmark that Docker fails to put fails the record (`keep`), before the switch, its fetch left to the undo, which bookmarks by its tags; an image the listing names that is gone is said and skipped, never a failure (a restart's last try, a snapshot of it, a restore that builds it again). An undo after a record keeps the record's listing. `Remove` takes what a fetch of the release left.
- A sharer of an own image (`sharers`) comes from the fetch file, else every profile's listing now (`ListAll`), never what an earlier file said: a fact carried goes stale, and a full restore's release has no file yet.
- A project behind the door (`Set.Door`) writes no fetch file (the door names its builds and refuses a tag of devopsy's), and runs its own builds by their bookmarks as any project.
- A held service's `build:` is reset in `ComposeFile` (`build: !reset null`): an `up --build`, a build or `pull_policy: build` would land the project's code on the image it is held on.
- A held service's own build keeps a bookmark (`withOwnBuilds`, an entry of no service): nothing else names it, and the next pruning on the server would take it, leaving nothing to give back.
- `Restore(target, against, fetched)` decides per service from target's compose now (`List`, never reading devopsy's own file): any pulled image what target's name holds now, pulled when gone; an own image its bookmark, a held one's from its kept bookmark, `ErrGone` (exit 1) to build again when there is none or its build key changed (`changed`, `rebuilt`).
- A rollback brings back code, never a changed name's exact image: a floating tag goes forward to the patched image.
- Never retag a pulled name: a backward retag takes a database back on its upgraded data, and a rollback moving a shared pin back changes another environment.
- A restart cannot refuse: it holds what the rule refuses and ignores `--accept-image` (`runImages`), which accepted the new image, never the old one back on the new data.
- Its last try (`img restore --restart --as-built`) runs the listing's images by their bookmarks whatever their build keys, and for a hold on an image this server lacks runs one the data allows (`allowedHere`, never from a release that failed its fetch, `FetchFailedFile`), else its release's own, said (`ownInstead`).
- `img hold` (`LastHold`) writes what a file can without the image lock or Docker when `get_images` failed: a pulled hold by digest, for the run step's `up` to pull, a built one by a bookmark that already names it, and one with no bookmark is said, with the release that fixes forward.
- A restore never changes another environment: pulled names never move back, and own names are never moved at all.
- `img undo` bookmarks a new release's fetch so it goes with the release and marks it (`FetchFailedFile`), and leaves a rollback's target alone (its names may hold the live release's images); the live release runs its own by its bookmarks, which no fetch moves.

## A copy that lands

- Data arriving from elsewhere goes through one entry point (`landing.go`), its path only saying what lands (`SyncLanding`, `ResetLanding`, `ImportLanding`, `ResolvedLanding`, `RestoreLanding`): `CheckLanding` before anything stops, `Land` once the data is in and before anything starts on it.
- `Land` reads the labels, has the listing take the claims of what it writes (`Landing.claimed`), takes the history over and derives the holds, in that order, and its error says which failed in words that follow "completed, but".
- A failed landing keeps the mark and what the copy stopped down, until a copy lands or `--resolved` accepts it: data that landed halfway must not start.
- What a service is and claims is its labels' now, in a sync's holder with the pending inherited variables and in a restore `--with-env` with the snapshot's, its files moved into place before it lands.
- Only what a copy writes takes its labels' claims, so a variable changed for another service since its release never reaches that service's history.
- A landed service whose labels want an image or a build its release did not fetch is refused (`notFetched`, `fetchedOther`, shared with `ReloadCheck`), and `--resolved`, which checks nothing, keeps the listing's claim.
- A service its labels make data since its release is judged by them like any other.
- `CheckLanding` also refuses what the landing would need and cannot have (`copyNeeds`): a built image gone, a pulled hold neither here nor pullable.
- What a copy writes has one answer (`Set.Copied`, `copiedServices`, from `cli.SyncServices` and what is there, as `syncData` and a take go), so a window always goes with its files.
- A window lands for what the listing leaves out too (a profile not up, `Landing.unlisted`), for the release that turns it on to be judged against.
- A service with none of its entries or volumes there sends nothing, and the receiving side keeps its data and history.
- A parent with no history of its data yet sends its label's line (time 0, so the push's checksum holds).
- A window's lines from an export's header give a service the project builds only their version and tag when they name another repository (`Landing.header`): a header is whoever made the tar's.
- `--resolved` keeps a hold whose release's own build is gone, said (`asIs`): a release refuses on the mark and an import on what it cannot derive, so nothing else would have a way out.
- What ran through a copy (`devopsy.data.stop=never`) is made again as what it stopped is, only what that copy wrote (`restoreServices`, `written`), and a landed data service whose shared name another environment runs goes by its bookmark.
- An import and `--resolved` record what then runs (`RanCut`), for whatever reads the history next.

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

- Each release's mode comes from its own record (`images.ModeOf`, `RecordFile`, written at the upload), the new release's at a record and a fetch, the restored one's at a rollback, a restart and a full restore, never the operator's checkout, and a `Set` has no mode: a checkout may be in the other mode by now, which would restore a release made in build mode as pulled, or put a bookmark on a pushed name.
- Every `Set` on a server comes from one constructor, `serverImages` (`cmd/devopsy/images.go`), which finds the image lock and the project's record of names left (`images.LeftFile`) from the release root (`ReleaseSettings`) and the environment's directory (`images.BaseEnv`), and the scripts pass `img` only `--name` and `--address`: a setting a script passes is one a script can forget.
- The server finding these itself is a named feature, `record-mode` (`RecordModeFeature`), found by every script that runs the server's images (`--images`, `--export`, `--import`, `--snapshot-step`): an older server, told nothing, would take build mode for every release and hold no lock.
- `runImages` ignores SIGPIPE, SIGTERM, SIGINT and SIGHUP: Go keeps no inherited ignored SIGPIPE or SIGTERM, so the script's `trap ''` alone would let a gone session or a second TERM kill an undo halfway.
- The history and the holds are a named feature, `data-images` (`DataImagesFeature`), found by the `img` words they added (`names`, `record`, `restore`, `ran`, `hold`, `data`, `sync-check`, `sync-done`) and by `img`'s `--name`, with its `--address`, which an older `--images` refuses as usage after the locks: every activation, holder, parent check and removal refuses on an older server before anything changes.

## Tests

- An in-memory Docker (`fakeDocker` in `images_test.go`, with a registry and containerd's digest names); `TestRandomHistory` runs 200 random histories of two environments sharing a tag and an alias; `TestDataRandom` 300 (`DEVOPSY_TEST_SEEDS` for more) of releases, refusals, `--accept-image`, failed runs, rollbacks, plain ups and pruning while the registry moves tags forward and back, checking that each database runs its data's newest image, that no rollback moves a pulled name and that nothing is left once all goes.
- Script tests call the real `--images` (`realDevopsy`, built by `TestMain` in `internal/remote`) behind a fake devopsy and a file-per-tag docker.
