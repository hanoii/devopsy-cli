package images

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// sim is one environment as its activations drive it: releases fetch
// what the registry serves (fakeDocker.upstream), record, go live (up:
// each service on its hold, else on what its name holds) and append
// what ran; rollbacks and restarts restore.
type sim struct {
	t   *testing.T
	d   *fakeDocker
	e   *recEnv
	out *bytes.Buffer
	cur string
	n   int
	// activation: the next up is an activation's, which labels anew.
	activation bool
}

func newSim(t *testing.T, d *fakeDocker, name string, out *bytes.Buffer) *sim {
	m := &sim{t: t, d: d, e: newRecEnv(t, d, name, ModeBuild, out), out: out}
	m.e.set.Address = "vm1:" + strings.TrimPrefix(name, "app-")
	return m
}

func (m *sim) base() string { return filepath.Dir(m.e.dir) }

func (m *sim) prev() string {
	if m.cur == "" {
		return ""
	}
	return m.e.rel(m.cur)
}

// fetch is a release's fetch: own images built anew, pulled ones pulled
// as the registry has them now.
func (m *sim) fetch(svcs []Service) {
	for _, svc := range svcs {
		if svc.own(m.e.set.Mode) {
			m.n++
			m.d.pull(svc.Ref, m.e.set.Name+"-build-"+string(rune('a'+m.n)))
			continue
		}
		if err := m.d.Pull(svc.Ref); err != nil {
			m.t.Fatalf("fetch %s: %v", svc.Ref, err)
		}
	}
}

// release fetches, records and, unless refused, goes live and runs.
func (m *sim) release(yes bool, svcs ...Service) (string, error) {
	m.t.Helper()
	id := m.e.release(svcs...)
	rel := m.e.rel(id)
	if err := m.e.set.SaveNames(rel); err != nil {
		m.t.Fatal(err)
	}
	m.fetch(svcs)
	m.e.set.Yes = yes
	defer func() { m.e.set.Yes = false }()
	if err := m.e.set.Record(rel, m.prev()); err != nil {
		m.e.set.Undo(rel, m.prev(), false)
		os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
		return id, err
	}
	m.cur = id
	m.activation = true
	m.up()
	if err := m.e.set.Ran(m.base(), rel); err != nil {
		m.t.Fatal(err)
	}
	return id, nil
}

// up is the run step: each service on its hold, else on its name.
func (m *sim) up() {
	var cs []container
	for _, c := range m.d.containers {
		if c.project != m.e.set.Name {
			cs = append(cs, c)
		}
	}
	holds := holdsIn(m.e.rel(m.cur))
	given := map[string]string{}
	for _, c := range m.d.containers {
		if c.project == m.e.set.Name && !m.activation {
			given[c.service] = c.digest
		}
	}
	m.activation = false
	for _, svc := range m.e.svcs[m.cur] {
		ref := svc.Ref
		if h := holds[svc.Name]; h != "" {
			ref = h
		}
		img, err := m.d.Inspect(ref)
		if errors.Is(err, ErrNoImage) {
			// Compose pulls an image that is gone, which another project's
			// removal may have taken.
			if m.d.Pull(ref) == nil {
				img, err = m.d.Inspect(ref)
				m.d.outside = append(m.d.outside, familiar(ref)+" "+img.ID)
			}
		}
		if err != nil {
			m.fail("up %s: %v (%v)", ref, err, m.d.names)
		}
		c := container{image: img.ID, project: m.e.set.Name, service: svc.Name}
		// Its label: what the activation gave it, which a plain up of an
		// unchanged service keeps.
		c.digest = h(holds[svc.Name], given[svc.Name])
		if c.digest == "" {
			if e, ok := match(readList(m.e.rel(m.cur)), entry{Service: svc.Name}); ok {
				c.digest = h(e.Digest, e.ID)
			}
		}
		cs = append(cs, c)
	}
	m.d.containers = cs
}

