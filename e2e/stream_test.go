//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// sqliteProject is a project with prod and dev, its child, whose two
// services hold real SQLite databases moved by the data capability as
// dumps (devopsy.data.copy=custom): db runs during a sync (its export and
// import exec into it), app depends on it and is stopped for the sync
// (devopsy.data.stop=own: its scripts use run --rm). An export fails
// halfway, after part of its dump, while ~/fail-export-<service> exists;
// each import appends its service to ~/imports-<compose project>.
func sqliteProject(t *testing.T, name string) string {
	dir := project(t, name, map[string]string{
		".devopsy/.env":        "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml": "project: " + name + "\ndefaults:\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n",
		".devopsy/Dockerfile":  "FROM alpine:3.22\nRUN apk add --no-cache sqlite\n",
		".devopsy/compose.yaml": `services:
  db:
    build: .
    command: [sleep, infinity]
    volumes: [./mnt/db:/data]
    labels: {devopsy.data: "true", devopsy.data.copy: custom}
  app:
    build: .
    command: [sleep, infinity]
    volumes: [./mnt/app:/data]
    depends_on: [db]
    labels: {devopsy.data: "true", devopsy.data.copy: custom, devopsy.data.stop: own}
`,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
		".devopsy/capabilities/data/export": `#!/bin/sh
set -eu
case " $DEVOPSY_DATA_STOP " in
  *" $1 "*) dump="devopsy run --rm --no-deps -T $1 sqlite3 /data/app.db .dump" ;;
  *) dump="devopsy exec -T $1 sqlite3 /data/app.db .dump" ;;
esac
if [ -e "$HOME/fail-export-$1" ]; then
  $dump </dev/null | head -n 4
  exit 3
fi
exec $dump </dev/null
`,
		".devopsy/capabilities/data/import": `#!/bin/sh
set -eu
echo "$1" >>"$HOME/imports-$COMPOSE_PROJECT_NAME"
load='rm -f /data/new.db && sqlite3 -bail /data/new.db && mv /data/new.db /data/app.db'
case " $DEVOPSY_DATA_STOP " in
  *" $1 "*) exec devopsy run --rm --no-deps -T "$1" sh -c "$load" ;;
esac
exec devopsy exec -T "$1" sh -c "$load"
`,
	})
	commitAll(t, dir)
	return dir
}

// rows are the values of table t in service's database of env, one per
// line, sorted; "" without one.
func rows(t *testing.T, name, env, service string) string {
	t.Helper()
	out, _ := srv(t, "docker run --rm -v \"$PWD\"/"+name+"/"+env+"/shared/mnt/"+service+":/data "+name+"-"+env+"-"+service+" sqlite3 /data/app.db 'select v from t order by v' 2>/dev/null")
	return strings.Join(strings.Fields(out), " ")
}

// startedAt is when the container of service in compose project p last
// started, on the server.
func startedAt(t *testing.T, p, service string) string {
	t.Helper()
	out, _ := srv(t, "docker inspect -f '{{.State.StartedAt}}' "+p+"-"+service+"-1")
	return strings.TrimSpace(out)
}

