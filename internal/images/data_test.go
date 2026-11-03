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
	"time"
)

// sim is one environment as its activations drive it: a release is
// checked (Check), fetches what the registry serves (fakeDocker.upstream),
// records, goes live (up: each service on what devopsy's compose file
// gives it, else on what its name holds) and appends what ran; rollbacks
// and restarts restore.
type sim struct {
	t   *testing.T
	d   *fakeDocker
	e   *recEnv
	out *bytes.Buffer
	cur string
	n   int
	// strict: a restart whose restore fails is the test's failure, never
	// a fall back to --as-built.
	strict bool
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

// fetch is a release's fetch, in the environment's mode (its releases'
// records'): own images built anew, pulled ones pulled as the registry
// has them now.
func (m *sim) fetch(svcs []Service) {
	for _, svc := range svcs {
		if svc.own(m.e.mode) {
			m.n++
			m.d.pull(svc.Ref, fmt.Sprintf("%s-build-%d", m.e.set.Name, m.n))
			continue
		}
		if err := m.d.Pull(svc.Ref); err != nil {
			m.t.Fatalf("fetch %s: %v", svc.Ref, err)
		}
	}
}

// release checks, fetches, records and, unless refused, goes live and
// runs: what an activation does.
func (m *sim) release(yes bool, svcs ...Service) (string, error) {
	m.t.Helper()
	id := m.e.release(svcs...)
	rel := m.e.rel(id)
	if yes {
		m.e.set.Accept = acceptAll(svcs)
	}
	defer func() { m.e.set.Accept = nil }()
	if err := m.e.set.Check(rel); err != nil {
		os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
		return id, err
	}
	m.fetch(svcs)
	if err := m.e.set.Record(rel, m.prev()); err != nil {
		m.e.set.Undo(rel, m.prev(), false)
		os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
		return id, err
	}
	m.cur = id
	m.up()
	if err := m.e.set.Ran(m.base(), rel); err != nil {
		m.t.Fatal(err)
	}
	return id, nil
}

// up is the run step: each service on what devopsy's compose file gives
// it (image:), else on its name, labeled as that file labels it.
func (m *sim) up() {
	var cs []container
	for _, c := range m.d.containers {
		if c.project != m.e.set.Name {
			cs = append(cs, c)
		}
	}
	imgs, labels, _ := composeFacts(m.e.rel(m.cur))
	for _, svc := range m.e.svcs[m.cur] {
		ref := svc.Ref
		if i := imgs[svc.Name]; i != "" {
			ref = i
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
		cs = append(cs, container{image: img.ID, project: m.e.set.Name, service: svc.Name, digest: labels[svc.Name]})
	}
	m.d.containers = cs
}

// rollback restores target in place of current, then runs it.
func (m *sim) rollback(target string, yes bool) error {
	if yes {
		m.e.set.Accept = acceptAll(m.e.svcs[target])
	}
	defer func() { m.e.set.Accept = nil }()
	err := m.e.set.Restore(m.e.rel(target), m.prev(), false)
	if errors.Is(err, ErrGone) {
		m.fetch(m.e.svcs[target])
		err = m.e.set.Restore(m.e.rel(target), m.prev(), true)
	}
	if err != nil {
		return err
	}
	m.cur = target
	m.up()
	return m.e.set.Ran(m.base(), m.e.rel(target))
}

// failRun is a release whose run step fails once its services ran,
// restarted on prev as get_images does it (img restore --restart, again
// --fetched when an image it builds is gone, else --as-built), then its
// run step and img ran. accept is the release's --accept-image, which
// the restart never sees (runImages drops it), here set throughout.
func (m *sim) failRun(accept bool, svcs ...Service) (string, error) {
	m.t.Helper()
	prev := m.cur
	if accept {
		m.e.set.Accept = acceptAll(svcs)
	}
	defer func() { m.e.set.Accept = nil }()
	id := m.e.release(svcs...)
	rel := m.e.rel(id)
	if err := m.e.set.Check(rel); err != nil {
		os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
		return id, err
	}
	m.fetch(svcs)
	if err := m.e.set.Record(rel, m.prev()); err != nil {
		m.t.Fatal(err)
	}
	m.cur = id
	m.up()
	m.e.set.Ran(m.base(), rel)
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	m.restart(prev, id)
	return id, nil
}

// restart is get_images then the restart's run step: prev restored in
// place of the failed release.
func (m *sim) restart(prev, failed string) {
	m.t.Helper()
	m.e.set.Restart = true
	defer func() { m.e.set.Restart, m.e.set.AsBuilt = false, false }()
	err := m.e.set.Restore(m.e.rel(prev), m.e.rel(failed), false)
	if errors.Is(err, ErrGone) {
		m.fetch(m.e.svcs[prev])
		err = m.e.set.Restore(m.e.rel(prev), m.e.rel(failed), true)
	}
	if err != nil {
		if m.strict {
			m.fail("the restart's restore: %v", err)
		}
		m.e.set.AsBuilt = true
		if err := m.e.set.Restore(m.e.rel(prev), m.e.rel(failed), false); err != nil {
			m.fail("restore --as-built: %v", err)
		}
	}
	m.cur = prev
	m.up()
	m.e.set.Ran(m.base(), m.e.rel(prev))
}

// snapshot takes a snapshot of the current release and its data as a
// take writes it: its data's windows (SnapshotData), its listing
// bookmarked (BookmarkFrom).
func (m *sim) snapshot(id string) string {
	m.t.Helper()
	dir := filepath.Join(m.e.set.Snapshots, id)
	os.MkdirAll(filepath.Join(dir, "data"), 0o755)
	os.WriteFile(filepath.Join(dir, "data", "images"), []byte(m.e.set.SnapshotData(m.base(), m.e.rel(m.cur))), 0o644)
	if err := m.e.set.BookmarkFrom(dir, m.e.rel(m.cur)); err != nil {
		m.fail("snapshot: %v", err)
	}
	return dir
}

// fullRestore restores the snapshot dir whole: a release made from its
// code (svcs), its record naming the snapshot and its listing the
// snapshot's (make-release), its images restored checking nothing (img
// restore --restore, again --fetched when one it builds is gone), then
// the data step (ImportDone), the run step and img ran.
func (m *sim) fullRestore(dir string, svcs []Service) (string, error) {
	m.t.Helper()
	id := m.e.release(svcs...)
	rel := m.e.rel(id)
	os.WriteFile(filepath.Join(rel, RecordFile), []byte(`{"mode":"`+m.e.mode+`","restore":"`+filepath.Base(dir)+`"}`), 0o644)
	if err := m.e.set.BookmarkFrom(rel, dir); err != nil {
		m.fail("make-release: %v", err)
	}
	m.e.set.NoCheck = true
	defer func() { m.e.set.NoCheck = false }()
	err := m.e.set.Restore(rel, m.prev(), false)
	if errors.Is(err, ErrGone) {
		m.fetch(svcs)
		err = m.e.set.Restore(rel, m.prev(), true)
	}
	if err != nil {
		return id, err
	}
	m.cur = id
	data, _ := os.ReadFile(filepath.Join(dir, "data", "images"))
	m.e.set.ImportDone(m.base(), rel, string(data))
	m.up()
	m.e.set.Ran(m.base(), rel)
	return id, nil
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

// history is a service's lines as how:id:version.
func (m *sim) history(svc string) []string {
	var ls []string
	for _, r := range readHistory(m.base()) {
		if r.Service == svc {
			ls = append(ls, r.How+":"+r.ID+":"+r.Version)
		}
	}
	return ls
}

func (m *sim) fail(format string, args ...any) {
	m.t.Helper()
	m.t.Fatalf(format+"\n%s", append(args, m.out.String())...)
}

// dbAt is a pulled database whose label says version; dbSvc's (in
// record_test) says its tag's, as the pattern of one variable for both.
func dbAt(ref, version string) Service {
	s := dbSvc(ref)
	s.DataVersion = version
	return s
}

// builtDB is a database the project builds (FROM mariadb), its label
// version.
func builtDB(name, version string) Service {
	return Service{Name: "database", Ref: name + "-database", Build: true, Data: true, Method: CopyVolumes, DataVersion: version}
}

func acceptAll(svcs []Service) map[string]bool {
	m := map[string]bool{}
	for _, svc := range svcs {
		if svc.Data {
			m[svc.Name] = true
		}
	}
	return m
}

var db118 = dbSvc("mariadb:11.8")

// The rule, a row per case: the window's version is its highest line's;
// higher goes, lower is refused, the same goes unless pulled under
// another tag than the window's newest line at that version (a line
// without a tag, a built one's, checks none); any, and a window without
// a version, check nothing.
func TestRule(t *testing.T) {
	t.Parallel()
	l := func(how, version, tag string) ran {
		return ran{Service: "database", How: how, Version: version, Tag: tag}
	}
	for _, c := range []struct {
		name         string
		w            []ran
		version, tag string
		refused      string
	}{
		{"no history", nil, "11.8", "11.8", ""},
		{"no version in the window", []ran{l(Ran, "", "")}, "10", "10", ""},
		{"after any", []ran{l(AnyRan, "", "")}, "1", "1", ""},
		{"higher", []ran{l(Ran, "11.8", "11.8")}, "12.1", "12.1", ""},
		{"higher by a part", []ran{l(Ran, "11.8", "11.8")}, "11.8.1", "11.8.1", ""},
		{"the same tag", []ran{l(Ran, "11.8", "11.8")}, "11.8", "11.8", ""},
		{"missing parts are 0", []ran{l(Ran, "11", "11")}, "11.0", "11", ""},
		{"lower", []ran{l(Ran, "12.1", "12.1")}, "11.8", "11.8", "devopsy.data.version=11.8 is lower than its data's version, 12.1"},
		{"lower than the highest, not the last", []ran{l(Ran, "12.1", "12.1"), l(Ran, "11.8", "11.8")}, "11.8", "11.8", "lower than its data's version, 12.1"},
		{"another tag at the same version", []ran{l(Ran, "11.8", "11.8.1")}, "11.8", "11.8.3", "its tag is 11.8.3, and its data ran 11.8.1 at the same devopsy.data.version=11.8"},
		{"the newest tag at the version counts", []ran{l(Ran, "11.8", "11.8.1"), l(Ran, "11.8", "11.8.3")}, "11.8", "11.8.3", ""},
		{"a built service checks no tag", []ran{l(Ran, "11.8", "11.8")}, "11.8", "", ""},
		{"a built line checks no tag", []ran{l(Ran, "11.8", "")}, "11.8", "11.8", ""},
		{"any is never refused", []ran{l(Ran, "12.1", "12.1")}, AnyVersion, "11.8", ""},
		{"a forced line starts over", []ran{l(Forced, "11.4", "11.4")}, "11.4", "11.4", ""},
	} {
		w := history(c.w).window("database")
		got := rule(w, c.version, c.tag, "its")
		if (got == "") != (c.refused == "") || !strings.Contains(got, c.refused) {
			t.Errorf("%s: %q, want %q", c.name, got, c.refused)
		}
	}
	// The window starts at its last line but a plain run's.
	w := history{l(Ran, "12.1", "12.1"), l(Forced, "11.4", "11.4"), l(Ran, "11.4", "11.4")}.window("database")
	if len(w) != 2 || rule(w, "11.4", "11.4", "its") != "" {
		t.Errorf("window %v", w)
	}
}

// One version parser: dotted non-negative integers, compared
// numerically, missing parts 0; the tag as the rule compares it, the
// repository never; the version a message suggests from an image.
func TestVersions(t *testing.T) {
	t.Parallel()
	for v, ok := range map[string]bool{"11": true, "11.8": true, "12.1.3": true, "0": true, "": false, "v11": false, "11.": false, ".1": false, "11.8-1": false, "any": false, "false": false} {
		if IsVersion(v) != ok {
			t.Errorf("IsVersion(%q) = %v", v, !ok)
		}
	}
	for _, c := range []struct {
		a, b string
		want int
	}{{"11.8", "11.10", -1}, {"12", "11.99", 1}, {"11", "11.0.0", 0}, {"2", "10", -1}, {"", "1", -1}} {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%s, %s) = %d", c.a, c.b, got)
		}
	}
	for ref, tag := range map[string]string{"mariadb:11.8": "11.8", "mariadb": "latest", "mirror.example:5000/mariadb": "latest", "mirror.example:5000/mariadb:11.8": "11.8", "mariadb@sha256:abc": "sha256:abc", "docker.io/library/mariadb:11.8": "11.8"} {
		if got := TagOf(ref); got != tag {
			t.Errorf("TagOf(%s) = %s, want %s", ref, got, tag)
		}
	}
	for ref, v := range map[string]string{"mariadb:11.8": "11.8", "postgres:16-alpine": "16", "mariadb:11.8.9-noble": "11.8.9", "mariadb:lts": "<version>", "mariadb": "<version>", "mariadb@sha256:abc": "<version>"} {
		if got := VersionHint(ref, false); got != v {
			t.Errorf("VersionHint(%s) = %s, want %s", ref, got, v)
		}
	}
	if got := VersionHint("mariadb:11.8", true); got != "<version>" {
		t.Errorf("a built service's hint: %s", got)
	}
}

// A release follows the rule before anything is built or pulled: the tag
// moving upstream goes (patches), a raised label goes, a tag changed
// with the label left as it was is refused and fetches nothing, and
// --accept-image lets it through, its history starting over once it
// runs. The listing and each history line carry the version and tag.
func TestDataRelease(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	r1, err := m.release(false, appSvc, db118)
	if err != nil || m.runs("database") != "I1" {
		m.fail("r1 (%v): %s", err, m.runs("database"))
	}
	if e, _ := match(readList(m.e.rel(r1)), entry{Service: "database"}); e.DataVersion != "11.8" || e.Tag != "11.8" {
		t.Errorf("listing: %+v", e)
	}
	if e, _ := match(readList(m.e.rel(r1)), entry{Service: "app"}); e.DataVersion != AnyVersion || e.Tag != "" {
		t.Errorf("a built app's listing: %+v", e)
	}
	// The tag moves forward: the same tag, a patch.
	d.publish("mariadb:11.8", "I2")
	if _, err := m.release(false, appSvc, db118); err != nil || m.runs("database") != "I2" {
		m.fail("r2 (%v): %s", err, m.runs("database"))
	}
	// A pin bumped under the coarser label: another tag at 11.8, refused
	// before its fetch, which would have pulled it.
	d.publish("mariadb:11.8.9", "I3")
	out.Reset()
	if _, err := m.release(false, appSvc, dbAt("mariadb:11.8.9", "11.8")); !errors.Is(err, ErrRefused) {
		m.fail("a tag changed without the label (%v)", err)
	}
	if _, pulled := d.names["mariadb:11.8.9"]; pulled || m.runs("database") != "I2" {
		m.fail("the refused release fetched (%v) or the database moved: %s", d.names, m.runs("database"))
	}
	for _, want := range []string{"the release would run its data on an image its version does not allow: nothing changed", "database: its tag is 11.8.9, and its data ran 11.8 at the same devopsy.data.version=11.8", "raise devopsy.data.version", "devopsy @vm1:prod --release --accept-image database"} {
		if !strings.Contains(out.String(), want) {
			m.fail("refusal: no %q", want)
		}
	}
	// With the pattern, the label is the tag: a higher version.
	if _, err := m.release(false, appSvc, dbSvc("mariadb:11.8.9")); err != nil || m.runs("database") != "I3" {
		m.fail("the pin with its label (%v): %s", err, m.runs("database"))
	}
	// Accepted: another tag at the same version runs, its history over.
	d.publish("mariadb:11.8.9-noble", "I4")
	if _, err := m.release(true, appSvc, dbAt("mariadb:11.8.9-noble", "11.8.9")); err != nil || m.runs("database") != "I4" {
		m.fail("accepted (%v): %s", err, m.runs("database"))
	}
	if got := strings.Join(m.history("database"), " "); got != "ran:I1:11.8 ran:I2:11.8 ran:I3:11.8.9 forced:I4:11.8.9" {
		t.Errorf("history: %s", got)
	}
	if l, _ := readHistory(m.base()).last("database"); l.Tag != "11.8.9-noble" || l.Ref != "mariadb:11.8.9-noble" {
		t.Errorf("the last line: %+v", l)
	}
}

// A rollback is checked as a release is: within a version and a tag it
// goes, a floating tag forward; to a lower version it is refused before
// any pull (nothing changes), naming the release that fixes forward, the
// release snapshot of the target's data and --accept-image, which lets
// it through, its history over. A lenient check (CheckRollback, at img
// rollback-target) refuses the same, and nothing when the target's
// config cannot be read.
func TestDataRollback(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	d.publish("mariadb:11.8", "I1")
	r1, _ := m.release(false, appSvc, db118)
	d.publish("mariadb:11.8", "I2")
	r2, _ := m.release(false, appSvc, db118)
	// Within the version and the tag: the name as it is now, I2.
	if err := m.rollback(r1, false); err != nil || m.runs("database") != "I2" {
		m.fail("rollback within 11.8 (%v): %s", err, m.runs("database"))
	}
	m.e.set.Remove(m.e.rel(r2))
	os.RemoveAll(m.e.rel(r2))
	d.publish("mariadb:12.1", "J1")
	r3, err := m.release(false, appSvc, dbSvc("mariadb:12.1"))
	if err != nil || m.runs("database") != "J1" {
		m.fail("r3 (%v): %s", err, m.runs("database"))
	}
	// The release snapshot r3 took of r1's data.
	snap := filepath.Join(m.e.set.Snapshots, "20261102000001")
	os.MkdirAll(snap, 0o755)
	os.WriteFile(filepath.Join(snap, SnapshotManifest), []byte("format\t1\nkind\trelease\nrelease\t"+r1+"\n"), 0o644)
	d.publish("mariadb:11.8", "I3")
	out.Reset()
	if err := m.e.set.CheckRollback(m.e.rel(r1)); !errors.Is(err, ErrRefused) {
		m.fail("the lenient check let it through: %v", err)
	}
	if err := m.rollback(r1, false); !errors.Is(err, ErrRefused) || m.runs("database") != "J1" || d.names["mariadb:11.8"] != "I2" {
		m.fail("a rollback below the data's version (%v): runs %s, mariadb:11.8 is %s (pulled?)", err, m.runs("database"), d.names["mariadb:11.8"])
	}
	for _, want := range []string{"the rollback to " + r1 + " would run its data on an image its version does not allow: nothing changed", "devopsy.data.version=11.8 is lower than its data's version, 12.1", "devopsy @vm1:prod --release", "devopsy @vm1:prod --snapshots restore 20261102000001", "devopsy @vm1:prod --rollback --accept-image database"} {
		if !strings.Contains(out.String(), want) {
			m.fail("refusal: no %q", want)
		}
	}
	// The target's code is the older release's: no label to raise there.
	if strings.Contains(out.String(), "raise "+DataVersionLabel) {
		m.fail("a rollback's refusal offers to raise the label")
	}
	m.e.set.List = func(string) ([]Service, error) { return nil, errors.New("required variable X is missing") }
	if err := m.e.set.CheckRollback(m.e.rel(r1)); err != nil {
		t.Errorf("a config that cannot be read refused the lenient check: %v", err)
	}
	m.e.set.List = func(rel string) ([]Service, error) { return m.e.svcs[filepath.Base(rel)], nil }
	// What mariadb:11.8 holds now, I2: a rollback pulls a name only once
	// it is gone. Accepted, said once: the lenient check (img
	// rollback-target) says only a refusal, img restore the rest.
	out.Reset()
	m.e.set.Accept = map[string]bool{"database": true}
	if err := m.e.set.CheckRollback(m.e.rel(r1)); err != nil {
		m.fail("the lenient check, accepted: %v", err)
	}
	if err := m.rollback(r1, true); err != nil || m.runs("database") != "I2" {
		m.fail("accepted (%v): %s", err, m.runs("database"))
	}
	if n := strings.Count(out.String(), "accepted (--accept-image)"); n != 1 {
		m.fail("the accepted warning said %d times", n)
	}
	if got := strings.Join(m.history("database"), " "); got != "ran:I1:11.8 ran:I2:11.8 ran:J1:12.1 forced:I2:11.8" {
		t.Errorf("history: %s", got)
	}
	_ = r3
}

// any: nothing checked, a rollback goes freely, and its run starts the
// window over, so a version set later is adopted, never compared with
// one from before. A built database is protected by its version alone:
// a rebuild at the same version goes (no tag), a lower one is refused,
// and a switch between build: and image: at the same version passes.
func TestDataAnyAndBuilt(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:12.1", "J1")
	m.release(false, dbSvc("mariadb:12.1"))
	d.publish("mariadb:11.8", "I1")
	r2, err := m.release(false, dbAt("mariadb:11.8", AnyVersion))
	if err != nil || m.runs("database") != "I1" {
		m.fail("any (%v): %s", err, m.runs("database"))
	}
	if _, err := m.release(false, dbSvc("mariadb:11.8")); err != nil {
		m.fail("a version after any is adopted: %v", err)
	}
	if err := m.rollback(r2, false); err != nil {
		m.fail("a rollback to any: %v", err)
	}
	if got := strings.Join(m.history("database"), " "); got != "ran:J1:12.1 any:I1: ran:I1:11.8 any:I1:" {
		t.Errorf("history: %s", got)
	}
	b := newSim(t, d, "app-stage", &out)
	if _, err := b.release(false, builtDB("app-stage", "11.8")); err != nil {
		b.fail("built: %v", err)
	}
	if _, err := b.release(false, builtDB("app-stage", "11.8")); err != nil {
		b.fail("rebuilt at the same version: %v", err)
	}
	if _, err := b.release(false, dbSvc("mariadb:11.8")); err != nil {
		b.fail("built to pulled at the same version: %v", err)
	}
	r4, err := b.release(false, builtDB("app-stage", "12.1"))
	if err != nil {
		b.fail("raised: %v", err)
	}
	if _, err := b.release(false, builtDB("app-stage", "11.8")); !errors.Is(err, ErrRefused) {
		b.fail("a built database lowered: %v", err)
	}
	_ = r4
}

// --accept-image forces only what a check refused, in every check: a
// release's (Check, then Record's ForcedFile), a rollback's, a sync's and
// an import's take-over; one it let through runs as it would, its
// history going on.
func TestDataAcceptOnlyRefused(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	r1, _ := m.release(true, db118)
	if _, err := os.Stat(filepath.Join(m.e.rel(r1), ForcedFile)); !os.IsNotExist(err) || !strings.Contains(out.String(), "database: --accept-image lets through only what the check refuses") {
		m.fail("a first release accepted: forced file %v", err)
	}
	d.publish("mariadb:12.1", "J1")
	r2, _ := m.release(true, dbSvc("mariadb:12.1"))
	if got := strings.Join(m.history("database"), " "); got != "ran:I1:11.8 ran:J1:12.1" {
		t.Errorf("history after accepted releases nothing refused: %s", got)
	}
	if err := m.rollback(r1, true); err != nil {
		m.fail("accepted rollback: %v", err)
	}
	m.e.set.Remove(m.e.rel(r1))
	if err := m.rollback(r2, true); err != nil {
		m.fail("accepted rollback nothing refuses: %v", err)
	}
	if got := strings.Join(m.history("database"), " "); got != "ran:I1:11.8 ran:J1:12.1 forced:I1:11.8 ran:J1:12.1" {
		t.Errorf("history after rollbacks: %s", got)
	}
	child := newSim(t, d, "app-dev", &out)
	child.release(false, dbSvc("mariadb:12.1"))
	if err := syncInto(t, m, child, true); err != nil {
		child.fail("sync: %v", err)
	}
	if got := strings.Join(child.history("database"), " "); !strings.HasPrefix(got, "sync vm1:prod:I1:11.8 ran:J1:12.1") {
		t.Errorf("an accepted sync nothing refuses takes the parent's window: %s", got)
	}
}

// A release that syncs checks the child's labels against the parent's
// window before it builds or pulls anything (Check with SyncData): the
// copy replaces the child's data. The child's own window does not count
// for what the parent copies.
func TestDataReleaseSyncChecksFirst(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	d.publish("mariadb:12.1", "J1")
	prod.release(false, dbSvc("mariadb:12.1"))
	d.publish("mariadb:11.8", "I1")
	dev.release(false, db118)
	dev.e.set.SyncData = prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	defer func() { dev.e.set.SyncData = "" }()
	d.publish("mariadb:11.8", "I2")
	out.Reset()
	if _, err := dev.release(false, db118); !errors.Is(err, ErrRefused) || d.names["mariadb:11.8"] != "I1" {
		dev.fail("a release that syncs onto the parent's 12.1 (%v): mariadb:11.8 is %s", err, d.names["mariadb:11.8"])
	}
	if !strings.Contains(out.String(), "devopsy.data.version=11.8 is lower than the parent's data's version, 12.1") || !strings.Contains(out.String(), "devopsy @vm1:dev --release --accept-image database") {
		dev.fail("refusal")
	}
	// A --release-sync's refusal names --release-sync (img's --again).
	dev.e.set.Again = "--release-sync"
	out.Reset()
	if _, err := dev.release(false, db118); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "devopsy @vm1:dev --release-sync --accept-image database") {
		dev.fail("a --release-sync onto the parent's 12.1 (%v)", err)
	}
	dev.e.set.Again = ""
	if _, err := dev.release(false, dbSvc("mariadb:12.1")); err != nil {
		dev.fail("at the parent's version: %v", err)
	}
}

