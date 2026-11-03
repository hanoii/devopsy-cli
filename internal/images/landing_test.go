package images

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestDataRandom in image mode: every release pulls what the project
// builds (a database FROM mariadb pushed under a commit tag), so a hold
// on a built data service goes by its registry digest, a rollback pulls
// its tag, and nothing is ever built on the server.
func TestDataRandomImageMode(t *testing.T) {
	t.Parallel()
	for seed := int64(1); seed <= int64(seeds(300)); seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { dataRandomMode(t, seed, ModeImage) })
	}
}

// Bug: in image mode a hold's line with digests is taken for one the registry
// serves (source), the reason of its build: service's tag being empty. Across
// a parent in build mode, a line's digests are a local build's (the
// containerd store gives every image some), which no registry serves. A child
// in image mode, after a release that syncs whose run step failed before its
// database ran, holds on that digest: the restart's last try keeps it "for
// the run step to pull", and the previous release's run step cannot start its
// database. The same scenario in build mode runs its own image, said
// (TestRestartAsBuiltHoldOnParentsBuiltImage; README, "the previous release's
// own image runs on it").
func TestRestartImageModeHoldOnParentsLocalBuild(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	prod := newSim(t, dp, "app-prod", &out)
	if _, err := prod.release(false, builtDB("app-prod", "12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	if lines := readHistory(prod.base()).window("database"); len(lines) == 0 || len(lines[0].Digests) == 0 {
		prod.fail("setup: a local build's line has no digests here: %v", lines)
	}
	dev := newSim(t, d, "app-dev", &out)
	dev.e.mode = ModeImage
	r1Svc := Service{Name: "database", Ref: "registry.example.com/db:one", Build: true, Data: true, Method: CopyVolumes, DataVersion: "11.8"}
	d.publish("registry.example.com/db:one", "I1")
	r1, err := dev.release(false, r1Svc)
	if err != nil {
		dev.fail("dev: %v", err)
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	r2Svc := Service{Name: "database", Ref: "registry.example.com/db:two", Build: true, Data: true, Method: CopyVolumes, DataVersion: "12.1"}
	d.publish("registry.example.com/db:two", "J2")
	r2 := dev.e.release(r2Svc)
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
	defer func() { dev.e.set.Restart, dev.e.set.AsBuilt = false, false }()
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); !errors.Is(err, ErrRefused) {
		dev.fail("img restore --restart with the held image on the parent's server only: %v", err)
	}
	dev.e.set.AsBuilt = true
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); err != nil {
		dev.fail("img restore --restart --as-built: %v", err)
	}
	imgs, _, _ := composeFacts(dev.e.rel(r1))
	if img := imgs["database"]; img != "" {
		if err := d.Pull(img); err != nil {
			t.Errorf("%s's compose file points database at %s, a local build's digest on the parent's server, which no registry serves (%v): its run step cannot start it:\n%s", r1, img, err, out.String())
		}
	}

}

// Bug: an import records what runs on the data once it lands (RanCut, as
// after a run step), reading a container that "once started" counts: an
// exited one too. A service down on purpose while its data is imported stays
// down (restoreServices starts only what the import stopped), and nothing
// ran on the new data; the history says its image did. Here 11.8 data is
// imported under a stopped 12.1: its history ends on a 12.1 that never ran on
// it, so an --export carries 12.1, and the release at 11.8 that matches the
// data is refused as lower than the data's version. The next activation's
// ran cut writes the same line, so the fix is not only here.
func TestImportDownServiceRecordsNoRun(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:12.1", "J1")
	if _, err := m.release(false, dbSvc("mariadb:12.1")); err != nil {
		m.fail("setup: %v", err)
	}
	for i := range d.containers {
		if d.containers[i].project == "app-prod" {
			d.containers[i].state, d.containers[i].started = "exited", time.Unix(1, 0)
		}
	}
	data := otherAt(t, &out, "app-other", "mariadb:11.8")
	rel := m.e.rel(m.cur)
	l := m.e.set.ImportLanding(data, "database")
	if err := m.e.set.CheckLanding(rel, l); err != nil {
		m.fail("the import of 11.8 under 12.1: %v", err)
	}
	if err := m.e.set.Land(m.base(), rel, l); err != nil {
		m.fail("land: %v", err)
	}
	if err := m.e.set.RanCut(m.base(), rel); err != nil {
		m.fail("ran: %v", err)
	}
	if v := windowVersion(readHistory(m.base()).window("database")); v != "11.8" {
		t.Errorf("the data's version is %s, its history %v: nothing ran on the imported 11.8 data, its container exited before it landed", v, m.history("database"))
	}
}

