package remote

import (
	"path"
	"slices"
	"strings"
)

// The project users' firewall is devopsy's own, one for the server, written and loaded by
// --project-users add: a chain in filter for what the project users' processes send, jumped
// to first from OUTPUT by their group, and one in raw for what containers send, jumped to
// from PREROUTING, each in IPv4 and IPv6. Its rules are written as iptables -S prints them,
// so the lines loaded are the lines compared.
const (
	DoorChain          = "DEVOPSY-PROJECTS"
	DoorContainerChain = "DEVOPSY-CONTAINERS"
	DoorFirewallScript = "/usr/local/libexec/devopsy-firewall"
	DoorFirewallUnit   = "/etc/systemd/system/devopsy-firewall.service"
	DoorFirewallTimer  = "/etc/systemd/system/devopsy-firewall.timer"
)

// firewallFamily is one of iptables' families: its command, how it rejects, and the cloud
// metadata services' addresses in it. In IPv4, all of link-local (AWS's, Google's, Azure's
// IMDS, DigitalOcean's, Tencent's, Scaleway's, ECS's), then Alibaba's and Azure's WireServer;
// in IPv6, AWS's, Google's and OpenStack's. Link-local is never another server's, so a range
// costs a sync nothing there.
type firewallFamily struct {
	cmd, reject string
	metadata    []string
}

var firewallFamilies = []firewallFamily{
	{"iptables", "icmp-port-unreachable", []string{"169.254.0.0/16", "100.100.100.200/32", "168.63.129.16/32"}},
	{"ip6tables", "icmp6-port-unreachable", []string{"fd00:ec2::254/128", "fd20:ce::254/128", "fe80::a9fe:a9fe/128"}},
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

// doorJumps are both chains' jumps, the project users' group by its id.
func doorJumps(gid string) []doorJump {
	return []doorJump{
		{"filter", "OUTPUT", DoorChain, "-m owner --gid-owner " + gid + " --suppl-groups -j " + DoorChain},
		{"raw", "PREROUTING", DoorContainerChain, "-j " + DoorContainerChain},
	}
}

// doorRestore is a family's chains for iptables-restore --noflush: declaring a chain flushes
// it, so each loads whole, at once, and nothing else changes.
func doorRestore(f firewallFamily) string {
	var b strings.Builder
	for _, t := range [][2]string{{"filter", DoorChain}, {"raw", DoorContainerChain}} {
		b.WriteString("*" + t[0] + "\n:" + t[1] + " - [0:0]\n")
		for _, r := range doorRules(f, t[1]) {
			b.WriteString("-A " + t[1] + " " + r + "\n")
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
# first <iptables> <table> <chain> <rule>: rule at the top of chain, and only there.
first() {
  "$1" -t "$2" -I "$3" 1 $4
  "$1" -t "$2" -S "$3" | awk -v r="-A $3 $4" '/^-A /{n++; if ($0 == r && n > 1) print n}' | sort -rn | while read -r n; do
    "$1" -t "$2" -D "$3" "$n"
  done
}
`
	for _, f := range firewallFamilies {
		load := "printf '%s' " + Quote(doorRestore(f)) + " | " + f.cmd + "-restore --noflush\n"
		for _, j := range doorJumps("$gid") {
			load += "first " + f.cmd + " " + j.table + " " + j.from + ` "` + j.rule + `"` + "\n"
		}
		if f.cmd == "ip6tables" {
			load = "if [ -e /proc/net/if_inet6 ]; then\n" + load + "fi\n"
		}
		s += load
	}
	return s
}

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
// "# <iptables> <table>" line before each, or "# ip6tables off" where the kernel has no IPv6.
const DoorFirewallRead = `for c in iptables ip6tables; do
  if [ "$c" = ip6tables ] && [ ! -e /proc/net/if_inet6 ]; then echo "# $c off"; continue; fi
  for t in filter raw; do echo "# $c $t"; "$c" -t "$t" -S; done
done
`

// DoorFirewallInstall is what add does for the firewall as root, chrooted into the host
// with its network: the group, the loader and its unit when missing or different, the loader
// run, then DoorFirewallRead.
func DoorFirewallInstall() string {
	return "set -eu\numask 022\n" + hostGroup + hostPut + "mkdir -p " + path.Dir(DoorFirewallScript) + " /etc/systemd/system/multi-user.target.wants\n" +
		"put " + DoorFirewallScript + " 755 'sh -n \"$1\"' " + Quote(DoorFirewallLoader()) + " || :\n" +
		"put " + DoorFirewallUnit + " 644 true " + Quote(doorFirewallUnitText) + " || :\n" +
		"put " + DoorFirewallTimer + " 644 true " + Quote(doorFirewallTimerText) + " || :\n" +
		"ln -sfn " + DoorFirewallUnit + " /etc/systemd/system/multi-user.target.wants/devopsy-firewall.service\n" +
		"mkdir -p /etc/systemd/system/timers.target.wants\n" +
		"ln -sfn " + DoorFirewallTimer + " /etc/systemd/system/timers.target.wants/devopsy-firewall.timer\n" +
		DoorFirewallScript + "\n" + DoorFirewallRead
}

// DoorFirewallWrong says what of the project users' firewall the host lacks, from
// DoorFirewallRead's output and the group's id: a chain not exactly devopsy's, a jump
// missing, or one behind a rule that may let a packet through (an ACCEPT, another chain's
// jump), which only a DROP or a REJECT may be. Empty when it is all there.
func DoorFirewallWrong(read, gid string) []string {
	tables := map[string][]string{}
	var key string
	for line := range strings.SplitSeq(read, "\n") {
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
	for _, f := range firewallFamilies {
		if _, off := tables[f.cmd+" off"]; off {
			continue
		}
		for _, j := range doorJumps(gid) {
			t, ok := tables[f.cmd+" "+j.table]
			if !ok {
				wrong = append(wrong, f.cmd+": its "+j.table+" table unread")
				continue
			}
			var got []string
			for _, line := range t {
				if r, ok := strings.CutPrefix(line, "-A "+j.chain+" "); ok {
					got = append(got, r)
				}
			}
			if !slices.Equal(got, doorRules(f, j.chain)) {
				wrong = append(wrong, f.cmd+": "+j.chain+" is not devopsy's")
			}
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
		f := strings.Fields(r)
		if i := slices.Index(f, "-j"); i < 0 || i+1 >= len(f) || (f[i+1] != "DROP" && f[i+1] != "REJECT") {
			return j.from + " lets packets through before its jump to " + j.chain + ": " + r
		}
	}
	return "nothing in " + j.from + " jumps to " + j.chain
}
