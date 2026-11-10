//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// BUG: Docker's daemon is past its chain once its cgroup is made again under the same path,
// which is every restart of docker.service (an upgrade of Docker, a change of daemon.json):
// systemd removes the unit's cgroup when it stops and makes one when it starts. The jump
// (-m cgroup --path) holds the cgroup that was there when it went in, never the path, so it
// matches nothing of the new daemon, while `iptables -S` prints the same line and
// DoorFirewallWrong, which compares lines, says the firewall is devopsy's. The daemon then
// contacts the host's loopback and the metadata services for a project user's pull or login
// until the loader puts the jump in again: the timer's next run, five minutes at most, and
// only from the first boot after `add` (the install enables the timer and starts nothing),
// before which nothing loads it again.
//
// The test server has no systemd, so the test does to the cgroup what a restart does: the
// daemon's processes out, the cgroup removed and made again, the processes back. The loader
// run once more, as its cleanup does, holds the daemon again.
func TestDoorDaemonFirewallHoldsOnceDockersCgroupIsMadeAgain(t *testing.T) {
	user := doorUser(t, "fablecg")
	const reg, host, port = "fablecg-reg", "fablecg-reg.test", "5017"
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f "+reg+" >/dev/null 2>&1; grep -v "+reg+" /etc/hosts >/tmp/fablecg-hosts && cat /tmp/fablecg-hosts >/etc/hosts; rm -f /tmp/fablecg-hosts; rmdir /sys/fs/cgroup/fablecg-away 2>/dev/null; /usr/local/libexec/devopsy-firewall")
	})
	if out, code := doorRoot(t, `set -e
echo '127.0.0.1 `+host+`' >>/etc/hosts
docker run -d --name `+reg+` --network host -e REGISTRY_HTTP_ADDR=127.0.0.1:`+port+` registry:3 >/dev/null
for i in $(seq 150); do curl -s -o /dev/null http://`+host+`:`+port+`/v2/ && exit 0; sleep 0.2; done
exit 1`); code != 0 {
		t.Fatalf("the registry on loopback (%d): %s", code, out)
	}
	// reached says whether the daemon logged in to the registry on loopback for the project user.
	reached := func() (string, bool) {
		out, code := doorAs(t, user, "printf x | docker login -u u --password-stdin "+host+":"+port+" 2>&1")
		return out, code == 0 && strings.Contains(out, "Login Succeeded")
	}
	// The control: the daemon is held as the firewall was loaded.
	if out, ok := reached(); ok {
		t.Fatalf("the control: the daemon reached the host's loopback with its jump just loaded:\n%s", out)
	}
	before, _ := doorRoot(t, "iptables -S OUTPUT | grep DEVOPSY-DOCKERD")
	if out, code := doorRoot(t, `set -e
cg=/sys/fs/cgroup
mkdir $cg/fablecg-away
# move <from> <to>: every process of one cgroup into the other, a child started meanwhile too.
move() {
  for i in 1 2 3 4 5 6 7 8 9 10; do
    procs=$(cat $cg/$1/cgroup.procs)
    [ -n "$procs" ] || return 0
    for p in $procs; do echo "$p" >$cg/$2/cgroup.procs 2>/dev/null || :; done
  done
  return 1
}
move dockerd fablecg-away
rmdir $cg/dockerd
mkdir $cg/dockerd
move fablecg-away dockerd
rmdir $cg/fablecg-away
grep -q "$(pidof -s dockerd)" $cg/dockerd/cgroup.procs`); code != 0 {
		t.Fatalf("the daemon's cgroup made again (%d): %s", code, out)
	}
	after, _ := doorRoot(t, "iptables -S OUTPUT | grep DEVOPSY-DOCKERD")
	if before != after || strings.TrimSpace(after) == "" {
		t.Fatalf("the daemon's jump as iptables prints it changed: before\n%s\nafter\n%s", before, after)
	}
	out, ok := reached()
	info, _ := op(t, "/", "devopsy @devopsy@doorsrv: --server-info 2>&1")
	if ok && !strings.Contains(info, "firewall") {
		t.Errorf("its cgroup made again under the same path, the daemon logged in to a registry on the host's loopback for a project user, and --server-info says nothing of the firewall:\n%s\nthe jump, unchanged:\n%s\n--server-info:\n%s", out, after, info)
	}
	// The loader, run again, holds it: the jump goes in for the cgroup there now.
	if out, code := doorRoot(t, "/usr/local/libexec/devopsy-firewall"); code != 0 {
		t.Fatalf("the loader (%d): %s", code, out)
	}
	if out, ok := reached(); ok {
		t.Errorf("the loader run again, the daemon still reached the host's loopback:\n%s", out)
	}
}
