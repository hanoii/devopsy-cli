//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// srvRoot runs script on the server as root.
func srvRoot(t *testing.T, script string) (string, int) {
	t.Helper()
	return execIn(t, []string{"-w", "/", server}, script)
}

// The firewall on a server with no project users, as --server-setup's root run loads it (its
// install with no door, the loader run by the install itself, as where systemd does not
// answer), on the server's real iptables: a container's request to a metadata address is
// dropped, TCP's and UDP's, its DNS there (port 53, both) let through, and the chain is the
// containers' alone, IPv6's too where the server has it; loaded again, it keeps another
// tool's rule in raw's PREROUTING and one jump of its own; a wipe shows in --server-info and
// the loader run again puts it back; then what firewall: metadata: allow runs takes it out,
// and the container reaches the address again. The lane's servers have no
// systemd, which --server-setup needs, so the test runs devopsy's own scripts, as its root
// run does.
func TestServerFirewallWithoutADoor(t *testing.T) {
	t.Cleanup(func() {
		srvRoot(t, "docker rm -f srvfw-md >/dev/null 2>&1; ip link del srvfwmd0 2>/dev/null; true")
		srvRoot(t, remote.FirewallRemove())
	})
	// A stand-in for the cloud's metadata service at its address, on TCP's port 80 and DNS's,
	// and UDP listeners, each keeping the first datagram it gets: DNS's port, another, and
	// one for the control.
	if out, code := srvRoot(t, `set -e
ip link add srvfwmd0 type dummy
ip addr add 169.254.169.254/32 dev srvfwmd0
ip link set srvfwmd0 up
docker run -d --name srvfw-md --network host busybox:1.37 sh -c 'mkdir -p /w && echo instance-credentials >/w/index.html && httpd -p 169.254.169.254:53 -h /w && for p in 53 5353 5354; do nc -lu -s 169.254.169.254 -p $p >/w/udp$p & done; exec httpd -f -p 169.254.169.254:80 -h /w' >/dev/null`); code != 0 {
		t.Fatalf("the stand-in (%d): %s", code, out)
	}
	reaches := func(port string) bool {
		t.Helper()
		out, _ := srvRoot(t, "docker run --rm busybox:1.37 wget -qO- -T 3 http://169.254.169.254:"+port+"/ 2>&1")
		return strings.Contains(out, "instance-credentials")
	}
	// udpSend sends a container's datagram to each port, at once.
	udpSend := func(ports ...string) {
		t.Helper()
		var sends []string
		for _, p := range ports {
			sends = append(sends, "echo udp-through | nc -u -w 1 169.254.169.254 "+p)
		}
		srvRoot(t, "docker run --rm busybox:1.37 sh -c '"+strings.Join(sends, "; ")+"' 2>&1")
	}
	// udpGot says whether port's listener got a datagram.
	udpGot := func(port string) bool {
		t.Helper()
		out, _ := srvRoot(t, "docker exec srvfw-md cat /w/udp"+port)
		return strings.Contains(out, "udp-through")
	}
	// The control: with no firewall a container reads it, and its datagram arrives.
	eventually(t, startWaitE2E, "the stand-in answering a container", func() bool { return reaches("80") })
	udpSend("5354")
	eventually(t, 5*time.Second, "a container's datagram to the stand-in, with no firewall", func() bool { return udpGot("5354") })

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
	// IPv6's chain and jump, where the server's kernel has it.
	families := 2
	if strings.Contains(read, "# ip6tables off") {
		families = 1
	}
	if n := strings.Count(read, "-A PREROUTING -j "+remote.FirewallContainerChain+"\n"); n != families {
		t.Errorf("%d jumps for %d families:\n%s", n, families, read)
	}
	if mark, _ := srvRoot(t, "cat "+remote.FirewallMark); mark != remote.FirewallContainers+"\n" {
		t.Errorf("the mark: %q", mark)
	}
	if reaches("80") {
		t.Error("a container read the metadata address through the firewall")
	}
	if !reaches("53") {
		t.Error("a container's DNS over TCP at the metadata address was dropped")
	}
	// Both datagrams sent together: once DNS's arrived, the other would have.
	udpSend("5353", "53")
	eventually(t, 5*time.Second, "a container's DNS over UDP at the metadata address", func() bool { return udpGot("53") })
	if udpGot("5353") {
		t.Error("a container's datagram to the metadata address went through the firewall")
	}
	// Loaded again, as the boot's unit and an install do, with another tool's rule in raw's
	// PREROUTING: the same chain, one jump, and the other rule still there.
	other := "-s 10.9.9.9/32 -j DROP"
	srvRoot(t, "iptables -t raw -A PREROUTING "+other)
	out, code := srvRoot(t, remote.FirewallScript+" && iptables -t raw -S PREROUTING")
	if code != 0 || strings.Count(out, "-A PREROUTING -j "+remote.FirewallContainerChain+"\n") != 1 || !strings.Contains(out, "-A PREROUTING "+other+"\n") {
		t.Errorf("loaded again (%d):\n%s", code, out)
	}
	srvRoot(t, "iptables -t raw -D PREROUTING "+other)
	// A wipe, which nothing loads back between a boot and an install: --server-info, as the
	// deploy user, reads the chains as root and says W0001 with what is missing; the loader
	// run again puts it back, and it says nothing.
	info := func() string {
		t.Helper()
		out, _ := op(t, "/", "devopsy @devopsy@server: --server-info 2>&1")
		return out
	}
	srvRoot(t, "iptables -t raw -D PREROUTING -j "+remote.FirewallContainerChain)
	// --server-info builds no util image (only an apply does): the deploy user's is made first.
	if out, code := srv(t, "devopsy "+remote.UtilImageFlag); code != 0 {
		t.Fatalf("the util image (%d): %s", code, out)
	}
	if out := info(); !strings.Contains(out, "metadata firewall: W0001: the server's firewall is not loaded as devopsy loads it") || !strings.Contains(out, "nothing in PREROUTING jumps to "+remote.FirewallContainerChain) {
		t.Errorf("--server-info, the firewall wiped:\n%s", out)
	}
	if out, code := srvRoot(t, remote.FirewallScript); code != 0 {
		t.Fatalf("the loader run again (%d): %s", code, out)
	}
	if out := info(); strings.Contains(out, "W0001") {
		t.Errorf("--server-info, the firewall loaded again:\n%s", out)
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
