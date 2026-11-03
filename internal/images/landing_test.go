package images

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A refusal's fix runs the command again accepting what was refused, but
// drops what the operator accepted already: with --accept-image db named
// and files refused too, the line says --accept-image files alone. Copied,
// db is refused, whose line says db alone, and the two lines alternate for
// good: no copied line ever goes through.
func TestRefusalFixKeepsAccepted(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:12.1", "I2")
	d.publish("mariadb:11.8", "I1")
	files := func(version string) Service {
		return Service{Name: "files", Ref: "busybox:1", Data: true, Method: CopyVolumes, DataVersion: version}
	}
	d.publish("busybox:1", "B1")
	if _, err := m.release(false, dbSvc("mariadb:12.1"), files("2")); err != nil {
		m.fail("setup: %v", err)
	}
	id := m.e.release(dbSvc("mariadb:11.8"), files("1"))
	m.e.set.Accept = map[string]bool{"database": true}
	defer func() { m.e.set.Accept = nil }()
	out.Reset()
	if err := m.e.set.Check(m.e.rel(id)); !errors.Is(err, ErrRefused) {
		m.fail("files is lower: %v", err)
	}
	if !strings.Contains(out.String(), "--accept-image database,files") && !strings.Contains(out.String(), "--accept-image files,database") {
		t.Errorf("the fix drops what was accepted already:\n%s", out.String())
	}
}

// A sync's refusal keeps the operator's --from in its --accept-image line
// (Set.Again), but its other fix, the release that builds what a copy
// needs, says --release-sync alone: copied, it syncs from the configured
// parent, another source than the one the operator named, or fails on a
// parent that does not resolve.
func TestCopyNeedsFixKeepsFrom(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	prod := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	if _, err := prod.release(false, dbSvc("mariadb:11.8")); err != nil {
		prod.fail("setup: %v", err)
	}
	qa := newSim(t, d, "app-qa", &out)
	qa.release(false, builtDB("app-qa", "11.8"))
	own := qa.runs("database")
	qa.failRun(false, builtDB("app-qa", "12.1"))
	for n, id := range d.names {
		if id == own {
			delete(d.names, n)
		}
	}
	delete(d.images, own)
	qa.e.set.Again = "--sync --from vm2:prod"
	out.Reset()
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	if err := qa.e.set.CheckLanding(qa.e.rel(qa.cur), qa.e.set.SyncLanding(data, "")); !errors.Is(err, ErrRefused) {
		qa.fail("its own build is gone: %v", err)
	}
	if !strings.Contains(out.String(), "--release-sync --from vm2:prod") {
		t.Errorf("the fix drops the source the operator named:\n%s", out.String())
	}
}

// A child on another server than its parent, whose database the parent
// builds: a release that syncs lands the parent's window (its built
// image's lines, an image only the parent's server has), and its run
// step fails before the child's database ran. The restart of the release
// before, lower than the parent's data, holds; the held image cannot be
// had here, so the restart's last try (--as-built) should hold on the
// previous release's own name, that lower image running on the parent's
// data, said (README, "Data and its images"). Instead holdOn keeps the
// parent's own name as the hold's image: (the parent's compose name
// -database), which this server has no image of, so the previous
// release's compose file points its database at a name nothing here
// holds, and its run step's up cannot start it.
func TestRestartAsBuiltHoldOnParentsBuiltImage(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	prod := newSim(t, dp, "app-prod", &out)
	if _, err := prod.release(false, builtDB("app-prod", "12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	dev := newSim(t, d, "app-dev", &out)
	r1, err := dev.release(false, builtDB("app-dev", "11.8"))
	if err != nil {
		dev.fail("dev: %v", err)
	}
	own := dev.runs("database")
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	r2 := dev.e.release(builtDB("app-dev", "12.1"))
	rel := dev.e.rel(r2)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("the release that syncs at the parent's version: %v", err)
	}
	dev.fetch(dev.e.svcs[r2])
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = r2
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	// The run step fails before the database is made again: it still
	// runs r1's build, which img ran does not count.
	dev.e.set.Ran(dev.base(), rel)
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	dev.e.set.SyncData = ""
	// The restart, as get_images runs it: --restart refuses the hold (the
	// parent's image is not here), then --as-built holds all the same.
	dev.e.set.Restart = true
	defer func() { dev.e.set.Restart, dev.e.set.AsBuilt = false, false }()
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); !errors.Is(err, ErrRefused) {
		dev.fail("img restore --restart with the held image on the parent's server only: %v", err)
	}
	dev.e.set.AsBuilt = true
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); err != nil {
		dev.fail("img restore --restart --as-built: %v", err)
	}
	imgs, labels, _ := composeFacts(dev.e.rel(r1))
	if img := imgs["database"]; img != "" {
		if _, err := d.ID(img); err != nil {
			t.Errorf("%s's compose file points database at %s, which this server has no image of (%v): its run step cannot start it; the previous release's own name should hold it, %s running on the data, said:\n%s", r1, img, err, own, out.String())
		}
	}
	if labels["database"] != own {
		t.Errorf("database's digest label is %s, not %s's own build %s", labels["database"], r1, own)
	}
}