// h is the first of its arguments that is set.
func h(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// rollback restores target in place of current, then runs it.
func (m *sim) rollback(target string) error {
	err := m.e.set.Restore(m.e.rel(target), m.prev(), false)
	if errors.Is(err, ErrGone) {
		m.fetch(m.e.svcs[target])
		err = m.e.set.Restore(m.e.rel(target), m.prev(), true)
	}
	if err != nil {
		return err
	}
	m.cur = target
	m.activation = true
	m.up()
	return m.e.set.Ran(m.base(), m.e.rel(target))
}

// runs is what a service's container runs.
func (m *sim) runs(svc string) string {
	for _, c := range m.d.containers {
		if c.project == m.e.set.Name && c.service == svc {
			return c.image
		}
	}
	return ""
}

func (m *sim) history(svc string) []string {
	var ids []string
	for _, r := range readHistory(m.base()) {
		if r.Service == svc {
			ids = append(ids, r.How+":"+r.ID)
		}
	}
	return ids
}

func (m *sim) fail(format string, args ...any) {
	m.t.Helper()
	m.t.Fatalf(format+"\n%s", append(args, m.out.String())...)
}

var db118 = dbSvc("mariadb:11.8")

// S2, S24, S28: a tag moving forward is pulled, held and recorded as it
// runs; rollbacks never take the database back, while the app goes back
// exactly; a rollback after rollbacks still keeps the newest.
func TestDataGoesForward(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	r1, err := m.release(false, appSvc, db118)
	if err != nil || m.runs("database") != "I1" {
		m.fail("r1 (%v): %s", err, m.runs("database"))
	}
	app1 := m.runs("app")
	d.publish("mariadb:11.8", "I2")
	r2, err := m.release(false, appSvc, db118)
	if err != nil || m.runs("database") != "I2" || holdsIn(m.e.rel(r2))["database"] != "mariadb@I2" {
		m.fail("r2 (%v): %s %v", err, m.runs("database"), holdsIn(m.e.rel(r2)))
	}
	if got := strings.Join(m.history("database"), " "); got != "ran:I1 ran:I2" {
		m.fail("history: %s", got)
	}
	d.publish("mariadb:11.8", "I3")
	r3, _ := m.release(false, appSvc, db118)
	out.Reset()
	if err := m.rollback(r2); err != nil || m.runs("database") != "I3" {
		m.fail("rollback to r2 (%v): %s", err, m.runs("database"))
	}
	if err := m.rollback(r1); err != nil || m.runs("database") != "I3" || m.runs("app") != app1 {
		m.fail("rollback to r1 (%v): database %s, app %s, want I3 and %s", err, m.runs("database"), m.runs("app"), app1)
	}
	if !strings.Contains(out.String(), "database stays on mariadb@I3, the newest image its data ran, not "+r1+"'s (mariadb@I1)") {
		m.fail("not said")
	}
	// The name was never moved back: another environment's next up
	// would follow it.
	if d.names["mariadb:11.8"] != "I3" {
		m.fail("mariadb:11.8 is %s", d.names["mariadb:11.8"])
	}
	// R1's own record now says what it runs.
	if es := readList(m.e.rel(r1)); es[1].ID != "I3" || holdsIn(m.e.rel(r1))["database"] != "mariadb@I3" {
		m.fail("r1's listing %+v", es)
	}
	_ = r3
	// A release after the rollbacks runs what it fetched, forward.
	d.publish("mariadb:11.8", "I4")
	if _, err := m.release(false, appSvc, db118); err != nil || m.runs("database") != "I4" {
		m.fail("r4 (%v): %s", err, m.runs("database"))
	}
}

// S3, S4, S5, S7, S8: what the history cannot order, or knows is older,
// is refused before anything changes, the names the fetch moved put
// back; --yes lets it through and starts the history over.
func TestDataRefusals(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8.3", "I1")
	if _, err := m.release(false, appSvc, dbSvc("mariadb:11.8.3")); err != nil {
		m.fail("p1: %v", err)
	}
	// S4: a pin bumped: the history cannot order it.
	d.publish("mariadb:11.8.9", "I2")
	out.Reset()
	if _, err := m.release(false, appSvc, dbSvc("mariadb:11.8.9")); !errors.Is(err, ErrRefused) || m.runs("database") != "I1" {
		m.fail("pin bump not refused (%v)", err)
	}
	if !strings.Contains(out.String(), "database: mariadb:11.8.9 (mariadb@I2) never ran on its data, which last ran mariadb@I1 as mariadb:11.8.3") || !strings.Contains(out.String(), "--release --yes") {
		m.fail("refusal")
	}
	// The name it pulled went: it was not there before.
	if _, ok := d.names["mariadb:11.8.9"]; ok {
		m.fail("mariadb:11.8.9 stays after the refusal")
	}
	out.Reset()
	if _, err := m.release(true, appSvc, dbSvc("mariadb:11.8.9")); err != nil || m.runs("database") != "I2" {
		m.fail("--yes (%v)", err)
	}
	if got := strings.Join(m.history("database"), " "); got != "ran:I1 forced:I2" {
		m.fail("history after --yes: %s", got)
	}
	// S5: the pin lowered to what ran before: moved past, whatever the
	// name.
	out.Reset()
	if _, err := m.release(false, appSvc, dbSvc("mariadb:11.8.3")); !errors.Is(err, ErrRefused) {
		m.fail("lowered pin: %v", err)
	}
	// S7: --yes started the history over at I2: I1 is no longer known as
	// moved past, but a name its data never ran, refused as such.
	if !strings.Contains(out.String(), "never ran on its data") {
		m.fail("after the reset")
	}
	// S8: a tag pushed again backward, under the same name.
	d.publish("mariadb:11.8.9", "I3")
	m.release(false, appSvc, dbSvc("mariadb:11.8.9"))
	d.publish("mariadb:11.8.9", "I2")
	out.Reset()
	if _, err := m.release(false, appSvc, dbSvc("mariadb:11.8.9")); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "an image its data moved past: it last ran mariadb@I3") {
		m.fail("backward tag (%v)", err)
	}
	if d.names["mariadb:11.8.9"] != "I3" || m.runs("database") != "I3" {
		m.fail("names not put back: %s runs %s", d.names["mariadb:11.8.9"], m.runs("database"))
	}
	// A tag moving forward under the same name needs nothing.
	d.publish("mariadb:11.8.9", "I4")
	if _, err := m.release(false, appSvc, dbSvc("mariadb:11.8.9")); err != nil || m.runs("database") != "I4" {
		m.fail("forward (%v)", err)
	}
}

// S16, S25, S27: across a changed name or repository, the data keeps the
// history's last; a pulled service whose name changed goes back
// exactly, held by digest when its name moved on (S30), pulled by its
// digest when gone (S32).
func TestDataRollbackAcrossNames(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	caddy1 := Service{Name: "alwaysup", Ref: "caddy:2.10"}
	caddy2 := Service{Name: "alwaysup", Ref: "caddy:2.11"}
	d.publish("mariadb:11", "I0")
	d.publish("caddy:2.10", "C1")
	r1, _ := m.release(false, dbSvc("mariadb:11"), caddy1)
	d.publish("mirror.example/mariadb:11.8", "I2")
	d.publish("caddy:2.11", "C2")
	if _, err := m.release(true, dbSvc("mirror.example/mariadb:11.8"), caddy2); err != nil {
		m.fail("r2: %v", err)
	}
	// caddy:2.10 pushed again meanwhile: its name points elsewhere now.
	d.publish("caddy:2.10", "C1b")
	d.Pull("caddy:2.10")
	out.Reset()
	if err := m.rollback(r1); err != nil || m.runs("database") != "I2" || m.runs("alwaysup") != "C1" {
		m.fail("rollback (%v): database %s, alwaysup %s", err, m.runs("database"), m.runs("alwaysup"))
	}
	if h := holdsIn(m.e.rel(r1)); h["database"] != "mirror.example/mariadb@I2" || h["alwaysup"] != "caddy@C1" {
		m.fail("holds %v", h)
	}
	// S32: the held caddy gone from the server: pulled by its digest.
	r3, _ := m.release(false, dbSvc("mirror.example/mariadb:11.8"), caddy2)
	_ = r3
	for n, i := range d.names {
		if i == "C1" {
			delete(d.names, n)
		}
	}
	delete(d.images, "C1")
	if err := m.rollback(r1); err != nil || m.runs("alwaysup") != "C1" || !strings.Contains(out.String(), "pulled caddy@C1 again by its digest") {
		m.fail("gone hold (%v): %s", err, m.runs("alwaysup"))
	}
}

// S15, S17: a run step that failed after the database started on the new
// image: what ran is recorded before the restart, which keeps it; one
// that failed before keeps the old one.
func TestDataRestartKeepsWhatRan(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	r1, _ := m.release(false, appSvc, db118)
	app1 := m.runs("app")
	d.publish("mariadb:11.8", "I2")
	// r2 records, its up starts the database, its run step fails.
	r2 := m.e.release(appSvc, db118)
	m.e.set.SaveNames(m.e.rel(r2))
	m.fetch(m.e.svcs[r2])
	if err := m.e.set.Record(m.e.rel(r2), m.e.rel(r1)); err != nil {
		m.fail("%v", err)
	}
	m.cur = r2
	m.up()
	m.e.set.Ran(m.base(), m.e.rel(r2))
	// The restart: r1 back, in place of r2.
	if err := m.e.set.Restore(m.e.rel(r1), m.e.rel(r2), false); err != nil {
		m.fail("%v", err)
	}
	m.cur = r1
	m.up()
	if m.runs("database") != "I2" || m.runs("app") != app1 {
		m.fail("restart: database %s, app %s", m.runs("database"), m.runs("app"))
	}
	// A release that failed before the database started: it stays.
	d.publish("mariadb:11.8", "I3")
	r3 := m.e.release(appSvc, db118)
	m.fetch(m.e.svcs[r3])
	m.e.set.Record(m.e.rel(r3), m.e.rel(r1))
	m.e.set.Ran(m.base(), m.e.rel(r3))
	if err := m.e.set.Restore(m.e.rel(r1), m.e.rel(r3), false); err != nil {
		m.fail("%v", err)
	}
	m.up()
	if m.runs("database") != "I2" {
		m.fail("restart before the database started: %s", m.runs("database"))
	}
}

