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
