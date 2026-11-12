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

- [`processes`](#processes): /proc hiding every user's processes from the others

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