// S13, S37: a failed fetch, or one cut off, puts the live release's own
// names back and never a pulled one; the live database stays held.
func TestDataUndoKeepsPulledNames(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	r1, _ := m.release(false, appSvc, db118)
	app1 := d.names["app-prod-app:latest"]
	d.publish("mariadb:11.8", "I2")
	r2 := m.e.release(appSvc, db118)
	m.fetch(m.e.svcs[r2])
	m.e.set.Undo(m.e.rel(r2), m.e.rel(r1), false)
	if d.names["app-prod-app:latest"] != app1 || d.names["mariadb:11.8"] != "I2" {
		m.fail("undo: app %s (want %s), mariadb:11.8 %s", d.names["app-prod-app:latest"], app1, d.names["mariadb:11.8"])
	}
	m.up()
	if m.runs("database") != "I1" {
		m.fail("the live database moved: %s", m.runs("database"))
	}
}

// S39 to S42: another environment's release, rollback or plain up never
// moves this one's database, which only its own activations change.
func TestDataOtherEnvironments(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	prod, stage := newSim(t, d, "app-prod", &out), newSim(t, d, "app-stage", &out)
	d.publish("mariadb:11.8", "I1")
	p1, _ := prod.release(false, db118)
	stage.release(false, db118)
	d.publish("mariadb:11.8", "I2")
	prod.release(false, db118)
	stage.up() // a plain up: held
	if stage.runs("database") != "I1" || prod.runs("database") != "I2" {
		t.Fatalf("stage %s, prod %s\n%s", stage.runs("database"), prod.runs("database"), out.String())
	}
	prod.rollback(p1)
	if d.names["mariadb:11.8"] != "I2" || stage.runs("database") != "I1" {
		t.Fatalf("prod's rollback moved a name: %s\n%s", d.names["mariadb:11.8"], out.String())
	}
	stage.release(false, db118)
	if stage.runs("database") != "I2" {
		t.Fatalf("stage's own release: %s", stage.runs("database"))
	}
}

// S88: a container moved back outside devopsy is said, never recorded.
func TestDataWentBackOutside(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	m.release(false, db118)
	d.publish("mariadb:11.8", "I2")
	r2, _ := m.release(false, db118)
	for i, c := range d.containers {
		if c.service == "database" {
			d.containers[i].image = "I1"
		}
	}
	out.Reset()
	m.e.set.Ran(m.base(), m.e.rel(r2))
	if got := strings.Join(m.history("database"), " "); got != "ran:I1 ran:I2" || !strings.Contains(out.String(), "went back outside devopsy") {
		m.fail("history %s", got)
	}
}

// A data service with a history from an older devopsy's release (no
// service column) and none of its own: what its container runs seeds
// it, and a changed name is refused (S33, a service gaining its label).
func TestDataSeed(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.4", "I0")
	d.Pull("mariadb:11.4")
	id := m.e.release(dbSvc("mariadb:11.4"))
	os.WriteFile(filepath.Join(m.e.rel(id), ListFile), []byte("mariadb:11.4\tI0\n"), 0o644)
	m.cur = id
	d.containers = []container{{image: "I0", project: "app-prod", service: "database", ref: "mariadb:11.4"}}
	d.publish("mariadb:11.8", "I1")
	out.Reset()
	if _, err := m.release(false, db118); !errors.Is(err, ErrRefused) {
		m.fail("a changed name over a seeded history: %v", err)
	}
	if got := strings.Join(m.history("database"), " "); got != "ran:I0" {
		m.fail("seed: %s", got)
	}
}

// --destroy-data forgets the history with the data.
func TestRemoveHistory(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	m.release(false, db118)
	if err := RemoveHistory(m.base()); err != nil || len(readHistory(m.base())) != 0 {
		t.Fatalf("history stays (%v)", err)
	}
	if err := RemoveHistory(m.base()); err != nil {
		t.Fatalf("twice: %v", err)
	}
}

// Two environments sharing mariadb tags release (the same name, another
// one, a pin, now and then with --yes), fail in their run step after the
// database started or in their fetch, roll back, run plain ups and lose
// releases to pruning, while the registry moves the tags forward and
// sometimes back. After every step: each database runs the newest image
// its data ran (rules 1 and 2), a rollback moved no pulled name (rule 4),
// every kept release's bookmarks hold what it lists; once all goes,
// nothing is left (rule 5).
func TestDataRandom(t *testing.T) {
	for seed := int64(1); seed <= int64(seeds(300)); seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { dataRandom(t, seed) })
	}
}

