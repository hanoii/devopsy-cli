//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// A label from a variable the parent inherits to its child follows the
// parent's in the same sync: the parent turns its service from rsync to
// stream, and the sync streams it (the child reads the pending inherited
// file; with last sync's values it would refuse the mismatch), the new
// value in the child's inherit.env afterwards. A sync that aborts
// before writing leaves the child's inherit.env as it was.
func TestSyncInheritedLabel(t *testing.T) {
	dir := project(t, "inhl", map[string]string{
		".devopsy/.env":        "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml": "project: inhl\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {inherit: [SYNC_DATA]}\n  dev: {parent: prod}\n",
		".devopsy/compose.yaml": `services:
  data:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [./mnt/data:/data]
    labels: {devopsy.data: "${SYNC_DATA:-mnt}", devopsy.data.stop: own}
`,
		".devopsy/commands/deploy":           "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
		".devopsy/capabilities/data/export":  "#!/bin/sh\nexec cat \"$DEVOPSY_PROJECT_DIR/mnt/data/stamp\"\n",
		".devopsy/capabilities/data/import":  "#!/bin/sh\necho \"import $1\" >>\"$HOME/inhl.log\"\nexec cat >\"$DEVOPSY_PROJECT_DIR/mnt/data/stamp\"\n",
		".devopsy/capabilities/data/prepare": "#!/bin/sh\n[ \"$1\" != parent ] || [ ! -e \"$HOME/fail-prepare-inhl\" ]\n",
	})
	t.Cleanup(func() { srv(t, "rm -f fail-prepare-inhl inhl.log") })
	inherited := func() string {
		out, _ := srv(t, "cat inhl/dev/shared/inherit.env")
		return strings.TrimSpace(out)
	}
	srv(t, "rm -f inhl.log; mkdir -p inhl/prod/shared/mnt/data && echo one >inhl/prod/shared/mnt/data/stamp && echo SYNC_DATA=mnt >inhl/prod/shared/.env")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, "devopsy @dev --release"); code != 0 || syncStamp(t, "inhl", "dev") != "one" || !strings.Contains(inherited(), "mnt") {
		t.Fatalf("dev's first release (%d), stamp %q, inherited %q:\n%s", code, syncStamp(t, "inhl", "dev"), inherited(), out)
	}

	srv(t, "echo SYNC_DATA=stream >inhl/prod/shared/.env && echo two >inhl/prod/shared/mnt/data/stamp")
	out, code := op(t, dir, "devopsy @dev --sync --yes")
	imported, _ := srv(t, "cat inhl.log 2>/dev/null")
	if code != 0 || syncStamp(t, "inhl", "dev") != "two" || !strings.Contains(imported, "import data") || !strings.Contains(inherited(), "stream") {
		t.Fatalf("a sync streaming what the last one rsynced (%d), stamp %q, imports %q, inherited %q:\n%s", code, syncStamp(t, "inhl", "dev"), imported, inherited(), out)
	}
	if exists(t, "inhl/dev/.devopsy-sync.inherit.env") {
		t.Errorf("the pending inherited variables left after the sync")
	}

	// Back to rsync on the parent, whose prepare then fails: nothing
	// written, the child's variables as they were.
	srv(t, "echo SYNC_DATA=mnt >inhl/prod/shared/.env && echo three >inhl/prod/shared/mnt/data/stamp && touch fail-prepare-inhl")
	out, code = op(t, dir, "devopsy @dev --sync --yes")
	if code == 0 || syncStamp(t, "inhl", "dev") != "two" || !strings.Contains(inherited(), "stream") || exists(t, "inhl/dev/.devopsy-sync.inherit.env") || exists(t, "inhl/dev/.devopsy-sync.incomplete") {
		t.Errorf("an aborted sync (%d), stamp %q, inherited %q, pending %v, marked %v:\n%s", code, syncStamp(t, "inhl", "dev"), inherited(), exists(t, "inhl/dev/.devopsy-sync.inherit.env"), exists(t, "inhl/dev/.devopsy-sync.incomplete"), out)
	}
	if syncState(t, "inhl", "dev", "data") != "running" || syncState(t, "inhl", "prod", "data") != "running" {
		t.Errorf("after the abort: dev data %q, prod data %q", syncState(t, "inhl", "dev", "data"), syncState(t, "inhl", "prod", "data"))
	}
}

