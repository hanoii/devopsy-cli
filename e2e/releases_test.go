//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// --releases while a release waits in its prepare step says IN
// PROGRESS; its activation killed outright (kill -9: no trap, no
// watcher's signal) leaves it INCOMPLETE, never current, removed by id
// (remove failed only takes those older than current).
func TestReleasesStates(t *testing.T) {
	dir := dropProject(t, "states")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	first, _ := srv(t, "readlink states/prod/current")
	srv(t, "rm -f preparing; touch wait-prepare")
	t.Cleanup(func() { srv(t, "rm -f wait-prepare preparing") })
	op(t, dir, "sleep 1.1")
	background(t, dir, "devopsy @server:prod --release", "/tmp/states.log")
	eventually(t, time.Minute, "prepare waiting", func() bool { return exists(t, "preparing") })
	if out, _ := op(t, dir, "devopsy @server:prod --releases"); !strings.Contains(out, "IN PROGRESS") {
		t.Errorf("--releases during the prepare step:\n%s", out)
	}
	if out, code := srv(t, "pkill -9 -f '[f]etch_release'"); code != 0 {
		t.Fatalf("killing the activation (%d): %s", code, out)
	}
	srv(t, "rm -f wait-prepare")
	eventually(t, 30*time.Second, "the lock freed", func() bool { return lockFree(t, "states/prod/.lock") })
	out, _ := op(t, dir, "devopsy @server:prod --releases")
	if !strings.Contains(out, "INCOMPLETE") {
		t.Errorf("--releases after the kill:\n%s", out)
	}
	if now, _ := srv(t, "readlink states/prod/current"); now != first {
		t.Errorf("current changed: %s", now)
	}
	// Newer than current, failed leaves it: it may be between its
	// upload and its activation. By id.
	id, _ := srv(t, "ls states/prod/releases | tail -n 1")
	if out, code := op(t, dir, "devopsy @server:prod --releases remove failed --yes"); code != 0 || !exists(t, "states/prod/releases/"+strings.TrimSpace(id)) {
		t.Errorf("remove failed took the incomplete release newer than current (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, "devopsy @server:prod --releases remove "+strings.TrimSpace(id)+" --yes"); code != 0 {
		t.Errorf("remove %s (%d):\n%s", id, code, out)
	}
	if n, _ := srv(t, "ls states/prod/releases | wc -l"); strings.TrimSpace(n) != "1" {
		t.Errorf("releases left: %s", n)
	}
}

// --destroy takes the containers, the data written as root by a
// container, the images and the directory, the levels' leftovers with
// --purge.
func TestDestroy(t *testing.T) {
	dir := project(t, "gone", map[string]string{
		".devopsy/config.yaml":     "project: gone\nenvironments:\n  prod:\n    mode: image\n    release: deploy\n    rollback: deploy\n",
		".devopsy/compose.yaml":    "services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'echo root >/data/stamp && exec sleep infinity']\n    volumes: [./mnt/data:/data]\n",
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	eventually(t, 30*time.Second, "the container's write", func() bool { return exists(t, "gone/prod/shared/mnt/data/stamp") })
	if out, code := op(t, dir, "devopsy @server:prod --destroy --yes --purge"); code != 0 {
		t.Fatalf("--destroy (%d):\n%s", code, out)
	}
	if exists(t, "gone") {
		out, _ := srv(t, "ls -laR gone")
		t.Errorf("left:\n%s", out)
	}
	if out, _ := srv(t, "docker ps -aq --filter label=com.docker.compose.project=gone-prod; docker image ls -q --filter reference='*:devopsy-gone-prod-*'"); strings.TrimSpace(out) != "" {
		t.Errorf("containers or bookmarks left: %s", out)
	}
}

// A release from a terminal: the server's lines colored, and a build's
// live progress drawn through the pipe (with_tty), though activations
// are never -t.
func TestTerminalRelease(t *testing.T) {
	dir := project(t, "tty", map[string]string{
		".devopsy/config.yaml":     "project: tty\nenvironments:\n  prod:\n    release: deploy\n    rollback: deploy\n",
		".devopsy/compose.yaml":    "services:\n  app:\n    build: .\n    command: [sleep, infinity]\n",
		".devopsy/Dockerfile":      "FROM busybox:1.37\nRUN echo built\n",
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	commitAll(t, dir)
	inTerminal(t, dir, "devopsy @server:prod --release", "/tmp/tty")
	code := terminalCode(t, "/tmp/tty")
	out, _ := op(t, "/", "cat /tmp/tty")
	if code != "exit=0" {
		t.Fatalf("release: %s\n%s", code, out)
	}
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("no colors:\n%s", out)
	}
	// BuildKit's, as devopsy-server installs it, redrawn (cursor up).
	if strings.Contains(out, "classic builder") || !strings.Contains(out, "\x1b[1A") {
		t.Errorf("no live build progress:\n%s", out)
	}
	if log, _ := op(t, dir, "devopsy @server:prod --log"); strings.Contains(log, "\x1b[") {
		t.Errorf("terminal codes in --log:\n%s", log)
	}
}