func dataRandom(t *testing.T, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	d := newFake()
	var out bytes.Buffer
	envs := []*sim{newSim(t, d, "app-prod", &out), newSim(t, d, "app-stage", &out)}
	left := filepath.Join(t.TempDir(), LeftFile)
	for _, m := range envs {
		m.e.set.Left = left
	}
	refs := []string{"mariadb:11.8", "mariadb:11", "mariadb:11.8.9", "mirror.example/mariadb:11.8"}
	img := 0
	var pushed []string
	push := func(ref string) {
		img++
		id := fmt.Sprintf("I%03d", img)
		if len(pushed) > 0 && rng.Intn(5) == 0 {
			id = pushed[rng.Intn(len(pushed))] // pushed again: maybe back
		}
		d.publish(ref, id)
		pushed = append(pushed, id)
	}
	for _, r := range refs {
		push(r)
	}
	var log []string
	check := func(step string) {
		t.Helper()
		log = append(log, step)
		for _, m := range envs {
			if m.cur == "" {
				continue
			}
			if got := m.runs("database"); got != "" {
				if last, ok := readHistory(m.base()).last("database"); ok && got != last.ID {
					t.Fatalf("after %v: %s's database runs %s, its data's newest is %s\n%s", log, m.e.set.Name, got, last.ID, out.String())
				}
			}
			for _, id := range m.e.releases() {
				es := readList(m.e.rel(id))
				for i, b := range bookmarks(es, m.e.set.Name, id) {
					if es[i].ID == "" {
						continue
					}
					if got, _ := d.ID(b); got != es[i].ID {
						t.Fatalf("after %v: %s's bookmark %s is %q, want %q\n%s", log, id, b, got, es[i].ID, out.String())
					}
				}
			}
		}
	}
	// Each environment builds its own app, named after its compose project.
	var m *sim
	svcs := func() []Service {
		return []Service{{Name: "app", Ref: m.e.set.Name + "-app", Build: true}, dbSvc(refs[rng.Intn(len(refs))]), proxySvc}
	}
	d.publish("caddy:2-alpine", "C1")
	for step := 0; step < 40; step++ {
		m = envs[rng.Intn(2)]
		switch op := rng.Intn(10); op {
		case 8, 9: // a sync of the other environment's data into this one
			other := envs[0]
			if other == m {
				other = envs[1]
			}
			if m.cur == "" || other.cur == "" {
				continue
			}
			if op == 9 { // aborted after its check: nothing changes but a pull
				data := other.e.set.ParentData(other.base(), other.e.rel(other.cur))
				before := m.runs("database")
				m.e.set.SyncCheck(m.e.rel(m.cur), data)
				m.up()
				if m.runs("database") != before {
					t.Fatalf("after %v: an aborted sync moved %s's database", log, m.e.set.Name)
				}
				check(m.e.set.Name + " sync aborted")
				continue
			}
			err := syncInto(t, other, m, rng.Intn(3) == 0)
			if err != nil && !errors.Is(err, ErrRefused) {
				t.Fatalf("after %v: sync: %v\n%s", log, err, out.String())
			}
			if err == nil && m.runs("database") != other.runs("database") {
				t.Fatalf("after %v: %s's database runs %s after a sync from %s, which runs %s", log, m.e.set.Name, m.runs("database"), other.e.set.Name, other.runs("database"))
			}
			check(m.e.set.Name + " sync")
		case 0: // the registry moves a tag
			push(refs[rng.Intn(len(refs))])
			if rng.Intn(2) == 0 {
				d.publish("caddy:2-alpine", fmt.Sprintf("C%d", step))
			}
		case 1, 2: // a release
			id, err := m.release(rng.Intn(4) == 0, svcs()...)
			if err != nil && !errors.Is(err, ErrRefused) {
				t.Fatalf("after %v: release: %v\n%s", log, err, out.String())
			}
			check(m.e.set.Name + " release " + id)
		case 3: // a run step failing after its database started, restarted
			if m.cur == "" {
				continue
			}
			prev := m.cur
			id := m.e.release(svcs()...)
			m.e.set.SaveNames(m.e.rel(id))
			m.fetch(m.e.svcs[id])
			m.e.set.Yes = rng.Intn(4) == 0
			err := m.e.set.Record(m.e.rel(id), m.e.rel(prev))
			m.e.set.Yes = false
			if err != nil {
				m.e.set.Undo(m.e.rel(id), m.e.rel(prev), false)
				os.WriteFile(filepath.Join(m.e.rel(id), failedFile), nil, 0o644)
				check(m.e.set.Name + " refused " + id)
				continue
			}
			m.cur = id
			m.up()
			m.e.set.Ran(m.base(), m.e.rel(id))
			os.WriteFile(filepath.Join(m.e.rel(id), failedFile), nil, 0o644)
			if err := m.e.set.Restore(m.e.rel(prev), m.e.rel(id), false); err != nil && !errors.Is(err, ErrGone) {
				t.Fatalf("after %v: restart: %v\n%s", log, err, out.String())
			} else if err != nil {
				m.fetch(m.e.svcs[prev])
				m.e.set.Restore(m.e.rel(prev), m.e.rel(id), true)
			}
			m.cur = prev
			m.up()
			check(m.e.set.Name + " failed run " + id)
		case 4: // a fetch failing: undone
			id := m.e.release(svcs()...)
			m.fetch(m.e.svcs[id])
			m.e.set.Undo(m.e.rel(id), m.prev(), false)
			os.WriteFile(filepath.Join(m.e.rel(id), failedFile), nil, 0o644)
			m.up()
			check(m.e.set.Name + " failed fetch " + id)
		case 5: // a rollback
			var target string
			for _, id := range m.e.releases() {
				if id < m.cur && !exists(filepath.Join(m.e.rel(id), failedFile)) {
					target = id
				}
			}
			if target == "" {
				continue
			}
			before := map[string]string{}
			for n, i := range d.names {
				if !isBookmark(n) && !strings.HasSuffix(n, "-app:latest") {
					before[n] = i
				}
			}
			if err := m.rollback(target); err != nil {
				t.Fatalf("after %v: rollback to %s: %v\n%s", log, target, err, out.String())
			}
			for n, i := range before {
				if got, ok := d.names[n]; ok && got != i && !strings.Contains(n, "@") {
					t.Fatalf("after %v: the rollback to %s moved %s from %s to %s\n%s", log, target, n, i, got, out.String())
				}
			}
			check(m.e.set.Name + " rollback " + target)
		case 6: // a plain up
			if m.cur != "" {
				m.up()
				check(m.e.set.Name + " up")
			}
		case 7: // pruning a release
			for _, id := range m.e.releases() {
				if id != m.cur {
					if err := m.e.set.Remove(m.e.rel(id)); err != nil {
						t.Fatal(err)
					}
					os.RemoveAll(m.e.rel(id))
					m.e.set.Prune()
					check(m.e.set.Name + " remove " + id)
					break
				}
			}
		}
	}
	d.containers = nil
	for _, m := range envs {
		for _, id := range m.e.releases() {
			m.e.set.Remove(m.e.rel(id))
			os.RemoveAll(m.e.rel(id))
		}
		m.e.set.Prune()
	}
	// What a plain up pulled outside devopsy is no release's.
	for _, o := range d.outside {
		n, id, _ := strings.Cut(o, " ")
		if d.names[n] == id {
			d.Remove(n)
		}
	}
	d.Prune()
	if len(d.images) > 0 {
		var ls []string
		for id := range d.images {
			var ns []string
			for n, i := range d.names {
				if i == id {
					ns = append(ns, n)
				}
			}
			ls = append(ls, id+" "+strings.Join(ns, ","))
		}
		sort.Strings(ls)
		t.Fatalf("after %v and removing everything, images are left: %v\n%s", log, ls, out.String())
	}
}

// seeds is n, or DEVOPSY_TEST_SEEDS for a longer run.
func seeds(n int) int {
	if v, err := strconv.Atoi(os.Getenv("DEVOPSY_TEST_SEEDS")); err == nil && v > 0 {
		return v
	}
	return n
}

