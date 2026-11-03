package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Bug: --resolved keeps a hold whose release's own build is gone, and says
// the way on is the release that builds it ("a release builds it: devopsy
// @... --release"). But the held image then starts on the accepted data,
// and every activation first records what runs on current (img ran cut),
// so that release finds the held image's version in the history it had
// just started over, and refuses the very label its line was for: copied,
// the fix fails. (TestResolvedOwnBuildGone passes only because its fake has
// no container there.)
func TestResolvedFixReleases(t *testing.T) {
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
			// Held on HELD, the data's 12; its own, 11, gone.
			l = strings.Join([]string{"shop-prod-app", "HELD", "app", "own", "", "", "", "12", "", "shop-prod-app", "GONE", "", "", "11", ""}, "\t")
		}
		lines = append(lines, l)
	}
	write(t, list, strings.Join(lines, "\n")+"\n", 0o644)
	write(t, filepath.Join(rel, images.ComposeFile), "services:\n  \"app\":\n    labels:\n      \"devopsy.digest\": \"HELD\"\n      \"devopsy.held\": \"2\"\n", 0o644)
	write(t, s.log+".shop-prod", "FAKE_DB=stream\nFAKE_APP=mnt+import\nFAKE_WORKER=none\nFAKE_APP_BUILD=1\nFAKE_APP_VERSION=11\nFAIL_PULL=1\nFAKE_NAMES='shop-prod-app=HELD HELD=HELD'\nFAKE_IMAGES='HELD=shop-prod-app@sha256:HELD'\n", 0o644)
	write(t, s.log+".containers.shop-prod", "app HELD HELD\n", 0o644)
	write(t, filepath.Join(s.base, images.HistoryFile), "1\tapp\tran\tshop-prod-app\tHELD\t\t12\t\n", 0o644)
	write(t, filepath.Join(s.base, remote.SyncIncompleteFile), "2026-11-03 10:00:00\tapp\t"+remote.IncompleteImport+"\tcut\n", 0o644)

	_, out, code := s.runIO(nil, nil, "@prod", "--import", "app", "--resolved", "--yes")
	if code != 0 || !strings.Contains(out, "a release builds it") {
		t.Fatalf("--resolved with its own build gone (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@prod", "--release"); code != 0 {
		t.Errorf("the release its line names (%d):\n%s", code, out)
	}
}
