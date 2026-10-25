//go:build e2e

package e2e

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// An export cut by a silent drop: the server notices the silence, stops
// the copy and restores what it stopped, its locks freed (it waited on a
// docker run client stuck on the dead session's pipe until TCP gave up).
func TestExportDropSilently(t *testing.T) {
	dir := project(t, "exp", map[string]string{
		".devopsy/config.yaml": "project: exp\nenvironments:\n  prod:\n    mode: image\n    release: deploy\n    rollback: deploy\n",
		".devopsy/compose.yaml": `services:
  data:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [./mnt/data:/data]
    labels: {devopsy.data: "true", devopsy.data.stop: own}
`,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	srv(t, "mkdir -p exp/prod/shared/mnt/data && head -c 300000000 /dev/urandom >exp/prod/shared/mnt/data/big")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	state := func() string {
		out, _ := srv(t, "docker ps -a --filter label=com.docker.compose.project=exp-prod --format '{{.State}}'")
		return strings.TrimSpace(out)
	}
	background(t, dir, fast()+" devopsy @server:prod --export data", "/tmp/exp.tar")
	eventually(t, time.Minute, "the export streaming", func() bool {
		out, _ := op(t, "/", "stat -c %s /tmp/exp.tar 2>/dev/null || echo 0")
		n, _ := strconv.Atoi(strings.TrimSpace(out))
		return n > 1000000
	})
	dropSilently(t)
	took := eventually(t, quiet(6, 40*time.Second), "the export cancelled, data back", func() bool {
		_, code := srv(t, "flock -n exp/prod/.devopsy-sync.lock true")
		return code == 0 && state() == "running"
	})
	t.Logf("restored %s after the drop", took)
	if out, _ := srv(t, "docker ps -a --filter name=devopsy-export --format '{{.Names}} {{.State}}'"); strings.TrimSpace(out) != "" {
		t.Errorf("the export's container is left: %s", out)
	}
	undrop(t)
	op(t, "/", "pkill devopsy || true")
}
