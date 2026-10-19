//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// guardProject is a one-service project whose .devopsy carries blob, so
// an upload lasts on a throttled link.
func guardProject(t *testing.T, name string) string {
	dir := project(t, name, map[string]string{
		".devopsy/config.yaml": "project: " + name + "\nenvironments:\n  prod:\n    mode: image\n    release: deploy\n    rollback: deploy\n",
		".devopsy/compose.yaml": `services:
  data:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [./mnt/data:/data]
    labels: {devopsy.sync: rsync}
`,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	return dir
}

// An upload cut silently: the server's guard ends it (24 beats without
// data), the release lock freed, nothing released.
func TestUploadDropSilently(t *testing.T) {
	dir := guardProject(t, "upguard")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	first, _ := srv(t, "readlink upguard/prod/current")
	op(t, dir, "head -c 20000000 /dev/urandom >.devopsy/blob && sleep 1.1")
	throttle(t, "2mbit")
	background(t, dir, fast()+" devopsy @server:prod --release", "/tmp/upguard.log")
	eventually(t, time.Minute, "the upload under way", func() bool {
		out, _ := srv(t, "ls -d upguard/prod/releases/*.tmp 2>/dev/null")
		return out != ""
	})
	time.Sleep(2 * time.Second)
	dropSilently(t)
	took := eventually(t, quiet(24, 30*time.Second), "the upload's lock freed", func() bool { return lockFree(t, "upguard/prod/.lock") })
	t.Logf("freed %s after the drop", took)
	if now, _ := srv(t, "readlink upguard/prod/current"); now != first {
		t.Errorf("current changed: %s", now)
	}
	undrop(t)
	op(t, "/", "pkill devopsy || true")
}

// An --import cut silently: the server's guard ends it, the release lock
// freed, the data as it was.
func TestImportDropSilently(t *testing.T) {
	dir := guardProject(t, "impguard")
	// Before the release: Docker would make the directory root's.
	srv(t, "mkdir -p impguard/prod/shared/mnt/data && echo kept >impguard/prod/shared/mnt/data/stamp")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	op(t, "/", "rm -rf /tmp/impdata && mkdir -p /tmp/impdata/data && head -c 20000000 /dev/urandom >/tmp/impdata/data/big && tar -cf /tmp/imp.tar -C /tmp/impdata data")
	throttle(t, "2mbit")
	background(t, dir, fast()+" devopsy @server:prod --import data --yes </tmp/imp.tar", "/tmp/impguard.log")
	eventually(t, time.Minute, "the import reading", func() bool {
		_, code := srv(t, "pgrep -f '[-]-import data'")
		return code == 0
	})
	time.Sleep(2 * time.Second)
	dropSilently(t)
	took := eventually(t, quiet(24, 30*time.Second), "the import's lock freed", func() bool { return lockFree(t, "impguard/prod/.lock") })
	t.Logf("freed %s after the drop", took)
	if out, _ := srv(t, "cat impguard/prod/shared/mnt/data/stamp"); out != "kept\n" {
		t.Errorf("the data changed: %q", out)
	}
	undrop(t)
	op(t, "/", "pkill devopsy || true")
}
