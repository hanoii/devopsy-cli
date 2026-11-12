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

// claimed is the uid an owner's claim holds on the server (env:<project>:<env>,
// project:<project>), under the deploy user's release root.
func claimed(t *testing.T, owner string) string {
	t.Helper()
	out, code := srv(t, "readlink "+quote(".devopsy-uids/"+owner))
	if code != 0 {
		t.Fatalf("the claim of %s: %s", owner, out)
	}
	return strings.TrimSpace(out)
}

// A copy into a child gives its files the child's own uids, a uid near 2 billion through
// rsync and tar alike: a sync's, and an export imported there. An owned entry's all go to
// its owner; an unowned entry's and a volume's (owned, its root alone) by range, a file on
// an environment's uid to the child's DEVOPSY_UID, one on a project's to its
// DEVOPSY_PROJECT_UID, uid and gid apart, and every other owner kept.
func TestUIDsThroughCopies(t *testing.T) {
	dir := project(t, "uidscopy", map[string]string{
		".devopsy/.env":        "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml": "project: uidscopy\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n",
		".devopsy/compose.yaml": `services:
  data:
    image: busybox:1.37
    user: "${DEVOPSY_UID:?}:${DEVOPSY_UID:?}"
    command: [sh, -c, "mkdir -p /data/sub && echo $$HOSTNAME >/data/sub/f && echo $$HOSTNAME >/vol/f && exec sleep infinity"]
    volumes: [./mnt/data:/data, ./mnt/files:/files:ro, "vol:/vol:nocopy"]
    labels: {devopsy.data: "true", devopsy.data.version: any, devopsy.mnt.owner: user}
volumes:
  vol: {}
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
	// Files of an unowned entry and of the volume on another project's uid, on a mix, and on
	// owners of no range of devopsy's.
	if out, code := srvRoot(t, `set -e; cd /home/devopsy/uidscopy/prod/shared/mnt; mkdir -p files; touch files/e files/p files/o files/m
chown `+prod+`:`+prod+` files/e; chown 1900000001:1900000001 files/p; chown 7:7 files/o; chown 1950000002:`+prod+` files/m
docker run --rm -v uidscopy-prod_vol:/v busybox:1.37 sh -c 'touch /v/p /v/o && chown 1900000001:1900000001 /v/p && chown 7:7 /v/o'`); code != 0 {
		t.Fatalf("prod's files: %s", out)
	}
	if got, _ := srvRoot(t, "docker run --rm -v uidscopy-prod_vol:/v busybox:1.37 stat -c %u:%g /v/f"); strings.TrimSpace(got) != prod+":"+prod {
		t.Fatalf("prod's volume written at its first start as %s, want its uid %s", got, prod)
	}
	project := claimed(t, "project:uidscopy")
	check := func(what, dev string) {
		t.Helper()
		if got := owner(t, srv, "uidscopy/dev/shared/mnt/data/sub/f"); got != dev+":"+dev {
			t.Errorf("after %s the child's owned file is %s's, want its uid %s", what, got, dev)
		}
		for f, want := range map[string]string{"e": dev + ":" + dev, "p": project + ":" + project, "o": "7:7", "m": project + ":" + dev} {
			if got := owner(t, srv, "uidscopy/dev/shared/mnt/files/"+f); got != want {
				t.Errorf("after %s the child's files/%s is %s's, want %s", what, f, got, want)
			}
		}
		got, _ := srvRoot(t, "docker run --rm -v uidscopy-dev_vol:/v busybox:1.37 stat -c '%n %u:%g' /v /v/f /v/p /v/o")
		if want := "/v " + dev + ":" + dev + " /v/f " + dev + ":" + dev + " /v/p " + project + ":" + project + " /v/o 7:7"; strings.Join(strings.Fields(got), " ") != want {
			t.Errorf("after %s the child's volume: %q, want %q", what, got, want)
		}
	}
	if out, code := op(t, dir, "devopsy @dev --release"); code != 0 {
		t.Fatalf("dev, synced (%d):\n%s", code, out)
	}
	dev := uidOf(t, srvRoot, "uidscopy-dev-data-1", uids.EnvDomain)
	if dev == prod || dev != claimed(t, "env:uidscopy:dev") {
		t.Fatalf("dev's uid %s, prod's %s", dev, prod)
	}
	check("the sync", dev)
	if out, code := op(t, dir, "devopsy @prod --export data >/tmp/uidscopy.tar && devopsy @dev --import data --yes </tmp/uidscopy.tar"); code != 0 {
		t.Fatalf("an export imported (%d):\n%s", code, out)
	}
	check("the import", dev)
}

// A named volume devopsy.mnt.owner owns is its service's from its first start, on its
// environment's uid near 2 billion, mounted with nocopy where the image has a directory of
// root's (/var/www): the release makes the volume and gives its root before its run step,
// and nocopy keeps Docker from filling it, and so from giving its root the image's owner.
// Compose's own config is what says nocopy, short syntax and long: a mount without it, a
// read-only one too, refuses the release before its switch.
func TestUIDsOwnedVolumeFirstStart(t *testing.T) {
	compose := func(reader string) string {
		return `services:
  db:
    image: busybox:1.37
    user: "${DEVOPSY_UID:?}:${DEVOPSY_UID:?}"
    command: [sh, -c, "echo up >/var/www/f && exec sleep infinity"]
    volumes: ["dbdata:/var/www:nocopy"]
    labels: {devopsy.mnt.owner: user}
  reader:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes:
      - type: volume
        source: dbdata
        target: /backup
        read_only: true
` + reader + `volumes:
  dbdata: {}
`
	}
	dir := uidsProject(t, "uidsvol", compose(""))
	out, code := op(t, dir, "devopsy @prod --release")
	if code == 0 || !strings.Contains(out, "reader mounts the volume dbdata at /backup without nocopy") || strings.Contains(out, "current is now") {
		t.Fatalf("a read-only mount of an owned volume without nocopy (%d):\n%s", code, out)
	}
	if got, _ := srvRoot(t, "docker volume ls -q --filter name=uidsvol-prod_dbdata"); strings.TrimSpace(got) != "" {
		t.Errorf("the refused release made the volume: %s", got)
	}
	if out, code := op(t, dir, "printf '%s' "+quote(compose("        volume: {nocopy: true}\n"))+" >.devopsy/compose.yaml"); code != 0 {
		t.Fatalf("the compose file: %s", out)
	}
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 || !strings.Contains(out, "giving the volume dbdata's root to") {
		t.Fatalf("prod, its owned volume written at its first start (%d):\n%s", code, out)
	}
	uid := uidOf(t, srvRoot, "uidsvol-prod-db-1", uids.EnvDomain)
	if got, _ := srvRoot(t, "docker run --rm -v uidsvol-prod_dbdata:/v busybox:1.37 stat -c '%u:%g %a' /v /v/f"); strings.Join(strings.Fields(got), " ") != uid+":"+uid+" 755 "+uid+":"+uid+" 644" {
		t.Errorf("the volume's root and file: %q, want both %s's", got, uid)
	}
}
