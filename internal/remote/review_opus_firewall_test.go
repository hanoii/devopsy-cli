package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

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
