package remote

import (
	"fmt"
	"path"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/report"
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
// never its own ~/.ssh/authorized_keys, and nothing of its home runs at a login (~/.ssh/rc),
// which a leaked key would leave for the next login of a key kept. No agent, X11 or tunnel is
// forwarded to it: devopsy forwards none, and a process a leaked key left could use an
// operator's forwarded agent. A Match ends with its included file.
func doorDropIn(deploy string) string {
	return "# Written by devopsy (devopsy @<server> --project-users add): a project user's keys are\n" +
		"# its deploy user's, asked of devopsy, never its own ~/.ssh/authorized_keys.\n" +
		"Match Group " + DoorGroup + "\n" +
		"\tAuthorizedKeysFile none\n" +
		"\tAuthorizedKeysCommand " + DoorKeysCommand + " %u\n" +
		"\tAuthorizedKeysCommandUser " + deploy + "\n" +
		"\tPermitUserRC no\n" +
		"\tAllowAgentForwarding no\n" +
		"\tX11Forwarding no\n" +
		"\tPermitTunnel no\n"
}

// doorKeysCommand is the root-owned script sshd runs, since it refuses a command root does
// not own, and the binary is the deploy user's so that it upgrades itself.
func doorKeysCommand(devopsy string) string {
	return "#!/bin/sh\n# Written by devopsy (devopsy @<server> --project-users add): sshd's\n" +
		"# AuthorizedKeysCommand for project users.\nexec " + Quote(devopsy) + " --authorized-keys \"$1\"\n"
}

// doorSudoers is the door's two rules: a project user asks its deploy user, which reads a
// project's tree as that project's user. A project user's sudo to the door keeps none of its
// variables, whatever the server's sudoers keeps for others: Go's runtime reads some
// (GODEBUG) before the door clears its environment. The Defaults are the command's, which
// sudo applies after every user's, group's and run-as user's, wherever those are written.
func doorSudoers(deploy, devopsy string) string {
	return "# Written by devopsy (devopsy @<server> --project-users add): the door's rules.\n" +
		"Cmnd_Alias DEVOPSY_DOOR = " + devopsy + " --door *\n" +
		"Defaults!DEVOPSY_DOOR env_reset, !env_keep, !setenv\n" +
		"%" + DoorGroup + " ALL=(" + deploy + " : " + DoorRunGroup + ") NOPASSWD: DEVOPSY_DOOR\n" +
		deploy + " ALL=(%" + DoorGroup + ") NOPASSWD: " + devopsy + " --door-read *\n"
}

// doorTreeCheck refuses a tree that is there and not the user's ($1 the user, $2 the tree): it
// would hand that user what another made there, whose entries it could then rename. A link
// there is never the user's.
var doorTreeCheck = `if [ -n "$2" ] && { [ -e "$2" ] || [ -L "$2" ]; }; then
  if [ -L "$2" ] || [ -z "$(find "$2" -maxdepth 0 -user "$1" 2>/dev/null)" ]; then
    ` + echoLine(report.ErrorLevel, "$2 is there already, and not $1's: move it aside first, since a project user gets only a tree --project-users add made") + `
    exit 3
  fi
fi
`

// doorUserCheck refuses a Unix user that is there and no project user ($1 the user): one add
// made is in the group, remove keeping it there, while any other is someone's account (the
// deploy user's, named devopsy-<project>), which the group would take from sshd's own keys
// and put behind the firewall, its home handed to the door.
var doorUserCheck = `if id -u "$1" >/dev/null 2>&1 && ! id -nG "$1" | tr ' ' '\n' | grep -qx ` + DoorGroup + `; then
  ` + echoLine(report.ErrorLevel, "a Unix user $1 is there already, and no project user: a project user is one --project-users add made") + `
  exit 3
fi
`

// hostGroup makes the project users' group when it is not there yet.
var hostGroup = "getent group " + DoorGroup + " >/dev/null || groupadd --system " + DoorGroup + "\n" +
	"getent group " + DoorRunGroup + " >/dev/null || groupadd --system " + DoorRunGroup + "\n"

// hostPut is put <file> <mode> <check> <content>: content into file when it differs, checked
// before it lands, failing otherwise when it changed nothing. Never in a pipeline, whose
// subshell an exit would leave alone.
var hostPut = `put() {
  tmp=$(mktemp "$1.XXXXXX")
  printf '%s' "$4" >"$tmp"
  chmod "$2" "$tmp"
  if cmp -s "$tmp" "$1" 2>/dev/null; then rm -f "$tmp"; return 1; fi
  if ! sh -c "$3" sh "$tmp" >&2; then rm -f "$tmp"; ` + echoLine(report.ErrorLevel, "$1 did not pass its check") + `; exit 4; fi
  mv -f "$tmp" "$1"
}
`

// doorSSHDTakes fails unless sshd takes the drop-in for $user, as it resolves that user's
// settings (sshd -T): devopsy's keys command as the deploy user, and no file of keys. A
// sshd_config with no Include of sshd_config.d, or with it below a Match, parses (sshd -t)
// and leaves the user on its own ~/.ssh/authorized_keys, where a leaked key adds a second one.
// The user stays with no keys: its entry is written only after this script.
func doorSSHDTakes(deploy string) string {
	return `[ -d /run/sshd ] || mkdir -p -m 755 /run/sshd
eff=$(sshd -T -C "user=$user,host=devopsy,addr=127.0.0.1" 2>&1) || { ` + echoLine(report.ErrorLevel, "sshd -T for $user failed: $eff") + `; exit 4; }
if ! { printf '%s\n' "$eff" | grep -qx 'authorizedkeysfile none' &&
  printf '%s\n' "$eff" | grep -qxF 'authorizedkeyscommand ` + DoorKeysCommand + ` %u' &&
  printf '%s\n' "$eff" | grep -qxF 'authorizedkeyscommanduser '` + Quote(deploy) + `; }; then
  ` + echoLine(report.ErrorLevel, "sshd does not take "+DoorSSHDropIn+" for $user, which would log in by its own ~/.ssh/authorized_keys", report.Fix("include it in /etc/ssh/sshd_config, at its top, before any Match:", "Include /etc/ssh/sshd_config.d/*.conf")) + `
  exit 4
fi
`
}

// DoorDevopsyPath refuses a path of devopsy's that sudoers would read as more than a path: it
// has no quoting, and a space, a comma, a colon, an equals sign or a backslash mean something.
func DoorDevopsyPath(p string) error {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, " \t,:=\\\n\"'") {
		return fmt.Errorf("devopsy is at %s, which sudo's rules cannot name: put it at a path with none of space , : = \\ or quotes", p)
	}
	return nil
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
` + hostGroup + `for u in root ` + Quote(deploy) + `; do
  if id -nG "$u" | tr ' ' '\n' | grep -qx "$group"; then
    ` + echoLine(report.ErrorLevel, "$u is in $group, and sshd would ignore its own keys for devopsy's", report.Fix("take it out first:", "gpasswd -d $u $group")) + `
    exit 3
  fi
done
(set -- "$user" "$tree"
` + doorUserCheck + doorTreeCheck + `)
` + hostPut + `# A project user's docker is devopsy, which asks the door; the deploy user's goes through it to
# Docker. A real docker there would put the project users on the socket.
if [ ! -L ` + DoorDockerLink + ` ] && [ -e ` + DoorDockerLink + ` ]; then
  ` + echoLine(report.ErrorLevel, DoorDockerLink+" is a real docker: project users would reach the socket through it") + `
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
    ` + echoLine(report.ErrorLevel, "sshd refused "+DoorSSHDropIn+": put back as it was") + `
    exit 4
  fi
  touch ` + DoorRebootMarker + ` /run/reboot-required
  grep -qx devopsy /run/reboot-required.pkgs 2>/dev/null || echo devopsy >>/run/reboot-required.pkgs
  echo reboot
fi
made=
if ! id -u "$user" >/dev/null 2>&1; then
  useradd -K SUB_UID_COUNT=0 -K SUB_GID_COUNT=0 --create-home --shell /bin/sh --groups "$group" "$user"
  usermod -p '*' "$user"
  made=1
fi
# No subordinate ids: with newuidmap on the host, a user namespace mapping them takes a process
# of the user's out of its groups, and so past the firewall, which goes by its group.
uid=$(id -u "$user")
for f in /etc/subuid /etc/subgid; do
  [ ! -f "$f" ] || sed -i -e "/^$user:/d" -e "/^$uid:/d" "$f"
done
id -nG "$user" | tr ' ' '\n' | grep -qx "$group" || usermod -aG "$group" "$user"
` + doorSSHDTakes(deploy) + `# No cron or at job of the project user's: it runs with no session, so a leaked key's job
# would outlive the key's removal. A cron.allow the admin keeps does not list it anyway.
for f in /etc/cron.deny /etc/at.deny; do
  grep -qxF "$user" "$f" 2>/dev/null || printf '%s\n' "$user" >>"$f"
done
# Nor a user manager of its own lingering with no session, for the same reason.
rm -f "/var/lib/systemd/linger/$user"
home=$(getent passwd "$user" | cut -d: -f6)
chmod 750 "$home"
[ -z "$tree" ] || [ -e "$tree" ] || install -d -m 750 -o "$user" -g "$user" "$tree"
# The project user's own releases root, so its devopsy releases into that tree: written as
# that user, whose home it is, and only when the user is new.
if [ -n "$made" ] && [ -n "$root" ]; then
  runuser -u "$user" -- sh -c 'mkdir -p "$HOME/.config/devopsy" && printf "releases:\n  root: %s\n" "$1" >"$HOME/.config/devopsy/config.yaml"' sh "$root"
fi
`
}
