//go:build e2e

package e2e

import (
	"strconv"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/uids"
)

// reach is what a process as uid reaches of container's first process through /proc, run as
// root on a server (srvRoot, doorRoot) dropping to uid as its gid too, with no capability
// and no_new_privs: an escape's best case. Its environment, a file through its root, and a
// signal.
func reach(t *testing.T, root func(*testing.T, string) (string, int), uid, container string) string {
	t.Helper()
	out, code := root(t, `pid=$(docker inspect -f '{{.State.Pid}}' `+container+`) && [ "$pid" -gt 0 ] || exit 1
setpriv --reuid `+uid+` --regid `+uid+` --clear-groups --inh-caps=-all --bounding-set=-all --no-new-privs sh -c '
{ tr "\0" "\n" </proc/'$pid'/environ; } 2>/dev/null | grep -q SECRET= && echo environ
[ -r /proc/'$pid'/root/etc/hostname ] && cat /proc/'$pid'/root/etc/hostname >/dev/null 2>&1 && echo root
kill -0 '$pid' 2>/dev/null && echo signal
true'`)
	if code != 0 {
		t.Fatalf("reading %s's process as %s (%d): %s", container, uid, code, out)
	}
	return strings.Join(strings.Fields(out), " ")
}

// uidOf is the uid container runs as, from its user: (uid:gid, one number), checked to be in
// the range of the domain it should be.
func uidOf(t *testing.T, root func(*testing.T, string) (string, int), container string, d uids.Domain) string {
	t.Helper()
	out, _ := root(t, "docker inspect -f '{{.Config.User}}' "+container)
	uid, gid, _ := strings.Cut(strings.TrimSpace(out), ":")
	n, err := strconv.ParseUint(uid, 10, 32)
	start, size := uids.Range(d)
	if err != nil || uid != gid || uint32(n) < start || uint32(n)-start >= size {
		t.Fatalf("%s runs as %q, not one uid of its range %d..%d", container, out, start, start+size-1)
	}
	return uid
}

const uidsService = `services:
  web:
    image: busybox:1.37
    user: "${DEVOPSY_UID:?}:${DEVOPSY_UID:?}"
    command: [sleep, infinity]
    environment: {SECRET: s3cret}
`