// --status names the services held by digest and those a plain up moved
// off the image their activation gave them.
func TestStatusLines(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	prod, stage := newSim(t, d, "app-prod", &out), newSim(t, d, "app-stage", &out)
	d.publish("mariadb:11.8", "I1")
	d.publish("caddy:2-alpine", "C1")
	stage.release(false, db118, proxySvc)
	d.publish("mariadb:11.8", "I2")
	d.publish("caddy:2-alpine", "C2")
	prod.release(false, db118, proxySvc)
	stage.up() // a plain up: caddy follows its name, the database is held
	got := StatusLines(d, "app-stage", stage.e.rel(stage.cur))
	want := []string{"database held on mariadb@I1", "alwaysup runs caddy@C2, not caddy@C1 its activation gave it: caddy:2-alpine moved since (a plain up follows names)"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("status lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// syncInto is a sync of parent's data into child, as the child's side
// runs it: the parent's data images read (ParentData), checked before
// anything stops (SyncCheck), and, the copy complete, taken over
// (SyncDone), then finish recreating what must be (Recreate) and the
// rest starting.
func syncInto(t *testing.T, parent, child *sim, yes bool) error {
	t.Helper()
	data := parent.e.set.ParentData(parent.base(), parent.e.rel(parent.cur))
	child.e.set.Yes = yes
	defer func() { child.e.set.Yes = false }()
	rel := child.e.rel(child.cur)
	if err := child.e.set.SyncCheck(rel, data); err != nil {
		return err
	}
	if err := child.e.set.SyncDone(child.base(), rel, data, "vm1:prod"); err != nil {
		return err
	}
	for _, svc := range Recreate(child.d, child.e.set.Name, rel) {
		_ = svc
	}
	child.activation = true
	child.up()
	return child.e.set.Ran(child.base(), rel)
}

// S50, S51, S58, S65: a child on an older image under the same name is
// held on the parent's exact image once the copy is in, recreated, its
// history the parent's; its next release moves it forward; a chain
// carries the history on.
func TestSyncTakesParentImage(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	prod, dev, pr := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out), newSim(t, d, "app-pr1", &out)
	d.publish("mariadb:11.8", "I1")
	dev.release(false, db118)
	pr.release(false, db118)
	d.publish("mariadb:11.8", "I2")
	prod.release(false, db118)
	d.publish("mariadb:11.8", "I3") // pulled later by another environment
	d.Pull("mariadb:11.8")
	rel := dev.e.rel(dev.cur)
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	if !strings.Contains(data, "image\tdatabase\tmariadb:11.8\tI2\t") {
		t.Fatalf("parent data:\n%s", data)
	}
	if err := dev.e.set.SyncCheck(rel, data); err != nil {
		t.Fatalf("check: %v\n%s", err, out.String())
	}
	// Before the copy completes, nothing changes on the child.
	if dev.runs("database") != "I1" || holdsIn(rel)["database"] != "mariadb@I1" {
		t.Fatalf("the check changed the child")
	}
	if err := dev.e.set.SyncDone(dev.base(), rel, data, "vm1:prod"); err != nil {
		t.Fatal(err)
	}
	if got := Recreate(d, "app-dev", rel); strings.Join(got, " ") != "database" {
		t.Errorf("recreate: %v", got)
	}
	dev.activation = true
	dev.up()
	if dev.runs("database") != "I2" || holdsIn(rel)["database"] != "mariadb@I2" {
		t.Fatalf("after the copy: runs %s, holds %v\n%s", dev.runs("database"), holdsIn(rel), out.String())
	}
	if got := strings.Join(dev.history("database"), " "); got != "sync vm1:prod:I2" {
		t.Errorf("history: %s", got)
	}
	// S65: the chain, dev into pr1.
	if err := syncInto(t, dev, pr, false); err != nil || pr.runs("database") != "I2" {
		t.Fatalf("chain (%v): %s\n%s", err, pr.runs("database"), out.String())
	}
	// S51: its next release moves it forward, no --yes.
	if _, err := dev.release(false, db118); err != nil || dev.runs("database") != "I3" {
		t.Fatalf("next release (%v): %s\n%s", err, dev.runs("database"), out.String())
	}
}

// S53, S54, S57, S69: a child pinned to an image the parent's data moved
// past, or on a name the parent's data never ran, is refused before
// anything stops, every sync, unless --yes; a child's local tag is
// never trusted as newer: held on the parent's digest.
func TestSyncRefusals(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	d.publish("mariadb:11.8.3", "I1")
	prod.release(false, dbSvc("mariadb:11.8.3"))
	dev.release(false, dbSvc("mariadb:11.8.3"))
	d.publish("mariadb:11.8.9", "I2")
	prod.release(true, dbSvc("mariadb:11.8.9"))
	// S53: dev pinned to 11.8.3, which prod's history starts after (--yes
	// started it over): a name prod's data never ran.
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	out.Reset()
	if err := dev.e.set.SyncCheck(dev.e.rel(dev.cur), data); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "never ran on the parent's data") {
		t.Fatalf("pinned child (%v):\n%s", err, out.String())
	}
	dev.e.set.Yes = true
	if err := dev.e.set.SyncCheck(dev.e.rel(dev.cur), data); err != nil {
		t.Fatalf("--yes: %v", err)
	}
	dev.e.set.Yes = false
	// S69: a child on mariadb:11.8.9 pulled long ago, another image than
	// prod's: same name, held on prod's.
	stage := newSim(t, d, "app-stage", &out)
	d.publish("mariadb:11.8.9", "I0")
	stage.release(true, dbSvc("mariadb:11.8.9"))
	if err := syncInto(t, prod, stage, false); err != nil || stage.runs("database") != "I2" {
		t.Fatalf("local tag (%v): %s\n%s", err, stage.runs("database"), out.String())
	}
	// S54 moved past: the parent's history ran I3 after I2; a child on I2
	// under another name is refused.
	d.publish("mariadb:11.8.9", "I3")
	prod.release(false, dbSvc("mariadb:11.8.9"))
	other := newSim(t, d, "app-other", &out)
	d.publish("mirror.example/mariadb:11.8.9", "I2")
	other.release(false, dbSvc("mirror.example/mariadb:11.8.9"))
	out.Reset()
	if err := syncInto(t, prod, other, false); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "moved past") {
		t.Fatalf("moved past (%v):\n%s", err, out.String())
	}
}

// S68, S70, S73: the parent's image missing on the child's server is
// pulled by its digest before anything stops and bookmarked as the
// child's release's at once; one the registry no longer serves is
// refused, nothing changed.
func TestSyncPullsParentImage(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	d.publish("mariadb:11.8", "I1")
	dev.release(false, db118)
	d.publish("mariadb:11.8", "I2")
	prod.release(false, db118)
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	// Another server: the child's has never seen I2.
	for n, i := range d.names {
		if i == "I2" {
			delete(d.names, n)
		}
	}
	delete(d.images, "I2")
	rel := dev.e.rel(dev.cur)
	if err := dev.e.set.SyncCheck(rel, data); err != nil || !d.images["I2"] {
		t.Fatalf("not pulled (%v)\n%s", err, out.String())
	}
	found := false
	for _, b := range bookmarks(readList(rel), "app-dev", dev.cur) {
		found = found || d.names[familiar(b)] == "I2"
	}
	if !found {
		t.Errorf("the pulled image is not bookmarked: %v", d.names)
	}
	// The registry lost it.
	for n, i := range d.names {
		if i == "I2" {
			delete(d.names, n)
		}
	}
	delete(d.images, "I2")
	d.registry = map[string][]string{}
	out.Reset()
	if err := dev.e.set.SyncCheck(rel, data); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "could not be pulled here") {
		t.Fatalf("gone from the registry (%v):\n%s", err, out.String())
	}
}

