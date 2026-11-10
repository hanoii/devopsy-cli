package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// testDaemonCgroup is Docker's daemon's cgroup in firewallRead, as systemd gives it.
const testDaemonCgroup = "/system.slice/docker.service"

// firewallRead is DoorFirewallRead's output for a host where the project users' firewall is
// loaded, gid 1001 (the door's 1003), Docker's daemon in testDaemonCgroup, with before put ahead of each jump of
// devopsy's.
func firewallRead(before string) string {
	var b strings.Builder
	b.WriteString("# dockerd " + testDaemonCgroup + "\n")
	for _, f := range firewallFamilies {
		for _, table := range []string{"filter", "raw"} {
			var jumps []doorJump
			for _, j := range doorJumps("1001", "1003", testDaemonCgroup) {
				if j.table == table {
					jumps = append(jumps, j)
				}
			}
			var chains []string
			for _, j := range jumps {
				if !slices.Contains(chains, j.chain) {
					chains = append(chains, j.chain)
				}
			}
			b.WriteString("# " + f.cmd + " " + table + "\n-P " + jumps[0].from + " ACCEPT\n")
			for _, c := range chains {
				b.WriteString("-N " + c + "\n")
			}
			if before != "" {
				b.WriteString("-A " + jumps[0].from + " " + before + "\n")
			}
			for _, j := range jumps {
				b.WriteString("-A " + j.from + " " + j.rule + "\n")
			}
			b.WriteString("-A " + jumps[0].from + " -j ufw-before-output\n")
			for _, c := range chains {
				for _, r := range doorRules(f, c) {
					b.WriteString("-A " + c + " " + r + "\n")
				}
			}
		}
	}
	return b.String()
}

