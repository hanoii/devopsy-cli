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
  copied into `<space>` first.
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

Any other image belongs to a project only once it has pulled the image
with its own registry logins (the `auths` of its
`~/.docker/config.json`), before it runs, inspects or tags it. A
private image another project pulled stays out of its reach without a
login, and a public one is pulled again for it. The door records what
each project pulled, so later starts do not contact the registry, and
compose runs the image by its digest (`nginx:1.27@sha256:...`).

Docker skips layers it already holds. A registry could use that to
serve a manifest naming another project's private layers without their
content, and the pull would get those layers. So the first time a
project uses an image, its own builder fetches it again from the
registry, the image for the server's platform. The builder holds only
what its project fetched or built, so every layer it lacks must come
from the registry. That costs one more download of those layers.

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
`--destroy` any environment you did not release, and reset the home.

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
AWS, Google, OpenStack and Oracle. Containers, the deploy user's
included, cannot reach them either: an app that takes the instance's
credentials from them needs credentials of its own. DNS at those
addresses still answers, since some clouds run their resolver there.

Docker's daemon contacts registries for a project user's pulls and
logins, as root, so the firewall keeps it off the host's loopback and
the metadata services too, DNS aside, by its cgroup (`docker.service`'s,
or the running `dockerd`'s): a registry a project runs could otherwise
redirect the daemon there. That also holds for the deploy user's own
pulls ([Registries](#registries)). The door's own registry clients (a
build's, the read of an image's manifest) are held too, and kept off
Docker's bridges as well: the door runs under a group of its own,
`devopsy-door`, which no user is in, and refuses to run under any other.
Docker's daemon still reaches the bridges, where its proxy connects to
containers, so a pull can still make it contact a container on one.

A project user's containers still reach what the host listens on at its
other addresses, its bridges' included, as on any Docker host: a site's
call to another site on the same server goes that way. A service on the
host that containers must not reach listens on `127.0.0.1` only.

The rules are in four chains devopsy owns, `DEVOPSY-PROJECTS`,
`DEVOPSY-DOCKERD` and `DEVOPSY-DOOR` in iptables' filter table and
`DEVOPSY-CONTAINERS` in
its raw table, and the server's other rules stay as they are.
`devopsy-firewall.service` loads them at every boot, and from the next
boot on `devopsy-firewall.timer` loads them every five minutes too, so
a reload of the server's own firewall that drops them is undone within
five minutes. Syncs between servers work with the firewall loaded.

`--server-info` checks the firewall and warns when:

- a chain is not devopsy's, nothing jumps to it, or a rule ahead of
  devopsy's jump lets packets through, which `--project-users add`
  fixes by loading the chains again;
- Docker's daemon is in no cgroup of its own, so nothing covers it;
- a TCP listener is open beyond loopback, other than sshd's and the
  ports Docker publishes, since containers reach it.

## What a compose file may hold

A project user's compose file is an ordinary one, within these rules.
The door names what it refuses, and logs each refusal in
`<space>/refused.log`.

- Every service runs as `<uid>:<gid>` in numbers, neither of them 0, an
  id of the host's own users and groups (the deploy user, the project
  users, the socket's group), nor in 50000 to 59999, the range project
  users take their ids from: `user: "10001:10001"`. A user name is
  refused, since the image's own `/etc/passwd` could map it to root.
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
- A start never pulls or builds: `pull_policy` is `missing`, `never` or
  `if_not_present`. devopsy's releases build and pull before they start.
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

A server needs `sudo` and `acl` installed, and the door's directory on a
mount of its own. The commands below address the server by its deploy
user, `devopsy@vm1`.

1. Make the door's directory, as root, and mount it on itself with
   `nosuid` and `nodev`, in `/etc/fstab`:

   ```sh
   install -d -o devopsy -m 755 /var/lib/devopsy-door
   echo '/var/lib/devopsy-door /var/lib/devopsy-door none bind,nosuid,nodev 0 0' >>/etc/fstab
   mount /var/lib/devopsy-door
   ```

2. Name it in the deploy user's config, `~/.config/devopsy/config.yaml`,
   beside its [release settings](config.md#release-settings):

   ```yaml
   door:
     space: /var/lib/devopsy-door
   ```

3. Add the project user and its keys, locally:

   ```sh
   devopsy @devopsy@vm1: --project-users add shop
   devopsy @devopsy@vm1: --ssh-keys --for shop add ~/.ssh/id_ed25519.pub
   ```

4. Reboot the server after the first `add`. sshd takes devopsy's part of
   its configuration at the next boot, and until then no project user
   logs in; `--project-users` says so.

`add` makes `devopsy-shop` with no password, a home closed to others,
and ids from 50000 to 59999, which no container of the door's runs as,
so a project user made later never shares a running container's uid.
The deploy user's own containers stay out of that range too.
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
there, and how many keys each has. `--server-info` also checks that
`<space>` is mounted `nosuid,nodev`, and whether sshd waits for a
reboot.

## Releasing as a project user

The project's address names its user, in `.devopsy/.env` or anywhere
else an address goes ([Targets](config.md#targets)):

```sh
DEVOPSY_SERVER=devopsy-shop@vm1
```

Everything else is as it is with a deploy user: `devopsy @prod
--release`, `--rollback`, compose commands, `--export` and `--import`,
snapshots, a child's sync, all go through the door. A sync goes between
two project users or two deploy users, never one of each, so a child on
another server needs its project user there too ([Syncs](syncs.md#how-the-data-travels)).

On the server, the project user's `docker compose` and `docker` work
for what is its own: its containers, images, volumes and networks.

On a server with project users, leave the deploy user's own builds
under the name compose gives them by default, from their compose
project, rather than an `image:` a registry serves: a project user's
pull of that name would replace the build with the registry's image.
An image with no registry host is a name on Docker Hub, so the door
pulls it like any other, even with `pull_policy: never`.

## Registries

A project user's `docker login` goes through the door: Docker's daemon
checks the login against the registry as it does for a pull, and the
project user's own devopsy then keeps it in its own docker config
(`~/.docker/config.json`, or under `DOCKER_CONFIG`). The deploy user
keeps no password. The password comes from stdin (`--password-stdin`,
with `--username`) or a prompt, never the command line, where every
user of the server would see it.

The door pulls with those logins alone. A registry at the server's
loopback, link-local or a metadata address is refused, for a login and
a pull alike, since Docker's daemon would contact it as root. A registry
on the same server is reached by its name or the address of Docker's
bridge. Since the firewall keeps Docker's daemon off loopback, the
deploy user's own pulls and logins reach no registry there either, a
mirror included: put one on a build server, or reach it by its name or
Docker's bridge address.

A registry with a CA of its own, or a mirror, needs each client that
reaches it set up, or no project user can use its images:

- Docker's daemon: `certs.d`, `insecure-registries` or a mirror, as for
  any pull;
- the builders: `<space>/buildx/buildkitd.default.toml`, read when a
  project's builder is made;
- the server's CA store (`/etc/ssl/certs/ca-certificates.crt` on Debian
  and Ubuntu), which the door trusts when it reads an image's manifest.

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

`--project-users remove shop` drops its entry and keys, so it can no
longer log in or use the door, then ends every process it runs and
removes its builder and the builder's network:

```sh
devopsy @devopsy@vm1: --project-users remove shop
```

Its Unix user, home and tree stay. So do its environments' containers
and volumes, which `--destroy` takes first.

Removing a project user clears nothing a leaked key left in its home:
delete and recreate the home too.

## What it costs

- A server with project users needs more than Docker, devopsy and a few
  tools: sudo, ACLs, a mount of its own, and the firewall.
- The door rewrites the file compose runs (images pinned, binds moved
  into `<space>`, builds renamed), so `devopsy -- config` and a plain
  `docker compose` on the server do not show exactly what runs.
- The first use of each image costs one more download, by the project's
  builder.
- A registry or mirror on the server's loopback is out of reach of the
  deploy user's pulls too.
- Every Docker call or compose key devopsy adds needs a rule in the
  door before project users can use it.
