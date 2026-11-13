# Codes

A message devopsy says for a server's admin carries a code, so a project
user who sees it on a release or a `docker` command can pass it on, and
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
