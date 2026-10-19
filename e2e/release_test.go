//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// The smallest project released to a server: it runs there, --status
// shows it, a second release and a rollback switch between the two.
func TestRelease(t *testing.T) {
	dir := project(t, "hello", map[string]string{
		".devopsy/config.yaml":     "project: hello\nenvironments:\n  prod:\n    mode: image\n    release: deploy\n    rollback: deploy\n",
		".devopsy/compose.yaml":    "services:\n  app:\n    image: busybox:1.37\n    command: [sleep, infinity]\n",
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	first, _ := srv(t, "readlink hello/prod/current")
	if out, _ := srv(t, "docker ps --filter label=com.docker.compose.project=hello-prod --format '{{.State}}'"); strings.TrimSpace(out) != "running" {
		t.Fatalf("not running: %q", out)
	}
	if out, code := op(t, dir, "devopsy @server:prod --status"); code != 0 || !strings.Contains(out, "app") {
		t.Errorf("--status (%d):\n%s", code, out)
	}

	// Release ids are seconds.
	op(t, dir, "sleep 1.1")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("second release (%d):\n%s", code, out)
	}
	if second, _ := srv(t, "readlink hello/prod/current"); second == first {
		t.Fatalf("current did not switch: %s", second)
	}
	if out, code := op(t, dir, "devopsy @server:prod --rollback"); code != 0 {
		t.Fatalf("rollback (%d):\n%s", code, out)
	}
	if back, _ := srv(t, "readlink hello/prod/current"); back != first {
		t.Errorf("rolled back to %s, not %s", back, first)
	}
}