// Real databases synced as dumps: a child's first release imports them
// in depends_on order (db before app, against the alphabet), each into
// its own service; a sync replaces the child's rows with the parent's,
// stopping only the own service on both sides; an export that
// fails halfway through its dump leaves the child's database as it was
// and its spool gone; and --export piped into --import moves one
// environment's database into another's.
func TestSyncStreamDatabase(t *testing.T) {
	const name = "sqldb"
	dir := sqliteProject(t, name)
	sql := func(env, service, stmt string) {
		t.Helper()
		if out, code := srv(t, "docker exec "+name+"-"+env+"-"+service+"-1 sqlite3 /data/app.db "+quote(stmt)); code != 0 {
			t.Fatalf("%s %s (%d): %s", env, service, code, out)
		}
	}
	srv(t, "rm -f fail-export-db fail-export-app imports-"+name+"-dev")
	t.Cleanup(func() { srv(t, "rm -f fail-export-db fail-export-app") })
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	sql("prod", "db", "create table t(v); insert into t values('db1')")
	sql("prod", "app", "create table t(v); insert into t values('app1')")

	out, code := op(t, dir, "devopsy @dev --release")
	order, _ := srv(t, "cat imports-"+name+"-dev")
	if code != 0 || rows(t, name, "dev", "db") != "db1" || rows(t, name, "dev", "app") != "app1" {
		t.Fatalf("dev's first release, syncing (%d): db %q, app %q:\n%s", code, rows(t, name, "dev", "db"), rows(t, name, "dev", "app"), out)
	}
	if strings.Join(strings.Fields(order), " ") != "db app" {
		t.Errorf("imports in this order: %q, not db then app (depends_on)", order)
	}
	if syncState(t, name, "dev", "db") != "running" || syncState(t, name, "dev", "app") != "running" {
		t.Errorf("dev after its first release: db %q, app %q", syncState(t, name, "dev", "db"), syncState(t, name, "dev", "app"))
	}

	// A sync: the child's own rows go, the parent's come; app, own,
	// restarted on both sides, db on neither.
	sql("prod", "db", "insert into t values('db2')")
	sql("prod", "app", "insert into t values('app2')")
	sql("dev", "db", "insert into t values('dev-only')")
	before := map[string]string{}
	for _, p := range []string{name + "-prod", name + "-dev"} {
		for _, s := range []string{"db", "app"} {
			before[p+" "+s] = startedAt(t, p, s)
		}
	}
	if out, code := op(t, dir, "devopsy @dev --sync --yes"); code != 0 || rows(t, name, "dev", "db") != "db1 db2" || rows(t, name, "dev", "app") != "app1 app2" {
		t.Fatalf("a sync (%d): db %q, app %q:\n%s", code, rows(t, name, "dev", "db"), rows(t, name, "dev", "app"), out)
	}
	for _, p := range []string{name + "-prod", name + "-dev"} {
		if now := startedAt(t, p, "db"); now != before[p+" db"] {
			t.Errorf("%s's db restarted (%s, was %s): it is not own", p, now, before[p+" db"])
		}
		if now := startedAt(t, p, "app"); now == before[p+" app"] || syncState(t, name, strings.TrimPrefix(p, name+"-"), "app") != "running" {
			t.Errorf("%s's app not stopped and started again for the sync: %s, %q", p, now, syncState(t, name, strings.TrimPrefix(p, name+"-"), "app"))
		}
	}

	// The first export fails after part of its dump: nothing imported.
	sql("prod", "db", "insert into t values('db3')")
	srv(t, "touch fail-export-db; rm -f imports-"+name+"-dev")
	out, code = op(t, dir, "devopsy @dev --sync --yes")
	if imported, _ := srv(t, "cat imports-"+name+"-dev 2>/dev/null"); code == 0 || rows(t, name, "dev", "db") != "db1 db2" || strings.TrimSpace(imported) != "" {
		t.Errorf("a failed export (%d): dev's db %q, imported %q:\n%s", code, rows(t, name, "dev", "db"), imported, out)
	}
	// The record of the last sync that completed stays (remote.SyncedFile).
	if left, _ := srv(t, "ls -a "+name+"/dev | grep devopsy-sync | grep -vx '\\.devopsy-sync\\.synced' || true"); strings.TrimSpace(left) != "" {
		t.Errorf("the sync's files left: %s", left)
	}
	if syncState(t, name, "dev", "app") != "running" || syncState(t, name, "prod", "app") != "running" {
		t.Errorf("after a failed export: dev app %q, prod app %q", syncState(t, name, "dev", "app"), syncState(t, name, "prod", "app"))
	}

	// --export into --import, through a pipe.
	srv(t, "rm -f fail-export-db")
	if out, code := op(t, dir, "devopsy @prod --export db | devopsy @dev --import db --yes"); code != 0 || rows(t, name, "dev", "db") != "db1 db2 db3" {
		t.Errorf("export | import (%d): dev's db %q:\n%s", code, rows(t, name, "dev", "db"), out)
	}
	// Through a pipe, a failed export looks like a short one: the import
	// takes what came, as the README says, and only the export's own
	// status tells. A truncated SQLite dump never commits: an empty
	// database replaces the child's.
	srv(t, "touch fail-export-db")
	out, code = op(t, dir, "{ devopsy @prod --export db; echo $? >/tmp/sqldb-export.code; } | devopsy @dev --import db --yes")
	exported, _ := op(t, "/", "cat /tmp/sqldb-export.code")
	if strings.TrimSpace(exported) == "0" || !strings.Contains(out, "what was written is incomplete") {
		t.Errorf("the export's failure not visible (export %s, import %d):\n%s", exported, code, out)
	}
	if code != 0 || rows(t, name, "dev", "db") != "" {
		t.Errorf("a failed export piped into an import (%d): dev's db %q", code, rows(t, name, "dev", "db"))
	}
}
