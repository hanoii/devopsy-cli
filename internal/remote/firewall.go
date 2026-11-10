package remote

import (
	"net"
	"path"
	"slices"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/door"
)

// The project users' firewall is devopsy's own, one for the server, written and loaded by
// --project-users add: a chain in filter for what the project users' processes send, jumped
// to first from OUTPUT by their group, and one in raw for what containers send, jumped to
// from PREROUTING, each in IPv4 and IPv6. Its rules are written as iptables -S prints them,
// so the lines loaded are the lines compared.
const (
	DoorChain          = "DEVOPSY-PROJECTS"
	DoorContainerChain = "DEVOPSY-CONTAINERS"
	DoorDaemonChain    = "DEVOPSY-DOCKERD"
	DoorRunChain       = "DEVOPSY-DOOR"
	DoorFirewallScript = "/usr/local/libexec/devopsy-firewall"
	DoorFirewallUnit   = "/etc/systemd/system/devopsy-firewall.service"
	DoorFirewallTimer  = "/etc/systemd/system/devopsy-firewall.timer"
	// DoorFirewallDockerDropIn runs the loader whenever Docker starts, in docker.service's
	// own cgroup: a cgroup's rule binds the cgroup there when it goes in, never its path, and
	// systemd makes docker.service's anew at every start.
	DoorFirewallDockerDropIn = "/etc/systemd/system/docker.service.d/devopsy-firewall.conf"
	// DoorFirewallDaemonMark is the inode of the cgroup the daemon's jump was loaded for.
	DoorFirewallDaemonMark = "/run/devopsy-firewall.dockerd"
)

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

