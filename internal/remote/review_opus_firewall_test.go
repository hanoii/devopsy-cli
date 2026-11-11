package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// Bug (review round 2): the firewall's lock is a file any local user can open. The loader
// makes it beside the mark (/run/devopsy-firewall.lock) with the umask it runs under, 022 in
// a systemd unit, so it is world-readable, and flock takes an exclusive lock on a descriptor
// opened read-only. A project user logged in over SSH holds it for good (flock
// /run/devopsy-firewall.lock sleep infinity): every load waits a minute and fails, the boot
// unit's, the timer's, --project-users add's and --server-setup's, and with a door
// Docker's start (FirewallDockerDropIn) waits that minute and starts with the daemon's jump
// still bound to the cgroup systemd just replaced, so the daemon is outside its chain until
// the lock is let go. Before the lock, nothing a project user did stopped a load. Only root
// may open what the firewall's lock is taken on.
func TestReviewFirewallLockOnlyRoots(t *testing.T) {
	t.Parallel()
	bin, run := t.TempDir(), t.TempDir()
	mark := filepath.Join(run, "devopsy-firewall.loaded")
	for _, name := range []string{"iptables", "ip6tables", "iptables-restore", "ip6tables-restore"} {
		write(t, filepath.Join(bin, name), "#!/bin/sh\n[ \"${0##*-}\" != restore ] || cat >/dev/null\n")
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := testfile.Flock(bin); err != nil {
		t.Fatal(err)
	}
	// systemd's default UMask for a service, which the boot unit and the timer's run take.
	cmd := exec.Command("sh", "-c", "umask 022\n"+FirewallLoader(false))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + mark}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	entries, err := os.ReadDir(run)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == filepath.Base(mark) {
			continue
		}
		fi, err := os.Lstat(filepath.Join(run, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o006 != 0 {
			t.Errorf("%s, left beside the mark by a load, is %v: any local user opens it and holds the firewall's lock", e.Name(), fi.Mode().Perm())
		}
	}
}

// Bug (review round 2): a removal leaves a load of the timer's waiting behind it. The removal
// takes the lock, then stops the timer; a timer that elapsed in between has had systemd start
// devopsy-firewall.service, whose loader, its script already open, waits on the lock and, once
// the removal lets it go, loads the containers' chain again and writes the mark, its files
// gone: firewall: metadata: allow then reads "to remove" again, and the chain is back until
// the next --server-setup. The fake systemctl starts that loader where the timer, stopped
// while the removal holds the lock, could have fired, and stops it as systemd stops the
// service; once the removal and that loader are done, nothing of the firewall may be loaded.
func TestReviewFirewallRemoveLeavesNoLoadBehind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin, host := filepath.Join(dir, "bin"), filepath.Join(dir, "host")
	mark := filepath.Join(dir, "devopsy-firewall.loaded")
	tables := `#!/bin/sh
[ "$1" != -w ] || shift 2
s=` + dir + `/fw.$(basename "$0")
case "$*" in
  "-t raw -I PREROUTING 1 -j ` + FirewallContainerChain + `") : >"$s.jump" ;;
  "-t raw -S PREROUTING") echo "-P PREROUTING ACCEPT"; [ ! -e "$s.jump" ] || echo "-A PREROUTING -j ` + FirewallContainerChain + `" ;;
  "-t raw -D PREROUTING -j ` + FirewallContainerChain + `") [ -e "$s.jump" ] || exit 1; rm -f "$s.jump" ;;
  "-t raw -S ` + FirewallContainerChain + `") [ -e "$s.chain" ] || exit 1 ;;
  "-t raw -X ` + FirewallContainerChain + `") rm -f "$s.chain" ;;
esac
`
	restore := "#!/bin/sh\ncat >/dev/null\n: >" + dir + "/fw.$(basename \"$0\" -restore).chain\n"
	// The timer elapsing while the removal holds the lock (a new descriptor on the lock file
	// cannot take it): systemd starts the service, its loader opened, then waiting.
	systemctl := `#!/bin/sh
case "$*" in
  "stop devopsy-firewall.timer"*|*" devopsy-firewall.timer"*)
    if [ -e "` + host + FirewallScript + `" ] && ! { flock -w 0 8; } 8>"` + dir + `/devopsy-firewall.lock"; then
      ( sh "` + host + FirewallScript + `" >/dev/null 2>&1 & echo $! >"` + dir + `/loader.pid"; wait; : >"` + dir + `/loader.done" ) 9>&- &
      until [ -s "` + dir + `/loader.pid" ]; do :; done
    fi ;;
esac
case " $* " in
  *" stop "*" devopsy-firewall.service "*) [ ! -s "` + dir + `/loader.pid" ] || kill "$(cat "` + dir + `/loader.pid")" 2>/dev/null || : ;;
esac
`
	for name, body := range map[string]string{"iptables": tables, "ip6tables": tables, "iptables-restore": restore, "ip6tables-restore": restore, "systemctl": systemctl} {
		write(t, filepath.Join(bin, name), body)
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := testfile.Flock(bin); err != nil {
		t.Fatal(err)
	}
	for p, f := range FirewallFiles(false) {
		write(t, filepath.Join(host, p), f.Text)
	}
	for _, c := range []string{"iptables", "ip6tables"} {
		write(t, filepath.Join(dir, "fw."+c+".jump"), "")
		write(t, filepath.Join(dir, "fw."+c+".chain"), "")
	}
	write(t, mark, FirewallContainers+"\n")
	cmd := exec.Command("sh", "-c", FirewallRemove(), "sh", host)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + mark}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the removal: %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "loader.pid")); err != nil {
		t.Fatal("the control: no loader started while the removal held the lock")
	}
	for start := time.Now(); ; {
		if _, err := os.Stat(filepath.Join(dir, "loader.done")); err == nil {
			break
		}
		if time.Since(start) > startWait {
			t.Fatal("the loader left waiting did not end")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Errorf("after the removal, a load waiting behind it marked the firewall loaded: %q", readFile(mark))
	}
	if _, err := os.Stat(filepath.Join(dir, "fw.iptables.chain")); err == nil {
		t.Error("after the removal, a load waiting behind it loaded the containers' chain again")
	}
}
