# Roles, exports and imports

Projects on one host share facts through compose labels. devopsy knows none
of the names; they are conventions between projects, like the Traefik
template's:

```yaml
# the proxy, on its traefik service
- devopsy.role=proxy
- devopsy.export.BASE_DOMAIN=${PROXY_BASE_DOMAIN:-}

# a project, on any service
- devopsy.import.PROXY_BASE_DOMAIN=proxy/BASE_DOMAIN
```

- `devopsy.role=<name>`: only one compose project per host holds it; a
  release claiming a taken role fails before uploading anything.
- `devopsy.export.<KEY>=<value>`: a fact on the running containers (visible
  to anyone with Docker access: never secrets).
- `devopsy.import.<VAR>=<role or project>/<KEY>[?]`: at each release, before
  it goes live, the value is copied into its `devopsy-target.env`, unless the
  environment already sets `<VAR>` (even empty, which opts out). A source
  not running is waited for 30 seconds; then the release fails, unless the
  import ends in `?`. An empty export is a value.

Each release keeps what it imported: `devopsy @vm1:prod --debug imports`
lists every project on that host and marks those with an outdated value as
STALE, to release again. Roles prevent mistakes, not attacks: anyone in the
docker group is root-equivalent.

## Probe

```sh
devopsy --probe [--ip <server ip>] <host>...
```

DNS through 1.1.1.1 (does it point at `--ip`, or at Cloudflare?), the
certificate the server presents, verified like a browser, and an HTTPS
request as visitors make it, with Cloudflare's 52x errors explained, and
its edge having no certificate for a name (a proxied second-level
subdomain, beyond its free certificate). Exits
1 on a problem, so it doubles as a smoke test. A proxy's own commands print
the line to run, like the Traefik template's `domains`.
