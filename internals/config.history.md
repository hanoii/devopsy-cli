History, not rules: `internals/config.md` wins where they differ.

- `~/.devopsy` was rejected as the user's config directory: project discovery walks up looking for `.devopsy/`, and a home directory holding one would have been taken for a project.
- A `path:` key once named a target's directory on the server; it went when names were made to come only from project, instance and environment (the root's Principles), so that the same target lands in the same place from any laptop or CI.
- `server:` in a project's config is an unknown key for the same reason: one checkout releases to any server, so the server is the target's (the address, `DEVOPSY_SERVER`, an alias), never the project's.
- Instances became all or nothing (`instances: true`) so that a project's environments never mix `<project>/<env>` and `<project>/<instance>/<env>` on one server and `--environments` lists one layout.
- A parent's server is never guessed from the child's: a guess would copy the wrong data.
- An unresolvable parent is recorded (`ParentErr`) rather than failing the load, so that every command but a sync works on a child whose parent's server is unknown locally.
- `confirmUnnamedSecrets` runs locally and without a session, by design: a secret-like name only in the server's files is left to `--vars set`, which asks the same question.
