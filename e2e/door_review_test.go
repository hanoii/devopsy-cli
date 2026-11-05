//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// A project user's flows TestDoorProjectsLife does not run, each on today's scripts.

// A project the rules refuse (no user:) is told so before anything changes: the refusal
// must not land past the switch, where it stops the live release and brings the old one back.
func TestDoorReviewRefusedProject(t *testing.T) {
	good := `services:
  web:
` + doorService + `    command: [sleep, infinity]
`
	dir := doorProject(t, "doorrefuse", good, nil)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a first release (%d):\n%s", code, out)
	}
	started := func() string {
		t.Helper()
		out, _ := doorRoot(t, "docker inspect -f '{{.State.StartedAt}}' doorrefuse-prod-web-1")
		return strings.TrimSpace(out)
	}
	before := started()
	// The same service with no user: the door refuses it.
	op(t, dir, `printf 'services:\n  web:\n    image: busybox:1.37\n    security_opt: [no-new-privileges]\n    command: [sleep, infinity]\n' >.devopsy/compose.yaml`)
	out, code := op(t, dir, "devopsy @prod --release")
	if code == 0 {
		t.Fatalf("a service with no user released:\n%s", out)
	}
	if !strings.Contains(out, "refused") || !strings.Contains(out, "user must be <uid>:<gid>") {
		t.Errorf("the refusal does not say what is wrong:\n%s", out)
	}
	t.Logf("the refusal:\n%s", out)
	// The rules run at the fetch's pull, before the switch: nothing live changes.
	if after := started(); after != before {
		t.Errorf("the live container was restarted (%s, then %s): the refusal came past the switch\n%s", before, after, out)
	}
	for _, word := range []string{"back", "restart"} {
		if strings.Contains(out, word) {
			t.Errorf("the refusal came past the switch (%q):\n%s", word, out)
		}
	}
	// A rollback whose hold is gone pulls its image again.
	if out, code := doorRoot(t, "docker image ls --format '{{.Repository}}:{{.Tag}}' | grep devopsy-doorrefuse-prod | xargs -r docker image rm"); code != 0 {
		t.Fatalf("removing the holds: %s", out)
	}
	op(t, dir, `printf '%s' `+quote(good)+` >.devopsy/compose.yaml; echo '# v2' >>.devopsy/compose.yaml`)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a second good release (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, "devopsy @prod --rollback --yes"); code != 0 {
		t.Errorf("a rollback with its holds gone (%d):\n%s", code, out)
	}
}

