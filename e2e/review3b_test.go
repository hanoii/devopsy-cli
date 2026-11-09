//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// BUG: a project user's process stops its own end. The end runs as that user
// (remote.EndProcessesArgs: docker run --pid host --user <uid> ... kill -9 -1), so whatever
// that user runs may signal it first: runc's init takes the user's uid before the container
// starts, and a process of the user's that sends kill -9 -1 in a loop, which never signals
// its sender, ends it there. The last key's removal then fails ("could not be ended") and
// the process stays, as it does after --project-users remove.
//
// A process a project user left running is ended with its last key, whatever it does.
func TestDoorKeylessUserCannotStopItsEnd(t *testing.T) {
	user := doorUser(t, "doorr3bend")
	flag := "/tmp/doorr3bend-loop"
	// The loop runs while root's flag is there, two minutes at most: it would otherwise
	// outlive a test cut short, and keep a processor busy on the lane's door server.
	t.Cleanup(func() { doorRoot(t, "rm -f "+flag+"; pkill -9 -u "+user+" || true") })
	if out, code := doorRoot(t, "touch "+flag+" && (setsid sh -c 'sleep 120; rm -f "+flag+"' </dev/null >/dev/null 2>&1 &)"); code != 0 {
		t.Fatalf("the loop's flag (%d): %s", code, out)
	}
	// kill -9 -1 never signals its sender, so the loop outlives every round of its own.
	doorAs(t, user, "setsid sh -c 'while [ -e "+flag+" ]; do kill -9 -1; done' </dev/null >/dev/null 2>&1 &")
	running := func() bool {
		out, _ := doorRoot(t, "ps -u "+user+" -o stat=,comm= | awk '$1 !~ /^Z/'")
		return strings.TrimSpace(out) != ""
	}
	eventually(t, startWaitE2E, "its loop", running)
	fp, code := op(t, "/", "ssh-keygen -lf /root/.ssh/id_ed25519.pub | awk '{print $2}'")
	if code != 0 {
		t.Fatalf("the operator's key (%d): %s", code, fp)
	}
	out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for doorr3bend remove "+strings.TrimSpace(fp)+" 2>&1")
	deadline := time.Now().Add(10 * time.Second)
	for running() && time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
	}
	if running() {
		left, _ := doorRoot(t, "ps -u "+user+" -o pid=,stat=,args=")
		t.Errorf("its last key removed (%d: %s), and its process still runs:\n%s", code, strings.TrimSpace(out), left)
	}
}

// BUG: on a host with newuidmap and newgidmap (the uidmap package, which rootless tools
// need), a project user's process leaves the firewall. useradd gives the user subordinate
// ids, so in a user namespace its process takes one of them and drops its groups, and the
// firewall goes by the devopsy-projects group of a socket's owner (--gid-owner, with
// --suppl-groups), which that process no longer has: it reaches the host's loopback, and so
// Docker's bridges and a metadata service.
//
// A project user's process reaches nothing the firewall closes, whatever ids the host lets
// it take. Skipped where uidmap is not there and cannot be installed.
func TestDoorFirewallHoldsUnderSubordinateIDs(t *testing.T) {
	user := doorUser(t, "doorr3bsub")
	if out, code := doorRoot(t, "command -v newgidmap >/dev/null && exit 0; apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq --no-install-recommends uidmap >/dev/null 2>&1 && touch /tmp/doorr3bsub-uidmap"); code != 0 {
		t.Skipf("no uidmap on the door's server, and none to install (%d): %s", code, out)
	}
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f doorr3bsub-loopback; [ ! -e /tmp/doorr3bsub-uidmap ] || { apt-get remove -y -qq uidmap; rm -f /tmp/doorr3bsub-uidmap; }; true")
	})
	if out, code := doorRoot(t, "docker run -d --name doorr3bsub-loopback -p 127.0.0.1:18081:8080 busybox:1.37 sh -c 'mkdir -p /w && echo hello-from-loopback >/w/index.html && exec httpd -f -p 8080 -h /w' >/dev/null"); code != 0 {
		t.Fatalf("the admin's port on the loopback (%d): %s", code, out)
	}
	site := "curl -s --max-time 3 http://127.0.0.1:18081/"
	eventually(t, startWaitE2E, "the port answering the deploy user", func() bool {
		out, _ := doorAs(t, "devopsy", site)
		return strings.Contains(out, "hello-from-loopback")
	})
	if out, _ := doorAs(t, user, site); strings.Contains(out, "hello-from-loopback") {
		t.Fatalf("the control: the firewall lets a project user's plain process through: %s", out)
	}
	// A subordinate uid and gid, and no groups: all the user's own to take.
	hidden := "unshare --user --map-root-user --map-users=auto --map-groups=auto --setgroups allow setpriv --reuid 1 --regid 1 --clear-groups "
	if out, _ := doorAs(t, user, hidden+site+" 2>&1"); strings.Contains(out, "hello-from-loopback") {
		t.Errorf("under a subordinate id with its groups dropped, a project user's process reached the host's loopback: %s", out)
	}
}
