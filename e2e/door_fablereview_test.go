//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// An attack round on project users, by one reviewer: what crosses to a project user on the
// host past the door's firewall rule, and what sshd reads of a project user when the host's
// sshd_config does not take the drop-in.

// BUG: the door's firewall rule goes by the interface a packet leaves through (-o docker0,
// -o br-+), and a port a container publishes on the host's loopback (127.0.0.1:<port>, as an
// admin publishes a database for itself, and as a sync's daemon listens) is reached through
// docker-proxy: a project user's connection to 127.0.0.1 leaves through lo, docker-proxy, a
// process of root's, opens the one to the container, and the rule sees neither. So a project
// user on the host reaches every port any project publishes on loopback. The control: the
// container's own address, out through docker0, is refused under the rule. The rule set
// should cover it, by the host's userland-proxy being off (then
// loopback goes by DNAT through docker0 too, and a sync's own daemon needs its port let
// through before the REJECT), or --server-info should say what it does not cover.
func TestDoorFableReviewLoopbackPortsPassTheRule(t *testing.T) {
	name := doorServer(t)
	user := doorUser(t, "doorlbfable")
	const victim = "doorlbfable-victim"
	rules := []string{"-o docker0", "-o br-+", "-d 169.254.169.254/32"}
	t.Cleanup(func() {
		for _, r := range rules {
			doorRoot(t, "iptables -D OUTPUT -m owner --gid-owner devopsy-projects --suppl-groups "+r+" -j REJECT")
		}
		doorRoot(t, "docker rm -f "+victim)
	})
	// Another project's container, the deploy user's say, with a port for the admin alone,
	// on loopback: not a door project's, which publish none.
	if out, code := execIn(t, []string{"-u", "devopsy", "-w", "/home/devopsy", name}, `docker run -d --name `+victim+` -p 127.0.0.1:18080:8080 busybox:1.37 sh -c 'while true; do echo -e "HTTP/1.1 200 OK\r\n\r\nhello-from-victim" | nc -l -p 8080; done' 2>&1`); code != 0 {
		t.Fatalf("the admin's container (%d): %s", code, out)
	}
	ip, _ := doorRoot(t, "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "+victim+" | cut -d' ' -f1")
	direct := "http://" + strings.TrimSpace(ip) + ":8080/"
	loopback := "http://127.0.0.1:18080/"
	ask := func(url string) string {
		t.Helper()
		out, _ := doorAs(t, user, "curl -s --max-time 3 "+url)
		return out
	}
	for _, url := range []string{direct, loopback} {
		if !strings.Contains(ask(url), "hello-from-victim") {
			t.Fatalf("the control: with no rule, a project user does not reach %s, so this proves nothing", url)
		}
	}
	for _, r := range rules {
		if out, code := doorRoot(t, "iptables -I OUTPUT -m owner --gid-owner devopsy-projects --suppl-groups "+r+" -j REJECT"); code != 0 {
			t.Fatal(out)
		}
	}
	if strings.Contains(ask(direct), "hello-from-victim") {
		t.Fatalf("the control: under the rule, a project user still reaches %s", direct)
	}
	if strings.Contains(ask(loopback), "hello-from-victim") {
		t.Errorf("BUG: under the door's firewall rule, a project user reaches a port another project publishes on the host's loopback (%s), through docker-proxy", loopback)
	}
}

