package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The removal, run: the containers' jump and chain go from raw, the loader's pieces go only
// where they carry devopsy's header (an admin's own file at one of their paths stays), the
// timer's link left dangling goes, and so does the mark; run again, as after a cut, it ends
// with nothing left to remove and no failure.
func TestReviewFirewallRemoveRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin, host := filepath.Join(dir, "bin"), filepath.Join(dir, "host")
	// iptables with raw's jump and chain as files: -D fails once the jump is gone, -S once
	// the chain is.
	tables := `#!/bin/sh
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
	for _, c := range []string{"iptables", "ip6tables"} {
		write(t, filepath.Join(dir, "fw."+c+".jump"), "")
		write(t, filepath.Join(dir, "fw."+c+".chain"), "")
	}
	files := FirewallFiles(true)
	for p, text := range files {
		write(t, filepath.Join(host, p), text)
	}
	// An admin's own unit at the unit's path: no header of devopsy's.
	write(t, filepath.Join(host, FirewallUnit), "[Unit]\nDescription=the admin's own\n")
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
	if _, err := os.Stat(filepath.Join(host, FirewallUnit)); err != nil || strings.Contains(out, "removed "+FirewallUnit) {
		t.Errorf("the admin's own unit removed:\n%s", out)
	}
	if _, err := os.Lstat(timerLink); err == nil {
		t.Error("the timer's link left dangling")
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("the mark left")
	}
	log := readFile(filepath.Join(dir, "log"))
	if stop, rm := strings.Index(log, "systemctl stop devopsy-firewall.timer"), strings.Index(log, "systemctl daemon-reload"); stop < 0 || rm < stop {
		t.Errorf("the timer not stopped before the units go:\n%s", log)
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