// S59, S60: a release that syncs checks against what it fetched; once
// synced, a restart after its failed run keeps the parent's image.
func TestSyncReleaseRecord(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	d.publish("mariadb:11.8.9", "I2")
	prod.release(false, dbSvc("mariadb:11.8.9"))
	dev.release(false, dbSvc("mariadb:11.8.9"))
	d.publish("mariadb:11.8.3", "I1")
	dev.e.set.SyncData = prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	if _, err := dev.release(false, dbSvc("mariadb:11.8.3")); !errors.Is(err, ErrRefused) {
		t.Fatalf("a lowered pin syncing: %v\n%s", err, out.String())
	}
	dev.e.set.SyncData = ""
}

// A complete import of a data service's files starts its history over
// from what runs it.
func TestImportedHistory(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	m.release(false, db118)
	d.publish("mariadb:11.8", "I2")
	m.release(false, db118)
	if err := m.e.set.Imported(m.base(), m.e.rel(m.cur), "database"); err != nil {
		t.Fatal(err)
	}
	h := readHistory(m.base())
	if w := h.window("database"); len(w) != 1 || w[0].How != Imported || w[0].ID != "I2" {
		t.Errorf("window after an import: %+v", w)
	}
}

// Bug: a release's ForcedFile stays in its directory after its --yes was
// used, so a later rollback to that release (img ran on the target) appends
// a forced line for an image that only ran, starting the history over and
// forgetting what the data moved past: a tag pushed back afterwards
// (S8) is let through without --yes, the database going back.
func TestDataRollbackToForcedReleaseKeepsHistory(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8.3", "I1")
	if _, err := m.release(false, dbSvc("mariadb:11.8.3")); err != nil {
		m.fail("p1: %v", err)
	}
	// A series switch, let through with --yes: its history starts at I2.
	d.publish("mariadb:11.8", "I2")
	forced, err := m.release(true, db118)
	if err != nil {
		m.fail("--yes: %v", err)
	}
	// The tag moves forward: I3 runs, after I2.
	d.publish("mariadb:11.8", "I3")
	if _, err := m.release(false, db118); err != nil || m.runs("database") != "I3" {
		m.fail("r3 (%v): %s", err, m.runs("database"))
	}
	// A rollback to the forced release keeps I3, which only ran: nothing
	// was let through this time.
	if err := m.rollback(forced); err != nil || m.runs("database") != "I3" {
		m.fail("rollback (%v): %s", err, m.runs("database"))
	}
	if got := strings.Join(m.history("database"), " "); got != "ran:I1 forced:I2 ran:I3" {
		t.Errorf("history after the rollback: %s, want ran:I1 forced:I2 ran:I3", got)
	}
	// The tag pushed back to I2, which the data moved past: refused.
	d.publish("mariadb:11.8", "I2")
	out.Reset()
	if _, err := m.release(false, db118); !errors.Is(err, ErrRefused) || m.runs("database") != "I3" {
		m.fail("a tag pushed back after a rollback to a forced release (%v): the database runs %s, want I3", err, m.runs("database"))
	}
}

// Bug: Ran marks forced whatever a forced release's data service runs,
// not the image --yes let through: a forced release whose run step fails
// before the database is recreated appends a forced line for the old
// image, under the forced release's reference. The next release on the
// name that kept running (a tag moved forward) is refused as a name its
// data never ran, and the history starts over at the old image,
// forgetting what the data moved past (S8).
func TestDataForcedReleaseFailingEarlyKeepsHistory(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I0")
	m.release(false, db118)
	d.publish("mariadb:11.8", "I1")
	r2, err := m.release(false, db118)
	if err != nil || m.runs("database") != "I1" {
		m.fail("r2 (%v): %s", err, m.runs("database"))
	}
	// A series switch let through with --yes, whose run step fails before
	// compose recreates the database: it still runs I1.
	d.publish("mariadb:11.4", "J1")
	r3 := m.e.release(dbSvc("mariadb:11.4"))
	m.e.set.SaveNames(m.e.rel(r3))
	m.fetch(m.e.svcs[r3])
	m.e.set.Yes = true
	err = m.e.set.Record(m.e.rel(r3), m.e.rel(r2))
	m.e.set.Yes = false
	if err != nil {
		m.fail("r3 with --yes: %v", err)
	}
	m.e.set.Ran(m.base(), m.e.rel(r3))
	if got := strings.Join(m.history("database"), " "); got != "ran:I0 ran:I1" {
		t.Errorf("history after a forced release that never ran its image: %s, want ran:I0 ran:I1", got)
	}
	// The restart of r2.
	if err := m.e.set.Restore(m.e.rel(r2), m.e.rel(r3), false); err != nil {
		m.fail("restart: %v", err)
	}
	m.cur = r2
	m.up()
	// mariadb:11.8 moves on to I2, then is pushed back to I0, which the
	// data moved past: refused.
	d.publish("mariadb:11.8", "I2")
	if _, err := m.release(false, db118); err != nil || m.runs("database") != "I2" {
		m.fail("r4 (%v): %s", err, m.runs("database"))
	}
	d.publish("mariadb:11.8", "I0")
	out.Reset()
	if _, err := m.release(false, db118); !errors.Is(err, ErrRefused) || m.runs("database") != "I2" {
		m.fail("a tag pushed back after a forced release failed early (%v): the database runs %s, want I2", err, m.runs("database"))
	}
}

