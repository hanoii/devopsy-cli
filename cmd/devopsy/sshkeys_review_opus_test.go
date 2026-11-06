package main

import (
	"strings"
	"testing"
)

// Bug (review, opus): targetRef names a server alone by its bare host, so
// for a server with a port the usage line and hints of a server command
// (@vm1:5369: --ssh-keys) write devopsy @vm1:5369 --ssh-keys, which as
// typed is environment 5369 on vm1, port 22: the hint cannot be copied
// back. A server alone is <host>: in a hint, as ServerRef writes it.
func TestServerAloneHintKeepsPortReview(t *testing.T) {
	t.Parallel()
	for _, server := range []string{"devopsy@vm1:5369", "[2001:db8::1]:22"} {
		out, code := runDevopsy(t, t.TempDir(), []string{"DEVOPSY_SSH_COMMAND=echo"}, "@"+server+":", "--ssh-keys", "bogus")
		if code == 0 || !strings.Contains(out, "usage: devopsy @") {
			t.Fatalf("%s: no usage (%d):\n%s", server, code, out)
		}
		if want := "@" + server + ": --ssh-keys"; !strings.Contains(out, want) && !strings.Contains(out, "@'"+server+":' --ssh-keys") {
			t.Errorf("%s: the usage names the server as no address takes it back, want %q:\n%s", server, want, out)
		}
	}
}