// The restart after a failed run step cannot refuse: every data service
// the rule refuses, once the failed release's image ran on its data,
// stays on that image, a pulled one by digest, a built one by its own
// name retagged from the failed release's bookmark, whatever
// --accept-image said; the hold's own version and tag are what ran
// records, so a later rollback finds the data's version; --status names
// both holds, and a reload is not refused for them.
func TestDataRestartHolds(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	// Its build key follows its version (a build argument), as a build
	// raised with the data's would.
	search := func(v string) Service {
		return Service{Name: "search", Ref: "app-prod-search", Build: true, Data: true, Method: CopyVolumes, DataVersion: v, BuildKey: "key;VERSION=" + v}
	}
	d.publish("mariadb:11.8", "I1")
	r1, _ := m.release(false, db118, search("8"))
	searchWas := m.runs("search")
	d.publish("mariadb:12.1", "J1")
	r2, err := m.failRun(true, dbSvc("mariadb:12.1"), search("9"))
	if err != nil {
		m.fail("r2: %v", err)
	}
	if m.cur != r1 || m.runs("database") != "J1" || m.runs("search") == searchWas || m.runs("search") == "" {
		m.fail("the restart of %s: database %s (want J1), search %s (want %s's build, not %s)", r1, m.runs("database"), m.runs("search"), r2, searchWas)
	}
	imgs, _, held := composeFacts(m.e.rel(r1))
	if imgs["database"] != "mariadb@J1" || imgs["search"] != "" || held["database"] != r2 || held["search"] != r2 {
		t.Errorf("devopsy's compose file: images %v, held %v", imgs, held)
	}
	if d.names["mariadb:11.8"] != "I1" {
		t.Errorf("the pulled name moved: %s", d.names["mariadb:11.8"])
	}
	if !strings.Contains(out.String(), "database stays on the image release "+r2+" ran on its data") || !strings.Contains(out.String(), "a release that fixes forward is the way on") {
		m.fail("the hold is not said")
	}
	for _, svc := range []string{"database", "search"} {
		if l, _ := readHistory(m.base()).last(svc); l.Version != map[string]string{"database": "12.1", "search": "9"}[svc] {
			t.Errorf("%s's history ends on %+v: the held image's own version, never %s's", svc, l, r1)
		}
	}
	m.e.set.RanCut(m.base(), m.e.rel(r1))
	if got := strings.Join(m.history("database"), " "); got != "ran:I1:11.8 ran:J1:12.1" {
		t.Errorf("history after the restart: %s", got)
	}
	lines := strings.Join(StatusLines(d, "app-prod", m.e.rel(r1)), "\n")
	if !strings.Contains(lines, "database held on mariadb@J1, the image release "+r2+" ran on its data") || !strings.Contains(lines, "search held on app-prod-search (") {
		t.Errorf("--status:\n%s", lines)
	}
	if needs, err := m.e.set.ReloadCheck(m.e.rel(r1)); err != nil || len(needs) > 0 {
		t.Errorf("a reload after the hold: %v %v", needs, err)
	}
	// The child of a held parent takes the held version.
	if p := parseParentData(m.e.set.ParentData(m.base(), m.e.rel(r1)))["database"]; p == nil || windowVersion(p.window()) != "12.1" {
		t.Errorf("ParentData of the held environment: %+v", p)
	}
}