// releaseSnapshot writes a release snapshot's manifest at id, taken by
// the activation that replaced rel (its data as it was then).
func releaseSnapshot(t *testing.T, m *sim, id, rel string) {
	t.Helper()
	dir := filepath.Join(m.e.set.Snapshots, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, SnapshotManifest), []byte("format\t1\nkind\trelease\nrelease\t"+rel+"\ntaken\t2026-11-02T00:00:00Z\ncopy\tnone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A refused rollback names the release snapshot from before the data's
// change, so that the operator restores that data, not older data. The
// fix takes the newest release snapshot whose manifest names the
// rollback's target (snapshotFix): right when the data upgrade is the
// current release (its activation took the target's data). But when the
// upgrade failed and the restart holds the previous release on it, the
// rollback's target is one release further back, and the fix names the
// snapshot of that older release's data, taken when the held release
// replaced it: everything written between those two releases is lost by
// the restore it tells the operator to run. The data from before the
// change is the current (held) release's, in the snapshot the failed
// release took.
func TestRollbackRefusalNamesSnapshotBeforeChange(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	d.publish("mariadb:11.8", "I1")
	r0, _ := m.release(false, db118)
	r1, _ := m.release(false, db118)
	// r1's activation snapshotted r0's data; the data written while r1
	// ran is in no snapshot yet.
	releaseSnapshot(t, m, "20261102000001", r0)
	d.publish("mariadb:12.1", "J1")
	r2, _ := m.failRun(false, dbSvc("mariadb:12.1"))
	// r2's activation snapshotted r1's data, from before the upgrade.
	releaseSnapshot(t, m, "20261102000002", r1)
	if m.cur != r1 || m.runs("database") != "J1" || heldIn(m.e.rel(r1))["database"] == "" {
		m.fail("setup: after %s failed, current %s runs %s, held %v", r2, m.cur, m.runs("database"), heldIn(m.e.rel(r1)))
	}
	out.Reset()
	if err := m.rollback(r0, false); !errors.Is(err, ErrRefused) {
		m.fail("the rollback to %s past the upgrade: %v", r0, err)
	}
	if !strings.Contains(out.String(), "--snapshots restore 20261102000002") || strings.Contains(out.String(), "--snapshots restore 20261102000001") {
		t.Errorf("the refusal names the snapshot of %s's data, older than the data from before the change (%s's, in 20261102000002):\n%s", r0, r1, out.String())
	}
}

// Bug: a window's line says nothing of how a service with build: got its
// image: without a tag (rule: no tag check) source takes it for an image
// built here, whatever its registry digests say. In image mode the project
// builds elsewhere and every server pulls by digest, so a child whose
// release that syncs fails before its database ran can be held on the
// parent's image: the registry has it. get_images refuses the hold ("built
// by the project, is gone"), and its last try runs the previous release's
// lower image on the parent's data.
func TestRestartImageModeHoldsByDigest(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	built := func(ref, version string) Service {
		return Service{Name: "database", Ref: ref, Build: true, Data: true, Method: CopyVolumes, DataVersion: version}
	}
	prod := newSim(t, dp, "app-prod", &out)
	prod.e.mode = ModeImage
	dp.publish("registry.example.com/db:c2", "C2")
	if _, err := prod.release(false, built("registry.example.com/db:c2", "12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	dev := newSim(t, d, "app-dev", &out)
	dev.e.mode = ModeImage
	d.publish("registry.example.com/db:c1", "C1")
	r1, err := dev.release(false, built("registry.example.com/db:c1", "11.8"))
	if err != nil {
		dev.fail("dev: %v", err)
	}
	// The registry serves the parent's image too, not pulled here yet.
	d.publish("registry.example.com/db:c2", "C2")
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	d.publish("registry.example.com/db:c3", "C3")
	r2 := dev.e.release(built("registry.example.com/db:c3", "12.1"))
	rel := dev.e.rel(r2)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("the release that syncs: %v", err)
	}
	dev.fetch(dev.e.svcs[r2])
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = r2
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	dev.e.set.Ran(dev.base(), rel)
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	dev.e.set.SyncData = ""
	dev.e.set.Restart = true
	defer func() { dev.e.set.Restart = false }()
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); err != nil {
		t.Errorf("the restart refuses a hold whose image the registry serves (%v), and its last try runs the lower image on the parent's data:\n%s", err, out.String())
		return
	}
	imgs, _, _ := composeFacts(dev.e.rel(r1))
	if digestOf(imgs["database"]) != digestOf(dp.digests["C2"][0]) {
		t.Errorf("database is held by %q, not the parent's digest:\n%s", imgs["database"], out.String())
	}
}

// Bug: notFetched refuses a landing whose labels name an image its release
// did not fetch (the rule would judge the labels, and the listed image
// would run), but only by name; ReloadCheck, which a sync and a --no-code
// restore run first, compares a built service's build too. An import runs
// only notFetched, and for a database the project builds its name never
// changes: with the README's pattern (one variable for the build argument
// and the label) a variable raised to 12.1 and no release lets an export
// at 12.1 through, and the image its release built at 11.8 starts on it.
func TestImportRefusesBuildNotFetched(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	built := func(name, version string) Service {
		s := builtDB(name, version)
		s.BuildKey = "section-" + version + ";MARIADB_VERSION=" + version
		return s
	}
	prod := newSim(t, dp, "app-prod", &out)
	if _, err := prod.release(false, built("app-prod", "12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	data := prod.e.set.ExportData(prod.base(), prod.e.rel(prod.cur), "database")
	dev := newSim(t, d, "app-dev", &out)
	if _, err := dev.release(false, built("app-dev", "11.8")); err != nil {
		dev.fail("dev: %v", err)
	}
	// --vars set MARIADB_VERSION=12.1, nothing released: what a reload or
	// a sync refuses, needing a release.
	dev.e.svcs[dev.cur] = []Service{built("app-dev", "12.1")}
	rel := dev.e.rel(dev.cur)
	needs, err := dev.e.set.ReloadCheck(rel)
	if err != nil || len(needs) == 0 {
		dev.fail("setup: ReloadCheck says the variable needs a release: %v %v", needs, err)
	}
	out.Reset()
	if err := dev.e.set.CheckLanding(rel, dev.e.set.ImportLanding(data, "database")); !errors.Is(err, ErrRefused) {
		t.Errorf("the import is let through (%v), the release's 11.8 build (%s) to start on data at 12.1, though %v:\n%s", err, dev.runs("database"), needs, out.String())
	}
}

// A copy's check refuses before anything stops what its landing would
// fail on (CheckLanding, then Land: one policy). But a data service held
// on another image whose compose no longer gives it (a profile a variable
// turned off since the release) passes the check: it is judged by the
// listing's own claim and nothing in copyNeeds minds a service compose
// lacks. Then derive, in Land, finds no service for the held entry and
// refuses ("compose no longer gives it"): the sync's copy is complete, the
// environment is marked, and what the sync stopped stays down. The check
// should refuse that sync (or an import, a --no-code restore) first, with
// nothing changed.
func TestLandingRefusesHeldServiceComposeLacksBeforeStop(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	d.publish("mariadb:11.8", "I1")
	prod.release(false, db118)
	dev.release(false, db118)
	r1, _ := dev.release(false, db118)
	d.publish("mariadb:12.1", "J1")
	dev.failRun(false, dbSvc("mariadb:12.1"))
	if dev.runs("database") != "J1" || heldIn(dev.e.rel(r1))["database"] == "" {
		dev.fail("setup: %s runs %s, held %v", r1, dev.runs("database"), heldIn(dev.e.rel(r1)))
	}
	// The database leaves compose's config under the same release.
	dev.e.svcs[r1] = []Service{proxySvc}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	rel := dev.e.rel(r1)
	out.Reset()
	checked := dev.e.set.CheckLanding(rel, dev.e.set.SyncLanding(data, ""))
	landed := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod"))
	if checked == nil && landed != nil {
		t.Errorf("the check let the sync through (nothing changed yet), and its landing failed once the copy was complete: %v\n%s", landed, out.String())
	}
	if landed != nil && !errors.Is(landed, ErrRefused) {
		t.Errorf("the landing's error: %v", landed)
	}
	if checked != nil && !strings.Contains(out.String(), "database") {
		t.Errorf("the refusal names no service:\n%s", out.String())
	}
}

// Bug: Land writes every service's labels into the listing (withClaims),
// the ones it does not land included, whose image nothing checks
// (notFetched only looks at what lands). A variable set since the release
// (cache's image and label, from one variable, raised to 8) with an import
// of another service makes the listing say cache is at 8 while its release
// fetched, and its container runs, redis:7; img ran then writes that into
// its history, and a later release back at the label of the image that
// really ran is refused as lower.
func TestLandKeepsOtherServicesClaims(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	d.publish("redis:7", "R7")
	cache := func(ref, version string) Service {
		return Service{Name: "cache", Ref: ref, Data: true, Method: CopyVolumes, DataVersion: version}
	}
	if _, err := m.release(false, dbSvc("mariadb:11.8"), cache("redis:7", "7")); err != nil {
		m.fail("setup: %v", err)
	}
	rel := m.e.rel(m.cur)
	data := m.e.set.ExportData(m.base(), rel, "database")
	// --vars set CACHE_VERSION=8, nothing released.
	m.e.svcs[m.cur] = []Service{dbSvc("mariadb:11.8"), cache("redis:8", "8")}
	l := m.e.set.ImportLanding(data, "database")
	if err := m.e.set.CheckLanding(rel, l); err != nil {
		m.fail("the import's check: %v", err)
	}
	if err := m.e.set.Land(m.base(), rel, l); err != nil {
		m.fail("the import's landing: %v", err)
	}
	if err := m.e.set.Ran(m.base(), rel); err != nil {
		m.fail("img ran: %v", err)
	}
	if last, _ := readHistory(m.base()).last("cache"); last.Version != "7" || last.Tag != "7" {
		t.Errorf("cache's container runs redis:7 (%s), which nothing fetched anew, and its history now says version %q, tag %q:\n%s", m.runs("cache"), last.Version, last.Tag, out.String())
	}
	// The variable taken back: the image that ran all along.
	m.e.svcs[m.cur] = []Service{dbSvc("mariadb:11.8"), cache("redis:7", "7")}
	if _, err := m.release(false, dbSvc("mariadb:11.8"), cache("redis:7", "7")); errors.Is(err, ErrRefused) {
		t.Errorf("a release of the image that always ran is refused as lower:\n%s", out.String())
	}
}

// Bug: the restart's last try, its hold's image not to be had, says "its
// release's own image runs" (ownInstead). But when another environment runs
// what the shared name holds now, the name stays (rightly), and nothing
// else points at its own image: no image: in its compose file, where holdOn
// and unhold hold by the release's own bookmark in that same case. Its run
// step's up runs the other environment's image on this environment's data,
// its listing and digest label naming its own, and img ran records that
// image at its own version.
func TestRestartLastTryOwnByBookmark(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	prod := newSim(t, dp, "app-prod", &out)
	if _, err := prod.release(false, builtDB("app-prod", "12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	shared := func(version string) Service {
		return Service{Name: "database", Ref: "shared-db:local", NeverPull: true, Build: true, Data: true, Method: CopyVolumes, DataVersion: version}
	}
	dev := newSim(t, d, "app-dev", &out)
	r1, err := dev.release(false, shared("11.8"))
	if err != nil {
		dev.fail("dev: %v", err)
	}
	own := dev.runs("database")
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	r2 := dev.e.release(shared("12.1"))
	rel := dev.e.rel(r2)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("the release that syncs: %v", err)
	}
	dev.fetch(dev.e.svcs[r2])
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = r2
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	dev.e.set.Ran(dev.base(), rel)
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	dev.e.set.SyncData = ""
	// Meanwhile another environment built and runs the shared name.
	stage := newSim(t, d, "app-stage", &out)
	if _, err := stage.release(false, shared("9")); err != nil {
		stage.fail("stage: %v", err)
	}
	other := stage.runs("database")
	dev.e.set.Restart = true
	defer func() { dev.e.set.Restart, dev.e.set.AsBuilt = false, false }()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); !errors.Is(err, ErrRefused) {
		dev.fail("img restore --restart with the held image on the parent's server only: %v", err)
	}
	dev.e.set.AsBuilt = true
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); err != nil {
		dev.fail("img restore --restart --as-built: %v", err)
	}
	dev.cur = r1
	dev.up()
	if got := dev.runs("database"); got != own {
		t.Errorf("dev's database runs %s (app-stage's is %s), not its own %s, its listing naming its own:\n%s", got, other, own, out.String())
	}
}

// Bug: a refused rollback names the newest release snapshot whose data
// allows it, among every directory with a manifest (snapshotFix), a take
// cut off before its rename included: a mirror's take writes its manifest
// before its tars, and only a renamed directory is a snapshot. Copied, the
// line's id is no snapshot (<id>.tmp, whose name also sorts after <id>).
func TestRollbackFixSkipsUnfinishedSnapshot(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	d.publish("mariadb:11.8", "I1")
	r0, _ := m.release(false, db118)
	releaseSnapshot(t, m, "20261102000001", r0)
	releaseSnapshot(t, m, "20261102000002.tmp", r0)
	d.publish("mariadb:12.1", "J1")
	if _, err := m.release(false, dbSvc("mariadb:12.1")); err != nil {
		m.fail("setup: %v", err)
	}
	out.Reset()
	if err := m.rollback(r0, false); !errors.Is(err, ErrRefused) {
		m.fail("the rollback past the upgrade: %v", err)
	}
	if strings.Contains(out.String(), ".tmp") || !strings.Contains(out.String(), "--snapshots restore 20261102000001") {
		t.Errorf("the fix names a take that never completed:\n%s", out.String())
	}
}
