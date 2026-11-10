package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// BUG: at boot the firewall's loader stops at the daemon's jump and loads no IPv6 rule.
// devopsy-firewall.service runs before docker.service, whose cgroup is not there yet, and
// daemonCgroup names it all the same (/system.slice/docker.service). The kernel takes a
// cgroup's rule only for a cgroup that is there (xt_cgroup resolves the path when the rule
// goes in): `iptables -I OUTPUT 1 -m cgroup --path "/system.slice/docker.service" ...` fails
// with "RULE_INSERT failed (Invalid argument)", exit 4, as measured on Debian 13's iptables
// 1.8.11 (nf_tables). The loader runs under set -e, so it ends there, in IPv4's part: the
// ip6tables chains and jumps never load, and Docker's daemon has no jump, until the timer's
// first run five minutes after the boot, sshd and Docker up meanwhile. A jump for a cgroup
// not there yet is to wait for the cgroup (a run of the loader when docker.service starts),
// never stop what follows it.
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
		// docker.service not started yet, as at boot: its unit there, no cgroup.
		"systemctl": `#!/bin/sh
case "$1 ${3:-$2}" in "show ControlGroup") echo ;; "show Slice") echo system.slice ;; "cat docker.service") ;; *) exit 1 ;; esac
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
	cmd := exec.Command("sh", "-c", DoorFirewallLoader())
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	data, _ := os.ReadFile(log)
	ran := string(data)
	// The control: IPv4's chains and the project users' jump went in before the daemon's.
	if !strings.Contains(ran, "iptables-restore --noflush\n") || !strings.Contains(ran, "iptables -t filter -I OUTPUT 1 -m owner --gid-owner 1001 --suppl-groups -j "+DoorChain+"\n") {
		t.Fatalf("the loader did not load IPv4's chains and jump (%v): %s\n%s", err, out, ran)
	}
	if !strings.Contains(ran, "ip6tables-restore --noflush\n") || !strings.Contains(ran, "ip6tables -t filter -I OUTPUT 1 -m owner --gid-owner 1001 --suppl-groups -j "+DoorChain+"\n") {
		t.Errorf("with docker.service's cgroup not there yet, the loader loaded no IPv6 chain or jump (%v): %s\nwhat it ran:\n%s", err, out, ran)
	}
}