// The hold survives a fetch: the restart whose own image is gone builds
// again (ErrGone, then --fetched), which moves a built database's name to
// the previous release's build, and the hold puts it back on the image
// its data ran.
func TestDataRestartHoldSurvivesFetch(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.strict = true
	r1, _ := m.release(false, appSvc, builtDB("app-prod", "11.8"))
	// r1's app bookmark gone: its restart builds again.
	es := readList(m.e.rel(r1))
	for i, b := range bookmarks(es, m.e.set.Name, r1) {
		if es[i].Service == "app" {
			d.Remove(b)
		}
	}
	r2, _ := m.failRun(false, appSvc, builtDB("app-prod", "12.1"))
	want := ""
	if e, ok := match(readList(m.e.rel(r2)), entry{Service: "database"}); ok {
		want = e.ID
	}
	if m.runs("database") != want || want == "" || d.names["app-prod-database:latest"] != want {
		m.fail("database runs %s, its name holds %s, want %s's %s", m.runs("database"), d.names["app-prod-database:latest"], r2, want)
	}
}

// The classic image store: a built image has no registry digest, so its
// hold goes by its own name and id, never a digest; and a failed release
// that built the service over a previous one that pulls it holds by
// image: its own name in the previous release's compose file, the pulled
// name never retagged, the listing recording that image so the
// previous release's bookmark keeps it once the failed one goes.
func TestDataRestartHoldBuilt(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	r1, _ := m.release(false, builtDB("app-prod", "11.8"))
	r2, _ := m.failRun(false, builtDB("app-prod", "12.1"))
	failed, _ := match(readList(m.e.rel(r2)), entry{Service: "database"})
	delete(d.digests, failed.ID)
	m.cur = r2
	m.restart(r1, r2)
	imgs, labels, held := composeFacts(m.e.rel(r1))
	if imgs["database"] != "" || labels["database"] != failed.ID || held["database"] != r2 || m.runs("database") != failed.ID {
		t.Errorf("classic store: image %q, label %q, held %q, runs %s, want %s", imgs["database"], labels["database"], held["database"], m.runs("database"), failed.ID)
	}

	p := newSim(t, d, "app-stage", &out)
	d.publish("mariadb:11.8", "I1")
	s1, _ := p.release(false, db118)
	s2, _ := p.failRun(false, builtDB("app-stage", "12.1"))
	built, _ := match(readList(p.e.rel(s2)), entry{Service: "database"})
	imgs, _, _ = composeFacts(p.e.rel(s1))
	if imgs["database"] != "app-stage-database" || d.names["mariadb:11.8"] != "I1" || p.runs("database") != built.ID {
		p.fail("built over pulled: image %q, mariadb:11.8 %s, runs %s (want %s)", imgs["database"], d.names["mariadb:11.8"], p.runs("database"), built.ID)
	}
	if e, _ := match(readList(p.e.rel(s1)), entry{Service: "database"}); e.ID != built.ID || e.Ref != "app-stage-database" {
		t.Errorf("listing of %s: %+v", s1, e)
	}
	p.e.set.Remove(p.e.rel(s2))
	os.RemoveAll(p.e.rel(s2))
	if !d.images[built.ID] {
		t.Errorf("the held image went with the failed release: %s's bookmark must keep it", s1)
	}
}

