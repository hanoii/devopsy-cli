# AGENTS.md

devopsy-cli is a Go command-line tool, `devopsy`, that wraps `docker compose`
for projects with a `.devopsy/` directory. Read `README.md` for the
user-facing behavior and keep it in sync with any change to it.

## Layout

- `cmd/devopsy/`: `main`, which executes what `internal/cli` plans, plus
  end-to-end tests that run the built binary against a fake `docker`.
- `internal/cli/`: finds the project, loads `.env` and decides what to run
  (`Build` returns a `Plan`; it never executes). Unit-tested.
- `internal/remote/`: `@target` support: targets.yaml, packing a release, and
  the POSIX `sh` scripts run on the server over SSH. Scripts are checked with
  `sh -n` in tests; keep them POSIX and GNU coreutils based (`mv -T`).
- `install.sh`: POSIX `sh` installer that downloads a release.
- `scripts/devopsy`: runs devopsy built from the checkout; developers
  symlink it into their PATH.
- `.goreleaser.yaml` and `.github/workflows/`: CI and release builds.

## Rules

- Keep behavior compatible: project discovery, `.env` loading (compose's own
  parser, caller's environment wins), custom commands in `commands/` with
  `DEVOPSY_PROJECT_DIR` and `DEVOPSY_CLI_COMMAND`, the recursion guard, and
  passing everything else to `docker compose` untouched.
- devopsy replaces itself with the command it runs (`syscall.Exec`), so
  signals, the terminal and exit codes belong to that command.
- Messages go to stderr. Compose output stays on stdout.
- Few dependencies; prefer the standard library. Linux and macOS only.
- Variable precedence: caller's environment, `.devopsy/.env` (on servers
  `shared/.env`), `.devopsy/target.env` (from targets.yaml), then
  `/etc/devopsy/devopsy.env`.
- Remote: only `ssh` locally, and `devopsy`, `tar` and `flock` on the server.
  No rsync: macOS ships openrsync. Releases are complete copies.
- Project-specific behavior belongs in a project's `.devopsy/commands/`, not
  here.
- `install.sh` stays POSIX `sh` (dash, busybox ash).

## Design decisions

The workspace README (`../devopsy/README.md` locally) describes how the repos
fit together, and `../devopsy/ROADMAP.md` the open ideas.

- devopsy is compose plus environment variables. It never generates or
  rewrites compose files: if a feature needs that, it is the wrong feature.
  Everything it computes is visible with `devopsy print-env`.
- An environment is a target: a server path, so its own compose project,
  data and public URL (`<project>.<DEVOPSY_PUBLIC_DOMAIN>`). Branches are
  only what gets released into one; no branch concept in the core.
- Remote commands run through the `current` symlink, never a release path:
  compose stores bind-mount paths in containers, and pruned releases would
  break them (for example after a reboot). `release` switches `current`
  first, runs the command, and switches back when it fails.
- Releases are full tar streams over SSH (catalyze, the largest project,
  compresses to about 3 MB). No rsync: macOS ships openrsync without the
  needed features.
- `.devopsy/target.env` is written into each release from targets.yaml, so
  per-target values apply however devopsy runs on the server, and rollbacks
  restore them. `shared/.env` is always linked so server edits apply without
  a release.

- `devopsy @target domains`: the server only reports facts (print-env,
  Traefik's routers from its local API, acme-dns registrations, its IP); DNS
  (through 1.1.1.1), certificates (a real TLS connection to the server per
  name, verified against system roots) and Cloudflare's proxy (its ranges,
  then a request through it) are checked locally, so servers need nothing
  extra. `--retry` writes a uniquely named router into Traefik's dynamic
  directory (Traefik only retries on a configuration change) and the file is
  removed once every routed host has a valid certificate.

## Gotchas

- Compose reads `--env-file` more than once: `<(devopsy print-env)` does not
  work, a file does (observed, not confirmed in compose's source).
- `.env` values in double quotes are interpolated when loaded (compose's
  parser): `"$HOME"` becomes the value of HOME.
- GitHub has delivered the same tag push several times (v0.5.0: 4 release
  runs, v0.6.0: 3), and the runs raced on uploads ("already_exists"). The
  release workflow now queues runs per tag and GoReleaser replaces existing
  artifacts. If a release run still fails, check the release's assets and
  install it before assuming a broken release.
- Traefik picks up a new container a couple of seconds after `up` returns:
  wait before querying its API in tests.

## Checks

```sh
go vet ./... && go test -count=1 ./...
docker run --rm -v "$PWD:/mnt" -w /mnt koalaman/shellcheck:stable -s sh install.sh
```

End to end, against a real server: an OrbStack Debian 13 machine set up with
devopsy-server (see its AGENTS.md), a linux/arm64 build installed in it, and
a test project whose `.devopsy/targets.local.yaml` points at
`devopsy@devopsy-test@orb`. OrbStack's SSH needs no keys.

## Releases

Tag `vX.Y.Z` on `main` and push the tag: the release workflow runs the tests
and GoReleaser publishes the binaries and `checksums.txt`. Archive names have
no version (`devopsy_linux_amd64.tar.gz`), which `install.sh` relies on.

## Commits

Conventional commits (`feat:`, `fix:`, `docs:`, `build:`...).
