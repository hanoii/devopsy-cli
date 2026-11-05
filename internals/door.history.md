# The door's history

History, not rules: `internals/door.md` wins where they differ.

- Docker's socket is root, so a leaked deploy key was the whole server. A policed socket came first, tried and reviewed: one round found about 33 ways through, most from forwarding JSON the daemon read differently. Compose also acts as whoever runs it (`env_file`, `extends`, `provider`), which no socket sees. So the file is rendered by the project's own user, with its own rights, and checked before compose runs it.
- Moving the scripts to the server first was weighed and put after: the door works with them as they are, and a key that may release runs a project's steps on the host either way.
- The first version kept the project's tree the deploy user's, which makes every write a request. The project user owns it instead, and nothing the door does trusts it. Daemons resolve a bind at each start, so pinning came from a swapped link reaching another project at a restart, in a test with its control.
- `nosuid,nodev` came from reading the door as an attacker, then testing: a container's setuid file reached another project through `/proc`, and a crafted tar gave root on the host.
- Two review rounds, on Opus and Fable, found what the door's own attack rows had missed. The first: mounts over a util image's binaries, a second `--mount` source, variables compose expands after a check, keys in other forms, a world-readable space. The second: still more routes to root, from checking a util container's command and mounts apart, after named requests had been put off as adding nothing. So a util container became one form of a table, its command, arguments and mounts together.
- A password written to ssh's stdin raised SIGPIPE when ssh exited first, which `interruptible` takes for the session's end: first blamed on load, until the same test on `main` passed every time. Hence a pipe filled before ssh starts.
- Firewalling all private ranges broke a sync over a private network, found by the first end-to-end run of it: only Docker's pools are blocked.
