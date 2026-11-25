# Servers

Commands with a target run on its server, over SSH. The ones a project
uses most:

```sh
devopsy @vm1:prod --release      # upload, switch, run the release steps
devopsy @vm1:prod --rollback     # back to the previous release, run the rollback steps; the one left is marked rolled back
devopsy @vm1:prod --reload       # the release again with its variables and imports as they are now, then its reload: step
devopsy @vm1:prod --releases     # list them and their images (version or short digest), * marks the current one; --borders in a box, --no-borders without
devopsy @vm1:prod --releases remove failed   # remove releases (ids or failed) and the images only they hold
devopsy @vm1:prod --log          # what the last release or rollback printed (or --log <id>)
devopsy @vm1:pr-12 --destroy     # take it down and remove the environment, data included
devopsy @vm1:develop --sync     # replace a child's data with its parent's, run its run step
devopsy @vm1:develop --reset    # start a child over: all its data goes, then a sync as on its first release
devopsy @vm1:develop --release-sync       # a release and the parent's data in one go (--release-no-sync: never)
devopsy @vm1:prod --export database > db.sql   # a service's data, as its devopsy.data.copy label says
devopsy @vm1:prod --export database | devopsy --import database   # prod's database into the local environment
devopsy @vm1:prod --import database --resolved   # accept the data an import that failed left, start what it kept down
devopsy @vm1:prod --snapshot     # a snapshot: every service with data stopped for the moment of its copy, code and images kept
devopsy @vm1:prod --snapshots    # list them; restore, remove, export or import one
devopsy @vm1:prod --snapshots restore <id>   # its data, code and images back, as a new release (--no-code: the data alone)
devopsy @vm1:prod --snapshots export <id> | aws s3 cp - s3://backups/prod.tar   # a snapshot off the server
devopsy @vm1:prod --status       # the release there, a sync running into or from it, the disks, its containers, images held or moved, the project's own lines; exit 3 when something needs attention, --json for fields, --borders in a box
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
`--server-setup`, `--gc`, `--reboot`, `--environments`, `--vars-project`,
`--upgrade` and `--match-server-version` (`--environments` and `--vars-project` with the checkout's
project, or an alias's).

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
devopsy @vm1 --match-server-version   # this devopsy, to the server's version
devopsy @vm1 --server-info       # what the server is, and what needs attention
devopsy @devopsy@vm1: --server-setup   # the server set up as devopsy wants it
devopsy @vm1 --gc --dry-run      # what nothing on the server owns, listed
devopsy @vm1 --reboot            # the server rebooted, its hostname typed
```

## Docker

