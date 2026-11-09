//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// BUG: a project user reaches the host's loopback through the door's receive form: its own
// rsync request (the form a sync's receive takes, with its own mnt and a password of the form's) runs as
// root on the host's network and connects to the port it names, and rsync says what the
// service there answered first (`rsync: server sent "SSH-2.0-..." rather than greeting`), or
// that nothing listens ("Connection refused"). The firewall keeps the user's own processes
// off the loopback (TestDoorFirewall), and this hands them a probe of it, a line of each
// service back. The controls: the deploy user reads sshd's banner on the loopback with curl,
// and the project user's own curl does not. A fix at the cause ties the form's port to the
// caller's own reverse forward (a listening socket of its uid), refusing every other.
func TestDoorReceiveProbesTheLoopback(t *testing.T) {
	dir := doorProject(t, "doorprobe", `services:
  web:
`+doorService+`    volumes: [./mnt/data:/d]
    command: [sleep, infinity]
`, nil)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a release (%d):\n%s", code, out)
	}
	user := "devopsy-doorprobe"
	banner := "curl -s --http0.9 --max-time 3 http://127.0.0.1:22/ 2>&1"
	if out, _ := doorAs(t, "devopsy", banner); !strings.Contains(out, "SSH-2.0") {
		t.Fatalf("the control: the deploy user did not read sshd's banner on the loopback: %s", out)
	}
	if out, _ := doorAs(t, user, banner); strings.Contains(out, "SSH-2.0") {
		t.Fatalf("the control: the firewall let the project user's own curl reach sshd on the loopback: %s", out)
	}
	// devopsy's own receive, as --sync-child would ask it, to sshd's port instead of a forward.
	probe := func(port string) string {
		t.Helper()
		out, _ := doorAs(t, user, "RSYNC_PASSWORD=0123456789abcdef0123456789abcdef docker run --rm --network host -e RSYNC_PASSWORD -v "+doorSpace+"/mnt/doorprobe-prod:/mnt devopsy-util:4 rsync -a --no-D --delete --numeric-ids --modify-window=-1 --sparse rsync://127.0.0.1:"+port+"/data/ /mnt/data/ 2>&1")
		return out
	}
	out := probe("22")
	t.Logf("the probe of sshd's port:\n%s", out)
	if strings.Contains(out, "SSH-2.0") {
		t.Errorf("through the door's receive, a project user read sshd's banner on the loopback:\n%s", out)
	} else if !strings.Contains(out, "refused:") {
		t.Errorf("the door ran a receive to a port no forward of the project user's listens on:\n%s", out)
	}
	out = probe("1")
	t.Logf("the probe of a closed port:\n%s", out)
	if strings.Contains(out, "Connection refused") {
		t.Errorf("through the door's receive, a project user told a closed loopback port from an open one:\n%s", out)
	}
}