// A project moving between modes: a release in build mode followed by
// one in image mode (its database pushed under a commit tag), and the
// reverse. The restart after the newer one's failed run step holds the
// older one on the failed release's image as that release's listing has
// it (pulled by digest in image mode, by the build's name in build
// mode), and a release that fixes forward then goes.
func TestDataRestartHoldsAcrossModes(t *testing.T) {
	t.Parallel()
	for _, first := range []string{ModeBuild, ModeImage} {
		second := ModeImage
		if first == ModeImage {
			second = ModeBuild
		}
		t.Run(first+"-then-"+second, func(t *testing.T) {
			t.Parallel()
			d := newFake()
			var out bytes.Buffer
			m := newSim(t, d, "app-prod", &out)
			m.strict = true
			db := func(mode, commit, version string) Service {
				s := builtDB("app-prod", version)
				if mode == ModeImage {
					s.Ref = "registry.example.com/app-prod-database:" + commit
					d.publish(s.Ref, "I"+commit)
				}
				return s
			}
			m.e.mode = first
			r1, err := m.release(false, db(first, "c1", "11.8"))
			if err != nil {
				m.fail("r1: %v", err)
			}
			m.e.mode = second
			r2, err := m.failRun(false, db(second, "c2", "12.1"))
			if err != nil {
				m.fail("r2: %v", err)
			}
			failed, _ := match(readList(m.e.rel(r2)), entry{Service: "database"})
			imgs, labels, held := composeFacts(m.e.rel(r1))
			if m.cur != r1 || m.runs("database") != failed.ID || held["database"] != r2 {
				m.fail("the restart of %s (%s) after %s (%s): runs %s, want %s; compose images %v labels %v held %v", r1, first, r2, second, m.runs("database"), failed.ID, imgs, labels, held)
			}
			if second == ModeImage && digestOf(imgs["database"]) == "" {
				m.fail("a hold on an image the registry serves goes by its digest, not %q", imgs["database"])
			}
			if l, _ := readHistory(m.base()).last("database"); l.Version != "12.1" {
				m.fail("the history ends on %+v", l)
			}
			// A release that fixes forward, in the mode the project is in now.
			if _, err := m.release(false, db(second, "c3", "12.1")); err != nil {
				m.fail("fixing forward: %v", err)
			}
			if heldIn(m.e.rel(m.cur))["database"] != "" {
				m.fail("the release that fixes forward is held: %v", heldIn(m.e.rel(m.cur)))
			}
		})
	}
}

// Data of no known version (an import without a record, a --resolved that
// starts its history over) gets one from what next runs on it: a service
// that runs once it lands is recorded at its own label there, and a lower
// one is then refused, as for any data that ran; the README and --import's
// help say so (review: they said the next release's label was taken).
func TestNoKnownVersionWhatRunsSaysIt(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:12.1", "J1")
	if _, err := m.release(false, dbSvc("mariadb:12.1")); err != nil {
		m.fail("setup: %v", err)
	}
	rel := m.e.rel(m.cur)
	l := m.e.set.ImportLanding("", "database")
	if err := m.e.set.CheckLanding(rel, l); err != nil {
		m.fail("the import of a tar made by hand: %v", err)
	}
	if err := m.e.set.Land(m.base(), rel, l); err != nil {
		m.fail("land: %v", err)
	}
	if err := m.e.set.RanCut(m.base(), rel); err != nil {
		m.fail("ran: %v", err)
	}
	d.publish("mariadb:11.8", "I1")
	if _, err := m.release(false, db118); !errors.Is(err, ErrRefused) {
		t.Errorf("a lower label on data its own image ran on (%v), its history %v:\n%s", err, m.history("database"), out.String())
	}
}