// doorRules are a family's rules in chain, each as iptables -S prints it after "-A <chain> ".
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
func doorRules(f firewallFamily, chain string) []string {
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
	case DoorContainerChain:
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

// doorJump is where a chain of devopsy's is jumped to from: its table, the built-in chain, and
// the jump as iptables -S prints it after "-A <built-in> ".
type doorJump struct{ table, from, chain, rule string }

// doorJumps are the chains' jumps, the project users' group by its id (gid), the door's
// own processes by theirs (doorGid, DoorRunGroup's, their effective group) to a chain of their
// own, as what the door starts on the host (compose's and buildx's registry clients, a
// claim's read) is a registry's to send where it likes too, and Docker's daemon by its cgroup
// (daemonCgroup), none when it has none of its own: its cgroup the root, every process's, it
// would firewall the whole host. A jump is as iptables -S prints it, a path quoted.
func doorJumps(gid, doorGid, cgroup string) []doorJump {
	j := []doorJump{
		{"filter", "OUTPUT", DoorChain, "-m owner --gid-owner " + gid + " --suppl-groups -j " + DoorChain},
		{"filter", "OUTPUT", DoorRunChain, "-m owner --gid-owner " + doorGid + " -j " + DoorRunChain},
		{"raw", "PREROUTING", DoorContainerChain, "-j " + DoorContainerChain},
	}
	if cgroup != "" && cgroup != "/" {
		j = append(j, doorJump{"filter", "OUTPUT", DoorDaemonChain, `-m cgroup --path "` + cgroup + `" -j ` + DoorDaemonChain})
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

// doorRestore is a family's chains for iptables-restore --noflush: declaring a chain flushes
// it, so each loads whole, at once, and nothing else changes.
func doorRestore(f firewallFamily) string {
	var b strings.Builder
	for _, t := range [][]string{{"filter", DoorChain, DoorDaemonChain, DoorRunChain}, {"raw", DoorContainerChain}} {
		b.WriteString("*" + t[0] + "\n")
		for _, c := range t[1:] {
			b.WriteString(":" + c + " - [0:0]\n")
		}
		for _, c := range t[1:] {
			for _, r := range doorRules(f, c) {
				b.WriteString("-A " + c + " " + r + "\n")
			}
		}
		b.WriteString("COMMIT\n")
	}
	return b.String()
}

// DoorFirewallLoader is DoorFirewallScript, which loads the firewall: at add, and at every
// boot through DoorFirewallUnit. Each jump goes in first, then any other copy of it goes, so
// project users are never without it. IPv6 only where the kernel has it.
func DoorFirewallLoader() string {
	s := `#!/bin/sh
# Written by devopsy (devopsy @<server> --project-users add): the project users' firewall,
# its own chains loaded whole and jumped to first. Run at boot by devopsy-firewall.service,
# and every few minutes by devopsy-firewall.timer.
set -eu
gid=$(getent group ` + DoorGroup + ` | cut -d: -f3)
[ -n "$gid" ] || { echo "no group ` + DoorGroup + `" >&2; exit 1; }
doorgid=$(getent group ` + DoorRunGroup + ` | cut -d: -f3)
[ -n "$doorgid" ] || { echo "no group ` + DoorRunGroup + `" >&2; exit 1; }
` + daemonCgroup + `# first <iptables> <table> <chain> <rule>: rule at the top of chain, and only there. The rule is
# as iptables -S prints it, a cgroup's path quoted: eval reads the quotes.
first() {
  eval "\"\$1\" -t \"\$2\" -I \"\$3\" 1 $4"
  "$1" -t "$2" -S "$3" | awk -v r="-A $3 $4" '/^-A /{n++; if ($0 == r && n > 1) print n}' | sort -rn | while read -r n; do
    "$1" -t "$2" -D "$3" "$n"
  done
}
# stale <iptables>: the jumps to the daemon's chain that are not the current cgroup's go.
stale() {
  "$1" -t filter -S OUTPUT | awk -v r="-A OUTPUT -m cgroup --path \"$cg\" -j ` + DoorDaemonChain + `" '/^-A /{n++; if ($0 ~ / -j ` + DoorDaemonChain + `$/ && $0 != r) print n}' | sort -rn | while read -r n; do
    "$1" -t filter -D OUTPUT "$n"
  done
}
`
	for _, f := range firewallFamilies {
		load := "printf '%s' " + Quote(doorRestore(f)) + " | " + f.cmd + "-restore --noflush\n"
		for _, j := range doorJumps("$gid", "$doorgid", "") {
			load += "first " + f.cmd + " " + j.table + " " + j.from + ` "` + strings.ReplaceAll(j.rule, `"`, `\"`) + `"` + "\n"
		}
		// The daemon's jump only for a cgroup that is there: at boot this runs before Docker,
		// and Docker's own start runs it again (DoorFirewallDockerDropIn).
		load += `[ -z "$cg" ] || [ "$cg" = / ] || [ ! -d "/sys/fs/cgroup$cg" ] || first ` + f.cmd + ` filter OUTPUT "-m cgroup --path \"$cg\" -j ` + DoorDaemonChain + `"` + "\n"
		load += "stale " + f.cmd + "\n"
		if f.cmd == "ip6tables" {
			load = "if [ -e /proc/net/if_inet6 ]; then\n" + load + "fi\n"
		}
		s += load
	}
	// The cgroup the daemon's jump holds, for DoorFirewallRead to tell one made again since.
	s += `if [ -n "$cg" ] && [ "$cg" != / ] && [ -d "/sys/fs/cgroup$cg" ]; then stat -c %i "/sys/fs/cgroup$cg" >` + DoorFirewallDaemonMark + `; else rm -f ` + DoorFirewallDaemonMark + `; fi
`
	return s
}

// doorFirewallDockerText is DoorFirewallDockerDropIn: the loader before Docker's daemon starts,
// its failure never Docker's.
const doorFirewallDockerText = `# Written by devopsy (devopsy @<server> --project-users add): devopsy's firewall loaded at
# every start of Docker, whose cgroup systemd makes anew, which the daemon's jump binds.
[Service]
ExecStartPre=-` + DoorFirewallScript + `
`

// doorFirewallUnitText loads the firewall at boot: after what loads a whole ruleset
// (netfilter-persistent, ufw, firewalld, all before network-pre.target), before the
// network's services, sshd and Docker among them.
const doorFirewallUnitText = `# Written by devopsy (devopsy @<server> --project-users add).
[Unit]
Description=devopsy's firewall for its project users
DefaultDependencies=no
After=local-fs.target network-pre.target netfilter-persistent.service ufw.service firewalld.service
Before=network.target docker.service
Wants=network-pre.target

[Service]
Type=oneshot
ExecStart=` + DoorFirewallScript + `

[Install]
WantedBy=multi-user.target
`

// doorFirewallTimerText loads the firewall again every few minutes: a reload of the server's
// own firewall (netfilter-persistent, firewalld) or an iptables -F drops devopsy's rules with
// no word, and loading them changes nothing while they are there.
const doorFirewallTimerText = `# Written by devopsy (devopsy @<server> --project-users add).
[Unit]
Description=devopsy's firewall for its project users, loaded again

[Timer]
OnBootSec=5min
OnUnitInactiveSec=5min

[Install]
WantedBy=timers.target
`

// DoorFirewallRead prints each family's tables as iptables -S does, for DoorFirewallWrong, a
// "# <iptables> <table>" line before each, or "# ip6tables off" where the kernel has no IPv6;
// then the host's TCP listeners and sshd's ports, for DoorListeners.
const DoorFirewallRead = daemonCgroup + `echo "# dockerd $cg"
echo "# dockerd-cgroup $(stat -c %i "/sys/fs/cgroup$cg" 2>/dev/null || :) $(cat ` + DoorFirewallDaemonMark + ` 2>/dev/null || :)"
for c in iptables ip6tables; do
  if [ "$c" = ip6tables ] && [ ! -e /proc/net/if_inet6 ]; then echo "# $c off"; continue; fi
  for t in filter raw; do echo "# $c $t"; "$c" -t "$t" -S; done
done
echo "# listeners"
ss -Htlnp 2>/dev/null || :
echo "# sshd ports"
sshd -T 2>/dev/null | awk '$1 == "port" { print $2 }' || :
`

// DoorFirewallInstall is what add does for the firewall as root, chrooted into the host
// with its network: the group, the loader and its unit when missing or different, the loader
// run, then DoorFirewallRead.
func DoorFirewallInstall() string {
	return "set -eu\numask 022\n" + hostGroup + hostPut + "mkdir -p " + path.Dir(DoorFirewallScript) + " /etc/systemd/system/multi-user.target.wants\n" +
		"put " + DoorFirewallScript + " 755 'sh -n \"$1\"' " + Quote(DoorFirewallLoader()) + " || :\n" +
		"put " + DoorFirewallUnit + " 644 true " + Quote(doorFirewallUnitText) + " || :\n" +
		"put " + DoorFirewallTimer + " 644 true " + Quote(doorFirewallTimerText) + " || :\n" +
		"mkdir -p " + path.Dir(DoorFirewallDockerDropIn) + "\n" +
		"put " + DoorFirewallDockerDropIn + " 644 true " + Quote(doorFirewallDockerText) + " || :\n" +
		"ln -sfn " + DoorFirewallUnit + " /etc/systemd/system/multi-user.target.wants/devopsy-firewall.service\n" +
		"mkdir -p /etc/systemd/system/timers.target.wants\n" +
		"ln -sfn " + DoorFirewallTimer + " /etc/systemd/system/timers.target.wants/devopsy-firewall.timer\n" +
		DoorFirewallScript + "\n" + DoorFirewallRead
}

// DoorFirewallWrong says what of the project users' firewall the host lacks, from
// DoorFirewallRead's output and the ids of the project users' group and the door's: a chain not exactly devopsy's, a jump
// missing, or one behind a rule that may let a packet through (an ACCEPT, another chain's
// jump), which only a DROP or a REJECT may be. Empty when it is all there.
func DoorFirewallWrong(read, gid, doorGid string) []string {
	tables := map[string][]string{}
	var key, cgroup string
	var made []string
	for line := range strings.SplitSeq(read, "\n") {
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
	if cgroup == "" || cgroup == "/" {
		wrong = append(wrong, "Docker's daemon has no cgroup of its own, so its pulls and logins for project users reach the host's loopback and metadata addresses")
	}
	// A cgroup there that is not the one the jump was loaded for: Docker restarted since, and
	// the jump, binding the cgroup and printed by its path, holds nothing.
	if len(made) == 2 && made[0] != made[1] || len(made) == 1 {
		wrong = append(wrong, "Docker's daemon runs in a cgroup made since the firewall loaded (a restart of Docker), which its jump does not hold")
	}
	for _, f := range firewallFamilies {
		if _, off := tables[f.cmd+" off"]; off {
			continue
		}
		var checked []string
		for _, j := range doorJumps(gid, doorGid, cgroup) {
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
			if !slices.Contains(checked, j.chain) && !slices.Equal(got, doorRules(f, j.chain)) {
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
func jumpedFirst(table []string, j doorJump) string {
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
		if i := slices.Index(f, "-j"); i < 0 || i+1 >= len(f) || !slices.Contains([]string{"DROP", "REJECT", DoorChain, DoorDaemonChain, DoorRunChain, DoorContainerChain}, f[i+1]) {
			return j.from + " lets packets through before its jump to " + j.chain + ": " + r
		}
	}
	return "nothing in " + j.from + " jumps to " + j.chain
}

// DoorListeners are the host's TCP listeners beyond loopback, from DoorFirewallRead's output,
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
