# Project users

A project user, `devopsy-<project>`, is a Unix user for one project,
with no access to Docker's socket. It releases, rolls back, runs compose
commands, moves data and builds images as a deploy user does, but each
of those goes through devopsy's door: the project user asks the deploy
user's devopsy, through sudo, and that devopsy checks the request and
carries it out. A project user's leaked key reaches its project, not
the server or the other projects on it.

## Why

Docker's socket is root on the server. Whoever can talk to the daemon
can start a container that mounts `/` and writes anywhere, so a deploy
user in the `docker` group is root in practice. Every key that logs in
as it, a person's or a CI job's, is then a key to the whole server and
every project on it.

That is acceptable when one person runs every project. It is not when a
server holds projects for different clients, or when each project's CI
holds a key of its own: one leaked key, or one compromised release step,
gives away everything else.

A project user changes that. Its key logs in as a user that is not in
the `docker` group, so it reaches Docker only through the door, and the
door gives it only what belongs to its project: its containers, images,
volumes, networks, data and builds.

Projects still share one Docker daemon and one kernel. The door keeps
them apart by checking every request, which is a policy, and a policy
can have bugs. Stronger isolation means a daemon per project (rootless
Docker) or a machine per project. devopsy sets those aside: one Traefik
must reach every project's containers to route their sites, which
separate daemons prevent, and a project that needs a machine to itself
gets a server to itself. So the door's aim is narrower: an attacker
holding a project user's key needs a bug in the door, where a deploy
user's key needs nothing.

## How the door works

### A checked compose file, not the socket

