# Server setup how-to

What devopsy checks on a server and does not set up itself, step by
step, for the server's admin. Each topic below is what
`devopsy --server-setup-howto <topic>` prints, and every fix devopsy
offers for one of them names that command; `devopsy --server-setup-howto`
lists the topics. It runs anywhere, needing no server, project or Docker.

The steps are for Debian and Ubuntu with systemd, run as root on the
server, and say where the two differ. A value you choose, such as the
deploy user's name, is a shell variable set on a step's first line with
its usual value; a value of the system is computed.

- [`docker`](#docker): Docker from its repository, its compose and buildx plugins
- [`tools`](#tools): tar, flock and the other tools devopsy uses, sudo and acl
- [`processes`](#processes): /proc hiding every user's processes from the others
- [`project-users`](#project-users): the door's directory, Docker's containerd image store
- [`kexec-handover`](#kexec-handover): kho=off on the kernel's command line, for its memory
- [`kdump`](#kdump): no memory kept for a crash kernel, a choice

## docker

```
Docker, with its compose and buildx plugins

devopsy runs everything through the host's docker.service, as the deploy
user, and never installs it: --server-setup refuses a Docker that does not
answer, lacks its compose or buildx plugin, or is not the host's service.

As root, on Debian or Ubuntu with systemd:

1. Add Docker's apt repository, as Docker's install guide says
   (https://docs.docker.com/engine/install/): the distributions' own
   packages lag behind it.

      apt-get update
      apt-get install -y ca-certificates curl
      . /etc/os-release
      install -d -m 755 /etc/apt/keyrings
      curl -fsSL "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/docker.asc
      chmod a+r /etc/apt/keyrings/docker.asc
      printf 'Types: deb\nURIs: https://download.docker.com/linux/%s\nSuites: %s\nComponents: stable\nSigned-By: /etc/apt/keyrings/docker.asc\n' "$ID" "$VERSION_CODENAME" > /etc/apt/sources.list.d/docker.sources
      apt-get update

2. Install Docker with its compose and buildx plugins, and apparmor, whose
   parser Docker needs to start containers on a kernel with AppArmor; then
   start Docker now and at every boot.

      apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin apparmor
      systemctl enable --now docker

3. Put the deploy user in the docker group, from its next login: devopsy
   runs Docker as that user, whom the group makes root's equivalent.

      user=devopsy
      usermod -aG docker "$user"

To check it: Docker answers the deploy user, with both plugins, and
--server-setup refuses none of it.

      user=devopsy
      runuser -u "$user" -- docker version --format '{{.Server.Version}}'
      runuser -u "$user" -- docker compose version
      runuser -u "$user" -- docker buildx version
```

## tools

```
The tools devopsy uses

A release needs tar and flock, and --server-setup and every release refuse a
server without them. setsid, script, mkfifo and a GNU tail that answers
--version each serve a part of a release or a sync, which goes without them,
and project users need sudo, visudo and setfacl, which --project-users add
refuses without; --server-info and --server-setup name each one missing.

As root, on Debian or Ubuntu with systemd:

1. Install their packages: tar, util-linux (flock, setsid), bsdutils
   (script), coreutils (mkfifo, tail), sudo (sudo, visudo) and acl
   (setfacl).

      apt-get install -y tar util-linux bsdutils coreutils sudo acl

2. On Ubuntu, put GNU's coreutils in place of Rust's uutils, which Ubuntu
   may install as its coreutils: the two packages conflict, and apt swaps
   them only when told to remove the other.

      apt-get install -y --allow-remove-essential coreutils-from-gnu coreutils-from-uutils-

To check it: each tool is found, and tail says it is GNU coreutils'.

      command -v tar flock setsid script mkfifo tail sudo visudo setfacl
      tail --version | head -n 1
```

## processes

```
Every user's processes hidden from the others

A process's command line names projects, paths and arguments, and a devopsy
session's is its whole script, the operator's command in it: on a server
with project users, each should see only its own. --server-info and
--project-users add warn while /proc has no hidepid; devopsy checks it and
never mounts /proc.

As root, on Debian or Ubuntu with systemd:

1. Make a group that sees every process, and put the deploy user in it, from
   its next login: it is root through Docker already.

      group=devopsy-proc user=devopsy
      getent group "$group" >/dev/null || groupadd --system "$group"
      usermod -aG "$group" "$user"

2. Mount /proc with hidepid=invisible and the group's id, which is all the
   kernel takes, now and at every boot, once: the line takes the place of
   any other for /proc in /etc/fstab, and names /proc's own flags, which
   defaults would drop.

      group=devopsy-proc
      gid=$(getent group "$group" | cut -d: -f3)
      awk '$1 ~ /^#/ || $2 != "/proc"' /etc/fstab > /etc/fstab.new
      echo "proc /proc proc nosuid,nodev,noexec,hidepid=invisible,gid=$gid 0 0" >> /etc/fstab.new
      mv /etc/fstab.new /etc/fstab
      systemctl daemon-reload
      mount -o remount,hidepid=invisible,gid="$gid" /proc

3. Where the server has polkit, give it the group: it reads its callers'
   processes to answer them.

      group=devopsy-proc
      mkdir -p /etc/systemd/system/polkit.service.d
      printf '[Service]\nSupplementaryGroups=%s\n' "$group" > /etc/systemd/system/polkit.service.d/90-devopsy.conf
      systemctl daemon-reload
      systemctl try-restart polkit

To check it: /proc's options say hidepid=invisible and the group's id, and
--server-info no longer says that /proc shows every user's processes.

      findmnt -no OPTIONS /proc
```

## project-users

```
What a server with project users needs

Project users reach Docker through the door, which keeps their data in a
directory of its own on a mount with nosuid and nodev, and needs Docker's
containerd image store with buildx 0.33.0 or later: --project-users add
refuses a server without that mount or that Docker, and --server-info warns
of both. The door needs sudo and acl too (the tools topic), and /proc should
hide other users' processes from project users (the processes topic).

As root, on Debian or Ubuntu with systemd:

1. Make the door's directory, the deploy user's, and mount it on itself with
   nosuid and nodev, now and at every boot.

      user=devopsy space=/var/lib/devopsy-door
      install -d -o "$user" -m 755 "$space"
      grep -qs "^$space " /etc/fstab || echo "$space $space none bind,nosuid,nodev 0 0" >> /etc/fstab
      systemctl daemon-reload
      mountpoint -q "$space" || mount "$space"

2. Name it in the deploy user's config, ~/.config/devopsy/config.yaml, where
   the file has no door: yet.

      user=devopsy space=/var/lib/devopsy-door
      config=$(getent passwd "$user" | cut -d: -f6)/.config/devopsy/config.yaml
      runuser -u "$user" -- mkdir -p "${config%/*}"
      grep -qs '^door:' "$config" || printf '\ndoor:\n  space: %s\n' "$space" | runuser -u "$user" -- tee -a "$config" >/dev/null

3. Give Docker buildx 0.33.0 or later by upgrading its plugin's package
   alone, which restarts nothing.

      apt-get update
      apt-get install -y --only-upgrade docker-buildx-plugin

4. Skip this step where Docker is on its containerd image store already, as
   a Docker 29 installed fresh is: the first command then shows
   io.containerd.snapshotter.v1 as its driver type, where the classic store
   shows overlay2 as its driver. Moving a Docker off the classic store
   restarts it, which stops every container, and no container or image of
   the classic store shows on the containerd one (volumes and networks
   stay): every environment then needs a release to make its containers
   again, a rollback finds none of the images held for it, and the classic
   store keeps its disk until it is removed. To move it, turn on its
   daemon's containerd-snapshotter feature in /etc/docker/daemon.json and
   restart Docker (https://docs.docker.com/engine/storage/containerd/);
   where jq fails, nothing is written and nothing restarts:

      docker info --format '{{.DriverStatus}}'
      apt-get install -y jq
      f=/etc/docker/daemon.json
      [ -s "$f" ] || echo '{}' > "$f"
      jq '.features."containerd-snapshotter" = true' "$f" > "$f.new" && cat "$f.new" > "$f" && systemctl restart docker
      rm -f "$f.new"

5. Then add project users, locally, with --project-users add, and reboot the
   server after the first: sshd takes its part at the next boot.

To check it: the directory is mounted with nosuid and nodev, Docker says its
containerd image store (io.containerd.snapshotter.v1) and buildx 0.33.0 or
later, and --server-info no longer warns of either.

      space=/var/lib/devopsy-door
      findmnt -no OPTIONS --target "$space"
      docker info --format '{{.DriverStatus}}'
      docker buildx version
```

## kexec-handover

```
Kexec HandOver off

A kernel with Kexec HandOver on, as Ubuntu 26.04's 7.0 is by default, keeps
memory as CMA, which only movable pages may use: page tables, slab and
socket buffers can run out while memory is free. Nothing on a server hands
over to another kernel with kexec, so kho=off on the kernel's command line
frees it; a kernel without Kexec HandOver passes the word to init, which
ignores it. --server-info warns while the memory is kept.

As root, on Debian or Ubuntu with systemd:

1. Add kho=off to GRUB's command line in a drop-in of its own, named to sort
   after the image's, then update GRUB's config.

      mkdir -p /etc/default/grub.d
      printf '%s\n' 'GRUB_CMDLINE_LINUX_DEFAULT="$GRUB_CMDLINE_LINUX_DEFAULT kho=off"' > /etc/default/grub.d/zz-kho-off.cfg
      update-grub

2. Reboot at a quiet time, since the kernel reads its command line when it
   starts; the containers come back with Docker.

      systemctl reboot

To check it: the kernel's command line holds kho=off, and --server-info no
longer warns of Kexec HandOver.

      cat /proc/cmdline
```

## kdump

```
No crash kernel

kdump keeps memory for a crash kernel, which nothing else may use, to save a
dump when the kernel crashes. Going without it is a choice: a crash then
leaves no dump, and --server-info says the memory kept without a warning.
These steps are for an image with kdump-tools, whose
/etc/default/grub.d/kdump-tools.cfg adds crashkernel= to the kernel's
command line.

As root, on Debian or Ubuntu with systemd:

1. Take crashkernel= off GRUB's command line in a drop-in of its own, named
   to sort after kdump-tools', then update GRUB's config.

      printf '%s\n' 'GRUB_CMDLINE_LINUX_DEFAULT=$(echo "$GRUB_CMDLINE_LINUX_DEFAULT" | sed "s/crashkernel=[^ ]*//g")' > /etc/default/grub.d/zz-no-crashkernel.cfg
      update-grub

2. Disable kdump's service, which fails without that memory.

      systemctl disable kdump-tools

3. Reboot at a quiet time, since the kernel reads its command line when it
   starts; the containers come back with Docker.

      systemctl reboot

To check it: /sys/kernel/kexec_crash_size says 0, and --server-info no
longer says that kdump keeps memory.

      cat /sys/kernel/kexec_crash_size
```
