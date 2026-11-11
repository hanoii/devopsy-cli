//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// srvRoot runs script on the server as root.
func srvRoot(t *testing.T, script string) (string, int) {
	t.Helper()
	return execIn(t, []string{"-w", "/", server}, script)
}

// The firewall on a server with no project users, as --server-setup's root run loads it (its
// install with no door), on the server's real iptables: a container's request to a metadata
// address is dropped, its DNS there (port 53) let through, and the chain is the containers'
// alone; then what firewall: metadata: allow runs takes it out, and the container reaches the
// address again. The lane's servers have no systemd, which --server-setup needs, so the test
// runs devopsy's own scripts, as its root run does.
func TestServerFirewallWithoutADoor(t *testing.T) {
	t.Cleanup(func() {
		srvRoot(t, "docker rm -f srvfw-md >/dev/null 2>&1; ip link del srvfwmd0 2>/dev/null; true")
		srvRoot(t, remote.FirewallRemove())
	})
	// A stand-in for the cloud's metadata service at its address, on port 80 and on DNS's.
	if out, code := srvRoot(t, `set -e
ip link add srvfwmd0 type dummy
ip addr add 169.254.169.254/32 dev srvfwmd0
ip link set srvfwmd0 up
docker run -d --name srvfw-md --network host busybox:1.37 sh -c 'mkdir -p /w && echo instance-credentials >/w/index.html && httpd -p 169.254.169.254:53 -h /w && exec httpd -f -p 169.254.169.254:80 -h /w' >/dev/null`); code != 0 {
		t.Fatalf("the stand-in (%d): %s", code, out)
	}
	reaches := func(port string) bool {
		t.Helper()
		out, _ := srvRoot(t, "docker run --rm busybox:1.37 wget -qO- -T 3 http://169.254.169.254:"+port+"/ 2>&1")
		return strings.Contains(out, "instance-credentials")
	}
	// The control: with no firewall a container reads it.
	eventually(t, startWaitE2E, "the stand-in answering a container", func() bool { return reaches("80") })

	read, code := srvRoot(t, remote.FirewallInstall(false))
	if code != 0 {
		t.Fatalf("the install (%d):\n%s", code, read)
	}
	if wrong := remote.ContainersFirewallWrong(read); len(wrong) != 0 {
		t.Errorf("not loaded as devopsy's: %q\n%s", wrong, read)
	}
	if strings.Contains(read, remote.DoorChain) || strings.Contains(read, remote.DoorDaemonChain) {
		t.Errorf("a door's chain loaded with no door:\n%s", read)
	}
	if mark, _ := srvRoot(t, "cat "+remote.FirewallMark); mark != remote.FirewallContainers+"\n" {
		t.Errorf("the mark: %q", mark)
	}
	if reaches("80") {
		t.Error("a container read the metadata address through the firewall")
	}
	if !reaches("53") {
		t.Error("a container's DNS at the metadata address was dropped")
	}
	// Loaded again, as the boot's unit and the timer do: the same chain, one jump.
	if out, code := srvRoot(t, remote.FirewallScript+" && iptables -t raw -S PREROUTING | grep -c -- '-j "+remote.FirewallContainerChain+"$'"); code != 0 || strings.TrimSpace(out) != "1" {
		t.Errorf("loaded again (%d): %s", code, out)
	}

	// firewall: metadata: allow: devopsy's pieces and the chain go, and the address answers.
	if out, code := srvRoot(t, remote.FirewallRemove()); code != 0 || !strings.Contains(out, "removed "+remote.FirewallScript) {
		t.Fatalf("the removal (%d): %s", code, out)
	}
	if out, _ := srvRoot(t, "iptables -t raw -S; ls "+remote.FirewallScript+" "+remote.FirewallMark+" 2>&1"); strings.Contains(out, remote.FirewallContainerChain) || !strings.Contains(out, "No such file") {
		t.Errorf("left after the removal:\n%s", out)
	}
	if !reaches("80") {
		t.Error("allowed, a container still could not read the metadata address")
	}
}
