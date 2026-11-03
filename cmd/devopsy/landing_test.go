package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
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

// An environment marked by an import, held, its own build gone (nothing
// names it once held: any pruning on the server takes it) has one way out
// of the mark: a release refuses on the mark, an import on what it cannot
// derive, and --resolved, which accepts the data as it is, keeps the hold,
// said, with a line that can be copied (its Set knows the address: the
// line once read "$ --release"), and a release then builds its own.
func TestResolvedOwnBuildGone(t *testing.T) {
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
	compose := filepath.Join(rel, images.ComposeFile)
	write(t, compose, "services:\n  \"app\":\n    labels:\n      \"devopsy.digest\": \"HELD\"\n      \"devopsy.held\": \"2\"\n", 0o644)
	write(t, s.log+".shop-prod", "FAKE_DB=stream\nFAKE_APP=mnt+import\nFAKE_WORKER=none\nFAKE_APP_BUILD=1\nFAKE_APP_VERSION=11\nFAIL_PULL=1\nFAKE_NAMES='shop-prod-app=HELD HELD=HELD'\n", 0o644)
	write(t, filepath.Join(s.base, remote.SyncIncompleteFile), "2026-11-03 10:00:00\tapp\t"+remote.IncompleteImport+"\tcut\n", 0o644)

	_, out, code := s.runIO(nil, nil, "@prod", "--import", "app", "--resolved", "--yes")
	if code != 0 || !strings.Contains(out, "own image of it, built by the project, is gone") || !strings.Contains(out, "$ devopsy @devopsy@one:prod --release\n") {
		t.Fatalf("--resolved with its own build gone (%d):\n%s", code, out)
	}
	if readMark(s.base) != nil {
		t.Errorf("the mark stays: %+v", readMark(s.base))
	}
	if c := readString(compose); !strings.Contains(c, images.HeldLabel) {
		t.Errorf("the hold went:\n%s", c)
	}
	if l := readString(list); !strings.Contains(l, "HELD\tapp\town") || !strings.Contains(l, "GONE") {
		t.Errorf("the listing:\n%s", l)
	}
	if out, code := s.run(nil, "@prod", "--release"); code != 0 {
		t.Errorf("the release it names (%d):\n%s", code, out)
	}
}

// Bug: an import lands the export's window and starts the environment's
// own image on the data, which may upgrade it (the receiving side always
// runs its own image), but nothing records that it ran: Ran's comment says
// "after a run step, a sync, an import", no import calls it, and only the
// next activation's img ran catches up. Until then what reads the history
// without an activation reads a version below what the data ran: an
// --export's header, a parent's img data, a snapshot's windows. A database
// at 13 importing a dump of 12 upgrades it; its --export then says 12, and
// an environment at 12 imports it unrefused, running 12 on data 13 changed.
func TestImportRecordsWhatRanOnTheData(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	base := filepath.Join(s.root, "shop", "prod")
	current := filepath.Join(base, "current")
	list := releaseFile(t, current, images.ListFile)
	const digests = "FAKE_IMAGES=sha256:twelve=shop-prod-app@sha256:twelve sha256:thirteen=shop-prod-app@sha256:thirteen"
	write(t, s.log+".shop-prod", "FAKE_APP=mnt+own\n", 0o644)
	// An export of the data at 12.
	write(t, list, "shop-prod-app\tsha256:twelve\tapp\tpulled\tshop-prod-app@sha256:twelve\t\t\t12\tlatest\n", 0o644)
	write(t, filepath.Join(base, images.HistoryFile), "1\tapp\tran\tshop-prod-app\tsha256:twelve\tshop-prod-app@sha256:twelve\t12\tlatest\n", 0o644)
	write(t, filepath.Join(s.mnt("prod"), "storage", "file.txt"), "at 12\n", 0o644)
	exported, errs, code := s.runIO([]string{"FAKE_APP_VERSION=12", digests}, nil, "@prod", "--export", "app")
	if code != 0 {
		t.Fatalf("the export (%d):\n%s", code, errs)
	}
	// Since, a release at 13, whose container runs.
	write(t, list, "shop-prod-app\tsha256:thirteen\tapp\tpulled\tshop-prod-app@sha256:thirteen\t\t\t13\tlatest\n", 0o644)
	write(t, filepath.Join(base, images.HistoryFile), "2\tapp\tran\tshop-prod-app\tsha256:thirteen\tshop-prod-app@sha256:thirteen\t13\tlatest\n", 0o644)
	write(t, s.log+".containers.shop-prod", "app shop-prod-app@sha256:thirteen sha256:thirteen\n", 0o644)
	env := []string{"FAKE_APP_VERSION=13", digests}
	if _, errs, code := s.runIO(env, exported, "@prod", "--import", "app", "--yes"); code != 0 {
		t.Fatalf("the import of the dump at 12 into 13 (%d):\n%s", code, errs)
	}
	// 13 was stopped, the data replaced, 13 started on it.
	if docker, _ := s.logs(); !strings.Contains(docker, "start app") {
		t.Fatalf("setup: app did not start again on the data:\n%s", docker)
	}
	again, errs, code := s.runIO(env, nil, "@prod", "--export", "app")
	if code != 0 {
		t.Fatalf("the export after it (%d):\n%s", code, errs)
	}
	spool := filepath.Join(t.TempDir(), "export.tar")
	write(t, spool, string(again), 0o644)
	record := images.DataFor(tarRecord(spool), "app")
	if !strings.Contains(record, "\t13\t") {
		t.Errorf("app runs 13 on the imported data, and its export says only:\n%s", record)
	}
}

