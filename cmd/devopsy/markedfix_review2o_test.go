package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Bug: an import a marked environment retries, refused for what its release
// did not fetch (a variable set since) or has no more (copyNeeds), says
// "release it first, ... then import again": a line a marked environment
// refuses, whose release stops at the mark. The way that goes through is
// --resolved (as the refusal of the release itself would say), which the
// import's own refusal never names.
func TestMarkedImportFixGoesThrough(t *testing.T) {
	t.Parallel()
	s := newImportSetup(t)
	write(t, s.log+".shop-prod", "FAKE_DB=stream\nFAKE_APP=mnt+import\nFAKE_WORKER=none\nFAKE_APP_BUILD=1\nFAKE_APP_IMAGE=shop-other:2\n", 0o644)
	write(t, filepath.Join(s.base, remote.SyncIncompleteFile), "2026-11-03 10:00:00\tapp\t"+remote.IncompleteImport+"\tcut\n", 0o644)
	_, errs, code := s.runIO(nil, tarOf(t, map[string]string{"storage/file.txt": "imported\n"}), "@prod", "--import", "app", "--yes")
	if code == 0 || !strings.Contains(errs, "did not fetch") {
		t.Fatalf("the retried import (%d):\n%s", code, errs)
	}
	if strings.Contains(errs, "--release\n") {
		if out, code := s.run(nil, "@prod", "--release"); code != 0 {
			t.Errorf("the import's refusal names a release:\n%s\nwhich its mark refuses (%d):\n%s", errs, code, out)
		}
	}
}
