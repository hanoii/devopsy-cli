//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// olderDevopsy builds devopsy as it was at tag, for the servers, and
// returns its path.
func olderDevopsy(t *testing.T, tag string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	repo := filepath.Dir(filepath.Dir(file))
	src := t.TempDir()
	archive := exec.Command("sh", "-c", "git archive "+tag+" | tar -x -C "+quote(src))
	archive.Dir = repo
	if out, err := archive.CombinedOutput(); err != nil {
		t.Fatalf("git archive %s: %v\n%s", tag, err, out)
	}
	out := filepath.Join(t.TempDir(), "devopsy")
	build := exec.Command("go", "build", "-o", out, "./cmd/devopsy")
	build.Dir = src
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0", "GOFLAGS=-buildvcs=false")
	if o, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building devopsy %s: %v\n%s", tag, err, o)
	}
	return out
}

// A server whose devopsy speaks an older protocol (v0.43.0's, 4, before
// --say): every command that reaches its environment is refused with the
// upgrade, and nothing on the server changes, files or containers.
func TestOlderServer(t *testing.T) {
	dir := project(t, "older", map[string]string{
		".devopsy/config.yaml":     "project: older\nenvironments:\n  prod:\n    mode: image\n    release: deploy\n    rollback: deploy\n",
		".devopsy/compose.yaml":    "services:\n  app:\n    image: busybox:1.37\n    command: [sleep, infinity]\n",
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	for i := 0; i < 2; i++ {
		op(t, dir, "sleep 1.1")
		if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
			t.Fatalf("release (%d):\n%s", code, out)
		}
	}
	old := olderDevopsy(t, "v0.43.0")
	if out, err := docker("cp", old, server+":/usr/local/bin/devopsy"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = docker("cp", bin, server+":/usr/local/bin/devopsy") })
	if out, _ := srv(t, "devopsy --release-settings"); !strings.Contains(out, "protocol=4") {
		t.Fatalf("not the older devopsy:\n%s", out)
	}

	state := func() string {
		out, _ := srv(t, "find older -printf '%p %y %s %T@\\n' | sort; docker ps -a --filter label=com.docker.compose.project=older-prod --format '{{.ID}} {{.State}}'; docker image ls --format '{{.Repository}}:{{.Tag}}' | sort")
		return out
	}
	before := state()
	for _, command := range []string{
		"--release",
		"--rollback",
		"--releases",
		"--releases remove failed --yes",
		"--status",
		"--vars",
		"--vars set FOO",
		"--log",
		"--destroy --yes",
		"ps",
	} {
		out, code := op(t, dir, "FOO=1 devopsy @server:prod "+command)
		if code == 0 || !strings.Contains(out, "older than this one needs") || !strings.Contains(out, "--upgrade") {
			t.Errorf("%s on an older server (%d):\n%s", command, code, out)
		}
	}
	if after := state(); after != before {
		t.Errorf("the server changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