// --destroy of a child a sync left partial removes it all, its mark
// included. While a sync holds the child's release lock and the
// parent's (its import running), --destroy of either waits, says so and
// changes nothing, and goes once the sync is done.
func TestSyncDestroy(t *testing.T) {
	const name = "sdestroy"
	dir := syncProject(t, name)
	t.Cleanup(func() { srv(t, "rm -f fail-export import-wait-"+name+"-dev importing-"+name+"-dev") })
	srv(t, "rm -f fail-export")
	syncReleaseBoth(t, name, dir)
	srv(t, "touch fail-export")
	if out, code := op(t, dir, "devopsy @dev --sync --yes"); code == 0 || !exists(t, name+"/dev/.devopsy-sync.incomplete") {
		t.Fatalf("no partial copy (%d):\n%s", code, out)
	}
	srv(t, "rm -f fail-export")
	left := func(env string) string {
		out, _ := srv(t, "docker ps -aq --filter label=com.docker.compose.project="+name+"-"+env)
		return strings.TrimSpace(out)
	}
	if out, code := op(t, dir, "devopsy @dev --destroy --yes --purge"); code != 0 || exists(t, name+"/dev") || left("dev") != "" {
		t.Fatalf("--destroy of a partial child (%d), left %v, containers %q:\n%s", code, exists(t, name+"/dev"), left("dev"), out)
	}

	// Released again, syncing; then a sync whose import lasts.
	if out, code := op(t, dir, "devopsy @dev --release"); code != 0 || syncStamp(t, name, "dev") != "one" {
		t.Fatalf("dev released again (%d), stamp %q:\n%s", code, syncStamp(t, name, "dev"), out)
	}
	srv(t, "rm -f importing-"+name+"-dev; echo 20 >import-wait-"+name+"-dev")
	op(t, "/", "rm -f /tmp/"+name+"*.log")
	background(t, dir, fast()+" devopsy @dev --sync --yes; echo exit=$?", "/tmp/"+name+".log")
	eventually(t, time.Minute, "the import running", func() bool { return exists(t, "importing-"+name+"-dev") })
	for _, env := range []string{"dev", "prod"} {
		background(t, dir, "devopsy @"+env+" --destroy --yes --purge; echo exit=$?", "/tmp/"+name+"-"+env+".log")
	}
	eventually(t, 30*time.Second, "both --destroy waiting", func() bool {
		out, _ := op(t, "/", "grep -l 'waiting for' /tmp/"+name+"-dev.log /tmp/"+name+"-prod.log | wc -l")
		return strings.TrimSpace(out) == "2"
	})
	if out, _ := op(t, dir, "devopsy @dev --status"); !strings.Contains(out, "a sync into it is running") {
		t.Errorf("--status during the sync:\n%s", out)
	}
	if out, _ := op(t, dir, "devopsy @dev --releases"); true {
		t.Logf("--releases during the sync:\n%s", out)
	}
	if syncStamp(t, name, "dev") != "one" || syncStamp(t, name, "prod") != "one" || syncState(t, name, "prod", "db") != "running" {
		t.Errorf("changed under the sync: dev stamp %q, prod stamp %q, prod db %q", syncStamp(t, name, "dev"), syncStamp(t, name, "prod"), syncState(t, name, "prod", "db"))
	}
	eventually(t, 2*time.Minute, "both destroyed", func() bool { return !exists(t, name+"/dev") && !exists(t, name+"/prod") })
	for _, log := range []string{name, name + "-dev", name + "-prod"} {
		// A directory gone is not devopsy done: --purge looks after it.
		var out string
		eventually(t, time.Minute, log+" ended", func() bool {
			out, _ = op(t, "/", "cat /tmp/"+log+".log")
			return strings.Contains(out, "exit=")
		})
		if !strings.Contains(out, "exit=0") {
			t.Errorf("%s:\n%s", log, out)
		}
	}
	if left("dev")+left("prod") != "" {
		t.Errorf("containers left: %q %q", left("dev"), left("prod"))
	}
}