// Without the failed release's image having run on the data (its run
// step failed before the database started), nothing is held: the data
// never met it.
func TestDataRestartHoldsOnlyWhatRan(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	r1, _ := m.release(false, db118)
	d.publish("mariadb:12.1", "J1")
	r2 := m.e.release(dbSvc("mariadb:12.1"))
	m.e.set.Check(m.e.rel(r2))
	m.fetch(m.e.svcs[r2])
	m.e.set.Record(m.e.rel(r2), m.prev())
	m.cur = r2
	m.e.set.Ran(m.base(), m.e.rel(r2)) // the old container: another label
	m.restart(r1, r2)
	if m.runs("database") != "I1" || len(heldIn(m.e.rel(r1))) > 0 {
		m.fail("held %v, runs %s", heldIn(m.e.rel(r1)), m.runs("database"))
	}
}

// img ran writes a line for every data service whose container runs the
// image the release's activation gave it (its label against devopsy's
// compose file), also when only the version changed, and never for a
// container another activation made; a line of the format before the
// version reads as one of none, so the next release adopts its label.
func TestDataRan(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	m.release(false, db118)
	if _, err := m.release(false, dbAt("mariadb:11.8", "11.9")); err != nil {
		m.fail("%v", err)
	}
	if got := strings.Join(m.history("database"), " "); got != "ran:I1:11.8 ran:I1:11.9" {
		t.Errorf("only the version changed: %s", got)
	}
	for i := range d.containers {
		d.containers[i].digest = "another"
	}
	d.publish("mariadb:11.8", "I2")
	id := m.e.release(dbAt("mariadb:11.8", "12"))
	m.e.set.Check(m.e.rel(id))
	m.fetch(m.e.svcs[id])
	m.e.set.Record(m.e.rel(id), m.prev())
	m.e.set.Ran(m.base(), m.e.rel(id))
	if got := strings.Join(m.history("database"), " "); got != "ran:I1:11.8 ran:I1:11.9" {
		t.Errorf("another activation's container counted: %s", got)
	}
	os.WriteFile(filepath.Join(m.base(), HistoryFile), []byte("1\tdatabase\tran\tmariadb:12.1\tJ1\tmariadb@J1\n"), 0o644)
	if _, err := m.release(false, db118); err != nil {
		m.fail("after an old history: %v", err)
	}
}