// The firewall is devopsy's only when each family's chains are exactly its rules and every
// packet reaches their jumps: a rule ahead of a jump may only drop or reject, since an
// ACCEPT there (ufw's own chain accepting each new connection, say) or another chain's jump
// lets a project user's packet past rules that are all there.
func TestDoorFirewallWrong(t *testing.T) {
	t.Parallel()
	if got := DoorFirewallWrong(firewallRead(""), "1001", "1003"); len(got) != 0 {
		t.Errorf("all there: %q", got)
	}
	if got := DoorFirewallWrong(firewallRead("-d 10.0.0.1/32 -j DROP"), "1001", "1003"); len(got) != 0 {
		t.Errorf("a DROP ahead: %q", got)
	}
	if got := DoorFirewallWrong(firewallRead("-o eth1 -j REJECT --reject-with icmp-port-unreachable"), "1001", "1003"); len(got) != 0 {
		t.Errorf("a REJECT ahead: %q", got)
	}
	for _, ahead := range []string{"-j ACCEPT", "-o lo -j ACCEPT", "-j ufw-track-output", "-j RETURN"} {
		got := DoorFirewallWrong(firewallRead(ahead), "1001", "1003")
		if len(got) != 8 || !strings.Contains(got[0], "lets packets through before its jump to "+DoorChain+": "+ahead) {
			t.Errorf("%q ahead of each jump: %q", ahead, got)
		}
	}
	// Another group's jump is not the project users', nor the door's.
	if got := DoorFirewallWrong(firewallRead(""), "1002", "1003"); len(got) != 2 || !strings.Contains(got[0], "OUTPUT") || !strings.Contains(got[1], "ip6tables") {
		t.Errorf("another group's: %q", got)
	}
	if got := DoorFirewallWrong(firewallRead(""), "1001", "1004"); len(got) != 2 || !strings.Contains(got[0], "jump to "+DoorDaemonChain) {
		t.Errorf("another group's for the door: %q", got)
	}
	// A rule of the chain gone, changed or added, IPv6's included.
	all := firewallRead("")
	for name, edit := range map[string][2]string{
		"one gone":       {"-A " + DoorChain + " -o br-+ -j REJECT --reject-with icmp6-port-unreachable\n", ""},
		"one accepting":  {"-o docker0 -j REJECT --reject-with icmp-port-unreachable", "-o docker0 -j ACCEPT"},
		"one added":      {"-A " + DoorContainerChain + " -d 169.254.0.0/16 -i docker0 -j DROP\n", "-A " + DoorContainerChain + " -j RETURN\n-A " + DoorContainerChain + " -d 169.254.0.0/16 -i docker0 -j DROP\n"},
		"its jump gone":  {"-A PREROUTING -j " + DoorContainerChain + "\n", ""},
		"a table unread": {"# ip6tables raw\n", "# ip6tables nothing\n"},
	} {
		if !strings.Contains(all, edit[0]) {
			t.Fatalf("%s: the read has no %q", name, edit[0])
		}
		if got := DoorFirewallWrong(strings.Replace(all, edit[0], edit[1], 1), "1001", "1003"); len(got) == 0 {
			t.Errorf("%s: nothing wrong", name)
		}
	}
	// Docker's daemon jumps to its own chain by its cgroup; with no cgroup of its own (the
	// root's, every process's, or none found) nothing covers what it contacts for a project.
	for _, cg := range []string{"/", ""} {
		read := strings.Replace(all, "# dockerd "+testDaemonCgroup+"\n", "# dockerd "+cg+"\n", 1)
		if got := DoorFirewallWrong(read, "1001", "1003"); len(got) == 0 || !strings.Contains(got[0], "Docker's daemon has no cgroup of its own") {
			t.Errorf("the daemon in %q: %q", cg, got)
		}
	}
	if got := DoorFirewallWrong(strings.Replace(all, "-A OUTPUT -m cgroup --path \""+testDaemonCgroup+"\" -j "+DoorDaemonChain+"\n", "", 1), "1001", "1003"); len(got) != 1 || !strings.Contains(got[0], DoorDaemonChain) {
		t.Errorf("the daemon's jump gone: %q", got)
	}
	// A kernel with no IPv6 has nothing to filter there.
	v4 := all[:strings.Index(all, "# ip6tables")] + "# ip6tables off\n"
	if got := DoorFirewallWrong(v4, "1001", "1003"); len(got) != 0 {
		t.Errorf("no IPv6: %q", got)
	}
	if got := DoorFirewallWrong(all[:strings.Index(all, "# ip6tables")], "1001", "1003"); len(got) != 2 {
		t.Errorf("IPv6 unread: %q", got)
	}
}

