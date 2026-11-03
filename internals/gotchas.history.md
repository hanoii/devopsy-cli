History, not rules: `internals/gotchas.md` wins where they differ.

- The compose recreation rules and the containerd store's behaviors were checked on Docker 29.9 with the containerd image store on 2026-10-23.
- The cursor escape before a marker was found on 2026-10-28: `devopsy-internal-beat` showed on the operator's terminal after a build, and the switch was answered only by the next heartbeat, since the marker's line started with `\e[?25h`.