Every server runs Docker from Docker's repository with its containerd
image store, buildx 0.33.0 or later and compose 2.33.0 or later: the
least devopsy needs of Docker, which these docs call its floor.
`--server-setup`, every release before its upload, `--project-users
setup` and `--project-users add` refuse a Docker below any of them, each
with its fix, and `--server-info` warns of it. A release reads Docker for
it only until `--server-setup` or `--upgrade` of the same devopsy
release found Docker at its floor, which its record notes; a development
build notes nothing and trusts no note, and a Docker that does not
answer takes the note off. A Docker put back below the floor since is
seen by `--server-info`, which always reads Docker, and by releases from
the next devopsy on. A Docker upgraded from an older version keeps the
classic image store, and a distribution's own package may run it too,
which devopsy does not support: such a server's projects go to a new
server whose Docker runs the containerd image store
([`docker`](server-setup-howto.md#docker)).

## Behind an HTTP proxy

A server that reaches the Internet only through an HTTP proxy needs it
in four places: apt, Docker's daemon, the deploy user's Docker client and
the login environment, each with loopback left direct
([`proxy`](server-setup-howto.md#proxy) has the steps). The deploy user's
client gives every container that user creates the proxy, as
`HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`, each in both cases, and most
HTTP clients follow them for every request, a container's requests to
another container included. The server's list names no project's
services: a project's own containers say what they reach directly.

- A container that makes an HTTP request to another container names it in
  its own `NO_PROXY` and `no_proxy` (for example `NO_PROXY: app` on a
  service that calls `app:8080`). Set in `environment:`, they replace the
  server's list, which is enough when the container calls nothing else
  that must bypass the proxy; otherwise its entrypoint appends to the list
  Docker gave it (for example `NO_PROXY="${NO_PROXY:+$NO_PROXY,}app"`
  before it runs its command).
- Both cases: Go reads `NO_PROXY` first today and `no_proxy` first in its
  coming releases.
- A name matches that host and its subdomains, never a shorter name, and
  an address range (`172.16.0.0/12`) matches only a request made to an
  address.
- Connections that are not HTTP (a database, a cache, SMTP) never use the
  proxy and need nothing.
- A check on a container's own loopback skips the proxy explicitly:
  `curl` sends `localhost` there unless the list names it
  (`curl --noproxy '*'`), and busybox's `wget` ignores `no_proxy`
  altogether (`wget -Y off`).

The server's own list still names loopback (`localhost,127.0.0.1,::1`):
an app's request to itself through curl or libcurl (PHP's, for one) goes
to the proxy otherwise, and no project can change that for its app's
code. `--server-info` shows the daemon's proxy and the containers' under
`docker`, warns when one is set without the other, and notes a
containers' `noProxy` that leaves loopback out. Containers created after
a change get it: a release recreates none for it, `up -d
--force-recreate` does.

## `--server-setup`

`devopsy @devopsy@vm1: --server-setup` sets up what devopsy needs of a
server, as its deploy user, the server named with that user. Steps as
root run through devopsy's util image, so the deploy user needs no sudo:
the docker group can do as much already. It needs systemd, and checks
first, refusing with what is missing, that the server has `tar` and
`flock` and Docker answering with its compose and buildx plugins as the
host's `docker.service`, at its floor ([Docker](#docker)), and warns of
the other tools a server needs when missing (`setsid`, `script`, `mkfifo`, a `tail` that answers
`--version`, and on a server with project users `sudo`, `visudo` and
`setfacl`). It installs none of them: they are the admin's
([`--server-setup-howto`](#--server-setup-howto)). The release root
(`releases: root` in the deploy user's `config.yaml`, else its home,
never `/`, a link to it included) it makes when missing, and gives to
the deploy user when root owns it and it is empty (a fresh volume's
`lost+found` aside); one root owns that holds files (a system
directory), one another user owns, or one that is no directory, it
refuses.

A refusal stops only what needs what it refuses, and `--server-setup`
applies the rest, says what it did not apply and why, and exits 1:

- Docker not answering, not this host's (rootless, or another host's
  through `DOCKER_HOST`), under another init than systemd, or its unit
  inactive: every item, each run as root through Docker;
- `docker.service` starting Docker with another `--config-file`, a
  `daemon.json` devopsy cannot judge, or a record it cannot read:
  `daemon.json`'s parts and a restart of Docker;
- the deploy user's record naming another user than the one it runs
  as, or one it cannot read ([The deploy user](#the-deploy-user)):
  every item;
- a release root refused (another user's, a file, `/`): its making and
  the mounts drop-in that names it; one a unit file cannot hold: the
  drop-in alone;
- a missing `flock`, which the firewall's loader takes its lock with:
  the metadata firewall;
- Docker without its compose or buildx plugin, below its floor, or a
  missing `tar`, which releases need: nothing of the setup, said all the
  same.

It shows the differences and the refusals together, each refusal once,
and its question covers what it will apply, naming what it will not. A
put that fails, by its check or its write, ends its root run there, with
what came before it put; `daemon.json` goes last of the puts, and what
follows the run (the firewall) still goes. A restart not confirmed in
time leaves the firewall to go when Docker answers, and names it as not
applied when Docker does not. A step a refusal stops whose change it
cannot tell (beside a `daemon.json` it cannot judge, or a root a unit
file cannot hold) counts as not applied, and `--upgrade` says what it
did not apply too, as a warning.

What it keeps as devopsy wants it:

| Item | devopsy's value |
|---|---|
| The deploy user | `/etc/devopsy/deploy-user`, root's, naming the user `--server-setup` runs as ([The deploy user](#the-deploy-user)); as root, none is written |
| The release root | made when missing, given to the deploy user when root owns it and it is empty |
| `daemon.json`'s logs | `"log-driver": "local"`, `"log-opts": {"max-size": "20m", "max-file": "5"}` |
| `daemon.json`'s `live-restore` | `true` |
| `daemon.json`'s `default-address-pools` | `172.16.0.0/12` and `192.168.0.0/16`, size 24 |
| Socket buffers | `/etc/sysctl.d/90-devopsy-net.conf`, `net.core.rmem_max` and `wmem_max` at 7500000; a live value lower is raised, a higher one never lowered by a run (it sets 7500000 at each boot: a higher value goes in a sysctl file sorting after it). A container gets what the host allows here, and QUIC asks for more than the kernel's default: a Cloudflare Tunnel's connector warns and runs on smaller buffers without it. It changes nothing on a server with no such service |
| Mounts drop-in | `/etc/systemd/system/docker.service.d/devopsy-mounts.conf`, `RequiresMountsFor=` the release root, and the project users' directory where `door: space:` names one and it is mounted with `nosuid` and `nodev`, so Docker waits at boot for the volumes they are on; putting it asks for no restart |
| Metadata firewall | packets from containers on Docker's bridges (`docker0`, `br-*`) to the cloud's metadata services (`169.254.0.0/16` and the providers' others) dropped, DNS there let through; with `door:` in the server's config, or the door's firewall loaded already, the door's chains too ([Project users](project-users.md#the-firewall)); by `/usr/local/libexec/devopsy-firewall`, loaded at boot (`devopsy-firewall.service`), by `--server-setup` and `--project-users add`, and with the door's at every start of Docker |

What a server with project users needs beyond these is `--project-users
setup`'s, which neither `--server-setup` nor `--upgrade` runs ([Project
users](project-users.md#--project-users-setup)).

It shows what differs from devopsy's setup, then asks. `daemon.json` is
merged: every other key stays as it is (a proxy's `proxies`, for
example), and the file keeps its mode and owner, a link to it staying a
link. A part someone else set is asked about, keep (Enter) or replace; a
value kept is not asked again while it stays the same. To go back to
devopsy's value, write it in `daemon.json` or remove the keys and answer
replace. A part one of whose keys `docker.service`'s own flags set is
left as it is and said, never asked about: putting it in the file too
would keep Docker from starting. If `docker.service` starts Docker with
another `--config-file`, `--server-setup` refuses. A `daemon.json`
Docker would refuse (not a JSON object, a value of the wrong type) is
refused, and never rewritten.

Docker takes `daemon.json` when it restarts, so `--server-setup` asks to
restart it now when the run changes the file or Docker has not taken it
yet, and restarts it only if that is still so once it holds its lock. A
restart cuts every docker command under way, and stops every container
unless Docker keeps them (`live-restore`). The question names the
projects there, the deploy user's and the project users', whose releases
are not waited for: one under way is cut. The restart is refused while a
release runs in one of the deploy user's environments, or while
`dockerd --validate`, given `docker.service`'s own flags, refuses
`daemon.json` (Docker would not start again), and
waits up to two minutes for Docker to come back (it clears the unit's
start counter first, so recent starts by hand do not count against it,
and a start systemd refuses ends the wait at once, with the commands
that start Docker again): not back by then is an error, `/run/reboot-required` raised for a reboot that takes the file
when Docker still answers, and said when it does not. Not restarted now,
Docker takes the file at its next restart, and `/run/reboot-required` is
raised for a reboot that takes it (devopsy-server's
`DEVOPSY_AUTO_REBOOT_TIME`): nothing is left to do, and a later
`--server-setup` offers the restart again for sooner.

The metadata firewall keeps an app in a container from reading its
cloud instance's credentials, and its first load can cut an app that read
them there off from them: `--server-setup` says so among what it shows.
It covers containers on Docker's bridges, the default one (`docker0`) and
the ones compose makes (`br-*`); a container on the host's network, on a
macvlan or ipvlan network, or on a bridge named otherwise is not
covered. `door:` in the deploy user's `config.yaml` loads the door's
chains with it, and a server whose loader is already the door's keeps
it, `door:` gone or not: devopsy never takes a door's firewall off a
server. `firewall: metadata: allow` there turns the firewall off for the
server, removed by the next `--server-setup`; it is refused where
`door:` is set or the door's firewall is loaded. What `--server-setup`
and `--server-info` show of it comes from its files, its boot link, the
mark the last load that went in leaves in `/run` (none after a load
failed), and, wherever devopsy's firewall is loaded, its chains read as
root through devopsy's util image, since the deploy user cannot read
iptables. Nothing loads it again between a boot and the next install, so
a reload of the server's own firewall or an `iptables -F`, which drops
it, shows there as the firewall not loaded as devopsy loads it, and the
next `--server-setup` loads it again; a release says it too, by its code
`W0001` ([Codes](codes.md)). The apply loads it through
`devopsy-firewall.service`, the loader run directly only where systemd
does not answer, then reads the chains back as root. One load runs at a
time, by a lock in a directory of root's that no other user enters.

`--yes` asks nothing: it keeps every value someone else set, restarts
Docker only when no container runs as it applies, and loads the
metadata firewall (or removes it, for `allow`), so a new server set up
in one command gets devopsy's address pools before its first release
makes networks, and the firewall. Without a terminal it needs `--yes`.

While the deploy user's `config.yaml` names no `releases: root`, the
home holds nothing of devopsy's (no project, no variables set before a
first release, no claim of the uids containers run as, no project user)
and a volume is offered, `--server-setup` asks where releases live: the
home, first, or one of the volumes the root could be on, listed with
their size and free space, and each volume it does not offer with why. A
volume is offered only where something mounts it at boot (`/etc/fstab`,
a `.mount` unit): one mounted by hand or by `systemd-mount` is gone
after a reboot, and releases would land on the disk under it. A volume
chosen is named as `releases: root` in that `config.yaml`, added to its
`releases:` or appended as a new one, the rest of the file left as it
is, then the root is made there and Docker waits for its mount; once
named, nothing is asked. The home chosen writes nothing, and the next
run asks again.

`--root <dir>` gives the answer with no question, for `--yes` and CI:
any directory (for example `/mnt/vol1/releases`); naming the root
releases live in already, it does nothing. It is refused before anything
changes while `config.yaml` names a `releases: root` or the home holds
something of devopsy's, where the release root's check refuses it, on a
filesystem nothing mounts at boot, under a mount point mounted at boot
and not mounted now, or in memory (`tmpfs`, `ramfs`), which keeps
nothing across a reboot. A `config.yaml` devopsy cannot add the line to
(a `releases:` written as a flow mapping, a `root:` left empty) refuses
every volume, each with the setting to make by hand. With `--yes`, or
without a terminal, and no `--root`, or with no volume offered, nothing
is asked and the home is kept: the volumes are listed instead, each
offered one with the `--server-setup --root` that names it, each other
with why it is not offered and what to do.

A second run changes nothing. What `--server-setup` applied, the values
kept, the devopsy that last applied with no failure, and the one that
last found Docker at its floor ([Docker](#docker)) are recorded in
`server.yaml` beside the deploy user's `config.yaml`: devopsy's record,
never edited by hand. Its first run writes it. A run whose only change
is that note writes it and says there is nothing to change.

### The deploy user

A server has one deploy user, which `--server-setup` records in
`/etc/devopsy/deploy-user` as the user it runs as, its plan naming that
user before it asks. From then on devopsy refuses any login other than
that user, root or a project user, before it reads or changes anything,
`--shell-host` included. The refusal is `W0009` ([Codes](codes.md#w0009))
and gives the deploy user's address, offering the command that was
refused with it where the address names the server (an alias's gets
`--server-info` on the deploy user's address). Two things are not
checked: `--shell-host --root`, root's own shell, and
`--match-server-version`, which only reads the version.

```
devopsy: error: W0009: this login is not the server's deploy user
devopsy: | logged in as admin, but this server's deploy user is devopsy (/etc/devopsy/deploy-user)
devopsy: | devopsy works on a server as one user only, so that every release, uid claim and setup there stays in one place
devopsy: | to fix it:
devopsy: | run it as devopsy:
devopsy: | $ devopsy @devopsy@vm1: --upgrade
devopsy: | to make admin the deploy user instead, follow the steps this prints:
devopsy: | $ devopsy --server-setup-howto deploy-user
```

Two users in the `docker` group, each taken for the deploy user (an
admin's own account the default SSH user, say), would each have a
release root, uid claims and a setup of their own on one Docker, whose
compose names they share. `--server-setup` run as another user refuses
too, and so does one on a server with no record yet that shows another
deploy user: another member of the `docker` group, root aside, with
devopsy's setup record in its home (`~/.config/devopsy/server.yaml`), or
uid claims or an environment of devopsy's under its release root (its
`releases: root`, else its home), the server's devopsy as its own (the
binary `--upgrade` replaces, which devopsy-server installs as the deploy
user's), or a release root another user owns. A home this login cannot
read (mode 700, as Debian and Ubuntu make them) is read as root in the
plan, which builds devopsy's util image for it; where that fails, the
setup refuses rather than record this login on what it could not see. It
refuses before anything changes, in one of three ways:

- where that user's setup record is all it holds (an account set up by
  mistake), two fixes: run it as that user, or remove that record as
  root (`sudo rm`, its path named), after which this login is recorded;
  where owning the server's devopsy is all, the same two, the second
  giving this login the binary's directory as root (`sudo chown -R`);
- where that user holds environments or uid claims and this login none:
  run it as that user, or change the deploy user by hand;
- where both hold environments or uid claims: an error saying so, with
  only the steps to settle it by hand, since no command can choose.

`--upgrade` refuses the same way, as a warning. The record never changes
by itself: to make another user the deploy user, follow the steps
`devopsy --server-setup-howto deploy-user` prints. A record that cannot be read,
or names no one user or a user the server does not have, refuses every
login but root and project users
with `W0010` until root corrects it, in root's own shell
(`--shell-host --root`). A server set up before the record
gets it at its next `--server-setup` or `--upgrade`, and until then
`--server-info` says it is not recorded yet, naming the user the next
`--server-setup` would record, and nothing is refused. On a server never
set up, `--server-info` says the same: the first `--server-setup` records
the login it runs as. Once recorded, `--server-info` names the deploy
user under `setup`.

## `--server-setup-howto`

What devopsy checks on a server and does not set up itself (Docker, the
tools, `/proc`'s `hidepid`, what project users need, the kernel's
command line, the data on XFS) is the server admin's, as root. `devopsy
--server-setup-howto` lists the topics, and `devopsy
--server-setup-howto <topic>` prints one's steps and how to check them,
the command every such fix offers; it runs anywhere, needing no server
or project. `--all` in place of a topic prints every topic with a table
of contents, which is the [Server setup how-to](server-setup-howto.md)
page. The text is Markdown that reads as it is, so a renderer shows it
too (for example `devopsy --server-setup-howto docker | glow -`). devopsy-server's `setup.sh` is one way to do these steps on Debian
13 and Ubuntu 26.04.

## `--server-info`

`--server-info` reads the server in one session that changes nothing and
waits for no lock. Without a target it reads the Linux server it runs on,
in `--shell-host`; anywhere else it refuses. It reports in four parts.

`server`, the machine:

- the system, with the memory the kernel keeps for Kexec HandOver and
  kdump, and whether its clock is synchronized and how far it is from the
  operator's;
- Docker: its version, its compose's and buildx's, its image store,
  security options, logs and where it keeps its data;
- the filesystems of the release root, Docker's and containerd's data,
  the snapshot root and `/`, one row each: where it is mounted, its type,
  free space, how full it is, whether the release root makes reflinks,
  and what lives there.

`devopsy`, its install on the server:

- its version and the release settings;
- its setup as `--server-setup` judges it: whether devopsy set it up,
  where its record is, the values someone else set that were kept, and
  whether `/proc` shows every user's processes.

`projects`, each project under its name with the project user it is
for, if any ([project users](project-users.md)), and its project uid;
each of its instances a level under it with the instance's project uid;
then, under the project or its instance, a row per environment: when its
current release was made and by whom, its containers running, the uid
its containers run as ([uids](#uids)) and what runs on it, with the role
it holds and what it exports under the row. A role held by a compose
project that is no environment is a line of its own. A project user's
`--server-info` shows its own project only.

`leftovers`, what may be dead or gone on the server, described below.

Each part's title is underlined, a blank line before and after it. A key
of several lines, a project and a warning get a blank line around them,
a warning about a row (a disk's, an environment's) a blank line after it
only, and several warnings in a row stay together. A fact that names
what it is says it first (`clock: synchronized`, `logs: json-file`).
Prose wraps at 100 columns, a command never, and a row of cells moves
its last cell whole to a line of its own instead.

What needs attention is a warning under its key, after the key's facts,
or under the row it is about, its first line starting with `warning: `, its details and its fix after a `| `, as
devopsy's own lines show them; a fix that serves several lines in a row
(`--server-setup` for the setup's items) comes once, after the last of
them:

- an older devopsy (`W0003`), with `--upgrade` or
  `--match-server-version` as its fix, or an older one on the operator's
  machine, which reads what it does not know of the server's facts as
  missing, with `--match-server-version`;
- a reboot the system asks for (`/run/reboot-required`, with the
  packages asking, devopsy among them when its setup or its door did,
  `W0004`);
- memory Kexec HandOver keeps as CMA. Only movable pages may use it, so
  the kernel's allocations can run out while memory is free. `kho=off`
  frees it ([`kexec-handover`](server-setup-howto.md#kexec-handover),
  `W0006`);
- a clock out of sync (`W0005`);
- Docker not answering, without its compose or buildx plugin, or below
  its floor ([Docker](#docker)), said under Docker and not again under
  the setup;
- a kernel with AppArmor and no `apparmor_parser`, on which no container
  starts;
- Docker logs that grow without a limit, on a server whose devopsy does
  not judge its setup yet;
- a server never set up by devopsy, on which `--upgrade` applies nothing,
  and each item of the setup that differs from devopsy's (a release root
  releases refuse, a part of `daemon.json`, the socket buffers, the
  mounts drop-in, the metadata firewall, `W0001`, the deploy user's
  record changed; one not written yet is a line, not a warning), each fixed by
  `--server-setup`, or, where the setup refuses every step, by what it
  refuses first;
- Docker needing a restart to take `daemon.json` (the parts, where
  `docker info` shows them), while nothing is to restart it: it waits
  for Docker's next restart, whoever makes it. Once a reboot is asked for,
  the line says the reboot does it, and is no warning: nothing is lost by
  waiting, and the reboot's own line is the one that needs attention.
  Either way `--server-setup` is offered to restart Docker sooner;
- a disk 90% full, or with less than 5 GB free, Docker's with
  [`--gc`](#--gc) as its fix;
- no reflinks;
- a snapshot root on the same filesystem as the release root, which
  usually means the backup volume is not mounted (`W0007`);
- a tool the server needs, missing, and on a server with project users
  a missing `sudo`, `visudo` or `setfacl` (with none yet it is a line,
  not a warning), `W0008`;
- on a server with project users, a `/proc` that shows them every other
  user's processes, whose command lines can hold a secret, or that hides
  them another way `--project-users setup` refuses ([setting up a
  server](project-users.md#setting-up-a-server) has the mount that hides
  them; with no project user it is a line, not a warning);
- a role held twice;
- an environment with a failed first release, an activation cut off past
  its switch, a sync, an import or a restore that did not complete, an
  import or a restore under way (what it stopped stays down until it
  completes), services an operation left stopped, or containers
  restarting, paused, dead, unhealthy, or exited on an error of their
  own (a stop's codes excepted), each as `--status` says it;
- an entry of the claims directory that is no claim, with its removal by
  hand.

A warning only the server's admin can fix carries its
[code](codes.md). A project user's `--server-info` says it with the
code to pass on and no fix, and leaves out what only the admin can do
(the hint to free what kdump keeps, data on XFS).

`--borders` draws each part in a box under its title, its keys, projects,
instances and environments between rules, a project's rules open in the
key column, every box's columns at one width;
`borders: true` in your user config makes boxes the default at a
terminal, and `--no-borders` leaves them out
([Configuration](config.md#boxes-by-default)). A box
widens for a command it cannot wrap up to the terminal's width (100
columns in a pipe); a longer command runs out past the box's side, on its
line alone. `--json` prints the facts and
the same lines for scripts and agents, never with `--borders` or
`--no-borders`, each
warning with its own fix, each line with its `section`, its project
(`group`) and instance (`instance`), a line of a project's heading
(`head`), a row's `cells`, and the name of what a row is of when its key
does not say it (`name`); the facts say whose report it is when a
project user's (`project_user`). Its
fields may change before 1.0.0: the door's `firewall_wrong`,
`firewall_error` and `project` are gone, the setup's `firewall` (its
`state`, `differs`, `wrong` and `unread`) saying the firewall for both
kinds.

It also shows, as information with a command to see to it, what may be
dead or gone. devopsy removes none of it, and none of it makes the exit 3.
What belongs to a project is shown in it, under the environment's name;
a name of no project on the server is a project of its own:

- what is left of an environment removed by hand, for a compose name
  with no environment directory: its uid claim, its releases' image
  names and a project user's builds, its containers, its volumes, its
  snapshots under the server's `snapshots: root`, with the commands that
  remove them (`docker compose -p <name> down`, `docker volume rm`,
  `docker rmi`, `rm -rf`, the claim's `rm` last). A claim left harms
  nothing: its uid stays reserved, and removing it while anything of it
  is left could hand those files to another environment;
- a directory a `--destroy` cut off while it removed an environment left
  (`<environment>.rm.<pid>`), with its size, and the `--destroy` that
  resumes it;
- a project user with no key whose project still holds anything its
  destroy removes (uid claims, containers, volumes, data, images), with
  `--project-users remove <project> --destroy` as the hint, or a key
  added to take it back ([project users](project-users.md));
- a project user whose `--project-users remove <project> --destroy` did
  not complete, with that command, which completes it, as the hint, and
  not as a project user with no key.

The rest is the server's, under `leftovers`:

- what [`--gc`](#--gc) would remove, counted (one-off containers,
  release snapshots, image names a project's record left, old util
  images, dangling images), with `--gc --dry-run` as its hint for the
  sizes and the build cache, which only `--gc` reads;
- images with a tag that no release holds and no container uses (a
  tool's tag a `--vars set` changed since its release, a tool a profile
  builds, the admin's own), one row each with its size, then one
  `docker rmi` of all of them as the hint; a release holds a name its
  tool runs by, whatever image that name is on now;
- anonymous volumes no container uses (an image's `VOLUME` after a
  `down` without `-v`, which may be a database's data), `docker volume
  prune` as the hint, once checked.

What goes by an environment's absence is shown only when the release
root lists environments and every one of them, and the uid claims, could
be read: with the root's disk not mounted, every environment would look
gone. A uid claim whose environment is gone is said all the same, alone,
with why nothing else of it could be looked for. A claim is "only" what
is left only when its containers, images and volumes all read. An
environment's claim with no row of its own (a project user's tree that
answered nothing, an environment that did not read) is said under its
project, so that every claim shows somewhere. Images and volumes of a compose project that is not a release's
(the admin's own) are not shown. What of it could not be read (the
images, the volumes, a part of `--gc`'s count) is a warning instead:
unknown, never none.

A sync or a snapshot under way is reported as information. So are the
volumes the release root could be on, while `--server-setup` may still
name another root, and the memory kdump keeps, with how to free it
([`kdump`](server-setup-howto.md#kdump)), since crash dumps are a choice.

It exits 3 when it warned, so CI can run it as a check: on a server never
set up by `--server-setup`, it always does. `--json` prints the facts and
the same lines as JSON on stdout, each with its fixes and hints
(`remedies`), for scripts and agents.

## `--gc`

A release prunes what it owns and `--destroy` what its environment owns.
`devopsy @vm1 --gc` removes, as the server's deploy user, what falls
between and would otherwise stay: only what a record of devopsy's or
Docker's own rule says can go, never something because its owner looks
gone. In this order:

- one-off containers (`compose run` and the like) that exited longer
  ago than its `containers` age, of an environment of the deploy user's
  or one under a project user's names. Their volumes stay: a database's
  `compose run` keeps its data in an anonymous volume. One only created,
  never started, stays too;
- build cache unused for its `build_cache` age, on the server's Docker
  and on each project user's builder, as `docker buildx prune -a
  --filter until=` decides it. A project user's builder is stopped
  between builds: `--gc`, its `--dry-run` included, starts each in
  turn to read or prune it, and stops it again before the next, first
  giving it back its network when a prune took it (its container made
  again, its cache kept);
- release snapshots whose release is gone, as `--snapshots remove`
  prunes them, for every environment of the deploy user's. An
  environment whose snapshots are in use (a snapshot taken, a restore, a
  removal) is skipped: whatever holds them prunes them;
- the names in the deploy user's projects' records of names left that
  can go now, as a release's pruning retries them (`--releases` lists a
  project's record), then the tags of `devopsy.internal/util` other than
  the one this devopsy uses that no container uses (an older devopsy's
  util image), by name and without `-f`, then the dangling images no
  container uses, as `docker image prune` finds them (never `-a`).

It never removes a tagged image no release holds, a volume, a release, an
environment, a project user's snapshots, the containers of a service, or
anything of a release root that is not mounted, which lists nothing.
[`--server-info`](#--server-info) shows what is left to look at, and the
last line says so.

It first lists what it would remove with sizes, in a session that changes
nothing, then asks. A second session removes only what was listed,
deciding each removal again as it goes, so a run cut off leaves only less
to remove: what came to qualify between the list and the answer (a
container that crossed its age, a release snapshot, build cache last used
after the list's cutoff) stays for the next run. `--yes` removes what it
finds, without listing or asking; without a terminal and without `--yes`
it removes nothing. `--dry-run` lists only and asks nothing.

The ages are the deploy user's `gc:` in its `config.yaml`, with their
defaults ([Release settings](config.md#release-settings)).

It exits 3 when something is left to remove: with `--dry-run`, anything
it lists; otherwise what it skipped, could not remove or could not read.
0 says nothing is, so a timer or a monitor can run it.

```sh
devopsy @vm1 --gc --dry-run   # what it would remove, with sizes
devopsy @vm1 --gc             # the same, asked, then removed
devopsy @vm1 --gc --yes       # removed without asking
```

## `--reboot`

`devopsy @vm1 --reboot` reboots the server, as its deploy user, by
asking logind for it as root through devopsy's util image (`systemctl
reboot` where logind does not answer), then waits up to
10 minutes for the server to answer on a new boot (its boot id, in
`/proc/sys/kernel/random/boot_id`). The containers Docker restarts (a
restart policy) come back with Docker; [`--server-info`](#--server-info)
shows the server once it is back.

It first looks, in a session that changes nothing, and says how many
containers stop, what waits for a reboot (`/run/reboot-required` and its
packages) and the projects there, the deploy user's and the project
users', with what it cuts of theirs, then asks for the server's
hostname: the name proves the machine, whatever alias or address reached
it. A second session decides everything again before it asks logind. `--yes` reboots without looking
or asking; without a terminal and without `--yes` nothing is rebooted.

A reboot is the deploy user's: a project user is refused before
anything. It is refused, nothing rebooted and nothing waited for, while
a release or another change that holds an environment's lock runs in
one of the deploy user's environments, or a setup of the server runs.
Project users' releases are not waited for: one under way is cut. It is
refused too when Docker does not answer or is not this host's (rootless,
another host's), or systemd is not the host's init: a root shell
([Shells](#shells)) reboots it then. It is refused while something holds
off shutdowns (a block inhibitor, `systemd-inhibit --list`, for example
apt installing packages), which logind refuses root too. A delay
inhibitor holds the shutdown off while its holder finishes (for example
unattended-upgrades stopping an install cleanly), up to logind's
`InhibitDelayMaxSec`. Where the host's AppArmor rules the system bus
(Ubuntu), the util container runs without Docker's AppArmor profile for
this call alone, which that profile would refuse. The locks stay held
until the shutdown ends the session, so nothing starts in between.

It fails when logind refuses the reboot, when the server is still up 2
minutes after logind took it, and when the wait ends: on the same boot
the server did not reboot, and with no answer it did not come back.

```sh
devopsy @vm1 --reboot         # the hostname typed, then rebooted
devopsy @vm1 --reboot --yes   # rebooted without asking
```

## uids

Every environment's containers run as a uid of its own, and its
project's (or instance's) as another, which the server's devopsy gives
its compose files as `DEVOPSY_UID` and `DEVOPSY_PROJECT_UID`
([Variables on a server](config.md#variables-on-a-server)): a process
escaped from one container then reaches no other environment's through
`/proc`. They are between 1,900,000,000 and 2,099,999,999, a range no
system user, subordinate range or systemd container takes: an
environment's from 2,000,000,000, a project's below it. The range alone
says a uid's kind, which is how a copy gives data the receiving side's
uids ([Owners in copies](data.md#owners-in-copies)).

Each is a claim in `<release root>/.devopsy-uids/`, a symlink named for
its owner whose target is the uid (`env:shop:prod -> 2064483025`,
`project:shop -> 1911770400`), made the first time devopsy loads the
environment there and removed by its `--destroy`. A uid two owners would
share goes to the next free one, and stays its owner's for as long as
the claim does. Back the directory up with the release root: claimed
anew elsewhere, a uid can differ.

`--server-info` shows each with its project or environment, and one
left over with its environment gone as a line of its project. The deploy user
may remove a claim by hand (`rm`), once nothing of that environment is
left on the server: its next start would claim a new uid while its data
keeps the old one. The names hold `:`, which rsync and scp read as a
host: name a claim as `./env:shop:prod`, or copy the directory.

## The server's devopsy

Commands on a server run part of their work in the server's devopsy
(a release's images, for example), so it must be recent enough for
yours. An
older one stops a command that needs what it lacks before anything
changes, says what is missing, and says to run `devopsy @<target>
--upgrade`, which always works. Commands that need nothing new still run.
`devopsy @<server> --match-server-version` goes the other way: your
devopsy becomes the server's release ([CI](#ci)).
A sync across servers checks the child's devopsy against the parent's
the same way, before the parent stops anything.

On a server `--server-setup` set up (its `server.yaml` there), `--upgrade`
applies the setup once per devopsy version, when the record names
another devopsy than the one now there (just installed, or the latest
already, its last apply having failed), asking nothing: it puts
devopsy's values where they are devopsy's (a part new in this devopsy
included), leaves and says each value someone else set, and never
restarts Docker. It loads the metadata firewall again as devopsy loads
it, the kind loaded kept, and never makes its first load, adds the
door's chains to the containers' alone, or removes it for `allow`: each
of those is `--server-setup`'s, and `--upgrade` says so. When it changes
`daemon.json` it says Docker takes it at its next restart and raises
`/run/reboot-required`. What fails is a
warning naming `--server-setup`, and the next `--upgrade` applies again;
the upgrade itself has succeeded. On a server whose devopsy applied
already, `--upgrade` changes nothing there: what someone changed since
(a drop-in removed, a file edited) is `--server-setup`'s to put back,
and `--server-info` warns of it.

`DEVOPSY_FEATURE_CHECK=warn` turns that stop into a warning and goes on,
at your own risk. It is for a local build ahead of the servers, when what
you run may not need what the server's devopsy lacks.

## Shells

- `--shell` opens a shell in a container, as it does without a target
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

`install.sh` installs the latest release, and a new devopsy may read a
project differently than the one on the server. `--match-server-version`
after installing makes CI's devopsy the server's release, older or newer,
downloading nothing when they already match:

```sh
devopsy @vm1 --match-server-version
```

It refuses a server whose devopsy is a development build, which no
release matches.

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
- With a server, `devopsy @<server> --ssh-keys-generate [<name>]` makes
  the pair the same way, then adds its `.pub` there as `add` does, taking
  `--restricted` and `--for <project>` (the name then defaulting to the
  project user's, `devopsy-<project>`). The private key stays on the
  operator's machine. A pair whose add fails is removed, so the same
  command can run again.

For CI: generate a pair, `add` the `.pub` on each server with
`--restricted`, store the private key as a protected file variable, and
in the job run `devopsy --ssh-option IdentityFile="$DEVOPSY_SSH_KEY"
@prod --release`, or load it into an agent. To rotate it, generate another, add it, then
remove the old one.

For CI's known hosts file (`UserKnownHostsFile` in
`DEVOPSY_SSH_COMMAND`, [CI](#ci)), run `ssh-keyscan <server>` on the
operator's machine and check its fingerprints against the ones the server prints:

```sh
for key in /etc/ssh/ssh_host_*_key.pub; do ssh-keygen -lf "$key"; done
```

## Project users

A project can have a Unix user of its own with no access to Docker's
socket, so that its keys reach that project and not the server: see
[Project users](project-users.md), which covers the setup, the rules a
project's compose file follows there, and the firewall.
