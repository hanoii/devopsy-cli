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

// A variable only an included file requires is listed and refused before the switch, as the
// server's compose reads the files, nothing current after it; one the include's own
// env_file sets is not missing to compose (the list may name it beside a missing one, as
// --variables cannot tell), so once the other is set the release goes.
func TestReleaseIncludedVariables(t *testing.T) {
	dir := project(t, "inclvars", map[string]string{
		".devopsy/.env":            "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml":     "project: inclvars\ndefaults:\n  mode: image\n  release: {run: deploy}\nenvironments:\n  prod: {}\n",
		".devopsy/compose.yaml":    "include:\n  - path: sub/inc.yaml\n    env_file: sub/inc.env\nservices:\n  app:\n    image: busybox:1.37\n    command: [sleep, infinity]\n",
		".devopsy/sub/inc.yaml":    "services:\n  side:\n    image: busybox:1.37\n    command: [sleep, infinity]\n    environment: [\"NAME=${INC_NAME:?the site's name}\", \"SET=${INC_SET:?}\"]\n",
		".devopsy/sub/inc.env":     "INC_SET=1\n",
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts\nexec devopsy up -d --wait\n",
	})
	out, code := op(t, dir, "devopsy @prod --release")
	if cur, _ := srv(t, "readlink inclvars/prod/current"); code == 0 || cur != "" || !strings.Contains(out, "compose requires variables that are not set") || !strings.Contains(out, "INC_NAME") || !strings.Contains(out, "required variable INC_NAME is missing a value: the site's name") || !strings.Contains(out, "not made current") {
		t.Fatalf("a release missing an included file's variable (%d), current %q:\n%s", code, cur, out)
	}
	if out, code := op(t, dir, "INC_NAME=shop devopsy @prod --vars set INC_NAME && devopsy @prod --release"); code != 0 {
		t.Fatalf("the release once it is set (%d):\n%s", code, out)
	}
}
