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
// builds: a release that syncs lands the parent's window (its built image's
// lines, an image only the parent's server has), and its run step fails
// before the child's database ran. The restart of the release before, lower
// than the parent's data, holds; the held image cannot be had here, so the
// restart's last try (--as-built) runs an image of this server the data
// allows, the failed release's own, never the name of an image nothing here
// has (review). With none, its release's own, said.
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
	failed := dev.runs("database")
	if id, err := d.ID("app-dev-database"); err == nil {
		failed = id
	}
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
			t.Errorf("%s's compose file points database at %s, which this server has no image of (%v):\n%s", r1, img, err, out.String())
		}
	}
	if labels["database"] != failed {
		t.Errorf("database's digest label is %s, not the failed release's own build %s, which the data allows:\n%s", labels["database"], failed, out.String())
	}
	// Without it, its release's own image, said.
	delete(d.images, failed)
	for n, id := range d.names {
		if id == failed {
			delete(d.names, n)
		}
	}
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); err != nil {
		dev.fail("img restore --restart --as-built, the failed release's build gone: %v", err)
	}
	if _, labels, _ := composeFacts(dev.e.rel(r1)); labels["database"] != own {
		t.Errorf("database's digest label is %s, not %s's own build %s:\n%s", labels["database"], r1, own, out.String())
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
// changes: with docs/images.md's pattern (one variable for the build argument
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
	// The failed release's own build gone: nothing here the data allows.
	e, _ := match(readList(rel), entry{Service: "database"})
	delete(d.images, e.ID)
	for n, id := range d.names {
		if id == e.ID {
			delete(d.names, n)
		}
	}
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
// (TestRestartAsBuiltHoldOnParentsBuiltImage; docs/images.md, "the previous release's
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
// one is then refused, as for any data that ran; docs/images.md and --import's
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
// one" (docs/images.md).
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
// --accept-image, where docs/images.md says the receiving side's history becomes
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

// Bug: a copy writes every profile's data (cli.SyncDirs over cli.SyncServices,
// --profile '*'), and ParentData sends only the windows of what Set.List gives,
// the profiles that are on. With database's profile off on the parent, its
// files land on the child and its window does not, so the child's check judges
// nothing and its history stays: 11.8 runs on the parent's 12.1 data. It is
// also the chain: a window Land kept for a profile that is off (for the
// release that turns it on) goes no further than that environment. And
// an --export of such a service carries no record, so its import takes the
// next label, whatever it is.
func TestCopiedDataOfAProfileOffCarriesItsWindow(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	prod := newSim(t, dp, "app-prod", &out)
	dp.publish("mariadb:12.1", "J1")
	dp.publish("caddy:2-alpine", "C1")
	if _, err := prod.release(false, proxySvc, dbSvc("mariadb:12.1")); err != nil {
		prod.fail("with its profile on: %v", err)
	}
	if _, err := prod.release(false, proxySvc); err != nil {
		prod.fail("with its profile off: %v", err)
	}
	prod.e.set.ListAll = func(string) ([]Service, error) { return []Service{proxySvc, dbSvc("mariadb:12.1")}, nil }
	dev := newSim(t, d, "app-dev", &out)
	d.publish("mariadb:11.8", "I1")
	if _, err := dev.release(false, db118); err != nil {
		dev.fail("dev: %v", err)
	}
	if err := syncInto(t, prod, dev, false); !errors.Is(err, ErrRefused) {
		t.Errorf("the copy writes the parent's 12.1 data, and the child's 11.8 is not refused (%v), its history %v:\n%s", err, dev.history("database"), out.String())
	}
	rel := prod.e.rel(prod.cur)
	if rec := prod.e.set.ExportData(prod.base(), rel, "database"); !strings.Contains(rec, "12.1") {
		t.Errorf("the export of its data records nothing of 12.1: %q", rec)
	}
}

// Bug: a restore lands one line of no version for each service its manifest
// names and its windows lack (RestoreLanding's over), and since Land also
// takes what the listing leaves out, that now includes a service in a profile
// that is off: the manifest counts every profile (cli.SyncServices), the
// windows only the ones compose lists (SnapshotData, by Set.List). A snapshot
// of an environment with database's profile off, restored, wipes database's
// real window though its data comes back as it was: with the profile on
// again, 11.8 runs on data 12.1 upgraded, unrefused.
func TestRestoreKeepsTheWindowOfAProfileOff(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	d.publish("mariadb:12.1", "J1")
	d.publish("caddy:2-alpine", "C1")
	if _, err := m.release(false, proxySvc, dbSvc("mariadb:12.1")); err != nil {
		m.fail("with its profile on: %v", err)
	}
	if _, err := m.release(false, proxySvc); err != nil {
		m.fail("with its profile off: %v", err)
	}
	m.e.set.ListAll = func(string) ([]Service, error) { return []Service{proxySvc, dbSvc("mariadb:12.1")}, nil }
	dir := m.snapshot("20260101000000")
	data, err := os.ReadFile(filepath.Join(dir, "data", "images"))
	if err != nil {
		t.Fatal(err)
	}
	// As snapshotLanding gives it: every service with data its manifest names.
	l := m.e.set.RestoreLanding(string(data), filepath.Base(dir), []string{"database"})
	rel := m.e.rel(m.cur)
	if err := m.e.set.CheckLanding(rel, l); err != nil {
		m.fail("the restore's check: %v", err)
	}
	if err := m.e.set.Land(m.base(), rel, l); err != nil {
		m.fail("the data step: %v", err)
	}
	d.publish("mariadb:11.8", "I1")
	if _, err := m.release(false, proxySvc, db118); !errors.Is(err, ErrRefused) {
		t.Errorf("the restored data is 12.1's, and a release runs 11.8 on it unrefused (%v), its history %v:\n%s", err, m.history("database"), out.String())
	}
}

// The last resort derives against the data's window, as the restart it
// replaces does: a ForcedFile a cut activation left in its target is no
// acceptance of the image it now restarts on data that moved past it.
func TestDataLastResortIgnoresStaleForced(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	r1, _ := m.release(false, db118)
	// Accepted once, its activation cut before anything ran it.
	if err := os.WriteFile(filepath.Join(m.e.rel(r1), ForcedFile), []byte("database\tI1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.publish("mariadb:12.1", "J1")
	r2 := m.e.release(dbSvc("mariadb:12.1"))
	m.fetch(m.e.svcs[r2])
	if err := m.e.set.Record(m.e.rel(r2), m.e.rel(r1)); err != nil {
		m.fail("record: %v", err)
	}
	m.cur = r2
	m.up()
	if err := m.e.set.Ran(m.base(), m.e.rel(r2)); err != nil {
		m.fail("ran: %v", err)
	}
	if err := m.e.set.LastHold(m.e.rel(r1), m.e.rel(r2)); err != nil {
		m.fail("the last resort: %v", err)
	}
	if heldIn(m.e.rel(r1))["database"] == "" {
		t.Errorf("%s runs its own 11.8 on data 12.1 ran on, a stale %s taken for an acceptance:\n%s", r1, ForcedFile, out.String())
	}
}

// A parent that has data and no history of it yet sends its label's version
// and tag, as a local export does: its copy is not data of no known version
// while the label says what it is.
func TestDataNoHistorySendsItsLabel(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:12.1", "J1")
	m.release(false, dbSvc("mariadb:12.1"))
	if err := os.Remove(filepath.Join(m.base(), HistoryFile)); err != nil {
		t.Fatal(err)
	}
	data := m.e.set.ParentData(m.base(), m.e.rel(m.cur))
	p := parseParentData(data)["database"]
	if p == nil || windowVersion(p.window()) != "12.1" {
		t.Fatalf("no history, what it sends: %q", data)
	}
	if again := m.e.set.ParentData(m.base(), m.e.rel(m.cur)); again != data {
		t.Errorf("it changes between the preflight and the push, whose checksum would refuse: %q, then %q", data, again)
	}
}

// A hold by a line's digest goes through the service's own repository when
// it pulls, whichever the line names; and an export's header, whoever made
// the tar's, gives a service the project builds no image of another
// repository to pull or tag: only its version and tag land.
func TestDataHoldStaysInItsOwnRepository(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	m.release(false, db118)
	rel := m.e.rel(m.cur)
	line := ran{Service: "database", How: Ran, Ref: "evil/image:12.1", ID: "E1", Digests: []string{"evil/image@sha256:abc"}, Version: "12.1", Tag: "12.1"}
	src, _ := m.e.set.source(rel, "database", line)
	if src.Digest != "mariadb@sha256:abc" {
		t.Errorf("the line's digest is held as %q", src.Digest)
	}
	b := newSim(t, d, "app-built", &out)
	b.release(false, builtDB("app-built", "13"))
	record := "data\tdatabase\nran\t" + line.line()
	if err := b.e.set.Land(b.base(), b.e.rel(b.cur), b.e.set.ImportLanding(record, "database")); err != nil {
		b.fail("the landing: %v", err)
	}
	w := readHistory(b.base()).window("database")
	if len(w) != 1 || w[0].Version != "12.1" || w[0].ID != "" || len(w[0].Digests) != 0 {
		t.Errorf("the header's line landed as %+v", w)
	}
}

// While a built data service is held its release's own build stays: once
// nothing named it, the next pruning anywhere on the server took it, and a
// sync, an import or --resolved then had no image to give it back on.
func TestDataHeldKeepsItsOwnBuild(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	r1, _ := m.release(false, builtDB("app-prod", "11.8"))
	own := m.runs("database")
	r2, _ := m.failRun(false, builtDB("app-prod", "12.1"))
	if heldIn(m.e.rel(r1))["database"] == "" {
		m.fail("setup: not held")
	}
	named := false
	for n, id := range d.names {
		if id == own && isBookmark(n) {
			named = true
		}
	}
	if !named {
		t.Fatalf("%s's own build %s has no bookmark while held, for any pruning to take: %v", r1, own, d.names)
	}
	// Nothing is left of it once its releases go.
	for _, r := range []string{r2, r1} {
		if err := m.e.set.Remove(m.e.rel(r)); err != nil {
			m.fail("remove %s: %v", r, err)
		}
	}
	for n, id := range d.names {
		if id == own {
			t.Errorf("%s still names its own build once its releases went", n)
		}
	}
}

// A held service compose no longer gives keeps its hold when its own build
// is gone, as derive does: a copy's check does not refuse what its landing
// goes through.
func TestDataCopyNeedsAgreesWithDerive(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	r1, _ := m.release(false, builtDB("app-prod", "11.8"))
	own := m.runs("database")
	m.failRun(false, builtDB("app-prod", "12.1"))
	delete(d.images, own)
	for n, id := range d.names {
		if id == own {
			delete(d.names, n)
		}
	}
	m.e.svcs[r1] = []Service{proxySvc}
	l := m.e.set.ImportLanding("", "database")
	if err := m.e.set.CheckLanding(m.e.rel(r1), l); err != nil {
		m.fail("the check: %v\n%s", err, out.String())
	}
	if err := m.e.set.Land(m.base(), m.e.rel(r1), l); err != nil {
		m.fail("the landing: %v\n%s", err, out.String())
	}
}

// Bug: allowedHere takes a release's listing for what it could run, but a
// release whose fetch failed lists whatever the name held (Undo's Bookmark,
// the reason source ranks failed releases last): its label, with the image
// of the release before. Here r2's build failed, so it lists r1's own 11.8
// build under 12.1; the parent's 12.1 data lands, and the restart's last try
// "finds" that image as allowed: r1's older image runs on the parent's data
// with no warning, and img ran then records it as 12.1. With nothing here
// the data allows, the release's own runs, said.
func TestAllowedHereSkipsAFailedFetch(t *testing.T) {
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
	// A release whose build fails: nothing moved, undone.
	r2 := dev.e.release(builtDB("app-dev", "12.1"))
	dev.e.set.Undo(dev.e.rel(r2), dev.prev(), false)
	os.WriteFile(filepath.Join(dev.e.rel(r2), failedFile), nil, 0o644)
	// A release that syncs, its data service's version now any.
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	r3 := dev.e.release(builtDB("app-dev", AnyVersion))
	rel := dev.e.rel(r3)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("check: %v", err)
	}
	dev.fetch(dev.e.svcs[r3])
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = r3
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	dev.e.set.SyncData = ""
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	dev.e.set.Restart, dev.e.set.AsBuilt = true, true
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); err != nil {
		dev.fail("img restore --restart --as-built: %v", err)
	}
	e, _ := match(readList(dev.e.rel(r1)), entry{Service: "database"})
	if e.ID == own && e.DataVersion == "12.1" {
		t.Errorf("%s's own 11.8 build (%s) is listed as 12.1, what its next run records:\n%s", r1, own, out.String())
	}
}

// Bug: a held service's own build keeps a bookmark now (withOwnBuilds), but
// Restore still takes it for gone once the hold is over: its restart's last
// try (--as-built, the build having failed) says "its own image is gone" and
// runs what the name holds, the failed release's build, under the release's
// code. AsBuilt's promise is the reverse ("never the failed release's image
// under the name"), and the bookmark that would give it back is dropped.
// Here r1 is held on 12.1's build, a release that syncs brings the parent's
// 11.8 data, so r1's own 11.8 is allowed again, and its run step fails.
func TestAsBuiltHoldOverRunsItsOwnBuild(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	dp.publish("mariadb:11.8", "P11")
	prod := newSim(t, dp, "app-prod", &out)
	if _, err := prod.release(false, dbSvc("mariadb:11.8")); err != nil {
		prod.fail("prod: %v", err)
	}
	dev := newSim(t, d, "app-dev", &out)
	r1, err := dev.release(false, builtDB("app-dev", "11.8"))
	if err != nil {
		dev.fail("dev: %v", err)
	}
	own := dev.runs("database")
	if _, err := dev.failRun(false, builtDB("app-dev", "12.1")); err != nil {
		dev.fail("failed release: %v", err)
	}
	if heldIn(dev.e.rel(r1))["database"] == "" {
		dev.fail("setup: %s is not held", r1)
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	r3 := dev.e.release(builtDB("app-dev", "11.8"))
	rel := dev.e.rel(r3)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("check: %v", err)
	}
	dev.fetch(dev.e.svcs[r3])
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = r3
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	dev.e.set.SyncData = ""
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	dev.e.set.Restart = true
	// Its own build back from its bookmark: nothing to build, no last try.
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); err != nil {
		dev.fail("img restore --restart: %v", err)
	}
	if got := labelsIn(dev.e.rel(r1))["database"]; got != own {
		t.Errorf("%s runs %s, not its own build %s, which its bookmark kept:\n%s", r1, got, own, out.String())
	}
}

