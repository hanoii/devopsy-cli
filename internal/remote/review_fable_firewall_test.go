package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// reviewFirewallFakes writes, in bin, an iptables and its restore for each family that keep
// raw's jump and chain as files in state (fw.<cmd>.jump, fw.<cmd>.chain), and the flock a
// test needs where the system has none.
func reviewFirewallFakes(t *testing.T, bin, state string) {
	t.Helper()
	tables := `#!/bin/sh
[ "$1" != -w ] || shift 2
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
	restore := "#!/bin/sh\ncat >/dev/null\n: >" + state + "/fw.$(basename \"$0\" -restore).chain\n"
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

// The firewall's lock is root's alone to open: flock takes an exclusive lock on a file opened
// for reading, so a lock file others may read is one any user of the server holds for as
// long as it likes (a project user among them), and every load then fails after its minute:
// the timer's, which puts back what a reload of the server's firewall dropped, and Docker's
// start, delayed by that minute and its daemon's jump left on the old cgroup.
//
// Bug: the loader makes /run/devopsy-firewall.lock with its caller's umask, 022 under
// systemd, so mode 644.
func TestFirewallLockIsNotOneAnotherUserHolds(t *testing.T) {
	t.Parallel()
	bin, state := t.TempDir(), t.TempDir()
	reviewFirewallFakes(t, bin, state)
	mark := filepath.Join(state, "mark")
	// As systemd runs a unit: umask 022.
	cmd := exec.Command("sh", "-c", "umask 022\n"+FirewallLoader(false))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", FirewallMarkEnv + "=" + mark}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	lock := filepath.Join(state, "devopsy-firewall.lock")
	// The control: reading the file is enough to hold its lock.
	f, err := os.OpenFile(lock, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("a lock file opened for reading takes no exclusive lock here: %v", err)
	}
	fi, err := os.Stat(lock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("the firewall's lock is %v: any user who reads it holds it, and no load runs while it does", fi.Mode().Perm())
	}
}

// A removal leaves nothing loaded, a loader that waited for its lock included: the timer's
// unit, started just before the removal stopped the timer, waits on the lock the removal
// holds, and stopping a timer stops no unit it started. Once the removal ends, that loader
// must load nothing: the server's want is no firewall.
//
// Bug: the waiting loader, its file removed under it, goes on once it has the lock: it loads
// the chain and its jump again and writes the mark, with no file, unit or timer left; the
// apply then says the removal did not hold, and containers stay cut off until the next run.
//
// The fake systemctl starts the loader when the timer is stopped and holds the removal until
// that loader asked for the lock.
func TestFirewallRemovalLeavesNoLoadToAWaitingLoader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin, host, state := filepath.Join(dir, "bin"), filepath.Join(dir, "host"), filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	reviewFirewallFakes(t, bin, state)
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
case "$*" in
  "stop devopsy-firewall.timer")
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
	out, _ := os.ReadFile(filepath.Join(state, "loader.out"))
	if fileThere(filepath.Join(state, "fw.iptables.jump")) || fileThere(filepath.Join(state, "fw.iptables.chain")) || fileThere(mark) {
		t.Errorf("after a removal, a loader that waited for its lock loaded the firewall again: jump %v, chain %v, mark %v\n%s",
			fileThere(filepath.Join(state, "fw.iptables.jump")), fileThere(filepath.Join(state, "fw.iptables.chain")), fileThere(mark), out)
	}
}
