# Servers

Commands with a target run on its server, over SSH. The ones a project
uses most:

```sh
devopsy @vm1:prod --release      # upload, switch, run the release steps
devopsy @vm1:prod --rollback     # back to the previous release, run the rollback steps; the one left is marked rolled back
devopsy @vm1:prod --reload       # the release again with its variables and imports as they are now, then its reload: step
devopsy @vm1:prod --releases     # list them and their images (version or short digest), * marks the current one
devopsy @vm1:prod --releases remove failed   # remove releases (ids or failed) and the images only they hold
devopsy @vm1:prod --log          # what the last release or rollback printed (or --log <id>)
devopsy @vm1:pr-12 --destroy     # take it down and remove the environment, data included
devopsy @vm1:develop --sync     # replace a child's data with its parent's, run its run step
devopsy @vm1:develop --reset    # start a child over: all its data goes, then a sync as on its first release
devopsy @vm1:develop --release-sync       # a release and the parent's data in one go (--release-no-sync: never)
devopsy @vm1:prod --export database > db.sql   # a service's data, as its devopsy.data.copy label says
devopsy @vm1:prod --export database | devopsy --import database   # prod's database locally
devopsy @vm1:prod --import database --resolved   # accept the data an import that failed left, start what it kept down
devopsy @vm1:prod --snapshot     # a snapshot: every service with data stopped for the moment of its copy, code and images kept
devopsy @vm1:prod --snapshots    # list them; restore, remove, export or import one
devopsy @vm1:prod --snapshots restore <id>   # its data, code and images back, as a new release (--no-code: the data alone)
devopsy @vm1:prod --snapshots export <id> | aws s3 cp - s3://backups/prod.tar   # a snapshot off the server
devopsy @vm1:prod --status       # the release there, a sync running into or from it, the disks, its containers, images held or moved, the project's own lines; exit 3 when something needs attention, --json for fields
devopsy @vm1:prod --env          # its variables there, env capability included, secrets masked
devopsy @vm1:prod --debug imports  # what devopsy sees there (capabilities, labels, imports)
devopsy @vm1:prod logs -f web    # any command, in the current release
devopsy --environments           # the project's environments on its servers: vm1:prod, vm1:b/prod...
devopsy @vm2 --environments      # on one server
```

`--environments` without a target asks each server that `DEVOPSY_SERVER`
and the environments' `DEVOPSY_SERVER_<ENVIRONMENT>` name (from the
caller's environment, then `.devopsy/.env`). It lists only the project's
layout, `<instance>/<environment>` with `instances: true`, else
`<environment>`.

## Server commands

Every command needs an environment, except these, which need only a
server: `--shell-host`, `--ssh-keys`, `--ssh-config`, `--server-info`,
`--environments`, `--vars-project` and `--upgrade` (`--environments` and
`--vars-project` with the checkout's project, or an alias's).

They take `@<server>` alone, or any target, whose server they use, with
no instance needed. A bare name is still the project's environment or an
alias first, as for every target, and a server only otherwise; when a
name is both, `@<server>:` says it is the server. A server with a port
or an IPv6 address always ends with `:` (`@vm1:5369:`,
`@2001:db8::1:`), since without it the last part is an environment.

```sh
devopsy @vm1 --ssh-keys          # vm1's deploy user's keys, from anywhere
devopsy @prod --ssh-keys         # prod's server's, in the project
devopsy @vm1 --upgrade           # the server's devopsy, as the deploy user
devopsy @vm1 --server-info       # what the server is, and what needs attention
```

## `--server-info`

`--server-info` reads the server in one session that changes nothing and
waits for no lock. Without a target it reads the Linux server it runs on,
in `--shell-host`; anywhere else it refuses. It reports:

- its devopsy and release settings;
- the system, with the memory the kernel keeps for Kexec HandOver and
  kdump, and whether its clock is synchronized and how far it is from the
  local one;
- Docker: its image store, security options and logs;
- the filesystems of the release root, Docker's and containerd's data,
  the snapshot root and `/`: free space, and whether the release root
  makes reflinks;
- the roles projects hold and what they export;
- every environment under the release root, with its current release,
  its containers and its state.

What needs attention is a warning under the line it is about, with its
fix:

- an older devopsy;
- a reboot the system asks for;
- memory Kexec HandOver keeps as CMA. Only movable pages may use it, so
  the kernel's allocations can run out while memory is free. `kho=off`
  frees it, and devopsy-server's `setup.sh` adds it;
- a clock out of sync;
- a kernel with AppArmor and no `apparmor_parser`, on which no container
  starts;
- Docker logs that grow without a limit;
- a disk 90% full, or with less than 5 GB free;
- no reflinks;
- a snapshot root on the same filesystem as the release root, which
  usually means the backup volume is not mounted;