// uidsProject is a deploy user's project whose service runs as its environment's uid.
func uidsProject(t *testing.T, name, compose string) string {
	return project(t, name, map[string]string{
		".devopsy/.env":            "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml":     "project: " + name + "\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {}\n",
		".devopsy/compose.yaml":    compose,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
}

// Deploy-user projects on their claimed uids: each environment its own, another project's and
// another environment's of the same project, so a process escaped from one reaches nothing of
// the other's through /proc, which the same uid would (the control). A uid another owner
// holds sends a first claim to the next slot; a rollback keeps the uid; DEVOPSY_PROJECT_UID
// is one for every environment of the project, as documented.
func TestUIDsKeepEnvironmentsApart(t *testing.T) {
	a := uidsProject(t, "uidsapa", uidsService)
	b := uidsProject(t, "uidsapb", uidsService)
	for _, r := range []struct{ dir, at string }{{a, "@prod"}, {a, "@dev"}, {b, "@prod"}} {
		if out, code := op(t, r.dir, "devopsy "+r.at+" --release"); code != 0 {
			t.Fatalf("%s %s (%d):\n%s", r.dir, r.at, code, out)
		}
	}
	prod := uidOf(t, srvRoot, "uidsapa-prod-web-1", uids.EnvDomain)
	dev := uidOf(t, srvRoot, "uidsapa-dev-web-1", uids.EnvDomain)
	other := uidOf(t, srvRoot, "uidsapb-prod-web-1", uids.EnvDomain)
	if prod == dev || prod == other || dev == other {
		t.Fatalf("uids shared: prod %s, dev %s, another project's %s", prod, dev, other)
	}
	if got := reach(t, srvRoot, prod, "uidsapa-prod-web-1"); got != "environ root signal" {
		t.Fatalf("the control: its own uid reaches %q of it", got)
	}
	for who, uid := range map[string]string{"another project's": other, "another environment's": dev} {
		if got := reach(t, srvRoot, uid, "uidsapa-prod-web-1"); got != "" {
			t.Errorf("%s uid reaches %q of prod's container", who, got)
		}
	}
	if out, _ := op(t, a, "devopsy @prod --env"); !strings.Contains(out, cli.UIDEnv+"='"+prod+"'") {
		t.Errorf("--env does not show the uid %s:\n%s", prod, out)
	}

	// A rollback runs the release it goes back to on the same uid.
	if out, code := op(t, a, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a second release (%d):\n%s", code, out)
	}
	if out, code := op(t, a, "devopsy @prod --rollback --yes"); code != 0 {
		t.Fatalf("a rollback (%d):\n%s", code, out)
	}
	if got := uidOf(t, srvRoot, "uidsapa-prod-web-1", uids.EnvDomain); got != prod {
		t.Errorf("after the rollback it runs as %s, was %s", got, prod)
	}

	// A uid another owner holds: the first claim takes the next slot.
	c := uidsProject(t, "uidsapc", uidsService)
	o := uids.Owner{Project: "uidsapc", Env: "prod"}
	if out, code := srv(t, "mkdir -p "+cli.UIDsDir+" && ln -s "+strconv.FormatUint(uint64(o.Plain()), 10)+" "+cli.UIDsDir+"/env:uidsapcx:prod"); code != 0 {
		t.Fatal(out)
	}
	if out, code := op(t, c, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a release on a held slot (%d):\n%s", code, out)
	}
	if got := uidOf(t, srvRoot, "uidsapc-prod-web-1", uids.EnvDomain); got != strconv.FormatUint(uint64(o.Plain()+1), 10) {
		t.Errorf("on a held slot it runs as %s, want the next, %d", got, o.Plain()+1)
	}

	// The project's uid, one for all its environments.
	p := uidsProject(t, "uidsapp", strings.ReplaceAll(uidsService, "DEVOPSY_UID", cli.ProjectUIDEnv))
	for _, at := range []string{"@prod", "@dev"} {
		if out, code := op(t, p, "devopsy "+at+" --release"); code != 0 {
			t.Fatalf("%s (%d):\n%s", at, code, out)
		}
	}
	if a, b := uidOf(t, srvRoot, "uidsapp-prod-web-1", uids.ProjectDomain), uidOf(t, srvRoot, "uidsapp-dev-web-1", uids.ProjectDomain); a != b {
		t.Errorf("the project's uid differs: prod %s, dev %s", a, b)
	}

	// --destroy frees its environment's claim.
	if out, code := op(t, b, "devopsy @prod --destroy --yes"); code != 0 {
		t.Fatalf("--destroy (%d):\n%s", code, out)
	}
	if out, _ := srv(t, "ls "+cli.UIDsDir); strings.Contains(out, "env:uidsapb:prod") || strings.Contains(out, "project:uidsapb") {
		t.Errorf("claims left after --destroy:\n%s", out)
	}
}

// Door projects on their claimed uids: a process escaped from one project's container reaches
// nothing of another's, which the same uid would (the control), and the door refuses a file
// naming another environment's uid, even one its own release claimed.
func TestDoorUIDsKeepProjectsApart(t *testing.T) {
	compose := "services:\n  web:\n" + doorService + "    command: [sleep, infinity]\n    environment: {SECRET: s3cret}\n"
	a := doorProject(t, "dooruida", compose, nil)
	b := doorProject(t, "dooruidb", compose, nil)
	for _, dir := range []string{a, b} {
		if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
			t.Fatalf("%s (%d):\n%s", dir, code, out)
		}
	}
	ua := uidOf(t, doorRoot, "dooruida-prod-web-1", uids.EnvDomain)
	ub := uidOf(t, doorRoot, "dooruidb-prod-web-1", uids.EnvDomain)
	if ua == ub {
		t.Fatalf("one uid for both: %s", ua)
	}
	if got := reach(t, doorRoot, ua, "dooruida-prod-web-1"); got != "environ root signal" {
		t.Fatalf("the control: its own uid reaches %q of it", got)
	}
	if got := reach(t, doorRoot, ub, "dooruida-prod-web-1"); got != "" {
		t.Errorf("another project's uid reaches %q of its container", got)
	}
	// dooruida's own dev, claimed by its release, is refused for its prod's file.
	if out, code := op(t, a, "devopsy @dev --release"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	dev := uidOf(t, doorRoot, "dooruida-dev-web-1", uids.EnvDomain)
	for who, uid := range map[string]string{"another project's": ub, "its dev's": dev} {
		out, code := doorAs(t, "devopsy-dooruida", "cd ~/dooruida/prod/current/.devopsy && DEVOPSY_UID="+uid+" COMPOSE_PROJECT_NAME=dooruida-prod docker compose up -d 2>&1")
		if code == 0 || !strings.Contains(out, "not this environment's uid") {
			t.Errorf("prod's file with %s uid (%d):\n%s", who, code, out)
		}
	}
	// Its --destroy, through the door, frees its environment's claim; the project's stays
	// with dev's.
	if out, code := op(t, a, "devopsy @prod --destroy --yes"); code != 0 {
		t.Fatalf("--destroy (%d):\n%s", code, out)
	}
	if out, _ := doorRoot(t, "ls /home/devopsy/"+cli.UIDsDir); strings.Contains(out, "env:dooruida:prod") || !strings.Contains(out, "project:dooruida") {
		t.Errorf("the claims after prod's --destroy:\n%s", out)
	}
}

// A copy into a child gives its entries the child's own uid, a uid near 2 billion through
// rsync and tar alike: a sync's, and an export imported there.
func TestUIDsThroughCopies(t *testing.T) {
	dir := project(t, "uidscopy", map[string]string{
		".devopsy/.env":        "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml": "project: uidscopy\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n",
		".devopsy/compose.yaml": `services:
  data:
    image: busybox:1.37
    user: "${DEVOPSY_UID:?}:${DEVOPSY_UID:?}"
    command: [sh, -c, "mkdir -p /data/sub && echo $$HOSTNAME >/data/sub/f && exec sleep infinity"]
    volumes: [./mnt/data:/data]
    labels: {devopsy.data: "true", devopsy.data.version: any, devopsy.mnt.owner: user}
`,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	prod := uidOf(t, srvRoot, "uidscopy-prod-data-1", uids.EnvDomain)
	if got := owner(t, srv, "uidscopy/prod/shared/mnt/data/sub/f"); got != prod+":"+prod {
		t.Fatalf("prod's file is %s's, want its uid %s", got, prod)
	}
	if out, code := op(t, dir, "devopsy @dev --release"); code != 0 {
		t.Fatalf("dev, synced (%d):\n%s", code, out)
	}
	dev := uidOf(t, srvRoot, "uidscopy-dev-data-1", uids.EnvDomain)
	if got := owner(t, srv, "uidscopy/dev/shared/mnt/data/sub/f"); got != dev+":"+dev {
		t.Errorf("after the sync the child's file is %s's, want its uid %s", got, dev)
	}
	if out, code := op(t, dir, "devopsy @prod --export data >/tmp/uidscopy.tar && devopsy @dev --import data --yes </tmp/uidscopy.tar"); code != 0 {
		t.Fatalf("an export imported (%d):\n%s", code, out)
	}
	if got := owner(t, srv, "uidscopy/dev/shared/mnt/data/sub/f"); got != dev+":"+dev {
		t.Errorf("after the import the child's file is %s's, want its uid %s", got, dev)
	}
}
