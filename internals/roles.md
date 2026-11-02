# Roles, exports and imports

Code: `internal/cli/roles.go`; `--prepare-release` (`runPrepareRelease`) in `cmd/devopsy/probe.go`, `--debug imports` in `cmd/devopsy/debug.go`.

- The labels are `devopsy.role` (one compose project per host), `devopsy.export.<KEY>` and `devopsy.import.<VAR>=<role or project>/<KEY>[?]`; devopsy knows no role or key names (`proxy` and `BASE_DOMAIN` are the Traefik template's convention), and its core defines no proxy contract.
- Imports resolve on the host at each release, never at runtime in a container: compose interpolates labels (a Traefik rule) before any container exists.
- `--prepare-release` (`cli.PrepareRelease`) checks the role and writes the imports into the release's `devopsy-target.env` after `cli.ImportedMarker` before the release goes live; exports are read from running containers (`Running`: one snapshot per lookup), waiting up to `WaitForExporters` (30 seconds) for a required source.
- An import never overrides what the environment sets, even empty.
- An empty export is a value: otherwise every project importing without `?` fails under a proxy without a wildcard.
- Rollbacks never check roles or imports: the check would fail a rollback exactly when one is needed, the proxy down.
- Roles are advisory, checked only at releases, never on a local `up` (a test proxy beside a main one is normal locally), and two releases claiming one role on different targets at once are not locked against each other; anyone in the docker group is root-equivalent anyway.
- `--debug imports` compares every running project's imports with its release's `devopsy-target.env`, found through compose's `working_dir` label.
