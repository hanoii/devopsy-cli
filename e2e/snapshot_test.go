//go:build e2e

package e2e

import (
	"regexp"
	"strings"
	"testing"
)

// snapshotProject is a database in a named volume and an app's files in
// an entry of mnt, both with data; prod keeps a snapshot mirror when
// mirror says so.
func snapshotProject(t *testing.T, name string, mirror bool) string {
	prod := "{}"
	if mirror {
		prod = "{snapshots: {mirror: true}}"
	}
	return project(t, name, map[string]string{
		".devopsy/.env":        "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml": "project: " + name + "\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: " + prod + "\n",
		".devopsy/compose.yaml": `services:
  db:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [dbdata:/data]
    labels: {devopsy.data: "true", devopsy.data.copy: none}
  app:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [./mnt/files:/files]
    depends_on: [db]
    labels: {devopsy.data: "true"}
volumes:
  dbdata: {}
`,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
}

var takenID = regexp.MustCompile(`snapshot ([0-9]{14}) taken`)

// A manual take and a full restore with real Docker: the data of every service with data
// (a named volume whatever its copy label, an entry of mnt), as
// zstd-compressed tars any tar reads, the services running again after.
func TestSnapshotTakeRestore(t *testing.T) {
	// No mirror (the default): the tars written from the stopped data.
	dir := snapshotProject(t, "snaptake", false)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	// Both written as root, as the services would (Docker made the
	// entry root's: the app has no owner label).
	if out, code := srv(t, "docker run --rm -v snaptake-prod_dbdata:/d -v \"$PWD/snaptake/prod/shared/mnt/files:/f\" busybox:1.37 sh -c 'echo rows >/d/table && echo pic >/f/a.jpg'"); code != 0 {
		t.Fatalf("writing the data: %s", out)
	}
	out, code := op(t, dir, "devopsy @prod --snapshot")
	m := takenID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	snap := "snaptake/prod/snapshots/" + m[1]
	for file, member := range map[string]string{"volume.dbdata.tar.zst": "dbdata/table", "mnt.files.tar.zst": "files/a.jpg"} {
		list, code := srv(t, "docker run --rm -v \"$PWD/"+snap+"/data:/s:ro\" devopsy-util:3 sh -c 'zstd -dc /s/"+file+" | tar -tf -'")
		if code != 0 || !strings.Contains(list, member) {
			t.Errorf("%s (%d): %s", file, code, list)
		}
	}
	for _, svc := range []string{"db", "app"} {
		if state := syncState(t, "snaptake", "prod", svc); state != "running" {
			t.Errorf("%s after the take: %s", svc, state)
		}
	}
	if out, _ := op(t, dir, "devopsy @prod --log"); !strings.Contains(out, "snapshot "+m[1]+" taken") {
		t.Errorf("the take's tar in the log:\n%s", out)
	}

	// Changed, then restored whole: the volume and the entry as they were
	// (what came since gone), the services running on the restored
	// release, db held on the image that ran on its data.
	if out, code := srv(t, "docker run --rm -v snaptake-prod_dbdata:/d -v \"$PWD/snaptake/prod/shared/mnt/files:/f\" busybox:1.37 sh -c 'echo changed >/d/table && echo later >/f/b.jpg'"); code != 0 {
		t.Fatalf("changing the data: %s", out)
	}
	out, code = op(t, dir, "devopsy @prod --snapshots restore "+m[1]+" --yes")
	if code != 0 || !strings.Contains(out, "snapshot "+m[1]+"'s data is in") {
		t.Fatalf("restore (%d):\n%s", code, out)
	}
	table, _ := srv(t, "docker run --rm -v snaptake-prod_dbdata:/d busybox:1.37 cat /d/table")
	files, _ := srv(t, "ls snaptake/prod/shared/mnt/files")
	if strings.TrimSpace(table) != "rows" || strings.TrimSpace(files) != "a.jpg" {
		t.Errorf("after the restore: table %q, files %q", table, files)
	}
	for _, svc := range []string{"db", "app"} {
		if state := syncState(t, "snaptake", "prod", svc); state != "running" {
			t.Errorf("%s after the restore: %s", svc, state)
		}
	}
	if compose, _ := srv(t, "cat snaptake/prod/current/../devopsy-compose.yaml"); !strings.Contains(compose, "busybox@sha256:") {
		t.Errorf("db not held on its data's image:\n%s", compose)
	}
}

// A snapshot exported from one server and imported on another, where
// the environment was just released: restored there with its
// shared/.env, its data in its volume and its entry.
func TestSnapshotMoveServers(t *testing.T) {
	secondServer(t)
	// With a mirror: two passes, the tars made from it after.
	dir := snapshotProject(t, "snapmove", true)
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	if out, code := srv(t, "echo SECRET=s3cret >>snapmove/prod/shared/.env && docker run --rm -v snapmove-prod_dbdata:/d -v \"$PWD/snapmove/prod/shared/mnt/files:/f\" busybox:1.37 sh -c 'echo rows >/d/table && echo pic >/f/a.jpg'"); code != 0 {
		t.Fatalf("writing the data: %s", out)
	}
	out, code := op(t, dir, "devopsy @server:prod --snapshot")
	m := takenID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	if out, code := srv(t, "docker run --rm -v \"$PWD/snapmove/prod/snapshots/.mirror:/m:ro\" busybox:1.37 cat /m/volumes/dbdata/table"); code != 0 || strings.TrimSpace(out) != "rows" {
		t.Errorf("the mirror lacks the database's volume (%d): %s", code, out)
	}
	if out, code := op(t, dir, "devopsy @server2:prod --release"); code != 0 {
		t.Fatalf("prod on server2 (%d):\n%s", code, out)
	}
	out, code = op(t, dir, "devopsy @server:prod --snapshots export "+m[1]+" | devopsy @server2:prod --snapshots import")
	im := regexp.MustCompile(`imported snapshot ` + m[1] + ` as ([0-9]{14})`).FindStringSubmatch(out)
	if code != 0 || im == nil {
		t.Fatalf("export into import (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, "devopsy @server2:prod --snapshots restore "+im[1]+" --with-env --yes"); code != 0 {
		t.Fatalf("restore on server2 (%d):\n%s", code, out)
	}
	table, _ := srv2(t, "docker run --rm -v snapmove-prod_dbdata:/d busybox:1.37 cat /d/table")
	env, _ := srv2(t, "cat snapmove/prod/shared/.env")
	files, _ := srv2(t, "ls snapmove/prod/shared/mnt/files")
	target, _ := srv2(t, "grep '^DEVOPSY_TARGET=' snapmove/prod/current/../devopsy-target.env")
	if strings.TrimSpace(table) != "rows" || !strings.Contains(env, "SECRET=s3cret") || strings.TrimSpace(files) != "a.jpg" || !strings.Contains(target, "server2") {
		t.Errorf("on server2: table %q, env %q, files %q, target %q", table, env, files, target)
	}
	if state := srv2State(t, "snapmove", "prod", "db"); state != "running" {
		t.Errorf("db on server2: %s", state)
	}
}
