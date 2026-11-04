package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// A failing finish parent go is said even when the sync had failed already
// (the parent's stopped services did not come back after a complete copy):
// the hook's own failure must not pass without a line, as an abort's does
// not, and the command still fails, the child going on with the copy.
func TestSyncParentFinishGoFailsAfterRestore(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "data", "finish"), "#!/bin/sh\necho \"finish $1 $2\"\n[ \"$1 $2\" != \"parent go\" ] || exit 7\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	out, code := s.run([]string{"FAIL_START=shop-prod"}, "@dev", "--sync", "--yes")
	if code == 0 {
		t.Errorf("the parent's services left down, the command passed:\n%s", out)
	}
	if !strings.Contains(out, "the data capability's finish parent go: exit status 7") {
		t.Errorf("a failing finish parent go after a failed restore not said:\n%s", out)
	}
	if !strings.Contains(out, "the copy had completed: going on with it") {
		t.Errorf("the child did not go on with the complete copy:\n%s", out)
	}
}
