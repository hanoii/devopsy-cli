# Roles, exports and imports

Projects on one server share facts through compose labels on their
running containers. devopsy knows none of the names: they are conventions
between projects, like the Traefik template's.

```yaml
# the proxy, on its traefik service
- devopsy.role=proxy
- devopsy.export.BASE_DOMAIN=${PROXY_BASE_DOMAIN:-}

# a project, on any service
- devopsy.import.PROXY_BASE_DOMAIN=proxy/BASE_DOMAIN
```

- `devopsy.role=<name>`: only one compose project per server may hold a
  role. A release claiming one that is taken fails before uploading
  anything.
- `devopsy.export.<KEY>=<value>`: a fact on the running containers. Anyone
  with Docker access can read it, so never export secrets.
- `devopsy.import.<VAR>=<role or project>/<KEY>[?]`: at each release,
  before it goes live, the value is copied into the release's
  `devopsy-target.env`, unless the environment already sets `<VAR>`,
  even to an empty value, which is how to opt out. A source that is not
  running is waited for 30 seconds, then the release fails, unless the
  import ends in `?`. An empty export is a value like any other.

Each release keeps what it imported, so a changed export reaches a project
only with its next release or `--reload`. `devopsy @vm1:prod --debug
imports` lists every project on that server and marks as STALE the ones
holding an outdated value.

Roles guard against mistakes and are no security boundary, since anyone
in the docker group is root-equivalent.

## Probe

```sh
devopsy --probe [--ip <server ip>] <host>...
```

`--probe` checks a host the way a visitor reaches it:

- its DNS, through 1.1.1.1: does it point at `--ip`, or at Cloudflare?
- the certificate the server presents, verified as a browser does;
- an HTTPS request as visitors make it, with Cloudflare's 52x errors
  explained, and the case of Cloudflare's edge having no certificate for
  a name (a proxied second-level subdomain, beyond its free
  certificate).

It exits 1 on a problem, so it can serve as a smoke test. A proxy's own
commands print the line to run, like the Traefik template's `domains`.