// A release with --accept-image killed past its switch, its database
// stopped for a release snapshot on the image before: RanCut keeps the
// forced line until its service runs that very image, so the settle that
// makes it again starts its history over from it; an activation's own
// Ran removes it whatever ran.
func TestDataForcedKeptUntilItRuns(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:12.1", "J1")
	m.release(false, dbSvc("mariadb:12.1"))
	d.publish("mariadb:11.8", "I2")
	id := m.e.release(db118)
	rel := m.e.rel(id)
	m.e.set.Accept = acceptAll([]Service{db118})
	if err := m.e.set.Check(rel); err != nil {
		m.fail("--accept-image: %v", err)
	}
	m.fetch([]Service{db118})
	if err := m.e.set.Record(rel, m.prev()); err != nil {
		m.fail("--accept-image: %v", err)
	}
	m.e.set.Accept = nil
	m.cur = id
	for i := range d.containers {
		d.containers[i].state = "exited"
	}
	if err := m.e.set.RanCut(m.base(), rel); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rel, ForcedFile)); err != nil {
		t.Fatalf("a Ran with the database still on its old image removed the forced line: %v", err)
	}
	m.up()
	if err := m.e.set.RanCut(m.base(), rel); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.history("database"), " "); got != "ran:J1:12.1 forced:I2:11.8" {
		t.Errorf("history once the forced image ran: %s", got)
	}
	if _, err := os.Stat(filepath.Join(rel, ForcedFile)); !os.IsNotExist(err) {
		t.Errorf("the forced line stays once its image ran (%v)", err)
	}
}

func TestRemoveHistory(t *testing.T) {
	t.Parallel()
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

// --status names a service a plain up moved off the image its activation
// gave it; nothing is held by digest any more on a release.
func TestStatusLines(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	stage := newSim(t, d, "app-stage", &out)
	d.publish("mariadb:11.8", "I1")
	d.publish("caddy:2-alpine", "C1")
	stage.release(false, db118, proxySvc)
	d.publish("caddy:2-alpine", "C2")
	d.pull("caddy:2-alpine", "C2")
	stage.up()
	got := StatusLines(d, "app-stage", stage.e.rel(stage.cur))
	want := []string{"alwaysup runs caddy@C2, not caddy@C1 its activation gave it: caddy:2-alpine moved since (a plain up follows names)"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("status lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A rollback whose activation is killed outright past its switch (by
// hand on OrbStack: the shell holding the release lock, SIGKILLed right
// after "current is now") leaves current on the older release while the
// newer release's containers run on, their own labels matching what they
// run. --status names the service that runs another image than the
// current release gives it, else the operator sees nothing amiss.
func TestStatusLinesRollbackCutPastSwitch(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	r1, err := m.release(false, appSvc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.release(false, appSvc); err != nil {
		t.Fatal(err)
	}
	newer := m.runs("app")
	// The rollback restores r1's images and switches, then dies before
	// its run step: nothing recreates the containers.
	if err := m.e.set.Restore(m.e.rel(r1), m.prev(), false); err != nil {
		t.Fatal(err)
	}
	m.cur = r1
	if m.runs("app") != newer {
		t.Fatalf("setup: app runs %s, want the newer release's %s", m.runs("app"), newer)
	}
	rel := m.e.rel(r1)
	if got := Recreate(d, "app-prod", rel); strings.Join(got, " ") != "app" {
		t.Fatalf("setup: Recreate says %v, want app (current gives it another image)", got)
	}
	got := StatusLines(d, "app-prod", rel)
	named := false
	for _, l := range got {
		if strings.HasPrefix(l, "app ") {
			named = true
		}
	}
	if !named {
		t.Errorf("status lines %q name no app, which runs %s while the current release gives it another image", got, newer)
	}
}

// syncInto is a sync of parent's data into child, as the child's side
// runs it: the parent's data read (ParentData), checked before anything
// stops (SyncCheck), and, the copy complete, its history taken over
// (SyncDone), then the child's run step on its own images.
func syncInto(t *testing.T, parent, child *sim, yes bool) error {
	t.Helper()
	data := parent.e.set.ParentData(parent.base(), parent.e.rel(parent.cur))
	if yes {
		child.e.set.Accept = acceptAll(child.e.svcs[child.cur])
	}
	defer func() { child.e.set.Accept = nil }()
	rel := child.e.rel(child.cur)
	if err := child.e.set.SyncCheck(rel, data); err != nil {
		return err
	}
	if err := child.e.set.SyncDone(child.base(), rel, data, "vm1:prod"); err != nil {
		return err
	}
	child.up()
	return child.e.set.Ran(child.base(), rel)
}

// A sync checks the child against the parent's window and runs the
// child's own image on the copy, its history the parent's window from a
// sync line; a child below the parent's version, or on another tag at
// it, is refused unless --accept-image, which makes its history one
// forced line of its own image; a parent labeled any sends a window of
// no version; a service whose
// entries are all missing on the parent keeps the child's history.
func TestDataSync(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	d.publish("mariadb:12.1", "J1")
	prod.release(false, dbSvc("mariadb:12.1"))
	d.publish("mariadb:11.8", "I1")
	dev.release(false, db118)
	out.Reset()
	if err := syncInto(t, prod, dev, false); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "the child would run the parent's data on an image its version does not allow: nothing changed") || !strings.Contains(out.String(), "devopsy @vm1:dev --sync --accept-image database") {
		dev.fail("a sync onto a lower child (%v)", err)
	}
	// A --reset's refusal names --reset, with its --from (img's --again).
	dev.e.set.Again = "--reset --from vm1:prod"
	out.Reset()
	if err := syncInto(t, prod, dev, false); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "devopsy @vm1:dev --reset --from vm1:prod --accept-image database") || strings.Contains(out.String(), "--sync --accept-image") {
		dev.fail("a --reset onto a lower child (%v)", err)
	}
	dev.e.set.Again = ""
	if err := syncInto(t, prod, dev, true); err != nil || dev.runs("database") != "I1" {
		dev.fail("accepted (%v): %s", err, dev.runs("database"))
	}
	if got := strings.Join(dev.history("database"), " "); got != "forced:I1:11.8" {
		t.Errorf("an accepted sync's history: %s", got)
	}
	d.publish("mariadb:12.1", "J2")
	dev.release(true, dbSvc("mariadb:12.1"))
	if err := syncInto(t, prod, dev, false); err != nil || dev.runs("database") != "J2" {
		dev.fail("a sync at the parent's version (%v): runs %s, its own", err, dev.runs("database"))
	}
	if got := strings.Join(dev.history("database"), " "); !strings.HasSuffix(got, "sync vm1:prod:J1:12.1 ran:J2:12.1") {
		t.Errorf("the child's history after the sync: %s", got)
	}
	// The parent on any: its window carries no version, and the child's
	// run on the copy starts it at its own.
	prod.release(false, dbAt("mariadb:12.1", AnyVersion))
	if err := syncInto(t, prod, dev, false); err != nil {
		dev.fail("from any: %v", err)
	}
	if got := strings.Join(dev.history("database"), " "); !strings.HasSuffix(got, "sync vm1:prod:J2: ran:J2:12.1") {
		t.Errorf("after a sync from any: %s", got)
	}
	// Entries all missing on the parent: nothing of its is sent.
	missing := dbSvc("mariadb:12.1")
	missing.EntriesKnown, missing.Entries = true, []string{"database"}
	prod.release(false, missing)
	if data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur)); data != "" {
		t.Errorf("ParentData of a service whose entries are missing: %q", data)
	}
}

// A custom copy's data is the parent's too (a dump at its version): the
// parent sends its window, the child checks and takes it over; one the
// copy does not carry (none) sends nothing.
func TestDataSyncCustom(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	custom := func(ref string) Service {
		s := dbSvc(ref)
		s.Method = CopyCustom
		return s
	}
	d.publish("mariadb:12.1", "J1")
	prod.release(false, custom("mariadb:12.1"))
	d.publish("mariadb:11.8", "I1")
	dev.release(false, custom("mariadb:11.8"))
	if err := syncInto(t, prod, dev, false); !errors.Is(err, ErrRefused) {
		dev.fail("a custom copy onto a lower child: %v", err)
	}
	none := dbSvc("mariadb:12.1")
	none.Method = "none"
	prod.release(false, none)
	if data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur)); data != "" {
		t.Errorf("ParentData of a copy of none: %q", data)
	}
}

