package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Two loaders running at once leave another tool's rule in PREROUTING as it was, and one jump
// of devopsy's: the install starts the timer, whose OnBootSec has passed on a server up for
// five minutes, so systemd starts the unit at once, beside the loader the install then runs
// itself.
//
// Bug: first() deletes the jump's other copies by the numbers it read before, with nothing
// held between the read and the delete. Two loaders that both insert, then both read, both
// delete rule 2: the second delete takes the rule that moved there, another tool's (Docker's
// own in raw's PREROUTING, a firewall manager's in filter's OUTPUT with a door), or fails when
// there is none. The fake iptables here keeps PREROUTING in a file and holds each read until
// both loaders inserted, and each delete until both read, the interleaving that loses the
// rule.
func TestFirewallLoadersAtOnceKeepOtherRules(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	state := t.TempDir()
	other := "-s 10.9.9.9/32 -j DROP"
	tables := `#!/bin/sh
s=` + state + `/$(basename "$0")
until mkdir "$s.lock" 2>/dev/null; do sleep 0.01; done
trap 'rmdir "$s.lock"' EXIT
case "$*" in
  "-t raw -I PREROUTING 1 "*)
    shift 5
    { echo "$*"; cat "$s.pre"; } >"$s.new" && mv "$s.new" "$s.pre"
    echo x >>"$s.inserts" ;;
  "-t raw -S PREROUTING")
    # Both loaders' inserts first, for 20 s at most.
    rmdir "$s.lock"; trap - EXIT
    n=0
    while [ "$(wc -l <"$s.inserts" 2>/dev/null || echo 0)" -lt 2 ] && [ "$n" -lt 1000 ]; do sleep 0.02; n=$((n + 1)); done
    until mkdir "$s.lock" 2>/dev/null; do sleep 0.01; done
    trap 'rmdir "$s.lock"' EXIT
    echo "-P PREROUTING ACCEPT"
    sed 's/^/-A PREROUTING /' "$s.pre"
    echo x >>"$s.reads" ;;
  "-t raw -D PREROUTING "*)
    # Both loaders' reads first, the same way.
    rmdir "$s.lock"; trap - EXIT
    n=0
    while [ "$(wc -l <"$s.reads" 2>/dev/null || echo 0)" -lt 2 ] && [ "$n" -lt 1000 ]; do sleep 0.02; n=$((n + 1)); done
    until mkdir "$s.lock" 2>/dev/null; do sleep 0.01; done
    trap 'rmdir "$s.lock"' EXIT
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
	for _, c := range []string{"iptables", "ip6tables"} {
		write(t, filepath.Join(state, c+".pre"), other+"\n")
	}
	var wg sync.WaitGroup
	outs := make([]string, 2)
	errs := make([]error, 2)
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command("sh", "-c", FirewallLoader(false))
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + filepath.Join(state, "mark")}
			out, err := cmd.CombinedOutput()
			outs[i], errs[i] = string(out), err
		}()
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

// The removal takes only the files that are devopsy's by their header: a loader the admin
// wrote in its place stays, a unit of devopsy's goes, and so does a link to it left dangling.
func TestFirewallRemoveKeepsAnAdminsFile(t *testing.T) {
	t.Parallel()
	bin, host := t.TempDir(), t.TempDir()
	for _, name := range []string{"iptables", "ip6tables", "systemctl"} {
		write(t, filepath.Join(bin, name), "#!/bin/sh\nexit 1\n")
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	admins := "#!/bin/sh\n# The admin's own.\n# Written by devopsy, it says further down\n"
	write(t, filepath.Join(host, FirewallScript), admins)
	write(t, filepath.Join(host, FirewallUnit), firewallUnitText)
	wants := filepath.Join(host, "/etc/systemd/system/multi-user.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(wants, "devopsy-firewall.service")
	if err := os.Symlink(filepath.Join(host, FirewallUnit), link); err != nil {
		t.Fatal(err)
	}
	mark := filepath.Join(host, "mark")
	write(t, mark, FirewallContainers+"\n")
	cmd := exec.Command("sh", "-c", FirewallRemove(), "sh", host)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + mark}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if data, _ := os.ReadFile(filepath.Join(host, FirewallScript)); string(data) != admins {
		t.Errorf("the admin's loader: %q", data)
	}
	if _, err := os.Stat(filepath.Join(host, FirewallUnit)); err == nil {
		t.Error("devopsy's unit stayed")
	}
	if _, err := os.Lstat(link); err == nil {
		t.Error("the link to the unit stayed, dangling")
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("the mark stayed")
	}
	if got := strings.TrimSpace(string(out)); got != "removed "+FirewallUnit {
		t.Errorf("what it said it removed: %q", got)
	}
}
