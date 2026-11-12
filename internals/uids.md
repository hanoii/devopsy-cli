# Container uids

Code: `internal/uids/` (owners, ranges, the hash, claims, start locks).

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
