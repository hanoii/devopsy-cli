package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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
	// Loaded at boot; a unit that stayed active once run would never start again, by hand or
	// by an install.
	install := FirewallInstall(true)
	if !strings.Contains(install, "ln -sfn "+FirewallUnit+` "$host_root`+FirewallUnitLink+`"`) {
		t.Errorf("the unit not enabled:\n%s", install)
	}
	if strings.Contains(firewallUnitText, "RemainAfterExit") {
		t.Errorf("the unit:\n%s", firewallUnitText)
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
// port being public anyway: those --server-info names, TCP only. The listeners are the read's
// last section, all of it: a line a process's name forges there is nothing else.
func TestDoorListeners(t *testing.T) {
	t.Parallel()
	read := "# iptables filter\n-P OUTPUT ACCEPT\n# sshd ports\n2222\n22\n# listeners\n" +
		"LISTEN 0      4096         0.0.0.0:443        0.0.0.0:*    users:((\"docker-proxy\",pid=1234,fd=7))\n" +
		"LISTEN 0      128          0.0.0.0:22         0.0.0.0:*    users:((\"sshd\",pid=812,fd=3))\n" +
		"LISTEN 0      4096               *:2222             *:*    users:((\"systemd\",pid=1,fd=50))\n" +
		"LISTEN 0      4096       127.0.0.1:8080       0.0.0.0:*    users:((\"docker-proxy\",pid=1240,fd=7))\n" +
		"LISTEN 0      4096   127.0.0.53%lo:53         0.0.0.0:*    users:((\"systemd-resolve\",pid=600,fd=15))\n" +
		"LISTEN 0      4096           [::1]:9000          [::]:*    users:((\"php-fpm\",pid=700,fd=6))\n" +
		"LISTEN 0      244          0.0.0.0:5432       0.0.0.0:*    users:((\"postgres\",pid=900,fd=5))\n" +
		"LISTEN 0      4096      172.17.0.1:9100       0.0.0.0:*    users:((\"node_exporter\",pid=950,fd=3))\n" +
		"LISTEN 0      4096            [::]:6379          [::]:*\n" +
		"# sshd ports\n5432\n"
	want := []string{"0.0.0.0:5432 (postgres)", "172.17.0.1:9100 (node_exporter)", "[::]:6379"}
	if got := DoorListeners(read); !slices.Equal(got, want) {
		t.Errorf("listeners: %q, want %q", got, want)
	}
	if got := DoorListeners("# sshd ports\n22\n# listeners\n"); len(got) != 0 {
		t.Errorf("none: %q", got)
	}
	// sshd's ports unread (its config), port 22 is sshd's.
	if got := DoorListeners("# sshd ports\n# listeners\nLISTEN 0 128 0.0.0.0:22 0.0.0.0:*\n"); len(got) != 0 {
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
	if data, _ := os.ReadFile(filepath.Join(bin, "mark")); string(data) != FirewallDoor+"\n" {
		t.Errorf("the mark: %q", data)
	}
}

// On a server with no door the loader loads the containers' chain alone, IPv4's and, where
// the kernel has it, IPv6's, and its jump from raw's PREROUTING, with no group, no cgroup and
// none of the door's chains, each iptables call waiting for xtables' lock; then it marks
// what it loaded.
func TestFirewallWithoutADoor(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	log, mark := filepath.Join(bin, "log"), filepath.Join(bin, "mark")
	tables := "#!/bin/sh\necho \"$(basename \"$0\") $*\" >>" + log + "\n"
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
		!strings.Contains(string(ran), "iptables"+w+" -t raw -I PREROUTING 1 -j "+FirewallContainerChain+"\n") || strings.Contains(string(ran), "*filter") {
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
	wiped := strings.Replace(read, "-A PREROUTING -j "+FirewallContainerChain+"\n", "", 1)
	if got := ContainersFirewallWrong(wiped); len(got) != 1 || !strings.Contains(got[0], "nothing in PREROUTING jumps to "+FirewallContainerChain) {
		t.Errorf("its jump gone: %q", got)
	}
	// A process's name in the listeners' section, the read's last, forging the jump: read as
	// no jump still.
	forged := wiped + "# listeners\nLISTEN 0 1 0.0.0.0:1 0.0.0.0:* users:((\"x\n# iptables raw\n-A PREROUTING -j " + FirewallContainerChain + "\",pid=1,fd=3))\n"
	if got := ContainersFirewallWrong(forged); len(got) != 1 {
		t.Errorf("a forged jump after the listeners counted: %q", got)
	}
	if got := firewallWrong(read, true, "1001", "1003"); len(got) == 0 {
		t.Error("a door's check took the containers' chain alone")
	}
}

// rawFirewallFakes writes, in bin, an iptables and its restore for each family that keep
// raw's jump and chain as files in state (fw.<cmd>.jump, fw.<cmd>.chain), each call noted in
// state/calls, and the flock a test needs where the system has none. With underLock, each
// call also tries the firewall's lock beside state's mark, without waiting, and notes in
// state/unlocked a call it got it for: one made outside the lock.
func rawFirewallFakes(t *testing.T, bin, state string, underLock bool) {
	t.Helper()
	check := ""
	if underLock {
		check = `if { flock -w 0 8; } 8>"` + state + `/devopsy-firewall/lock"; then echo "$(basename "$0") $*" >>"` + state + `/unlocked"; fi
`
	}
	tables := `#!/bin/sh
[ "$1" != -w ] || shift 2
` + check + `echo "$(basename "$0") $*" >>"` + state + `/calls"
s=` + state + `/fw.$(basename "$0")
case "$*" in
  "-t raw -I PREROUTING 1 -j ` + FirewallContainerChain + `") : >"$s.jump" ;;
  "-t raw -S PREROUTING") echo "-P PREROUTING ACCEPT"; [ ! -e "$s.jump" ] || echo "-A PREROUTING -j ` + FirewallContainerChain + `" ;;
  "-t raw -D PREROUTING -j ` + FirewallContainerChain + `") [ -e "$s.jump" ] || exit 1; rm -f "$s.jump" ;;
  "-t raw -S ` + FirewallContainerChain + `") [ -e "$s.chain" ] || exit 1 ;;
  "-t raw -F ` + FirewallContainerChain + `") ;;
  "-t raw -X ` + FirewallContainerChain + `") rm -f "$s.chain" ;;
esac
`
	restore := "#!/bin/sh\ncat >/dev/null\n" + check + ": >" + state + "/fw.$(basename \"$0\" -restore).chain\n"
	for name, body := range map[string]string{"iptables": tables, "ip6tables": tables, "iptables-restore": restore, "ip6tables-restore": restore} {
		write(t, filepath.Join(bin, name), body)
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := testfile.Flock(bin); err != nil {
		t.Fatal(err)
	}
}

// Every iptables call of a load runs under the firewall's lock, two loads at once taking
// turns: a load deletes its jump's other copies by the numbers it read, which another load's
// insert or delete in between would make another tool's rule (Docker's own in raw's
// PREROUTING). The fake iptables tries the lock at each call, never waiting, and notes a call
// it got it for.
func TestFirewallLoadersTakeTurns(t *testing.T) {
	t.Parallel()
	bin, state := t.TempDir(), t.TempDir()
	rawFirewallFakes(t, bin, state, true)
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
	if calls := readFile(filepath.Join(state, "calls")); strings.Count(calls, "-I PREROUTING") < 2 {
		t.Fatalf("the control: the two loads made %q", calls)
	}
	if u := readFile(filepath.Join(state, "unlocked")); u != "" {
		t.Errorf("calls made outside the firewall's lock:\n%s", u)
	}
}

// The firewall's lock is root's alone to reach: flock takes an exclusive lock on a file
// opened for reading, so a lock file others may open is one any user of the server (a
// project user among them) holds for as long as it likes, every load failing meanwhile, and
// Docker's start, with a door, delayed and its daemon's jump left on the old cgroup. The lock
// lives in a directory of its own that only its owner enters, whatever the umask (systemd's
// 022 here); the mark beside it stays readable, the deploy user's to judge by.
func TestFirewallLockIsRootsAlone(t *testing.T) {
	t.Parallel()
	bin, state := t.TempDir(), t.TempDir()
	rawFirewallFakes(t, bin, state, false)
	mark := filepath.Join(state, "mark")
	cmd := exec.Command("sh", "-c", "umask 022\n"+FirewallLoader(false))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + mark}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	// The control: reading the file is enough to hold its lock.
	lock := filepath.Join(state, filepath.Base(FirewallLockDir), "lock")
	f, err := os.OpenFile(lock, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("a lock file opened for reading takes no exclusive lock here: %v", err)
	}
	f.Close()
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		fi, err := os.Lstat(filepath.Join(state, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case e.Name() == filepath.Base(mark):
			if fi.Mode().Perm()&0o004 == 0 {
				t.Errorf("the mark is %v: the deploy user reads it", fi.Mode().Perm())
			}
		case e.Name() == filepath.Base(FirewallLockDir):
			if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
				t.Errorf("the lock's directory is %v: another user reaches the lock", fi.Mode())
			}
		case strings.HasPrefix(e.Name(), "fw.") || e.Name() == "calls":
		default:
			t.Errorf("%s left beside the mark, %v", e.Name(), fi.Mode())
		}
	}
	// A directory there already, opened to others, is closed again.
	if err := os.Chmod(filepath.Join(state, filepath.Base(FirewallLockDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("sh", "-c", FirewallLoader(false))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + mark}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if fi, err := os.Stat(filepath.Join(state, filepath.Base(FirewallLockDir))); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the lock's directory opened to others stays so: %v (%v)", fi.Mode(), err)
	}
}

// A load that cannot take the lock within its bound fails, saying the lock was held and for
// how long, its mark left as the last load wrote it; flock failing for another reason says
// that instead.
func TestFirewallLoaderLockWait(t *testing.T) {
	t.Parallel()
	bin, state := t.TempDir(), t.TempDir()
	rawFirewallFakes(t, bin, state, false)
	mark := filepath.Join(state, "mark")
	write(t, mark, FirewallContainers+"\n")
	if err := os.MkdirAll(filepath.Join(state, filepath.Base(FirewallLockDir)), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(state, filepath.Base(FirewallLockDir), "lock"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	run := func(path string) (string, error) {
		cmd := exec.Command("sh", "-c", FirewallLoader(false))
		cmd.Env = []string{"PATH=" + path, FirewallMarkEnv + "=" + mark, FirewallWaitEnv + "=0"}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run(bin + ":/usr/bin:/bin")
	if err == nil || !strings.Contains(out, "another load of devopsy's firewall held "+filepath.Join(state, filepath.Base(FirewallLockDir), "lock")+" for 0 s") || readFile(mark) != FirewallContainers+"\n" || fileThere(filepath.Join(state, "calls")) {
		t.Errorf("a lock held (%v): %s, the mark %q", err, out, readFile(mark))
	}
	other := t.TempDir()
	write(t, filepath.Join(other, "flock"), "#!/bin/sh\nexit 66\n")
	if err := os.Chmod(filepath.Join(other, "flock"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := run(other + ":" + bin + ":/usr/bin:/bin"); err == nil || !strings.Contains(out, "flock could not take") || !strings.Contains(out, "(exit 66)") {
		t.Errorf("flock failing otherwise (%v): %s", err, out)
	}
}

// A load leaves the last good mark while it runs, written by rename once it went in, and
// none when it fails: a check made while another load runs reads the firewall loaded.
func TestFirewallLoaderMarks(t *testing.T) {
	t.Parallel()
	bin, state := t.TempDir(), t.TempDir()
	rawFirewallFakes(t, bin, state, false)
	mark := filepath.Join(state, "mark")
	write(t, mark, FirewallDoor+"\n")
	// The mark as the load's own restore finds it: the last load's, until this one is in.
	write(t, filepath.Join(bin, "iptables-restore"), "#!/bin/sh\ncat >/dev/null\ncat "+mark+" >"+state+"/seen\n")
	if err := os.Chmod(filepath.Join(bin, "iptables-restore"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", FirewallLoader(false))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + mark}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if seen, now := readFile(filepath.Join(state, "seen")), readFile(mark); seen != FirewallDoor+"\n" || now != FirewallContainers+"\n" {
		t.Errorf("the mark while loading %q, after %q", seen, now)
	}
	write(t, filepath.Join(bin, "iptables-restore"), "#!/bin/sh\nexit 1\n")
	cmd = exec.Command("sh", "-c", FirewallLoader(false))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + mark}
	if out, err := cmd.CombinedOutput(); err == nil || fileThere(mark) {
		t.Errorf("a load that failed (%v) left its mark %q: %s", err, readFile(mark), out)
	}
}

// A removal leaves nothing loaded, a load waiting for its lock included: it stops the unit
// while it holds the lock, and a loader that still gets the lock after it (one started by
// other means, by hand) finds its own file gone and loads nothing. The fake systemctl starts
// such a loader when the unit is stopped, not stopping it, and holds the removal until that
// loader asked for the lock.
func TestFirewallRemovalLeavesNoLoadBehind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin, host, state := filepath.Join(dir, "bin"), filepath.Join(dir, "host"), filepath.Join(dir, "state")
	for _, d := range []string{bin, state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rawFirewallFakes(t, bin, state, false)
	// flock noting each call, then the real one (the system's, or the test's own).
	realFlock := filepath.Join(bin, "flock.real")
	if err := os.Rename(filepath.Join(bin, "flock"), realFlock); err != nil {
		realFlock = "flock"
		for _, d := range []string{"/usr/bin", "/bin"} {
			if _, err := os.Stat(filepath.Join(d, "flock")); err == nil {
				realFlock = filepath.Join(d, "flock")
			}
		}
	}
	loader := filepath.Join(host, FirewallScript)
	for name, body := range map[string]string{
		"flock": "#!/bin/sh\necho x >>" + state + "/flocks\nexec " + realFlock + " \"$@\"\n",
		"systemctl": `#!/bin/sh
echo "systemctl $*" >>` + state + `/systemctl.log
case "$*" in
  "stop devopsy-firewall.service")
    # As systemd starts it: none of the removal's descriptors, its lock's among them.
    ( exec 9>&-; sh ` + loader + ` >` + state + `/loader.out 2>&1; : >` + state + `/loader.done ) &
    n=0
    while [ "$(wc -l <` + state + `/flocks)" -lt 2 ] && [ "$n" -lt 3000 ]; do sleep 0.02; n=$((n + 1)); done ;;
esac
`,
	} {
		write(t, filepath.Join(bin, name), body)
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, f := range FirewallFiles(false) {
		write(t, filepath.Join(host, p), f.Text)
	}
	mark := filepath.Join(state, "mark")
	write(t, mark, FirewallContainers+"\n")
	for _, c := range []string{"iptables", "ip6tables"} {
		write(t, filepath.Join(state, "fw."+c+".jump"), "")
		write(t, filepath.Join(state, "fw."+c+".chain"), "")
	}
	cmd := exec.Command("sh", "-c", FirewallRemove(), "sh", host)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + mark}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the removal: %v: %s", err, out)
	}
	for start := time.Now(); !fileThere(filepath.Join(state, "loader.done")); time.Sleep(20 * time.Millisecond) {
		if time.Since(start) > startWait {
			t.Fatal("the waiting loader never ended")
		}
	}
	out := readFile(filepath.Join(state, "loader.out"))
	if fileThere(filepath.Join(state, "fw.iptables.jump")) || fileThere(filepath.Join(state, "fw.iptables.chain")) || fileThere(mark) {
		t.Errorf("after a removal, a loader that waited for its lock loaded the firewall again: jump %v, chain %v, mark %v\n%s",
			fileThere(filepath.Join(state, "fw.iptables.jump")), fileThere(filepath.Join(state, "fw.iptables.chain")), fileThere(mark), out)
	}
	if l := readFile(filepath.Join(state, "systemctl.log")); !strings.Contains(l, "systemctl stop devopsy-firewall.service\n") {
		t.Errorf("the removal stopped no unit:\n%s", l)
	}
}

// devopsy's header on a firewall's file reads the same in Go and in the removal's shell: on
// its first line, or on its second after a #!, and nowhere else, an older install's header
// too.
func TestFirewallPiece(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for text, want := range map[string]bool{
		firewallUnitText:                    true,
		FirewallLoader(false):               true,
		firewallDockerText:                  true,
		"[Unit]\n" + firewallHeader + "x\n": false,
		"#!/bin/sh\n# The admin's own.\n" + firewallHeader + "it says further down\n": false,
		"# Written by devopsy (devopsy @<server> --project-users add).\n":             true,
		"# Written by someone.\n": false,
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

// The removal, run: under the firewall's lock, the unit stopped before anything else, as a run
// of it would load the firewall again; the containers' jump and chain out of raw; the
// firewall's files gone only where they carry devopsy's header (an admin's own file at one
// of their paths stays); a retired timer an older install put stopped and gone, with its
// link, and so the mark, one half written, and the lock's directory; run again, as after a cut, it ends with nothing left to remove and
// no failure.
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
	timer := "/etc/systemd/system/devopsy-firewall.timer"
	write(t, filepath.Join(host, timer), "# Written by devopsy (devopsy @<server> --project-users add).\n[Timer]\n")
	timerLink := filepath.Join(host, FirewallRetired[timer])
	if err := os.MkdirAll(filepath.Dir(timerLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(timer, timerLink); err != nil {
		t.Fatal(err)
	}
	mark := filepath.Join(dir, "mark")
	write(t, mark, FirewallContainers+"\n")
	write(t, mark+".new", FirewallContainers+"\n")
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
	if fileThere(filepath.Join(host, timer)) || !strings.Contains(readFile(filepath.Join(dir, "log")), "systemctl stop devopsy-firewall.timer\n") {
		t.Error("the retired timer left, or not stopped")
	}
	for _, p := range []string{FirewallScript, FirewallDockerDropIn} {
		if _, err := os.Stat(filepath.Join(host, p)); err == nil || !strings.Contains(out, "removed "+p) {
			t.Errorf("%s left, or not said:\n%s", p, out)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(host, FirewallUnit)); string(data) != admins || strings.Contains(out, "removed "+FirewallUnit) {
		t.Errorf("the admin's own unit removed:\n%s", out)
	}
	if _, err := os.Lstat(timerLink); err == nil {
		t.Error("the retired timer's link left")
	}
	if fileThere(mark) || fileThere(mark+".new") || fileThere(filepath.Join(dir, filepath.Base(FirewallLockDir))) {
		t.Error("the mark, a mark half written, or the lock's directory left")
	}
	log := readFile(filepath.Join(dir, "log"))
	if stop, del := strings.Index(log, "systemctl stop devopsy-firewall.service"), strings.Index(log, "-D PREROUTING"); stop < 0 || del < stop || strings.Contains(log, "no wait") {
		t.Errorf("the unit not stopped before the jump goes, or a call with no wait for xtables' lock:\n%s", log)
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

// The install loads by the unit, failing with words of its own when it does not: the unit's
// result and where systemd logged why.
func TestFirewallInstallByItsUnit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin, host := filepath.Join(dir, "bin"), filepath.Join(dir, "host")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	rawFirewallFakes(t, bin, dir, false)
	write(t, filepath.Join(bin, "systemctl"), `#!/bin/sh
case "$*" in
  "start devopsy-firewall.service") exit 1 ;;
  "show -p Result --value devopsy-firewall.service") echo exit-code ;;
esac
`)
	if err := os.Chmod(filepath.Join(bin, "systemctl"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", FirewallInstall(false), "sh", host)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + filepath.Join(dir, "mark")}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	_ = cmd.Run()
	want := "devopsy: error: devopsy-firewall.service did not load devopsy's firewall: exit-code\ndevopsy: | what it said, as root:\ndevopsy: | $ journalctl -u devopsy-firewall.service\n"
	if code := cmd.ProcessState.ExitCode(); code != 5 || !strings.Contains(stderr.String(), want) {
		t.Errorf("(%d): %s", code, stderr.String())
	}
}

// A piece the install cannot write (a full disk, a read-only file system) ends it there with
// put's exit 5, its `|| :` taking only put's "same text": nothing loads a loader not written.
func TestFirewallInstallStopsAtAPieceNotWritten(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin, host := filepath.Join(dir, "bin"), filepath.Join(dir, "host")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	rawFirewallFakes(t, bin, dir, false)
	for name, body := range map[string]string{
		"systemctl": "#!/bin/sh\necho \"$*\" >>" + dir + "/systemctl\n",
		"mv":        "#!/bin/sh\nfor a; do case $a in */" + filepath.Base(FirewallScript) + ") exit 1 ;; esac; done\nexec /bin/mv \"$@\"\n",
	} {
		write(t, filepath.Join(bin, name), body)
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", FirewallInstall(false), "sh", host)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + filepath.Join(dir, "mark")}
	out, _ := cmd.CombinedOutput()
	if code := cmd.ProcessState.ExitCode(); code != 5 || !strings.Contains(string(out), host+FirewallScript+" could not be written") {
		t.Errorf("(%d): %s", code, out)
	}
	if fileThere(filepath.Join(dir, "systemctl")) || fileThere(filepath.Join(dir, "fw.iptables.chain")) || fileThere(host+FirewallScript) {
		t.Errorf("a firewall loaded past a piece not written:\n%s", out)
	}
}
