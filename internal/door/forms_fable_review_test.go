package door

import (
	"strings"
	"testing"
)

// BUG: the receive form takes any port on the loopback (recvSrc: `rsync://127.0.0.1:\d+/`),
// and a project user may ask for it directly, with its own mnt and any password: a root
// container on the host's network then connects there with rsync, which prints the first
// line the service answers (`rsync: server sent "SSH-2.0-OpenSSH_..." rather than greeting`)
// and tells an open port from a closed one (its exit 5 against 10, "Connection refused").
// The firewall rejects that user's own packets to the loopback (doorRules' `-o lo -j REJECT`,
// docker-proxy's published ports behind it), and this form hands the user a probe of every
// loopback port, one line of each service back. A sync's receive reaches one port only: the
// reverse forward its own sshd listens on, a socket of the caller's uid (/proc/net/tcp says
// whose a listening socket is), which the door can ask of the port before it runs the form;
// anything else on the loopback is not the caller's to reach. The control: a host other than
// the loopback is refused today, so the form checks the address and not the port's owner.
func TestDecideReceiveReachesOnlyTheCallersForward(t *testing.T) {
	t.Parallel()
	mnt := "/space/mnt/shop-prod"
	receive := func(src string) []string {
		return []string{"run", "--network", "host", "-e", "RSYNC_PASSWORD", "-v", mnt + ":/mnt", "util:1", "rsync", "-a", src, "/mnt/data/"}
	}
	if _, err := decide(t, receive("rsync://evil:22/data/")...); err == nil {
		t.Fatal("the control: a receive from another host went through")
	}
	// sshd's port, and the port of a container the admin published on the loopback: a sync's
	// forward listens on neither, and nothing says the port is this user's.
	for _, port := range []string{"22", "18080"} {
		a, err := decide(t, receive("rsync://127.0.0.1:"+port+"/data/")...)
		if err == nil {
			t.Errorf("a receive from 127.0.0.1:%s, a port no forward of this user's listens on, went through: %q", port, a.Docker)
		} else if !strings.Contains(err.Error(), "refused") {
			t.Errorf("port %s: %v", port, err)
		}
	}
}
