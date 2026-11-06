# Config and targets

Code: `internal/remote/config.go` (the files, targets, aliases, patterns), `internal/remote/schema.go`, `internal/cli/cli.go` (project discovery, `.env`, commands), `internal/cli/envtrace.go`, `cmd/devopsy/init.go`, `cmd/devopsy/environments.go`.

## Files

- A project's config is `.devopsy/config.yaml` (`project:`, `instances:`, `secrets:`, `public:`, `releases: {keep}`, `defaults:`, `environments:`), with `config.local.yaml` over it, and the user's is `~/.config/devopsy/config.yaml` (`aliases:`; on a server `releases:` and `snapshots: {root, max_keep, mirror}`); an unknown key is an error.
- Never `~/.devopsy`: project discovery would take the home directory for a project.
- `defaults:` gives environments `mode`, steps, `releases`, `snapshots`, `inherit` and `env`, merged: `env` key by key, `snapshots` by kind and key (`mergeSnapshots`); a `~` value removes the key (`KEY: ~`, `snapshots.manual.keep: ~`, `inherit: ~`), and `""` stays empty.
- `config.local.yaml` replaces the project's top-level keys, but its `defaults:` and an environment of the same key (a pattern only over the same pattern) merge over the project's as `defaults:` do, `parent: ~` removing the parent (`mergeEnvironment`): a local file changing one key of an environment must not have to repeat the rest.
- The config is parsed as YAML nodes (`nullKeys`): a plain decode cannot tell `~` from absent.
- `.env` is read with compose's parser (`LoadDotenv`), and the caller's environment wins over it.
- A project command in `.devopsy/commands/` gets `DEVOPSY_PROJECT_DIR` and `DEVOPSY_COMMAND_NAME`, and `devopsy <its own name>` inside it reaches the compose command it wraps, never itself.
- `--debug schema [--user]` prints `ProjectSchema` and `UserSchema`, checked by `TestSchemas`: a new key goes there too.
- `--init` writes a new config and never touches an existing one.

## Targets

- A target is an address, `@[<server>:][<instance>/]<environment>` (`parseAddress`): the server ends at the last `:`, the instance before the last `/`, an empty part meaning none.
- A server is `[user@]host[:port]` or an IPv6 address, in brackets with a port (`remote.ParseServer`), from an address or a variable alike, and anything else is refused (`ServerError`), a user or host starting with `-` or holding a shell's special characters included: every address devopsy writes back (`<host>:<path>`, hints, a server alone as `<host>:`) splits again at its last `:`, and ssh never takes a server for an option.
- A server's port reaches ssh as `-p` after `DEVOPSY_SSH_COMMAND`'s options (`SSHArgs`), `ssh -G` included, and a server whose port ssh -G resolves to another is refused as its target loads strictly (`PortError` in `resolveTarget`, a parent's into `ParentErr`, a server alone in `runRemote`), never for help: ssh keeps the first value, so a `-p` there would win without a word.
- A missing part comes from `DEVOPSY_SERVER_<ENVIRONMENT>`, the pattern's variable (`PatternVar`, `pr-*` gives `DEVOPSY_SERVER_PR`), `DEVOPSY_SERVER` and `DEVOPSY_INSTANCE`, read from the caller's environment, then the project's `.env`, never for an alias; no server is an error.
- An explicit none (`@vm1:/prod`) only parses: it is the default.
- `instances: true` makes every target name an instance (an alias's from its `to:`), and without it none may: a project's environments never mix `<project>/<env>` and `<project>/<instance>/<env>` on a server, so `--environments` lists one layout.
- An environment key with `*` (one or more name characters) is a pattern; the exact name wins, then the most literal characters, and a tie is an error.
- An alias (`{to, source}` or `{to, project}`) matches only a bare `@name`, and a project's exact environment wins over it.
- An alias with `source:` takes its project, environments and steps from that checkout, and releases run only from there (`ReleasesHere`, symlinks resolved): anywhere else a release would upload whatever project devopsy runs in.
- `remote.ServerTarget` takes `@<server>:` alone, a bare name that is no environment or alias, or any target leniently, and a server named alone has no `Name`; a flag that needs an environment, given `@<server>:`, says so.
- `parent:` is set per environment (a pattern environment's included), never under `defaults:`, where it is an unknown key: a child gets its parent's data, secrets and personal data included, so it is named where the child is defined.
- `parent:` names a concrete environment, never a pattern; a cycle, one naming itself included, is an error, since neither could be released first; `parent: ~` is no parent, as any key's null.
- A parent resolves one level only, at the child's instance, its server from the usual variables, never from the child's (`ParentTarget`): a guessed server would copy the wrong data.
- A parent that cannot be resolved goes into `ParentErr` instead of failing the load, so every other command works on the child; `DescribeTarget` resolves it leniently (internals/syncs.md for what fails with it).
- A target's `From` holds each value's origin, and `Env` every definition of a variable as it loads (`EnvDef`, `TraceEnv`), what `--debug env` prints.

## Release settings

- A server's `releases:` (`root`, default the deploy user's home; `keep`, `max_keep`, default 5) and `snapshots:` (`root`, `max_keep`, `mirror`) are read by the hidden `--release-settings`, which every script asks first (`basePrelude`, `ReleaseSettings`): a laptop and CI then resolve the same paths.
- A project's `keep` is capped by the server's `max_keep`; `root` and `max_keep` in a project are errors.

## Secrets on a server

- On a server the release's own config decides what is a secret; `--vars` masks locally from the configs `VarsReadScript` sends plus the checkout's.
- `confirmUnnamedSecrets` runs locally, without a session: a secret-like name only in the server's files is left to `--vars set`.
