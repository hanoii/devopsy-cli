//go:build e2e

package e2e

import (
	"testing"
)


// BUG: PermitUserRC no closes one of the home's login hooks, ~/.ssh/rc, while the login shell's
// ~/.profile, as much the project user's to write, runs at the next login of a key kept: a
// leaked key writes it, its removal leaves it, and --shell-host, which execs a login shell
// (remote.ShellScript's `sh -l`), runs it for the owner, as would a plain `ssh` with no
// command. The doc's steps after a leak reset the home last, after a release logs in again.
func TestReviewOpusProfileRunsAtAKeptKeysLogin(t *testing.T) {
	user := doorUser(t, "opusprofile")
	keys := func(args string) {
		t.Helper()
		if out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for opusprofile "+args+" 2>&1"); code != 0 {
			t.Fatalf("--ssh-keys --for opusprofile %s (%d): %s", args, code, out)
		}
	}
	if out, code := op(t, "/", "rm -f /tmp/opusprofile /tmp/opusprofile.pub && ssh-keygen -q -t ed25519 -N '' -C opusprofile-leaked -f /tmp/opusprofile"); code != 0 {
		t.Fatalf("a second key (%d): %s", code, out)
	}
	mark := "/tmp/opusprofile-ran"
	t.Cleanup(func() { doorRoot(t, "rm -f "+mark+"; sed -i '/opusprofile-ran/d' ~"+user+"/.profile") })
	keys("add /tmp/opusprofile.pub")
	// The leaked key's shell.
	if out, code := doorAs(t, user, "echo 'touch "+mark+"' >>~/.profile"); code != 0 {
		t.Fatalf("its ~/.profile (%d): %s", code, out)
	}
	keys("remove opusprofile-leaked")
	// The owner's way in with a key kept: --shell-host's login shell.
	if out, code := op(t, "/", `ssh -o BatchMode=yes -T `+user+`@doorsrv 'exec "${SHELL:-/bin/sh}" -l' </dev/null`); code != 0 {
		t.Fatalf("the owner's login (%d): %s", code, out)
	}
	if _, code := doorRoot(t, "test -e "+mark); code == 0 {
		t.Error("BUG: a removed key's ~/.profile ran at the login of a key kept")
	}
}
