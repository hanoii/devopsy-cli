//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// registry runs a registry on the server's Docker, once, which both
// servers reach as server:5000 (insecure: daemon.json), so a test can
// move a tag as a registry does.
func registry(t *testing.T) {
	t.Helper()
	if _, code := srv(t, "docker inspect e2e-registry >/dev/null 2>&1 || docker run -d --name e2e-registry --restart always -p 5000:5000 registry:3"); code != 0 {
		t.Fatal("could not start the registry")
	}
	eventually(t, time.Minute, "the registry", func() bool {
		_, code := srv(t, "curl -fs http://server:5000/v2/ >/dev/null")
		return code == 0
	})
}

// pushAs points the registry's server:5000/<tag> at src's image, pulled
// from Docker Hub: the tag moves, as upstream's do.
func pushAs(t *testing.T, src, tag string) {
	t.Helper()
	if out, code := srv(t, "docker pull -q "+src+" >/dev/null && docker tag "+src+" server:5000/"+tag+" && docker push -q server:5000/"+tag+" >/dev/null && docker rmi -f server:5000/"+tag+" >/dev/null"); code != 0 {
		t.Fatalf("pushing %s as %s: %s", src, tag, out)
	}
}

// container is a compose service's container on the server (or
// server2): its id and its image's id.
func container(t *testing.T, on func(*testing.T, string) (string, int), project, service string) (id, image string) {
	t.Helper()
	out, _ := on(t, "docker ps -a --filter label=com.docker.compose.project="+project+" --filter label=com.docker.compose.service="+service+" --format '{{.ID}}' | head -1 | xargs -r docker inspect -f '{{.Id}} {{.Image}}'")
	id, image, _ = strings.Cut(strings.TrimSpace(out), " ")
	return id, image
}

// S24: a rollback across a tag that moved keeps the database on the
// newer image, its container untouched, while the app, whose name
// changed between the releases, goes back exactly; --status names the
// hold.
func TestRollbackKeepsData(t *testing.T) {
	registry(t)
	pushAs(t, "busybox:1.36", "imgrb-db:x")
	pushAs(t, "busybox:1.36", "imgrb-app:1")
	pushAs(t, "busybox:1.37", "imgrb-app:2")
	files := func(tag string) map[string]string {
		return map[string]string{
			".devopsy/config.yaml": "project: imgrb\nenvironments:\n  prod:\n    mode: image\n    release: deploy\n    rollback: deploy\n    env: {APP_TAG: \"" + tag + "\"}\n",
			".devopsy/compose.yaml": `services:
  app:
    image: server:5000/imgrb-app:${APP_TAG}
    command: [sleep, infinity]
  database:
    image: server:5000/imgrb-db:x
    command: [sleep, infinity]
    volumes: [./mnt/database:/data]
    labels: {devopsy.data: mnt, devopsy.data.stop: own}
`,
			".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
		}
	}
	dir := project(t, "imgrb", files("1"))
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("r1 (%d):\n%s", code, out)
	}
	_, app1 := container(t, srv, "imgrb-prod", "app")
	_, db1 := container(t, srv, "imgrb-prod", "database")
	// The tag moves; the next release names another app.
	pushAs(t, "busybox:1.37", "imgrb-db:x")
	op(t, dir, "sed -i 's/APP_TAG: \"1\"/APP_TAG: \"2\"/' .devopsy/config.yaml")
	out, code := op(t, dir, "devopsy @server:prod --release")
	dbID, db2 := container(t, srv, "imgrb-prod", "database")
	if code != 0 || db2 == db1 || !strings.Contains(out, "images that changed:") {
		t.Fatalf("r2 (%d): database %s, was %s:\n%s", code, db2, db1, out)
	}
	if hold, _ := srv(t, "cat imgrb/prod/current/../devopsy-compose.yaml"); !strings.Contains(hold, `image: "server:5000/imgrb-db@sha256:`) {
		t.Errorf("devopsy's compose file:\n%s", hold)
	}
	out, code = op(t, dir, "devopsy @server:prod --rollback")
	idAfter, dbAfter := container(t, srv, "imgrb-prod", "database")
	_, appAfter := container(t, srv, "imgrb-prod", "app")
	if code != 0 || idAfter != dbID || dbAfter != db2 || appAfter != app1 {
		t.Errorf("rollback (%d): database container %s (was %s) on %s (want %s), app on %s (want %s):\n%s", code, idAfter, dbID, dbAfter, db2, appAfter, app1, out)
	}
	if !strings.Contains(out, "database stays on server:5000/imgrb-db@sha256:") {
		t.Errorf("the rollback does not say the database stays:\n%s", out)
	}
	if status, _ := op(t, dir, "devopsy @server:prod --status"); !strings.Contains(status, "database held on server:5000/imgrb-db@sha256:") {
		t.Errorf("--status:\n%s", status)
	}
	// The history records both images, oldest first.
	if h, _ := srv(t, "cut -f2,3,5 imgrb/prod/.devopsy-data-images"); strings.Count(h, "database\tran\t") != 2 {
		t.Errorf("history:\n%s", h)
	}
}

