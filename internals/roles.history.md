History, not rules: `internals/roles.md` wins where they differ.

- A `domains` contract in devopsy's core was rejected: it was shaped by one pairing, the Traefik template and the projects behind it, and devopsy owns mechanics, not a proxy's policy; the role and key names stayed the template's convention.
- Imports resolve at each release because compose interpolates a label (a Traefik router rule) when it reads the files, before any container exists, so a value read at runtime would come too late.
- An empty export counts as a value because otherwise every project importing without `?` would fail under a proxy that exports no wildcard domain.
- Rollbacks never check roles or imports because the check would fail a rollback exactly when one is needed, the proxy down.
- Roles were left advisory and unlocked between targets as an accepted cost: anyone in the docker group is root-equivalent, so a check at releases is all the guarantee a host can give.