// otherAt is the window another environment's export or sync of database
// carries, which ran ref.
func otherAt(t *testing.T, out *bytes.Buffer, name, ref string) string {
	t.Helper()
	d := newFake()
	other := newSim(t, d, name, out)
	d.publish(ref, "other-"+ref)
	if _, err := other.release(false, dbSvc(ref)); err != nil {
		other.fail("the other environment: %v", err)
	}
	return other.e.set.ParentData(other.base(), other.e.rel(other.cur))
}

// Bug: derive keeps a held service its compose no longer gives (a profile
// turned off), "nothing runs it to derive for". But that branch is reached
// only when the rule allows its release's own image: one it refuses is in
// held, a few lines above. So what it keeps is exactly the hold the data no
// longer needs. Here database is held on 12.1, its profile goes off, 11.8
// data lands on it (an import names any profile's service), its own image
// allowed; the profile comes back on, ReloadCheck passes and a reload runs
// the held 12.1 on the 11.8 data, upgrading it, where "a hold goes as soon
// as the data allows the release's own image again, and nothing else keeps
// one" (README).
func TestStaleHoldOnServiceComposeLacks(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	r1, err := m.release(false, db118)
	if err != nil {
		m.fail("r1: %v", err)
	}
	d.publish("mariadb:12.1", "J1")
	if _, err := m.failRun(false, dbSvc("mariadb:12.1")); err != nil {
		m.fail("r2: %v", err)
	}
	if m.cur != r1 || m.runs("database") != "J1" {
		m.fail("setup: current %s runs %s", m.cur, m.runs("database"))
	}
	data := otherAt(t, &out, "app-other", "mariadb:11.8")
	rel := m.e.rel(r1)
	// Its profile off: compose config no longer lists it.
	m.e.svcs[r1] = []Service{proxySvc}
	l := m.e.set.ImportLanding(data, "database")
	if err := m.e.set.CheckLanding(rel, l); err != nil {
		m.fail("the import of 11.8 data, its own 11.8 allowed: %v", err)
	}
	if err := m.e.set.Land(m.base(), rel, l); err != nil {
		m.fail("land: %v", err)
	}
	// Its profile back on.
	m.e.svcs[r1] = []Service{db118}
	if needs, err := m.e.set.ReloadCheck(rel); err != nil || len(needs) != 0 {
		m.fail("a reload is refused (%v): %v", err, needs)
	}
	m.up()
	if got := m.runs("database"); got != d.names["mariadb:11.8"] {
		t.Errorf("database's data is 11.8 and its own 11.8 is allowed, and it runs %s, not its own %s: the hold on 12.1 stayed (%v) and upgrades the data:\n%s", got, d.names["mariadb:11.8"], heldIn(rel), out.String())
	}
}

