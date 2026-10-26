//go:build e2e

package e2e

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// snapshotProject is a database in a named volume and an app's files in
// an entry of mnt, both with data; prod's config is prod (YAML).
func snapshotProject(t *testing.T, name, prod string) string {
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
	dir := snapshotProject(t, "snaptake", "{}")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	// Both written as root, as the services would (Docker made the
	// entry root's: the app has no owner label).
	if out, code := srv(t, "docker run --rm -v snaptake-prod_dbdata:/d -v \"$PWD/snaptake/prod/shared/mnt/files:/f\" busybox:1.37 sh -c 'echo rows >/d/table && echo pic >/f/a.jpg && truncate -s 256M /d/sparse'"); code != 0 {
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
	// A sparse file (a database's punched holes, a preallocated log) comes
	// back as sparse as it was taken: 256 MB of holes take no blocks, and a
	// restore writing them as zeros needs room the take never measured.
	if du, _ := srv(t, "docker run --rm -v snaptake-prod_dbdata:/d busybox:1.37 du -k /d/sparse"); len(strings.Fields(du)) == 0 || strings.Fields(du)[0] != "0" {
		t.Errorf("the sparse file after the restore takes %s kB", du)
	}
	for _, svc := range []string{"db", "app"} {
		if state := syncState(t, "snaptake", "prod", svc); state != "running" {
			t.Errorf("%s after the restore: %s", svc, state)
		}
	}
	if compose, _ := srv(t, "cat snaptake/prod/current/../devopsy-compose.yaml"); !strings.Contains(compose, "busybox@sha256:") {
		t.Errorf("db not held on its data's image:\n%s", compose)
	}

	// A tar whose stream goes on past its end-of-archive blocks (made
	// with larger records, tar -b 2048: 1 MiB, zeros after the end, as an
	// imported or hand-made snapshot may be) passes the restore's check,
	// which reads to the end blocks and drains the rest. The extraction
	// must take it too: GNU tar stops reading at the end blocks, and
	// zstd's write of the rest then dies of SIGPIPE, which pipefail made
	// the extraction's failure, past the restore's point of no return,
	// the services left down and the environment marked.
	if out, code := srv(t, "docker run --rm -v \"$PWD/"+snap+"/data:/s:ro\" devopsy-util:3 sh -c 'cd /tmp && zstd -dc /s/mnt.files.tar.zst | tar -xpf - --numeric-owner && tar -b 2048 --numeric-owner -cf - files | zstd -q' >"+snap+"/data/mnt.files.new && mv "+snap+"/data/mnt.files.new "+snap+"/data/mnt.files.tar.zst"); code != 0 {
		t.Fatalf("remaking the files' tar with 1 MiB records: %s", out)
	}
	// The entry is its owner's: removed as the app would, in a container.
	if out, code := srv(t, "docker run --rm -v \"$PWD/snaptake/prod/shared/mnt/files:/f\" busybox:1.37 rm -f /f/a.jpg"); code != 0 {
		t.Fatalf("removing a.jpg: %s", out)
	}
	out, code = op(t, dir, "devopsy @prod --snapshots restore "+m[1]+" --no-code --yes")
	if files, _ := srv(t, "ls snaptake/prod/shared/mnt/files"); code != 0 || strings.TrimSpace(files) != "a.jpg" {
		t.Errorf("a restore of a tar with 1 MiB records (%d), files %q:\n%s", code, files, out)
	}
}

// A snapshot exported from one server and imported on another, where
// the environment was just released: restored there with its
// shared/.env, its data in its volume and its entry.
func TestSnapshotMoveServers(t *testing.T) {
	secondServer(t)
	// With a mirror: two passes, the tars made from it after.
	dir := snapshotProject(t, "snapmove", "{snapshots: {mirror: true}}")
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

// Release snapshots with real Docker, without a mirror and with one: the
// second release takes one of the first's data before its switch, the
// services running on the new release after, the tars (the mirror's made
// detached, after the activation) any tar reads.
func TestReleaseSnapshotE2E(t *testing.T) {
	for _, c := range []struct{ name, prod string }{
		{"relsnap", "{releases: {keep: 3}, snapshots: {release: true}}"},
		{"relsnapm", "{releases: {keep: 3}, snapshots: {release: true, mirror: true}}"},
	} {
		dir := snapshotProject(t, c.name, c.prod)
		if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
			t.Fatalf("%s: first release (%d):\n%s", c.name, code, out)
		}
		first, _ := srv(t, "readlink "+c.name+"/prod/current")
		if out, code := srv(t, "docker run --rm -v "+c.name+"-prod_dbdata:/d busybox:1.37 sh -c 'echo rows >/d/table'"); code != 0 {
			t.Fatalf("%s: writing the data: %s", c.name, out)
		}
		if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
			t.Fatalf("%s: second release (%d):\n%s", c.name, code, out)
		}
		for _, svc := range []string{"db", "app"} {
			if state := syncState(t, c.name, "prod", svc); state != "running" {
				t.Errorf("%s: %s after the release: %s", c.name, svc, state)
			}
		}
		var list string
		eventually(t, 2*time.Minute, "the release snapshot", func() bool {
			list, _ = srv(t, "grep -l '^kind\trelease' "+c.name+"/prod/snapshots/2*/manifest 2>/dev/null")
			return strings.TrimSpace(list) != ""
		})
		snap := strings.TrimSuffix(strings.TrimSpace(list), "/manifest")
		if manifest, _ := srv(t, "cat "+snap+"/manifest"); !strings.Contains(manifest, "release\t"+strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(first), "releases/"), "/code")) {
			t.Errorf("%s: the snapshot is not the first release's:\n%s", c.name, manifest)
		}
		if list, code := srv(t, "docker run --rm -v \"$PWD/"+snap+"/data:/s:ro\" devopsy-util:3 sh -c 'zstd -dc /s/volume.dbdata.tar.zst | tar -xOf - dbdata/table'"); code != 0 || strings.TrimSpace(list) != "rows" {
			t.Errorf("%s: the database's tar (%d): %s", c.name, code, list)
		}
	}
}

