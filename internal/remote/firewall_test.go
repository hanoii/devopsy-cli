package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// testDaemonCgroup is Docker's daemon's cgroup in firewallRead, as systemd gives it.
const testDaemonCgroup = "/system.slice/docker.service"

// firewallRead is FirewallRead's output for a host where the project users' firewall is
// loaded, gid 1001 (the door's 1003), Docker's daemon in testDaemonCgroup, with before put ahead of each jump of
// devopsy's.
func firewallRead(before string) string {
	var b strings.Builder
	b.WriteString("# groups 1001 1003\n# dockerd " + testDaemonCgroup + "\n")
	for _, f := range firewallFamilies {
		for _, table := range []string{"filter", "raw"} {
			var jumps []firewallJump
			for _, j := range firewallJumps(true, "1001", "1003", testDaemonCgroup) {
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
				for _, r := range firewallRules(f, c) {
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
func TestFirewallWrong(t *testing.T) {
	t.Parallel()
	// The groups' ids come from the read, as the loader's root read them.
	if got, err := FirewallWrong(firewallRead("")); err != nil || len(got) != 0 {
		t.Errorf("all there: %q (%v)", got, err)
	}
	for _, groups := range []string{"# groups  1003\n", "# groups 1001 \n", ""} {
		if _, err := FirewallWrong(strings.Replace(firewallRead(""), "# groups 1001 1003\n", groups, 1)); err == nil || !strings.Contains(err.Error(), "group yet") {
			t.Errorf("a group not there (%q): %v", groups, err)
		}
	}
	if got := firewallWrong(firewallRead("-d 10.0.0.1/32 -j DROP"), true, "1001", "1003"); len(got) != 0 {
		t.Errorf("a DROP ahead: %q", got)
	}
	if got := firewallWrong(firewallRead("-o eth1 -j REJECT --reject-with icmp-port-unreachable"), true, "1001", "1003"); len(got) != 0 {
		t.Errorf("a REJECT ahead: %q", got)
	}
	for _, ahead := range []string{"-j ACCEPT", "-o lo -j ACCEPT", "-j ufw-track-output", "-j RETURN"} {
		got := firewallWrong(firewallRead(ahead), true, "1001", "1003")
		if len(got) != 8 || !strings.Contains(got[0], "lets packets through before its jump to "+DoorChain+": "+ahead) {
			t.Errorf("%q ahead of each jump: %q", ahead, got)
		}
	}
	// Another group's jump is not the project users', nor the door's.
	if got := firewallWrong(firewallRead(""), true, "1002", "1003"); len(got) != 2 || !strings.Contains(got[0], "OUTPUT") || !strings.Contains(got[1], "ip6tables") {
		t.Errorf("another group's: %q", got)
	}
	if got := firewallWrong(firewallRead(""), true, "1001", "1004"); len(got) != 2 || !strings.Contains(got[0], "jump to "+DoorRunChain) {
		t.Errorf("another group's for the door: %q", got)
	}
	// A rule of the chain gone, changed or added, IPv6's included.
	all := firewallRead("")
	for name, edit := range map[string][2]string{
		"one gone":       {"-A " + DoorChain + " -o br-+ -j REJECT --reject-with icmp6-port-unreachable\n", ""},
		"one accepting":  {"-o docker0 -j REJECT --reject-with icmp-port-unreachable", "-o docker0 -j ACCEPT"},
		"one added":      {"-A " + FirewallContainerChain + " -d 169.254.0.0/16 -i docker0 -j DROP\n", "-A " + FirewallContainerChain + " -j RETURN\n-A " + FirewallContainerChain + " -d 169.254.0.0/16 -i docker0 -j DROP\n"},
		"its jump gone":  {"-A PREROUTING -j " + FirewallContainerChain + "\n", ""},
		"a table unread": {"# ip6tables raw\n", "# ip6tables nothing\n"},
	} {
		if !strings.Contains(all, edit[0]) {
			t.Fatalf("%s: the read has no %q", name, edit[0])
		}
		if got := firewallWrong(strings.Replace(all, edit[0], edit[1], 1), true, "1001", "1003"); len(got) == 0 {
			t.Errorf("%s: nothing wrong", name)
		}
	}
	// Docker's daemon jumps to its own chain by its cgroup; with no cgroup of its own (the
	// root's, every process's, or none found) nothing covers what it contacts for a project.
	for _, cg := range []string{"/", ""} {
		read := strings.Replace(all, "# dockerd "+testDaemonCgroup+"\n", "# dockerd "+cg+"\n", 1)
		if got := firewallWrong(read, true, "1001", "1003"); len(got) == 0 || !strings.Contains(got[0], "Docker's daemon has no cgroup of its own") {
			t.Errorf("the daemon in %q: %q", cg, got)
		}
	}
	if got := firewallWrong(strings.Replace(all, "-A OUTPUT -m cgroup --path \""+testDaemonCgroup+"\" -j "+DoorDaemonChain+"\n", "", 1), true, "1001", "1003"); len(got) != 1 || !strings.Contains(got[0], DoorDaemonChain) {
		t.Errorf("the daemon's jump gone: %q", got)
	}
	// Docker started again since the loader ran: its cgroup another (another inode under the
	// same path), which the daemon's jump, binding the one it was loaded for, does not hold.
	if got := firewallWrong(strings.Replace(all, "# dockerd "+testDaemonCgroup+"\n", "# dockerd "+testDaemonCgroup+"\n# dockerd-cgroup 4242 4141\n", 1), true, "1001", "1003"); len(got) != 1 || !strings.Contains(got[0], "made since the firewall loaded") {
		t.Errorf("a cgroup made again: %q", got)
	}
	if got := firewallWrong(strings.Replace(all, "# dockerd "+testDaemonCgroup+"\n", "# dockerd "+testDaemonCgroup+"\n# dockerd-cgroup 4242 4242\n", 1), true, "1001", "1003"); len(got) != 0 {
		t.Errorf("the cgroup the jump was loaded for: %q", got)
	}
	// A kernel with no IPv6 has nothing to filter there.
	v4 := all[:strings.Index(all, "# ip6tables")] + "# ip6tables off\n"
	if got := firewallWrong(v4, true, "1001", "1003"); len(got) != 0 {
		t.Errorf("no IPv6: %q", got)
	}
	if got := firewallWrong(all[:strings.Index(all, "# ip6tables")], true, "1001", "1003"); len(got) != 2 {
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
	v4, v6 := firewallRules(firewallFamilies[0], DoorChain), firewallRules(firewallFamilies[1], DoorChain)
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
	in(firewallRules(firewallFamilies[0], DoorDaemonChain), "-m conntrack --ctstate RELATED,ESTABLISHED -j RETURN",
		"-d 127.0.0.0/8 -p udp -m conntrack --ctorigdstport 53 -j RETURN", "-d 127.0.0.0/8 -j REJECT --reject-with icmp-port-unreachable", "-d 169.254.0.0/16 -j REJECT --reject-with icmp-port-unreachable")
	in(firewallRules(firewallFamilies[1], DoorDaemonChain), "-d ::1/128 -p tcp -m conntrack --ctorigdstport 53 -j RETURN", "-d ::1/128 -j REJECT --reject-with icmp6-port-unreachable", "-d fd00:ec2::254/128 -j REJECT --reject-with icmp6-port-unreachable")
	if r := firewallRules(firewallFamilies[0], DoorDaemonChain); slices.ContainsFunc(r, func(s string) bool { return strings.Contains(s, "-o ") }) {
		t.Errorf("the daemon's chain closes an interface: %q", r)
	}
	// Containers: no conntrack yet in raw, and no NAT before it, so the port asked for.
	in(firewallRules(firewallFamilies[0], FirewallContainerChain), "-d 169.254.0.0/16 -i docker0 -p udp -m udp --dport 53 -j RETURN", "-d 169.254.0.0/16 -i br-+ -p tcp -m tcp --dport 53 -j RETURN",
		"-d 169.254.0.0/16 -i docker0 -j DROP", "-d 168.63.129.16/32 -i br-+ -j DROP")
	in(firewallRules(firewallFamilies[1], FirewallContainerChain), "-d fe80::a9fe:a9fe/128 -i br-+ -p udp -m udp --dport 53 -j RETURN", "-d fd00:ec2::254/128 -i br-+ -j DROP", "-d fe80::a9fe:a9fe/128 -i docker0 -j DROP")
}

// The loader and add's script parse, and the loader loads each family's chains whole before
// it jumps to them, IPv6 only where the kernel has it.
func TestFirewallScripts(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]string{"loader": FirewallLoader(true), "install": FirewallInstall(true), "loader without a door": FirewallLoader(false), "install without a door": FirewallInstall(false), "removal": FirewallRemove()} {
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s: %v %s", name, err, out)
		}
	}
	l := FirewallLoader(true)
	restore, jump := strings.Index(l, "| ip6tables-restore -w "+xtablesWait+" --noflush"), strings.Index(l, `first ip6tables filter OUTPUT "-m owner --gid-owner $gid --suppl-groups -j `+DoorChain+`"`)
	if restore < 0 || jump < restore || !strings.Contains(l[:restore], "if [ -e /proc/net/if_inet6 ]; then\nprintf '%s' '*filter\n:"+DoorChain+" - [0:0]\n") {
		t.Errorf("the loader:\n%s", l)
	}
	// Loaded at boot, and every few minutes after: a reload of the server's own firewall
	// (netfilter-persistent, firewalld) or an iptables -F drops devopsy's rules silently. A
	// service that stays active once run would never run again by its timer.
	install := FirewallInstall(true)
	if !strings.Contains(install, "ln -sfn "+FirewallUnit+` "$host_root/etc/systemd/system/multi-user.target.wants/`) || !strings.Contains(install, "ln -sfn "+FirewallTimer+` "$host_root/etc/systemd/system/timers.target.wants/`) {
		t.Errorf("the units not enabled:\n%s", install)
	}
	if strings.Contains(firewallUnitText, "RemainAfterExit") || !strings.Contains(firewallTimerText, "OnUnitInactiveSec=") {
		t.Errorf("the units:\n%s\n%s", firewallUnitText, firewallTimerText)
	}
	// At boot it loads after what loads a whole ruleset, which would drop it, and before the
	// network's services, sshd and Docker among them: no project user logs in ahead of it.
	var after, before []string
	for _, line := range strings.Split(firewallUnitText, "\n") {
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
	// add loads the firewall it wrote before it reads the rules back, as the read is what it
	// checks; and both go by the daemon's cgroup found one way.
	if run, read := strings.LastIndex(install, "\"$host_root\""+FirewallScript+"\n"), strings.Index(install, `echo "# dockerd $cg"`); run < 0 || read < run {
		t.Errorf("the install reads the rules before it loads them:\n%s", install)
	}
	if !strings.Contains(l, daemonCgroup) || !strings.Contains(FirewallRead, daemonCgroup) {
		t.Error("the loader and the read find the daemon's cgroup two ways")
	}
}

// Docker's daemon's cgroup is docker.service's under systemd, before it starts too (at boot
// the firewall loads ahead of Docker), else the running dockerd's own; with neither there is
// none, and the daemon gets no jump (firewallJumps), which --server-info then says.
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
		if j := firewallJumps(true, "1001", "1003", cg); slices.ContainsFunc(j, func(j firewallJump) bool { return strings.Contains(j.rule, "-m cgroup") }) {
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

// At boot the firewall's loader runs before docker.service, whose cgroup is not there yet: it
// loads every chain and jump, IPv6's included, and leaves the daemon's jump for Docker's own
// start (FirewallDockerDropIn), never stopping at it. The kernel takes a cgroup's rule only
// for a cgroup that is there ("RULE_INSERT failed (Invalid argument)", exit 4, on Debian 13's
// iptables 1.8.11), and the loader runs under set -e.
//
// The fake iptables refuses what the kernel refuses: a rule on a cgroup's path, none being
// there. Everything else it takes, and logs.
func TestFirewallLoaderLoadsIPv6BeforeDockersCgroupIsThere(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/proc/net/if_inet6"); err != nil {
		t.Skip("no IPv6 here (macOS has no /proc): the loader loads none")
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	tables := `#!/bin/sh
echo "$(basename "$0") $*" >>` + log + `
case " $* " in *" -I "*" -m cgroup --path "*) echo "$(basename "$0"): RULE_INSERT failed (Invalid argument)" >&2; exit 4 ;; esac
`
	restore := "#!/bin/sh\ncat >/dev/null\necho \"$(basename \"$0\") $*\" >>" + log + "\n"
	for name, body := range map[string]string{
		"getent": `#!/bin/sh
case "$2" in ` + DoorGroup + `) echo "` + DoorGroup + `:x:1001:" ;; ` + DoorRunGroup + `) echo "` + DoorRunGroup + `:x:1003:" ;; *) exit 2 ;; esac
`,
		// docker.service not started yet, as at boot: its unit there, no cgroup. Its slice is one
		// no host has, so its cgroup is not there whatever runs this (a CI runner's own Docker
		// has system.slice/docker.service).
		"systemctl": `#!/bin/sh
case "$1 ${3:-$2}" in "show ControlGroup") echo ;; "show Slice") echo devopsy-test-absent.slice ;; "cat docker.service") ;; *) exit 1 ;; esac
`,
		"pidof":             "#!/bin/sh\nexit 1\n",
		"iptables":          tables,
		"ip6tables":         tables,
		"iptables-restore":  restore,
		"ip6tables-restore": restore,
	} {
		write(t, filepath.Join(bin, name), body)
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", FirewallLoader(true))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + filepath.Join(bin, "mark")}
	out, err := cmd.CombinedOutput()
	data, _ := os.ReadFile(log)
	ran := string(data)
	// The control: IPv4's chains and the project users' jump went in before the daemon's.
	w := " -w " + xtablesWait
	if !strings.Contains(ran, "iptables-restore"+w+" --noflush\n") || !strings.Contains(ran, "iptables"+w+" -t filter -I OUTPUT 1 -m owner --gid-owner 1001 --suppl-groups -j "+DoorChain+"\n") {
		t.Fatalf("the loader did not load IPv4's chains and jump (%v): %s\n%s", err, out, ran)
	}
	if !strings.Contains(ran, "ip6tables-restore"+w+" --noflush\n") || !strings.Contains(ran, "ip6tables"+w+" -t filter -I OUTPUT 1 -m owner --gid-owner 1001 --suppl-groups -j "+DoorChain+"\n") {
		t.Errorf("with docker.service's cgroup not there yet, the loader loaded no IPv6 chain or jump (%v): %s\nwhat it ran:\n%s", err, out, ran)
	}
	// Once it all went in, the mark says it loaded the door's.
	if data, _ := os.ReadFile(filepath.Join(bin, "mark")); err == nil && string(data) != FirewallDoor+"\n" {
		t.Errorf("the mark: %q", data)
	}
}

// On a server with no door the loader loads the containers' chain alone, IPv4's and, where
// the kernel has it, IPv6's, and its jump from raw's PREROUTING, with no group, no cgroup and
// none of the door's chains, each iptables call waiting for xtables' lock; then it marks
// what it loaded, a mark of an earlier load gone while it loads.
func TestFirewallWithoutADoor(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	log, mark := filepath.Join(bin, "log"), filepath.Join(bin, "mark")
	tables := "#!/bin/sh\necho \"$(basename \"$0\") $*\" >>" + log + "\n[ ! -e " + mark + " ] || echo 'a mark while loading' >>" + log + "\n"
	restore := "#!/bin/sh\necho \"$(basename \"$0\") $* $(cat)\" >>" + log + "\n"
	for name, body := range map[string]string{"iptables": tables, "ip6tables": tables, "iptables-restore": restore, "ip6tables-restore": restore} {
		write(t, filepath.Join(bin, name), body)
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := testfile.Flock(bin); err != nil {
		t.Fatal(err)
	}
	l := FirewallLoader(false)
	if strings.Contains(l, DoorChain) || strings.Contains(l, DoorDaemonChain) || strings.Contains(l, DoorRunChain) || strings.Contains(l, "getent") || strings.Contains(l, "systemctl") || strings.Contains(l, DoorFirewallDaemonMark) {
		t.Errorf("the loader without a door holds the door's:\n%s", l)
	}
	write(t, mark, FirewallDoor+"\n")
	cmd := exec.Command("sh", "-c", l)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + mark}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	ran, _ := os.ReadFile(log)
	w := " -w " + xtablesWait
	if !strings.Contains(string(ran), "iptables-restore"+w+" --noflush *raw\n:"+FirewallContainerChain+" - [0:0]\n-A "+FirewallContainerChain+" -d 169.254.0.0/16 -i docker0 -p udp -m udp --dport 53 -j RETURN") ||
		!strings.Contains(string(ran), "iptables"+w+" -t raw -I PREROUTING 1 -j "+FirewallContainerChain+"\n") || strings.Contains(string(ran), "*filter") || strings.Contains(string(ran), "a mark while loading") {
		t.Errorf("what it ran:\n%s", ran)
	}
	if _, err := os.Stat("/proc/net/if_inet6"); err == nil && !strings.Contains(string(ran), "ip6tables"+w+" -t raw -I PREROUTING 1 -j "+FirewallContainerChain+"\n") {
		t.Errorf("no IPv6 jump:\n%s", ran)
	}
	if data, _ := os.ReadFile(mark); string(data) != FirewallContainers+"\n" {
		t.Errorf("the mark: %q", data)
	}
}

// The check without a door reads the containers' chain and its jump alone, never the door's
// or the daemon's cgroup.
func TestContainersFirewallWrong(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for _, f := range firewallFamilies {
		b.WriteString("# " + f.cmd + " filter\n-P OUTPUT ACCEPT\n# " + f.cmd + " raw\n-A PREROUTING -j " + FirewallContainerChain + "\n")
		for _, r := range firewallRules(f, FirewallContainerChain) {
			b.WriteString("-A " + FirewallContainerChain + " " + r + "\n")
		}
	}
	read := "# dockerd \n" + b.String()
	if got := ContainersFirewallWrong(read); len(got) != 0 {
		t.Errorf("loaded: %q", got)
	}
	if got := ContainersFirewallWrong(strings.Replace(read, "-A PREROUTING -j "+FirewallContainerChain+"\n", "", 1)); len(got) != 1 || !strings.Contains(got[0], "nothing in PREROUTING jumps to "+FirewallContainerChain) {
		t.Errorf("its jump gone: %q", got)
	}
	if got := firewallWrong(read, true, "1001", "1003"); len(got) == 0 {
		t.Error("a door's check took the containers' chain alone")
	}
}

// Two loads at once (the timer's and Docker's start, an install's and the timer's) take
// turns on the firewall's lock, so another tool's rule in PREROUTING stays and one jump of
// devopsy's is there after both: each loader deletes the jump's other copies by the numbers
// it read, which a second loader's insert and delete between its read and its delete would
// make another rule's (Docker's own in raw's PREROUTING, a firewall manager's in filter's
// OUTPUT with a door). The fake iptables keeps PREROUTING in a file and holds each read
// until both loaders inserted, and each delete until both read, a second at most: the
// interleaving that loses the rule, which the lock never lets happen.
func TestFirewallLoadersTakeTurns(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	state := t.TempDir()
	other := "-s 10.9.9.9/32 -j DROP"
	tables := `#!/bin/sh
[ "$1" != -w ] || shift 2
s=` + state + `/$(basename "$0")
until mkdir "$s.lock" 2>/dev/null; do sleep 0.01; done
trap 'rmdir "$s.lock"' EXIT
# wait_for <file>: until two lines are in file, a second at most.
wait_for() {
  rmdir "$s.lock"; trap - EXIT
  n=0
  while [ "$(wc -l <"$1" 2>/dev/null || echo 0)" -lt 2 ] && [ "$n" -lt 50 ]; do sleep 0.02; n=$((n + 1)); done
  until mkdir "$s.lock" 2>/dev/null; do sleep 0.01; done
  trap 'rmdir "$s.lock"' EXIT
}
case "$*" in
  "-t raw -I PREROUTING 1 "*)
    shift 5
    { echo "$*"; cat "$s.pre"; } >"$s.new" && mv "$s.new" "$s.pre"
    echo x >>"$s.inserts" ;;
  "-t raw -S PREROUTING")
    wait_for "$s.inserts"
    echo "-P PREROUTING ACCEPT"
    sed 's/^/-A PREROUTING /' "$s.pre"
    echo x >>"$s.reads" ;;
  "-t raw -D PREROUTING "*)
    wait_for "$s.reads"
    [ "$(wc -l <"$s.pre")" -ge "$5" ] || { echo "no rule $5" >&2; exit 1; }
    awk -v n="$5" 'NR != n' "$s.pre" >"$s.new" && mv "$s.new" "$s.pre" ;;
esac
`
	restore := "#!/bin/sh\ncat >/dev/null\n"
	for name, body := range map[string]string{"iptables": tables, "ip6tables": tables, "iptables-restore": restore, "ip6tables-restore": restore} {
		write(t, filepath.Join(bin, name), body)
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := testfile.Flock(bin); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"iptables", "ip6tables"} {
		write(t, filepath.Join(state, c+".pre"), other+"\n")
	}
	var wg sync.WaitGroup
	outs := make([]string, 2)
	errs := make([]error, 2)
	for i := range outs {
		wg.Go(func() {
			cmd := exec.Command("sh", "-c", FirewallLoader(false))
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + filepath.Join(state, "mark")}
			out, err := cmd.CombinedOutput()
			outs[i], errs[i] = string(out), err
		})
	}
	wg.Wait()
	for i := range outs {
		if errs[i] != nil {
			t.Errorf("a loader beside another failed: %v: %s", errs[i], outs[i])
		}
	}
	data, _ := os.ReadFile(filepath.Join(state, "iptables.pre"))
	if got := string(data); got != "-j "+FirewallContainerChain+"\n"+other+"\n" {
		t.Errorf("PREROUTING after two loaders at once, another tool's rule (%s) and one jump wanted:\n%s", other, got)
	}
}

// devopsy's header on a firewall's file reads the same in Go and in the removal's shell: on
// its first line, or on its second after a #!, and nowhere else.
func TestFirewallPiece(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for text, want := range map[string]bool{
		firewallUnitText:                    true,
		FirewallLoader(false):               true,
		firewallDockerText:                  true,
		"[Unit]\n" + firewallHeader + "x\n": false,
		"#!/bin/sh\n# The admin's own.\n" + firewallHeader + "it says further down\n": false,
		"# Written by devopsy.\n": false,
		firewallHeader:            true,
		"":                        false,
	} {
		f := filepath.Join(dir, "piece")
		write(t, f, text)
		err := exec.Command("sh", "-c", firewallPieceSh+`devopsys "$1"`, "sh", f).Run()
		if FirewallPiece(text) != want || (err == nil) != want {
			t.Errorf("%q: Go %v, shell %v, want %v", text, FirewallPiece(text), err == nil, want)
		}
	}
	if err := exec.Command("sh", "-c", firewallPieceSh+`devopsys "$1"`, "sh", filepath.Join(dir, "none")).Run(); err == nil {
		t.Error("a file not there is devopsy's")
	}
}

// The removal, run: under the firewall's lock, the timer stopped before anything else, as it
// would load the firewall again under the removal; the containers' jump and chain out of raw;
// the firewall's files gone only where they carry devopsy's header (an admin's own file at
// one of their paths stays); the timer's link left dangling gone, and so the mark; run
// again, as after a cut, it ends with nothing left to remove and no failure.
func TestFirewallRemove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin, host := filepath.Join(dir, "bin"), filepath.Join(dir, "host")
	// iptables with raw's jump and chain as files: -D fails once the jump is gone, -S once
	// the chain is.
	tables := `#!/bin/sh
[ "$1" = -w ] && shift 2 || { echo "$(basename "$0") $*: no wait for xtables' lock" >>` + dir + `/log; exit 2; }
s=` + dir + `/fw.$(basename "$0")
echo "$(basename "$0") $*" >>` + dir + `/log
case "$*" in
  "-t raw -D PREROUTING -j ` + FirewallContainerChain + `") [ -e "$s.jump" ] || exit 1; rm -f "$s.jump" ;;
  "-t raw -S ` + FirewallContainerChain + `") [ -e "$s.chain" ] || exit 1 ;;
  "-t raw -F ` + FirewallContainerChain + `") ;;
  "-t raw -X ` + FirewallContainerChain + `") rm -f "$s.chain" ;;
  *) exit 2 ;;
esac
`
	for name, body := range map[string]string{"iptables": tables, "ip6tables": tables, "systemctl": "#!/bin/sh\necho \"systemctl $*\" >>" + dir + "/log\n"} {
		write(t, filepath.Join(bin, name), body)
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := testfile.Flock(bin); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"iptables", "ip6tables"} {
		write(t, filepath.Join(dir, "fw."+c+".jump"), "")
		write(t, filepath.Join(dir, "fw."+c+".chain"), "")
	}
	for p, f := range FirewallFiles(true) {
		write(t, filepath.Join(host, p), f.Text)
	}
	// An admin's own unit at the unit's path, and devopsy's header further down in it.
	admins := "[Unit]\nDescription=the admin's own\n" + firewallHeader + "it says further down\n"
	write(t, filepath.Join(host, FirewallUnit), admins)
	timerLink := filepath.Join(host, "/etc/systemd/system/timers.target.wants/devopsy-firewall.timer")
	if err := os.MkdirAll(filepath.Dir(timerLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(host, FirewallTimer), timerLink); err != nil {
		t.Fatal(err)
	}
	mark := filepath.Join(dir, "mark")
	write(t, mark, FirewallContainers+"\n")
	run := func() (string, error) {
		cmd := exec.Command("sh", "-c", FirewallRemove(), "sh", host)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + mark}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run()
	if err != nil {
		t.Fatalf("%v: %s\n%s", err, out, readFile(filepath.Join(dir, "log")))
	}
	for _, p := range []string{FirewallScript, FirewallTimer, FirewallDockerDropIn} {
		if _, err := os.Stat(filepath.Join(host, p)); err == nil || !strings.Contains(out, "removed "+p) {
			t.Errorf("%s left, or not said:\n%s", p, out)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(host, FirewallUnit)); string(data) != admins || strings.Contains(out, "removed "+FirewallUnit) {
		t.Errorf("the admin's own unit removed:\n%s", out)
	}
	if _, err := os.Lstat(timerLink); err == nil {
		t.Error("the timer's link left dangling")
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("the mark left")
	}
	log := readFile(filepath.Join(dir, "log"))
	if stop, del := strings.Index(log, "systemctl stop devopsy-firewall.timer"), strings.Index(log, "-D PREROUTING"); stop < 0 || del < stop || strings.Contains(log, "no wait") {
		t.Errorf("the timer not stopped before the jump goes, or a call with no wait for xtables' lock:\n%s", log)
	}
	for _, c := range []string{"iptables", "ip6tables"} {
		if _, err := os.Stat(filepath.Join(dir, "fw."+c+".chain")); err == nil && (c == "iptables" || fileThere("/proc/net/if_inet6")) {
			t.Errorf("%s's chain left", c)
		}
	}
	if out, err := run(); err != nil || strings.Contains(out, "removed") {
		t.Errorf("run again (%v): %s", err, out)
	}
}

func fileThere(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
