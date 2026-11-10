//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// BUG: a key's removal ends a project user's processes by reading /proc and killing each
// one it finds (remote.EndUserArgs' script), round after round until a round kills none. A
// process that forks its successor and exits at once is never there by the time the scan
// reads its status: the first round kills nothing, the script exits 0,
// the removal says "the processes of <user> are ended", and the process lives on with the
// user's group (the door, past the firewall's other rules) after its key is gone. A kill(-1,
// SIGKILL) from a process whose effective uid is the user's and whose real uid is no one's
// is atomic against fork and out of the user's reach to signal; a scan alone is not.
//
// The test server's init never reaps, so a root subreaper of the test's stands in for a
// real server's init, reaping what the chain leaves; it starts the chain as the project
// user, its groups included, as a login's shell would, and the chain touches a file every
// 1000 hops so its life shows from outside.
func TestReviewOpusKeyRemovalMissesAHoppingProcess(t *testing.T) {
	user := doorUser(t, "opushop")
	flag, beat := "/tmp/opushop-flag", "/tmp/opushop-beat"
	t.Cleanup(func() { doorRoot(t, "rm -f "+flag+" "+beat+"; sleep 1; pkill -9 -u "+user+" || true") })
	if out, code := op(t, "/", "rm -f /tmp/opushop /tmp/opushop.pub && ssh-keygen -q -t ed25519 -N '' -C opushop-ci -f /tmp/opushop"); code != 0 {
		t.Fatalf("a second key (%d): %s", code, out)
	}
	if out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for opushop add /tmp/opushop.pub 2>&1"); code != 0 {
		t.Fatalf("--ssh-keys --for opushop add (%d): %s", code, out)
	}
	// The chain stops by itself once root's flag goes, two minutes at most.
	chain := `my $n = 0; while (-e "` + flag + `") { my $p = fork; exit 0 if $p; utime(undef, undef, "` + beat + `") if ++$n % 1000 == 0; }`
	reaper := `syscall($ENV{NR}, 36, 1, 0, 0, 0) == 0 or die "prctl: $!"; ` +
		`if (!fork) { exec "setpriv", "--reuid=` + user + `", "--regid=` + user + `", "--init-groups", "perl", "-e", $ENV{CHAIN}; die "exec: $!" } ` +
		`while (-e "` + flag + `") { 1 while waitpid(-1, 1) > 0; select(undef, undef, undef, 0.01); } 1 while waitpid(-1, 0) > 0;`
	if out, code := doorRoot(t, `set -e
touch `+flag+` `+beat+` && chown `+user+` `+beat+`
(setsid sh -c 'sleep 120; rm -f `+flag+`' </dev/null >/dev/null 2>&1 &)
nr=167; [ "$(uname -m)" = x86_64 ] && nr=157
NR=$nr CHAIN='`+chain+`' setsid perl -e '`+reaper+`' </dev/null >/dev/null 2>&1 &`); code != 0 {
		t.Fatalf("the chain (%d): %s", code, out)
	}
	mtime := func() string {
		out, _ := doorRoot(t, "stat -c %y "+beat)
		return strings.TrimSpace(out)
	}
	alive := func() bool {
		a := mtime()
		time.Sleep(500 * time.Millisecond) // Not a wait for a state: a gap for the beat to move in.
		return mtime() != a
	}
	eventually(t, startWaitE2E, "the chain's beat", alive)
	out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for opushop remove opushop-ci 2>&1")
	if alive() && alive() {
		t.Errorf("BUG: a process of %s outlives its key's removal (the removal said, exit %d: %s)", user, code, strings.TrimSpace(out))
	}
}
