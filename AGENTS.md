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
- Remote: only `ssh` locally, and `devopsy`, `tar` and `flock` on the server.
  No rsync: macOS ships openrsync. Releases are complete copies.
- Project-specific behavior belongs in a project's `.devopsy/commands/`, not
  here.
- `install.sh` stays POSIX `sh` (dash, busybox ash).

## Checks

```sh
go vet ./... && go test -count=1 ./...
docker run --rm -v "$PWD:/mnt" -w /mnt koalaman/shellcheck:stable -s sh install.sh
```

## Releases

Tag `vX.Y.Z` on `main` and push the tag: the release workflow runs the tests
and GoReleaser publishes the binaries and `checksums.txt`. Archive names have
no version (`devopsy_linux_amd64.tar.gz`), which `install.sh` relies on.

## Commits

Conventional commits (`feat:`, `fix:`, `docs:`, `build:`...).
