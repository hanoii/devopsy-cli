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
| W0001 | the server's firewall is not loaded as devopsy loads it | Run `devopsy @devopsy@<server>: --server-setup`, which loads devopsy's firewall again. Its chains were dropped (a reload of the server's own firewall, an `iptables -F`) or a load failed; `--server-info` says what is missing. |
