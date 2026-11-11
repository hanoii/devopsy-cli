package remote

import (
	"fmt"
	"io/fs"
	"maps"
	"net"
	"path"
	"slices"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// devopsy's firewall is its own, one for the server, written and loaded by --server-setup and
// --project-users add through one loader: a chain in raw for what containers send, jumped to
// from PREROUTING, on every server; and with a door, chains in filter for what the project
// users' processes, the door's and Docker's daemon send, jumped to first from OUTPUT. Each is
// in IPv4 and IPv6, its rules written as iptables -S prints them, so the lines loaded are the
// lines compared.
const (
	DoorChain              = "DEVOPSY-PROJECTS"
	FirewallContainerChain = "DEVOPSY-CONTAINERS"
	DoorDaemonChain        = "DEVOPSY-DOCKERD"
	DoorRunChain           = "DEVOPSY-DOOR"
	FirewallScript         = "/usr/local/libexec/devopsy-firewall"
	FirewallUnit           = "/etc/systemd/system/devopsy-firewall.service"
	// FirewallDockerDropIn runs the loader whenever Docker starts, in docker.service's
	// own cgroup: a cgroup's rule binds the cgroup there when it goes in, never its path, and
	// systemd makes docker.service's anew at every start.
	FirewallDockerDropIn = "/etc/systemd/system/docker.service.d/devopsy-firewall.conf"
	// DoorFirewallDaemonMark is the inode of the cgroup the daemon's jump was loaded for.
	DoorFirewallDaemonMark = "/run/devopsy-firewall.dockerd"
	// FirewallMark is what the last load that went in loaded, FirewallContainers or
	// FirewallDoor, written by rename once it did and removed when a load fails: a load
	// under way leaves the last one's, so through a change of kind it says the old kind until
	// the new one is in. The deploy user, who cannot read iptables, judges the firewall by
	// it.
	FirewallMark = "/run/devopsy-firewall.loaded"
	// FirewallMarkEnv moves FirewallMark, and FirewallLockDir beside it, for tests.
	FirewallMarkEnv = "DEVOPSY_INTERNAL_FIREWALL_MARK"
	// FirewallLockDir holds the firewall's lock, a directory of root's that no one else may
	// enter: flock takes an exclusive lock on a file opened for reading, so a lock any user
	// could open is one any user could hold, and no load would run.
	FirewallLockDir = "/run/devopsy-firewall"
	// FirewallWaitEnv is how long, in seconds, a load or a removal waits for the firewall's
	// lock, 60 unset, for tests.
	FirewallWaitEnv = "DEVOPSY_INTERNAL_FIREWALL_WAIT"
)

// xtablesWait is how long, in seconds, an iptables call waits for xtables' lock, which
// iptables-legacy holds for any change (Docker's own, a firewall manager's) and fails without.
const xtablesWait = "10"

// firewallLock is a firewall script's start: the mark's path in $mark, and the firewall's
// lock, in FirewallLockDir (made root's and closed to others when it is not), held on fd 9
// for the rest of the script, one load or removal at a time whoever runs it (the unit, its
// start of Docker, an install): another waits for the one under way to end, a minute at most,
// then goes on over what it left; past the minute it fails. A failure of flock's own is said
// as such.
var firewallLock = `mark=${` + FirewallMarkEnv + `:-` + FirewallMark + `}
lockdir=${mark%/*}/` + path.Base(FirewallLockDir) + `
mkdir -p "$lockdir"
chmod 700 "$lockdir"
exec 9>"$lockdir/lock"
flock -w "${` + FirewallWaitEnv + `:-60}" 9 || {
  rc=$?
  if [ "$rc" -eq 1 ]; then echo "another load of devopsy's firewall held $lockdir/lock for ${` + FirewallWaitEnv + `:-60} s" >&2; else echo "flock could not take $lockdir/lock (exit $rc)" >&2; fi
  exit 1
}
`

// What a loader loads: the containers' chain alone, or the door's chains with it.
const (
	FirewallContainers = "containers"
	FirewallDoor       = "door"
)

// firewallHeader starts each file the firewall's install writes whole, on its first line or,
// after a #!, its second. A removal takes a file there only when it starts with
// devopsysHeader, which every header of devopsy's starts with, an older install's included
// (FirewallPiece, firewallPieceSh).
const (
	firewallHeader = devopsysHeader + ": "
	devopsysHeader = "# Written by devopsy"
)

// FirewallPiece says whether text is a file of the firewall's devopsy wrote whole, by its
// header.
func FirewallPiece(text string) bool {
	first, rest, _ := strings.Cut(text, "\n")
	second, _, _ := strings.Cut(rest, "\n")
	return strings.HasPrefix(first, devopsysHeader) || strings.HasPrefix(first, "#!") && strings.HasPrefix(second, devopsysHeader)
}

// firewallPieceSh is FirewallPiece in a script on the host: devopsys <file> fails unless the
// file is there and devopsy's.
const firewallPieceSh = `devopsys() {
  [ -f "$1" ] || return 1
  l1= l2=
  { IFS= read -r l1 || :; IFS= read -r l2 || :; } <"$1"
  case $l1 in "` + devopsysHeader + `"*) return 0 ;; "#!"*) case $l2 in "` + devopsysHeader + `"*) return 0 ;; esac ;; esac
  return 1
}
`

// firewallFamily is one of iptables' families: its command, how it rejects, and the cloud
// metadata services' addresses in it (door.Metadata4, door.Metadata6, which the door's
// registry rule refuses too). Link-local is never another server's, so a range costs a sync
// nothing there.
type firewallFamily struct {
	cmd, reject, loopback string
	metadata              []string
}

var firewallFamilies = []firewallFamily{
	{"iptables", "icmp-port-unreachable", "127.0.0.0/8", door.Metadata4},
	{"ip6tables", "icmp6-port-unreachable", "::1/128", door.Metadata6},
}

// bridges are Docker's: the default one and every network compose makes.
var bridges = []string{"docker0", "br-+"}

// firewallRules are a family's rules in chain, each as iptables -S prints it after "-A <chain> ".
//
// A project user's process reaches nothing on the host's loopback, which its own addresses go
// out through too: a port a container publishes there is reached through docker-proxy, a
// process of root's whose own connection no rule by group sees. Nor Docker's bridges, where
// every project's containers are, nor a metadata service, which hands out the instance's
// credentials. It goes by interface and by those addresses, never another range: a sync
// reaches another server at any address. Two ways out: the answers to a connection another
// made (a sync's forward on the child is the project user's sshd), and DNS, on loopback or
// at a metadata address (Google's resolver is its metadata server, AWS's 169.254.169.253),
// by the port first asked for, 53, since a NAT may have moved it (Docker's own resolver, at
// 127.0.0.11).
//
// A container reaches no metadata service either, through the host or past it, DNS there
// aside: in raw, before connection tracking and any NAT, so by the port it asks.
func firewallRules(f firewallFamily, chain string) []string {
	var r []string
	dns := func(match, prefix string) {
		for _, p := range []string{"udp", "tcp"} {
			r = append(r, prefix+" -p "+p+" "+match+" -j RETURN")
		}
	}
	switch chain {
	case DoorChain:
		r = append(r, "-m conntrack --ctstate RELATED,ESTABLISHED -j RETURN")
		dns("-m conntrack --ctorigdstport 53", "-o lo")
		for _, m := range f.metadata {
			dns("-m conntrack --ctorigdstport 53", "-d "+m)
		}
		for _, out := range append([]string{"lo"}, bridges...) {
			r = append(r, "-o "+out+" -j REJECT --reject-with "+f.reject)
		}
		for _, m := range f.metadata {
			r = append(r, "-d "+m+" -j REJECT --reject-with "+f.reject)
		}
	case DoorDaemonChain, DoorRunChain:
		// Docker's daemon contacts registries as root (the deploy user's pulls, devopsy's own
		// images), and what the door starts on the host (a claim's read and copy, compose's and
		// buildx's registry clients) answers a registry the project names: either can be
		// sent (a redirect, a token's realm, a name) to the host's loopback or a metadata
		// service. The host's other addresses stay theirs to reach (a registry on this server,
		// by its name or Docker's bridge address, goes out on loopback), and so does DNS on
		// loopback (systemd-resolved). The door's clients reach no container on Docker's
		// bridges either, any project's; the daemon must, its cgroup holding docker-proxy.
		r = append(r, "-m conntrack --ctstate RELATED,ESTABLISHED -j RETURN")
		for _, d := range append([]string{f.loopback}, f.metadata...) {
			dns("-m conntrack --ctorigdstport 53", "-d "+d)
		}
		for _, d := range append([]string{f.loopback}, f.metadata...) {
			r = append(r, "-d "+d+" -j REJECT --reject-with "+f.reject)
		}
		if chain == DoorRunChain {
			for _, out := range bridges {
				r = append(r, "-o "+out+" -j REJECT --reject-with "+f.reject)
			}
		}
	case FirewallContainerChain:
		for _, m := range f.metadata {
			for _, in := range bridges {
				r = append(r, "-d "+m+" -i "+in+" -p udp -m udp --dport 53 -j RETURN", "-d "+m+" -i "+in+" -p tcp -m tcp --dport 53 -j RETURN")
			}
		}
		for _, m := range f.metadata {
			for _, in := range bridges {
				r = append(r, "-d "+m+" -i "+in+" -j DROP")
			}
		}
	}
	return r
}

// firewallJump is where a chain of devopsy's is jumped to from: its table, the built-in chain, and
// the jump as iptables -S prints it after "-A <built-in> ".
type firewallJump struct{ table, from, chain, rule string }

// firewallJumps are the chains' jumps: the containers' from raw's PREROUTING, and withDoor
// the door's, the project users' group by its id (gid), the door's own processes by theirs
// (doorGid, DoorRunGroup's, their effective group) to a chain of their own, as what the door
// starts on the host (compose's and buildx's registry clients, a claim's read) is a
// registry's to send where it likes too, and Docker's daemon by its cgroup (daemonCgroup),
// none when it has none of its own: its cgroup the root, every process's, it would firewall
// the whole host. A jump is as iptables -S prints it, a path quoted.
func firewallJumps(withDoor bool, gid, doorGid, cgroup string) []firewallJump {
	j := []firewallJump{{"raw", "PREROUTING", FirewallContainerChain, "-j " + FirewallContainerChain}}
	if !withDoor {
		return j
	}
	j = append([]firewallJump{
		{"filter", "OUTPUT", DoorChain, "-m owner --gid-owner " + gid + " --suppl-groups -j " + DoorChain},
		{"filter", "OUTPUT", DoorRunChain, "-m owner --gid-owner " + doorGid + " -j " + DoorRunChain},
	}, j...)
	if cgroup != "" && cgroup != "/" {
		j = append(j, firewallJump{"filter", "OUTPUT", DoorDaemonChain, `-m cgroup --path "` + cgroup + `" -j ` + DoorDaemonChain})
	}
	return j
}

// daemonCgroup sets $cg, in a script on the host, to Docker's daemon's cgroup: docker.service's
// under systemd (even before it starts, as at boot), else the running dockerd's.
const daemonCgroup = `cg=$(systemctl show -p ControlGroup --value docker.service 2>/dev/null || :)
if [ -z "$cg" ] && systemctl cat docker.service >/dev/null 2>&1; then
  cg=/$(systemctl show -p Slice --value docker.service 2>/dev/null || echo system.slice)/docker.service
fi
if [ -z "$cg" ] && pid=$(pidof -s dockerd 2>/dev/null); then
  cg=$(sed -n 's/^0:://p' /proc/"$pid"/cgroup)
fi
`

// firewallRestore is a family's chains for iptables-restore --noflush, the door's only with
// withDoor: declaring a chain flushes it, so each loads whole, at once, and nothing else
// changes.
func firewallRestore(f firewallFamily, withDoor bool) string {
	var b strings.Builder
	tables := [][]string{{"raw", FirewallContainerChain}}
	if withDoor {
		tables = append([][]string{{"filter", DoorChain, DoorDaemonChain, DoorRunChain}}, tables...)
	}
	for _, t := range tables {
		b.WriteString("*" + t[0] + "\n")
		for _, c := range t[1:] {
			b.WriteString(":" + c + " - [0:0]\n")
		}
		for _, c := range t[1:] {
			for _, r := range firewallRules(f, c) {
				b.WriteString("-A " + c + " " + r + "\n")
			}
		}
		b.WriteString("COMMIT\n")
	}
	return b.String()
}

// FirewallLoader is FirewallScript, which loads the firewall: at --server-setup and add, at
// every boot through FirewallUnit, and with a door at every start of Docker
// (FirewallDockerDropIn). withDoor loads
// the door's chains too, else the containers' alone. Each jump goes in first, then any other
// copy of it goes, so nothing is ever without it. IPv6 only where the kernel has it. It holds
// the firewall's lock throughout (firewallLock), and FirewallMark says what it loaded once it
// all went in, none before.
func FirewallLoader(withDoor bool) string {
	what := FirewallContainers
	if withDoor {
		what = FirewallDoor
	}
	s := `#!/bin/sh
` + firewallHeader + `its own chains loaded whole and jumped to first
# (` + what + `). Run at boot by devopsy-firewall.service.
set -eu
` + firewallLock + `# Its own file gone (a removal that held the lock it waited for), it loads nothing.
case $0 in /*) [ -e "$0" ] || { echo "$0 is gone: devopsy's firewall was removed, nothing loaded" >&2; exit 0; } ;; esac
# A load that fails leaves no mark: what it loaded may be half.
trap '[ $? -eq 0 ] || rm -f "$mark"' EXIT
`
	if withDoor {
		s += `gid=$(getent group ` + DoorGroup + ` | cut -d: -f3)
[ -n "$gid" ] || { echo "no group ` + DoorGroup + `" >&2; exit 1; }
doorgid=$(getent group ` + DoorRunGroup + ` | cut -d: -f3)
[ -n "$doorgid" ] || { echo "no group ` + DoorRunGroup + `" >&2; exit 1; }
` + daemonCgroup
	}
	s += `# first <iptables> <table> <chain> <rule>: rule at the top of chain, and only there. The rule is
# as iptables -S prints it, a cgroup's path quoted: eval reads the quotes.
first() {
  eval "\"\$1\" -w ` + xtablesWait + ` -t \"\$2\" -I \"\$3\" 1 $4"
  "$1" -w ` + xtablesWait + ` -t "$2" -S "$3" | awk -v r="-A $3 $4" '/^-A /{n++; if ($0 == r && n > 1) print n}' | sort -rn | while read -r n; do
    "$1" -w ` + xtablesWait + ` -t "$2" -D "$3" "$n"
  done
}
`
	if withDoor {
		s += `# stale <iptables>: the jumps to the daemon's chain that are not the current cgroup's go.
stale() {
  "$1" -w ` + xtablesWait + ` -t filter -S OUTPUT | awk -v r="-A OUTPUT -m cgroup --path \"$cg\" -j ` + DoorDaemonChain + `" '/^-A /{n++; if ($0 ~ / -j ` + DoorDaemonChain + `$/ && $0 != r) print n}' | sort -rn | while read -r n; do
    "$1" -w ` + xtablesWait + ` -t filter -D OUTPUT "$n"
  done
}
`
	}
	for _, f := range firewallFamilies {
		load := "printf '%s' " + Quote(firewallRestore(f, withDoor)) + " | " + f.cmd + "-restore -w " + xtablesWait + " --noflush\n"
		for _, j := range firewallJumps(withDoor, "$gid", "$doorgid", "") {
			load += "first " + f.cmd + " " + j.table + " " + j.from + ` "` + strings.ReplaceAll(j.rule, `"`, `\"`) + `"` + "\n"
		}
		if withDoor {
			// The daemon's jump only for a cgroup that is there: at boot this runs before
			// Docker, and Docker's own start runs it again (FirewallDockerDropIn).
			load += `[ -z "$cg" ] || [ "$cg" = / ] || [ ! -d "/sys/fs/cgroup$cg" ] || first ` + f.cmd + ` filter OUTPUT "-m cgroup --path \"$cg\" -j ` + DoorDaemonChain + `"` + "\n"
			load += "stale " + f.cmd + "\n"
		}
		s += familyGuard(f, load, "")
	}
	if withDoor {
		// The cgroup the daemon's jump holds, for FirewallRead to tell one made again since.
		s += `if [ -n "$cg" ] && [ "$cg" != / ] && [ -d "/sys/fs/cgroup$cg" ]; then stat -c %i "/sys/fs/cgroup$cg" >` + DoorFirewallDaemonMark + `; else rm -f ` + DoorFirewallDaemonMark + `; fi
`
	}
	s += `echo ` + what + ` >"$mark.new"
mv -f "$mark.new" "$mark"
`
	return s
}

// firewallDockerText is FirewallDockerDropIn: the loader before Docker's daemon starts, its
// failure never Docker's. Only with a door: the daemon's jump is its cgroup's, which systemd
// makes anew at each start; the containers' chain goes by interface, and its jump, inserted
// first, stays through Docker's start, which adds its own rules to raw's PREROUTING after it.
const firewallDockerText = firewallHeader + `devopsy's firewall loaded at every start of Docker, whose cgroup
# systemd makes anew, which the daemon's jump binds.
[Service]
ExecStartPre=-` + FirewallScript + `
`

// firewallUnitText loads the firewall at boot: after what loads a whole ruleset
// (netfilter-persistent, ufw, firewalld, all before network-pre.target), before the
// network's services, sshd and Docker among them.
const firewallUnitText = firewallHeader + `devopsy's firewall, loaded at boot.
[Unit]
Description=devopsy's firewall
DefaultDependencies=no
After=local-fs.target network-pre.target netfilter-persistent.service ufw.service firewalld.service
Before=network.target docker.service
Wants=network-pre.target

[Service]
Type=oneshot
ExecStart=` + FirewallScript + `

[Install]
WantedBy=multi-user.target
`

// FirewallRead prints, for FirewallWrong, the ids of the project users' group and the
// door's as the loader reads them, then each family's tables as iptables -S does, a
// "# <iptables> <table>" line before each, or "# ip6tables off" where the kernel has no IPv6;
// then the host's TCP listeners and sshd's ports, for DoorListeners.
var FirewallRead = daemonCgroup + `echo "# groups $(getent group ` + DoorGroup + ` | cut -d: -f3) $(getent group ` + DoorRunGroup + ` | cut -d: -f3)"
echo "# dockerd $cg"
echo "# dockerd-cgroup $(stat -c %i "/sys/fs/cgroup$cg" 2>/dev/null || :) $(cat ` + DoorFirewallDaemonMark + ` 2>/dev/null || :)"
` + firewallTablesRead() + `echo "# listeners"
ss -Htlnp 2>/dev/null || :
echo "# sshd ports"
sshd -T 2>/dev/null | awk '$1 == "port" { print $2 }' || :
`

// firewallTablesRead prints each family's tables for FirewallRead.
func firewallTablesRead() string {
	var s string
	for _, f := range firewallFamilies {
		read := ""
		for _, t := range []string{"filter", "raw"} {
			read += `echo "# ` + f.cmd + " " + t + `"; ` + f.cmd + " -w " + xtablesWait + " -t " + t + " -S\n"
		}
		s += familyGuard(f, read, `echo "# `+f.cmd+` off"`+"\n")
	}
	return s
}

// familyGuard is script run for family f only where the kernel has it (IPv6 where
// /proc/net/if_inet6 is there), else otherwise.
func familyGuard(f firewallFamily, script, otherwise string) string {
	if f.cmd != "ip6tables" {
		return script
	}
	s := "if [ -e /proc/net/if_inet6 ]; then\n" + script
	if otherwise != "" {
		s += "else\n" + otherwise
	}
	return s + "fi\n"
}

// FirewallUnitLink enables FirewallUnit at boot.
const FirewallUnitLink = "/etc/systemd/system/multi-user.target.wants/devopsy-firewall.service"

// FirewallRetired are the units an earlier install put that devopsy puts no more, by path,
// each with the link that enabled it: the install and the removal stop them and remove them
// where they are devopsy's by their header, and the link left to one gone
// (firewallRetireSh); the facts read one left as a firewall to load again.
var FirewallRetired = map[string]string{
	// The timer that loaded the firewall again every five minutes.
	"/etc/systemd/system/devopsy-firewall.timer": "/etc/systemd/system/timers.target.wants/devopsy-firewall.timer",
}

// firewallRetireSh stops and removes, in a script on the host, the retired units that are
// devopsy's, and the links left to them.
func firewallRetireSh() string {
	var s string
	for _, u := range slices.Sorted(maps.Keys(FirewallRetired)) {
		l := FirewallRetired[u]
		s += `if devopsys "$host_root` + u + `"; then
  SYSTEMD_IGNORE_CHROOT=1 systemctl stop ` + path.Base(u) + ` >/dev/null 2>&1 || :
  rm -f "$host_root` + u + `"
fi
[ ! -L "$host_root` + l + `" ] || [ -e "$host_root` + u + `" ] || rm -f "$host_root` + l + `"
`
	}
	return s
}

// FirewallFile is one of the files the firewall's install writes whole: its text and mode.
type FirewallFile struct {
	Text string
	Mode fs.FileMode
}

// FirewallFiles are the files the firewall's install writes, by path, for withDoor: the
// loader, its unit and, with a door, the Docker drop-in.
func FirewallFiles(withDoor bool) map[string]FirewallFile {
	files := map[string]FirewallFile{FirewallScript: {FirewallLoader(withDoor), 0o755}, FirewallUnit: {firewallUnitText, 0o644}}
	if withDoor {
		files[FirewallDockerDropIn] = FirewallFile{firewallDockerText, 0o644}
	}
	return files
}

// FirewallInstall is what --server-setup and add do for the firewall as root, chrooted into
// the host with its network, the host's files under its first argument ("" on the host
// itself, a test's directory): the door's groups only withDoor, FirewallFiles when missing or
// different, the unit's boot link, the retired units gone (firewallRetireSh), the firewall
// loaded, then FirewallRead.
//
// The load is the unit's where systemd answers from the chroot (the run has the host's pids),
// its result then the load's own. Where systemd does not answer (no systemd), the loader runs
// here.
func FirewallInstall(withDoor bool) string {
	s := "set -eu\numask 022\nhost_root=${1:-}\n"
	if withDoor {
		s += hostGroup
	}
	s += hostPut + firewallPieceSh
	files := FirewallFiles(withDoor)
	for _, p := range slices.Sorted(maps.Keys(files)) {
		check := "true"
		if files[p].Mode&0o111 != 0 {
			check = `'sh -n "$1"'`
		}
		s += `mkdir -p "$host_root` + path.Dir(p) + `"` + "\n" +
			`put "$host_root` + p + `" ` + fmt.Sprintf("%o", files[p].Mode) + " " + check + " " + Quote(files[p].Text) + " || :\n"
	}
	s += `mkdir -p "$host_root` + path.Dir(FirewallUnitLink) + `"` + "\n" + "ln -sfn " + FirewallUnit + ` "$host_root` + FirewallUnitLink + `"` + "\n" + firewallRetireSh()
	unit := report.Hint("what it said, as root:", "journalctl -u devopsy-firewall.service")
	return s + `if SYSTEMD_IGNORE_CHROOT=1 systemctl daemon-reload >/dev/null 2>&1; then
  if ! SYSTEMD_IGNORE_CHROOT=1 systemctl start devopsy-firewall.service >/dev/null 2>&1; then
    result=$(SYSTEMD_IGNORE_CHROOT=1 systemctl show -p Result --value devopsy-firewall.service 2>/dev/null || :)
    ` + echoLine(report.ErrorLevel, "devopsy-firewall.service did not load devopsy's firewall: ${result:-its result unread}", unit) + `
    exit 5
  fi
else
  "$host_root"` + FirewallScript + `
fi
` + FirewallRead
}

// FirewallRemove is what --server-setup does as root for a server whose containers may reach
// the metadata services (firewall: metadata: allow, no door), the host's files under its first
// argument as FirewallInstall's, holding the firewall's lock (firewallLock): the unit stopped
// first, where systemctl reaches systemd, as a run of it waiting for the lock would load the
// firewall again once the removal let it go (a loader that gets the lock and finds its own
// file gone loads nothing, for one run by other means), then the containers' jump and chain
// out of raw, the files of FirewallFiles that are devopsy's by their header (FirewallPiece),
// the retired units (firewallRetireSh), the boot link left, and the mark. It prints each file
// of FirewallFiles removed. A removal cut halfway is finished by the next: every step goes on
// from what it finds.
func FirewallRemove() string {
	s := "set -eu\nhost_root=${1:-}\n" + firewallLock + firewallPieceSh +
		"SYSTEMD_IGNORE_CHROOT=1 systemctl stop devopsy-firewall.service >/dev/null 2>&1 || :\n"
	for _, f := range firewallFamilies {
		c := f.cmd + " -w " + xtablesWait + " -t raw"
		r := `while ` + c + ` -D PREROUTING -j ` + FirewallContainerChain + ` 2>/dev/null; do :; done
if ` + c + ` -S ` + FirewallContainerChain + ` >/dev/null 2>&1; then ` + c + ` -F ` + FirewallContainerChain + ` && ` + c + ` -X ` + FirewallContainerChain + `; fi
`
		s += familyGuard(f, r, "")
	}
	return s + `for f in ` + strings.Join(slices.Sorted(maps.Keys(FirewallFiles(true))), " ") + `; do
  if devopsys "$host_root$f"; then rm -f "$host_root$f"; echo "removed $f"; fi
done
` + firewallRetireSh() + `[ ! -L "$host_root` + FirewallUnitLink + `" ] || [ -e "$host_root` + FirewallUnit + `" ] || rm -f "$host_root` + FirewallUnitLink + `"
rm -f "$mark"
SYSTEMD_IGNORE_CHROOT=1 systemctl daemon-reload >/dev/null 2>&1 || :
`
}

// FirewallWrong says what of a door's firewall the host lacks, from FirewallRead's output, by
// the ids of the project users' group and the door's it holds: a chain not exactly devopsy's,
// a jump missing, or one behind a rule that may let a packet through (an ACCEPT, another
// chain's jump), which only a DROP or a REJECT may be. Empty when it is all there; an error
// when a group is not there yet.
func FirewallWrong(read string) ([]string, error) {
	var gids []string
	for line := range strings.SplitSeq(read, "\n") {
		if g, ok := strings.CutPrefix(line, "# groups "); ok {
			gids = strings.Split(g, " ")
		}
	}
	for i, g := range []string{DoorGroup, DoorRunGroup} {
		if len(gids) <= i || gids[i] == "" {
			return nil, fmt.Errorf("no %s group yet", g)
		}
	}
	return firewallWrong(read, true, gids[0], gids[1]), nil
}

// ContainersFirewallWrong is FirewallWrong for a server with no door: the containers' chain
// and its jump alone.
func ContainersFirewallWrong(read string) []string {
	return firewallWrong(read, false, "", "")
}

// firewallWrong is what the host lacks of the chains firewallJumps gives for withDoor, the
// daemon's own checked with a door.
func firewallWrong(read string, withDoor bool, gid, doorGid string) []string {
	tables := map[string][]string{}
	var key, cgroup string
	var made []string
	for line := range strings.SplitSeq(read, "\n") {
		if strings.HasPrefix(line, "# groups ") {
			continue
		}
		if c, ok := strings.CutPrefix(line, "# dockerd-cgroup "); ok {
			made = strings.Fields(c)
			continue
		}
		if c, ok := strings.CutPrefix(line, "# dockerd "); ok {
			cgroup = strings.TrimSpace(c)
			continue
		}
		if k, ok := strings.CutPrefix(line, "# "); ok {
			key = k
			tables[key] = []string{}
			continue
		}
		if key != "" && strings.HasPrefix(line, "-A ") {
			tables[key] = append(tables[key], line)
		}
	}
	var wrong []string
	if withDoor && (cgroup == "" || cgroup == "/") {
		wrong = append(wrong, "Docker's daemon has no cgroup of its own, so its pulls and logins for project users reach the host's loopback and metadata addresses")
	}
	// A cgroup there that is not the one the jump was loaded for: Docker restarted since, and
	// the jump, binding the cgroup and printed by its path, holds nothing.
	if withDoor && (len(made) == 2 && made[0] != made[1] || len(made) == 1) {
		wrong = append(wrong, "Docker's daemon runs in a cgroup made since the firewall loaded (a restart of Docker), which its jump does not hold")
	}
	for _, f := range firewallFamilies {
		if _, off := tables[f.cmd+" off"]; off {
			continue
		}
		var checked []string
		for _, j := range firewallJumps(withDoor, gid, doorGid, cgroup) {
			t, ok := tables[f.cmd+" "+j.table]
			if !ok {
				if !slices.Contains(checked, j.table) {
					wrong = append(wrong, f.cmd+": its "+j.table+" table unread")
				}
				checked = append(checked, j.table)
				continue
			}
			var got []string
			for _, line := range t {
				if r, ok := strings.CutPrefix(line, "-A "+j.chain+" "); ok {
					got = append(got, r)
				}
			}
			// A chain two jumps lead to is checked once.
			if !slices.Contains(checked, j.chain) && !slices.Equal(got, firewallRules(f, j.chain)) {
				wrong = append(wrong, f.cmd+": "+j.chain+" is not devopsy's")
			}
			checked = append(checked, j.chain)
			if why := jumpedFirst(t, j); why != "" {
				wrong = append(wrong, f.cmd+": "+why)
			}
		}
	}
	return wrong
}

// jumpedFirst says why a table's built-in chain does not reach j's jump with every packet:
// none there, or a rule before it that is neither a DROP nor a REJECT.
func jumpedFirst(table []string, j firewallJump) string {
	for _, line := range table {
		r, ok := strings.CutPrefix(line, "-A "+j.from+" ")
		if !ok {
			continue
		}
		if r == j.rule {
			return ""
		}
		// A jump to a chain of devopsy's returns what it does not reject.
		f := strings.Fields(r)
		if i := slices.Index(f, "-j"); i < 0 || i+1 >= len(f) || !slices.Contains([]string{"DROP", "REJECT", DoorChain, DoorDaemonChain, DoorRunChain, FirewallContainerChain}, f[i+1]) {
			return j.from + " lets packets through before its jump to " + j.chain + ": " + r
		}
	}
	return "nothing in " + j.from + " jumps to " + j.chain
}

// DoorListeners are the host's TCP listeners beyond loopback, from FirewallRead's output,
// as "<address> (<process>)": every container reaches them through its bridge, which the
// firewall does not close (it would refuse a site's call to another on its own server). Not
// sshd's (its ports, 22 when unread, whoever holds them, systemd's socket included), nor
// docker-proxy's, a published port being public anyway.
func DoorListeners(read string) []string {
	var lines, ports []string
	section := ""
	for line := range strings.SplitSeq(read, "\n") {
		if k, ok := strings.CutPrefix(line, "# "); ok {
			section = k
			continue
		}
		switch section {
		case "listeners":
			lines = append(lines, line)
		case "sshd ports":
			if p := strings.TrimSpace(line); p != "" {
				ports = append(ports, p)
			}
		}
	}
	if len(ports) == 0 {
		ports = []string{"22"}
	}
	var out []string
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) < 4 || f[0] != "LISTEN" {
			continue
		}
		local := f[3]
		i := strings.LastIndex(local, ":")
		if i < 0 {
			continue
		}
		host, port := strings.Trim(local[:i], "[]"), local[i+1:]
		host, _, _ = strings.Cut(host, "%")
		if ip := net.ParseIP(host); (ip != nil && ip.IsLoopback()) || slices.Contains(ports, port) {
			continue
		}
		process := ""
		if len(f) > 5 {
			if _, rest, ok := strings.Cut(f[5], `(("`); ok {
				process, _, _ = strings.Cut(rest, `"`)
			}
		}
		if process == "docker-proxy" {
			continue
		}
		if process != "" {
			local += " (" + process + ")"
		}
		out = append(out, local)
	}
	return out
}
