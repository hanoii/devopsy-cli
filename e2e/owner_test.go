//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// ownedProject is a project whose data service runs as 10001 from its
// first process, no init and no root: it writes a stamp into its entry
// as it starts, which only its owner can. devopsy.mnt.owner says so. A
// tool in a profile never up owns an entry of its own, and another
// writes the data service's entry without a label (a tool sharing it).
func ownedProject(t *testing.T, name string, files map[string]string) string {
	base := map[string]string{
		".devopsy/.env":        "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml": "project: " + name + "\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n",
		".devopsy/compose.yaml": `services:
  data:
    image: busybox:1.37
    user: "10001:10001"
    command: [sh, -c, "echo up >/data/started && exec sleep infinity"]
    volumes: [./mnt/data:/data]
    labels: {devopsy.data: mnt, devopsy.mnt.owner: "10001:10001"}
  tool:
    image: busybox:1.37
    profiles: [tools]
    user: "999:999"
    volumes: [./mnt/tool:/tool]
    labels: {devopsy.mnt.owner: "999:999"}
  shell:
    image: busybox:1.37
    profiles: [tools]
    volumes: [./mnt/data:/data]
`,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	}
	for p, content := range files {
		base[p] = content
	}
	return project(t, name, base)
}

// owner is the uid:gid of path under the deploy user's home on a server
// (srv or srv2).
func owner(t *testing.T, on func(*testing.T, string) (string, int), path string) string {
	out, _ := on(t, "stat -c %u:%g "+path)
	return strings.TrimSpace(out)
}

// A first release on an empty shared/mnt: the release makes the owned
// entries, an inactive profile's too, and gives them to their owners
// before its run step, so a service running as 10001 from its first
// process writes its entry.
func TestOwnerFirstRelease(t *testing.T) {
	dir := ownedProject(t, "ownfirst", nil)
	out, code := op(t, dir, "devopsy @prod --release")
	started, _ := srv(t, "cat ownfirst/prod/shared/mnt/data/started")
	if code != 0 || strings.TrimSpace(started) != "up" || syncState(t, "ownfirst", "prod", "data") != "running" {
		t.Fatalf("first release (%d), started %q:\n%s", code, started, out)
	}
	for path, want := range map[string]string{"ownfirst/prod/shared/mnt/data": "10001:10001", "ownfirst/prod/shared/mnt/data/started": "10001:10001", "ownfirst/prod/shared/mnt/tool": "999:999"} {
		if got := owner(t, srv, path); got != want {
			t.Errorf("%s is %s's, want %s's", path, got, want)
		}
	}
	// What devopsy did not write, deep under a correct top (an upload by
	// hand), gets its owner at the next release.
	asRoot(t, server, "mkdir -p ownfirst/prod/shared/mnt/data/up/deep && touch ownfirst/prod/shared/mnt/data/up/deep/f && chown -R 1000:1000 ownfirst/prod/shared/mnt/data/up")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 || owner(t, srv, "ownfirst/prod/shared/mnt/data/up/deep/f") != "10001:10001" {
		t.Errorf("a release over files by hand (%d): %s:\n%s", code, owner(t, srv, "ownfirst/prod/shared/mnt/data/up/deep/f"), out)
	}
}

// A sync across servers from a parent whose files are 1000's (written
// by hand there) lands as the child's owner, by the child's labels.
func TestOwnerSyncAcross(t *testing.T) {
	secondServer(t)
	dir := ownedProject(t, "ownsync", nil)
	op(t, dir, "echo DEVOPSY_SERVER_DEV=server2 >>.devopsy/.env")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	asRoot(t, server, "chown -R 1000:1000 ownsync/prod/shared/mnt/data")
	srv(t, "echo one >ownsync/prod/shared/mnt/data/stamp")
	if got := owner(t, srv, "ownsync/prod/shared/mnt/data/stamp"); got != "1000:1000" {
		t.Fatalf("the parent's file is %s's", got)
	}
	out, code := op(t, dir, agent+" devopsy @dev --release")
	stamp, _ := srv2(t, "cat ownsync/dev/shared/mnt/data/stamp")
	if code != 0 || strings.TrimSpace(stamp) != "one" || srv2State(t, "ownsync", "dev", "data") != "running" {
		t.Fatalf("dev's first release, syncing across servers (%d), stamp %q:\n%s", code, stamp, out)
	}
	for _, p := range []string{"ownsync/dev/shared/mnt/data", "ownsync/dev/shared/mnt/data/stamp"} {
		if got := owner(t, srv2, p); got != "10001:10001" {
			t.Errorf("%s is %s's on the child, want 10001's", p, got)
		}
	}
}

// An --import of a tar carrying 1000-owned files lands as the entry's
// owner's.
func TestOwnerImport(t *testing.T) {
	dir := ownedProject(t, "ownimport", nil)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	out, code := op(t, dir, "t=$(mktemp -d) && mkdir -p $t/data/sub && echo imported >$t/data/sub/file && chown -R 1000:1000 $t/data && tar -C $t -cf - --numeric-owner data | devopsy @prod --import data --yes")
	got, _ := srv(t, "cat ownimport/prod/shared/mnt/data/sub/file")
	if code != 0 || strings.TrimSpace(got) != "imported" {
		t.Fatalf("import (%d), file %q:\n%s", code, got, out)
	}
	for _, p := range []string{"ownimport/prod/shared/mnt/data", "ownimport/prod/shared/mnt/data/sub", "ownimport/prod/shared/mnt/data/sub/file"} {
		if got := owner(t, srv, p); got != "10001:10001" {
			t.Errorf("%s is %s's, want 10001's", p, got)
		}
	}
}
