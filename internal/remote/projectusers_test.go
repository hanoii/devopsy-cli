package remote

import (
	"os/exec"
	"strings"
	"testing"
)

// What --project-users add runs as root on the host is POSIX sh, and what it writes names the
// deploy user and the devopsy it found: the door's two sudo rules, the script sshd runs, and
// sshd's block for the project users' group alone.
func TestDoorHostScript(t *testing.T) {
	t.Parallel()
	s := DoorHostScript("deploy", "/opt/bin/devopsy")
	if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
	for _, want := range []string{
		"%devopsy-projects ALL=(deploy) NOPASSWD: /opt/bin/devopsy --door *",
		"deploy ALL=(%devopsy-projects) NOPASSWD: /opt/bin/devopsy --door-read *",
		"exec /opt/bin/devopsy --authorized-keys",
		"Match Group devopsy-projects",
		"AuthorizedKeysFile none",
		"AuthorizedKeysCommandUser deploy",
		"visudo -cqf",
		"sshd -t",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q", want)
		}
	}
}

// The door's firewall rule is found as iptables -S writes it, by the group's id: each of its
// parts a REJECT or DROP of the project users' traffic, and nothing else counts.
func TestDoorFirewallMissing(t *testing.T) {
	t.Parallel()
	all := "-P OUTPUT ACCEPT\n" +
		"-A OUTPUT -o docker0 -m owner --gid-owner 1001 --suppl-groups -j REJECT --reject-with icmp-port-unreachable\n" +
		"-A OUTPUT -o br-+ -m owner --gid-owner 1001 --suppl-groups -j DROP\n" +
		"-A OUTPUT -d 169.254.169.254/32 -m owner --gid-owner 1001 --suppl-groups -j REJECT --reject-with icmp-port-unreachable\n"
	if got := DoorFirewallMissing(all, "1001"); len(got) != 0 {
		t.Errorf("all there: %q", got)
	}
	if got := DoorFirewallMissing(all, "1002"); len(got) != 3 {
		t.Errorf("another group's: %q", got)
	}
	accept := strings.ReplaceAll(all, "-j DROP", "-j ACCEPT")
	if got := DoorFirewallMissing(accept, "1001"); len(got) != 1 || got[0] != "-o br-+" {
		t.Errorf("an ACCEPT for a part: %q", got)
	}
	if got := DoorFirewallMissing("-P OUTPUT ACCEPT\n", "1001"); len(got) != 3 {
		t.Errorf("none: %q", got)
	}
}