// A snapshot of an environment whose built database was held on a pulled
// image (the failed release's, by its registry digest), imported into
// another server (FilterImported clears its ids), is fully restored there:
// the hold's line names the pulled image's digest, which the registry
// serves, so the restore pulls it by that digest and holds the database on
// it, as the snapshot's listing says. Instead source re-roots the digest
// into the service's own repository, the project's build name
// (app-stage-database@sha256:...), which no registry serves: the pull
// fails and the restore is refused, saying the image is gone and could
// not be pulled by its digest.
func TestImportedSnapshotHoldOnPulledImage(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	d := newFake()
	d.strictRepo = true
	prod := newSim(t, d, "app-prod", &out)
	prod.e.set.Snapshots = filepath.Join(prod.base(), "snapshots")
	d.publish("mariadb:12.1", "J1")
	r1, err := prod.release(false, builtDB("app-prod", "11.8"))
	if err != nil {
		prod.fail("release: %v", err)
	}
	if _, err := prod.failRun(false, dbSvc("mariadb:12.1")); err != nil {
		prod.fail("the failed release: %v", err)
	}
	if heldIn(prod.e.rel(r1))["database"] == "" || prod.runs("database") != "J1" {
		prod.fail("setup: %s is not held on the failed release's image (%v)", r1, heldIn(prod.e.rel(r1)))
	}
	snap := prod.snapshot("20261017000001")

	// Another server, whose registry serves mariadb:12.1 at the same digest.
	d2 := newFake()
	d2.strictRepo = true
	d2.publish("mariadb:12.1", "J1")
	stage := newSim(t, d2, "app-stage", &out)
	stage.e.set.Snapshots = filepath.Join(stage.base(), "snapshots")
	if _, err := stage.release(false, builtDB("app-stage", "11.8")); err != nil {
		stage.fail("release: %v", err)
	}
	dir := filepath.Join(stage.e.set.Snapshots, "20261017000002")
	if err := os.CopyFS(dir, os.DirFS(snap)); err != nil {
		t.Fatal(err)
	}
	if err := FilterImported(dir, stage.e.rel(stage.cur)); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	id, err := stage.fullRestore(dir, []Service{builtDB("app-stage", "11.8")})
	if err != nil {
		t.Fatalf("the full restore of the imported snapshot is refused, the hold's image pullable by its digest (mariadb@J1): %v\n%s", err, out.String())
	}
	if heldIn(stage.e.rel(id))["database"] == "" || stage.runs("database") != "J1" {
		t.Errorf("the restored release runs database on %s, not held on the snapshot's J1 (%v)\n%s", stage.runs("database"), heldIn(stage.e.rel(id)), out.String())
	}
}