// Bug: a refused release puts every pulled name back on what it held
// when its own fetch began (SaveNames), whatever moved it since: another
// environment's release that pulled the same name meanwhile (fetches
// share the image lock) has it moved back under it, so that
// environment's run step or next plain up runs the older image its
// release never gave it (rule 4: no environment moves another's
// images), a patch undone.
func TestDataRefusalLeavesOthersNames(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	prod, stage := newSim(t, d, "app-prod", &out), newSim(t, d, "app-stage", &out)
	d.publish("mariadb:11.8.3", "I1")
	d.publish("caddy:2-alpine", "C1")
	prod.release(false, dbSvc("mariadb:11.8.3"), proxySvc)
	stage.release(false, dbSvc("mariadb:11.8.3"), proxySvc)
	// caddy patched upstream; prod starts a release bumping its pin,
	// which its data never ran: refused once its fetch ends.
	d.publish("caddy:2-alpine", "C2")
	d.publish("mariadb:11.8.9", "I2")
	r := prod.e.release(dbSvc("mariadb:11.8.9"), proxySvc)
	if err := prod.e.set.SaveNames(prod.e.rel(r)); err != nil {
		t.Fatal(err)
	}
	// Meanwhile stage releases, pulling the patched caddy.
	if _, err := stage.release(false, dbSvc("mariadb:11.8.3"), proxySvc); err != nil || stage.runs("alwaysup") != "C2" {
		t.Fatalf("stage's release (%v): %s\n%s", err, stage.runs("alwaysup"), out.String())
	}
	prod.fetch(prod.e.svcs[r])
	if err := prod.e.set.Record(prod.e.rel(r), prod.prev()); !errors.Is(err, ErrRefused) {
		t.Fatalf("prod's pin bump not refused: %v\n%s", err, out.String())
	}
	prod.e.set.Undo(prod.e.rel(r), prod.prev(), false)
	// stage's next plain up runs what its release gave it.
	stage.up()
	if got := stage.runs("alwaysup"); got != "C2" {
		t.Errorf("stage's alwaysup runs %s after prod's refused release, want C2, which its own release pulled and gave it (caddy:2-alpine is %s)\n%s", got, d.names["caddy:2-alpine"], out.String())
	}
}

// A restart whose images could not be restored still holds the data
// services on the newest image their data ran, never on what the
// release's last activation held.
func TestHoldData(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	r1, _ := m.release(false, appSvc, db118)
	d.publish("mariadb:11.8", "I2")
	m.release(false, appSvc, db118)
	if err := m.e.set.HoldData(m.base(), m.e.rel(r1)); err != nil {
		t.Fatal(err)
	}
	if got := holdsIn(m.e.rel(r1))["database"]; got != "mariadb@I2" {
		t.Errorf("held on %q, want mariadb@I2", got)
	}
}

// A database streamed (devopsy.data=stream): its dump goes through the
// child's own image, which keeps its history; a sync neither checks nor
// takes over its image.
func TestSyncStreamKeepsChildImage(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	stream := Service{Name: "database", Ref: "mariadb:11.8", Data: true, Method: "stream"}
	d.publish("mariadb:11.8", "I1")
	dev.release(false, stream)
	d.publish("mariadb:11.8", "I2")
	prod.release(false, stream)
	if data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur)); data != "" {
		t.Errorf("a stream's image in the parent's data:\n%s", data)
	}
	if err := syncInto(t, prod, dev, false); err != nil || dev.runs("database") != "I1" {
		t.Errorf("after the sync (%v): %s", err, dev.runs("database"))
	}
}

// labeled is a service as compose config gives it: image ref, and a
// devopsy.data label with value data.
func labeled(t *testing.T, name, ref, data string) Service {
	t.Helper()
	svcs, err := ParseComposeConfig([]byte(`{"name":"app","services":{"` + name + `":{"image":"` + ref + `","labels":{"devopsy.data":"` + data + `"}}}}`))
	if err != nil || len(svcs) != 1 {
		t.Fatalf("compose config: %v", err)
	}
	return svcs[0]
}

// Bug: a sync takes over the parent's image and history for every pulled
// service with any devopsy.data label (ParentData, SyncCheck and SyncDone
// read only KindData), stream and none included, when only the data an
// mnt copy writes is the parent's (S55: a stream service keeps its own
// history). A child whose stream database runs a newer image than the
// parent's under the same tag is held on the parent's older one after the
// dump is imported into it, and one on another series is refused.
func TestSyncStreamKeepsOwnImage(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	db := labeled(t, "database", "mariadb:11.8", "stream")
	d.publish("mariadb:11.8", "I1")
	prod.release(false, db)
	// mariadb:11.8 moves on; dev's release takes it: its data runs I2.
	d.publish("mariadb:11.8", "I2")
	if _, err := dev.release(false, db); err != nil || dev.runs("database") != "I2" {
		t.Fatalf("dev's release (%v): %s\n%s", err, dev.runs("database"), out.String())
	}
	// prod's dump imported into dev's running I2 server: dev's data is
	// still what I2 wrote.
	if err := syncInto(t, prod, dev, false); err != nil {
		t.Fatalf("a stream sync (%v):\n%s", err, out.String())
	}
	if got := dev.runs("database"); got != "I2" {
		t.Errorf("dev's stream database runs %s after the sync, want I2, the image its own data ran last (held %v)", got, holdsIn(dev.e.rel(dev.cur)))
	}
	if got := strings.Join(dev.history("database"), " "); got != "ran:I2" {
		t.Errorf("dev's stream database's history: %s, want its own, ran:I2", got)
	}
	// A child on another series, with a stream database: no check.
	stage := newSim(t, d, "app-stage", &out)
	d.publish("mariadb:11.4", "J1")
	stage.release(false, labeled(t, "database", "mariadb:11.4", "stream"))
	out.Reset()
	if err := syncInto(t, prod, stage, false); err != nil {
		t.Errorf("a stream sync into a child on another series refused (%v):\n%s", err, out.String())
	}
}

// Bug: ParentData gives a drifted parent's container image (S52) as the
// newest its data ran, and SyncDone appends it after the parent's
// history's last, so the child's history says the data moved past the
// image the parent's own history keeps as its newest. The parent's next
// release of its unchanged tag goes through, the child's (same tag, same
// image, the same data) is refused as moved past.
func TestSyncDriftedParentHistory(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	d.publish("mariadb:11.8", "I1")
	prod.release(false, db118)
	d.publish("mariadb:11.8", "I2")
	prod.release(false, db118)
	dev.release(false, db118)
	// prod's database moved back to I1 outside devopsy.
	for i, c := range d.containers {
		if c.project == "app-prod" && c.service == "database" {
			d.containers[i].image = "I1"
		}
	}
	if err := syncInto(t, prod, dev, false); err != nil || dev.runs("database") != "I1" {
		t.Fatalf("the sync (%v): dev runs %s\n%s", err, dev.runs("database"), out.String())
	}
	// mariadb:11.8 is still I2, the newest image this data ever ran.
	out.Reset()
	if _, err := dev.release(false, db118); err != nil || dev.runs("database") != "I2" {
		t.Errorf("dev's next release of the same tag (%v): runs %s, want I2, the newest its data ran (history %v)\n%s", err, dev.runs("database"), dev.history("database"), out.String())
	}
	if _, err := prod.release(false, db118); err != nil || prod.runs("database") != "I2" {
		t.Errorf("prod's next release (%v): runs %s", err, prod.runs("database"))
	}
}

