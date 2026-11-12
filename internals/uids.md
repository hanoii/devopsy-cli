# Container uids

Code: `internal/uids/` (owners, ranges, the hash, claims, start locks), `internal/cli/uids.go` (the variables as a project loads), `cmd/devopsy/uids.go` (the claims as the deploy user, the door's `uids` request).

- Containers of different owners never share a uid: a process escaped from one reaches another's environment, root and signals through `/proc` with the same uid and gid alone.
- An owner is named by its parts joined with `:` (`env:<project>[:<instance>]:<environment>`, `project:<project>[:<instance>]`, `Owner.String`), never by its compose name: compose names are flat, so two owners would share one name.
- Every part is checked as the config takes it, the environment lowercase (`Owner.check`), before it names a file.
- Each domain has a range of its own (`ranges`), inside the block systemd's table of uids leaves unused, so a tenant's own names never hash into a bare project's range.
- An owner's hashed slot is FNV-1a-64 of its name into its range (`Owner.Plain`), pinned by `TestPlainIsPinned`: a change moves every uid an image baked or a server claimed.
- A claim is a symlink named for its owner whose target is the uid (`Store.Lookup`), keyed by owner so a probed uid never moves when the slot it was pushed from is freed.
- A claim is made only under the claims directory's lock, after reading again, taking the hashed slot or the next free one going round, never a slot `Taken` names or any entry holds.
- A project holds at most `MaxClaims` claims in a domain: a tenant makes environments freely, and a first claim reads its domain's.
- Start locks live in `locks/` under the claims directory, never beside a claim: an environment may be named like a lock file.
- A start lock is opened close-on-exec, read-write, and cleared only right before the exec (`Lock.KeepAcrossExec`), so nothing started before it holds the lock.
- A start lock is checked against its path after it is taken and taken again on the file there now: a free removes the file while it holds it.
- A claim is freed only with its owner's start lock held exclusive (`Store.Free`), which removes the lock's file too.
- Only the server's devopsy computes a uid, at every load of a release (`setUIDs` in `internal/cli/uids.go`), setting `DEVOPSY_UID` and `DEVOPSY_PROJECT_UID` over every layer and the caller's value, and stores them nowhere: a restore would bring another environment's back.
- A release's owner is read from where it is (`releaseOwners`), its target lines checked against its place under the release root, never from a variable a `.env` could set.
- A project user's devopsy asks the door's `uids` request, whose answer is the two numbers alone (`askDoorUIDs`); the door reads the owner from the path in the caller's tree (`ownerAt`).
- Locally the uids are the plain hashes, under the caller's and the `.env`'s, the project's from `config.yaml`'s `project:` alone (`CommittedName`) so it matches every server's.
- `--vars set` and a config's `env:` refuse the two names (`uids.Reserved`).
- A release whose compose files read either variable needs the server's `uids` feature (`cli.ReadsUIDs`, `remote.WithAlsoNeeded`), checked in its first session and its upload's: an older server leaves them unset, and compose would refuse after the upload.
- Every start of containers in a release holds both start locks shared, the environment's and its project's, from before it reads the claims again until compose ends (`holdStartLocks` for the deploy user, `doorSide.startLocks` for the door's `up`, `create` and `run`), and fails when a claim changed since the load: a free between the load and the start would leave a container on a freed uid.
- Starts that run compose as a child (`runCompose`, `ensureVolumes`) need no start lock: they run under the release or sync lock, which `--destroy` takes.
- A claim is freed only by a `--destroy`'s last step (`--free-uids`, `freeUIDs`, the door's `free`), after the removal and in its "nothing at" branch too, so a rerun finishes a cut one: under the start lock exclusive it takes the compose name down once more, then frees the claim, then the project's when no environment claim of it is left (`FreeIfUnused`).
- The door frees only its own project's environment, and none whose data (`<space>/mnt/<compose name>`) is left.
