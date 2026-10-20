//go:build e2e

package e2e

import (
	"strings"
	"testing"
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
    labels: {devopsy.sync: "${SYNC_DATA:-rsync}", devopsy.sync.stop: "true"}
`,
		".devopsy/commands/deploy":           "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
		".devopsy/capabilities/sync/export":  "#!/bin/sh\nexec cat \"$DEVOPSY_PROJECT_DIR/mnt/data/stamp\"\n",
		".devopsy/capabilities/sync/import":  "#!/bin/sh\necho \"import $1\" >>\"$HOME/inhl.log\"\nexec cat >\"$DEVOPSY_PROJECT_DIR/mnt/data/stamp\"\n",
		".devopsy/capabilities/sync/prepare": "#!/bin/sh\n[ \"$1\" != parent ] || [ ! -e \"$HOME/fail-prepare-inhl\" ]\n",
	})
	t.Cleanup(func() { srv(t, "rm -f fail-prepare-inhl inhl.log") })
	inherited := func() string {
		out, _ := srv(t, "cat inhl/dev/shared/inherit.env")
		return strings.TrimSpace(out)
	}
	srv(t, "rm -f inhl.log; mkdir -p inhl/prod/shared/mnt/data && echo one >inhl/prod/shared/mnt/data/stamp && echo SYNC_DATA=rsync >inhl/prod/shared/.env")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, "devopsy @dev --release"); code != 0 || syncStamp(t, "inhl", "dev") != "one" || !strings.Contains(inherited(), "rsync") {
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
	srv(t, "echo SYNC_DATA=rsync >inhl/prod/shared/.env && echo three >inhl/prod/shared/mnt/data/stamp && touch fail-prepare-inhl")
	out, code = op(t, dir, "devopsy @dev --sync --yes")
	if code == 0 || syncStamp(t, "inhl", "dev") != "two" || !strings.Contains(inherited(), "stream") || exists(t, "inhl/dev/.devopsy-sync.inherit.env") || exists(t, "inhl/dev/.devopsy-sync.incomplete") {
		t.Errorf("an aborted sync (%d), stamp %q, inherited %q, pending %v, marked %v:\n%s", code, syncStamp(t, "inhl", "dev"), inherited(), exists(t, "inhl/dev/.devopsy-sync.inherit.env"), exists(t, "inhl/dev/.devopsy-sync.incomplete"), out)
	}
	if syncState(t, "inhl", "dev", "data") != "running" || syncState(t, "inhl", "prod", "data") != "running" {
		t.Errorf("after the abort: dev data %q, prod data %q", syncState(t, "inhl", "dev", "data"), syncState(t, "inhl", "prod", "data"))
	}
}
