//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

const startWaitE2E = time.Minute

// An attack round on project users (step 2), by one reviewer: what a project user reaches
// on a real server, each with its control.

// BUG: --project-users add takes a project the deploy user already runs on the server (the
// same name is let through, "a release root's project moving to its own user"), and the
// door then answers for every container under its compose names by their labels alone. A
// container the deploy user made, never checked by the door (root, any mount), is reached
// by the project user's `docker compose exec`, which runs as that container's own user.
// Here the deploy user's old container runs as root with the host's /etc: the project user
// reads /etc/shadow through it. add should refuse while anything the door did not make is
// under the project's names (or the project's directory in the release root is still the
// deploy user's), the deploy user's --destroy first.
func TestDoorOpusReviewAddTakesTheDeployUsersContainers(t *testing.T) {
	const project = "adoptopus"
	user := "devopsy-" + project
	name := doorServer(t)
	deploy := func(script string) (string, int) {
		t.Helper()
		return execIn(t, []string{"-u", "devopsy", "-w", "/home/devopsy", name}, script)
	}
	t.Cleanup(func() {
		deploy("cd /home/devopsy/" + project + "/old && docker compose -p " + project + "-prod down >/dev/null 2>&1; rm -rf /home/devopsy/" + project)
		op(t, "/", "devopsy @devopsy@doorsrv: --project-users remove "+project)
		doorRoot(t, "pkill -u "+user+"; userdel -r "+user+" 2>/dev/null; true")
	})
	// The deploy user's own project, before it had a user: its environment's directory in
	// the release root, and its container, which compose made, root with the host's /etc.
	if out, code := deploy(`set -e
mkdir -p ` + project + `/prod/releases ` + project + `/old
cd ` + project + `/old
printf 'services:\n  web:\n    image: busybox:1.37\n    command: [sleep, infinity]\n    volumes: [/etc:/hostetc:ro]\n' >compose.yaml
docker compose -p ` + project + `-prod up -d --quiet-pull 2>&1`); code != 0 {
		t.Fatalf("the deploy user's own project (%d): %s", code, out)
	}
	out, code := op(t, "/", "devopsy @devopsy@doorsrv: --project-users add "+project+" 2>&1")
	if code != 0 {
		t.Logf("add refused (the fix): %s", out)
		return
	}
	// add went through: what the project user reaches now, by a file the door checks.
	compose := `name: ` + project + `-prod
services:
  web:
` + doorService + `    command: [sleep, infinity]
`
	exploit := `set -e
mkdir -p ~/` + project + `/prod/x
cd ~/` + project + `/prod/x
printf '%s' ` + quote(compose) + ` >compose.yaml
docker compose exec -T web sh -c 'id -u; head -n 1 /hostetc/shadow'`
	got, code := doorAs(t, user, exploit)
	if code == 0 && strings.Contains(got, "root:") {
		t.Errorf("BUG: --project-users add took a project the deploy user runs, and its user reads the host's /etc/shadow as root through the deploy user's container:\n%s", got)
		return
	}
	t.Errorf("add took a project the deploy user runs (%s), and the exec did not reach its container (%d): %s", strings.TrimSpace(out), code, got)
}

