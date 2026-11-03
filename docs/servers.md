# Servers

Commands with a target run on its server, over SSH:

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
devopsy @vm1:prod --status       # the release there, a sync running into or from it, its containers, images held or moved, the project's own lines
devopsy @vm1:prod --env          # its variables there, env capability included, secrets masked
devopsy @vm1:prod --debug imports  # what devopsy sees there (capabilities, labels, imports)
devopsy @vm1:prod logs -f web    # any command, in the current release
devopsy --environments           # the project's environments on its servers: vm1:prod, vm1:b/prod...
devopsy @vm2 --environments      # on one server
```

`--environments` without a target asks each server `DEVOPSY_SERVER` and
the environments' `DEVOPSY_SERVER_<ENVIRONMENT>` name (the caller's
environment, then `.devopsy/.env`). It lists the project's layout only:
`<instance>/<environment>` with `instances: true`, else `<environment>`.

## Server commands

Server commands need only a server: `--shell-host`, `--ssh-keys`,
`--ssh-config`, `--server-info`, `--environments`, `--vars-project` and `--upgrade`
(`--environments` and `--vars-project` with the checkout's project, or an
alias's). They take `@<server>`
alone, as well as any target, whose server they use (no instance
needed). A bare name is still the project's environment or an alias
first, as for every target, else a server; `@<server>:` says it is a
server when a name is both. Every other command needs an environment.

```sh
devopsy @vm1 --ssh-keys          # vm1's deploy user's keys, from anywhere
devopsy @prod --ssh-keys         # prod's server's, in the project
devopsy @vm1 --upgrade           # the server's devopsy, as the deploy user
devopsy @vm1 --server-info       # what the server is, and what needs attention
```

## `--server-info`

`--server-info` reads the server in one session that changes nothing
and waits for no lock (without a target, the Linux server it runs on,
in `--shell-host`; elsewhere it refuses): its devopsy and release settings,
the system (with the memory the kernel keeps for Kexec HandOver and
kdump, and whether its clock is synchronized and how far it is from
the local one), Docker (image store, security options, logs), the filesystems of the
release root, Docker's and containerd's data, the snapshot root and `/`
(free space, whether the release root makes reflinks), the roles
projects hold and what they export, and every environment under the release root
with its current release, its containers and its state. What needs
attention is a warning with its fix: an older devopsy, a reboot the
system asks for, memory Kexec HandOver keeps as CMA (only movable
pages may use it, so the kernel's own allocations can run out with
memory free; `kho=off` frees it, devopsy-server's `setup.sh` adds it),
a clock out of sync, a kernel with AppArmor and no
`apparmor_parser` (no container starts), Docker logs that grow without
a limit, a disk 90% full or with less than 5 GB
free, no reflinks, a snapshot root on the release root's filesystem (a
backup volume not mounted), a tool the server needs missing, a role
held twice, and an environment with a failed first release, an
activation cut off past its switch, a sync, an import or a restore that
did not complete, services an operation left stopped, or containers
restarting, unhealthy or paused; what is running (a sync, an import, a
restore) is said, not warned. What kdump keeps is said with how to
free it (devopsy-server's `DEVOPSY_KDUMP=off`), never warned: crash
dumps are a choice. It exits 3 when it warned, so CI can run it as a
check.

## The server's devopsy

Commands on a server run part of their work in the server's devopsy
(releases' images, for one), so it must be recent enough for yours: an
older one stops a command that needs what it lacks before anything
changes, says what that is, and says to run `devopsy @<target>
--upgrade`, which always works. Commands that need nothing new still
run. A sync across servers checks the child's devopsy against the
parent's the same way, before the parent stops anything.
`DEVOPSY_PROTOCOL_CHECK=warn`
turns that stop into a warning and goes on, at your own risk: for a
local build ahead of the servers, when what you run may not need what
the server's devopsy lacks.

## Shells

`--shell` opens a container shell as locally;
`--shell-host` your login shell on the server, in the current release
(in the release root with `@<server>` alone), independent of the project
(the way in when it is broken); `--shell-host --root` root's, in its home,
whatever user the address or `~/.ssh/config` names (ssh's `-l root`), so
root's `authorized_keys` needs your key. An environment's directory that is a plain
`.devopsy/` directory without releases runs commands in place and refuses
releases. Project commands come from the current release.

## CI

Set `DEVOPSY_SSH_COMMAND` for SSH options, like git's
`GIT_SSH_COMMAND`:

```sh
DEVOPSY_SSH_COMMAND="ssh -i $KEY -o UserKnownHostsFile=$KNOWN_HOSTS" devopsy @vm1:prod --release
```

Its options come first, so they win over devopsy's own: ssh keeps the first
value of each. devopsy adds `-o ClearAllForwardings=yes` to every session:
it never needs the host's `LocalForward` lines, and each session would try
to bind them again while another holds them.

## Keys only

devopsy's sessions never ask for a password (`-o
PasswordAuthentication=no -o KbdInteractiveAuthentication=no`, over
`~/.ssh/config`). A devopsy server takes keys only, so a missing key fails
at once with ssh's `Permission denied (publickey)`, rather than asking in
the middle of a release's sessions, or with a child's services stopped for
a sync. A key's passphrase and a new host key are still asked for. For a
server you reach by password, set `DEVOPSY_SSH_ALLOW_PASSWORD=1`.

## Environments nobody releases

Floating tags (`mariadb:11.8`,
`caddy:2-alpine`) and base images get their fixes with a release, which
pulls them again ([Choosing tags](images.md#choosing-tags)). An environment nobody releases for
weeks keeps what it pulled last: schedule a release from CI, weekly,
with the same restricted key (`devopsy @prod --release` in a scheduled
job).

## SSH settings

A release opens several SSH sessions (checks, upload,
activation, its log), each a new connection and handshake: noticeable
through a jump host or VPN. `devopsy @<server> --ssh-config` prints a
`Host` block for `~/.ssh/config`: where it connects (`Hostname`, `User`,
`Port`), then connection sharing (`ControlMaster`, `ControlPersist`) and
keepalives, commented out. It never connects.

## Keys on a server

Who may log in as the deploy user is its
`~/.ssh/authorized_keys`, and `devopsy @<server> --ssh-keys` edits it over
the usual session, as that user, no root; any target on the server does
too. `--ssh-keys` lists them (fingerprint, type, comment, options),
`--ssh-keys add <file>` authorizes a public key, `--ssh-keys remove
<fingerprint or comment>` removes one (never the last: it would lock
everyone out). `add --restricted` is for a machine's key: `restrict`
(commands only, no shell) plus the agent and port forwarding a sync across
servers needs. Without a target, `devopsy --ssh-keys-generate [<name>]`
makes an ed25519 pair in `~/.ssh` (`devopsy-ci` by default; `--out <file>`
elsewhere), nothing sent anywhere. For CI: generate, `add` the `.pub` on
each server with `--restricted`, the private key as a protected file
variable, and in the job `eval "$(ssh-agent)"; ssh-add "$DEVOPSY_SSH_KEY"`
before `devopsy @prod --release`. Rotate by generating another, adding it,
then removing the old one.