// What the firewall keeps a project user's process and a container off, in both families,
// and the ways out it leaves: a DNS resolver on loopback or at a metadata address (Google's
// resolver is its metadata server, AWS's 169.254.169.253), and the answers to a connection
// another made (a sync's forward is the project user's sshd). The metadata services are all
// of link-local, never another server's, and the clouds' others: Alibaba's, Azure's
// WireServer, OpenStack's and Oracle's in IPv6.
func TestDoorRules(t *testing.T) {
	t.Parallel()
	in := func(rules []string, want ...string) {
		t.Helper()
		last := -1
		for _, w := range want {
			i := slices.Index(rules, w)
			if i < 0 || i < last {
				t.Errorf("no %q after %q in:\n%s", w, want[:max(0, slices.Index(want, w))], strings.Join(rules, "\n"))
				return
			}
			last = i
		}
	}
	v4, v6 := doorRules(firewallFamilies[0], DoorChain), doorRules(firewallFamilies[1], DoorChain)
	in(v4, "-m conntrack --ctstate RELATED,ESTABLISHED -j RETURN",
		"-o lo -p udp -m conntrack --ctorigdstport 53 -j RETURN", "-o lo -p tcp -m conntrack --ctorigdstport 53 -j RETURN",
		"-d 169.254.0.0/16 -p udp -m conntrack --ctorigdstport 53 -j RETURN", "-d 169.254.0.0/16 -p tcp -m conntrack --ctorigdstport 53 -j RETURN",
		"-o lo -j REJECT --reject-with icmp-port-unreachable", "-o docker0 -j REJECT --reject-with icmp-port-unreachable", "-o br-+ -j REJECT --reject-with icmp-port-unreachable",
		"-d 169.254.0.0/16 -j REJECT --reject-with icmp-port-unreachable", "-d 100.100.100.200/32 -j REJECT --reject-with icmp-port-unreachable", "-d 168.63.129.16/32 -j REJECT --reject-with icmp-port-unreachable")
	in(v6, "-m conntrack --ctstate RELATED,ESTABLISHED -j RETURN", "-o lo -p udp -m conntrack --ctorigdstport 53 -j RETURN",
		"-d fd00:ec2::254/128 -p udp -m conntrack --ctorigdstport 53 -j RETURN", "-d fe80::a9fe:a9fe/128 -p tcp -m conntrack --ctorigdstport 53 -j RETURN",
		"-o lo -j REJECT --reject-with icmp6-port-unreachable", "-o br-+ -j REJECT --reject-with icmp6-port-unreachable",
		"-d fd00:ec2::254/128 -j REJECT --reject-with icmp6-port-unreachable", "-d fd20:ce::254/128 -j REJECT --reject-with icmp6-port-unreachable", "-d fe80::a9fe:a9fe/128 -j REJECT --reject-with icmp6-port-unreachable", "-d fd00:c1::a9fe:a9fe/128 -j REJECT --reject-with icmp6-port-unreachable")
	// Docker's daemon: the host's loopback and the metadata services, DNS aside; never the
	// host's other addresses (a registry on this server, by its name or Docker's bridge).
	in(doorRules(firewallFamilies[0], DoorDaemonChain), "-m conntrack --ctstate RELATED,ESTABLISHED -j RETURN",
		"-d 127.0.0.0/8 -p udp -m conntrack --ctorigdstport 53 -j RETURN", "-d 127.0.0.0/8 -j REJECT --reject-with icmp-port-unreachable", "-d 169.254.0.0/16 -j REJECT --reject-with icmp-port-unreachable")
	in(doorRules(firewallFamilies[1], DoorDaemonChain), "-d ::1/128 -p tcp -m conntrack --ctorigdstport 53 -j RETURN", "-d ::1/128 -j REJECT --reject-with icmp6-port-unreachable", "-d fd00:ec2::254/128 -j REJECT --reject-with icmp6-port-unreachable")
	if r := doorRules(firewallFamilies[0], DoorDaemonChain); slices.ContainsFunc(r, func(s string) bool { return strings.Contains(s, "-o ") }) {
		t.Errorf("the daemon's chain closes an interface: %q", r)
	}
	// Containers: no conntrack yet in raw, and no NAT before it, so the port asked for.
	in(doorRules(firewallFamilies[0], DoorContainerChain), "-d 169.254.0.0/16 -i docker0 -p udp -m udp --dport 53 -j RETURN", "-d 169.254.0.0/16 -i br-+ -p tcp -m tcp --dport 53 -j RETURN",
		"-d 169.254.0.0/16 -i docker0 -j DROP", "-d 168.63.129.16/32 -i br-+ -j DROP")
	in(doorRules(firewallFamilies[1], DoorContainerChain), "-d fe80::a9fe:a9fe/128 -i br-+ -p udp -m udp --dport 53 -j RETURN", "-d fd00:ec2::254/128 -i br-+ -j DROP", "-d fe80::a9fe:a9fe/128 -i docker0 -j DROP")
}

