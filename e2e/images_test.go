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

// imgProject is a project in image mode whose database is
// server:5000/<name>-db:<DB_TAG>, labeled devopsy.data.version=<DB_VERSION>,
// and whose app's tag is APP_TAG, each set per release in config.yaml; its
// deploy fails after its up when FAIL_RUN is set.
func imgProject(t *testing.T, name string) string {
	t.Helper()
	return project(t, name, map[string]string{
		".devopsy/config.yaml": "project: " + name + "\nenvironments:\n  prod:\n    mode: image\n    release: deploy\n    rollback: deploy\n    env: {APP_TAG: \"1\", DB_TAG: x, DB_VERSION: \"1\", FAIL_RUN: \"\"}\n",
		".devopsy/compose.yaml": `services:
  app:
    image: server:5000/` + name + `-app:${APP_TAG}
    command: [sleep, infinity]
  database:
    image: server:5000/` + name + `-db:${DB_TAG}
    command: [sleep, infinity]
    volumes: [./mnt/database:/data]
    labels: {devopsy.data: "true", devopsy.data.version: "${DB_VERSION}", devopsy.data.stop: own}
`,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\ndevopsy up -d --wait || exit\n[ -z \"$FAIL_RUN\" ] || { echo 'deploy fails on purpose'; exit 1; }\n",
	})
}

// setVar changes one of imgProject's variables in its config.
func setVar(t *testing.T, dir, key, value string) {
	t.Helper()
	if out, code := op(t, dir, "sed -i 's/"+key+": [^,}]*/"+key+": \""+value+"\"/' .devopsy/config.yaml"); code != 0 {
		t.Fatalf("setting %s: %s", key, out)
	}
}

// A rollback within the data's version and tag goes, the database on
// what its floating tag holds now (its patch), while the app, whose name
// changed between the releases, goes back exactly; nothing is held, and
// the history records each run's version and tag.
func TestRollbackKeepsData(t *testing.T) {
	registry(t)
	pushAs(t, "busybox:1.36", "imgrb-db:x")
	pushAs(t, "busybox:1.36", "imgrb-app:1")
	pushAs(t, "busybox:1.37", "imgrb-app:2")
	dir := imgProject(t, "imgrb")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("r1 (%d):\n%s", code, out)
	}
	_, app1 := container(t, srv, "imgrb-prod", "app")
	_, db1 := container(t, srv, "imgrb-prod", "database")
	// The tag moves; the next release names another app.
	pushAs(t, "busybox:1.37", "imgrb-db:x")
	setVar(t, dir, "APP_TAG", "2")
	out, code := op(t, dir, "devopsy @server:prod --release")
	_, db2 := container(t, srv, "imgrb-prod", "database")
	if code != 0 || db2 == db1 || !strings.Contains(out, "images that changed:") {
		t.Fatalf("r2 (%d): database %s, was %s:\n%s", code, db2, db1, out)
	}
	out, code = op(t, dir, "devopsy @server:prod --rollback")
	_, dbAfter := container(t, srv, "imgrb-prod", "database")
	_, appAfter := container(t, srv, "imgrb-prod", "app")
	if code != 0 || dbAfter != db2 || appAfter != app1 {
		t.Errorf("rollback (%d): database on %s (want %s, what its tag holds now), app on %s (want %s):\n%s", code, dbAfter, db2, appAfter, app1, out)
	}
	// Each pulled service is pinned there by its digest; a hold carries devopsy.held.
	if hold, _ := srv(t, "cat imgrb/prod/current/../devopsy-compose.yaml"); strings.Contains(hold, "devopsy.held") {
		t.Errorf("devopsy's compose file holds something:\n%s", hold)
	}
	if h, _ := srv(t, "cut -f2,3,7,8 imgrb/prod/.devopsy-data-images"); !strings.Contains(h, "database\tran\t1\tx") {
		t.Errorf("history:\n%s", h)
	}
}

// A release that raises the data's version goes; the rollback to the
// release before, below it, is refused before anything changes (the
// database's container untouched, current the same), naming its ways
// out, until --rollback --accept-image database lets it through, the
// data's history starting over from it.
func TestDataVersionRollbackRefused(t *testing.T) {
	registry(t)
	pushAs(t, "busybox:1.36", "imgvr-db:x")
	pushAs(t, "busybox:1.37", "imgvr-db:y")
	pushAs(t, "busybox:1.36", "imgvr-app:1")
	dir := imgProject(t, "imgvr")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("r1 (%d):\n%s", code, out)
	}
	setVar(t, dir, "DB_TAG", "y")
	setVar(t, dir, "DB_VERSION", "2")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("r2 at version 2 (%d):\n%s", code, out)
	}
	cur, _ := srv(t, "readlink imgvr/prod/current")
	dbID, db2 := container(t, srv, "imgvr-prod", "database")
	out, code := op(t, dir, "devopsy @server:prod --rollback")
	after, _ := srv(t, "readlink imgvr/prod/current")
	id, img := container(t, srv, "imgvr-prod", "database")
	if code == 0 || after != cur || id != dbID || img != db2 || !strings.Contains(out, "devopsy.data.version=1 is lower than its data's version, 2") || !strings.Contains(out, "--rollback --accept-image database") || !strings.Contains(out, "--release") {
		t.Fatalf("a rollback below the data's version (%d): current %s (was %s), database %s on %s:\n%s", code, after, cur, id, img, out)
	}
	if out, code := op(t, dir, "devopsy @server:prod --rollback --accept-image database"); code != 0 {
		t.Fatalf("the rollback accepted (%d):\n%s", code, out)
	}
	if h, _ := srv(t, "cut -f3,7 imgvr/prod/.devopsy-data-images | tail -1"); strings.TrimSpace(h) != "forced\t1" {
		t.Errorf("the history after the accepted rollback ends on %q", h)
	}
}

