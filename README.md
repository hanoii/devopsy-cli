# devopsy-cli

A small wrapper around `docker compose` for projects that keep their
deployment in a `.devopsy/` directory. Run `devopsy` from anywhere inside the
project and it finds `.devopsy/`, loads its `.env` and runs compose with the
right files. Anything it doesn't know becomes a `docker compose` command.

It is plain POSIX `sh` with no dependencies besides Docker and the Compose
plugin.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/hanoii/devopsy-cli/main/install.sh | sh
```

It installs to `/usr/local/bin` when it can, otherwise `~/.local/bin`. Set
`DEVOPSY_VERSION` to install a tag or branch, and `DEVOPSY_INSTALL_DIR` to
choose the directory. Or copy the `devopsy` script anywhere in your `PATH`.

## Project layout

```
my-project/
└── .devopsy/
    ├── compose.yaml            # required
    ├── compose.override.yaml   # optional, usually not committed
    ├── .env                    # optional, usually not committed
    └── commands/               # optional, executable scripts
        └── deploy
```

## Usage

```sh
devopsy up -d          # docker compose -f .devopsy/compose.yaml [-f .devopsy/compose.override.yaml] up -d
devopsy logs -f web
devopsy deploy         # runs .devopsy/commands/deploy if it exists
devopsy help           # lists the custom commands
```

### Custom commands

Any executable file in `.devopsy/commands/` becomes a command. It runs with
the project's `.env` loaded and these variables set:

- `DEVOPSY_PROJECT_DIR`: absolute path to `.devopsy/`.
- `DEVOPSY_CLI_COMMAND`: the command's name.

A command can override a compose command and still call it. A
`commands/up` that runs `devopsy up "$@"` reaches `docker compose up`, not
itself.

### .env

`.devopsy/.env` is the file compose reads for variable substitution, and
devopsy also sources it for custom commands. Keep it shell-compatible:
`KEY=value`, quoting values with spaces. Variables already set in your
environment win over `.env`, as they do in compose.

### Project name

Set a top-level `name:` in `compose.yaml`. Without one, devopsy uses the name
of the directory containing `.devopsy/`. Otherwise compose would call every
project `devopsy`. `COMPOSE_PROJECT_NAME` still overrides both.

## License

GPL-3.0. See [LICENSE](LICENSE).