- a tool the server needs, missing;
- a role held twice;
- an environment with a failed first release, an activation cut off past
  its switch, a sync, an import or a restore that did not complete, an
  import or a restore under way (what it stopped stays down until it
  completes), services an operation left stopped, or containers
  restarting, paused, dead, unhealthy, or exited on an error of their
  own (a stop's codes excepted), each as `--status` says it.

A sync or a snapshot under way is reported as information. So is the
memory kdump keeps, with how to free it
(devopsy-server's `DEVOPSY_KDUMP=off`), since crash dumps are a choice.

It exits 3 when it warned, so CI can run it as a check. `--json` prints
the facts and the same lines as JSON on stdout, each with its fixes and
hints (`remedies`), for scripts and agents.

## The server's devopsy

Commands on a server run part of their work in the server's devopsy
(a release's images, for example), so it must be recent enough for
yours. An
older one stops a command that needs what it lacks before anything
changes, says what is missing, and says to run `devopsy @<target>
--upgrade`, which always works. Commands that need nothing new still run.
A sync across servers checks the child's devopsy against the parent's
the same way, before the parent stops anything.

`DEVOPSY_PROTOCOL_CHECK=warn` turns that stop into a warning and goes on,
at your own risk. It is for a local build ahead of the servers, when what
you run may not need what the server's devopsy lacks.

## Shells

- `--shell` opens a shell in a container, as locally
  ([Shells](local.md#shells)).
- `--shell-host` opens your login shell on the server, in the current
  release (in the release root with `@<server>` alone). It does not depend
  on the project, so it is the way in when the project is broken.
- `--shell-host --root` opens root's, in its home, as root even when the
  address or `~/.ssh/config` names another user (ssh's `-l root`). Root's
  `authorized_keys` needs your key.

Project commands on a server come from the current release. An
environment's directory that is a plain `.devopsy/` directory without
releases runs commands in place, and refuses releases.

## CI

Set `DEVOPSY_SSH_COMMAND` for SSH options, as git's `GIT_SSH_COMMAND`:

```sh
DEVOPSY_SSH_COMMAND="ssh -i $KEY -o UserKnownHostsFile=$KNOWN_HOSTS" devopsy @vm1:prod --release
```

Its options come first, and ssh keeps the first value of each, so they
win over devopsy's. A port goes in the server instead
(`DEVOPSY_SERVER=devopsy@203.0.113.7:5369`, [Targets](config.md#targets)),
since a `-p` there applies to every server a command reaches, a parent's
on another server included. devopsy refuses a server whose port
`DEVOPSY_SSH_COMMAND` overrides.

For one command, `--ssh-option <key=value>` before everything else, as
many as needed, adds an ssh `-o` to every session it opens, after
`DEVOPSY_SSH_COMMAND`'s options and before devopsy's:

```sh
devopsy --ssh-option ForwardAgent=yes @vm1 --shell-host
```

devopsy adds `-o ClearAllForwardings=yes` to every session. It needs none
of the host's `LocalForward` lines, and each session would try to bind
them again while another holds them. It adds `-o ForwardAgent=no` too,
whatever `~/.ssh/config` says: a session's user, a project user's above
all, would otherwise reach every server your agent's keys open. A sync
across servers needs none: it uses a key made for it
([Syncs](syncs.md)). `--ssh-option ForwardAgent=yes` asks for the agent
by hand; on `--shell` it reaches the server, not the container.

A key for CI is set up with `--ssh-keys` (Keys on a server, below).

## Keys only

devopsy's sessions never ask for a password (`-o PasswordAuthentication=no
-o KbdInteractiveAuthentication=no`, over `~/.ssh/config`). A devopsy
server takes keys only, so a missing key fails at once with ssh's
`Permission denied (publickey)`. Otherwise ssh would ask in the middle of
a release's sessions, or while a child's services are stopped for a sync.
A key's passphrase and a new host key are still asked for. For a server
you reach by password, set `DEVOPSY_SSH_ALLOW_PASSWORD=1`.

## Environments nobody releases

Floating tags (`mariadb:11.8`, `caddy:2-alpine`) and base images get their
fixes with a release, which pulls them again
([Choosing tags](images.md#choosing-tags)). An environment nobody releases
for weeks keeps what it pulled last. Schedule a release from CI, weekly
for example, with the same restricted key (`devopsy @prod --release` in a
scheduled job).

## SSH settings

A release opens several SSH sessions (the checks, the upload, the
activation, its log), each a new connection and handshake, which shows
through a jump host or a VPN. `devopsy @<server> --ssh-config` prints a
`Host` block for `~/.ssh/config`: where it connects (`Hostname`, `User`,
`Port`), then connection sharing (`ControlMaster`, `ControlPersist`) and
keepalives, commented out. It does not connect.

## Keys on a server

The deploy user's `~/.ssh/authorized_keys` decides who may log in as
that user. `devopsy @<server> --ssh-keys` edits that file over the usual
session, as that user, without root; any target on the server works
too.

- `--ssh-keys` lists the keys: fingerprint, type, comment, options.
- `--ssh-keys add <file>` authorizes a public key. `add --restricted` is
  for a machine's key: `restrict` (commands only, no shell, no pty, no
  forwarding).
- `--ssh-keys remove <fingerprint or comment>` removes one. It refuses
  the last, which would lock everyone out.
- `--ssh-keys --for <project>`, with any of the above, edits a project
  user's keys instead, on a server with project users (below), logged in
  as the deploy user: they are kept in its config directory, out of the
  project user's reach, and sshd asks devopsy for them. Removing any of
  its keys ends every process that user runs, a session of another key's
  included (a release cut there is marked and settled by the next one);
  its last key may go, which is how its logins stop, and the door
  answers it no more.
- Without a target, `devopsy --ssh-keys-generate [<name>]` makes an
  ed25519 pair in `~/.ssh` (`devopsy-ci` by default; `--out <file>` for
  elsewhere), sending nothing anywhere.

For CI: generate a pair, `add` the `.pub` on each server with
`--restricted`, store the private key as a protected file variable, and
in the job run `devopsy --ssh-option IdentityFile="$DEVOPSY_SSH_KEY"
@prod --release`, or load it into an agent. To rotate it, generate another, add it, then
remove the old one.

## Project users with no Docker socket

A deploy user's key is root on the server, since its group reaches
Docker's socket. A project can have a Unix user of its own that does
not: it finds devopsy as `docker`, and asks the deploy user's devopsy
through sudo for what its compose file needs, each request checked. A
leaked key of that user then reaches its project, not the server or the
others. Everything else stays as it is on a server without it.

A server takes project users once its deploy user's config names the
door's space, a directory of the deploy user's on a mount of its own
with `nosuid` and `nodev` (the door refuses every request otherwise):

```yaml
door:
  space: /var/lib/devopsy-door
```

```
/var/lib/devopsy-door /var/lib/devopsy-door none bind,nosuid,nodev 0 0
```

Then, as the deploy user, with the server named with its user:

```sh
devopsy @devopsy@vm1: --project-users add shop
devopsy @devopsy@vm1: --ssh-keys --for shop add ~/.ssh/id_ed25519.pub
```

`add` makes `devopsy-shop` (no password, its home closed to others, no
cron or at jobs),
and, with `releases: root` elsewhere than the deploy user's home, its
tree there. Its entry is a file of the deploy user's,
`~/.config/devopsy/door/users/devopsy-shop.yaml`, where an admin may add
`joinable:` or `roles:` (`devopsy --debug schema`). The
first `add` also puts on the server what devopsy needs there, each piece
checked before it lands: the `devopsy-projects` group, the door's sudo
rules, a `docker` that is devopsy, and sshd's part, which asks devopsy
for a project user's keys and never reads its own
`~/.ssh/authorized_keys`. sshd takes that part at the next reboot: until
then no project user logs in, and `--project-users` says so. It refuses
while the deploy user or root is in `devopsy-projects`, which would have
sshd ignore their own keys, and on a server whose sshd would not read
its part for the project user: an `sshd_config` with no `Include
/etc/ssh/sshd_config.d/*.conf` at its top, before any `Match`. It also
refuses a devopsy at a path sudo's rules cannot name, one with a space,
`,`, `:`, `=`, `\` or a quote.

`add` also loads a firewall for project users, one for the whole
server, for IPv4 and, where the kernel supports it, IPv6. A project
user's processes on the host reach no container on Docker's networks
and no cloud metadata service. At the host's own addresses, loopback
included, they reach only port 53, for a DNS resolver. The metadata
addresses are IPv4's link-local (`169.254.0.0/16`), Alibaba's and
Azure's addresses outside it, and the IPv6 addresses of AWS, Google,
OpenStack and Oracle. Containers on Docker's networks, the deploy user's
included, cannot reach them either: an app that takes the instance's
credentials from them needs credentials of its own. DNS at those
addresses still answers, since some clouds run their resolver there.
Syncs still work with the firewall loaded.

Docker's daemon contacts registries for project users too, as root, so
the firewall keeps it off the host's loopback and the metadata
services, DNS aside, by its cgroup (`docker.service`'s, or the running
`dockerd`'s): a registry a project runs could otherwise send it there.
On a server with project users, the deploy user's own pulls and logins
reach no registry on loopback either, a mirror included: put one on a
build server, or reach it by its name or Docker's bridge address. With
Docker's daemon in no cgroup of its own, `--server-info` warns that
nothing covers it.

A project user's containers, like any container on a Docker host, still
reach what the host listens on at its other addresses, its bridges'
included: the firewall leaves them alone, since refusing them would
also refuse a site's call to another site on the same server. A
service on the host that containers must not reach listens on
`127.0.0.1` only, which no container reaches. `--server-info` warns of
every TCP listener beyond loopback, other than sshd's and the ports
Docker publishes.

The rules are in three chains devopsy owns, `DEVOPSY-PROJECTS` and
`DEVOPSY-DOCKERD` in iptables' filter table and `DEVOPSY-CONTAINERS` in
its raw table, and the server's other rules stay as they are. `devopsy-firewall.service`
loads them at every boot. From the next boot on, which the first `add`
needs anyway for sshd, `devopsy-firewall.timer` loads them every five
minutes too, so if a reload of the server's own firewall drops them,
they are back within five minutes. `--server-info` checks them and
warns when a chain is not devopsy's, when nothing jumps to it, or when
a rule ahead of devopsy's jump lets packets through: `add` loads them
again. `--server-info` also checks that the door's space is mounted
`nosuid,nodev`, and whether sshd waits for a reboot.

A project's name may not extend another's on the server, or be
extended by one (`shop` and `shop-admin`): compose names are flat, so
`shop`'s environment `admin-prod` is `shop-admin`'s `prod`.

The door acts only on containers it made for the project user. A
project the deploy user already runs moves to its own user once it is
down: `add` refuses while containers the door did not make are under
the project's names, naming the `docker compose -p <name> down` that
takes them down with their volumes kept, and so does every compose
command of the project user's while one is there. `add` also refuses a
tree that is there already and not the project user's, the deploy
user's releases of the project for example: move it aside first. Once
the project has its user, a release by the deploy user under its names
is refused before it goes live.

Every release keeps its project's directory, `<root>/<project>`,
closed to other users (mode 700), so a project user reads none of the
deploy user's projects, even under a release root it enters to reach
its own tree. `add` closes the deploy user's projects already in that
root, and says which.

A project user has images of its own too: those it builds, and those
its releases hold. devopsy names what a project user builds
`devopsy.internal/<compose name>/<service>`, whatever the service's
`image:` says, which is the name `docker ps` and `--releases` show;
`devopsy -- config` shows the `image:` the project wrote. `.internal`
is reserved for private networks and never exists on the public
internet, so a project user's build never lands on a name a registry
serves, which another project may pull and run. Any other image is pulled once with the project's registry logins
(the `auths` of its `~/.docker/config.json`) before the project runs
it, inspects it or tags it, even when another project has it on the
server already. So a private image another project pulled is never
this project's without its own login, and a public image is pulled once
more for it. The door records what each project pulled this way, so
later starts do not contact the registry. Compose then runs such an
image by its digest (`nginx:1.27@sha256:...`), and the project's own
builds by their image id, so nothing that moves a name afterwards
changes what a start runs. An image with
`pull_policy: never` and no registry host is pulled like any other: its
name is a registry's. A project can't be named `devopsy` or anything
starting with `devopsy-`, which are devopsy's own names.

Each project builds on a BuildKit builder of its own, on a Docker
network of its own (`devopsy-<project>-build`), so no project's build
shares another's cache or reaches its builder. The first time a project
uses an image this way, its own builder fetches the image again from
the project's registry, the image for the server's platform, to prove
the registry serves its content and not only its name: Docker skips
the layers it already has, so a registry could otherwise name another
project's image without having it. That costs one more download of
what the builder lacks, once per layer. The builders keep their own
registry settings, in `<space>/buildx/buildkitd.default.toml`, read
when a project's builder is made: a registry the server's Docker
trusts through `certs.d` or `insecure-registries`, or a mirror of
Docker Hub it pulls through, goes there too, or no project user can
use that registry's images.

On a server with project users, the deploy user's own projects name
their builds as compose does by default, under their compose name,
never as an image a registry serves: a project user's pull of that name
replaces the build with the registry's image.

The project user's `docker login` goes through the door. Docker checks
the login against the registry as it does for a pull, so a registry on
the same server accepts it, reached by its name or the address of
Docker's bridge: a registry at the server's loopback, link-local or a
cloud metadata address is refused, for a login and a pull alike, since
Docker would contact it for the project user. The project user's
devopsy then keeps the login in its own docker config (`~/.docker/config.json`, or under
`DOCKER_CONFIG`). The password is read from stdin (`--password-stdin`,
with `--username`) or at a prompt, never from the command line.

The project's address then names its user: `DEVOPSY_SERVER=devopsy-shop@vm1`.
A child on another server needs its own project user there
([Syncs](syncs.md#how-the-data-travels)).

`--project-users remove shop` drops its entry and keys, so it logs in no
more and the door answers it no more, then ends every process it runs;
its Unix user, home and tree stay. Neither clears what a leaked key left
in that user's home: reset the home too.
