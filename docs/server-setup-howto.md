# Server setup how-to

What devopsy checks on a server and does not set up itself, step by step,
for the server's admin. `devopsy --server-setup-howto` lists the topics,
`devopsy --server-setup-howto <topic>` prints one, and `devopsy
--server-setup-howto --all` all of them, which is this page. Every fix
devopsy offers for one of them names its command. It runs anywhere, needing
no server, project or Docker.

- [docker](#docker): Docker with the containerd image store, buildx and compose
- [proxy](#proxy): an HTTP proxy for apt, Docker, containers and logins
- [tools](#tools): tar, flock and the other tools devopsy uses
- [processes](#processes): /proc hiding every user's processes from the others
- [project-users](#project-users): sudo and acl, then --project-users setup
- [kexec-handover](#kexec-handover): kho=off on the kernel's command line, freeing memory it keeps
- [kdump](#kdump): no memory kept for a crash kernel, a choice
- [xfs](#xfs): the data on XFS, so its copies are reflinks
- [deploy-user](#deploy-user): the one deploy user, and changing it

## docker

Docker with the containerd image store, buildx and compose

devopsy runs everything through the host's docker.service, as the deploy
user, and never installs it. Every server runs Docker's containerd image
store, buildx 0.33.0 or later and compose 2.33.0 or later: --server-setup,
every release, --project-users setup and --project-users add refuse a Docker
that does not answer, lacks its compose or buildx plugin, or runs below any
of them, and --server-setup one that is not the host's service. A Docker
installed from its repository runs the containerd image store. One upgraded
from an older version keeps the classic image store, and a distribution's
own package may run it too: devopsy refuses it, and does not move a Docker
from one store to the other. Such a server is replaced: these steps set up a
new one, and the old one's projects are released there.

Run each step as root on the server, Debian or Ubuntu with systemd. The
deploy user is the Unix user devopsy logs in as, which must exist. A step's
first line sets the values to change, if it has any (for example
user=devopsy).

1. Add Docker's apt repository, as Docker's install guide says
   (https://docs.docker.com/engine/install/), since the distributions' own
   packages lag behind it. A list of it already there is removed first: apt
   refuses every source while two lists of it name different keys.

   ```sh
   for f in /etc/apt/sources.list.d/*; do if grep -qs download.docker.com "$f"; then rm -f "$f"; fi; done
   apt-get update
   apt-get install -y ca-certificates curl
   . /etc/os-release
   install -d -m 755 /etc/apt/keyrings
   curl -fsSL "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/docker.asc
   chmod a+r /etc/apt/keyrings/docker.asc
   printf 'Types: deb\nURIs: https://download.docker.com/linux/%s\nSuites: %s\nComponents: stable\nSigned-By: /etc/apt/keyrings/docker.asc\n' "$ID" "$VERSION_CODENAME" > /etc/apt/sources.list.d/docker.sources
   apt-get update
   ```

2. Install Docker with its compose and buildx plugins, and apparmor, whose
   parser Docker needs to start containers on a kernel with AppArmor; then
   start Docker now and at every boot.

   ```sh
   apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin apparmor
   systemctl enable --now docker
   ```

3. Put the deploy user in the docker group, which applies from its next
   login: devopsy runs Docker as that user, and the group makes it as good
   as root.

   ```sh
   user=devopsy
   usermod -aG docker "$user"
   ```

4. Where Docker from its repository runs already with an older buildx or
   compose, upgrade the two plugins' packages alone, which restarts nothing:
   an upgrade of docker-ce or containerd.io restarts Docker, which stops
   every container.

   ```sh
   apt-get update
   apt-get install -y --only-upgrade docker-buildx-plugin docker-compose-plugin
   ```

To check it: Docker answers the deploy user, its driver type
io.containerd.snapshotter.v1 (the classic image store shows none), buildx
0.33.0 or later and compose 2.33.0 or later, and --server-setup refuses none
of it.

```sh
user=devopsy
runuser -u "$user" -- docker version --format '{{.Server.Version}}'
runuser -u "$user" -- docker info --format '{{.DriverStatus}}'
runuser -u "$user" -- docker buildx version
runuser -u "$user" -- docker compose version
```

## proxy

An HTTP proxy

A server that reaches the Internet only through an HTTP proxy needs it in
four places: apt, for installs and unattended upgrades, which run with no
proxy in their environment; Docker's daemon, for its pulls; the deploy
user's Docker client, which passes it to every container and build that user
makes; and the login environment, which every SSH session gets, devopsy
--upgrade among what runs there. Each leaves loopback (localhost, 127.0.0.1,
::1) and the hosts to reach without the proxy out of it: curl and libcurl
send even localhost to a proxy unless its list names it. A project's own
services never go in the list, since each container names those it reaches
itself. The proxy's address carries no login: the login environment is every
user's to read, and every container's environment shows it. --server-info
shows the daemon's proxy and the containers' under docker, and warns when
one is set without the other.

Run each step as root on the server, Debian or Ubuntu with systemd. The
deploy user is the Unix user devopsy logs in as, which must exist. A step's
first line sets the values to change, if it has any (for example
user=devopsy).

1. Set the proxy and what to reach without it, comma separated (host names,
   addresses, ranges), then give apt the proxy and each host name and
   address of the list direct: apt reads no list of exceptions, only a host
   at a time, never a range.

   ```sh
   proxy=http://proxy.example.com:3128 direct=
   printf 'Acquire::http::Proxy "%s";\nAcquire::https::Proxy "%s";\n' "$proxy" "$proxy" > /etc/apt/apt.conf.d/90-devopsy-proxy
   echo "$direct" | tr , '\n' | while read -r h; do case "$h" in '' | */* | .* | *[*:]*) ;; *) printf 'Acquire::http::Proxy::%s "DIRECT";\nAcquire::https::Proxy::%s "DIRECT";\n' "$h" "$h" >> /etc/apt/apt.conf.d/90-devopsy-proxy ;; esac; done
   ```

2. Give Docker's daemon the proxy for its pulls in a drop-in, loopback and
   the list left direct, then restart it, since it reads the proxy when it
   starts. With live-restore on in daemon.json, which --server-setup sets,
   the containers keep running through the restart; without it every
   container stops, so restart at a quiet time. A proxies key in
   daemon.json, where someone set one, wins over the drop-in.

   ```sh
   proxy=http://proxy.example.com:3128 direct=
   list=localhost,127.0.0.1,::1${direct:+,$direct}
   mkdir -p /etc/systemd/system/docker.service.d
   printf '[Service]\nEnvironment="HTTP_PROXY=%s" "HTTPS_PROXY=%s" "NO_PROXY=%s"\n' "$proxy" "$proxy" "$list" > /etc/systemd/system/docker.service.d/90-devopsy-proxy.conf
   systemctl daemon-reload
   systemctl restart docker
   ```

3. Give the deploy user's Docker client the proxy, which Docker passes to
   every container and build that user makes, the file's other keys kept
   (registry logins among them). Containers made before keep their old
   values until recreated: from the operator's machine, devopsy with the
   environment's address and up -d --force-recreate.

   ```sh
   user=devopsy proxy=http://proxy.example.com:3128 direct=
   list=localhost,127.0.0.1,::1${direct:+,$direct}
   apt-get update
   apt-get install -y jq
   home=$(getent passwd "$user" | cut -d: -f6)
   [ -d "$home/.docker" ] || install -d -m 700 -o "$user" -g "$user" "$home/.docker"
   if [ -s "$home/.docker/config.json" ]; then cat "$home/.docker/config.json"; else echo '{}'; fi | jq --arg p "$proxy" --arg n "$list" '.proxies.default += {httpProxy: $p, httpsProxy: $p, noProxy: $n}' > "$home/.docker/config.json.new" && install -m 600 -o "$user" -g "$user" "$home/.docker/config.json.new" "$home/.docker/config.json"
   rm -f "$home/.docker/config.json.new"
   ```

4. Put the proxy in the login environment, /etc/environment, which pam_env
   gives every login and SSH session, devopsy's included: the proxy's six
   variables taken out wherever they are, then appended, every other line
   kept.

   ```sh
   proxy=http://proxy.example.com:3128 direct=
   list=localhost,127.0.0.1,::1${direct:+,$direct}
   grep -vE '^[[:space:]]*(export[[:space:]]+)?(http_proxy|https_proxy|no_proxy|HTTP_PROXY|HTTPS_PROXY|NO_PROXY)=' /etc/environment > /etc/environment.new
   printf '%s\n' "http_proxy=\"$proxy\"" "https_proxy=\"$proxy\"" "no_proxy=\"$list\"" "HTTP_PROXY=\"$proxy\"" "HTTPS_PROXY=\"$proxy\"" "NO_PROXY=\"$list\"" >> /etc/environment.new
   cat /etc/environment.new > /etc/environment
   rm /etc/environment.new
   ```

To check it: Docker's daemon runs with the proxy, and --server-info shows it
and the containers' proxy under docker, with no warning that one is set
without the other; an SSH session started after it has the proxy in its
environment.

```sh
docker info --format '{{.HTTPSProxy}} {{.NoProxy}}'
grep -i _proxy= /etc/environment
```

## tools

The tools devopsy uses

A release needs tar and flock, and --server-setup and every release refuse a
server without them. setsid, script, mkfifo and a tail that answers
--version each serve a part of a release or a sync, which goes without them,
and --server-info and --server-setup warn of each one missing. What only
project users need, sudo and acl, is in the project-users topic.

Run each step as root on the server, Debian or Ubuntu with systemd. The
deploy user is the Unix user devopsy logs in as, which must exist. A step's
first line sets the values to change, if it has any (for example
user=devopsy).

1. Install their packages: tar, util-linux (flock, setsid), bsdutils
   (script) and coreutils (mkfifo, tail).

   ```sh
   apt-get update
   apt-get install -y tar util-linux bsdutils coreutils
   ```

To check it: each tool is found, and tail answers --version.

```sh
command -v tar flock setsid script mkfifo tail
tail --version | head -n 1
```

## processes

Every user's processes hidden from the others

A process's command line names projects, paths and arguments, and a devopsy
session's is its whole script, with the command that started it: on a server
with project users, each should see only its own. On a server with project
users, --project-users setup does this, with a unit of devopsy's
(devopsy-proc.service) in place of the /etc/fstab line below: these steps
are for doing it by hand. --server-info and --project-users add warn while
/proc has no hidepid.

Run each step as root on the server, Debian or Ubuntu with systemd. The
deploy user is the Unix user devopsy logs in as, which must exist. A step's
first line sets the values to change, if it has any (for example
user=devopsy).

1. Make a group that sees every process, and put the deploy user in it, from
   its next login: it is root through Docker already.

   ```sh
   group=devopsy-proc user=devopsy
   getent group "$group" >/dev/null || groupadd --system "$group"
   usermod -aG "$group" "$user"
   ```

2. Mount /proc with hidepid=invisible and gid= the group's number (the
   kernel takes no name), now and at every boot: the /etc/fstab line
   replaces any other for /proc, and names nosuid, nodev and noexec, which
   /proc has by default and an fstab line without them would drop. No
   reboot, and containers are not touched.

   ```sh
   group=devopsy-proc
   gid=$(getent group "$group" | cut -d: -f3)
   awk '$1 ~ /^#/ || $2 != "/proc"' /etc/fstab > /etc/fstab.new
   echo "proc /proc proc nosuid,nodev,noexec,hidepid=invisible,gid=$gid 0 0" >> /etc/fstab.new
   mv /etc/fstab.new /etc/fstab
   systemctl daemon-reload
   mount -o remount,hidepid=invisible,gid="$gid" /proc
   ```

3. Where the server has polkit, give it the group: it reads its callers'
   processes to answer them.

   ```sh
   group=devopsy-proc
   mkdir -p /etc/systemd/system/polkit.service.d
   printf '[Service]\nSupplementaryGroups=%s\n' "$group" > /etc/systemd/system/polkit.service.d/90-devopsy-proc.conf
   systemctl daemon-reload
   systemctl try-restart polkit
   ```

To check it: /proc's options say hidepid=invisible and the group's id, and
--server-info no longer says that /proc shows every user's processes.

```sh
findmnt -no OPTIONS /proc
```

## project-users

What a server with project users needs

Project users reach Docker through the door, devopsy's check of every Docker
request they make. It needs sudo, visudo and setfacl, which devopsy does not
install: --project-users setup and --project-users add refuse a server
without any of them, as they refuse a Docker below what every server runs
(the docker topic). --project-users setup sets the rest: the door's
directory on a mount of its own with nosuid and nodev, its name in the
deploy user's config, and every user's processes hidden from the others.
--server-info warns of each tool missing, and only notes sudo, visudo or
setfacl while the server has no project user. Steps 4 to 6 are for a server
set up by hand in place of step 3, where a line in /etc/fstab is fine.

Run each step as root on the server, Debian or Ubuntu with systemd. A step
that starts "From the operator's machine" runs there, where devopsy
addresses the server by its deploy user, not on the server. The deploy user
is the Unix user devopsy logs in as, which must exist. A step's first line
sets the values to change, if it has any (for example user=devopsy).

1. Install sudo (sudo, visudo) and acl (setfacl), which nothing else of
   devopsy uses.

   ```sh
   apt-get update
   apt-get install -y sudo acl
   ```

2. From the operator's machine, run --server-setup for this server: it makes
   the release root the deploy user's, which --project-users setup and
   --project-users add refuse without. It shows what it sets, asks, and
   applies.

3. From the operator's machine, run --project-users setup for this server:
   it shows what it sets, asks, and applies.

4. By hand, in place of step 3: make the door's directory, the deploy
   user's, and mount it on itself with nosuid and nodev, now and at every
   boot.

   ```sh
   user=devopsy space=/var/lib/devopsy-door
   install -d -o "$user" -m 755 "$space"
   grep -qs "^$space " /etc/fstab || echo "$space $space none bind,nosuid,nodev 0 0" >> /etc/fstab
   systemctl daemon-reload
   mountpoint -q "$space" || mount "$space"
   ```

5. By hand, in place of step 3: name it as door: space: in the deploy user's
   config, ~/.config/devopsy/config.yaml, unless the file has a door:
   already.

   ```sh
   user=devopsy space=/var/lib/devopsy-door
   config=$(getent passwd "$user" | cut -d: -f6)/.config/devopsy/config.yaml
   runuser -u "$user" -- mkdir -p "${config%/*}"
   grep -qs '^door:' "$config" || printf '\ndoor:\n  space: %s\n' "$space" | runuser -u "$user" -- tee -a "$config" >/dev/null
   ```

6. By hand, in place of step 3: hide every user's processes from the others,
   as the processes topic says.

7. From the operator's machine, add project users with --project-users add,
   and reboot the server after the first (--reboot): sshd reads what the add
   wrote for it only at the next boot, and until then no project user logs
   in.

To check it: the three tools are found, the directory is mounted with nosuid
and nodev, and --server-info no longer says either of them.

```sh
command -v sudo visudo setfacl
space=/var/lib/devopsy-door
findmnt -no OPTIONS --target "$space"
```

## kexec-handover

Kexec HandOver off

A kernel with Kexec HandOver on, as Ubuntu 26.04's 7.0 is by default, keeps
memory as CMA, which only movable pages may use: page tables, slab and
socket buffers can run out while memory is free. Nothing on a server hands
over to another kernel with kexec, so kho=off on the kernel's command line
frees it; a kernel without Kexec HandOver passes the word to init, which
ignores it. --server-info warns while the memory is kept.

Run each step as root on the server, Debian or Ubuntu with systemd. The
deploy user is the Unix user devopsy logs in as, which must exist. A step's
first line sets the values to change, if it has any (for example
user=devopsy).

1. Add kho=off to GRUB's command line in a drop-in of its own, named to sort
   after the cloud image's own drop-ins, then update GRUB's config.

   ```sh
   mkdir -p /etc/default/grub.d
   printf '%s\n' 'GRUB_CMDLINE_LINUX_DEFAULT="$GRUB_CMDLINE_LINUX_DEFAULT kho=off"' > /etc/default/grub.d/zz-kho-off.cfg
   update-grub
   ```

2. Reboot at a quiet time, since the kernel reads its command line when it
   starts; the containers come back with Docker.

   ```sh
   systemctl reboot
   ```

To check it: the kernel's command line holds kho=off, and --server-info no
longer warns of Kexec HandOver.

```sh
cat /proc/cmdline
```

## kdump

No crash kernel

kdump keeps memory for a crash kernel, which nothing else may use, to save a
dump when the kernel crashes. Going without it is a choice: a crash then
leaves no dump, and --server-info only notes the memory kept, with no
warning. These steps are for an image with kdump-tools, whose
/etc/default/grub.d/kdump-tools.cfg adds crashkernel= to the kernel's
command line.

Run each step as root on the server, Debian or Ubuntu with systemd. The
deploy user is the Unix user devopsy logs in as, which must exist. A step's
first line sets the values to change, if it has any (for example
user=devopsy).

1. Take crashkernel= off GRUB's command line in a drop-in of its own, named
   to sort after kdump-tools', then update GRUB's config.

   ```sh
   printf '%s\n' 'GRUB_CMDLINE_LINUX_DEFAULT=$(echo "$GRUB_CMDLINE_LINUX_DEFAULT" | sed "s/crashkernel=[^ ]*//g")' > /etc/default/grub.d/zz-no-crashkernel.cfg
   update-grub
   ```

2. Disable kdump's service, which fails without that memory.

   ```sh
   systemctl disable kdump-tools
   ```

3. Reboot at a quiet time, since the kernel reads its command line when it
   starts; the containers come back with Docker.

   ```sh
   systemctl reboot
   ```

To check it: /sys/kernel/kexec_crash_size says 0, and --server-info no
longer says that kdump keeps memory.

```sh
cat /sys/kernel/kexec_crash_size
```

## xfs

The data on XFS, for copies by reflinks

On a filesystem with reflinks (XFS, btrfs) a copy shares its blocks with the
original until one of them is written, so it takes seconds at any size. A
snapshot and a sync then stop an environment's services only for that clone;
without reflinks they stay stopped for the whole copy, which --server-info
warns of. A reflink never crosses filesystems, so the release root and
Docker's own directory, where volumes are, must be on the same one. The
simplest server has its whole root on XFS: the system, the releases and
Docker on one filesystem the size of the disk, which grows with it. devopsy
formats and converts nothing, and for a site XFS is about as fast as ext4.

Run each step as root on the server, Debian or Ubuntu with systemd. The
deploy user is the Unix user devopsy logs in as, which must exist. A step's
first line sets the values to change, if it has any (for example
user=devopsy).

1. See what the server has: the filesystem of the deploy user's home, where
   releases go unless its config names another root, and of Docker's
   directory. Both xfs, on one device, is what reflinks need.

   ```sh
   user=devopsy
   findmnt -no SOURCE,FSTYPE --target "$(getent passwd "$user" | cut -d: -f6)"
   findmnt -no SOURCE,FSTYPE --target "$(docker info --format '{{.DockerRootDir}}')"
   ```

2. Choose it for a new server, before anything is on its disk: no root is
   converted in place once it holds data. Where the server is installed by
   hand, pick XFS for the root in the installer's partitioning. A cloud
   image comes as ext4 with no choice: its root can be converted during its
   first boots, by user data given when the server is created, and never
   later.

3. Leave an existing server on ext4 as it is. To get reflinks for it, make a
   new server on XFS and move each environment there, by a release to it and
   a sync or a snapshot of its data. Until then, snapshots: {mirror: true}
   in a project's config keeps a copy that takes most of a snapshot while
   the services run.

4. Know what a clone costs a database: while a clone exists, what the
   database writes goes to new blocks, and its files stay fragmented once
   the clone is gone. A snapshot and a sync remove their clone once they
   have read it, for that reason. xfs_fsr, from the xfsprogs package, puts a
   file back in one extent while it is in use.

To check it: a copy by reflink works in the deploy user's home, and
--server-info says reflinks on its disks line and no longer warns of none.

```sh
user=devopsy
home=$(getent passwd "$user" | cut -d: -f6)
echo check > "$home/.reflink-a" && cp --reflink=always "$home/.reflink-a" "$home/.reflink-b" && echo reflinks
rm -f "$home/.reflink-a" "$home/.reflink-b"
```

## deploy-user

The server's deploy user, and changing it

devopsy works on a server as one Unix user, its deploy user: every release,
uid claim and setup there belongs to that user. --server-setup records the
user it runs as in /etc/devopsy/deploy-user, a file root owns, and from then
on devopsy refuses any other login, except root and the project users.
--server-setup itself refuses to run as another user, and to record its user
where the server shows another one set up already. Without that, two users
in the docker group would share one Docker, and a release from one could
take over the other's containers, volumes and uids. devopsy never changes
the record itself. When the wrong user was recorded, or the deploy user has
to change, these steps hand everything devopsy made for the old user to the
new one, then rewrite the record. Run them while no release is in progress.
The old user's own files, its ~/.ssh and dotfiles, stay its own.

Run each step as root on the server, Debian or Ubuntu with systemd. A step
that starts "From the operator's machine" runs there, where devopsy
addresses the server by its deploy user, not on the server. The deploy user
is the Unix user devopsy logs in as, which must exist. A step's first line
sets the values to change, if it has any (for example user=devopsy).

1. Name the old and the new user, and add the new one to the docker group.
   The group applies from its next login.

   ```sh
   old=devopsy new=deploy
   usermod -aG docker "$new"
   ```

2. Let the keys that log in as the old user log in as the new one too: the
   operator's, CI's and every sync's. The steps after this one log in as the
   new user.

   ```sh
   old=devopsy new=deploy
   from=$(getent passwd "$old" | cut -d: -f6)
   to=$(getent passwd "$new" | cut -d: -f6)
   group=$(id -gn "$new")
   install -d -m 700 -o "$new" -g "$group" "$to/.ssh"
   cat "$from/.ssh/authorized_keys" >> "$to/.ssh/authorized_keys"
   chown "$new:$group" "$to/.ssh/authorized_keys"
   chmod 600 "$to/.ssh/authorized_keys"
   ```

3. Move the old user's devopsy config to the new user: config.yaml, the
   setup's record server.yaml, and the project users' entries under door/.
   If the new user already has a devopsy config with files in it, nothing
   moves: merge the two by hand, keeping one server.yaml.

   ```sh
   old=devopsy new=deploy
   from=$(getent passwd "$old" | cut -d: -f6)/.config/devopsy
   to=$(getent passwd "$new" | cut -d: -f6)/.config/devopsy
   group=$(id -gn "$new")
   install -d -o "$new" -g "$group" "${to%/*}"
   if [ -e "$to" ] && ! rmdir "$to" 2>/dev/null; then
     echo "$to holds files already: merge the two configs by hand"
   else
     mv "$from" "$to" && chown -R -h "$new:$group" "$to"
   fi
   ```

4. Skip this step if the config names a releases: root. Otherwise the
   release root is the old user's home, and what devopsy made there moves to
   the new user's home. First stop every environment, the project users'
   too, with devopsy down in each one's current. Then move each project's
   directory, the uid claims (.devopsy-uids) and the image lock. A project
   is told by what only devopsy makes: an environment's releases/ beside its
   .lock, or a release holding a devopsy-target.env. A directory that only
   another tool's deploy layout fills (its releases/ beside current and
   shared/) is no project, and stays. Where the new home already holds an
   item of the same name, the old user's stays where it is: the step says
   so, and the two are merged by hand. Snapshots inside an environment move
   with it, and a snapshots: root elsewhere stays. Containers keep the paths
   their bind mounts had, so each environment is started again at the end.

   ```sh
   old=devopsy new=deploy
   from=$(getent passwd "$old" | cut -d: -f6)
   to=$(getent passwd "$new" | cut -d: -f6)
   projects=$(cd "$from" && for r in */*/releases */*/*/releases; do
     d=${r%/releases}
     if [ -d "$r" ] && { [ -e "$d/.lock" ] || [ -n "$(ls "$r"/*/devopsy-target.env 2>/dev/null)" ]; }; then echo "${r%%/*}"; fi
   done | sort -u)
   for p in $projects .devopsy-uids .devopsy-images.lock; do
     if [ -e "$to/$p" ]; then echo "$to/$p is there already: merge it by hand"
     elif [ -e "$from/$p" ]; then mv "$from/$p" "$to/$p"
     fi
   done
   ```

5. Hand the new user what devopsy made under the release root, the projects,
   the uid claims and the image lock: only what the old user owns, or its
   group holds, changes to the new user and its group. Files a container
   owns keep their owner. Set root to the release root: the new user's home
   after the step before, or the releases: root the config names. Do the
   same under a snapshots: root, if the config names one.

   ```sh
   old=devopsy new=deploy
   root=$(getent passwd "$new" | cut -d: -f6)
   og=$(id -gn "$old") ng=$(id -gn "$new")
   [ -z "$(find "$root" -maxdepth 0 -user "$old")" ] || chown "$new:$ng" "$root"
   projects=$(cd "$root" && for r in */*/releases */*/*/releases; do
     d=${r%/releases}
     if [ -d "$r" ] && { [ -e "$d/.lock" ] || [ -n "$(ls "$r"/*/devopsy-target.env 2>/dev/null)" ]; }; then echo "${r%%/*}"; fi
   done | sort -u)
   for p in $projects .devopsy-uids .devopsy-images.lock; do
     [ -e "$root/$p" ] || continue
     find "$root/$p" -xdev -user "$old" -exec chown -h "$new" {} +
     find "$root/$p" -xdev -group "$og" -exec chgrp -h "$ng" {} +
   done
   ```

6. If the old user owns the devopsy binary or its directory, as
   devopsy-server installs it, give them to the new user, so that --upgrade
   can replace it.

   ```sh
   old=devopsy new=deploy
   bin=$(readlink -f "$(command -v devopsy)")
   for p in "$bin" "${bin%/*}"; do
     [ -z "$(find "$p" -maxdepth 0 -user "$old")" ] || chown "$new" "$p"
   done
   ```

7. If the server has project users, give the new user what the old one owns
   in their directory (door: space: in the config, else
   /var/lib/devopsy-door), changing owners only, and add it to the group
   that sees every process once /proc hides them, which applies from its
   next login. Then name it in the door's sudo rules and in sshd's drop-in.
   Both files are checked before sshd reads them again.

   ```sh
   old=devopsy new=deploy space=/var/lib/devopsy-door
   find "$space" -xdev -user "$old" -exec chown -h "$new" {} +
   usermod -aG devopsy-proc "$new"
   sed -i "s/(${old} :/(${new} :/; s/^${old} ALL=/${new} ALL=/" /etc/sudoers.d/devopsy-projects
   visudo -cf /etc/sudoers.d/devopsy-projects
   sed -i "s/^\(\tAuthorizedKeysCommandUser \)${old}\$/\1${new}/" /etc/ssh/sshd_config.d/devopsy.conf
   sshd -t && systemctl reload ssh
   ```

8. Write the new user into the record, readable by everyone, since every
   session reads it. The record is replaced only once the new one is written
   whole. The next step's --server-setup rewrites it in devopsy's own form.

   ```sh
   new=deploy
   install -d -m 755 "$(dirname /etc/devopsy/deploy-user)"
   printf '%s\n' "$new" > /etc/devopsy/deploy-user.new &&
     chmod 644 /etc/devopsy/deploy-user.new &&
     mv /etc/devopsy/deploy-user.new /etc/devopsy/deploy-user
   ```

9. From the operator's machine, run --server-setup for this server as the
   new user. It finds the record naming it, and sets up the rest. Then start
   each environment you stopped earlier, with its deploy command or a
   release.

To check it: the record names the new user, nothing devopsy made under the
release root still belongs to the old user, and --server-info as the new
user lists the server's environments, with no refusal of the login.

```sh
old=devopsy new=deploy
cat /etc/devopsy/deploy-user
root=$(getent passwd "$new" | cut -d: -f6)
find "$root" -maxdepth 4 -user "$old" -print | head
```
