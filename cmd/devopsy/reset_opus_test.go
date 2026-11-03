package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A reset's check runs before anything stops so that what its wipe will
// derive is refused there (images.SyncLanding reads Set.Reset), but the
// holder exports SyncResetEnv only after img sync-check: the check reads
// a plain sync. Here dev's app, which the parent copies nothing of, is
// held on another image and its own build is gone: a reset has to refuse
// before it stops anything ("a sync builds nothing"). It goes on instead,
// and only the wipe's landing finds the image gone, the data emptied.
func TestResetCheckSeesReset(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, s.log+".shop-prod", "FAKE_APP=keep\n", 0o644)
	s.releaseProd(nil)
	write(t, s.log+".shop-dev", "FAKE_APP=keep\nFAKE_APP_BUILD=1\nFAKE_APP_VERSION=11\n", 0o644)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	s.logs()
	base := filepath.Join(s.root, "shop", "dev")
	link, err := os.Readlink(filepath.Join(base, "current"))
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(base, filepath.Dir(link))
	// Built in build mode, as its record says (git ls-files keeps the
	// harness itself in image mode).
	record := filepath.Join(rel, "devopsy-release.json")
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("the release's record: %v", err)
	}
	write(t, record, `{"mode":"build"}`, 0o644)
	list := filepath.Join(rel, "devopsy-images")
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(readString(list), "\n"), "\n") {
		if f := strings.Split(l, "\t"); f[2] == "app" {
			// Held on HELD, the data's 11.8; its own, 11, gone.
			l = strings.Join([]string{"shop-dev-app", "HELD", "app", "own", "", "", "", "11.8", "", "shop-dev-app", "GONE", "", "", "11", ""}, "\t")
		}
		lines = append(lines, l)
	}
	write(t, list, strings.Join(lines, "\n")+"\n", 0o644)
	write(t, filepath.Join(base, ".devopsy-data-images"), "1\tapp\tran\tshop-dev-app\tHELD\t\t11.8\t\n", 0o644)
	write(t, s.log+".shop-dev", "FAKE_APP=keep\nFAKE_APP_BUILD=1\nFAKE_APP_VERSION=11\nFAIL_PULL=1\nFAKE_NAMES='shop-dev-app=HELD HELD=HELD'\n", 0o644)
	write(t, filepath.Join(s.mnt("dev"), "storage", "file.txt"), "dev's\n", 0o644)
	out, code := s.run(nil, "@dev", "--reset", "--yes")
	docker, _ := s.logs()
	wiped := strings.Contains(docker, "-wipe ")
	if code == 0 || !strings.Contains(out, "a sync builds nothing") || wiped || readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")) != "dev's\n" {
		t.Errorf("a reset whose wipe needs a build that is gone (%d), wiped %v:\n%s", code, wiped, out)
	}
}