// Snapshots by reflinks with real Docker on an XFS: the environment's
// directory on a loop device (mounted where the project's directory goes,
// before its first release), its data in mnt: a take clones it while
// stopped and makes the tars from the clone, which goes; a restore brings
// it back.
func TestSnapshotReflinkE2E(t *testing.T) {
	dir := project(t, "snapref", map[string]string{
		".devopsy/.env":        "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml": "project: snapref\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n",
		".devopsy/compose.yaml": `services:
  app:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [./mnt/files:/files]
    labels: {devopsy.data: "true"}
`,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	if out, code := execIn(t, []string{server}, "set -e; truncate -s 300M /var/tmp/snapref.xfs; mkfs.xfs -q -f /var/tmp/snapref.xfs; mkdir -p /home/devopsy/snapref; mount -o loop /var/tmp/snapref.xfs /home/devopsy/snapref; chown devopsy:devopsy /home/devopsy/snapref"); code != 0 {
		t.Fatalf("the XFS (%d): %s", code, out)
	}
	t.Cleanup(func() {
		_, _ = execIn(t, []string{server}, "umount -l /home/devopsy/snapref; rm -f /var/tmp/snapref.xfs")
	})
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	if out, code := srv(t, "docker run --rm -v \"$PWD/snapref/prod/shared/mnt/files:/f\" busybox:1.37 sh -c 'echo pic >/f/a.jpg'"); code != 0 {
		t.Fatalf("writing the data: %s", out)
	}
	out, code := op(t, dir, "devopsy @prod --snapshot")
	m := takenID.FindStringSubmatch(out)
	if code != 0 || m == nil || !strings.Contains(out, "the data clones by reflinks") {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	snap := "snapref/prod/snapshots/" + m[1]
	if manifest, _ := srv(t, "cat "+snap+"/manifest"); !strings.Contains(manifest, "copy\treflink") {
		t.Errorf("the manifest:\n%s", manifest)
	}
	if list, code := srv(t, "docker run --rm -v \"$PWD/"+snap+"/data:/s:ro\" devopsy-util:3 sh -c 'zstd -dc /s/mnt.files.tar.zst | tar -tf -'"); code != 0 || !strings.Contains(list, "files/a.jpg") {
		t.Errorf("the tar (%d): %s", code, list)
	}
	if left, _ := srv(t, "ls snapref/prod/snapshots/.clone 2>/dev/null"); strings.TrimSpace(left) != "" {
		t.Errorf("the clone left: %s", left)
	}
	if out, code := srv(t, "docker run --rm -v \"$PWD/snapref/prod/shared/mnt/files:/f\" busybox:1.37 rm /f/a.jpg"); code != 0 {
		t.Fatalf("changing the data: %s", out)
	}
	if out, code := op(t, dir, "devopsy @prod --snapshots restore "+m[1]+" --no-code --yes"); code != 0 {
		t.Fatalf("restore (%d):\n%s", code, out)
	}
	if files, _ := srv(t, "ls snapref/prod/shared/mnt/files"); strings.TrimSpace(files) != "a.jpg" {
		t.Errorf("after the restore: %q", files)
	}
}