// A tag changed with the label left as it was is refused before the
// fetch: the new tag is never pulled, nothing changes.
func TestDataTagChangedRefused(t *testing.T) {
	registry(t)
	pushAs(t, "busybox:1.36", "imgtc-db:x")
	pushAs(t, "busybox:1.37", "imgtc-db:y")
	pushAs(t, "busybox:1.36", "imgtc-app:1")
	dir := imgProject(t, "imgtc")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("r1 (%d):\n%s", code, out)
	}
	setVar(t, dir, "DB_TAG", "y")
	out, code := op(t, dir, "devopsy @server:prod --release")
	if code == 0 || !strings.Contains(out, "its tag is y, and its data ran x at the same devopsy.data.version=1") {
		t.Fatalf("a tag changed without the label (%d):\n%s", code, out)
	}
	if _, code := srv(t, "docker image inspect server:5000/imgtc-db:y >/dev/null 2>&1"); code == 0 {
		t.Errorf("the refused release pulled the new tag")
	}
	setVar(t, dir, "DB_VERSION", "1.1")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("the tag with its label raised (%d):\n%s", code, out)
	}
}

// Data labeled any: devopsy checks nothing, so a rollback across tags
// goes freely.
func TestDataAnyRollsBack(t *testing.T) {
	registry(t)
	pushAs(t, "busybox:1.36", "imgany-db:x")
	pushAs(t, "busybox:1.37", "imgany-db:y")
	pushAs(t, "busybox:1.36", "imgany-app:1")
	dir := imgProject(t, "imgany")
	setVar(t, dir, "DB_VERSION", "any")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("r1 (%d):\n%s", code, out)
	}
	_, db1 := container(t, srv, "imgany-prod", "database")
	setVar(t, dir, "DB_TAG", "y")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("r2 (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, "devopsy @server:prod --rollback"); code != 0 {
		t.Fatalf("the rollback (%d):\n%s", code, out)
	}
	if _, db := container(t, srv, "imgany-prod", "database"); db != db1 {
		t.Errorf("the rollback runs the database on %s, want x's %s", db, db1)
	}
}

// A release raising the data's version whose run step fails after its up
// ran the database: the restart of the release before cannot refuse, so
// it keeps the database on the failed release's image, by digest, says
// so, and --status names the hold.
func TestDataRestartHolds(t *testing.T) {
	registry(t)
	pushAs(t, "busybox:1.36", "imgrh-db:x")
	pushAs(t, "busybox:1.37", "imgrh-db:y")
	pushAs(t, "busybox:1.36", "imgrh-app:1")
	dir := imgProject(t, "imgrh")
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("r1 (%d):\n%s", code, out)
	}
	setVar(t, dir, "DB_TAG", "y")
	setVar(t, dir, "DB_VERSION", "2")
	setVar(t, dir, "FAIL_RUN", "1")
	out, code := op(t, dir, "devopsy @server:prod --release")
	_, db := container(t, srv, "imgrh-prod", "database")
	y, _ := srv(t, "docker image inspect -f '{{.Id}}' server:5000/imgrh-db:y")
	if code == 0 || db != strings.TrimSpace(y) || !strings.Contains(out, "database stays on the image release ") {
		t.Fatalf("the failed release (%d): the database runs %s, want y's %s:\n%s", code, db, y, out)
	}
	if c, _ := srv(t, "cat imgrh/prod/current/../devopsy-compose.yaml"); !strings.Contains(c, `image: "server:5000/imgrh-db@sha256:`) || !strings.Contains(c, `"devopsy.held"`) {
		t.Errorf("devopsy's compose file:\n%s", c)
	}
	if status, _ := op(t, dir, "devopsy @server:prod --status"); !strings.Contains(status, "database held on server:5000/imgrh-db@sha256:") {
		t.Errorf("--status:\n%s", status)
	}
	if out, code := op(t, dir, "devopsy @server:prod --reload"); code != 0 {
		t.Errorf("a reload of the held release (%d):\n%s", code, out)
	}
}

