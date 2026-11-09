//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

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