// Bug: an import made again only what it imported when that ran through it
// (recreateRunning); restoreServices now takes every running volumes
// service whose container another activation made, whatever the import
// wrote. An import of db recreates app, which it never touched and which
// runs on (devopsy.data.stop never): a release cut off past its switch
// (nothing finishes it by itself, Sessions, drops and signals) goes live
// for app without its run step, an unrelated service restarts, and its
// up failing fails an import that completed.
func TestImportLeavesOtherServicesContainers(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	base := filepath.Join(s.root, "shop", "prod")
	current := filepath.Join(base, "current")
	write(t, s.log+".shop-prod", "FAKE_DB=mnt\nFAKE_APP=mnt\n", 0o644)
	list := releaseFile(t, current, images.ListFile)
	write(t, list, "shop-prod-app\tsha256:new\tapp\town\t\t\t\tany\t\nshop-prod-db\tsha256:db\tdb\town\t\t\t\tany\t\n", 0o644)
	// app's container is the release before's, db's this one's.
	write(t, s.log+".containers.shop-prod", "app sha256:old\ndb sha256:db\n", 0o644)
	if err := os.MkdirAll(filepath.Join(s.mnt("prod"), "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.logs()
	_, errs, code := s.runIO(nil, tarOf(t, map[string]string{"db/file.txt": "imported\n"}), "@prod", "--import", "db", "--yes")
	docker, _ := s.logs()
	if code != 0 {
		t.Fatalf("the import of db (%d):\n%s", code, errs)
	}
	if strings.Contains(docker, "up -d --no-deps --wait app") {
		t.Errorf("an import of db made app again:\n%s\n%s", errs, docker)
	}
}

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

// --resolved keeps a hold whose release's own build is gone, and the held
// image then starts on the accepted data: every activation first records
// what runs on current (img ran cut), so a release at the label it had is
// refused, rightly. Its lines say so: the label raised, or accepted, which
// copied goes through (review: it named the plain release that refuses).
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
	if code != 0 || !strings.Contains(out, "its label raised to the data's version") || !strings.Contains(out, "$ devopsy @devopsy@one:prod --release --accept-image app\n") {
		t.Fatalf("--resolved with its own build gone (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@prod", "--release", "--accept-image", "app"); code != 0 {
		t.Errorf("the release its line names (%d):\n%s", code, out)
	}
}

// Bug: a marked environment's --no-code restore names what goes through when
// a label wants an image its release did not fetch (RestoreLanding's Marked:
// the full restore, as a mark refuses a release), but that check is never
// reached: --check-images runs ReloadCheck first, which asks the very same
// question (fetchedOther) over every service, and its own fix says --release,
// without reading the mark. Copied, the release refuses on the mark.
func TestNoCodeMarkedVariablesNeedAnotherImage(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.labels("FAKE_DB=none")
	shared := filepath.Join(s.root, "shop", "prod", "shared", ".env")
	write(t, shared, "FAKE_DB_IMAGE=mariadb:11\n", 0o600)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	write(t, shared, "FAKE_DB_IMAGE=mariadb:12\n", 0o600)
	write(t, filepath.Join(s.root, "shop", "prod", remote.SyncIncompleteFile), "2026-10-25 00:00:00\tapp\timport\tthe import of app did not complete\n", 0o644)
	s.logs()
	out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--no-code", "--yes")
	if code == 0 || !strings.Contains(out, "only a release fetches one") {
		t.Fatalf("the restore under variables that need another image (%d):\n%s", code, out)
	}
	if strings.Contains(out, "--release\n") {
		release, rcode := s.run(nil, "@prod", "--release")
		if rcode != 0 {
			t.Errorf("the refusal's fix is a release, which the mark refuses (%d):\n%s\nthe release:\n%s", rcode, out, release)
		}
	}
}

// An import's mark is timed at its latest write, so --resolved keeps only a
// window that landed since; a sync's keeps its first time.
func TestImportMarkTimesItsLatestWrite(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	path := filepath.Join(base, remote.SyncIncompleteFile)
	write(t, path, "2026-01-01 00:00:00\tapp\t"+remote.IncompleteImport+"\tcut\n", 0o644)
	if err := markIncomplete(base, "app", remote.IncompleteImport, "cut", false); err != nil {
		t.Fatal(err)
	}
	if m := remote.ParseMark(readString(path)); strings.HasPrefix(m.Since, "2026-01-01") {
		t.Errorf("a retried import keeps its first time: %q", m.Since)
	}
	write(t, path, "2026-01-01 00:00:00\tprod\t"+remote.IncompleteCopy+"\tcut\n", 0o644)
	if err := markIncomplete(base, "prod", remote.IncompleteCopy, "cut", false); err != nil {
		t.Fatal(err)
	}
	if m := remote.ParseMark(readString(path)); !strings.HasPrefix(m.Since, "2026-01-01") {
		t.Errorf("a sync's later mark took a new time: %q", m.Since)
	}
}

// Bug: parentData takes a named volume with no history for one nothing ever
// made, but what the copy skips goes by the volume being there (syncData), and
// a real volume can have no history: Ran only writes what a release's listing
// names, so a data service started by hand or by a project command (a profile
// a release left off, a one-off) has none. The push copies it, with no window:
// CheckLanding never judges it and Land keeps the child's. Here prod's db, on
// a volume at 13, is copied into a child labeled 12: its 12 runs on that data
// unrefused, where before it was refused by the parent's label.
func TestSyncVolumeWithNoHistoryIsJudged(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, s.log+".shop-prod", "FAKE_DB_VOLUME=dbdata\nFAKE_DB_NO_BIND=1\nFAKE_DB_VERSION=13\n", 0o644)
	write(t, s.log+".shop-dev", "FAKE_DB_VOLUME=dbdata\nFAKE_DB_NO_BIND=1\nFAKE_DB_VERSION=12\n", 0o644)
	s.releaseProd(nil)
	vol := filepath.Join(s.log+".vol", "shop-prod_dbdata")
	write(t, filepath.Join(vol, "ibd"), "pages at 13\n", 0o644)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	// Its volume there, its history saying nothing of it.
	os.Remove(filepath.Join(s.root, "shop", "prod", images.HistoryFile))
	write(t, filepath.Join(s.root, "shop", "dev", images.HistoryFile), "1\tdb\tran\tbusybox:12\tsha256:d12\t\t12\t\n", 0o644)
	s.logs()
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	copied := readString(filepath.Join(s.log+".vol", "shop-dev_dbdata", "ibd"))
	if code == 0 || strings.Contains(copied, "at 13") {
		t.Errorf("a child at 12 took a volume at 13 unjudged (%d), its volume %q, its history %q:\n%s", code, copied, readString(filepath.Join(s.root, "shop", "dev", images.HistoryFile)), out)
	}
}
