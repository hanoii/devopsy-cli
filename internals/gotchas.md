# Gotchas

Findings that belong to no one area.

- Compose recreates a container when a label's value changes (its config hash covers labels) and when its `image:` reference changes, even to the same image by digest; a plain `up` follows a name moved to another image; `pull_policy: never` makes `compose pull` skip the service; `compose start` starts the old container whatever the files now say.
- On Docker's containerd image store a pulled image's id is its index digest, every image (built and loaded too) has `RepoDigests`, a pull by digest leaves a name `docker image prune` never removes, and `docker rmi <repo>@<digest>` removes every name of that repository on the image, bookmarks included.
- Compose at a terminal (a pseudo-terminal from `with_tty` too) ends its progress by showing the cursor again, `\e[?25h`, with no newline, so the script's next line starts with it: `LineMarker` matches a marker after a line's leading escapes and passes them on.
- Compose reads `--env-file` more than once: `<(devopsy --env)` fails, a file works.
- `.env` values in double quotes are interpolated by compose's parser.
- GitHub can deliver a tag push several times: the release workflow queues runs per tag and GoReleaser replaces assets, so check a release's assets before assuming a failed run broke it.
- GitHub's `releases/latest` can lag a new release by minutes: `devopsy --upgrade vX.Y.Z` installs one directly.
- Go's HTTPS to github.com takes about 1.5 s, hence the detached update check.
- Traefik picks up a new container a few seconds after `up` returns.
- macOS hides what Linux servers do: dash as `sh` never execs the last command of `sh -c`, so a signal to it does not reach that command.
- OrbStack machines are `ProxyCommand` hosts from macOS, so a sync across servers needs hosts reachable directly (`ssh -G` without a proxy, for example `<machine>.orb.local` with a key of your own); their `/etc/ssh/ssh_config.d/10-orbstack.conf` sets `IdentityAgent`, which hides a forwarded agent from the parent's `ssh` ("get_agent_identities: Permission denied"), so remove it on test machines.
- OrbStack's own SSH keeps stdin open and leads no process group: the watcher needs a real sshd (`e2e/`).