// The loader and add's script parse, and the loader loads each family's chains whole before
// it jumps to them, IPv6 only where the kernel has it.
func TestDoorFirewallScripts(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]string{"loader": DoorFirewallLoader(), "install": DoorFirewallInstall()} {
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s: %v %s", name, err, out)
		}
	}
	l := DoorFirewallLoader()
	restore, jump := strings.Index(l, "| ip6tables-restore --noflush"), strings.Index(l, `first ip6tables filter OUTPUT "-m owner --gid-owner $gid --suppl-groups -j `+DoorChain+`"`)
	if restore < 0 || jump < restore || !strings.Contains(l[:restore], "if [ -e /proc/net/if_inet6 ]; then\nprintf '%s' '*filter\n:"+DoorChain+" - [0:0]\n") {
		t.Errorf("the loader:\n%s", l)
	}
	// Loaded at boot, and every few minutes after: a reload of the server's own firewall
	// (netfilter-persistent, firewalld) or an iptables -F drops devopsy's rules silently. A
	// service that stays active once run would never run again by its timer.
	install := DoorFirewallInstall()
	if !strings.Contains(install, "ln -sfn "+DoorFirewallUnit+" /etc/systemd/system/multi-user.target.wants/") || !strings.Contains(install, "ln -sfn "+DoorFirewallTimer+" /etc/systemd/system/timers.target.wants/") {
		t.Errorf("the units not enabled:\n%s", install)
	}
	if strings.Contains(doorFirewallUnitText, "RemainAfterExit") || !strings.Contains(doorFirewallTimerText, "OnUnitInactiveSec=") {
		t.Errorf("the units:\n%s\n%s", doorFirewallUnitText, doorFirewallTimerText)
	}
	// At boot it loads after what loads a whole ruleset, which would drop it, and before the
	// network's services, sshd and Docker among them: no project user logs in ahead of it.
	var after, before []string
	for _, line := range strings.Split(doorFirewallUnitText, "\n") {
		if v, ok := strings.CutPrefix(line, "After="); ok {
			after = strings.Fields(v)
		}
		if v, ok := strings.CutPrefix(line, "Before="); ok {
			before = strings.Fields(v)
		}
	}
	for _, unit := range []string{"netfilter-persistent.service", "ufw.service", "firewalld.service", "network-pre.target"} {
		if !slices.Contains(after, unit) {
			t.Errorf("the unit does not load after %s: After=%q", unit, after)
		}
	}
	for _, unit := range []string{"network.target", "docker.service"} {
		if !slices.Contains(before, unit) {
			t.Errorf("the unit does not load before %s: Before=%q", unit, before)
		}
	}
	// add runs the loader it wrote before it reads the rules back, as the read is what it
	// checks; and both go by the daemon's cgroup found one way.
	if run, read := strings.LastIndex(install, "\n"+DoorFirewallScript+"\n"), strings.Index(install, `echo "# dockerd $cg"`); run < 0 || read < run {
		t.Errorf("the install reads the rules before it loads them:\n%s", install)
	}
	if !strings.Contains(l, daemonCgroup) || !strings.Contains(DoorFirewallRead, daemonCgroup) {
		t.Error("the loader and the read find the daemon's cgroup two ways")
	}
}