// syncedAndFailed is a child that builds its database (r1, at 11.8) after a
// release that syncs from a parent that pulls mariadb:12.1 and whose run
// step failed before the child's database ran: the window is the parent's
// line, mariadb:12.1 by its digest, and the restart of r1 must hold.
func syncedAndFailed(t *testing.T, out *bytes.Buffer) (dev *sim, d *fakeDocker, r1, failed string) {
	t.Helper()
	dp := newFake()
	d = newFake()
	d.strictRepo = true
	dp.publish("mariadb:12.1", "P12")
	d.publish("mariadb:12.1", "P12")
	prod := newSim(t, dp, "app-prod", out)
	if _, err := prod.release(false, dbSvc("mariadb:12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	dev = newSim(t, d, "app-dev", out)
	r1, err := dev.release(false, builtDB("app-dev", "11.8"))
	if err != nil {
		dev.fail("dev: %v", err)
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	failed = dev.e.release(builtDB("app-dev", "12.1"))
	rel := dev.e.rel(failed)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("check: %v", err)
	}
	dev.fetch(dev.e.svcs[failed])
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = failed
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	dev.e.set.Ran(dev.base(), rel)
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	dev.e.set.SyncData = ""
	return dev, d, r1, failed
}

// Bug: source holds a window's digest in the service's own repository,
// whatever the line names. That is right for a mirror, but a service its
// release builds has no repository in any registry: the parent's
// mariadb@<digest> becomes app-dev-database@<digest>, which nothing serves.
// The restart's last resort (img hold, once get_images failed) writes that
// into the compose file, so the run step's up cannot pull it and the
// previous release does not come back. Before the digest moved, the hold
// was mariadb@<digest>, which the run step pulls.
func TestLastHoldDigestInBuiltRepository(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dev, d, r1, failed := syncedAndFailed(t, &out)
	if err := dev.e.set.LastHold(dev.e.rel(r1), dev.e.rel(failed)); err != nil {
		dev.fail("img hold: %v", err)
	}
	imgs, _, _ := composeFacts(dev.e.rel(r1))
	if img := imgs["database"]; img != "" {
		if err := d.Pull(img); err != nil {
			t.Errorf("the restart's up cannot get what the last resort held the service on, %s: %v\n%s", img, err, out.String())
		}
	}
}

// Bug: the same in the restart's last try when the failed release's own
// image is gone too (allowedHere finds nothing): "held on it anyway, for the
// run step to pull", on a reference no registry serves.
func TestAsBuiltDigestInBuiltRepository(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dev, d, r1, failed := syncedAndFailed(t, &out)
	gone, _ := d.ID("app-dev-database")
	for n, id := range d.names {
		if id == gone {
			delete(d.names, n)
		}
	}
	delete(d.images, gone)
	dev.e.set.Restart, dev.e.set.AsBuilt = true, true
	if err := dev.e.set.Restore(dev.e.rel(r1), dev.e.rel(failed), false); err != nil {
		dev.fail("img restore --restart --as-built: %v", err)
	}
	imgs, _, _ := composeFacts(dev.e.rel(r1))
	if img := imgs["database"]; img != "" {
		if err := d.Pull(img); err != nil {
			t.Errorf("the restart's up cannot get what its last try held the service on, %s: %v\n%s", img, err, out.String())
		}
	}
}

// Bug: a data service whose only data is a named volume counts as written by
// a copy (parentData: "a named volume counts as there"), but the push skips a
// volume the parent's server does not have (syncData). So a parent's service
// in a profile that was never up writes nothing into the child, and yet
// parentData, which now goes by every profile and sends a label's line when
// there is no history, replaces the child's window with that label. Here the
// child ran search 9 on its data; the parent says 8 of a service that never
// ran there; the sync writes nothing of it, and the child's data, still
// upgraded by 9, now reads as 8: a release of 8 goes through, unrefused.
func TestParentDataNeverMadeVolumeKeepsWindow(t *testing.T) {
	t.Parallel()
	search := func(ref, version string) Service {
		return Service{Name: "search", Ref: ref, Data: true, Method: CopyVolumes, DataVersion: version}
	}
	d := newFake()
	var out bytes.Buffer
	d.publish("search:8", "S8")
	d.publish("search:9", "S9")
	d.publish("mariadb:11.8", "M1")
	prod := newSim(t, d, "app-prod", &out)
	// Its profile is off: every profile's list has it, the release's has not.
	prod.e.set.ListAll = func(string) ([]Service, error) { return []Service{dbSvc("mariadb:11.8"), search("search:8", "8")}, nil }
	prod.e.set.Copied = func(string, bool) ([]string, error) { return []string{"database"}, nil }
	if _, err := prod.release(false, dbSvc("mariadb:11.8")); err != nil {
		prod.fail("prod: %v", err)
	}
	dev := newSim(t, d, "app-dev", &out)
	if _, err := dev.release(false, dbSvc("mariadb:11.8"), search("search:9", "9")); err != nil {
		dev.fail("dev: %v", err)
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	rel := dev.e.rel(dev.cur)
	if err := dev.e.set.CheckLanding(rel, dev.e.set.SyncLanding(data, "")); err != nil {
		dev.fail("sync-check: %v", err)
	}
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	id := dev.e.release(dbSvc("mariadb:11.8"), search("search:8", "8"))
	if err := dev.e.set.Check(dev.e.rel(id)); !errors.Is(err, ErrRefused) {
		t.Errorf("search 8 on data search 9 upgraded, the sync having written none of it: %v\nwindow %+v\n%s", err, readHistory(dev.base()).window("search"), out.String())
	}
}

// heldThenOver is a child whose r1 (a build at 11.8, key k1) was held on a
// failed release's 12.1 build, then a release that syncs brought a parent's
// 11.8 data and failed its run step, so r1's own is allowed again: the
// restart of r1 is next, r3's build under the name.
func heldThenOver(t *testing.T, out *bytes.Buffer) (dev *sim, r1, r3, own string) {
	t.Helper()
	dp, d := newFake(), newFake()
	dp.publish("mariadb:11.8", "P11")
	prod := newSim(t, dp, "app-prod", out)
	if _, err := prod.release(false, dbSvc("mariadb:11.8")); err != nil {
		prod.fail("prod: %v", err)
	}
	dev = newSim(t, d, "app-dev", out)
	db := builtDB("app-dev", "11.8")
	db.BuildKey = "k1;UID=a"
	r1, err := dev.release(false, db)
	if err != nil {
		dev.fail("dev: %v", err)
	}
	own = dev.runs("database")
	if _, err := dev.failRun(false, builtDB("app-dev", "12.1")); err != nil {
		dev.fail("failed release: %v", err)
	}
	if heldIn(dev.e.rel(r1))["database"] == "" {
		dev.fail("setup: %s is not held", r1)
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	r3 = dev.e.release(builtDB("app-dev", "11.8"))
	rel := dev.e.rel(r3)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("check: %v", err)
	}
	dev.fetch(dev.e.svcs[r3])
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = r3
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	dev.e.set.SyncData = ""
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	return dev, r1, r3, own
}

// Bug: the last try (--as-built) puts a bookmark back even when its build
// changed since: that is its whole promise (Set.AsBuilt, "never the failed
// release's image under the name"), and rebuilt says so for a service never
// held. For one whose hold is over, Restore requires !changed to take the
// bookmark withOwnBuilds kept, so with a build argument changed since (the
// rebuild get_images just tried having failed) it says the own image is
// "gone" and runs what the name holds: the failed release's build, under r1's
// code and its version, with the bookmark there all along.
func TestAsBuiltHoldOverBuildChanged(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dev, r1, r3, own := heldThenOver(t, &out)
	dev.e.svcs[r1][0].BuildKey = "k2;UID=b"
	dev.e.set.Restart = true
	if err := dev.e.set.Restore(dev.e.rel(r1), dev.e.rel(r3), false); err == nil {
		dev.fail("the first try takes the changed build as it is")
	}
	dev.e.set.AsBuilt = true
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), dev.e.rel(r3), false); err != nil {
		dev.fail("img restore --restart --as-built: %v", err)
	}
	if got := labelsIn(dev.e.rel(r1))["database"]; got != own {
		t.Errorf("%s's last try runs %s, the failed release's build, not its own %s, which its bookmark keeps:\n%s", r1, got, own, out.String())
	}
}

// Bug: withOwnBuilds keeps no bookmark of its own when the held image is the
// release's own build (a label raised over the same cached build: the failed
// release's image id is r1's), since the entry's bookmark holds it already,
// and Restore looks only at the entry of no service. So once the hold is over
// the last try says the image is gone and runs the failed release's build
// under the name, though the entry's own bookmark holds exactly that build.
func TestAsBuiltHoldOverSameImage(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	dp.publish("mariadb:11.8", "P11")
	prod := newSim(t, dp, "app-prod", &out)
	if _, err := prod.release(false, dbSvc("mariadb:11.8")); err != nil {
		prod.fail("prod: %v", err)
	}
	dev := newSim(t, d, "app-dev", &out)
	r1, err := dev.release(false, builtDB("app-dev", "11.8"))
	if err != nil {
		dev.fail("dev: %v", err)
	}
	own := dev.runs("database")
	// The label raised, the build cached: the same image under 12.1.
	r2 := dev.e.release(builtDB("app-dev", "12.1"))
	if err := dev.e.set.Check(dev.e.rel(r2)); err != nil {
		dev.fail("check: %v", err)
	}
	if err := dev.e.set.Record(dev.e.rel(r2), dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = r2
	dev.up()
	dev.e.set.Ran(dev.base(), dev.e.rel(r2))
	os.WriteFile(filepath.Join(dev.e.rel(r2), failedFile), nil, 0o644)
	dev.restart(r1, r2)
	if heldIn(dev.e.rel(r1))["database"] == "" {
		dev.fail("setup: %s is not held", r1)
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	r3 := dev.e.release(builtDB("app-dev", "11.8"))
	rel := dev.e.rel(r3)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("check: %v", err)
	}
	dev.fetch(dev.e.svcs[r3])
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = r3
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	dev.e.set.SyncData = ""
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	dev.e.set.Restart, dev.e.set.AsBuilt = true, true
	out.Reset()
	if err := dev.e.set.Restore(dev.e.rel(r1), rel, false); err != nil {
		dev.fail("img restore --restart --as-built: %v", err)
	}
	if got := labelsIn(dev.e.rel(r1))["database"]; got != own {
		t.Errorf("%s's last try runs %s, the failed release's build, not its own %s, which its entry's bookmark holds:\n%s", r1, got, own, out.String())
	}
}

// Bug: a child held on an image its parent built, on one server, is held by
// image: <the parent's own name> in its compose file (holdOn: "built under
// another name"), a name the parent's next release moves. Its bookmark holds
// the right image, and the listing says that image, but the next up in the
// child (a --reload, any plain up, its run step) follows the name: the
// parent's newer build, at a version the child's data never had, on the
// child's data. holdOn guards a name others run from moving, not from a name
// moving under it.
func TestHoldOnParentsNameFollowsItsNextBuild(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	d := newFake()
	prod := newSim(t, d, "app-prod", &out)
	if _, err := prod.release(false, builtDB("app-prod", "12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	dev := newSim(t, d, "app-dev", &out)
	r1, err := dev.release(false, builtDB("app-dev", "11.8"))
	if err != nil {
		dev.fail("dev: %v", err)
	}
	// A release that syncs, whose run step fails before its database ran.
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	failed := dev.e.release(builtDB("app-dev", "12.1"))
	rel := dev.e.rel(failed)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("check: %v", err)
	}
	dev.fetch(dev.e.svcs[failed])
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = failed
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	dev.e.set.SyncData = ""
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	dev.strict = true
	dev.restart(r1, failed)
	held := dev.runs("database")
	if heldIn(dev.e.rel(r1))["database"] == "" || held != prod.runs("database") {
		dev.fail("setup: %s is not held on the parent's build (%v, runs %s)", r1, heldIn(dev.e.rel(r1)), held)
	}
	// The parent's next release builds under its name.
	if _, err := prod.release(false, builtDB("app-prod", "13")); err != nil {
		prod.fail("prod: %v", err)
	}
	// A plain up in the child, as a reload's.
	dev.up()
	if got := dev.runs("database"); got != held {
		imgs, _, _ := composeFacts(filepath.Clean(dev.e.rel(r1)))
		t.Errorf("the child's up runs the parent's next build %s on its data, not the %s it is held on (image: %s):\n%s", got, held, imgs["database"], out.String())
	}
}

// Bug: the fix for a volume nothing ever made stops at a copy (parentData's
// non-all branch): a snapshot's windows still carry, for a service whose
// volume did not exist at the take, its label's line. The take writes no tar
// of it (readSnapshotData), the restore extracts none and leaves the volume as
// it is, and yet Land replaces its window with that line. Here search was
// never up when the snapshot was taken (label 8), then ran at 9, upgrading its
// volume; a full restore leaves that volume alone, forgets 9, and search:8
// runs on it, unheld and unsaid.
func TestRestoreNeverMadeVolumeKeepsWindow(t *testing.T) {
	t.Parallel()
	search := func(ref, version string) Service {
		return Service{Name: "search", Ref: ref, Data: true, Method: CopyVolumes, DataVersion: version}
	}
	var out bytes.Buffer
	d := newFake()
	d.publish("mariadb:11.8", "M1")
	d.publish("search:8", "S8")
	d.publish("search:9", "S9")
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = m.base() + "/snapshots"
	m.e.set.ListAll = func(string) ([]Service, error) { return []Service{dbSvc("mariadb:11.8"), search("search:8", "8")}, nil }
	m.e.set.Copied = func(string, bool) ([]string, error) { return []string{"database"}, nil }
	if _, err := m.release(false, dbSvc("mariadb:11.8")); err != nil {
		m.fail("release: %v", err)
	}
	snap := m.snapshot("20261017000001")
	m.e.set.ListAll = nil
	m.e.set.Copied = nil
	// Its profile turned on, its label raised.
	if _, err := m.release(false, dbSvc("mariadb:11.8"), search("search:9", "9")); err != nil {
		m.fail("release: %v", err)
	}
	// The snapshot's code, the profile's variable still on: search at 8.
	if _, err := m.fullRestore(snap, []Service{dbSvc("mariadb:11.8"), search("search:8", "8")}); err != nil {
		m.fail("the full restore: %v", err)
	}
	if got := m.runs("search"); got != "S9" {
		t.Errorf("search runs %s on a volume 9 upgraded, which the snapshot does not hold: window %+v\n%s", got, readHistory(m.base()).window("search"), out.String())
	}
}

// Bug: a rollback of a built data service whose name another environment's
// newer build holds keeps the name, as it must, and runs that image: "stays on
// its newer image ... runs it too". For a data service the rule judged the
// release's own label, never that image: here stage's 12 build runs on prod's
// 11.8 data, upgrading it in place, and img ran writes it down as 11.8, so
// nothing refuses the older image after it. The bookmark that a hold going
// away runs by in this same case (unhold, ownNameBack) holds the right one.
func TestRollbackSharedNameRunsOthersBuildOnData(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	d := newFake()
	db := func(version string) Service {
		return Service{Name: "database", Ref: "shop-db:latest", Build: true, Data: true, Method: CopyVolumes, DataVersion: version}
	}
	prod := newSim(t, d, "app-prod", &out)
	r1, err := prod.release(false, db("11.8"))
	if err != nil {
		prod.fail("prod: %v", err)
	}
	own := prod.runs("database")
	if _, err := prod.release(false, db("11.8")); err != nil {
		prod.fail("prod: %v", err)
	}
	stage := newSim(t, d, "app-stage", &out)
	stage.n = 100
	if _, err := stage.release(false, db("12.1")); err != nil {
		stage.fail("stage: %v", err)
	}
	newer := stage.runs("database")
	out.Reset()
	if err := prod.rollback(r1, false); err != nil {
		prod.fail("rollback: %v", err)
	}
	if got := prod.runs("database"); got != own {
		t.Errorf("prod's 11.8 data runs %s, stage's 12.1 build %s, not %s, its own, which its bookmark holds: history %v\n%s", got, newer, own, prod.history("database"), out.String())
	}
}

// heldByBookmark leaves dev's first release, which pulls mariadb:11.8, held
// by its bookmark on the build a parent on the same server ran on the data:
// a release that syncs, built, whose run step fails before its database ran,
// restarted on the first. The parent's build is a name only the history
// gives, so holdOn keeps it out of the compose file.
func heldByBookmark(t *testing.T, out *bytes.Buffer) (dev *sim, d *fakeDocker, r1, parents string) {
	t.Helper()
	d = newFake()
	d.publish("mariadb:11.8", "M1")
	prod := newSim(t, d, "app-prod", out)
	if _, err := prod.release(false, builtDB("app-prod", "13")); err != nil {
		prod.fail("prod: %v", err)
	}
	parents = prod.runs("database")
	dev = newSim(t, d, "app-dev", out)
	r1, err := dev.release(false, dbAt("mariadb:11.8", "11.8"))
	if err != nil {
		dev.fail("dev: %v", err)
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData = data
	failed := dev.e.release(builtDB("app-dev", "13"))
	rel := dev.e.rel(failed)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("check: %v", err)
	}
	dev.fetch(dev.e.svcs[failed])
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = failed
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	dev.e.set.SyncData = ""
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	dev.strict = true
	dev.restart(r1, failed)
	if dev.runs("database") != parents || heldIn(dev.e.rel(r1))["database"] == "" {
		dev.fail("setup: %s is not held on the parent's build (runs %s)", r1, dev.runs("database"))
	}
	if d.names[familiar("mariadb:11.8")] != "M1" {
		dev.fail("setup: mariadb:11.8 moved already, to %s", d.names[familiar("mariadb:11.8")])
	}
	return dev, d, r1, parents
}

// Bug: a hold by bookmark lists the held image under the release's own name
// (holdOn: e.Ref = o.Ref), with Kind own. RestoreOwn takes that for a built
// image and puts the name on its bookmark, so the next release whose fetch
// fails retags the pulled mariadb:11.8 to the parent's build. From then on the
// release's "own" image is that build: a landing that lets it run again
// (unhold reads what the name holds) runs version 13 on data at 11.8, and img
// ran writes it down as 11.8.
func TestBookmarkHoldFailedFetchRetagsPulledName(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dev, d, _, parents := heldByBookmark(t, &out)
	// An export from another server, at 11.8.
	other := newFake()
	other.publish("mariadb:11.8", "M1")
	base := newSim(t, other, "app-base", &out)
	if _, err := base.release(false, dbAt("mariadb:11.8", "11.8")); err != nil {
		base.fail("base: %v", err)
	}
	d.publish("mariadb:13", "M3")
	id := dev.e.release(dbAt("mariadb:13", "13"))
	dev.fetch(dev.e.svcs[id])
	dev.e.set.Undo(dev.e.rel(id), dev.prev(), false)
	os.WriteFile(filepath.Join(dev.e.rel(id), failedFile), nil, 0o644)
	if got := d.names[familiar("mariadb:11.8")]; got != "M1" {
		t.Errorf("a failed fetch retagged the pulled mariadb:11.8 to %s (the parent's build is %s), not M1", got, parents)
	}
	// What it leads to: 11.8 data imported, whose window lets the release's
	// own run again.
	data := DataFor(base.e.set.ExportData(base.base(), base.e.rel(base.cur), "database"), "database")
	rel := dev.e.rel(dev.cur)
	if err := dev.e.set.CheckLanding(rel, dev.e.set.ImportLanding(data, "database")); err != nil {
		dev.fail("the import's check: %v", err)
	}
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.ImportLanding(data, "database")); err != nil {
		dev.fail("the import: %v", err)
	}
	dev.up()
	if got := dev.runs("database"); got != "M1" {
		t.Errorf("11.8 data runs %s, the parent's version 13 build under the name mariadb:11.8, not M1:\n%s", got, out.String())
	}
}

// Bug: the same entry read back: derived again, source finds it in the release
// itself (its Ref the release's own), so holdOn takes it for a hold under the
// release's own name and moves mariadb:11.8 onto the parent's build, the retag
// of a pulled name its comment rules out. A --no-code restore of a snapshot of
// the held environment, the restart after another failure before its database
// ran and a full restore all derive it again.
func TestBookmarkHoldDerivedAgainRetagsPulledName(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dev, d, _, parents := heldByBookmark(t, &out)
	dev.e.set.Snapshots = filepath.Join(dev.base(), "snapshots")
	snap := dev.snapshot("20261017000001")
	if err := dev.noCode(snap); err != nil {
		dev.fail("--no-code: %v", err)
	}
	if got := d.names[familiar("mariadb:11.8")]; got != "M1" {
		t.Errorf("deriving the hold again retagged the pulled mariadb:11.8 to %s (the parent's build is %s), not M1:\n%s", got, parents, out.String())
	}
}

// Bug: a copy's check minds only the built images its landing will need
// (copyNeeds), never a pulled one. A --no-code restore of a snapshot whose
// data's window ends on a pulled image passes the check while that image is
// gone from the server and the registry no longer serves it (an imported
// snapshot, whose images are bookmarked only where the server has them),
// then its data step lands the data and derive fails pulling the hold: the
// services stay down, marked, for a condition the check could have refused
// before anything stopped ("Check everything before changing anything").
// Here: the environment held on J2 (12.1) under an 11.8 release, the
// snapshot's 12.1 data having last run J1, which is gone everywhere.
func TestNoCodeCheckRefusesHeldPullBeforeStop(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	d.publish("mariadb:11.8", "I1")
	if _, err := m.release(false, db118); err != nil {
		m.fail("11.8: %v", err)
	}
	d.publish("mariadb:12.1", "J1")
	if _, err := m.failRun(false, dbSvc("mariadb:12.1")); err != nil {
		m.fail("failed 12.1: %v", err)
	}
	if heldIn(m.e.rel(m.cur))["database"] == "" {
		m.fail("setup: not held on J1")
	}
	snap := m.snapshot("20261017000001")
	d.publish("mariadb:12.1", "J2")
	if _, err := m.release(false, dbSvc("mariadb:12.1")); err != nil {
		m.fail("fix forward: %v", err)
	}
	if _, err := m.release(true, db118); err != nil {
		m.fail("accepted 11.8: %v", err)
	}
	if _, err := m.failRun(false, dbSvc("mariadb:12.1")); err != nil {
		m.fail("failed 12.1 again: %v", err)
	}
	if heldIn(m.e.rel(m.cur))["database"] == "" || m.runs("database") != "J2" {
		m.fail("setup: held %v, runs %s", heldIn(m.e.rel(m.cur)), m.runs("database"))
	}
	// J1, the image the snapshot's data ran last, goes from the server and
	// from the registry (as for a snapshot imported from another server).
	for n, id := range d.names {
		if id == "J1" {
			delete(d.names, n)
		}
	}
	delete(d.images, "J1")
	delete(d.registry, "J1")
	rel := m.e.rel(m.cur)
	raw, err := os.ReadFile(filepath.Join(snap, "data", "images"))
	if err != nil {
		t.Fatal(err)
	}
	data := string(raw)
	out.Reset()
	err = m.e.set.CheckLanding(rel, m.e.set.RestoreLanding(data, filepath.Base(snap), nil))
	if !errors.Is(err, ErrRefused) {
		land := m.e.set.Land(m.base(), rel, m.e.set.RestoreLanding(data, "", nil))
		t.Fatalf("the check let the --no-code restore through (%v), and its data step then failed (%v): the data is in, the services stay down, marked, for an image the check could have found gone before anything stopped\n%s", err, land, out.String())
	}
}

// Bug: the fix for a rollback under a shared name stops at the rollback. A
// landing's derive leaves a data service it does not hold on its name, which
// another environment's newer build holds, so the run step after a sync runs
// that build on the copy: stage's 12.1 on prod's 11.8 data, its label judged,
// never that image, and img ran writes it down as 11.8. Its bookmark holds the
// right one, as in Restore and unhold. An import and a --no-code restore go
// through the same derive.
func TestLandingSharedNameRunsOthersBuildOnData(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	d := newFake()
	d.publish("mariadb:11.8", "M1")
	db := func(version string) Service {
		return Service{Name: "database", Ref: "shop-db:latest", Build: true, Data: true, Method: CopyVolumes, DataVersion: version}
	}
	base := newSim(t, d, "app-base", &out)
	if _, err := base.release(false, dbAt("mariadb:11.8", "11.8")); err != nil {
		base.fail("base: %v", err)
	}
	prod := newSim(t, d, "app-prod", &out)
	if _, err := prod.release(false, db("11.8")); err != nil {
		prod.fail("prod: %v", err)
	}
	own := prod.runs("database")
	stage := newSim(t, d, "app-stage", &out)
	stage.n = 100
	if _, err := stage.release(false, db("12.1")); err != nil {
		stage.fail("stage: %v", err)
	}
	newer := stage.runs("database")
	out.Reset()
	if err := syncInto(t, base, prod, false); err != nil {
		prod.fail("sync: %v", err)
	}
	if got := prod.runs("database"); got != own {
		t.Errorf("prod's run step after a sync runs %s, stage's 12.1 build %s, on 11.8 data, not its own %s: history %v\n%s", got, newer, own, prod.history("database"), out.String())
	}
}

// A pulled release held under the failed release's own build name counts on
// that name: a later fetch that fails moved it, and its undo puts it back (a
// pulled name of its own it never retags). Else its next up runs what the
// failed fetch built on the data. Found by TestDataRandom's seed 5283.
func TestDataUndoRestoresTheNameAHoldIsOn(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11", "I1")
	r1, _ := m.release(false, dbSvc("mariadb:11"))
	m.failRun(false, builtDB("app-prod", "11.8"))
	held := m.runs("database")
	if imgs, _, marks := composeFacts(m.e.rel(r1)); imgs["database"] != "app-prod-database" || marks["database"] == "" {
		m.fail("setup: the hold is %v, %v", imgs, marks)
	}
	r3 := m.e.release(builtDB("app-prod", "11.8"))
	m.fetch(m.e.svcs[r3])
	if got, _ := d.ID("app-prod-database"); got == held {
		m.fail("setup: the fetch built nothing new")
	}
	m.e.set.Undo(m.e.rel(r3), m.e.rel(r1), false)
	if got, _ := d.ID("app-prod-database"); got != held {
		t.Errorf("the name the hold is on is %s after the undo, not %s", got, held)
	}
}

// The restart's last resort holds a built image too, with no lock and no
// Docker: by the failed release's bookmark as image: in its compose file, a
// name nothing moves. It only said so, and the release before's older image
// ran on the data the failed one's had upgraded.
func TestDataLastResortHoldsABuiltImage(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	r1, _ := m.release(false, builtDB("app-prod", "11.8"))
	r2 := m.e.release(builtDB("app-prod", "12.1"))
	m.fetch(m.e.svcs[r2])
	if err := m.e.set.Record(m.e.rel(r2), m.e.rel(r1)); err != nil {
		m.fail("record: %v", err)
	}
	m.cur = r2
	m.up()
	if err := m.e.set.Ran(m.base(), m.e.rel(r2)); err != nil {
		m.fail("ran: %v", err)
	}
	built := m.runs("database")
	os.WriteFile(filepath.Join(m.e.rel(r2), failedFile), nil, 0o644)
	if err := m.e.set.LastHold(m.e.rel(r1), m.e.rel(r2)); err != nil {
		m.fail("the last resort: %v\n%s", err, out.String())
	}
	m.cur = r1
	m.up()
	if got := m.runs("database"); got != built {
		t.Errorf("%s runs %s, not %s, the image the data ran:\n%s", r1, got, built, out.String())
	}
	if labelsIn(m.e.rel(r1))["database"] != built || heldIn(m.e.rel(r1))["database"] != r2 {
		t.Errorf("its compose file holds nothing of it: %v, %v", labelsIn(m.e.rel(r1)), heldIn(m.e.rel(r1)))
	}
	// With no release listing it, no bookmark to name: said, refused.
	stage := newSim(t, d, "app-stage", &out)
	s1, _ := stage.release(false, builtDB("app-stage", "11.8"))
	s2 := stage.e.release(builtDB("app-stage", "12.1"))
	stage.fetch(stage.e.svcs[s2])
	if err := stage.e.set.Record(stage.e.rel(s2), stage.e.rel(s1)); err != nil {
		stage.fail("record: %v", err)
	}
	stage.cur = s2
	stage.up()
	stage.e.set.Ran(stage.base(), stage.e.rel(s2))
	os.Remove(filepath.Join(stage.e.rel(s2), ListFile))
	out.Reset()
	if err := stage.e.set.LastHold(stage.e.rel(s1), stage.e.rel(s2)); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "has no bookmark here") || !strings.Contains(out.String(), "--release") {
		stage.fail("the last resort with no bookmark (%v):\n%s", err, out.String())
	}
}
