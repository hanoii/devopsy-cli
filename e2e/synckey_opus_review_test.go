//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// BUG: a sync key's forwards need no open sync, and its unix socket forwards go anywhere.
// sshd runs a key's forced command only for a session, so `ssh -N` with the key runs
// nothing, --sync-child's gate check included, while sshd still takes the key's forwards:
// whoever holds the key (the parent's server, for the sync's two days at most when its holder
// died) keeps a listener on the child's loopback at any port it picks. And permitlisten bounds
// only TCP: sshd takes a remote unix socket forward (-R <path>:...) by port-forwarding alone,
// so the key plants a socket, listened to by the parent, at any path the child's user can
// write, its home included. The controls hold: the forced command refuses with no gate, and
// a local forward to Docker's socket is refused.
func TestOpusReviewSyncKeyForwardsWithoutItsSyncE2E(t *testing.T) {
	id := "1762600000.0bcdef"
	key := "/tmp/opus-synckey"
	pub, code := op(t, "/", "rm -f "+key+" "+key+".pub && ssh-keygen -q -t ed25519 -N '' -C opus -f "+key+" && cut -d' ' -f2 "+key+".pub")
	if code != 0 {
		t.Fatalf("ssh-keygen (%d): %s", code, pub)
	}
	line, err := remote.SyncKeyLine("/usr/local/bin/devopsy", id, "/home/devopsy/opus-nowhere/dev", "server:dev", false, strings.TrimSpace(pub), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if out, code := srv(t, "printf '%s\\n' "+quote(line)+" >>~/.ssh/authorized_keys"); code != 0 {
		t.Fatalf("authorizing (%d): %s", code, out)
	}
	t.Cleanup(func() {
		op(t, "/", "pkill -f "+key+" || :; rm -f "+key+" "+key+".pub /tmp/opus-l.sock")
		srv(t, "sed -i '/devopsy-sync-"+id+"/d' ~/.ssh/authorized_keys; rm -f ~/opus-planted.sock")
	})
	ssh := "ssh -F /dev/null -i " + key + " -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ExitOnForwardFailure=yes"
	// Control: its forced command does nothing with no sync open.
	if out, code := op(t, "/", ssh+` devopsy@server '{"exec":["--sync-services","list"]}'`); code == 0 || !strings.Contains(out, "is not open") {
		t.Fatalf("control: the forced command with no gate (%d): %s", code, out)
	}
	// Control: no local forward, to Docker's socket least of all.
	background(t, "/", ssh+" -N -L /tmp/opus-l.sock:/var/run/docker.sock devopsy@server", "/tmp/opus-l.log")
	eventually(t, 20*time.Second, "the local forward's socket", func() bool {
		_, code := op(t, "/", "test -S /tmp/opus-l.sock")
		return code == 0
	})
	if out, _ := op(t, "/", "curl -s -m 5 --unix-socket /tmp/opus-l.sock http://docker/version"); strings.Contains(out, "ApiVersion") {
		t.Fatalf("control: Docker's socket through the key: %s", out)
	}
	// within says whether cond comes true in 15 seconds.
	within := func(cond func() bool) bool {
		for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
			if cond() {
				return true
			}
		}
		return false
	}
	background(t, "/", ssh+" -N -R 127.0.0.1:24681:127.0.0.1:9 devopsy@server", "/tmp/opus-r.log")
	if within(func() bool {
		out, _ := srv(t, "ss -ltnH 'sport = :24681'")
		return strings.Contains(out, "127.0.0.1:24681")
	}) {
		t.Error("BUG: the sync key, its sync not open, holds a listener on the child's loopback (127.0.0.1:24681): ssh -N runs no forced command")
	}
	background(t, "/", ssh+" -N -R /home/devopsy/opus-planted.sock:127.0.0.1:9 devopsy@server", "/tmp/opus-s.log")
	if within(func() bool {
		_, code := srv(t, "test -S ~/opus-planted.sock")
		return code == 0
	}) {
		t.Error("BUG: the sync key plants a unix socket the parent listens to in the deploy user's home (/home/devopsy/opus-planted.sock): permitlisten bounds TCP only")
	}
}