// rsync entries, on one server and across two: a file a running
// container bind-mounts is written in place (it sees the new content,
// through --import too), an excluded entry and one missing on the
// parent are left as the child has them, --delete removes what the
// parent lacks, and owners and modes come as they are (UID 10001).
func TestSyncRsyncEntries(t *testing.T) {
	for _, across := range []bool{false, true} {
		name, child, childSrv, env := "entries", server, srv, ""
		if across {
			secondServer(t)
			name, child, childSrv, env = "entriesx", server2, srv2, agent+" "
		}
		t.Run(name, func(t *testing.T) {
			dir := project(t, name, map[string]string{
				".devopsy/.env":        "DEVOPSY_SERVER=server\n",
				".devopsy/config.yaml": "project: " + name + "\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n",
				".devopsy/compose.yaml": `services:
  files:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [./mnt/files:/files, ./mnt/logs:/logs, ./mnt/gone:/gone]
    labels: {devopsy.data: mnt, devopsy.data.exclude: logs}
  conf:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [./mnt/app.ini:/etc/app.ini]
    labels: {devopsy.data: mnt}
`,
				".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
			})
			if across {
				op(t, dir, "echo DEVOPSY_SERVER_DEV=server2 >>.devopsy/.env")
			}
			p, c := name+"/prod/shared/mnt", name+"/dev/shared/mnt"
			conf := func() string {
				out, _ := childSrv(t, "docker exec "+name+"-dev-conf-1 cat /etc/app.ini")
				return strings.TrimSpace(out)
			}
			srv(t, "mkdir -p "+p+"/files/sub "+p+"/logs && echo v1 >"+p+"/app.ini && echo prod >"+p+"/files/sub/a && echo secret >"+p+"/files/secret && echo prod-log >"+p+"/logs/access.log")
			asRoot(t, server, "chown -R 10001:10002 "+p+"/files && chmod 640 "+p+"/files/secret && chmod 750 "+p+"/files/sub")
			if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
				t.Fatalf("prod (%d):\n%s", code, out)
			}
			// Docker made it for the container: gone again.
			asRoot(t, server, "rm -rf "+p+"/gone")

			out, code := op(t, dir, env+"devopsy @dev --release")
			owners, _ := asRoot(t, child, "stat -c '%n %u:%g %a' "+c+"/files/secret "+c+"/files/sub")
			want := c + "/files/secret 10001:10002 640\n" + c + "/files/sub 10001:10002 750\n"
			if code != 0 || owners != want || conf() != "v1" {
				t.Fatalf("dev's first release (%d), owners %q, app.ini %q:\n%s", code, owners, conf(), out)
			}
			if !strings.Contains(out, "mnt/gone does not exist") {
				t.Errorf("no notice of the entry missing on the parent:\n%s", out)
			}
			if log, _ := asRoot(t, child, "cat "+c+"/logs/access.log 2>/dev/null"); log != "" {
				t.Errorf("the excluded logs copied: %q", log)
			}

			asRoot(t, child, "echo dev-log >"+c+"/logs/access.log && echo kept >"+c+"/gone/keep && echo extra >"+c+"/files/extra")
			asRoot(t, server, "echo prod2 >"+p+"/files/sub/a")
			srv(t, "echo v2 >"+p+"/app.ini")
			if out, code := op(t, dir, env+"devopsy @dev --sync --yes"); code != 0 || conf() != "v2" {
				t.Fatalf("a sync (%d): the running container reads %q:\n%s", code, conf(), out)
			}
			got, _ := asRoot(t, child, "cat "+c+"/files/sub/a "+c+"/logs/access.log "+c+"/gone/keep; ls "+c+"/files")
			if got != "prod2\ndev-log\nkept\nsecret\nsub\n" {
				t.Errorf("after the sync: %q", got)
			}

			srv(t, "echo v3 >"+p+"/app.ini")
			if out, code := op(t, dir, "devopsy @prod --export conf | "+env+"devopsy @dev --import conf --yes"); code != 0 || conf() != "v3" {
				t.Errorf("export | import of the file (%d): the running container reads %q:\n%s", code, conf(), out)
			}
		})
	}
}

