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
- OrbStack machines are `ProxyCommand` hosts from macOS, so a sync across servers needs hosts reachable directly (`ssh -G` without a proxy, for example `<machine>.orb.local` with a key of your own).
- OrbStack's `docker exec` runs with umask 000, Docker Engine's (CI's, a server's) with 022, so what an end-to-end test writes through it is world-writable locally and a bug that hangs on a mode passes: `execIn` sets 022 itself.
- OrbStack's own SSH keeps stdin open and leads no process group: the watcher needs a real sshd (`e2e/`).
- `docker load` of an OCI layout exits 0 when a blob is missing or fails to unpack, saying `Error unpacking image` on stdout, and leaves the name; it says `Loaded image: ` and the image's familiar name (`busybox@sha256:...` for Docker Hub's library) on Docker 27.5, 28.5 and 29.9. Its index needs the canonical name in `io.containerd.image.name`: a familiar one loads with no `RepoDigests`. The classic image store refuses a layout (`does not contain a manifest.json`).
- buildx before 0.33.0 answers `invalid reference format` to an `oci-layout://` destination, and `imagetools create` wraps a single manifest in a new index of another digest unless given `--prefer-index=false`.
- Compose's `--pull missing` overrides a service's `pull_policy: never`, and `compose config --images` always lists dependencies, names no service and repeats an image several services share.