// ParentData stays bounded (one argument of the child's script), and
// what it cuts never changes the rule: the window's first line, and its
// newest line at its highest version, stay.
func TestParentDataBounded(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	var h history
	h = append(h, ran{Time: 1, Service: "database", How: Forced, Version: "11.8", Tag: "11.8"})
	h = append(h, ran{Time: 2, Service: "database", How: Ran, Version: "12.1", Tag: "12.1"})
	for i := 0; i < 300; i++ {
		h = append(h, ran{Time: int64(3 + i), Service: "database", How: Ran, Version: "11.9", Tag: "11.9", ID: fmt.Sprint(i)})
	}
	m.cur = m.e.release(db118)
	if err := h.write(m.base()); err != nil {
		t.Fatal(err)
	}
	data := m.e.set.ParentData(m.base(), m.e.rel(m.cur))
	p := parseParentData(data)["database"]
	if p == nil || len(p.History) == 0 || len(p.History) > ParentHistoryLines || p.History[0].How != Forced || windowVersion(p.window()) != "12.1" {
		t.Fatalf("ParentData: %+v\n%.300s", p, data)
	}
}

// An export records its service's window (on a target) or its label's
// version and tag alone (locally); an import checks the environment's
// labels against it and takes it over from an import line; a custom
// import, or a tar without the record, starts the history over with a
// line of no version; --resolved keeps a window an import took over
// since its mark, else starts over.
func TestDataImport(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	prod, stage := newSim(t, d, "app-prod", &out), newSim(t, d, "app-stage", &out)
	d.publish("mariadb:12.1", "J1")
	prod.release(false, dbSvc("mariadb:12.1"))
	d.publish("mariadb:11.8", "I1")
	stage.release(false, db118)
	data := DataFor(prod.e.set.ExportData(prod.base(), prod.e.rel(prod.cur), "database"), "database")
	if data == "" || DataFor(data, "app") != "" {
		t.Fatalf("export data %q", data)
	}
	rel := stage.e.rel(stage.cur)
	out.Reset()
	if err := stage.e.set.ImportCheck(rel, data, "database"); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "this environment would run the export's data on an image its version does not allow: nothing imported") || !strings.Contains(out.String(), "--import database --accept-image database") {
		stage.fail("an import onto a lower version (%v)", err)
	}
	// Accepted, its history over from its own image; --resolved after it
	// (the data in, a later step failed) keeps that start, never a line of
	// no version the next release would adopt any label over.
	mark := time.Now().Add(-time.Minute)
	stage.e.set.Accept = map[string]bool{"database": true}
	if err := stage.e.set.ImportCheck(rel, data, "database"); err != nil {
		stage.fail("an accepted import: %v", err)
	}
	stage.e.set.ImportDone(stage.base(), rel, data)
	stage.e.set.Accept = nil
	if err := stage.e.set.Resolved(stage.base(), rel, "database", mark); err != nil || strings.Join(stage.history("database"), " ") != "forced:I1:11.8" {
		t.Errorf("--resolved after an accepted import (%v): %v", err, stage.history("database"))
	}
	stage.release(false, dbSvc("mariadb:12.1"))
	rel = stage.e.rel(stage.cur)
	if err := stage.e.set.ImportCheck(rel, data, "database"); err != nil {
		stage.fail("at its version: %v", err)
	}
	before := time.Now().Add(-time.Hour)
	stage.e.set.ImportDone(stage.base(), rel, data)
	if got := strings.Join(stage.history("database"), " "); got != "import:J1:12.1" {
		t.Errorf("history after the import: %s", got)
	}
	if err := stage.e.set.Resolved(stage.base(), rel, "database", before); err != nil || strings.Join(stage.history("database"), " ") != "import:J1:12.1" {
		t.Errorf("--resolved after a complete import (%v): %v", err, stage.history("database"))
	}
	if err := stage.e.set.Imported(stage.base(), rel, "database"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stage.history("database"), " "); got != "import:J1:12.1 import:J1:" {
		t.Errorf("history after an import without a record: %s", got)
	}
	if _, err := stage.release(false, db118); err != nil {
		stage.fail("a version after an import of none is adopted: %v", err)
	}
	// Locally: the label's version and tag, as one line.
	local := &Set{Docker: d, Name: "app-local", Out: &out, List: func(string) ([]Service, error) { return []Service{db118}, nil }}
	p := parseParentData(DataFor(local.ExportData("", "", "database"), "database"))["database"]
	if p == nil || len(p.History) != 1 || p.History[0].Version != "11.8" || p.History[0].Tag != "11.8" {
		t.Errorf("a local export's record: %+v", p)
	}
}

// A --no-code restore checks the current release's labels against the
// snapshot's window, as a sync checks the parent's.
func TestDataRestoreCheck(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	m.release(false, db118)
	snap := m.e.set.SnapshotData(m.base(), m.e.rel(m.cur))
	d.publish("mariadb:12.1", "J1")
	m.release(false, dbSvc("mariadb:12.1"))
	if err := m.e.set.RestoreCheck(m.e.rel(m.cur), snap, "s1"); err != nil {
		m.fail("12.1 on the snapshot's 11.8 data: %v", err)
	}
	snap = m.e.set.SnapshotData(m.base(), m.e.rel(m.cur))
	m.release(false, dbSvc("mariadb:12.1"))
	m.release(true, db118)
	out.Reset()
	if err := m.e.set.RestoreCheck(m.e.rel(m.cur), snap, "s2"); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "--snapshots restore s2 --no-code --accept-image database") {
		m.fail("11.8 on the snapshot's 12.1 data (%v)", err)
	}
}

// A snapshot taken while the restart holds a data service on the failed
// release's image has the previous release's code and the newer data: a
// manual one, or the release snapshot of that release a fix forward
// takes, which a refused rollback names. Its full restore checks
// nothing, but holds every data service the rule would refuse against
// the snapshot's window, as the restart holds: a pulled one by digest, a
// built one by its own name from its bookmark; said, and in --status.
func TestDataFullRestoreOfHeld(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.strict = true
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	search := func(v string) Service {
		return Service{Name: "search", Ref: "app-prod-search", Build: true, Data: true, Method: CopyVolumes, DataVersion: v}
	}
	d.publish("mariadb:11.8", "I1")
	r1, _ := m.release(false, db118, search("8"))
	d.publish("mariadb:12.1", "J1")
	r2, _ := m.failRun(false, dbSvc("mariadb:12.1"), search("9"))
	searchHeld, _ := match(readList(m.e.rel(r2)), entry{Service: "search"})
	if m.cur != r1 || m.runs("database") != "J1" || m.runs("search") != searchHeld.ID {
		m.fail("the restart of %s: database %s, search %s", r1, m.runs("database"), m.runs("search"))
	}
	snap := m.snapshot("20261102000001")
	// A release fixing forward, then the restore of the snapshot: r1's code.
	d.publish("mariadb:12.1", "J2")
	if _, err := m.release(false, dbSvc("mariadb:12.1"), search("9")); err != nil {
		m.fail("fix forward: %v", err)
	}
	out.Reset()
	id, err := m.fullRestore(snap, m.e.svcs[r1])
	if err != nil {
		m.fail("restore: %v", err)
	}
	rel := m.e.rel(id)
	if m.runs("database") != "J1" || m.runs("search") != searchHeld.ID {
		m.fail("the restored code runs database on %s (want J1), search on %s (want %s): the images the snapshot's data ran", m.runs("database"), m.runs("search"), searchHeld.ID)
	}
	imgs, _, held := composeFacts(rel)
	if imgs["database"] != "mariadb@J1" || held["database"] != "snapshot 20261102000001" || held["search"] != "snapshot 20261102000001" {
		t.Errorf("devopsy's compose file: images %v, held %v", imgs, held)
	}
	for svc, v := range map[string]string{"database": "12.1", "search": "9"} {
		if w := readHistory(m.base()).window(svc); windowVersion(w) != v {
			t.Errorf("%s's window after the restore: %v, want %s", svc, w, v)
		}
		if l, _ := readHistory(m.base()).last(svc); l.Version != v {
			t.Errorf("%s's history ends on %+v, not the held image's version %s", svc, l, v)
		}
	}
	if !strings.Contains(out.String(), "database stays on the image its data ran when snapshot 20261102000001 was taken") {
		m.fail("the hold is not said")
	}
	lines := strings.Join(StatusLines(d, "app-prod", rel), "\n")
	if !strings.Contains(lines, "database held on mariadb@J1, the image its data ran when snapshot 20261102000001 was taken") || !strings.Contains(lines, "search held on app-prod-search (") {
		t.Errorf("--status:\n%s", lines)
	}
}