// Docker's daemon's cgroup is docker.service's under systemd, before it starts too (at boot
// the firewall loads ahead of Docker), else the running dockerd's own; with neither there is
// none, and the daemon gets no jump (doorJumps), which --server-info then says.
func TestDaemonCgroup(t *testing.T) {
	t.Parallel()
	own := ""
	if data, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if cg, ok := strings.CutPrefix(line, "0::"); ok {
				own = cg
			}
		}
	}
	for name, c := range map[string]struct {
		systemctl, pidof, want string
	}{
		"docker.service running":         {`[ "$1 $2 $3" = "show -p ControlGroup" ] && echo /system.slice/docker.service`, "exit 1", "/system.slice/docker.service"},
		"docker.service not started yet": {`case "$1 ${3:-$2}" in "show ControlGroup") echo ;; "show Slice") echo system.slice ;; "cat docker.service") ;; *) exit 1 ;; esac`, "exit 1", "/system.slice/docker.service"},
		"its slice the admin's":          {`case "$1 ${3:-$2}" in "show ControlGroup") echo ;; "show Slice") echo docker.slice ;; "cat docker.service") ;; *) exit 1 ;; esac`, "exit 1", "/docker.slice/docker.service"},
		"no systemd, dockerd running":    {"exit 1", "echo self", own},
		"no unit of that name":           {`[ "$1" = show ] && echo; [ "$1" != cat ]`, "echo self", own},
		"neither":                        {"exit 1", "exit 1", ""},
	} {
		if c.pidof == "echo self" && own == "" {
			continue // No /proc here (macOS): a running dockerd's cgroup is read on Linux.
		}
		bin := t.TempDir()
		write(t, filepath.Join(bin, "systemctl"), "#!/bin/sh\n"+c.systemctl+"\n")
		write(t, filepath.Join(bin, "pidof"), "#!/bin/sh\n"+c.pidof+"\n")
		for _, f := range []string{"systemctl", "pidof"} {
			if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("sh", "-c", "set -eu\n"+daemonCgroup+`printf '%s' "$cg"`)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		if err != nil || string(out) != c.want {
			t.Errorf("%s: %q (%v), want %q", name, out, err, c.want)
		}
	}
	for _, cg := range []string{"", "/"} {
		if j := doorJumps("1001", "1003", cg); slices.ContainsFunc(j, func(j doorJump) bool { return strings.Contains(j.rule, "-m cgroup") }) {
			t.Errorf("a daemon in the cgroup %q, every process's, gets a jump: %+v", cg, j)
		}
	}
}

// What every container reaches on the host beyond loopback is a listener there that is
// neither sshd's (on its ports, under systemd's socket too) nor docker-proxy's, a published
// port being public anyway: those --server-info names, TCP only.
func TestDoorListeners(t *testing.T) {
	t.Parallel()
	read := "# iptables filter\n-P OUTPUT ACCEPT\n# listeners\n" +
		"LISTEN 0      4096         0.0.0.0:443        0.0.0.0:*    users:((\"docker-proxy\",pid=1234,fd=7))\n" +
		"LISTEN 0      128          0.0.0.0:22         0.0.0.0:*    users:((\"sshd\",pid=812,fd=3))\n" +
		"LISTEN 0      4096               *:2222             *:*    users:((\"systemd\",pid=1,fd=50))\n" +
		"LISTEN 0      4096       127.0.0.1:8080       0.0.0.0:*    users:((\"docker-proxy\",pid=1240,fd=7))\n" +
		"LISTEN 0      4096   127.0.0.53%lo:53         0.0.0.0:*    users:((\"systemd-resolve\",pid=600,fd=15))\n" +
		"LISTEN 0      4096           [::1]:9000          [::]:*    users:((\"php-fpm\",pid=700,fd=6))\n" +
		"LISTEN 0      244          0.0.0.0:5432       0.0.0.0:*    users:((\"postgres\",pid=900,fd=5))\n" +
		"LISTEN 0      4096      172.17.0.1:9100       0.0.0.0:*    users:((\"node_exporter\",pid=950,fd=3))\n" +
		"LISTEN 0      4096            [::]:6379          [::]:*\n" +
		"# sshd ports\n2222\n22\n"
	want := []string{"0.0.0.0:5432 (postgres)", "172.17.0.1:9100 (node_exporter)", "[::]:6379"}
	if got := DoorListeners(read); !slices.Equal(got, want) {
		t.Errorf("listeners: %q, want %q", got, want)
	}
	if got := DoorListeners("# listeners\n# sshd ports\n22\n"); len(got) != 0 {
		t.Errorf("none: %q", got)
	}
	// sshd's ports unread (its config), port 22 is sshd's.
	if got := DoorListeners("# listeners\nLISTEN 0 128 0.0.0.0:22 0.0.0.0:*\n"); len(got) != 0 {
		t.Errorf("sshd's default port: %q", got)
	}
}
