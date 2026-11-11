History, not rules: internals/server-setup.md wins where they differ.

- The root run shares the host's pid namespace, and is no privileged container: tried on a Debian 13 machine with systemd and Docker, `systemctl` from the chroot failed ("Failed to connect to system scope bus via local transport: No data available") until the run had the host's pids, since systemd refuses a peer outside its pid namespace; the firewall's run already declared them.
- Docker is never restarted on a `daemon.json` dockerd refuses: on that machine, a pending file with a key Docker did not know, written by someone else, kept Docker down after the restart, so the root script checks the file before any restart.
- A drop-in put without a restart is reloaded: otherwise systemd said at every call that `docker.service` changed on disk.
- Whether Docker restarts is the apply's to judge under its locks, the operator passing only the condition: a part changed between the sessions left the apply with nothing for Docker to take while the operator's answer still asked for a restart, and `--yes`'s "no container running" was counted in the first session.
- One judgment of each part, the model's, carried in the facts: the operator's plan judged the states a second time and missed recording devopsy's value for a kept part back in sync, so the kept value written again later read as kept.
- A part set outside `daemon.json` is a state of its own: a part the file did not set read as devopsy's while `docker.service`'s flags set it, and putting it would keep Docker from starting.
- The release root has one form, its links followed: the facts resolved links only once the root was there, so a root behind a link and not made yet got two drop-ins on two runs.
- The record is written right after the file, before the restart's wait: a wait not confirmed in time used to return first, losing a first run's record and its keep answers.
- The deploy user's environments are walked through `--server-info`'s own filter: the restart's locks walked a project user's tree in the release root, opening a lock there that user could change.
