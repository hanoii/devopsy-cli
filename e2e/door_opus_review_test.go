//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// BUG: a project user's containers reach the host's own addresses, which the project users'
// firewall keeps that user's processes off. DEVOPSY-PROJECTS rejects a project user's process
// at the host's addresses (they go out through loopback) but port 53, while DEVOPSY-CONTAINERS
// drops only the metadata services: a service of the host's listening on all its addresses (a
// monitoring agent, the admin's database, behind a cloud firewall that never sees a bridge) is
// one container away for the project user, who runs any command in a container of its own.
// The control: the project user's process does not reach it.
func TestOpusReviewDoorContainerReachesTheHostE2E(t *testing.T) {
	web := `    image: busybox:1.37
    user: "10001:10001"
    security_opt: [no-new-privileges]
    command: [sleep, "3600"]
`
	dir := doorProject(t, "opusgw", "services:\n  web:\n"+web, nil)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a release (%d):\n%s", code, out)
	}
	t.Cleanup(func() { doorRoot(t, "docker rm -f opusgw-host") })
	// The admin's own service, on all the host's addresses.
	if out, code := doorRoot(t, `docker run -d --name opusgw-host --network host busybox:1.37 sh -c 'mkdir -p /w && echo host-only-service >/w/index.html && exec httpd -f -p 18181 -h /w' >/dev/null`); code != 0 {
		t.Fatalf("the host's service (%d): %s", code, out)
	}
	addr, _ := doorRoot(t, "hostname -i | cut -d' ' -f1")
	url := "http://" + strings.TrimSpace(addr) + ":18181/"
	eventually(t, startWaitE2E, "the host's service answering the deploy user", func() bool {
		out, _ := doorAs(t, "devopsy", "curl -s --max-time 3 "+url)
		return strings.Contains(out, "host-only-service")
	})
	// Control: the project user's own process is kept off it.
	if out, _ := doorAs(t, "devopsy-opusgw", "curl -s --max-time 3 "+url); strings.Contains(out, "host-only-service") {
		t.Fatalf("control: the project user's process reached the host's service: %s", out)
	}
	if out, _ := op(t, dir, "devopsy @prod exec -T web wget -qO- -T 3 "+url+" 2>&1"); strings.Contains(out, "host-only-service") {
		t.Errorf("BUG: the project user's container reached %s, the host's own address its processes are kept off: %s", url, out)
	}
}