// BUG: the door's firewall rule is IPv4 only (DoorFirewall, --server-info, docs/servers.md),
// while the rules let a project's network have IPv6 (topNetworkKeys' enable_ipv6), which
// Docker gives a subnet of its own pools with no ipam: another project user then reaches
// that project's containers from the host over IPv6, past the rule. The control: the same
// container's IPv4 address is refused under the rule. The same holds for a metadata service
// on IPv6 (fd00:ec2::254). The rule should cover ip6tables too, or the rules refuse
// enable_ipv6.
func TestDoorOpusReviewFirewallMissesIPv6(t *testing.T) {
	name := doorServer(t)
	prev, _ := doorRoot(t, "sysctl -n net.ipv6.conf.all.disable_ipv6")
	prevDefault, _ := doorRoot(t, "sysctl -n net.ipv6.conf.default.disable_ipv6")
	if out, code := doorRoot(t, "sysctl -qw net.ipv6.conf.all.disable_ipv6=0 net.ipv6.conf.default.disable_ipv6=0"); code != 0 {
		t.Skipf("no IPv6 on %s: %s", name, out)
	}
	rules := []string{"-o docker0", "-o br-+", "-d 169.254.169.254/32"}
	t.Cleanup(func() {
		for _, r := range rules {
			doorRoot(t, "iptables -D OUTPUT -m owner --gid-owner devopsy-projects --suppl-groups "+r+" -j REJECT")
		}
		doorRoot(t, "docker rm -f doorv6opus-prod-web-1; docker network rm doorv6opus-prod_default")
		doorRoot(t, "sysctl -qw net.ipv6.conf.all.disable_ipv6="+strings.TrimSpace(prev)+" net.ipv6.conf.default.disable_ipv6="+strings.TrimSpace(prevDefault))
	})
	dir := doorProject(t, "doorv6opus", `services:
  web:
`+doorService+`    command: [sh, -c, "mkdir -p /tmp/w && echo hello-v6 >/tmp/w/index.html && exec httpd -f -p 8080 -h /tmp/w"]
networks:
  default:
    enable_ipv6: true
`, nil)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a release with an IPv6 network (%d):\n%s", code, out)
	}
	doorUser(t, "doorv6opuso")
	addrs, _ := doorRoot(t, "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{.GlobalIPv6Address}}{{end}}' doorv6opus-prod-web-1")
	f := strings.Fields(addrs)
	if len(f) != 2 {
		t.Skipf("Docker gave the network no IPv6 subnet here: %q", addrs)
	}
	v4, v6 := "http://"+f[0]+":8080/", "http://["+f[1]+"]:8080/"
	ask := func(url string) string {
		t.Helper()
		out, _ := doorAs(t, "devopsy-doorv6opuso", "curl -s --max-time 3 '"+url+"'")
		return out
	}
	if !strings.Contains(ask(v6), "hello-v6") || !strings.Contains(ask(v4), "hello-v6") {
		t.Fatalf("the control: with no rule, a project user reaches no site directly (%s, %s), so this proves nothing", v4, v6)
	}
	for _, r := range rules {
		if out, code := doorRoot(t, "iptables -I OUTPUT -m owner --gid-owner devopsy-projects --suppl-groups "+r+" -j REJECT"); code != 0 {
			t.Fatal(out)
		}
	}
	if strings.Contains(ask(v4), "hello-v6") {
		t.Fatal("the control: the rule lets a project user reach another's site over IPv4")
	}
	if strings.Contains(ask(v6), "hello-v6") {
		t.Errorf("BUG: under the door's firewall rule, a project user reaches another project's container over IPv6 (%s), which the rules let a network have", v6)
	}
}

// BUG: the metadata service is kept off a project user's own traffic on the host (an OUTPUT
// rule by its group), never off its containers': a container's traffic is forwarded, or
// delivered, by no process of the group, so any door project's container reads the cloud's
// metadata service (an instance's credentials). Here a stand-in answers at 169.254.169.254
// on the host. The control: a project user on the host is refused under the rule. The rule
// should hold containers too (for example in DOCKER-USER, by Docker's bridges, and INPUT).
func TestDoorOpusReviewContainersReachTheMetadataService(t *testing.T) {
	name := doorServer(t)
	rules := []string{"-o docker0", "-o br-+", "-d 169.254.169.254/32"}
	t.Cleanup(func() {
		for _, r := range rules {
			doorRoot(t, "iptables -D OUTPUT -m owner --gid-owner devopsy-projects --suppl-groups "+r+" -j REJECT")
		}
		doorRoot(t, "docker rm -f doorimdsopus-md; ip link del imdsopus0")
	})
	if out, code := doorRoot(t, `set -e
ip link add imdsopus0 type dummy
ip addr add 169.254.169.254/32 dev imdsopus0
ip link set imdsopus0 up
docker run -d --name doorimdsopus-md --network host busybox:1.37 sh -c 'mkdir -p /w && echo instance-credentials >/w/index.html && exec httpd -f -p 169.254.169.254:80 -h /w' >/dev/null`); code != 0 {
		t.Fatalf("a metadata stand-in on %s (%d): %s", name, code, out)
	}
	dir := doorProject(t, "doorimdsopus", "services:\n  web:\n"+doorService+"    command: [sleep, infinity]\n", nil)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a release (%d):\n%s", code, out)
	}
	for _, r := range rules {
		if out, code := doorRoot(t, "iptables -I OUTPUT -m owner --gid-owner devopsy-projects --suppl-groups "+r+" -j REJECT"); code != 0 {
			t.Fatal(out)
		}
	}
	eventually(t, startWaitE2E, "the stand-in answering", func() bool {
		out, _ := doorRoot(t, "curl -s --max-time 3 http://169.254.169.254/")
		return strings.Contains(out, "instance-credentials")
	})
	if out, _ := doorAs(t, "devopsy-doorimdsopus", "curl -s --max-time 3 http://169.254.169.254/"); strings.Contains(out, "instance-credentials") {
		t.Fatal("the control: the rule lets a project user on the host read the metadata service")
	}
	out, _ := op(t, dir, "devopsy @prod exec -T web wget -qO- -T 3 http://169.254.169.254/ 2>&1")
	if strings.Contains(out, "instance-credentials") {
		t.Errorf("BUG: a door project's container reads the metadata service under the door's rule: %s", out)
	}
}