On a server with project users, `docker` is devopsy. A project user's
`docker compose up` (or devopsy's own, in a release) runs devopsy, which
does two things:

1. It renders the project's compose file as the project user, with that
   user's rights: `env_file`, `extends`, interpolation and every other
   file compose reads are read by the project user, who can read only
   its own files.
2. It sends the rendered file and the command, through sudo, to the
   deploy user's devopsy (`devopsy --door ...`), which checks the file
   against its rules, rewrites what it must, and runs compose on that
   exact file.

The rules are deny by default: a compose key the door does not know is
refused, so a key compose adds in a later version fails closed.

A filter in front of Docker's socket could not do this. The filter and
the daemon can read the same JSON request differently, and one such
filter, tried for devopsy, had about thirty ways through it, most of
them that. Compose also reads files and runs programs as whoever runs it
(`env_file`, `extends`, `provider`), which no filter on the socket sees.
The door checks one rendered file, and compose runs that file as it is.

### The door's directory

The door keeps the projects' data, copies of what they mount and its
own records in a directory of the deploy user's, called `<space>` below,
on a mount of its own with `nosuid` and `nodev`. Without them, a setuid
file a container writes there, or a device node in a crafted tar, could
reach root on the host. The door refuses every request while `<space>`
is not mounted that way.

### What runs is what was checked

The door keeps each thing it checked fixed until it is used, since a
name or a path resolved again later is one nobody checked:

- Images run by their digest or id, never by a name: another project's
  pull, or the deploy user's build, can move a name in between.
- A bind mount's source is a real path the door made in `<space>`, not a
  path in the project's tree, where the project user could swap a link
  between the check and the start. Read-only files from a release are
  copied into `<space>` first, at each start (`up`, `create`, `run`),
  and a container keeps that copy ([Changing a file of a running
  release](#changing-a-file-of-a-running-release)). A bind is an entry
  itself (`./mnt/data`), never a directory inside one (`./mnt/data/sub`),
  which the door refuses.
- The rendered file reaches compose as an open file with no name on
  disk.

The door also never reads the project's tree as the deploy user. When it
needs to know what is there, it asks the project user's own devopsy
(`devopsy --door-read`, as that user, through sudo), so a link the
project user plants shows the door only what that user could read
anyway.

### Data

A project's data lives in entries the door makes, under
`<space>/mnt/<compose name>/`. The project's `./mnt/<name>` binds reach
them through a link in its tree ([Data](data.md)). The project user can
write inside an entry but not replace or move it.

Copies, imports, exports, snapshots and syncs run in short-lived
containers of devopsy's util image, as root. The door runs only the
forms devopsy itself uses, each a fixed command with fixed mounts, never
a container a caller describes. These containers have no network and
cannot write their own files, and what one writes in an entry belongs
to the project user again when the container exits.

### Images

Each project builds on a BuildKit builder of its own (`devopsy-shop`),
on a Docker network of its own (`devopsy-shop-build`), so no project's
build shares another's cache or reaches another's builder.

What a project user builds is named `devopsy.internal/<compose
name>/<service>`, whatever the service's `image:` says, and a release's
containers run its own build by a tag of that release's. `.internal` is
reserved for private networks and never exists on the public internet,
so a build never lands on a name a registry serves, which another
project could pull and run. `--releases` shows that name, and
`devopsy -- config` the `image:` the project wrote.

Any other image belongs to a project only once it has fetched the
image with its own registry logins (the `auths` of its
`~/.docker/config.json`), before it runs, inspects or tags it. A
private image another project pulled stays out of its reach without a
login, and a public one is fetched again for it. The door records what
each project fetched, so later starts do not contact the registry, and
compose runs the image by its digest (`nginx:1.27@sha256:...`).

Docker's daemon never contacts a registry a project user names: the
registry could send it, by a redirect or the address of a token, to
another project's container. The door fetches the image itself, the
registry's own bytes of the image for the server's platform, checks
that every layer is there and matches its digest, and has Docker load
them, which leaves the image as a pull by digest would. While it runs,
`<space>` needs room for the image's compressed layers.

The image a fetch lands is tagged at once as the project's
(`<repository>:devopsy-shop-claim.<digest>`, the first 12 hex digits
of its digest), so no other project's or the deploy user's pruning
removes it before the release's own tag holds it. The release's tag
then replaces it, and the project's next pruning removes any left
over. The fetch loads the image while holding the server's images
shared, so a removal of the deploy user's waits for it, naming the
project as claiming, and the fetch waits for a removal, saying
`waiting for the server's images, held by a removal, up to 10
minutes`.

Docker skips layers it already holds. A registry could use that to
serve a manifest naming another project's private layers without their
content, or an image whose config names another project's layers over
layers of junk. So the door checks that the registry served every
layer, and that each layer's content, unpacked, is the one the image's
config names. A layer it cannot unpack as Docker would, or of a kind it
does not take (a foreign or an encrypted layer), refuses the image.

### Keys and logins

A project user's keys are the deploy user's to edit
(`--ssh-keys --for shop`). sshd asks devopsy for them and never reads
the project user's own `~/.ssh/authorized_keys`, so a leaked key cannot
add another that survives its removal. Removing any of a project user's
keys ends every process the user runs, so nothing a leaked key started
outlives it, and removes the project's builder with its caches, which a
leaked key's build could have filled and the next build would trust.
A project user also has no cron or at jobs and no linger,
which would run its programs with no session. Nothing of its home runs
at a login: sshd runs no `~/.ssh/rc`, its login shell is devopsy's,
which reads no `~/.profile`, and it has no systemd user manager, which
would start the units in its home. sshd forwards it no agent, X11 or
tunnel either, so `--ssh-option ForwardAgent=yes` does nothing there.

A leaked key could still change what the project user owns: its home
and its tree, the steps of an older release among them, which a
rollback runs, and it could start environments of its own under the
project's names. After a leak, treat all of it as changed: release the
project again, remove the older releases (`--releases remove`),
`--destroy` any environment you did not release, and reset the home, or
start over: `--project-users remove shop --destroy` removes the user
with all it holds, and `add` makes it anew.

### The firewall

A project user's own processes on the host (its shell, its release
steps) would otherwise reach what the server keeps private: services
the host runs, other projects' containers on Docker's networks, and a
cloud's metadata service, which can hand out the machine's credentials.
devopsy loads a firewall for the whole server that refuses those to
project users, in IPv4 and, where the kernel supports it, IPv6. At the
host's own addresses, loopback included, they reach only port 53, for a
DNS resolver.

The metadata addresses are IPv4's link-local (`169.254.0.0/16`),
Alibaba's and Azure's addresses outside it, and the IPv6 addresses of
AWS, Google, OpenStack and Oracle. Containers on Docker's bridges
(`docker0`, `br-*`), the deploy user's included, cannot reach them
either, on this server as on any that `--server-setup` set up
([Servers](servers.md#--server-setup)): an app that takes the
instance's credentials from them needs credentials of its own. A
container on the host's network, on macvlan or ipvlan, or on a bridge
named otherwise is not covered, and a project user's compose file cannot
ask for the host's network. DNS at those addresses still answers, since
some clouds run their resolver there. A server with `door:` in its
config, or with the door's firewall loaded, refuses
`firewall: metadata: allow`, and devopsy never takes the door's firewall
off a server.

Docker's daemon contacts no registry a project user names, but it still
pulls for the deploy user, so the firewall keeps it off the host's
loopback and the metadata services too, DNS aside, by its cgroup
(`docker.service`'s, or the running `dockerd`'s)
([Registries](#registries)). The door's own registry clients (a
build's, the read and the copy of an image) are held too, and kept off
Docker's bridges as well: the door runs under a group of its own,
`devopsy-door`, which no user is in, and refuses to run under any other.
Docker's daemon still reaches the bridges, where its proxy connects to
containers, which is why it never fetches a project's images.

A project user's containers still reach what the host listens on at its
other addresses, its bridges' included, as on any Docker host: a site's
call to another site on the same server goes that way. A service on the
host that containers must not reach listens on `127.0.0.1` only.

The rules are in four chains devopsy owns, `DEVOPSY-PROJECTS`,
`DEVOPSY-DOCKERD` and `DEVOPSY-DOOR` in iptables' filter table and
`DEVOPSY-CONTAINERS` in
its raw table, and the server's other rules stay as they are.
`devopsy-firewall.service` loads them at every boot, a drop-in for
`docker.service` loads them at every start of Docker, whose daemon's
rule holds only the cgroup it was loaded for, and `--project-users add`
and `--server-setup` load them too. Nothing loads them in between: a
reload of the server's own firewall, or an `iptables -F`, drops them
until one of those runs again. On a server with no `door:` in its
config and no door's firewall loaded, the same loader and unit load
`DEVOPSY-CONTAINERS` alone. Syncs between servers work with the firewall
loaded.

`--server-info` reads the chains as root and warns, in its setup's
metadata firewall line, with `--server-setup` as the fix, when:

- a chain is not devopsy's, nothing jumps to it, or a rule ahead of
  devopsy's jump lets packets through (a reload of the server's own
  firewall, an `iptables -F`);
- Docker's daemon is in no cgroup of its own, so nothing covers it,
  which no setup repairs: Docker run as `docker.service` under systemd
  gives it one;
- Docker's daemon is in a cgroup made since the firewall loaded (Docker
  restarted outside its unit), which its rule does not hold; a start of
  Docker by its unit, or `--server-setup`, loads it again.

It also warns of a TCP listener open beyond loopback, other than
sshd's and the ports Docker publishes, since containers reach it.

A project user's release asks the door whether the firewall is loaded
as devopsy loads it, and when it is not, says a line for the server's
admin with a code to pass on ([Codes](codes.md)):

```
devopsy: warning: W0001: the server's firewall is not loaded as devopsy loads it
devopsy: | report W0001 to the server's admin
```

The release goes on, and the project user's `--server-info` says the
same line in its report. The door answers only that, from the deploy
user's own read of the chains as root. That `--server-info` shows no
setup of the server's, its admin's to judge, and skips Docker's own
facts (`docker info`, its compose and buildx versions), which the door
refuses a project user: it shows its system, disks, roles and
environments, the containers they need listed through the door.

## What a compose file may hold

A project user's compose file is an ordinary one, within these rules.
The door names what it refuses, and logs each refusal in
`<space>/refused.log`.

- Every service runs as the uid the server gives its environment, or
  its project's, as uid and gid both:
  `user: "${DEVOPSY_UID:?}:${DEVOPSY_UID:?}"`, or `DEVOPSY_PROJECT_UID`
  ([Variables on a server](config.md#variables-on-a-server)). No other
  environment's containers on the server run as it, so a process that
  escapes one container cannot reach another's through `/proc`. A user
  name is refused, since the image's own `/etc/passwd` could map it to
  root, and so is any other number.
- Every service has `security_opt: [no-new-privileges]`, and nothing
  else there. `cap_add` takes only `NET_BIND_SERVICE`.
- No `ports`: a project publishes nothing on the host, and the server's
  Traefik reaches its containers on a shared network.
- No `network_mode` but `none`, no `sysctls`, no `group_add`, no
  `devices`, no `privileged`. Log drivers are `json-file` or `local`.
  `deploy` holds only resources.
- A bind mount is an entry of `./mnt/<name>`, or a path in the release
  mounted read-only. Any other host path is refused.
- A build holds only `context`, `dockerfile`, `args`, `target`, `pull`
  and `no_cache`, its context inside the release. Another build context
  is only another service of the same file that builds
  (`additional_contexts: {base: "service:base"}`, compose 2.33.0 or
  later, the way to build one service on another,
  [Releases](releases.md)). A path is refused, since the deploy user's
  rights would read it, and so are an image and a URL, which the
  builder would fetch without the door recording them as the project's.
- A start never pulls or builds: every service starts with
  `pull_policy: never`, a project's own `pull_policy` is `missing`,
  `never` or `if_not_present`, and `--pull` takes only `never`. devopsy's
  releases build and pull before they start.
- A service is on its project's own networks, a shared network the
  deploy user let it join ([Shared networks, roles and
  vets](#shared-networks-roles-and-vets)), or none, never Docker's
  default bridge. Networks have no IPv6.
- On a shared network, a service is named `<project>-<name>`, with no
  alias, no fixed address, and a hostname that starts with its
  project's name, in lowercase. Docker's DNS answers for those names on
  the shared network, so without this rule one project could take
  another's names.
- A service holds none of compose's labels (`com.docker.compose.*`) or
  the ones devopsy sets on what it runs, and a role (`devopsy.role`)
  only one given to the project. An image whose own labels are under
  `devopsy.` or a vet's prefix is refused: they land on its containers,
  past the file's check.
- The file's `name:` is one of the project's compose names, and no
  `include`, `extends`, `env_file` or `provider` reaches the door: the
  render already read them.

## Setting up a server

A server for project users needs `sudo` and `acl` installed (`sudo`,
`visudo` and `setfacl`), which devopsy leaves to the admin, the Docker
every server runs ([Docker](servers.md#docker)), and what
`--project-users setup` sets. On a server with project users,
`--server-info` and `--server-setup` warn of a missing `sudo`, `visudo`
or `setfacl`; on one with none yet, `--server-info` notes it without a
warning and `--server-setup` says nothing of it. The commands below
address the server by its deploy user, `devopsy@vm1`.

1. Install `sudo` and `acl`, as root: `devopsy --server-setup-howto
   project-users` prints the steps ([Server setup
   how-to](server-setup-howto.md#project-users)).

2. Set the server up for project users, locally:

   ```sh
   devopsy @devopsy@vm1: --project-users setup
   ```

3. Add the project user and its keys, locally:

   ```sh
   devopsy @devopsy@vm1: --project-users add shop
   devopsy @devopsy@vm1: --ssh-keys --for shop add ~/.ssh/id_ed25519.pub
   ```

4. Reboot the server after the first `add`. sshd takes devopsy's part of
   its configuration at the next boot, and until then no project user
   logs in; `--project-users` says so.

### `--project-users setup`

`devopsy @devopsy@vm1: --project-users setup [<directory>] [--yes]` runs
as the deploy user, in two sessions as `--server-setup` does: it shows
what is missing, asks once, applies, and checks again. `--yes` asks
nothing, and without a terminal it fails naming `--yes`. A second run
changes nothing. It refuses a server without `sudo`, `visudo` or
`setfacl`, or with a Docker below the floor, before anything changes.
Neither `--server-setup` nor `--upgrade` runs it: project users are a
choice. It sets:

- The door's directory, `<space>`: `/var/lib/devopsy-door` unless the
  command names another, or the one `door: space:` names already, in
  which case another directory is refused (a space is moved by hand).
  It is made the deploy user's, mode 755, and bound on itself with
  `nosuid` and `nodev` now and at every boot by a mount unit of
  devopsy's in `/etc/systemd/system`, named as `systemd-escape --path`
  names it (`var-lib-devopsy\x2ddoor.mount`), never by a line in
  `/etc/fstab`. Docker's mounts drop-in names it beside the release
  root, so Docker starts after it at boot. A space whose mount went
  (a boot where it failed) is mounted again, its files and all: a
  directory bound on itself hides nothing. A space another user owns,
  one root owns that holds files, one that is no directory, or someone's
  mount without the two options is refused.
- `door:` with `space:` in the deploy user's `config.yaml`, appended as
  text when the file has no `door:`, the rest of the file as it is, once
  the directory is made and mounted, whatever else of the run failed.
  The two lines are shown before the question. A `door:` with no
  `space:` is refused, naming the line to add, and so is a file where
  lines added at its end would not be read (its YAML ends before its
  last line), naming the two lines to add by hand.
- Every user's processes hidden from the others: a process's command
  line names projects and paths, and a devopsy session's is its whole
  script, the operator's command in it. The group `devopsy-proc` is
  made, the deploy user put in it, and `devopsy-proc.service` remounts
  `/proc` with `hidepid=invisible` and the group's number early at every
  boot; where the server has polkit, a drop-in gives it the group. The
  deploy user sees every process from its next login: a session open
  during the setup, a shared SSH connection's included, sees only its
  own until it is opened again. A tool that runs as another user and
  reads `/proc` (a monitoring agent) sees only its own processes until
  it is put in `devopsy-proc`.

What is set another way is left as it is, judged by what the mounts
say, never by whose file made it: a space already on a mount with
`nosuid` and `nodev` (a line in `/etc/fstab`, by hand), or a `/proc`
already hiding processes with `gid=` a group the deploy user is in.
A `/proc` that hides them with no `gid=`, or for a group the deploy user
is not in, is refused. The files devopsy writes for it start with
`# Written by devopsy (--project-users setup).`

`add`, and `--server-info` on a server with project users, warn when
`/proc` has no `hidepid` of `invisible`, `ptraceable` or `noaccess`
(`2`, `4` or `1` on an older kernel), and go on; on a server with none
yet, `--server-info` says it without a warning. `noaccess` shows that a
process is there and whose it is, never its command line. `add` on a
server with no `door: space:`, or one not mounted `nosuid,nodev`, and
`--server-info`'s warnings of either, offer `--project-users setup` as
the fix, and the how-to's steps for doing it by hand.

`add` makes `devopsy-shop` with no password, a home closed to others,
and one id from 50000 to 59999 for its uid and its group, which no
container of the door's runs as, so a project user made later never
shares a running container's uid. The deploy user's own containers stay
out of that range too. An id is never given twice: the deploy user
records the highest it gave, so a project user added after another's
removal never owns what that one left on the host. An `add` that
stopped after it made the user's group goes on from that group, its id
the group's, when asked again; a group of that name with members, or
with a gid outside the range, is refused.
When the release root (`releases: root`) is outside the deploy user's
home, it also makes the project's tree under that root. The project
user's entry goes in the deploy user's config directory ([Shared
networks, roles and vets](#shared-networks-roles-and-vets)).

The first `add` also puts on the server what the door needs, each piece
checked before it lands: the `devopsy-projects` and `devopsy-door`
groups, the door's sudo rules, a `docker` that is devopsy, sshd's part, which asks devopsy for a
project user's keys, and the firewall. `add` refuses:

- while `<space>` is not mounted `nosuid,nodev`;
- while the deploy user or root is in `devopsy-projects`, which would
  have sshd ignore their own keys;
- on a server whose sshd would not read its part for the project user:
  an `sshd_config` with no `Include /etc/ssh/sshd_config.d/*.conf` at its
  top, before any `Match`;
- over a real `docker` at `/usr/local/bin/docker`;
- a Unix user `devopsy-<project>` that is there already and is no
  project user, someone's own account;
- a devopsy at a path sudo's rules cannot name: one with a space, `,`,
  `:`, `=`, `\`, `*`, `?`, `[`, `]`, `#`, `!` or a quote;
- a project's name that is another's followed by a dash, or the start
  of another's (`shop` and `shop-admin`): compose names are flat, so
  `shop`'s environment `admin-prod` would be `shop-admin`'s `prod`;
- `devopsy` and any name starting with `devopsy-`, which are devopsy's
  own, and `projects` and `door`, whose users would bear the names of
  devopsy's groups.

Every release keeps its project's directory, `<root>/<project>`,
closed to other users (mode 700). A project user passes through the
release root to reach its own tree, but can read none of the deploy
user's projects there. `add` closes the deploy user's projects already
in that root, and says which.

`--project-users` lists the project users, whether each Unix user is
there, and how many keys each has, and says each with no key whose
project still holds anything its `remove --destroy` removes (uid claims,
containers, volumes and networks under its names, its data, image names
and directories in the door's space), as `--server-info` does, and each
whose `remove --destroy` did not complete, with that command, which
completes it, instead. `--server-info` also checks that `<space>` is
mounted `nosuid,nodev`, that other users' processes are hidden from
project users, and whether sshd waits for a reboot.

## Releasing as a project user

The project's address names its user, in `.devopsy/.env` or anywhere
else an address goes ([Targets](config.md#targets)):

```sh
DEVOPSY_SERVER=devopsy-shop@vm1
```

Everything else is as it is with a deploy user: `devopsy @prod
--release`, `--rollback`, compose commands, `--export` and `--import`,
snapshots, a child's sync, all go through the door. Addressed through the
deploy user instead, devopsy refuses the project's environments before
changing anything: compose would act on the project user's containers
and volumes, which share their names. A sync goes between
two project users or two deploy users, never one of each, so a child on
another server needs its project user there too ([Syncs](syncs.md#how-the-data-travels)).

On the server, the project user's `docker compose` and `docker` work
for what is its own: its containers, images, volumes and networks.

On a server with project users, leave the deploy user's own builds
under the name compose gives them by default, from their compose
project, rather than an `image:` a registry serves: a project user's
pull of that name would replace the build with the registry's image.
An image with no registry host is a name on Docker Hub, so the door
fetches it like any other at a start, even with `pull_policy: never`.

A project user's `docker compose pull` is the door's own: it fetches
each service's image as above, skipping the project's own builds,
services with `pull_policy: never`, and those with `missing` or
`if_not_present` whose image the project has already, and says `pulling <image>` and then
its time on stderr, nothing with `-q`. It takes compose's `-q`,
`--ignore-buildable`, `--ignore-pull-failures` and `--include-deps`,
and stops at the first image that fails, where compose pulls the others
first.

## Changing a file of a running release

A read-only bind of a path in the release (`./config:/etc/app:ro`) is a
copy the door made when compose made the container. The container keeps
that copy for as long as it exists, so a file changed in place in the
release does not show in it, after a restart either.

Make the change in the checkout and release it: a release makes its
containers again, each with a copy of its own files.

A file changed in place on the server shows once compose makes the
container again, there with `docker compose up -d`, which makes again
each container whose copy is no longer the release's and leaves the
others running. The next release replaces that file with the checkout's.

`restart` and `start` keep each container's copy. Before they run, they
warn of the services whose copy differs from what the release holds now,
which is also the case for a container made from an older release, and
offer a release of the environment:

```
devopsy: warning: what these containers bind read-only differs from the release's: restart keeps the copy each was made with
devopsy: | web: ./config
devopsy: | to fix it:
devopsy: | release again, with any change made in the checkout, which makes these containers again:
devopsy: | $ devopsy @devopsy-shop@vm1:prod --release
```

A door that an older devopsy set up on the server says the same in words,
without the command.

A bind of `./mnt/<name>` is no copy: what is written there shows at
once.

## Registries

A project user's `docker login` keeps the login in its own docker
config (`~/.docker/config.json`, or under `DOCKER_CONFIG`), unchecked:
it warns `<registry>: saved, not checked: the first pull checks it` and
prints no `Login Succeeded`. A wrong password shows at the next pull or
release, in the registry's words: checking it at the login would have
Docker's daemon contact the registry for the project. The door keeps a
copy of those logins' `auths` in `<space>`, for its own requests, until
`--project-users remove`. The password comes from stdin (`--password-stdin`,
with `--username`) or a prompt, never the command line, where every
user of the server would see it.

The door fetches with those logins alone. A registry at the server's
loopback, link-local or a metadata address is refused, for a login and
a pull alike. A registry
on the same server is reached by its name or the address of Docker's
bridge. Since the firewall keeps Docker's daemon off loopback, the
deploy user's own pulls and logins reach no registry there either, a
mirror included: put one on a build server, or reach it by its name or
Docker's bridge address.

A registry with a CA of its own, or a mirror, needs the door's clients
set up, or no project user can use its images. Docker's daemon needs
none of it for them:

- the builders: `<space>/buildx/buildkitd.default.toml`, read when a
  project's builder is made;
- the server's CA store (`/etc/ssl/certs/ca-certificates.crt` on Debian
  and Ubuntu), which the door trusts when it reads and copies an image.

## Shared networks, roles and vets

A project user's entry is a file of the deploy user's,
`~/.config/devopsy/door/users/devopsy-shop.yaml`, which `add` writes and
the deploy user may extend (`devopsy --debug schema` lists its keys):

```yaml
project: shop
joinable: [traefik]
roles: [app]
```

`joinable` lists the shared networks the project may join, Traefik's
for example. `roles` lists the roles its services may claim
([Roles](roles.md)).

devopsy knows no URL, so it cannot tell whether a Traefik label routes
another project's domain. A program of the server's own can: `vets` in
the deploy user's config names programs for labels under a prefix.

```yaml
door:
  space: /var/lib/devopsy-door
  vets:
    - prefix: traefik.
      command: /usr/local/bin/door-vet-traefik
```

When a file that starts containers (`up`, `create`, `run`) has labels
under a vet's prefix, the door runs its program with the file as the
door checked and rewrote it, as JSON, on stdin, `DEVOPSY_DOOR_PROJECT`
set to the project, and `DEVOPSY_DOOR_JOINABLE` to its joinable
networks, separated by commas. Exit 0 allows the file. Anything else
refuses it, with what the program printed on stderr. The program runs
under the door's group, so the firewall keeps it off the host's
loopback and Docker's bridges: an API it asks (Traefik's, for example)
is reached at another of the host's addresses.

## Moving a project to its own user

The door acts only on containers it made for the project user. While a
container it did not make is under the project's names, `add` refuses,
and so does every compose command of the project user's. A project the
deploy user already runs moves this way:

1. Take each environment's data out as the deploy user, while it still
   runs there (`devopsy @prod --export <service> >prod-db.tar`,
   [Data](data.md)).
2. Take its environments down with their volumes kept (`docker compose
   -p shop-prod down`, as the deploy user), the command `add` names.
3. Move the deploy user's tree of the project aside: `add` refuses a
   tree that is there and not the project user's.
4. `--project-users add shop`, its keys, and its address in the project.
5. Release it as the project user, then import each service's data
   (`devopsy @prod --import <service> <prod-db.tar`).

Once the project has its user, a release by the deploy user under its
names is refused before it goes live.

## Removing a project user

`--project-users remove shop` drops its keys, so it can no longer log
in or use the door, then ends every process it runs and removes its
builder and the builder's network:

```sh
devopsy @devopsy@vm1: --project-users remove shop
```

Its entry stays, so its compose names stay its own and no deploy
project takes them, and so do its Unix user, home and tree, and its
environments' containers and volumes. A key added for it (`--ssh-keys
--for shop add`) takes it back. What the door kept for it, its registry
logins and the record of the images it claimed, goes.

If the end of its processes fails (Docker restarting, say), the removal
says so and stays marked: the door answers the user nothing and sshd
takes none of its keys, and the same command run again finishes it, as
does a listing of the user's keys (`--ssh-keys --for shop`) while its
Unix user is there. `--project-users` lists any removal left so, and
`--project-users add` of that project refuses until it is finished. A
user under `door: users:` in the config is removed the same way, then
named for its admin to drop from there once nothing is under its names;
while something is, the removal names `--destroy` first, since that line
is the one record of them.

### Removing everything it holds

`--destroy` after it removes the user, then everything it holds, and
its Unix user last:

```sh
devopsy @devopsy@vm1: --project-users remove shop --destroy
```

It goes by the deploy user's own records, never by reading the user's
tree or running anything of it, so no `destroy:` step of the project
runs: the uid claims of its environments, the containers, volumes and
networks under its compose names with their data in the door's space,
the images only it names (its releases' bookmarks, its builds, its
claims), its sealed copies, clones and work directories, its tree, its
home and its Unix user. It lists them first, the tree and home with
their sizes, and asks you to type the project's name; `--yes` asks
nothing, and without a terminal it needs `--yes`.

It refuses before removing anything when a name it found belongs to
another project now (a claim of `shop`'s environment `eu-prod` is
`shop-eu-prod`, which a project `shop-eu` may hold), when the tree its
entry names is not a directory (a disk not mounted), when that tree or
the user's home is one it does not remove (a link, no directory, or a
path not named for the project or the user), when a Unix user of that
name is not in `devopsy-projects`, when the uid claims' directory is
missing while containers or volumes are under the project's names (the
release root's disk not mounted, perhaps), and when the release root
does not read. An image still used by a container stops
it, as does anything left once it is done, and the same command run
again goes on from where it stopped: until it completes, the user takes
no key, its keys cannot be listed or changed, and `--project-users add`
refuses. `--project-users` and `--server-info` show a destroy left so.

It leaves the snapshots under a `snapshots: root` of the user's own
outside its tree, which the deploy user holds no record of, and the
user's lines in `/etc/cron.deny` and `/etc/at.deny`. The uid it had is
never given to another project user.

Removing a project user without `--destroy` clears nothing a leaked key
left in its home: delete and recreate the home too, or remove it with
`--destroy` and add it again.

## What it costs

- A server with project users needs more than Docker, devopsy and a few
  tools: sudo, ACLs, a mount of its own, and the firewall.
- The door rewrites the file compose runs (images pinned, binds moved
  into `<space>`, builds renamed), so `devopsy -- config` and a plain
  `docker compose` on the server do not show exactly what runs.
- A registry or mirror on the server's loopback is out of reach of the
  deploy user's pulls too.
- A project user's `docker compose pull` is the door's, its output and
  its stop at the first failure unlike compose's, and its `docker login`
  is checked only by the next pull.
- Every Docker call or compose key devopsy adds needs a rule in the
  door before project users can use it.
