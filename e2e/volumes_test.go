//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// A database in a named volume, with real Docker and Compose: a child on
// another server gets its own volume, made by devopsy as compose would
// (compose takes it for its own: no warning), and the parent's data in
// it; an export carries the volume and an import puts it back; a reset
// takes the volume down and fills it again, and takes the volume and the
// network only an older release of the child named.
func TestVolumesAcross(t *testing.T) {
	secondServer(t)
	dir := project(t, "volacross", map[string]string{
		".devopsy/.env":        "DEVOPSY_SERVER=server\nDEVOPSY_SERVER_DEV=server2\n",
		".devopsy/config.yaml": "project: volacross\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n",
		".devopsy/compose.yaml": `services:
  db:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [dbdata:/data]
    labels: {devopsy.data: "true", devopsy.data.version: any, devopsy.data.stop: own}
volumes:
  dbdata: {}
`,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	put := func(on func(*testing.T, string) (string, int), volume, stamp string) {
		t.Helper()
		if out, code := on(t, "docker run --rm -v "+volume+":/d busybox:1.37 sh -c 'echo "+stamp+" >/d/stamp'"); code != 0 {
			t.Fatalf("writing %s: %s", volume, out)
		}
	}
	stamp := func() string {
		out, _ := srv2(t, "docker run --rm -v volacross-dev_dbdata:/d busybox:1.37 cat /d/stamp")
		return strings.TrimSpace(out)
	}
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	put(srv, "volacross-prod_dbdata", "one")

	out, code := op(t, dir, noAgent+" devopsy @dev --release")
	labels, _ := srv2(t, "docker volume inspect -f '{{index .Labels \"com.docker.compose.project\"}} {{index .Labels \"com.docker.compose.volume\"}}' volacross-dev_dbdata")
	if code != 0 || stamp() != "one" || strings.TrimSpace(labels) != "volacross-dev dbdata" || strings.Contains(out, "not created by Docker Compose") || !strings.Contains(out, "rsync volume dbdata") {
		t.Fatalf("dev's first release across servers (%d), stamp %q, labels %q:\n%s", code, stamp(), labels, out)
	}

	put(srv, "volacross-prod_dbdata", "two")
	if out, code := op(t, dir, noAgent+" devopsy @prod --export db | "+noAgent+" devopsy @dev --import db --yes"); code != 0 || stamp() != "two" {
		t.Errorf("an export into an import (%d), stamp %q:\n%s", code, stamp(), out)
	}

	// An older release of the child names a volume and a network the next
	// names no more.
	release := func(extra string) {
		t.Helper()
		if out, code := op(t, dir, `cat >.devopsy/compose.yaml <<'EOF'
services:
  db:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [dbdata:/data`+extra+`]
    labels: {devopsy.data: "true", devopsy.data.version: any, devopsy.data.stop: own}
volumes:
  dbdata: {}
  older: {}
networks:
  older: {}
EOF
`+noAgent+` devopsy @dev --release-no-sync`); code != 0 {
			t.Fatalf("dev's release (%d):\n%s", code, out)
		}
	}
	release(", older:/older]\n    networks: [default, older")
	release("")
	const older = "docker volume ls -q --filter name=volacross-dev_older; docker network ls -q --filter name=volacross-dev_older"
	if out, _ := srv2(t, older); len(strings.Fields(out)) != 2 {
		t.Fatalf("the older release's volume and network, before the reset: %q", out)
	}

	put(srv, "volacross-prod_dbdata", "three")
	out, code = op(t, dir, noAgent+" devopsy @dev --reset --yes")
	if left, _ := srv2(t, older); strings.TrimSpace(left) != "" {
		t.Errorf("the older release's volume or network left by a reset: %q", left)
	}
	labels, _ = srv2(t, "docker volume inspect -f '{{index .Labels \"com.docker.compose.project\"}}' volacross-dev_dbdata")
	if code != 0 || stamp() != "three" || strings.TrimSpace(labels) != "volacross-dev" || strings.Contains(out, "not created by Docker Compose") {
		t.Errorf("a reset (%d), stamp %q, labels %q:\n%s", code, stamp(), labels, out)
	}
}