// S68: a child on another server, on the parent's tag's older image,
// gets the parent's image pulled by its digest before anything stops,
// and after the copy runs exactly it, its container made again.
func TestSyncAcrossPullsParentImage(t *testing.T) {
	secondServer(t)
	registry(t)
	pushAs(t, "busybox:1.36", "imgsy-db:x")
	dir := project(t, "imgsy", map[string]string{
		".devopsy/.env":        "DEVOPSY_SERVER=server\nDEVOPSY_SERVER_DEV=server2\n",
		".devopsy/config.yaml": "project: imgsy\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n",
		".devopsy/compose.yaml": `services:
  database:
    image: server:5000/imgsy-db:x
    command: [sleep, infinity]
    volumes: [./mnt/database:/data]
    labels: {devopsy.data: mnt, devopsy.data.stop: own}
`,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	srv(t, "mkdir -p imgsy/prod/shared/mnt/database && echo one >imgsy/prod/shared/mnt/database/stamp")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, agent+" devopsy @dev --release"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	_, dev1 := container(t, srv2, "imgsy-dev", "database")
	pushAs(t, "busybox:1.37", "imgsy-db:x")
	srv(t, "echo two >imgsy/prod/shared/mnt/database/stamp")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod again (%d):\n%s", code, out)
	}
	_, prod2 := container(t, srv, "imgsy-prod", "database")
	if prod2 == dev1 {
		t.Fatalf("the tag did not move: %s", prod2)
	}
	if _, code := srv2(t, "docker image inspect "+prod2+" >/dev/null 2>&1"); code == 0 {
		t.Fatalf("server2 has the parent's new image already")
	}
	out, code := op(t, dir, agent+" devopsy @dev --sync --yes")
	devID, dev2 := container(t, srv2, "imgsy-dev", "database")
	stamp, _ := srv2(t, "cat imgsy/dev/shared/mnt/database/stamp")
	if code != 0 || dev2 != prod2 || strings.TrimSpace(stamp) != "two" || !strings.Contains(out, "pulled the parent's image") {
		t.Fatalf("the sync (%d): dev runs %s, prod %s, stamp %q:\n%s", code, dev2, prod2, stamp, out)
	}
	// Bookmarked as the child's: a pruning keeps it while dev runs it.
	if tags, _ := srv2(t, "docker image inspect -f '{{range .RepoTags}}{{println .}}{{end}}' "+dev2); !strings.Contains(tags, ":devopsy-imgsy-dev-") {
		t.Errorf("not bookmarked: %s", tags)
	}
	// Its next plain up keeps it: held.
	srv2(t, "cd imgsy/dev/current && devopsy up -d --wait >/dev/null 2>&1")
	if id, img := container(t, srv2, "imgsy-dev", "database"); id != devID || img != prod2 {
		t.Errorf("a plain up moved it: %s on %s", id, img)
	}
}
