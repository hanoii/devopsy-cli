# AGENTS.md

devopsy-cli is a single POSIX `sh` script, `devopsy`, that wraps
`docker compose` for projects with a `.devopsy/` directory. `install.sh`
installs it. Read `README.md` for the user-facing behavior; keep it in sync
with any change to that behavior.

## Rules

- POSIX `sh` only. No bashisms (`[[ ]]`, arrays, `local`, `echo -e`,
  `source`). It must run under dash (Debian's `/bin/sh`) and busybox ash.
- No dependencies beyond Docker with the Compose plugin and POSIX utilities.
- Keep it small. Project-specific behavior belongs in a project's
  `.devopsy/commands/`, not here.
- Never break argument passing: user arguments go to compose or the custom
  command untouched, including ones with spaces.
- Messages go to stderr. Compose output stays on stdout.

## Checks

```sh
docker run --rm -v "$PWD:/mnt" -w /mnt koalaman/shellcheck:stable -s sh devopsy install.sh
```

Test under dash too, for example in `debian:stable-slim` with a fake `docker`
script on the `PATH` that prints its arguments.

## Commits

Conventional commits (`feat:`, `fix:`, `docs:`, `build:`...).
