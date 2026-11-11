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
`--server-setup`, `--environments`, `--vars-project` and `--upgrade` (`--environments` and
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
devopsy @devopsy@vm1: --server-setup   # the server set up as devopsy wants it
```

## `--server-setup`

`devopsy @devopsy@vm1: --server-setup` sets up what devopsy needs of a
server, as its deploy user, the server named with that user. Steps as
root run through devopsy's util image, so the deploy user needs no sudo:
the docker group can do as much already. It needs systemd, and checks
first, refusing with what is missing, that the server has `tar` and
`flock` and Docker answering with its compose and buildx plugins as the
host's `docker.service`. It installs none of them: devopsy-server's
`setup.sh` does. The release root (`releases: root` in the deploy user's
`config.yaml`, else its home, never `/`, a link to it included) it makes
when missing, and gives to the deploy user when root owns it and it is
empty (a fresh volume's `lost+found` aside); one root owns that holds
files (a system directory), one another user owns, or one that is no
directory, it refuses.

What it keeps as devopsy wants it:

| Item | devopsy's value |
|---|---|
| The release root | made when missing, given to the deploy user when root owns it and it is empty |
| `daemon.json`'s logs | `"log-driver": "local"`, `"log-opts": {"max-size": "20m", "max-file": "5"}` |
| `daemon.json`'s `live-restore` | `true` |
| `daemon.json`'s `default-address-pools` | `172.16.0.0/12` and `192.168.0.0/16`, size 24 |
| Socket buffers | `/etc/sysctl.d/90-devopsy-net.conf`, `net.core.rmem_max` and `wmem_max` at 7500000; a live value lower is raised, a higher one never lowered by a run (it sets 7500000 at each boot: a higher value goes in a sysctl file sorting after it) |
| Mounts drop-in | `/etc/systemd/system/docker.service.d/devopsy-mounts.conf`, `RequiresMountsFor=` the release root, so Docker waits at boot for the volume it is on |

It shows what is not devopsy's, then asks. `daemon.json` is merged: every
other key stays as it is (a proxy's `proxies`, for example), and the file
keeps its mode and owner, a link to it staying a link. A part someone else set is asked about, keep
(Enter) or replace; a value kept is not asked again while it stays the
same. To go back to devopsy's value, write it in `daemon.json` or remove
the keys and answer replace. A part one of whose keys `docker.service`'s
own flags set is left as it is and said, never asked about: putting it
in the file too would keep Docker from starting. If `docker.service`
starts Docker with another `--config-file`, `--server-setup` refuses. A `daemon.json` Docker
would refuse (not a JSON object, a value of the wrong type) is refused,
and never rewritten.

Docker takes `daemon.json` when it restarts, so `--server-setup` asks to
restart it now when the run changes the file or Docker has not taken it
yet, and restarts it only if that is still so once it holds its lock. A
restart cuts every docker command under way, and stops every container
unless Docker keeps them (`live-restore`); it is refused while a release
runs in one of the deploy user's environments, or while
`dockerd --validate`, given `docker.service`'s own flags, refuses
`daemon.json` (Docker would not start again), and
waits up to two minutes for Docker to come back (it clears the unit's
start counter first, so recent starts by hand do not count against it,
and a start systemd refuses ends the wait at once, with the commands
that start Docker again): not back by then is an error, `/run/reboot-required` raised for a reboot that takes the file
when Docker still answers, and said when it does not. Not restarted now,
Docker takes the file at its next restart, and `/run/reboot-required` is
raised for a reboot that takes it (devopsy-server's
`DEVOPSY_AUTO_REBOOT_TIME`).

`--yes` asks nothing: it keeps every value someone else set, and restarts
Docker only when no container runs as it applies, so a new server gets
devopsy's address pools before its first release makes networks. Without a
terminal it needs `--yes`.

While the release root holds no project and is not on a volume of its
own, it lists the volumes it could be on, with their size and free
space: `releases: root: <mount point>` in the deploy user's
`config.yaml`, before the first release, puts releases there.

A second run changes nothing. What `--server-setup` applied, the values
kept, and the devopsy that last applied with no failure are recorded in
`server.yaml` beside the deploy user's `config.yaml`: devopsy's record,
never edited by hand. Its first run writes it.

## `--server-info`

`--server-info` reads the server in one session that changes nothing and
waits for no lock. Without a target it reads the Linux server it runs on,
in `--shell-host`; anywhere else it refuses. It reports:

- its devopsy and release settings;
- the system, with the memory the kernel keeps for Kexec HandOver and
  kdump, and whether its clock is synchronized and how far it is from the
  local one;
- Docker: its image store, security options and logs;
- its setup as `--server-setup` judges it: whether devopsy set it up,
  where its record is, and the values someone else set that were kept;
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
- Docker logs that grow without a limit, on a server whose devopsy does
  not judge its setup yet;
- a server never set up by devopsy, on which `--upgrade` applies nothing,
  and each item of the setup that is not devopsy's (a release root
  releases refuse, a part of `daemon.json`, the socket buffers, the
  mounts drop-in) or that Docker has not taken yet, each fixed by
  `--server-setup`;
- a disk 90% full, or with less than 5 GB free;
- no reflinks;
- a snapshot root on the same filesystem as the release root, which
  usually means the backup volume is not mounted;
- a tool the server needs, missing;
- on a server with project users, a Docker without its containerd image
  store or buildx 0.33.0, on which the door refuses every image a project
  user does not build ([project users](project-users.md));
- a role held twice;
- an environment with a failed first release, an activation cut off past
  its switch, a sync, an import or a restore that did not complete, an
  import or a restore under way (what it stopped stays down until it
  completes), services an operation left stopped, or containers
  restarting, paused, dead, unhealthy, or exited on an error of their
  own (a stop's codes excepted), each as `--status` says it.

A sync or a snapshot under way is reported as information. So are the
volumes the release root could be on, while it holds no project and is
not on one, and the memory kdump keeps, with how to free it
(devopsy-server's `DEVOPSY_KDUMP=off`), since crash dumps are a choice.

It exits 3 when it warned, so CI can run it as a check: on a server never
set up by `--server-setup`, it always does. `--json` prints the facts and
the same lines as JSON on stdout, each with its fixes and hints
(`remedies`), for scripts and agents.

## The server's devopsy

Commands on a server run part of their work in the server's devopsy
(a release's images, for example), so it must be recent enough for
yours. An
older one stops a command that needs what it lacks before anything
changes, says what is missing, and says to run `devopsy @<target>
--upgrade`, which always works. Commands that need nothing new still run.
A sync across servers checks the child's devopsy against the parent's
the same way, before the parent stops anything.

On a server `--server-setup` set up (its `server.yaml` there), `--upgrade`
applies the setup once per devopsy version, when the record names
another devopsy than the one now there (just installed, or the latest
already, its last apply having failed), asking nothing: it puts
devopsy's values where they are devopsy's (a part new in this devopsy
included), leaves and says each value someone else set, and never
restarts Docker. When it changes `daemon.json` it says Docker takes it at
its next restart and raises `/run/reboot-required`. What fails is a
warning naming `--server-setup`, and the next `--upgrade` applies again;
the upgrade itself has succeeded. On a server whose devopsy applied
already, `--upgrade` changes nothing there: what someone changed since
(a drop-in removed, a file edited) is `--server-setup`'s to put back,
and `--server-info` warns of it.

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

## Project users

A project can have a Unix user of its own with no access to Docker's
socket, so that its keys reach that project and not the server: see
[Project users](project-users.md), which covers the setup, the rules a
project's compose file follows there, and the firewall.
