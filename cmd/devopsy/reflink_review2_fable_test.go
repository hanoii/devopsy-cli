package main

import (
	"bytes"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Second review of feat/reflink-syncs (fable). Each test names the bug it
// proves and fails on the branch as it is.

// Bug: cloneCheck (round 1 moved it before the stop) makes the clone's
// directory, $base/.devopsy-sync.clone.<id>, through reflinks'
// os.MkdirAll, and nothing removes it when the stop that follows fails:
// runSyncPush's stopServices error path (sync.go, after cloneCheck) and
// runExport's stopSet error path (data.go) return without the os.Remove
// the other paths do (no clone made, a clone that failed). The directory
// then reads as what a push or an export killed outright left: the next
// push or export says "removing .devopsy-sync.clone.<id>, a clone an
// earlier copy left", removes the parent's containers by label and runs
// an unclone container as root, for an empty directory a clean failure
// left; --destroy-data does the same. Here: db's stop fails on the
// parent (FAIL_STOP), for a sync and for an export.
func TestReview2FableStopFailureLeavesCloneDir(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	clones := func() []string {
		left, _ := filepath.Glob(filepath.Join(s.root, "shop", "prod", remote.SyncClonePrefix+"*"))
		return left
	}

	// A sync whose stop on the parent fails after the reflink check.
	out, code := s.run([]string{"FAKE_REFLINK=1", "FAIL_STOP=shop-prod"}, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(docker, "-reflink ") || strings.Contains(docker, "-clone ") {
		t.Fatalf("the sync (%d) should fail at the stop, after the check and before any clone:\n%s\n%s", code, out, docker)
	}
	if left := clones(); len(left) > 0 {
		t.Errorf("a sync whose stop failed left the clone's directory, which nothing used: %v", left)
	}
	out, code = s.run([]string{"FAKE_REFLINK=1"}, "@dev", "--sync", "--yes")
	if code != 0 {
		t.Fatalf("the sync after (%d):\n%s", code, out)
	}
	if strings.Contains(out, "a clone an earlier copy left") {
		t.Errorf("the next sync took the directory for a killed copy's clone:\n%s", out)
	}
	s.logs()

	// An export on a target whose stop fails likewise.
	cmd := exec.Command(binary, "@prod", "--export", "db")
	cmd.Dir, cmd.Env = s.project, append(append([]string{}, s.env...), "FAKE_REFLINK=1", "FAIL_STOP=shop-prod")
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	err := cmd.Run()
	docker, _ = s.logs()
	if err == nil || !strings.Contains(docker, "-reflink ") || strings.Contains(docker, "-clone ") {
		t.Fatalf("the export (%v) should fail at the stop, after the check and before any clone:\n%s\n%s", err, stderr.String(), docker)
	}
	if left := clones(); len(left) > 0 {
		t.Errorf("an export whose stop failed left the clone's directory: %v", left)
	}
}
