//go:build e2e

package e2e

import (
	"regexp"
	"strings"
	"testing"
)

// snapshotProject is a database in a named volume and an app's files in
// an entry of mnt, both with data.
func snapshotProject(t *testing.T, name string) string {
	return project(t, name, map[string]string{
		".devopsy/.env":        "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml": "project: " + name + "\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n",
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

// A manual take with real Docker: the data of every service with data
// (a named volume whatever its copy label, an entry of mnt), as
// zstd-compressed tars any tar reads, the services running again after.
func TestSnapshotTake(t *testing.T) {
	dir := snapshotProject(t, "snaptake")
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
}