// S68: a child on another server, at the parent's version, syncs: it
// runs its own image on the copy, never the parent's, its history the
// parent's from a sync line; a child below the parent's version is
// refused before anything stops.
func TestSyncAcrossRunsChildImage(t *testing.T) {
	secondServer(t)
	registry(t)
	pushAs(t, "busybox:1.36", "imgsy-db:x")
	dir := project(t, "imgsy", map[string]string{
		".devopsy/.env":        "DEVOPSY_SERVER=server\nDEVOPSY_SERVER_DEV=server2\n",
		".devopsy/config.yaml": "project: imgsy\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {env: {DB_VERSION: \"1\"}}\n  dev: {parent: prod, env: {DB_VERSION: \"1\"}}\n",
		".devopsy/compose.yaml": `services:
  database:
    image: server:5000/imgsy-db:x
    command: [sleep, infinity]
    volumes: [./mnt/database:/data]
    labels: {devopsy.data: "true", devopsy.data.version: "${DB_VERSION}", devopsy.data.stop: own}
`,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	srv(t, "mkdir -p imgsy/prod/shared/mnt/database && echo one >imgsy/prod/shared/mnt/database/stamp")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, noAgent+" devopsy @dev --release"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	_, dev1 := container(t, srv2, "imgsy-dev", "database")
	pushAs(t, "busybox:1.37", "imgsy-db:x")
	srv(t, "echo two >imgsy/prod/shared/mnt/database/stamp")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod again (%d):\n%s", code, out)
	}
	_, prod2 := container(t, srv, "imgsy-prod", "database")
	out, code := op(t, dir, noAgent+" devopsy @dev --sync --yes")
	_, dev2 := container(t, srv2, "imgsy-dev", "database")
	stamp, _ := srv2(t, "cat imgsy/dev/shared/mnt/database/stamp")
	if code != 0 || dev2 != dev1 || dev2 == prod2 || strings.TrimSpace(stamp) != "two" {
		t.Fatalf("the sync (%d): dev runs %s (its own %s), prod %s, stamp %q:\n%s", code, dev2, dev1, prod2, stamp, out)
	}
	if h, _ := srv2(t, "cut -f3 imgsy/dev/.devopsy-data-images"); !strings.Contains(h, "sync ") {
		t.Errorf("dev's history:\n%s", h)
	}
	// prod raises its data's version: dev, still at 1, is refused.
	op(t, dir, "sed -i 's/prod: {env: {DB_VERSION: \"1\"}}/prod: {env: {DB_VERSION: \"2\"}}/' .devopsy/config.yaml")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod at 2 (%d):\n%s", code, out)
	}
	srv(t, "echo three >imgsy/prod/shared/mnt/database/stamp")
	out, code = op(t, dir, noAgent+" devopsy @dev --sync --yes")
	stamp, _ = srv2(t, "cat imgsy/dev/shared/mnt/database/stamp")
	if code == 0 || strings.TrimSpace(stamp) != "two" || !strings.Contains(out, "devopsy.data.version=1 is lower than the parent's data's version, 2") {
		t.Fatalf("a sync onto a child below the parent (%d), stamp %q:\n%s", code, stamp, out)
	}
}

// A run step that builds (`up --build`, which older configs keep) builds onto its release's
// own name, the image: devopsy's compose file gives each built service, a tag only that
// release writes: the release goes live on what it built, and no name is left that no
// listing has (never one named by an id, which a build cannot tag).
func TestBuildModeRunStepThatBuilds(t *testing.T) {
	dir := project(t, "upbuild", map[string]string{
		// As devopsy --init writes it.
		".devopsy/config.yaml":  "project: upbuild\ndefaults:\n  release: up -d --wait --remove-orphans --build\n  rollback: up -d --wait --remove-orphans --build\nenvironments:\n  prod: {}\n",
		".devopsy/compose.yaml": "services:\n  app:\n    build: .\n    command: [sleep, infinity]\n    stop_grace_period: 0s\n",
		".devopsy/Dockerfile":   "FROM busybox:1.37\n",
	})
	commitAll(t, dir)
	t.Cleanup(func() {
		srv(t, "docker images --format '{{.Repository}}:{{.Tag}}' | grep '^sha256:' | sed 's|^|docker.io/library/|' | xargs -r docker rmi >/dev/null 2>&1 || true")
	})
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("the release, its run step up --build (%d):\n%s", code, out)
	}
	if out, _ := srv(t, "docker ps --filter label=com.docker.compose.project=upbuild-prod --format '{{.State}}'"); strings.TrimSpace(out) != "running" {
		t.Errorf("not running: %q", out)
	}
	if out, code := op(t, dir, "devopsy @server:prod build"); code != 0 {
		t.Errorf("a build by hand in the release (%d):\n%s", code, out)
	}
	if out, _ := srv(t, "docker images --format '{{.Repository}}:{{.Tag}} {{.ID}}' | grep '^sha256:' || true"); strings.TrimSpace(out) != "" {
		t.Errorf("the run step's build left an image named like the id devopsy's compose file pins, which nothing lists or removes:\n%s", out)
	}
}