// BUG: --project-users add checks sshd's drop-in with `sshd -t` alone, which says the file
// parses, never that sshd reads it: a host whose sshd_config has no `Include
// /etc/ssh/sshd_config.d/*.conf` (older Debian and Ubuntu, an admin's own config), or has it
// below a Match block, where an Include is conditional, leaves sshd on its defaults for
// project users, and add says the user is there. sshd then reads the project user's own
// ~/.ssh/authorized_keys, so a leaked key adds a second one that outlives its rotation and
// --project-users remove, which the docs say never happens. add should check what sshd
// resolves for the user (`sshd -T -C user=<user>,host=x,addr=127.0.0.1`: its
// authorizedkeyscommand is devopsy's, its authorizedkeysfile none) and refuse otherwise. The
// control: with the Include back, the same planted key does not log in.
func TestDoorFableReviewAddTrustsAnUnincludedDropIn(t *testing.T) {
	const project = "doorincfable"
	user := "devopsy-" + project
	name := doorServerAs(t, "doorsrv2")
	root := func(script string) (string, int) {
		t.Helper()
		return execIn(t, []string{"-w", "/", name}, script)
	}
	// The host's sshd_config without its Include, as a host may be set up: put back after.
	if out, code := root(`set -e
cp /etc/ssh/sshd_config /tmp/sshd_config.fable
sed -i '/^Include \/etc\/ssh\/sshd_config.d\//d' /etc/ssh/sshd_config
if grep -q '^Include' /etc/ssh/sshd_config; then echo 'an Include stays'; exit 1; fi
sshd -t
kill -HUP "$(cat /run/sshd.pid)"`); code != 0 {
		t.Fatalf("taking the Include out (%d): %s", code, out)
	}
	t.Cleanup(func() {
		root(`cp /tmp/sshd_config.fable /etc/ssh/sshd_config && kill -HUP "$(cat /run/sshd.pid)"; rm -f /run/devopsy-door-reboot /run/reboot-required /run/reboot-required.pkgs`)
		op(t, "/", "devopsy @devopsy@doorsrv2: --project-users remove "+project)
		root("pkill -u " + user + "; userdel -r " + user + " 2>/dev/null; true")
	})
	out, code := op(t, "/", "devopsy @devopsy@doorsrv2: --project-users add "+project+" 2>&1")
	if code != 0 {
		t.Logf("add refused on a host whose sshd does not take the drop-in (the fix): %s", out)
		return
	}
	// add went through. A reboot would take nothing more: the drop-in is not read.
	root(`kill -HUP "$(cat /run/sshd.pid)"; rm -f /run/devopsy-door-reboot /run/reboot-required /run/reboot-required.pkgs`)
	if out, code := op(t, "/", "rm -f /tmp/fableplanted /tmp/fableplanted.pub && ssh-keygen -q -t ed25519 -N '' -f /tmp/fableplanted && cat /tmp/fableplanted.pub"); code != 0 {
		t.Fatal(out)
	} else if out, code := execIn(t, []string{"-u", user, "-w", "/", name}, "mkdir -p ~/.ssh && echo '"+strings.TrimSpace(out)+"' >>~/.ssh/authorized_keys && chmod 700 ~/.ssh && chmod 600 ~/.ssh/authorized_keys"); code != 0 {
		t.Fatal(out)
	}
	login := "ssh -o BatchMode=yes -o IdentitiesOnly=yes -i /tmp/fableplanted " + user + "@doorsrv2 true 2>&1"
	// sshd takes a HUP in its own time: the planted key is tried until it logs in, or long
	// enough for any reload.
	loggedIn := false
	for i := 0; i < 20 && !loggedIn; i++ {
		_, code := op(t, "/", login)
		loggedIn = code == 0
	}
	if loggedIn {
		t.Errorf("BUG: --project-users add said %s is there (%s) on a host whose sshd_config does not include the drop-in, and a key planted in its home logs in", user, strings.TrimSpace(out))
	} else {
		t.Log("the planted key did not log in")
	}
	// The control: the Include back, the same key is not read.
	if out, code := root(`cp /tmp/sshd_config.fable /etc/ssh/sshd_config && sshd -t && kill -HUP "$(cat /run/sshd.pid)"`); code != 0 {
		t.Fatalf("putting the Include back (%d): %s", code, out)
	}
	eventually(t, time.Minute, "the planted key refused with the drop-in read", func() bool {
		_, code := op(t, "/", login)
		return code != 0
	})
}
