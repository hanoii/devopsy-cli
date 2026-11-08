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
