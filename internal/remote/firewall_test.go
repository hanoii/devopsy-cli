package remote

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// firewallRead is DoorFirewallRead's output for a host where the project users' firewall is
// loaded, gid 1001, with before put ahead of each jump of devopsy's.
func firewallRead(before string) string {
	var b strings.Builder
	for _, f := range firewallFamilies {
		for _, j := range doorJumps("1001") {
			b.WriteString("# " + f.cmd + " " + j.table + "\n-P " + j.from + " ACCEPT\n-N " + j.chain + "\n")
			if before != "" {
				b.WriteString("-A " + j.from + " " + before + "\n")
			}
			b.WriteString("-A " + j.from + " " + j.rule + "\n-A " + j.from + " -j ufw-before-output\n")
			for _, r := range doorRules(f, j.chain) {
				b.WriteString("-A " + j.chain + " " + r + "\n")
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
	if got := DoorFirewallWrong(firewallRead(""), "1001"); len(got) != 0 {
		t.Errorf("all there: %q", got)
	}
	if got := DoorFirewallWrong(firewallRead("-d 10.0.0.1/32 -j DROP"), "1001"); len(got) != 0 {
		t.Errorf("a DROP ahead: %q", got)
	}
	if got := DoorFirewallWrong(firewallRead("-o eth1 -j REJECT --reject-with icmp-port-unreachable"), "1001"); len(got) != 0 {
		t.Errorf("a REJECT ahead: %q", got)
	}
	for _, ahead := range []string{"-j ACCEPT", "-o lo -j ACCEPT", "-j ufw-track-output", "-j RETURN"} {
		got := DoorFirewallWrong(firewallRead(ahead), "1001")
		if len(got) != 4 || !strings.Contains(got[0], "lets packets through before its jump to "+DoorChain+": "+ahead) {
			t.Errorf("%q ahead of each jump: %q", ahead, got)
		}
	}
	// Another group's jump is not the project users'.
	if got := DoorFirewallWrong(firewallRead(""), "1002"); len(got) != 2 || !strings.Contains(got[0], "OUTPUT") || !strings.Contains(got[1], "ip6tables") {
		t.Errorf("another group's: %q", got)
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
		if got := DoorFirewallWrong(strings.Replace(all, edit[0], edit[1], 1), "1001"); len(got) == 0 {
			t.Errorf("%s: nothing wrong", name)
		}
	}
	// A kernel with no IPv6 has nothing to filter there.
	v4 := all[:strings.Index(all, "# ip6tables")] + "# ip6tables off\n"
	if got := DoorFirewallWrong(v4, "1001"); len(got) != 0 {
		t.Errorf("no IPv6: %q", got)
	}
	if got := DoorFirewallWrong(all[:strings.Index(all, "# ip6tables")], "1001"); len(got) != 2 {
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
