# Data labels

`internal/cli/sync.go` (the labels, `SyncService`, the stop set, dependents), `cmd/devopsy/volumes.go` (named volumes); one set of labels for syncs (`internals/syncs.md`), `--export` and `--import` (`internals/export-import.md`) and snapshots (`internals/snapshots.md`).

## The labels

- `devopsy.data=true|false` (`cli.DataLabel`, `SyncService.HasData`) says whether a service has data; `devopsy.data.copy=volumes|custom|none` (`DataCopyLabel`, `Copy`, `Copies()`) how a copy carries it; `devopsy.data.exclude` (`DataExcludeLabel`) what of it stays out; `devopsy.data.stop=never|own|import|always` (`DataStopLabel`) when the service is stopped around a copy.
- No label is no data, as `false` is; an empty value on either label is an error: a variable left unset by accident must fail, never silently stop copying.
- The copy label is ignored on a service without data, so a variable can flip `devopsy.data` and keep it.
- `devopsy.data.version` (`images.DataVersionLabel`, `SyncService.Version`) is required with data, dotted non-negative integers or `any` (`images.AnyVersion`), ignored without data, refused per service where labels are read, its error saying why with both ways to set it.
- `copy=none` is data all the same: its images only go forward (`internals/images.md`), `--reset` empties it, a sync leaves the child's own, and `--export` and `--import` refuse it.
- `custom` makes the copy the project's job, through the data capability's `export` and `import` scripts, both required for it; `volumes` copies the service's entries of `mnt` and its named volumes as files.
- `stop` defaults to `never` for every copy, so every stop shows in compose.yaml; it applies to services without data too (for example an app needing `import`), and `never` is accepted explicitly, which silences the dependents notice.
- `own` without any `devopsy.data` label is an error (it would never stop); with `false` or `copy=none` it is ignored, since no copy moves that data.
- Label errors come only where labels are read (`parseSyncServices`, `SyncServices`), never in `Build`: a rollback to an older release, or the restart after a failed one, runs a plain `up` and must not fail on labels.
- The labels are a named feature, `data-copy` (internals/releases.md), found by `--sync-push`, `--export`, `--import` (`--resolved` through it) and `--sync-services prepare|finish|import|list`, never by `settle` (which only warns on labels it cannot read) nor by an activation (an older `--check-mounts` refuses the new values before the switch): an older devopsy reads the copy label as no label, so a parent says "nothing to sync", which a holder takes for `go`, and a child's first release goes live on empty data.

## Entries, volumes and names

- A `volumes` service's data is the top-level entries of `mnt` it bind-mounts (`SyncService.Dirs`) and its named volumes by compose key (`SyncService.Volumes`, every service's, so the child guard sees a tool on one), less what `devopsy.data.exclude` names.
- `devopsy.data.exclude` (entries of `mnt` and volume keys, comma or space separated) drops them at parse time, whatever the copy, so every consumer (a copy, the guard, the child's list, a snapshot) agrees; without data it is ignored, not validated.
- An entry of `mnt` with whitespace in its name, starting with a dash or named `!` or `(` is refused (`argName`) as an owned service's writable entry and as a data service's entry or excluded name, and so is a data service's volume key with whitespace or a leading dash: lists and rsync modules name entries split by spaces, and find, chown and tar take them as arguments.
- A data service's named volume with `external:` or a fixed `name:` is refused where labels are read (it would be another project's or every environment's data), and so is a key named like an entry a data service mounts: tars and rsync modules name both by name.
- A named volume is copied into the other side's volume of the same key (`<compose project>_<key>`) and keeps its copied owners (`devopsy.mnt.owner` owns entries only).
- A volume a copy needs is made first with compose's two labels, project and key, and the volume's driver, options and labels (`ensureVolumes`): Docker makes one a container mounts unlabeled, and compose then warns "not created by Docker Compose" on every up.

## The stop set

- `cli.DataStops` is a pure function of the operation (`ExportOne`, `ImportOne`, `SyncParent`, `SyncChild`) and the services, giving the set with each one's why; the table in the README is the spec.
- `own` and `import` overlap only on the service's own import, and the values are not a scale (`import` on a raw database exports it running, a corrupt copy).
- An operation over several services takes the union of their sets, under one stop and one start.
- Dependents follow everywhere devopsy stops something, the services whose `depends_on` reaches a stopped one, transitively, every edge (`required: false`, and Compose's implicit `network_mode: service:`, `links` and `volumes_from`, which its config lists under `depends_on`, pinned by a fixture), computed by `cli.WithDependents` and never left to Compose's `stop`: the record, the hooks' lists and the restore need the full set.
- The closure comes first, then only what runs is stopped and recorded, so a database down already still stops its running app, and a dependent down or in an inactive profile stays as it was; the stops that end a cut write (`endWriters`, `endImport`) follow the same rule.
- devopsy says what it stops and why (`cli.StoppingLine`).
- The contract of a custom script: a service in the set is down, its own and a dependent's included (`DEVOPSY_DATA_STOP` names it), and only an import brings up a custom service outside the set (`upFor`); a script that needs its service running refuses when the set has it, never starts it.
- A running dependent without a stop label is named on `--import` and in a child's prepare (`cli.UnlabeledDependents`, filtered by what runs): a worker writing to a database being loaded is what authors miss.
- Nothing uses `docker pause`: it flushes nothing, and a proxy drops a paused container's route; consistency comes from a stop or a custom export's dump.
- A `restart: always` service in a kept-down set is named (`restartAlwaysNotice`): Docker starts it with its daemon, so a reboot brings a kept-down app back on half a database.
