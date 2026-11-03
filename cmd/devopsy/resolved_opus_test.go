package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// A landing can fail with a fix that names a release (images.Set.unhold),
// but three of its callers never tell the Set the address (--resolved,
// a reset's wipe, a restore's data step: only an import and img do), so
// the line to copy is "$ --release", which is no command.
func TestLandingFixNamesTheAddress(t *testing.T) {
	t.Parallel()
	s := newImportSetup(t)
	link, err := os.Readlink(filepath.Join(s.base, "current"))
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(s.base, filepath.Dir(link))
	write(t, filepath.Join(rel, images.RecordFile), `{"mode":"build"}`, 0o644)
	list := filepath.Join(rel, images.ListFile)
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(readString(list), "\n"), "\n") {
		if f := strings.Split(l, "\t"); f[2] == "app" {
			// Held on HELD, the data's 11.8; its own, 11, gone.
			l = strings.Join([]string{"shop-prod-app", "HELD", "app", "own", "", "", "", "11.8", "", "shop-prod-app", "GONE", "", "", "11", ""}, "\t")
		}
		lines = append(lines, l)
	}
	write(t, list, strings.Join(lines, "\n")+"\n", 0o644)
	write(t, s.log+".shop-prod", "FAKE_DB=stream\nFAKE_APP=mnt+import\nFAKE_WORKER=none\nFAKE_APP_BUILD=1\nFAKE_APP_VERSION=11\nFAIL_PULL=1\nFAKE_NAMES='shop-prod-app=HELD HELD=HELD'\n", 0o644)
	write(t, filepath.Join(s.base, remote.SyncIncompleteFile), "2026-11-03 10:00:00\tapp\t"+remote.IncompleteImport+"\tcut\n", 0o644)
	_, out, code := s.runIO(nil, nil, "@prod", "--import", "app", "--resolved", "--yes")
	if code == 0 || !strings.Contains(out, "own image of it, built by the project, is gone") {
		t.Fatalf("--resolved with its own build gone (%d):\n%s", code, out)
	}
	if strings.Contains(out, "$ --release\n") || !strings.Contains(out, "$ devopsy @devopsy@one:prod --release\n") {
		t.Errorf("the fix cannot be copied:\n%s", out)
	}
}
