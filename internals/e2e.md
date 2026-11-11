# End-to-end tests

Code: `e2e/` (`main_test.go` holds the lanes, the containers and the helpers; `Dockerfile`, `entrypoint`, `daemon.json`), `.github/workflows/e2e.yml`.

## The setup

- The servers are privileged Debian 13 containers with their own Docker and sshd, the operator a container too, devopsy built from the checkout in each; the tests drive them with `docker exec` and check from outside.
- The suite runs locally with any Docker, on GitHub on demand and nightly when `main` moved since the last run that passed (`e2e.yml`); the release workflow does not run it.
- `E2E_BEAT` (default 1) is passed as `DEVOPSY_INTERNAL_HEARTBEAT_EVERY`, so quiet times and the sessions' keepalive keep their order; `E2E_BEAT=5` runs everything with the real timeouts, and one test always does.
- The test server's deploy user has `/bin/sh` (dash) as login shell, stricter than devopsy-server's bash.
- `e2e/daemon.json` carries `--server-setup`'s want for `daemon.json` (`setup.Parts`): a want changed changes it too, as the e2e servers have no systemd to apply it.
- The deploy user's home, where the releases go, is a tmpfs (`entrypoint`), so nothing clones by reflinks there on any host; a test of reflinks mounts an XFS loop device of its own below it (`TestSnapshotReflinkE2E`, `TestSyncReflinkE2E`), and one of their absence an ext4 (`TestSyncReflinkExt4E2E`).
- Servers pull through a mirror of Docker Hub (`startMirror`, the volume `devopsy-e2e-mirror` its cache), so four lanes cost Docker Hub no more than one.
- Lanes (`E2E_LANES`, default 4; `runLanes`) run the tests at once: each a network, an operator and a server (a second one when a test asks), each test a process of its own on the first lane free, the slowest first by its last time (`timesFile`, in the user's cache directory); within a lane they run one at a time, and `resetLane` undoes between them what a test may leave.
- `E2E_LANES=1` is one process running every test in order; CI runs two jobs of two lanes, half the tests each (`E2E_SHARD`, round robin by name).
- `E2E_KEEP=1` keeps the containers after the run.
- A project user's door has a server of its own (`doorServer`), started once per lane by the tests that need it, since its sudo rules and `docker` link are the whole server's.

## Writing one

- A scenario a review finds goes here only when no unit test can cover it (Workflow's Tests and checks).
- Use only the globals (`run`, `network`, `server` and `operator`, set per process by `lane.use`, and `server2`, set by `secondServer(t)` for a test across servers), the helpers on them (`op`, `srv`, `srv2`) and `@server:` and `server2` as addresses, never a container or network name: they end with the lane's id.
- Every `project(t, name, ...)` name is unique in the suite, named after the test: a lane's tests share its server.
- What a test changes outside its project (iptables, `tc`, the login shell, a process on the operator, a file in the deploy user's home) is undone in `t.Cleanup`, and a new kind of leftover goes into `resetLane` too: a cleanup never runs when a test's process dies on its timeout, and the next test on the lane must not see it.
- No test depends on another's state or on the order.
- A new test's time lands in `timesFile` after its first run; one far longer than the rest bounds the whole run, so split it only into independent behaviors, each still checking all it did.

## By hand

- What only a real setup shows (Traefik, certificates, the templates) is checked by hand on a throwaway Linux machine set up with devopsy-server and a build of the checkout installed in it (for example an OrbStack Debian 13 machine, `orb create debian:trixie devopsy-test`, a linux/arm64 build, and targets like `@devopsy@devopsy-test@orb:prod`, since OrbStack's SSH needs no keys), deleted afterwards.
- The hand test releases the Traefik template, then a template importing from it, and checks `devopsy-target.env`, `--debug imports`, the Traefik `domains` command, an instance, a pattern environment, `--destroy`, a rollback and a release with Traefik stopped.