// Bug: SyncCheck and SyncDone get and hold the parent's image by its
// digest in the parent's repository, even when the child already runs
// that very image (the same digest) from its own mirror (S74: equal
// digests are equal whatever the references). Docker finds a digest only
// in its repository, so the child's server pulls it from the parent's
// registry, which it may not reach: the sync is refused, nothing wrong.
func TestSyncMirrorSameDigest(t *testing.T) {
	d := newFake()
	d.strictRepo = true
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	d.publish("mariadb:11.8", "I2")
	prod.release(false, db118)
	d.publish("mirror.example/mariadb:11.8", "I2")
	if _, err := dev.release(false, dbSvc("mirror.example/mariadb:11.8")); err != nil {
		t.Fatalf("dev's release: %v\n%s", err, out.String())
	}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	// Another server, which reaches only its mirror: it has I2 under the
	// mirror's name alone.
	for n := range d.names {
		if repoKey(n) == "mariadb" {
			delete(d.names, n)
		}
	}
	d.digests["I2"] = []string{"mirror.example/mariadb@I2"}
	d.registry["I2"] = []string{"mirror.example/mariadb@I2"}
	rel := dev.e.rel(dev.cur)
	out.Reset()
	if err := dev.e.set.SyncCheck(rel, data); err != nil {
		t.Fatalf("the child runs the parent's very image from its mirror, refused (%v):\n%s", err, out.String())
	}
	if err := dev.e.set.SyncDone(dev.base(), rel, data, "vm1:prod"); err != nil {
		t.Fatal(err)
	}
	if hold := holdsIn(rel)["database"]; hold != "" {
		if _, err := d.Inspect(hold); err != nil {
			t.Errorf("dev held on %s, which this server does not have: compose's up would pull it from the parent's registry", hold)
		}
	}
}

// Bug: a release that syncs refused by the parent's check (SyncCheck,
// through Record) leaves the name its fetch pulled moved, where Record's
// own refusal puts it back (putNamesBack): another environment's plain up
// on that name then runs the image this release was refused.
func TestSyncReleaseRefusalPutsNamesBack(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	prod, dev, stage := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out), newSim(t, d, "app-stage", &out)
	d.publish("mariadb:11.4", "J1")
	prod.release(false, dbSvc("mariadb:11.4"))
	dev.release(false, dbSvc("mariadb:11.4"))
	d.publish("mariadb:11.8", "I1")
	stage.release(false, Service{Name: "client", Ref: "mariadb:11.8"})
	// dev bumps its series in a release that syncs: prod's data never ran
	// mariadb:11.8, refused once its fetch moved the name to I2.
	d.publish("mariadb:11.8", "I2")
	dev.e.set.SyncData = prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	_, err := dev.release(false, dbSvc("mariadb:11.8"))
	dev.e.set.SyncData = ""
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("a series bump syncing from a parent that never ran it: %v\n%s", err, out.String())
	}
	stage.up()
	if got := stage.runs("client"); got != "I1" {
		t.Errorf("stage's client runs %s after dev's refused release, want I1 (mariadb:11.8 is %s)\n%s", got, d.names["mariadb:11.8"], out.String())
	}
}

// Bug: SyncDone gives the child the parent's history under the parent's
// references, so a child whose reference differs from the parent's for
// the very same image (S74: its own mirror) finds, at its next release,
// its unchanged reference "never ran on its data": the mirror's tag
// moving forward needs --yes after every sync, where a tag moving under
// an unchanged reference needs nothing (decision 6).
func TestSyncChildKeepsItsReference(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	mirror := dbSvc("mirror.example/mariadb:11.8")
	d.publish("mariadb:11.8", "I2")
	prod.release(false, db118)
	d.publish("mirror.example/mariadb:11.8", "I2")
	dev.release(false, mirror)
	if err := syncInto(t, prod, dev, false); err != nil || dev.runs("database") != "I2" {
		t.Fatalf("the sync (%v): %s\n%s", err, dev.runs("database"), out.String())
	}
	// The mirror's tag moves forward; dev releases, its reference as it
	// always was.
	d.publish("mirror.example/mariadb:11.8", "I3")
	out.Reset()
	if _, err := dev.release(false, mirror); err != nil || dev.runs("database") != "I3" {
		t.Errorf("dev's next release on its unchanged reference (%v): runs %s, want I3\n%s", err, dev.runs("database"), out.String())
	}
}

// A parent's history sent to a child is its newest lines only, whatever
// its length: it rides in one argument.
func TestParentDataBounded(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I0")
	m.release(false, db118)
	var b strings.Builder
	for i := 0; i < 3*ParentHistoryLines; i++ {
		b.WriteString(ran{Time: int64(i), Service: "database", How: Ran, Ref: "mariadb:11.8", ID: fmt.Sprintf("X%d", i), Digests: []string{fmt.Sprintf("mariadb@X%d", i)}}.line())
	}
	b.WriteString(ran{Time: 9999, Service: "database", How: Ran, Ref: "mariadb:11.8", ID: "I0", Digests: []string{"mariadb@I0"}}.line())
	os.WriteFile(historyPath(m.base()), []byte(b.String()), 0o644)
	data := m.e.set.ParentData(m.base(), m.e.rel(m.cur))
	if n := strings.Count(data, "\nran\t"); n != ParentHistoryLines || !strings.Contains(data, "\tI0\t") {
		t.Errorf("%d history lines sent, want %d with the newest", n, ParentHistoryLines)
	}
}

// A child whose release an older devopsy recorded (no service column)
// cannot tell which service runs a database: a sync refuses before
// anything stops, naming the release that fixes it.
func TestSyncOldChildListing(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	d.publish("mariadb:11.8", "I1")
	prod.release(false, db118)
	dev.release(false, db118)
	os.WriteFile(filepath.Join(dev.e.rel(dev.cur), ListFile), []byte("mariadb:11.8\tI1\n"), 0o644)
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	if err := dev.e.set.SyncCheck(dev.e.rel(dev.cur), data); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "devopsy @vm1:dev --release-sync") {
		t.Errorf("an old listing (%v):\n%s", err, out.String())
	}
}

// Bug: a parent whose current release an older devopsy recorded (no
// service column, no kinds) has no data images for ParentData, which
// skips every entry that is not KindData: the sync then checks nothing
// and takes nothing over, so a child on an older image runs it on data a
// newer one wrote (rule 7), where an old child listing refuses
// (TestSyncOldChildListing). The first sync from prod after the server's
// upgrade, before prod is released again.
func TestSyncOldParentListing(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	d.publish("mariadb:11.8", "I1")
	dev.release(false, db118)
	d.publish("mariadb:11.8", "I2")
	prod.release(false, db118)
	// prod's release as an older devopsy recorded it, and no history yet.
	os.WriteFile(filepath.Join(prod.e.rel(prod.cur), ListFile), []byte("mariadb:11.8\tI2\n"), 0o644)
	os.Remove(historyPath(prod.base()))
	err := syncInto(t, prod, dev, false)
	if !errors.Is(err, ErrRefused) && dev.runs("database") != "I2" {
		t.Errorf("a sync from a parent an older devopsy released (%v): the child's database runs %s on data mariadb:11.8 at I2 wrote, want I2 or a refusal\n%s", err, dev.runs("database"), out.String())
	}
}
