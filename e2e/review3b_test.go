//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// BUG: on a host with newuidmap and newgidmap (the uidmap package, which rootless tools
// need), a project user's process leaves the firewall. useradd gives the user subordinate
// ids, so in a user namespace its process takes one of them and drops its groups, and the
// firewall goes by the devopsy-projects group of a socket's owner (--gid-owner, with
// --suppl-groups), which that process no longer has: it reaches the host's loopback, and so
// Docker's bridges and a metadata service.
//
// A project user's process reaches nothing the firewall closes, whatever ids the host lets
// it take. Skipped where uidmap is not there and cannot be installed.
func TestDoorFirewallHoldsUnderSubordinateIDs(t *testing.T) {
	user := doorUser(t, "doorr3bsub")
	if out, code := doorRoot(t, "command -v newgidmap >/dev/null && exit 0; apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq --no-install-recommends uidmap >/dev/null 2>&1 && touch /tmp/doorr3bsub-uidmap"); code != 0 {
		t.Skipf("no uidmap on the door's server, and none to install (%d): %s", code, out)
	}
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f doorr3bsub-loopback; [ ! -e /tmp/doorr3bsub-uidmap ] || { apt-get remove -y -qq uidmap; rm -f /tmp/doorr3bsub-uidmap; }; true")
	})
	if out, code := doorRoot(t, "docker run -d --name doorr3bsub-loopback -p 127.0.0.1:18081:8080 busybox:1.37 sh -c 'mkdir -p /w && echo hello-from-loopback >/w/index.html && exec httpd -f -p 8080 -h /w' >/dev/null"); code != 0 {
		t.Fatalf("the admin's port on the loopback (%d): %s", code, out)
	}
	site := "curl -s --max-time 3 http://127.0.0.1:18081/"
	eventually(t, startWaitE2E, "the port answering the deploy user", func() bool {
		out, _ := doorAs(t, "devopsy", site)
		return strings.Contains(out, "hello-from-loopback")
	})
	if out, _ := doorAs(t, user, site); strings.Contains(out, "hello-from-loopback") {
		t.Fatalf("the control: the firewall lets a project user's plain process through: %s", out)
	}
	// A subordinate uid and gid, and no groups: all the user's own to take.
	hidden := "unshare --user --map-root-user --map-users=auto --map-groups=auto --setgroups allow setpriv --reuid 1 --regid 1 --clear-groups "
	if out, _ := doorAs(t, user, hidden+site+" 2>&1"); strings.Contains(out, "hello-from-loopback") {
		t.Errorf("under a subordinate id with its groups dropped, a project user's process reached the host's loopback: %s", out)
	}
}

// BUG: a project user reads the projects the deploy user runs, wherever it can enter the
// release root. A release makes <root>/<project> and all under it with the session's umask
// (mkdir -p "$base/releases" "$base/shared", 755), so nothing but the deploy user's home
// keeps a project user's shell out of them, and with releases: root elsewhere (a block
// volume) the root is every project user's to enter, their own trees being in it. What a
// container of the deploy user's writes in its entries with modes open to others (a
// registry's blobs, its htpasswd, a site's uploads) is then that project user's to read.
// The test server's home is open (a tmpfs, mode 755), as such a root is.
//
// A project the deploy user runs is closed to project users, whatever its root's mode.
func TestDoorProjectUsersReadNoneOfTheDeployUsers(t *testing.T) {
	peek := doorUser(t, "doorr3bpeek")
	dir := project(t, "doorr3bown", map[string]string{
		".devopsy/.env":            "DEVOPSY_SERVER=devopsy@doorsrv\n",
		".devopsy/config.yaml":     "project: doorr3bown\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n",
		".devopsy/compose.yaml":    "services:\n  web:\n    image: busybox:1.37\n    volumes: [./mnt/data:/d]\n    command: [sh, -c, \"echo s3cret >/d/readable && exec sleep infinity\"]\n",
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	base := "/home/devopsy/doorr3bown"
	t.Cleanup(func() {
		doorRoot(t, "docker compose -p doorr3bown-prod down -v >/dev/null 2>&1; rm -rf "+base)
	})
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("the deploy user's release (%d):\n%s", code, out)
	}
	secret := base + "/prod/shared/mnt/data/readable"
	eventually(t, startWaitE2E, "what its container writes", func() bool {
		out, _ := doorRoot(t, "cat "+secret+" 2>/dev/null")
		return strings.Contains(out, "s3cret")
	})
	if out, _ := doorAs(t, peek, "cat "+secret+" 2>&1"); strings.Contains(out, "s3cret") {
		modes, _ := doorRoot(t, "stat -c '%a %U %n' /home/devopsy "+base+" "+base+"/prod "+base+"/prod/shared "+base+"/prod/shared/mnt/data "+secret)
		t.Errorf("a project user read what a project of the deploy user's wrote in its entry:\n%s", modes)
	}
	if out, code := doorAs(t, peek, "ls "+base+"/prod/current/ 2>&1"); code == 0 {
		t.Errorf("a project user listed a release of the deploy user's: %s", out)
	}
}
