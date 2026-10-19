//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// A child synced from its parent on the same server: its first release
// copies the data; a copy cut after the rsync leaves it down and marked,
// nothing but a sync runs on it, and a sync that completes brings it
// back.
func TestSyncPartial(t *testing.T) {
	dir := syncProject(t, "shop")
	stamp := func(env string) string { return syncStamp(t, "shop", env) }
	state := func(service string) string { return syncState(t, "shop", "dev", service) }
	srv(t, "rm -f finish.log fail-export")
	syncReleaseBoth(t, "shop", dir)

	// The export fails after the rsync went: a partial copy.
	srv(t, "echo two >shop/prod/shared/mnt/data/stamp && touch fail-export")
	out, code := op(t, dir, "devopsy @dev --sync --yes")
	finish, _ := srv(t, "cat finish.log")
	if code == 0 || stamp("dev") != "two" || state("data") != "exited" || !exists(t, "shop/dev/.devopsy-sync.incomplete") || !strings.Contains(finish, "child partial") {
		t.Fatalf("a cut copy (%d), stamp %q, data %q, finish %q:\n%s", code, stamp("dev"), state("data"), finish, out)
	}
	releases, _ := srv(t, "ls shop/dev/releases | wc -l")
	if out, code := op(t, dir, "devopsy @dev --release"); code == 0 || !strings.Contains(out, "did not complete") {
		t.Errorf("a release on it (%d):\n%s", code, out)
	}
	if again, _ := srv(t, "ls shop/dev/releases | wc -l"); again != releases {
		t.Errorf("uploaded anyway: %s releases, were %s", again, releases)
	}
	if out, code := op(t, dir, "devopsy @dev --status"); code != 0 || !strings.Contains(out, "did not complete") {
		t.Errorf("--status (%d):\n%s", code, out)
	}

	// A sync that completes.
	srv(t, "rm -f fail-export")
	if out, code := op(t, dir, "devopsy @dev --sync --yes"); code != 0 || exists(t, "shop/dev/.devopsy-sync.incomplete") || state("data") != "running" {
		t.Fatalf("a complete sync (%d), data %q:\n%s", code, state("data"), out)
	}
	if finish, _ := srv(t, "cat finish.log"); !strings.HasSuffix(strings.TrimSpace(finish), "child go") {
		t.Errorf("finish: %q", finish)
	}
}

// syncProject is a project with prod and dev, its child, on the server:
// an rsync service stopped for syncs (data, its stamp file the copy's
// witness) and a stream one (db), whose import waits as long as
// ~/import-wait-<compose project> says, and hooks that log.
func syncProject(t *testing.T, name string) string {
	return project(t, name, map[string]string{
		".devopsy/.env":        "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml": "project: " + name + "\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n",
		".devopsy/compose.yaml": `services:
  data:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [./mnt/data:/data]
    labels: {devopsy.sync: rsync, devopsy.sync.stop: "true"}
  db:
    image: busybox:1.37
    command: [sleep, infinity]
    labels: {devopsy.sync: stream}
`,
		".devopsy/commands/deploy":          "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
		".devopsy/capabilities/sync/export": "#!/bin/sh\n[ ! -e \"$HOME/fail-export\" ] || exit 3\necho dump\n",
		".devopsy/capabilities/sync/import": "#!/bin/sh\n: >\"$HOME/importing-$COMPOSE_PROJECT_NAME\"\nsleep \"$(cat \"$HOME/import-wait-$COMPOSE_PROJECT_NAME\" 2>/dev/null || echo 0)\"\ncat >/dev/null\n",
		".devopsy/capabilities/sync/finish": "#!/bin/sh\necho \"$1 $2\" >>\"$HOME/finish.log\"\n",
	})
}

// syncReleaseBoth releases prod, its data stamped one, then dev, which
// syncs it.
func syncReleaseBoth(t *testing.T, name, dir string) {
	t.Helper()
	srv(t, "mkdir -p "+name+"/prod/shared/mnt/data && echo one >"+name+"/prod/shared/mnt/data/stamp")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, "devopsy @dev --release"); code != 0 || syncStamp(t, name, "dev") != "one" || syncState(t, name, "dev", "data") != "running" {
		t.Fatalf("dev's first release, syncing (%d):\n%s", code, out)
	}
}

func syncStamp(t *testing.T, name, env string) string {
	out, _ := srv(t, "cat "+name+"/"+env+"/shared/mnt/data/stamp")
	return strings.TrimSpace(out)
}

func syncState(t *testing.T, name, env, service string) string {
	out, _ := srv(t, "docker ps -a --filter label=com.docker.compose.project="+name+"-"+env+" --filter label=com.docker.compose.service="+service+" --format '{{.State}}'")
	return strings.TrimSpace(out)
}
