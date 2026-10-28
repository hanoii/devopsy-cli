//go:build e2e

package e2e

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// Syncs by reflinks with real Docker on an XFS: the project's directory
// on a loop device (mounted before its first release), so prod and dev
// share a filesystem. A child's first release clones the parent's data
// while its database is stopped, starts it again and rsyncs the
// database from the clone; the app's files, labeled
// devopsy.data.copy.reflink, are cloned into the child, so they take
// next to no room there.
func TestSyncReflinkE2E(t *testing.T) {
	dir := project(t, "synref", map[string]string{
		".devopsy/.env":        "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml": "project: synref\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n",
		".devopsy/compose.yaml": `services:
  db:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [./mnt/db:/db]
    labels: {devopsy.data: "true", devopsy.data.stop: own}
  app:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [./mnt/files:/files]
    labels: {devopsy.data: "true", devopsy.data.copy.reflink: "true"}
`,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	if out, code := execIn(t, []string{server}, "set -e; truncate -s 400M /var/tmp/synref.xfs; mkfs.xfs -q -f /var/tmp/synref.xfs; mkdir -p /home/devopsy/synref; mount -o loop /var/tmp/synref.xfs /home/devopsy/synref; chown devopsy:devopsy /home/devopsy/synref"); code != 0 {
		t.Fatalf("the XFS (%d): %s", code, out)
	}
	t.Cleanup(func() {
		_, _ = execIn(t, []string{server}, "umount -l /home/devopsy/synref; rm -f /var/tmp/synref.xfs")
	})
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	// As root, as the services would: Docker made their entries root's.
	if out, code := srv(t, "docker run --rm -v \"$PWD/synref/prod/shared/mnt:/m\" busybox:1.37 sh -c 'dd if=/dev/urandom of=/m/files/a.bin bs=1M count=100 2>/dev/null && dd if=/dev/urandom of=/m/db/table bs=1M count=5 2>/dev/null'"); code != 0 {
		t.Fatalf("writing the data: %s", out)
	}
	used := func() int {
		t.Helper()
		out, _ := srv(t, "sync; df --output=used -k synref | tail -n 1")
		n, err := strconv.Atoi(strings.TrimSpace(out))
		if err != nil {
			t.Fatalf("df: %q", out)
		}
		return n
	}
	before := used()

	out, code := op(t, dir, "devopsy @dev --release")
	if code != 0 {
		t.Fatalf("dev's first release (%d):\n%s", code, out)
	}
	// The database stopped for the clone, started before its rsync.
	clone, start, rsync := strings.Index(out, "the data clones by reflinks"), strings.Index(out, "starting db"), strings.Index(out, "rsync mnt/db")
	if clone < 0 || start < clone || rsync < start || !strings.Contains(out, "mnt/files shared with the parent by reflinks") {
		t.Errorf("the order (clone %d, start %d, rsync %d):\n%s", clone, start, rsync, out)
	}
	for _, f := range []string{"files/a.bin", "db/table"} {
		if sums, _ := srv(t, "md5sum synref/prod/shared/mnt/"+f+" synref/dev/shared/mnt/"+f+" | cut -d' ' -f1 | uniq | wc -l"); strings.TrimSpace(sums) != "1" {
			t.Errorf("%s differs on the child", f)
		}
	}
	// 105 MiB copied would take as much; the files shared take none.
	if grew := used() - before; grew > 40*1024 {
		t.Errorf("the child took %d KiB: its files not shared", grew)
	}
	if left, _ := srv(t, "ls -d synref/prod/.devopsy-sync.clone.* 2>/dev/null"); strings.TrimSpace(left) != "" {
		t.Errorf("the clone left: %s", left)
	}
}

// The same on ext4, as most servers' roots, where nothing clones: the
// child's files labeled devopsy.data.copy.reflink are rsynced, said
// once, the child's entry never emptied first (a file both sides have
// keeps its inode: an emptied entry rsynced again was a full copy every
// sync), and what only the child had removed by the rsync.
func TestSyncReflinkExt4E2E(t *testing.T) {
	dir := project(t, "synext", map[string]string{
		".devopsy/.env":        "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml": "project: synext\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n",
		".devopsy/compose.yaml": `services:
  app:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [./mnt/files:/files]
    labels: {devopsy.data: "true", devopsy.data.copy.reflink: "true"}
`,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	if out, code := execIn(t, []string{server}, "set -e; truncate -s 300M /var/tmp/synext.ext4; mkfs.ext4 -q -F /var/tmp/synext.ext4; mkdir -p /home/devopsy/synext; mount -o loop /var/tmp/synext.ext4 /home/devopsy/synext; chown devopsy:devopsy /home/devopsy/synext"); code != 0 {
		t.Fatalf("the ext4 (%d): %s", code, out)
	}
	t.Cleanup(func() {
		_, _ = execIn(t, []string{server}, "umount -l /home/devopsy/synext; rm -f /var/tmp/synext.ext4")
	})
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	files := "\"$PWD/synext/%s/shared/mnt/files:/f\""
	write := func(env, script string) {
		t.Helper()
		if out, code := srv(t, "docker run --rm -v "+fmt.Sprintf(files, env)+" busybox:1.37 sh -c '"+script+"'"); code != 0 {
			t.Fatalf("writing %s's files: %s", env, out)
		}
	}
	write("prod", "echo one >/f/a.txt")
	if out, code := op(t, dir, "devopsy @dev --release"); code != 0 || strings.Count(out, "no reflinks between the parent's data and the child's") != 1 || strings.Contains(out, "shared with the parent by reflinks") {
		t.Fatalf("dev's first release (%d):\n%s", code, out)
	}
	inode := func() string {
		t.Helper()
		out, _ := srv(t, "stat -c %i synext/dev/shared/mnt/files/a.txt")
		return strings.TrimSpace(out)
	}
	before := inode()
	write("prod", "echo two >/f/b.txt")
	write("dev", "echo mine >/f/mine.txt")
	if out, code := op(t, dir, "devopsy @dev --sync --yes"); code != 0 || strings.Count(out, "no reflinks between the parent's data and the child's") != 1 {
		t.Fatalf("the sync (%d):\n%s", code, out)
	}
	if after := inode(); before == "" || after != before {
		t.Errorf("a.txt's inode %s, then %s: the child's entry emptied before the rsync", before, after)
	}
	if got, _ := srv(t, "ls synext/dev/shared/mnt/files | tr '\\n' ' '"); strings.TrimSpace(got) != "a.txt b.txt" {
		t.Errorf("the child's files: %q", got)
	}
}
