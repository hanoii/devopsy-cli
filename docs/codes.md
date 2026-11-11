# Codes

A message devopsy says for a server's admin carries a code, so a project
user who sees it on a release can pass it on, and the admin finds the same
code in `--server-info` and in their own releases. A project user's line
reads:

```
devopsy: warning: W0001: the server's firewall is not loaded as devopsy loads it
devopsy: | report W0001 to the server's admin
```

| Code | Says | What the admin does |
|------|------|---------------------|
| W0001 | the server's firewall is not loaded as devopsy loads it | Run `devopsy @devopsy@<server>: --server-setup`, which loads devopsy's firewall again; `--server-info` says what differs. |

## W0001

`--server-info` and `--server-setup` say `W0001` for any of these, each
named on a line of its own:

- the firewall's chains are not as devopsy loads them (a reload of the
  server's own firewall, an `iptables -F`), read as root;
- its last load failed, so no load is marked in `/run`;
- one of its files is missing, not devopsy's, or of another mode;
- its unit's boot link is gone, so no boot loads it;
- a timer an older install put is still there.

A release sees only the first two: it reads the chains, and the mark, and
nothing else of the setup.