// A child's first release copies what the labels say, and what the
// parent lacks goes. Then the child guard: with an unlabeled service
// mounting the copied entry running on the child, a sync refuses before
// the parent stops anything, the child's data as it was and its
// services back.
func TestSyncChildGuard(t *testing.T) {
	dir := project(t, "whole", map[string]string{
		".devopsy/.env":        "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml": "project: whole\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n",
		".devopsy/compose.yaml": `services:
  all:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [./mnt/db:/all]
    labels: {devopsy.data: mnt, devopsy.data.stop: own}
  db:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [./mnt/db:/db]
`,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	stamp := func(env string) string {
		out, _ := srv(t, "cat whole/"+env+"/shared/mnt/db/stamp")
		return strings.TrimSpace(out)
	}
	srv(t, "mkdir -p whole/prod/shared/mnt/db whole/dev/shared/mnt/db && echo one >whole/prod/shared/mnt/db/stamp && echo extra >whole/dev/shared/mnt/db/extra")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	out, code := op(t, dir, "devopsy @dev --release")
	if code != 0 || stamp("dev") != "one" || exists(t, "whole/dev/shared/mnt/db/extra") {
		t.Fatalf("dev's first release (%d), stamp %q, extra %v:\n%s", code, stamp("dev"), exists(t, "whole/dev/shared/mnt/db/extra"), out)
	}

	srv(t, "echo two >whole/prod/shared/mnt/db/stamp")
	before := map[string]string{}
	for _, p := range []string{"whole-prod", "whole-dev"} {
		for _, s := range []string{"all", "db"} {
			before[p+" "+s] = startedAt(t, p, s)
		}
	}
	out, code = op(t, dir, "devopsy @dev --sync --yes")
	if code == 0 || !strings.Contains(out, "do not match") || !strings.Contains(out, "the child's db is running") || stamp("dev") != "one" || exists(t, "whole/dev/.devopsy-sync.incomplete") {
		t.Errorf("a sync over the child's running db (%d), stamp %q, marked %v:\n%s", code, stamp("dev"), exists(t, "whole/dev/.devopsy-sync.incomplete"), out)
	}
	for _, k := range []string{"whole-prod all", "whole-prod db", "whole-dev db"} {
		p, s, _ := strings.Cut(k, " ")
		if now := startedAt(t, p, s); now != before[k] {
			t.Errorf("%s restarted (%s, was %s): nothing should stop", k, now, before[k])
		}
	}
	if syncState(t, "whole", "dev", "all") != "running" || syncState(t, "whole", "prod", "all") != "running" {
		t.Errorf("after the refusal: dev all %q, prod all %q", syncState(t, "whole", "dev", "all"), syncState(t, "whole", "prod", "all"))
	}
}

// The parent starts to inherit a variable the new release requires
// (${NEW:?}): --release-sync of its child inherits it before the copy,
// so the release that syncs finds it before its switch too
// (--missing-vars, the image fetch), in the pending file the sync reads,
// as a first release finds it in inherit.env.
func TestSyncReleaseInheritsRequired(t *testing.T) {
	const name = "inhreq"
	compose := `services:
  data:
    image: busybox:1.37
    command: [sleep, infinity]
    volumes: [./mnt/data:/data]
    labels: {devopsy.data: mnt}
`
	dir := project(t, name, map[string]string{
		".devopsy/.env":            "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml":     "project: " + name + "\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n",
		".devopsy/compose.yaml":    compose,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	})
	srv(t, "mkdir -p "+name+"/prod/shared/mnt/data && echo one >"+name+"/prod/shared/mnt/data/stamp")
	for _, env := range []string{"prod", "dev"} {
		if out, code := op(t, dir, "devopsy @"+env+" --release"); code != 0 {
			t.Fatalf("%s (%d):\n%s", env, code, out)
		}
	}
	// prod inherits NEW to its children, and the project requires it.
	config := "project: " + name + "\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {inherit: [NEW]}\n  dev: {parent: prod}\n"
	compose += "    environment: {NEW: \"${NEW:?inherited from prod}\"}\n"
	op(t, dir, "printf %s "+quote(config)+" >.devopsy/config.yaml && printf %s "+quote(compose)+" >.devopsy/compose.yaml")
	srv(t, "echo NEW=fromprod >>"+name+"/prod/shared/.env && echo two >"+name+"/prod/shared/mnt/data/stamp")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod requiring NEW (%d):\n%s", code, out)
	}
	out, code := op(t, dir, "devopsy @dev --release-sync --yes")
	got, _ := srv(t, "docker exec "+name+"-dev-data-1 printenv NEW")
	if code != 0 || strings.TrimSpace(got) != "fromprod" || syncStamp(t, name, "dev") != "two" {
		t.Errorf("--release-sync inheriting what the release requires (%d): NEW %q, stamp %q:\n%s", code, strings.TrimSpace(got), syncStamp(t, name, "dev"), out)
	}
}
