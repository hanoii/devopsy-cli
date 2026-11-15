# Codes

A message devopsy says for a server's admin carries a code, so a project
user who sees it on a release, a `docker` command or its `--server-info`
can pass it on, and
the admin finds the same code in `--server-info` and in their own
commands. A project user's line
reads:

```
devopsy: warning: W0001: the server's firewall is not loaded as devopsy loads it
devopsy: | report W0001 to the server's admin
```

| Code | Says | What the admin does |
|------|------|---------------------|
| W0001 | the server's firewall is not loaded as devopsy loads it | Run `devopsy @devopsy@<server>: --server-setup`, which loads devopsy's firewall again; `--server-info` says what differs. |
| W0002 | the project users' directory is not on a mount with nosuid and nodev | Run `devopsy @devopsy@<server>: --project-users setup`, which mounts it again; `--server-info` says why. |
| W0003 | the server's devopsy is older than the operator's | Run `devopsy @devopsy@<server>: --upgrade`. |
| W0004 | the server needs a reboot | Reboot it at a quiet time: its containers come back with Docker. `--server-info` names the packages that asked for it. |
| W0005 | the server's clock is not synchronized | Turn its time service on (`timedatectl set-ntp true`, as root), or name a time server its network answers; `--server-info` says which. |
| W0006 | Kexec HandOver keeps memory as CMA, which only movable pages may use | Put `kho=off` on the kernel's command line and reboot; `devopsy --server-setup-howto kexec-handover` prints the steps. |
| W0007 | the snapshot root is on the release root's filesystem | Mount the volume meant for snapshots on the snapshot root, which `--server-info` names. |
| W0008 | the server lacks a tool devopsy uses | Install it with the system's packages; `--server-info` names it, with the topic whose steps install it. |
| W0009 | this login is not the server's deploy user | Address the server as the user the error names, for example `devopsy @devopsy@<server>: --server-info`; `devopsy --server-setup-howto deploy-user` prints the steps to make another user the deploy user. |
| W0010 | the server's record of its deploy user cannot be read | Correct `/etc/devopsy/deploy-user` as root, in root's shell (`devopsy @<server>: --shell-host --root`): the deploy user's name alone on a line, the file root's with mode 644. |

## W0001

`--server-info` and `--server-setup` say `W0001` for any of these, each
named on a line of its own:

- the firewall's chains are not as devopsy loads them (a reload of the
  server's own firewall, an `iptables -F`), read as root;
- its last load failed, so no load is marked in `/run`, or the mark says
  the other kind's (the containers' chain alone where the door's is
  wanted);
- one of its files is missing, differs from devopsy's, or is of another
  mode;
- its unit's boot link is gone, so no boot loads it;
- a timer an older install put is still there.

A release sees two of them: chains not as devopsy loads them, read as root
by the kind its mark says, and a last load that failed. A mark of the
other kind, the files, the boot link and the timer are for `--server-info`
and `--server-setup`, which judge the setup against what is wanted. A
release reads the chains only through this host's Docker, never another
host's (`DOCKER_HOST`, a context).

## W0002

The project users' directory (`door: space:` in the deploy user's
config) is where devopsy keeps their data. On a mount without `nosuid`
and `nodev`, a container's setuid file or a crafted archive there could
cross projects or reach root, so the door refuses every request of
every project user until it is mounted with both again. A project user
sees it as an error on any `docker` command; `--server-info` and
`--project-users add` say it with why: the directory missing, or its
mount gone (a boot where it failed) or without the two options.

## W0009

`--server-setup` records the user it runs as in `/etc/devopsy/deploy-user`,
root's, and every session's devopsy then refuses any other login, but root
and project users, before it changes or reads anything, `--shell-host`
included (`--shell-host --root` and `--match-server-version` aside). The reader is anyone else in the `docker`
group, often the admin logged in under their own account by default. Two
such users would each have a release root, uid claims and a setup of their
own on one Docker, sharing its compose names: a release of one target from
the second would take over the first one's containers and volumes. A server
set up before the record gets it at its next `--server-setup` or
`--upgrade`; until then nothing is refused.

## W0010

The record of the deploy user is there but cannot be read, or names no one
user: no line, several, or a name no login has. devopsy does not guess who
it names, so it refuses every login but root and project users, the deploy
user's and `--shell-host`'s too, until root corrects the file from root's own
shell, `--shell-host --root`.