// Two environments release, fail in their run step after the database
// started (restarted, as built now and then), fail in their fetch, roll
// back, sync from each other, import each other's exports, take and
// restore snapshots (whole or --no-code) and lose releases to pruning,
// their database pulled under several tags and versions, built, or
// labeled any, accepting now and then, while the registry moves the tags.
// A model of the data, kept by the test from what each step runs (the
// versions and tags its releases claim, never devopsy's history or
// listings), says what the rule allows: devopsy refuses exactly what the
// model refuses unaccepted, a restart and a full restore hold exactly
// what the model refuses, and devopsy's window says the model's version.
// And every run the model records is checked against the model's window
// itself: no data ever runs a lower version, or another tag at its
// version, unaccepted. And every kept release's and snapshot's bookmarks
// hold what it lists; once all goes, nothing is left.
func TestDataRandom(t *testing.T) {
	t.Parallel()
	for seed := int64(1); seed <= int64(seeds(300)); seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { dataRandom(t, seed) })
	}
}

// seeds is n, or DEVOPSY_TEST_SEEDS for a longer run.
func seeds(n int) int {
	if v, err := strconv.Atoi(os.Getenv("DEVOPSY_TEST_SEEDS")); err == nil && v > 0 {
		return v
	}
	return n
}

func dataRandom(t *testing.T, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	d := newFake()
	var out bytes.Buffer
	envs := []*sim{newSim(t, d, "app-prod", &out), newSim(t, d, "app-stage", &out)}
	left := filepath.Join(t.TempDir(), LeftFile)
	for _, m := range envs {
		m.e.set.Left = left
		m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	}
	// The model: each environment's database's window, lines of the
	// versions and tags its runs claimed (claim), started over as the rule
	// says (a forced run, any, a copy's take-over).
	model := map[*sim]history{}
	claim := func(svc Service, how string) ran {
		r := ran{Service: "database", How: how, Version: svc.DataVersion, Tag: svc.tag()}
		if svc.DataVersion == AnyVersion {
			r.How, r.Version = AnyRan, ""
		}
		return r
	}
	dbOf := func(svcs []Service) Service {
		for _, s := range svcs {
			if s.Name == "database" {
				return s
			}
		}
		return Service{}
	}
	// verdict is the model's: refused or not, for svc's claim on m's data
	// (or on w's, a copy's source).
	verdict := func(w history, svc Service) bool {
		return rule(w.window("database"), svc.DataVersion, svc.tag(), "its") != ""
	}
	// running is the claim each environment's database runs under now,
	// as the model has it: the release's that gave it its image (a
	// hold's, the failed release's or the snapshot's).
	running := map[*sim]Service{}
	var log []string
	// run is a run of svc's claim on m's data in the model: the invariant,
	// checked against the model's window whatever devopsy said, then the
	// line, a forced one when accepted and refused.
	run := func(m *sim, svc Service, accepted bool) {
		t.Helper()
		refused := verdict(model[m], svc)
		if refused && !accepted {
			t.Fatalf("after %v: %s runs its database as %s=%s (tag %q) on data its window refuses it on, unaccepted\nmodel %v\n%s", log, m.e.set.Name, DataVersionLabel, svc.DataVersion, svc.tag(), model[m].window("database"), out.String())
		}
		how := Ran
		if accepted && refused {
			how = Forced
		}
		model[m] = append(model[m], claim(svc, how))
		running[m] = svc
	}
	type snapshot struct {
		dir     string
		svcs    []Service
		model   history
		running Service
	}
	snaps := map[*sim][]snapshot{}
	snapN := 0
	refs := []string{"mariadb:11.8", "mariadb:11", "mariadb:11.8.9", "mariadb:12.1", "mirror.example/mariadb:11.8"}
	versions := []string{"11", "11.8", "11.8.9", "12.1", AnyVersion}
	img := 0
	push := func(ref string) {
		img++
		d.publish(ref, fmt.Sprintf("I%03d", img))
	}
	for _, r := range refs {
		push(r)
	}
	check := func(step string) {
		t.Helper()
		log = append(log, step)
		for _, m := range envs {
			if m.cur == "" {
				continue
			}
			// devopsy's window says the model's version, and its tag there.
			w, mw := readHistory(m.base()).window("database"), model[m].window("database")
			if windowVersion(w) != windowVersion(mw) {
				t.Fatalf("after %v: %s's data is at %q, the model's at %q\nhistory %v\nmodel %v\n%s", log, m.e.set.Name, windowVersion(w), windowVersion(mw), w, mw, out.String())
			}
			for i, b := range bookmarks(readList(m.e.rel(m.cur)), m.e.set.Name, m.cur) {
				es := readList(m.e.rel(m.cur))
				if es[i].ID != "" {
					if got, _ := d.ID(b); got != es[i].ID {
						t.Fatalf("after %v: %s's bookmark %s is %q, want %q\n%s", log, m.cur, b, got, es[i].ID, out.String())
					}
				}
			}
			for _, sn := range snaps[m] {
				es := readList(sn.dir)
				for i, b := range bookmarks(es, m.e.set.Name, m.e.set.idOf(sn.dir)) {
					if es[i].ID != "" {
						if got, _ := d.ID(b); got != es[i].ID {
							t.Fatalf("after %v: snapshot %s's bookmark %s is %q, want %q\n%s", log, filepath.Base(sn.dir), b, got, es[i].ID, out.String())
						}
					}
				}
			}
		}
	}
	// expect says devopsy's answer agrees with the model's: refused
	// exactly when the model refuses and nothing accepts it.
	expect := func(what string, err error, refused, accepted bool) bool {
		t.Helper()
		want := refused && !accepted
		if err != nil && !errors.Is(err, ErrRefused) {
			t.Fatalf("after %v: %s: %v\n%s", log, what, err, out.String())
		}
		if errors.Is(err, ErrRefused) != want {
			t.Fatalf("after %v: %s: refused %v, the model says %v (accepted %v)\n%s", log, what, err != nil, refused, accepted, out.String())
		}
		return err == nil
	}
	var m *sim
	svcs := func() []Service {
		var db Service
		if rng.Intn(5) == 0 {
			db = builtDB(m.e.set.Name, versions[rng.Intn(len(versions))])
		} else {
			db = dbSvc(refs[rng.Intn(len(refs))])
			if rng.Intn(3) == 0 {
				db.DataVersion = versions[rng.Intn(len(versions))]
			}
		}
		return []Service{{Name: "app", Ref: m.e.set.Name + "-app", Build: true}, db, proxySvc}
	}
	other := func() *sim {
		if m == envs[0] {
			return envs[1]
		}
		return envs[0]
	}
	d.publish("caddy:2-alpine", "C1")
	for step := 0; step < 40; step++ {
		m = envs[rng.Intn(2)]
		switch op := rng.Intn(16); op {
		case 0: // the registry moves a tag
			push(refs[rng.Intn(len(refs))])
		case 1, 2: // a release
			ss := svcs()
			yes := rng.Intn(4) == 0
			refused := m.cur != "" && verdict(model[m], dbOf(ss))
			id, err := m.release(yes, ss...)
			if expect("release "+id, err, refused, yes) {
				run(m, dbOf(ss), yes)
			}
			check(m.e.set.Name + " release " + id)
		case 3, 11: // a run step failing after its database ran, restarted
			if m.cur == "" {
				continue
			}
			prev, ss := m.cur, svcs()
			yes := rng.Intn(4) == 0
			refused := verdict(model[m], dbOf(ss))
			id, err := m.failRun(yes, ss...)
			if !expect("failed release "+id, err, refused, yes) {
				check(m.e.set.Name + " refused " + id)
				continue
			}
			run(m, dbOf(ss), yes)
			failedImg, _ := match(readList(m.e.rel(id)), entry{Service: "database"})
			held := verdict(model[m], dbOf(m.e.svcs[prev]))
			if held != (heldIn(m.e.rel(prev))["database"] != "") {
				t.Fatalf("after %v: the restart of %s held %v, the model says %v\n%s", log, prev, heldIn(m.e.rel(prev)), held, out.String())
			}
			if held && m.runs("database") != failedImg.ID {
				t.Fatalf("after %v: the restart of %s runs its database on %s, not %s's %s that ran on its data\n%s", log, prev, m.runs("database"), id, failedImg.ID, out.String())
			}
			if held {
				run(m, dbOf(ss), false)
			} else {
				run(m, dbOf(m.e.svcs[prev]), false)
			}
			check(m.e.set.Name + " failed run " + id)
		case 4: // a fetch failing: undone
			id := m.e.release(svcs()...)
			if m.e.set.Check(m.e.rel(id)) == nil {
				m.fetch(m.e.svcs[id])
			}
			m.e.set.Undo(m.e.rel(id), m.prev(), false)
			os.WriteFile(filepath.Join(m.e.rel(id), failedFile), nil, 0o644)
			if m.cur != "" {
				m.up()
			}
			check(m.e.set.Name + " failed fetch " + id)
		case 5: // a rollback
			var target string
			for _, id := range m.e.releases() {
				if id < m.cur && !exists(filepath.Join(m.e.rel(id), failedFile)) && !exists(filepath.Join(m.e.rel(id), RolledBackFile)) {
					target = id
				}
			}
			if target == "" {
				continue
			}
			yes := rng.Intn(3) == 0
			refused := verdict(model[m], dbOf(m.e.svcs[target]))
			before := map[string]string{}
			for n, i := range d.names {
				if !isBookmark(n) && !strings.HasSuffix(n, "-app:latest") && !strings.HasSuffix(n, "-database:latest") {
					before[n] = i
				}
			}
			cur := m.cur
			if expect("rollback to "+target, m.rollback(target, yes), refused, yes) {
				run(m, dbOf(m.e.svcs[target]), yes)
				os.WriteFile(filepath.Join(m.e.rel(cur), RolledBackFile), nil, 0o644)
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
		case 8: // a sync of the other environment's data into this one
			o := other()
			if m.cur == "" || o.cur == "" {
				continue
			}
			yes := rng.Intn(3) == 0
			// What runs: a hold's claim, the failed release's or the
			// snapshot's.
			db := running[m]
			refused := verdict(model[o], db)
			if expect("sync from "+o.e.set.Name, syncInto(t, o, m, yes), refused, yes) {
				if yes && refused {
					model[m] = history{claim(db, Forced)}
				} else {
					w := append(history{}, model[o].window("database")...)
					if len(w) == 0 {
						w = history{{Service: "database"}}
					}
					w[0].How = SyncedFrom + "x"
					model[m] = w
					run(m, db, false)
				}
			}
			check(m.e.set.Name + " sync from " + o.e.set.Name)
		case 10: // a complete import of the other's export, or of none
			o := other()
			if m.cur == "" {
				continue
			}
			if o.cur == "" || rng.Intn(3) == 0 {
				if err := m.e.set.Imported(m.base(), m.e.rel(m.cur), "database"); err != nil {
					t.Fatal(err)
				}
				model[m] = history{{Service: "database", How: Imported}}
				check(m.e.set.Name + " import")
				continue
			}
			data := DataFor(o.e.set.ExportData(o.base(), o.e.rel(o.cur), "database"), "database")
			db := running[m]
			yes := rng.Intn(3) == 0
			if yes {
				m.e.set.Accept = map[string]bool{"database": true}
			}
			refused := verdict(model[o], db)
			err := m.e.set.ImportCheck(m.e.rel(m.cur), data, "database")
			if expect("import of "+o.e.set.Name+"'s export", err, refused, yes) {
				m.e.set.ImportDone(m.base(), m.e.rel(m.cur), data)
				if yes && refused {
					model[m] = history{claim(db, Forced)}
				} else {
					w := append(history{}, model[o].window("database")...)
					if len(w) == 0 {
						w = history{{Service: "database"}}
					}
					w[0].How = Imported
					model[m] = w
				}
			}
			m.e.set.Accept = nil
			check(m.e.set.Name + " import of " + o.e.set.Name + "'s export")
		case 12: // a snapshot of the current release and its data
			if m.cur == "" {
				continue
			}
			snapN++
			dir := filepath.Join(m.e.set.Snapshots, fmt.Sprintf("2026101700%04d", snapN))
			m.snapshot(filepath.Base(dir))
			snaps[m] = append(snaps[m], snapshot{dir: dir, svcs: m.e.svcs[m.cur], model: append(history{}, model[m]...), running: running[m]})
			check(m.e.set.Name + " snapshot " + filepath.Base(dir))
		case 13, 14: // a restore of a snapshot: full (a release made from it, nothing checked), or --no-code
			if m.cur == "" || len(snaps[m]) == 0 {
				continue
			}
			sn := snaps[m][rng.Intn(len(snaps[m]))]
			data, _ := os.ReadFile(filepath.Join(sn.dir, "data", "images"))
			if op == 13 {
				id, err := m.fullRestore(sn.dir, sn.svcs)
				if err != nil {
					t.Fatalf("after %v: the restore's images: %v\n%s", log, err, out.String())
				}
				w := append(history{}, sn.model.window("database")...)
				if len(w) == 0 {
					w = history{{Service: "database"}}
				}
				w[0].How = Imported
				model[m] = w
				// The snapshot's code on its data: held on what its data ran
				// when the model refuses the code's claim.
				held := verdict(model[m], dbOf(sn.svcs))
				rel := m.e.rel(id)
				if held != (heldIn(rel)["database"] != "") {
					t.Fatalf("after %v: the full restore of %s held %v, the model says %v\n%s", log, filepath.Base(sn.dir), heldIn(rel), held, out.String())
				}
				if held {
					if e, _ := match(readList(sn.dir), entry{Service: "database"}); m.runs("database") != e.ID {
						t.Fatalf("after %v: the full restore of %s runs its database on %s, not the snapshot's %s\n%s", log, filepath.Base(sn.dir), m.runs("database"), e.ID, out.String())
					}
					run(m, sn.running, false)
				} else {
					run(m, dbOf(sn.svcs), false)
				}
				check(m.e.set.Name + " full restore " + filepath.Base(sn.dir))
				continue
			}
			db := running[m]
			yes := rng.Intn(3) == 0
			if yes {
				m.e.set.Accept = map[string]bool{"database": true}
			}
			refused := verdict(sn.model, db)
			rel := m.e.rel(m.cur)
			if expect("--no-code restore of "+filepath.Base(sn.dir), m.e.set.RestoreCheck(rel, string(data), "s"), refused, yes) {
				m.e.set.ImportDone(m.base(), rel, string(data))
				if yes && refused {
					model[m] = history{claim(db, Forced)}
				} else {
					w := append(history{}, sn.model.window("database")...)
					if len(w) == 0 {
						w = history{{Service: "database"}}
					}
					w[0].How = Imported
					model[m] = w
				}
				m.up()
				m.e.set.Ran(m.base(), rel)
				run(m, db, false)
			}
			m.e.set.Accept = nil
			check(m.e.set.Name + " --no-code restore " + filepath.Base(sn.dir))
		case 15: // a snapshot removed
			if len(snaps[m]) == 0 {
				continue
			}
			i := rng.Intn(len(snaps[m]))
			if err := m.e.set.Remove(snaps[m][i].dir); err != nil {
				t.Fatal(err)
			}
			os.RemoveAll(snaps[m][i].dir)
			m.e.set.Prune()
			snaps[m] = append(snaps[m][:i], snaps[m][i+1:]...)
			check(m.e.set.Name + " remove snapshot")
		}
	}
	d.containers = nil
	for _, m := range envs {
		for _, sn := range snaps[m] {
			m.e.set.Remove(sn.dir)
			os.RemoveAll(sn.dir)
		}
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