// Bug: Land walks the release's listing, and a listing has the services
// compose gives without profiles, while a copy writes every profile's
// (cli.SyncServices, --profile '*'). A parent whose profile is on sends the
// window of a service whose profile is off on the child: its files land, and
// its history does not. Turned on later, the child's first release adopts its
// label, however low: 11.8 runs on the parent's 12.1 data with no refusal, no
// --accept-image, where the README says the receiving side's history becomes
// the source's for every service whose files the copy writes.
func TestLandingOutsideTheListingKeepsNoWindow(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	prod := newSim(t, dp, "app-prod", &out)
	dp.publish("mariadb:12.1", "J1")
	if _, err := prod.release(false, dbSvc("mariadb:12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev := newSim(t, d, "app-dev", &out)
	d.publish("caddy:2-alpine", "C1")
	if _, err := dev.release(false, proxySvc); err != nil {
		dev.fail("dev: %v", err)
	}
	rel := dev.e.rel(dev.cur)
	l := dev.e.set.SyncLanding(data, "vm1:prod")
	if err := dev.e.set.CheckLanding(rel, l); err != nil {
		dev.fail("the sync: %v", err)
	}
	if err := dev.e.set.Land(dev.base(), rel, l); err != nil {
		dev.fail("land: %v", err)
	}
	// Its profile turned on, at a version below the data the sync wrote.
	d.publish("mariadb:11.8", "I1")
	if _, err := dev.release(false, proxySvc, db118); !errors.Is(err, ErrRefused) {
		t.Errorf("a release runs 11.8 on the parent's 12.1 data the sync wrote, unrefused (%v), its history %v:\n%s", err, dev.history("database"), out.String())
	}
}

// Bug, the same walk the other way: a reset's wipe empties every profile's
// data (down --volumes --profile '*', every entry of mnt), and ResetLanding
// says every history starts over "the ones the copy writes nothing for
// included", but only the listing's do. A service whose profile is off keeps
// the window of data that no longer exists: turned on later, its label is
// refused for it, the very thing that comment fixed.
func TestResetOutsideTheListingKeepsItsHistory(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-dev", &out)
	d.publish("mariadb:12.1", "J1")
	d.publish("caddy:2-alpine", "C1")
	if _, err := m.release(false, proxySvc, dbSvc("mariadb:12.1")); err != nil {
		m.fail("with its profile on: %v", err)
	}
	if _, err := m.release(false, proxySvc); err != nil {
		m.fail("with its profile off: %v", err)
	}
	if err := m.e.set.Land(m.base(), m.e.rel(m.cur), m.e.set.ResetLanding()); err != nil {
		m.fail("the wipe: %v", err)
	}
	d.publish("mariadb:11.8", "I1")
	if _, err := m.release(false, proxySvc, db118); err != nil {
		t.Errorf("its data was wiped and no copy wrote any, and a release is refused against it (%v), its history %v:\n%s", err, m.history("database"), out.String())
	}
}

// Bug: the fix of a refused copy that needs a build keeps what the operator
// ran for a sync (ReleaseSync, its --from) but not for a --no-code restore
// --with-env: its labels were judged with the snapshot's variables, and the
// full restore it names, without --with-env, runs under the environment's as
// they are, another version, another image. --check-images' own line for the
// same restore keeps it ("--snapshots restore <id> --with-env").
func TestCopyNeedsFixKeepsWithEnv(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-qa", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	r1, err := m.release(false, builtDB("app-qa", "11.8"))
	if err != nil {
		m.fail("r1: %v", err)
	}
	own := m.runs("database")
	dir := m.snapshot("20261102000001")
	if _, err := m.failRun(false, builtDB("app-qa", "12.1")); err != nil {
		m.fail("r2: %v", err)
	}
	if m.cur != r1 {
		m.fail("setup: current %s", m.cur)
	}
	// Its own build gone, nothing but a build gives it back: only what the
	// snapshot's bookmark held kept it.
	for n, id := range d.names {
		if id == own {
			delete(d.names, n)
		}
	}
	delete(d.images, own)
	m.e.set.Again = "--snapshots restore 20261102000001 --no-code --with-env"
	out.Reset()
	data, _ := os.ReadFile(filepath.Join(dir, "data", "images"))
	err = m.e.set.CheckLanding(m.e.rel(m.cur), m.e.set.RestoreLanding(string(data), "20261102000001", nil))
	if !errors.Is(err, ErrRefused) {
		m.fail("its own build is gone: %v", err)
	}
	if !strings.Contains(out.String(), "--snapshots restore 20261102000001 --with-env") {
		t.Errorf("the fix drops the --with-env the operator ran:\n%s", out.String())
	}
}