// Data of every kind through a project user: a named volume, a file entry, a step writing
// in an entry, --shell as devopsy.shell.user, at a terminal too, --log, --status, an import
// into a volume removed, a child's sync, --sync again, --reset, --inherit, a restore with
// --no-code, --releases, --destroy.
func TestDoorReviewDataFlows(t *testing.T) {
	dir := doorProject(t, "doorflow", `services:
  web:
`+doorService+`    command: [sleep, infinity]
    volumes: [./mnt/data:/d, ./mnt/settings.ini:/etc/settings.ini, db:/v]
    labels: {devopsy.mnt.owner: "10001:10001", devopsy.data: "true", devopsy.data.version: any, devopsy.shell: "true", devopsy.shell.user: "10001"}
volumes:
  db: {}
`, map[string]string{
		// A step writing in an entry before anything starts, as the templates do.
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nset -e\nmkdir -p \"$DEVOPSY_PROJECT_DIR/mnt/data/seeded\"\necho seed >\"$DEVOPSY_PROJECT_DIR/mnt/data/seeded/f\"\nexec devopsy up -d --wait\n",
	})
	must := func(what, script string) string {
		t.Helper()
		out, code := op(t, dir, script)
		if code != 0 {
			t.Fatalf("%s (%d):\n%s", what, code, out)
		}
		return out
	}
	check := func(what, script string) string {
		t.Helper()
		out, code := op(t, dir, script)
		if code != 0 {
			t.Errorf("%s (%d):\n%s", what, code, out)
		}
		return out
	}
	mnt := doorSpace + "/mnt/doorflow-prod"
	must("a first release", "devopsy @prod --release")
	if out, _ := doorRoot(t, "cat "+mnt+"/data/seeded/f"); !strings.Contains(out, "seed") {
		t.Errorf("the step's write in its entry: %s", out)
	}
	if out := check("--shell with a command", "devopsy @prod --shell -- id"); !strings.Contains(out, "uid=10001") {
		t.Errorf("--shell runs as %s", out)
	}
	log := "/tmp/doorflow-shell.log"
	inTerminal(t, dir, "devopsy @prod --shell -- sh -c 'tty && echo shell-ok'", log)
	if code := terminalCode(t, log); code != "exit=0" {
		out, _ := op(t, "/", "cat "+log)
		t.Errorf("--shell at a terminal: %s\n%s", code, out)
	} else if out, _ := op(t, "/", "cat "+log); !strings.Contains(out, "shell-ok") {
		t.Errorf("--shell at a terminal:\n%s", out)
	}
	check("--log", "devopsy @prod --log")
	check("--status", "devopsy @prod --status")
	check("--releases", "devopsy @prod --releases")
	check("--debug imports", "devopsy @prod --debug imports")
	// The volume is root's inside, so its data comes by an import; the entry is 10001's.
	volume := func(what, value string) {
		t.Helper()
		must(what, `rm -rf /tmp/vt && mkdir -p /tmp/vt/data /tmp/vt/db && echo dat-`+value+` >/tmp/vt/data/f && echo `+value+` >/tmp/vt/db/f && tar -C /tmp/vt -cf /tmp/vt.tar data db && devopsy @prod --import web --yes </tmp/vt.tar`)
	}
	volume("an import into the volume", "vol")
	must("writing in the entry", "devopsy @prod exec -T web sh -c 'echo dat >/d/f'")
	must("--export", "devopsy @prod --export web >/tmp/doorflow.tar")
	// The volume gone: the import makes it again.
	if out, code := doorRoot(t, "docker rm -f doorflow-prod-web-1 && docker volume rm doorflow-prod_db"); code != 0 {
		t.Fatalf("removing the volume: %s", out)
	}
	must("--import into a missing volume", "devopsy @prod --import web --yes </tmp/doorflow.tar")
	if out, _ := doorRoot(t, "docker run --rm -v doorflow-prod_db:/v busybox:1.37 cat /v/f; cat "+mnt+"/data/f"); !strings.Contains(out, "vol") || !strings.Contains(out, "dat") {
		t.Errorf("the data after the import: %s", out)
	}
	must("up after the import", "devopsy @prod up -d --wait")
	// A file entry, which only an import or a sync tells from a directory: the door made
	// it a directory at the first release's `compose config`, as Docker would at `up`.
	must("a tar with a file entry", `rm -rf /tmp/fe && mkdir -p /tmp/fe && echo ini >/tmp/fe/settings.ini && tar -C /tmp/fe -cf /tmp/fe.tar settings.ini`)
	// The door replaces its own empty directory (entryKindDiffers, readyEntry): a project user
	// could not, the directory being the deploy user's.
	fileEntry := true
	if out, code := op(t, dir, "devopsy @prod --import web --yes </tmp/fe.tar 2>&1"); code != 0 {
		fileEntry = false
		rm, _ := doorAs(t, "devopsy-doorflow", "rmdir "+mnt+"/settings.ini 2>&1")
		t.Errorf("--import of a file entry (%d):\n%s\nand the project user cannot make way for it: %s", code, out, rm)
	} else if out, _ := doorRoot(t, "test -f "+mnt+"/settings.ini && cat "+mnt+"/settings.ini"); !strings.Contains(out, "ini") {
		t.Errorf("the file entry after the import: %s", out)
	}
	must("a child's release", "devopsy @dev --release")
	child := doorSpace + "/mnt/doorflow-dev"
	if out, _ := doorRoot(t, "cat "+child+"/data/f"); !strings.Contains(out, "dat") {
		t.Errorf("the child's entry: %s", out)
	}
	if out, _ := doorRoot(t, "test -f "+child+"/settings.ini && cat "+child+"/settings.ini"); fileEntry && !strings.Contains(out, "ini") {
		t.Errorf("the child's file entry: %s", out)
	}
	if out, _ := doorRoot(t, "docker run --rm -v doorflow-dev_db:/v busybox:1.37 cat /v/f"); !strings.Contains(out, "vol") {
		t.Errorf("the child's volume: %s", out)
	}
	volume("new data", "vol3")
	if fileEntry {
		// A running container holds what it was started with: a directory, until recreated.
		must("the file entry in its container", "devopsy @prod up -d --force-recreate --wait")
		must("new data in the file", "devopsy @prod exec -T web sh -c 'echo ini3 >/etc/settings.ini'")
	}
	must("--sync of a live child", "devopsy @dev --sync --yes")
	if out, _ := doorRoot(t, "cat "+child+"/settings.ini "+child+"/data/f; docker run --rm -v doorflow-dev_db:/v busybox:1.37 cat /v/f"); (fileEntry && !strings.Contains(out, "ini3")) || !strings.Contains(out, "vol3") || !strings.Contains(out, "dat-vol3") {
		t.Errorf("the child after --sync: %s", out)
	}
	check("--reset", "devopsy @dev --reset --yes")
	check("--inherit", "devopsy @dev --inherit")
	must("a snapshot", "devopsy @prod --snapshot")
	out, code := op(t, dir, "devopsy @prod --snapshots | grep -oE '[0-9]{14}' | head -n 1")
	if code != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("listing snapshots (%d): %s", code, out)
	}
	volume("overwriting", "gone")
	check("a restore with --no-code", "devopsy @prod --snapshots restore "+strings.TrimSpace(out)+" --no-code --yes")
	if out, _ := doorRoot(t, "docker run --rm -v doorflow-prod_db:/v busybox:1.37 cat /v/f"); !strings.Contains(out, "vol3") {
		t.Errorf("the volume after the restore: %s", out)
	}
	check("--destroy of the child", "devopsy @dev --destroy --yes")
	check("--destroy with data", "devopsy @prod --destroy --yes")
	if out, _ := doorRoot(t, "ls -A "+mnt+" 2>&1; docker volume ls -q | grep doorflow"); strings.TrimSpace(out) != "" {
		t.Errorf("after --destroy: %s", out)
	}
	must("a release after --destroy", "devopsy @prod --release")
	// --destroy's `img remove` asks `docker image rm busybox:1.37` for a release's image no
	// container uses, which is the server's: nothing to do, and nothing refused.
	if out, _ := doorRoot(t, "cat "+doorSpace+"/refused.log 2>/dev/null | grep doorflow"); out != "" {
		t.Errorf("refused:\n%s", out)
	}
	if out, _ := doorRoot(t, "ls "+doorSpace+"/work 2>/dev/null | grep doorflow"); out != "" {
		t.Errorf("left in its work directory: %s", out)
	}
}
