//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// BUG: a service runs as 2147483646, the uid of the end's kill(-1) child (cmd/devopsy
// noProcessUID), which userRefused takes; the end of a project user's processes then refuses
// to kill anything while that container runs, so a key's removal leaves what the leaked key
// started running (internal/door TestReviewOpusNoContainerTakesTheEndsUID).
func TestReviewOpusEndFailsBesideItsOwnUID(t *testing.T) {
	const name = "opusids"
	dir := doorProject(t, name, `services:
  web:
    image: busybox:1.37
    user: "2147483646:2147483646"
    security_opt: [no-new-privileges]
    command: [sleep, infinity]
`, nil)
	user := "devopsy-" + name
	t.Cleanup(func() {
		doorRoot(t, "pkill -9 -u "+user+"; docker rm -f "+name+"-prod-web-1 >/dev/null 2>&1; docker network rm "+name+"-prod_default >/dev/null 2>&1; true")
	})
	if out, code := op(t, dir, "devopsy @prod --release 2>&1"); code != 0 {
		t.Fatalf("a release of a service as 2147483646 (%d):\n%s", code, out)
	}
	if out, _ := doorRoot(t, "docker inspect -f '{{.Config.User}}' "+name+"-prod-web-1"); strings.TrimSpace(out) == "2147483646:2147483646" {
		t.Errorf("BUG: the door ran a service as 2147483646, the end's own uid")
	}

	// A process the leaked key left, then a key's removal.
	running := func() bool {
		out, _ := doorRoot(t, "ps -u "+user+" -o stat= | grep -v '^Z'")
		return strings.TrimSpace(out) != ""
	}
	doorAs(t, user, "setsid sh -c 'sleep 300' </dev/null >/dev/null 2>&1 &")
	eventually(t, startWaitE2E, "its process", running)
	if out, code := op(t, "/", "rm -f /tmp/opusids /tmp/opusids.pub && ssh-keygen -q -t ed25519 -N '' -C opusids-ci -f /tmp/opusids"); code != 0 {
		t.Fatalf("a second key (%d): %s", code, out)
	}
	if out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for "+name+" add /tmp/opusids.pub 2>&1"); code != 0 {
		t.Fatalf("adding the second key (%d): %s", code, out)
	}
	out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for "+name+" remove opusids-ci 2>&1")
	deadline := time.Now().Add(10 * time.Second)
	for running() && time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond) // A poll, bounded as eventually's, without its Fatal.
	}
	if running() {
		t.Errorf("BUG: a key's removal leaves the user's process running beside a container as 2147483646 (%d):\n%s", code, out)
	}
}

// BUG: the door's own registry clients on the host (a claim's manifest read, buildx's
// imagetools; a build's token fetch) are held by the daemon's chain alone (doorJumps), which
// refuses only loopback and the metadata services: a registry the project user names can send
// them, by a token's realm or a redirect, to a container of any project on any Docker
// network, which a project user's own processes (DEVOPSY-PROJECTS: -o docker0, -o br-+) and
// its containers cannot reach. Docker's daemon does the same for a pull, and the door refuses
// no registry host at a container's address. The control, TestDoorClientsReachNoLoopback,
// shows the same realm on loopback held.
func TestReviewOpusDoorClientsReachContainers(t *testing.T) {
	user := doorUser(t, "opusrealm")
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f opusrealm-int >/dev/null 2>&1; docker network rm opusrealm-net >/dev/null 2>&1; true")
	})
	// Another project's container, on a network of its own, which no project user is on: it
	// logs every request.
	ip, code := doorRoot(t, `set -e
docker network create opusrealm-net >/dev/null
docker run -d --name opusrealm-int --network opusrealm-net busybox:1.37 sh -c 'mkdir -p /w && exec httpd -f -vv -p 5032 -h /w' >/dev/null
ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' opusrealm-int)
for i in $(seq 150); do curl -s -o /dev/null "http://$ip:5032/ready" && { echo "$ip"; exit 0; }; sleep 0.2; done
exit 1`)
	ip = strings.TrimSpace(ip)
	if code != 0 || ip == "" {
		t.Fatalf("the other project's container (%d): %s", code, ip)
	}
	// The control: the project user's own process reaches it not.
	if out, code := doorAs(t, user, "curl -s -m 5 -o /dev/null -w '%{http_code}' http://"+ip+":5032/direct"); code == 0 {
		t.Fatalf("the control, the project user's own curl reached %s: %s", ip, out)
	}
	reg := doorRegistry(t, "opusrealm-reg", "5031", false, "-e REGISTRY_AUTH=token",
		"-e REGISTRY_AUTH_TOKEN_REALM=http://"+ip+":5032/token", "-e REGISTRY_AUTH_TOKEN_SERVICE=r",
		"-e REGISTRY_AUTH_TOKEN_ISSUER=r", "-e REGISTRY_AUTH_TOKEN_ROOTCERTBUNDLE=/certs/c.pem")
	out, _ := doorAs(t, user, "docker image inspect "+reg+"/x:1 2>&1")
	if logs, _ := doorRoot(t, "docker logs opusrealm-int 2>&1"); strings.Contains(logs, "/token") {
		t.Errorf("BUG: a claim's read asked another project's container, on a network the project is not on, for a token:\n%s\nit said:\n%s", logs, out)
	}
	if strings.Contains(out, "/direct") {
		t.Errorf("the control leaked into the claim's answer: %s", out)
	}
}
