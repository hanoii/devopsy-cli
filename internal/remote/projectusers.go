package remote

import (
	"path"
)

// ProjectUsersFlag is the hidden flag behind --project-users, run by its session on the
// server as the deploy user: a server without it refuses, naming the upgrade.
const ProjectUsersFlag = "--project-users-step"

// ProjectUsersScript runs ProjectUsersFlag with args on the server, after the prelude's checks.
func ProjectUsersScript(t *Target, args []string) string {
	return "set -eu\n" + basePrelude(t) + devopsyCall("", append([]string{ProjectUsersFlag}, args...), true) + "\n"
}

// DoorRebootMarker says sshd has not taken the door's drop-in yet: /run is emptied at boot,
// so it goes by itself with the reboot that takes it.
const DoorRebootMarker = "/run/devopsy-door-reboot"

// DoorSSHDropIn, DoorKeysCommand and DoorSudoers are what devopsy puts on a host for its
// project users, all of them devopsy's own interface: written by --project-users add.
const (
	DoorSSHDropIn   = "/etc/ssh/sshd_config.d/devopsy.conf"
	DoorKeysCommand = "/usr/local/libexec/devopsy-authorized-keys"
	DoorSudoers     = "/etc/sudoers.d/devopsy-projects"
	DoorDockerLink  = "/usr/local/bin/docker"
)

// doorDropIn is sshd's part: a project user's keys are its deploy user's, asked of devopsy,
// never its own ~/.ssh/authorized_keys. A Match ends with its included file.
func doorDropIn(deploy string) string {
	return "# Written by devopsy (devopsy @<server> --project-users add): a project user's keys are\n" +
		"# its deploy user's, asked of devopsy, never its own ~/.ssh/authorized_keys.\n" +
		"Match Group " + DoorGroup + "\n" +
		"\tAuthorizedKeysFile none\n" +
		"\tAuthorizedKeysCommand " + DoorKeysCommand + " %u\n" +
		"\tAuthorizedKeysCommandUser " + deploy + "\n"
}

// doorKeysCommand is the root-owned script sshd runs, since it refuses a command root does
// not own, and the binary is the deploy user's so that it upgrades itself.
func doorKeysCommand(devopsy string) string {
	return "#!/bin/sh\n# Written by devopsy (devopsy @<server> --project-users add): sshd's\n" +
		"# AuthorizedKeysCommand for project users.\nexec " + devopsy + " --authorized-keys \"$1\"\n"
}

// doorSudoers is the door's two rules: a project user asks its deploy user, which reads a
// project's tree as that project's user.
func doorSudoers(deploy, devopsy string) string {
	return "# Written by devopsy (devopsy @<server> --project-users add): the door's rules.\n" +
		"%" + DoorGroup + " ALL=(" + deploy + ") NOPASSWD: " + devopsy + " --door *\n" +
		deploy + " ALL=(%" + DoorGroup + ") NOPASSWD: " + devopsy + " --door-read *\n"
}

// DoorHostScript is what --project-users add does as root on the host, through the util
// image (chroot into /host): the door's pieces when missing or different, each checked before
// it lands (visudo, sshd -t), then the user, its home and its tree. Its arguments: the user,
// the deploy user, the tree to make ("" for the home's default), and the release root the
// project user's own config names ("" for none). The deploy user and root are never in the
// group: sshd would then ignore their own keys. A drop-in written says "reboot" on stdout.
func DoorHostScript(deploy, devopsy string) string {
	return `set -eu
umask 022
user=$1 tree=$2 root=$3
group=` + DoorGroup + `
getent group "$group" >/dev/null || groupadd --system "$group"
for u in root ` + Quote(deploy) + `; do
  if id -nG "$u" | tr ' ' '\n' | grep -qx "$group"; then
    echo "` + errorHead + `$u is in $group, and sshd would ignore its own keys for devopsy's: take it out first (gpasswd -d $u $group)" >&2
    exit 3
  fi
done
# put <file> <mode> <check> <content>: content into file when it differs, checked before it
# lands. Never in a pipeline, whose subshell an exit would leave alone.
put() {
  tmp=$(mktemp "$1.XXXXXX")
  printf '%s' "$4" >"$tmp"
  chmod "$2" "$tmp"
  if cmp -s "$tmp" "$1" 2>/dev/null; then rm -f "$tmp"; return 1; fi
  if ! sh -c "$3" sh "$tmp" >&2; then rm -f "$tmp"; echo "` + errorHead + `$1 did not pass its check" >&2; exit 4; fi
  mv -f "$tmp" "$1"
}
# A project user's docker is devopsy, which asks the door; the deploy user's goes through it to
# Docker. A real docker there would put the project users on the socket.
if [ ! -L ` + DoorDockerLink + ` ] && [ -e ` + DoorDockerLink + ` ]; then
  echo "` + errorHead + DoorDockerLink + ` is a real docker: project users would reach the socket through it" >&2
  exit 3
fi
[ "$(readlink ` + DoorDockerLink + ` 2>/dev/null || :)" = ` + Quote(devopsy) + ` ] || ln -sfn ` + Quote(devopsy) + ` ` + DoorDockerLink + `
mkdir -p ` + path.Dir(DoorKeysCommand) + ` ` + path.Dir(DoorSSHDropIn) + `
put ` + DoorSudoers + ` 440 'visudo -cqf "$1"' ` + Quote(doorSudoers(deploy, devopsy)) + ` || :
put ` + DoorKeysCommand + ` 755 'sh -n "$1"' ` + Quote(doorKeysCommand(devopsy)) + ` || :
old=$(cat ` + DoorSSHDropIn + ` 2>/dev/null || :)
if put ` + DoorSSHDropIn + ` 644 true ` + Quote(doorDropIn(deploy)) + `; then
  if ! sshd -t; then
    if [ -n "$old" ]; then printf '%s\n' "$old" >` + DoorSSHDropIn + `; else rm -f ` + DoorSSHDropIn + `; fi
    echo "` + errorHead + `sshd refused ` + DoorSSHDropIn + `: put back as it was" >&2
    exit 4
  fi
  touch ` + DoorRebootMarker + ` /run/reboot-required
  grep -qx devopsy /run/reboot-required.pkgs 2>/dev/null || echo devopsy >>/run/reboot-required.pkgs
  echo reboot
fi
made=
if ! id -u "$user" >/dev/null 2>&1; then
  useradd --create-home --shell /bin/sh --groups "$group" "$user"
  usermod -p '*' "$user"
  made=1
fi
id -nG "$user" | tr ' ' '\n' | grep -qx "$group" || usermod -aG "$group" "$user"
home=$(getent passwd "$user" | cut -d: -f6)
chmod 750 "$home"
[ -z "$tree" ] || install -d -m 750 -o "$user" -g "$user" "$tree"
# The project user's own releases root, so its devopsy releases into that tree: written as
# that user, whose home it is, and only when the user is new.
if [ -n "$made" ] && [ -n "$root" ]; then
  runuser -u "$user" -- sh -c 'mkdir -p "$HOME/.config/devopsy" && printf "releases:\n  root: %s\n" "$1" >"$HOME/.config/devopsy/config.yaml"' sh "$root"
fi
`
}
