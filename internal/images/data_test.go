package images

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	// fetched, when set, is told each image a fetch got for a service:
	// the claim (version and tag) it was fetched under.
	fetched func(svc Service, id string)
	// ups is the image each up ran the database on ("" without one).
	ups []string
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
		} else if err := m.d.Pull(svc.Ref); err != nil {
			m.t.Fatalf("fetch %s: %v", svc.Ref, err)
		}
		if m.fetched != nil {
			m.fetched(svc, m.d.names[familiar(svc.Ref)])
		}
	}
}

// fetchID is release id's fetch as an activation runs it: its fetch file
// written first (WriteFetch), each build landing under the fetch tag it
// names, a service sharing a build's image given that build's, then
// what is pulled pulled.
func (m *sim) fetchID(id string) {
	rel := m.e.rel(id)
	if err := m.e.set.WriteFetch(rel); err != nil {
		m.t.Fatalf("fetch file %s: %v", id, err)
	}
	tags := fetchTags(rel)
	svcs := m.e.svcs[id]
	for _, svc := range svcs {
		if t := tags[svc.Name]; t != "" && svc.Build {
			m.n++
			m.d.pull(t, fmt.Sprintf("%s-build-%d", m.e.set.Name, m.n))
		}
	}
	for _, svc := range svcs {
		switch t := tags[svc.Name]; {
		case t != "" && svc.Build:
		case t != "":
		case svc.own(m.e.mode):
			m.n++
			m.d.pull(svc.Ref, fmt.Sprintf("%s-build-%d", m.e.set.Name, m.n))
		default:
			if err := m.d.Pull(svc.Ref); err != nil {
				m.t.Fatalf("fetch %s: %v", svc.Ref, err)
			}
		}
		if m.fetched != nil {
			ref := svc.Ref
			if t := tags[svc.Name]; t != "" {
				ref = t
			}
			m.fetched(svc, m.d.names[familiar(ref)])
		}
	}
}

// fetchCached is release id's fetch of a build the cache gave back
// unchanged: each fetch tag on the image the service had (ids), nothing
// built anew.
func (m *sim) fetchCached(id string, ids map[string]string) {
	rel := m.e.rel(id)
	if err := m.e.set.WriteFetch(rel); err != nil {
		m.t.Fatalf("fetch file %s: %v", id, err)
	}
	for svc, t := range fetchTags(rel) {
		if img := ids[svc]; img != "" {
			if err := m.d.Tag(img, t); err != nil {
				m.t.Fatal(err)
			}
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
	m.fetchID(id)
	if err := m.e.set.Record(rel, m.prev()); err != nil {
		m.e.set.Undo(rel, false)
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
	m.ups = append(m.ups, m.runs("database"))
}

// rollback restores target in place of current, then runs it.
func (m *sim) rollback(target string, yes bool) error {
	if yes {
		m.e.set.Accept = acceptAll(m.e.svcs[target])
	}
	defer func() { m.e.set.Accept = nil }()
	err := m.e.set.Restore(m.e.rel(target), m.prev(), false)
	if errors.Is(err, ErrGone) {
		m.fetchID(target)
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
	m.fetchID(id)
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

// failEarly is a release whose run step fails before its database ran
// on its data: the container there is the one before, another
// activation's, or none at all (gone), so img ran writes nothing for it;
// then the restart, as failRun's.
func (m *sim) failEarly(accept, gone bool, svcs ...Service) (string, bool, error) {
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
		return id, false, err
	}
	m.fetchID(id)
	if err := m.e.set.Record(rel, m.prev()); err != nil {
		m.t.Fatal(err)
	}
	m.cur = id
	if gone {
		var cs []container
		for _, c := range m.d.containers {
			if c.project != m.e.set.Name || c.service != "database" {
				cs = append(cs, c)
			}
		}
		m.d.containers = cs
	}
	// The container before runs the image the release gave it already
	// (the same digest): img ran counts it, as it ran.
	ran := false
	for _, c := range m.d.containers {
		if c.project == m.e.set.Name && c.service == "database" && c.digest != "" && c.digest == labelsIn(rel)["database"] {
			ran = true
		}
	}
	m.e.set.Ran(m.base(), rel)
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	m.restart(prev, id)
	return id, ran, nil
}

// restart is get_images then the restart's run step: prev restored in
// place of the failed release.
func (m *sim) restart(prev, failed string) {
	m.t.Helper()
	m.e.set.Restart = true
	defer func() { m.e.set.Restart, m.e.set.AsBuilt = false, false }()
	err := m.e.set.Restore(m.e.rel(prev), m.e.rel(failed), false)
	if errors.Is(err, ErrGone) {
		m.fetchID(prev)
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
// the data step (Land), the run step and img ran.
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
		m.fetchID(id)
		err = m.e.set.Restore(rel, m.prev(), true)
	}
	if err != nil {
		return id, err
	}
	m.cur = id
	data, _ := os.ReadFile(filepath.Join(dir, "data", "images"))
	m.e.set.Land(m.base(), rel, m.e.set.RestoreLanding(string(data), "", nil))
	m.up()
	m.e.set.Ran(m.base(), rel)
	return id, nil
}

// noCode restores the snapshot dir's data alone under the current
// release, as a --no-code restore does: its check against the snapshot's
// data, the data step (Land, which derives its holds again), the
// run step and img ran.
func (m *sim) noCode(dir string) error {
	m.t.Helper()
	rel := m.e.rel(m.cur)
	data, _ := os.ReadFile(filepath.Join(dir, "data", "images"))
	if err := m.e.set.CheckLanding(rel, m.e.set.RestoreLanding(string(data), filepath.Base(dir), nil)); err != nil {
		return err
	}
	if err := m.e.set.Land(m.base(), rel, m.e.set.RestoreLanding(string(data), "", nil)); err != nil {
		m.fail("the data step: %v", err)
	}
	m.up()
	return m.e.set.Ran(m.base(), rel)
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

// A release's run step, and every up after it (a plain one, a reload's,
// a rollback's), runs the image the release fetched, whatever its name
// holds by then: a name is anyone's on the server to move (a project
// user's build of a Docker Hub name, mongo's mongo-express:1, outside
// this environment's locks), so ComposeFile pins each pulled service to
// the digest its listing records, a rollback's file too, and a landing
// (derive) keeps the pin as it is, never taking it for an own image held
// by a bookmark.
func TestReleaseRunsWhatItFetched(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	r1, err := m.release(false, appSvc, db118)
	if err != nil || m.runs("database") != "I1" {
		m.fail("the control, r1 (%v): %s", err, m.runs("database"))
	}
	// Another project's build lands on the name, with a registry digest of its own, as
	// Docker's containerd store gives built images: a digest tells no pulled image apart.
	swap := func() {
		d.images["X1"] = true
		d.names["mariadb:11.8"] = "X1"
		d.digests["X1"] = []string{"mariadb@X1"}
	}
	swap()
	m.up()
	if m.runs("database") != "I1" || len(d.outside) != 0 {
		m.fail("a plain up after the name moved runs %s, not I1 the release fetched (pulled by the up: %v)", m.runs("database"), d.outside)
	}
	// A copy landing (derive) sees the pin and leaves it: a pulled service's image: is no
	// own image by a bookmark.
	before, _ := os.ReadFile(filepath.Join(m.e.rel(r1), ComposeFile))
	if err := m.e.set.derive(m.base(), m.e.rel(r1), map[string]Service{"app": appSvc, "database": db118}, false); err != nil {
		m.fail("derive: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(m.e.rel(r1), ComposeFile))
	if imgs, _, held := composeFacts(m.e.rel(r1)); string(after) != string(before) || held["database"] != "" || isBookmark(imgs["database"]) {
		m.fail("a landing changed the pin:\n%s\nwas:\n%s", after, before)
	}
	// A rollback takes what the name holds at the time, under the lock (a floating tag
	// goes forward), and pins that: the next up runs it whatever the name holds then.
	d.names["mariadb:11.8"] = "I1"
	d.publish("mariadb:11.8", "I2")
	if _, err := m.release(false, appSvc, db118); err != nil || m.runs("database") != "I2" {
		m.fail("r2 (%v): %s", err, m.runs("database"))
	}
	if err := m.rollback(r1, false); err != nil || m.runs("database") != "I2" {
		m.fail("the rollback (%v): %s", err, m.runs("database"))
	}
	if imgs, _, _ := composeFacts(m.e.rel(r1)); imgs["database"] != "mariadb:11.8@I2" {
		m.fail("the rollback's pin: %q", imgs["database"])
	}
	swap()
	m.up()
	if m.runs("database") != "I2" || len(d.outside) != 0 {
		m.fail("an up after the rollback, the name moved, runs %s, not I2 (pulled by the up: %v)", m.runs("database"), d.outside)
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

// A release that syncs, accepted over the parent's higher data, starts
// its history over from its own image, never the container's, which runs
// the release before's until the run step: so when that step fails
// before the database runs, the restart holds the release before on the
// accepted release's image (review: it ran its own older one on the
// copy).
func TestDataReleaseSyncAcceptedRestart(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	d.publish("mariadb:12", "P1")
	prod.release(false, dbSvc("mariadb:12"))
	d.publish("mariadb:11", "R1")
	r1, _ := dev.release(false, dbSvc("mariadb:11"))
	d.publish("mariadb:11.8", "R2")
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	dev.e.set.SyncData, dev.e.set.Accept = data, map[string]bool{"database": true}
	r2 := dev.e.release(dbSvc("mariadb:11.8"))
	rel := dev.e.rel(r2)
	if err := dev.e.set.Check(rel); err != nil {
		dev.fail("the accepted release that syncs: %v", err)
	}
	dev.fetchID(r2)
	if err := dev.e.set.Record(rel, dev.prev()); err != nil {
		dev.fail("record: %v", err)
	}
	dev.cur = r2
	if err := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		dev.fail("sync-done: %v", err)
	}
	if got := strings.Join(dev.history("database"), " "); got != "forced:R2:11.8" {
		t.Errorf("the accepted sync's history: %s, not the release's own image", got)
	}
	// The run step fails before the database is made again: it still
	// runs R1, which img ran does not count.
	dev.e.set.Ran(dev.base(), rel)
	os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
	dev.e.set.SyncData, dev.e.set.Accept = "", nil
	dev.restart(r1, r2)
	if dev.runs("database") != "R2" || heldIn(dev.e.rel(r1))["database"] == "" {
		dev.fail("the restart of %s runs %s, held %v: not the accepted release's image", r1, dev.runs("database"), heldIn(dev.e.rel(r1)))
	}
}

// The restart after a failed run step cannot refuse: every data service
// the rule refuses, once the failed release's image ran on its data,
// stays on that image, a pulled one by digest, a built one by the
// release's own bookmark of it, whatever
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
	if imgs["database"] != "mariadb@J1" || !isBookmark(imgs["search"]) || fileImage(d, m.e.rel(r1), "search", "") != m.runs("search") || held["database"] != r2 || held["search"] != r2 {
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
	holds := StatusLines(d, "app-prod", m.e.rel(r1))
	lines := strings.Join(statusTexts(holds), "\n")
	if !strings.Contains(lines, "database held on mariadb@J1, the image release "+r2+" ran on its data") || !strings.Contains(lines, "search held on app-prod-search (") {
		t.Errorf("--status:\n%s", lines)
	}
	// A hold is what a rollback chose: said, never warned.
	for _, l := range holds {
		if l.Warn {
			t.Errorf("a hold is warned: %s", l.Text)
		}
	}
	if needs, err := m.e.set.ReloadCheck(m.e.rel(r1)); err != nil || len(needs) > 0 {
		t.Errorf("a reload after the hold: %v %v", needs, err)
	}
	// A variable changing a held service's image or build still needs a
	// release: a reload keeps the held image (review: it said a reload
	// applies it).
	was := m.e.svcs[r1]
	m.e.svcs[r1] = []Service{dbSvc("mariadb:11.9"), search("9")}
	needs, err := m.e.set.ReloadCheck(m.e.rel(r1))
	if got := strings.Join(needs, "\n"); err != nil || !strings.Contains(got, "database's image is mariadb:11.9 now, the release fetched mariadb:11.8") || !strings.Contains(got, "search's build changed since the release built its image: VERSION") {
		t.Errorf("a reload of held services whose image and build changed: %v\n%s", err, got)
	}
	m.e.svcs[r1] = was
	// The child of a held parent takes the held version.
	if p := parseParentData(m.e.set.ParentData(m.base(), m.e.rel(r1)))["database"]; p == nil || windowVersion(p.window()) != "12.1" {
		t.Errorf("ParentData of the held environment: %+v", p)
	}
}

// The restart's last resort, once get_images failed: from the listings
// and the history alone, a pulled data service is held by its digest in
// the previous release's compose file and listing; a built one, which
// only a name moved by Docker could hold, is named, with the way on.
func TestDataRestartLastResort(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	search := func(v string) Service {
		return Service{Name: "search", Ref: "app-prod-search", Build: true, Data: true, Method: CopyVolumes, DataVersion: v}
	}
	d.publish("mariadb:11.8", "I1")
	r1, _ := m.release(false, db118, search("8"))
	d.publish("mariadb:12.1", "J1")
	r2 := m.e.release(dbSvc("mariadb:12.1"), search("9"))
	if err := m.e.set.Check(m.e.rel(r2)); err != nil {
		m.fail("r2: %v", err)
	}
	m.fetchID(r2)
	m.e.set.Record(m.e.rel(r2), m.prev())
	m.cur = r2
	m.up()
	m.e.set.Ran(m.base(), m.e.rel(r2))
	// get_images failed: nothing restored, r1's own compose file.
	out.Reset()
	err := m.e.set.LastHold(m.e.rel(r1), m.e.rel(r2))
	m.cur = r1
	m.up()
	if err != nil || m.runs("database") != "J1" {
		m.fail("the last resort (%v): database %s (want J1)", err, m.runs("database"))
	}
	imgs, _, held := composeFacts(m.e.rel(r1))
	if imgs["database"] != "mariadb@J1" || held["database"] != r2 || held["search"] != r2 {
		t.Errorf("devopsy's compose file: images %v, held %v", imgs, held)
	}
	if e, _ := match(readList(m.e.rel(r1)), entry{Service: "database"}); e.ID != "J1" || e.DataVersion != "12.1" {
		t.Errorf("r1's listing: %+v", e)
	}
	for _, want := range []string{"database stays on the image release " + r2 + " ran on its data", "search stays on the image release " + r2 + " ran on its data"} {
		if !strings.Contains(out.String(), want) {
			m.fail("no %q", want)
		}
	}
	m.e.set.Ran(m.base(), m.e.rel(r1))
	if l, _ := readHistory(m.base()).last("database"); l.Version != "12.1" {
		t.Errorf("the history ends on %+v, not the held image's version", l)
	}
	// The image the hold replaced in r1's listing keeps no name once every
	// release goes (review: mariadb:11.8 outlived them all).
	d.publish("mariadb:12.2", "K1")
	if _, err := m.release(false, dbSvc("mariadb:12.2"), search("9")); err != nil {
		m.fail("fix forward: %v", err)
	}
	d.containers = nil
	for _, id := range m.e.releases() {
		if err := m.e.set.Remove(m.e.rel(id)); err != nil {
			t.Fatal(err)
		}
		os.RemoveAll(m.e.rel(id))
	}
	m.e.set.Prune()
	d.Prune()
	if len(d.images) > 0 {
		t.Errorf("images left once every release went: %v %v\n%s", d.images, d.names, out.String())
	}
}

// The last resort rewrites the compose file only with what it can write,
// and every hold already there keeps its mark: a built one held under its
// own name (no image:, only devopsy.held) is no less held because a
// pulled one beside it gets a hold by digest.
func TestDataLastResortKeepsMarks(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	search := func(v string) Service {
		return Service{Name: "search", Ref: "app-prod-search", Build: true, Data: true, Method: CopyVolumes, DataVersion: v}
	}
	d.publish("mariadb:11.8", "I1")
	r1, _ := m.release(false, db118, search("8"))
	r2, _ := m.failRun(false, db118, search("9"))
	if heldIn(m.e.rel(r1))["search"] != r2 {
		m.fail("setup: search held %v", heldIn(m.e.rel(r1)))
	}
	d.publish("mariadb:12.1", "J1")
	r3 := m.e.release(dbSvc("mariadb:12.1"), search("9"))
	m.e.set.Check(m.e.rel(r3))
	m.fetchID(r3)
	m.e.set.Record(m.e.rel(r3), m.prev())
	m.cur = r3
	m.up()
	m.e.set.Ran(m.base(), m.e.rel(r3))
	m.e.set.LastHold(m.e.rel(r1), m.e.rel(r3))
	// search's data last ran r3's build: held on that, by its bookmark.
	if imgs, _, held := composeFacts(m.e.rel(r1)); imgs["database"] != "mariadb@J1" || held["database"] != r3 || held["search"] != r3 || !isBookmark(imgs["search"]) {
		m.fail("the last resort's compose file: images %v, held %v", imgs, held)
	}
}

// The last resort gives a held pulled service its own image back in the
// shape every other derivation writes for an unheld one: its own
// digest's label, no image: (compose runs its name) and no mark.
func TestDataLastResortUnholds(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	r1, _ := m.release(false, db118)
	d.publish("mariadb:12.1", "J1")
	m.failRun(false, dbSvc("mariadb:12.1"))
	if heldIn(m.e.rel(r1))["database"] == "" {
		m.fail("setup: database not held")
	}
	// Accepted back on 11.8, run, then its run step failed and get_images
	// too: the data's window allows r1's own again.
	m.e.set.Accept = map[string]bool{"database": true}
	r3 := m.e.release(db118)
	if err := m.e.set.Check(m.e.rel(r3)); err != nil {
		m.fail("r3: %v", err)
	}
	m.fetchID(r3)
	m.e.set.Record(m.e.rel(r3), m.prev())
	m.e.set.Accept = nil
	m.cur = r3
	m.up()
	m.e.set.Ran(m.base(), m.e.rel(r3))
	if err := m.e.set.LastHold(m.e.rel(r1), m.e.rel(r3)); err != nil {
		m.fail("the last resort: %v", err)
	}
	imgs, labels, held := composeFacts(m.e.rel(r1))
	if imgs["database"] != "mariadb:11.8@I1" || held["database"] != "" || labels["database"] != "mariadb@I1" || !strings.Contains(out.String(), "database runs "+r1+"'s own image again") {
		m.fail("devopsy's compose file: images %v, labels %v, held %v", imgs, labels, held)
	}
	m.cur = r1
	m.up()
	if m.runs("database") != "I1" {
		m.fail("runs %s, not r1's own", m.runs("database"))
	}
}

// The hold survives a fetch: the restart whose own image is gone builds
// again (ErrGone, then --fetched), which moves a built database's name to
// the previous release's build, and the hold runs the image its data ran
// by the release's bookmark of it.
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
	if m.runs("database") != want || want == "" || fileImage(d, m.e.rel(r1), "database", "app-prod-database") != want {
		m.fail("database runs %s, its file %s, want %s's %s", m.runs("database"), fileImage(d, m.e.rel(r1), "database", "app-prod-database"), r2, want)
	}
}

// The classic image store: a built image has no registry digest, so its
// hold goes by the release's bookmark of it, never a digest; and a failed
// release that built the service over a previous one that pulls it holds
// by the previous release's bookmark in the build's repository, the pulled
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
	if text, _ := os.ReadFile(filepath.Join(m.e.rel(r1), ComposeFile)); !strings.Contains(string(text), "\"database\":\n    image: \""+imgs["database"]+"\"\n    build: !reset null\n") {
		// Held: no build of the project's code lands on the image it is held on.
		t.Errorf("the held database's build is not reset:\n%s", text)
	}
	if !isBookmark(imgs["database"]) || fileImage(d, m.e.rel(r1), "database", "") != failed.ID || labels["database"] != failed.ID || held["database"] != r2 || m.runs("database") != failed.ID {
		t.Errorf("classic store: image %q, label %q, held %q, runs %s, want %s", imgs["database"], labels["database"], held["database"], m.runs("database"), failed.ID)
	}

	p := newSim(t, d, "app-stage", &out)
	d.publish("mariadb:11.8", "I1")
	s1, _ := p.release(false, db118)
	s2, _ := p.failRun(false, builtDB("app-stage", "12.1"))
	built, _ := match(readList(p.e.rel(s2)), entry{Service: "database"})
	imgs, _, _ = composeFacts(p.e.rel(s1))
	if RepoOf(imgs["database"]) != "app-stage-database" || !isBookmark(imgs["database"]) || d.names["mariadb:11.8"] != "I1" || p.runs("database") != built.ID {
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
	m.fetchID(r2)
	m.e.set.Record(m.e.rel(r2), m.prev())
	m.cur = r2
	m.e.set.Ran(m.base(), m.e.rel(r2)) // the old container: another label
	m.restart(r1, r2)
	if m.runs("database") != "I1" || len(heldIn(m.e.rel(r1))) > 0 {
		m.fail("held %v, runs %s", heldIn(m.e.rel(r1)), m.runs("database"))
	}
}

// A hold outlives a later failed release whose database never ran: the
// rule alone decides, so the restart keeps the earlier hold (its entry,
// bookmark and HeldLabel as they were) whether img ran found the
// release before's container or none (review: the window's last line
// was the earlier failed release's, so nothing was held and 11.8 ran on
// the 12.1 data). A built database likewise.
func TestDataRestartHoldSurvivesFailures(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.strict = true
	d.publish("mariadb:11.8", "I1")
	r1, _ := m.release(false, db118)
	d.publish("mariadb:12.1", "J1")
	r2, err := m.failRun(false, dbSvc("mariadb:12.1"))
	if err != nil || m.runs("database") != "J1" {
		m.fail("r2 (%v): database %s", err, m.runs("database"))
	}
	for _, gone := range []bool{false, true} {
		d.publish("mariadb:12.1", fmt.Sprintf("J%d", map[bool]int{false: 2, true: 3}[gone]))
		if _, _, err := m.failEarly(false, gone, dbSvc("mariadb:12.1")); err != nil {
			m.fail("a failed release (gone %v): %v", gone, err)
		}
		imgs, _, held := composeFacts(m.e.rel(r1))
		if m.runs("database") != "J1" || imgs["database"] != "mariadb@J1" || held["database"] != r2 {
			m.fail("after a failed release whose database never ran (no container: %v): database runs %s (want J1, the earlier hold), images %v, held %v", gone, m.runs("database"), imgs, held)
		}
		es := readList(m.e.rel(r1))
		for i, b := range bookmarks(es, m.e.set.Name, r1) {
			if got, _ := d.ID(b); es[i].Service == "database" && (es[i].ID != "J1" || es[i].DataVersion != "12.1" || got != "J1") {
				t.Errorf("r1's listing %+v, its bookmark %s on %s", es[i], b, got)
			}
		}
	}
	if l, _ := readHistory(m.base()).last("database"); l.ID != "J1" || l.Version != "12.1" {
		t.Errorf("the history ends on %+v", l)
	}

	p := newSim(t, d, "app-stage", &out)
	p.strict = true
	s1, _ := p.release(false, builtDB("app-stage", "11.8"))
	s2, _ := p.failRun(false, builtDB("app-stage", "12.1"))
	built, _ := match(readList(p.e.rel(s2)), entry{Service: "database"})
	if _, _, err := p.failEarly(false, false, builtDB("app-stage", "12.1")); err != nil {
		p.fail("a failed release: %v", err)
	}
	if p.runs("database") != built.ID || heldIn(p.e.rel(s1))["database"] != s2 || fileImage(d, p.e.rel(s1), "database", "") != built.ID {
		p.fail("built: database runs %s, its file %s, held %v, want %s's %s", p.runs("database"), fileImage(d, p.e.rel(s1), "database", ""), heldIn(p.e.rel(s1)), s2, built.ID)
	}
}

// A rollback (accepted) to a release the restart held on a failed
// release's build runs its own build, built again, never the held image
// its bookmark holds: the history records what then runs (review: it ran
// the failed release's 12.1 build as its own, recorded as 11.8).
func TestDataRollbackToHeld(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	r1, _ := m.release(false, builtDB("app-prod", "11.8"))
	r2, _ := m.failRun(false, builtDB("app-prod", "12.1"))
	held, _ := match(readList(m.e.rel(r2)), entry{Service: "database"})
	if m.runs("database") != held.ID {
		m.fail("the restart runs %s, not %s's %s", m.runs("database"), r2, held.ID)
	}
	if _, err := m.release(false, builtDB("app-prod", "12.1")); err != nil {
		m.fail("fix forward: %v", err)
	}
	if err := m.rollback(r1, true); err != nil {
		m.fail("the accepted rollback to %s: %v", r1, err)
	}
	e, _ := match(readList(m.e.rel(r1)), entry{Service: "database"})
	if m.runs("database") == held.ID || m.runs("database") != e.ID || e.held() || e.DataVersion != "11.8" {
		m.fail("the rollback to %s runs %s (%s's held build %s), listing %+v", r1, m.runs("database"), r2, held.ID, e)
	}
	if l, _ := readHistory(m.base()).last("database"); l.ID != e.ID || l.Version != "11.8" || l.How != Forced {
		t.Errorf("the history ends on %+v, not %s's own build", l, r1)
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
	m.fetchID(id)
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
	m.fetchID(id)
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

// --status names a service running another image than its activation gave
// it: a plain up runs the pin whatever the name holds since, so only
// something outside devopsy's file does that (an override's image:, a
// container made by hand under the label); nothing is held by digest.
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
	if got := statusTexts(StatusLines(d, "app-stage", stage.e.rel(stage.cur))); len(got) != 0 {
		t.Errorf("an up after the name moved: %v", got)
	}
	for i, c := range d.containers {
		if c.service == "alwaysup" {
			d.containers[i].image = "C2"
		}
	}
	lines := StatusLines(d, "app-stage", stage.e.rel(stage.cur))
	got := statusTexts(lines)
	want := []string{"alwaysup runs caddy@C2, not caddy@C1 its activation gave it: something outside devopsy's compose file made it"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("status lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// An image moved under a service needs attention: --status warns.
	if len(lines) != 1 || !lines[0].Warn {
		t.Errorf("a moved image is not warned: %+v", lines)
	}
}

// statusTexts are lines' texts.
func statusTexts(lines []StatusLine) []string {
	var texts []string
	for _, l := range lines {
		texts = append(texts, l.Text)
	}
	return texts
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
	got := statusTexts(StatusLines(d, "app-prod", rel))
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
// stops (CheckLanding), and, the copy complete, its history taken over
// (Land), then the child's run step on its own images.
func syncInto(t *testing.T, parent, child *sim, yes bool) error {
	t.Helper()
	data := parent.e.set.ParentData(parent.base(), parent.e.rel(parent.cur))
	if yes {
		child.e.set.Accept = acceptAll(child.e.svcs[child.cur])
	}
	defer func() { child.e.set.Accept = nil }()
	rel := child.e.rel(child.cur)
	if err := child.e.set.CheckLanding(rel, child.e.set.SyncLanding(data, "")); err != nil {
		return err
	}
	if err := child.e.set.Land(child.base(), rel, child.e.set.SyncLanding(data, "vm1:prod")); err != nil {
		return err
	}
	child.up()
	return child.e.set.Ran(child.base(), rel)
}

// A label from an inherited variable never lags one sync: the check and
// the listing img ran records go by the labels as they read now (in the
// holder, with the sync's pending inherited variables), never the
// version the child's last activation listed; a held service keeps its
// held image's.
func TestDataSyncInheritedVersion(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	d.publish("mariadb:12.1", "J1")
	prod.release(false, dbSvc("mariadb:12.1"))
	dev.release(false, builtDB("app-dev", "11.8"))
	// The parent's inherit: raised the child's label, its image the same.
	dev.e.svcs[dev.cur][0].DataVersion = "12.1"
	out.Reset()
	if err := syncInto(t, prod, dev, false); err != nil {
		dev.fail("a sync at the version the inherited label gives: %v", err)
	}
	if e, _ := match(readList(dev.e.rel(dev.cur)), entry{Service: "database"}); e.DataVersion != "12.1" {
		t.Errorf("the listing after the sync: %+v", e)
	}
	if l, _ := readHistory(dev.base()).last("database"); l.Version != "12.1" {
		t.Errorf("the history ends on %+v, not the label's version", l)
	}
	// An inherited variable that makes a service data in the same sync:
	// its labels decide, as for any: checked against the parent's window,
	// its listing and history following (review round 3: nothing checked
	// or recorded it, and the next release adopted its label on the copy).
	qa := newSim(t, d, "app-qa", &out)
	plain := dbSvc("mariadb:12.1")
	plain.Data, plain.DataVersion = false, ""
	qa.release(false, plain)
	qa.e.svcs[qa.cur][0] = dbSvc("mariadb:12.1")
	qa.e.svcs[qa.cur][0].DataVersion = "11.8"
	qa.e.set.Address = "vm1:qa"
	out.Reset()
	if err := syncInto(t, prod, qa, false); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "devopsy.data.version=11.8 is lower than the parent's data's version, 12.1") {
		qa.fail("a sync into a service an inherited variable makes data, at a lower version (%v)", err)
	}
	if h := readHistory(qa.base()); len(h) > 0 {
		t.Errorf("the refused sync wrote a history: %v", h)
	}
	qa.e.svcs[qa.cur][0].DataVersion = "12.1"
	if err := syncInto(t, prod, qa, false); err != nil {
		qa.fail("a sync into a service an inherited variable makes data: %v", err)
	}
	if e, _ := match(readList(qa.e.rel(qa.cur)), entry{Service: "database"}); e.DataVersion != "12.1" || e.Tag != "12.1" {
		t.Errorf("the listing after the sync: %+v", e)
	}
	if l, _ := readHistory(qa.base()).last("database"); l.Version != "12.1" {
		t.Errorf("the history ends on %+v", l)
	}
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
	prod.e.set.Copied = func(string, bool) ([]string, error) { return nil, nil }
	if data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur)); data != "" {
		t.Errorf("ParentData of a service whose entries are missing: %q", data)
	}
}

// A custom copy carries no image information, as a custom import does:
// the dump is loaded by the image that runs, so the data's files are that
// image's, never the parent's version. The parent sends nothing for it
// (nor for a copy of none), the child checks nothing, and its window and
// hold stay as they were: a held child stays held on the image that
// loaded the dump (review round 3: the parent's 11.8 window was taken
// over, the hold dropped, and 11.8 ran on the datadir 12.1 had written).
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
	d.publish("mariadb:11.8", "I1")
	prod.release(false, custom("mariadb:11.8"))
	if data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur)); data != "" {
		t.Errorf("ParentData of a custom copy: %q", data)
	}
	// dev held on 12.1, its own 11.8.
	dev.release(false, custom("mariadb:11.8"))
	d.publish("mariadb:12.1", "J1")
	dev.failRun(false, custom("mariadb:12.1"))
	if dev.runs("database") != "J1" || heldIn(dev.e.rel(dev.cur))["database"] == "" {
		dev.fail("setup: dev runs %s, held %v", dev.runs("database"), heldIn(dev.e.rel(dev.cur)))
	}
	history := strings.Join(dev.history("database"), " ")
	compose, _ := os.ReadFile(filepath.Join(dev.e.rel(dev.cur), ComposeFile))
	out.Reset()
	if err := syncInto(t, prod, dev, false); err != nil {
		dev.fail("a custom copy of 11.8 data into the held dev: %v", err)
	}
	if got := strings.Join(dev.history("database"), " "); got != history {
		t.Errorf("the custom copy changed the history: %s, was %s", got, history)
	}
	if c, _ := os.ReadFile(filepath.Join(dev.e.rel(dev.cur), ComposeFile)); string(c) != string(compose) || dev.runs("database") != "J1" {
		dev.fail("the custom copy changed the hold: runs %s\n%s", dev.runs("database"), c)
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
// labels against it and takes it over from an import line; a tar
// without the record starts the history over with a line of no version;
// --resolved keeps a window an import took over since its mark, else
// starts over. (A custom import calls none of these: TestImportResolved.)
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
	if err := stage.e.set.CheckLanding(rel, stage.e.set.ImportLanding(data, "database")); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "this environment would run the export's data on an image its version does not allow: nothing imported") || !strings.Contains(out.String(), "--import database --accept-image database") {
		stage.fail("an import onto a lower version (%v)", err)
	}
	// Accepted, its history over from its own image; --resolved after it
	// (the data in, a later step failed) keeps that start, never a line of
	// no version the next release would adopt any label over.
	mark := time.Now().Add(-time.Minute)
	stage.e.set.Accept = map[string]bool{"database": true}
	if err := stage.e.set.CheckLanding(rel, stage.e.set.ImportLanding(data, "database")); err != nil {
		stage.fail("an accepted import: %v", err)
	}
	stage.e.set.Land(stage.base(), rel, stage.e.set.ImportLanding(data, "database"))
	stage.e.set.Accept = nil
	if err := stage.e.set.Land(stage.base(), rel, stage.e.set.ResolvedLanding("database", mark)); err != nil || strings.Join(stage.history("database"), " ") != "forced:I1:11.8" {
		t.Errorf("--resolved after an accepted import (%v): %v", err, stage.history("database"))
	}
	stage.release(false, dbSvc("mariadb:12.1"))
	rel = stage.e.rel(stage.cur)
	if err := stage.e.set.CheckLanding(rel, stage.e.set.ImportLanding(data, "database")); err != nil {
		stage.fail("at its version: %v", err)
	}
	before := time.Now().Add(-time.Hour)
	stage.e.set.Land(stage.base(), rel, stage.e.set.ImportLanding(data, "database"))
	if got := strings.Join(stage.history("database"), " "); got != "import:J1:12.1" {
		t.Errorf("history after the import: %s", got)
	}
	if err := stage.e.set.Land(stage.base(), rel, stage.e.set.ResolvedLanding("database", before)); err != nil || strings.Join(stage.history("database"), " ") != "import:J1:12.1" {
		t.Errorf("--resolved after a complete import (%v): %v", err, stage.history("database"))
	}
	if err := stage.e.set.Land(stage.base(), rel, stage.e.set.ImportLanding("", "database")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stage.history("database"), " "); got != "import::" {
		t.Errorf("history after an import without a record: %s", got)
	}
	if _, err := stage.release(false, db118); err != nil {
		stage.fail("a version after an import of none is adopted: %v", err)
	}
	// A service its current release lists without data, which its labels
	// make data now (a variable set since): its labels decide, as for any:
	// the export's window judged, with a record, its listing and history
	// following (review round 4: nothing was checked, and the next release
	// adopted its label on the imported data).
	qa := newSim(t, d, "app-qa", &out)
	plain := dbSvc("mariadb:11.8")
	plain.Data, plain.DataVersion = false, ""
	qa.release(false, plain)
	qa.e.svcs[qa.cur][0] = db118
	qa.e.set.Address = "vm1:qa"
	out.Reset()
	if err := qa.e.set.CheckLanding(qa.e.rel(qa.cur), qa.e.set.ImportLanding(data, "database")); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "devopsy.data.version=11.8 is lower than the export's data's version, 12.1") {
		qa.fail("an import into a service made data since its release, at a lower version: %v", err)
	}
	if err := qa.e.set.CheckLanding(qa.e.rel(qa.cur), qa.e.set.ImportLanding("", "database")); err != nil {
		qa.fail("an import of no record into a service made data since its release: %v", err)
	}
	if err := qa.e.set.Land(qa.base(), qa.e.rel(qa.cur), qa.e.set.ImportLanding("", "database")); err != nil {
		qa.fail("its landing: %v", err)
	}
	if e, _ := match(readList(qa.e.rel(qa.cur)), entry{Service: "database"}); e.DataVersion != "11.8" {
		t.Errorf("the listing after the import: %+v", e)
	}
	// Locally: the label's version and tag, as one line.
	local := &Set{Docker: d, Name: "app-local", Out: &out, List: func(string) ([]Service, error) { return []Service{db118}, nil }}
	p := parseParentData(DataFor(local.ExportData("", "", "database"), "database"))["database"]
	if p == nil || len(p.History) != 1 || p.History[0].Version != "11.8" || p.History[0].Tag != "11.8" {
		t.Errorf("a local export's record: %+v", p)
	}
}

// A sync or an import of lower-version data into an environment the
// restart holds runs its release's own image on the copy, the hold gone:
// the hold is derived from the history, which the copy's window replaced
// (it ran the held 12.1 image on the parent's 11.8 copy before). A copy
// without a record of its history (an import of no header) starts it over,
// which drops the hold too, as a reset's wipe does. One whose own build is gone is refused before
// anything stops: a copy builds nothing.
func TestDataCopyDropsHold(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	prod := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	prod.release(false, db118)
	held := func(m *sim) (string, Service) {
		t.Helper()
		m.release(false, db118)
		d.publish("mariadb:12.1", "J1")
		m.failRun(false, dbSvc("mariadb:12.1"))
		if m.runs("database") != "J1" || heldIn(m.e.rel(m.cur))["database"] == "" {
			m.fail("setup: %s runs %s, held %v", m.e.set.Name, m.runs("database"), heldIn(m.e.rel(m.cur)))
		}
		return m.cur, db118
	}
	dropped := func(m *sim, what string) {
		t.Helper()
		rel := m.e.rel(m.cur)
		imgs, _, h := composeFacts(rel)
		e, _ := match(readList(rel), entry{Service: "database"})
		if m.runs("database") != "I1" || imgs["database"] != "mariadb:11.8@I1" || h["database"] != "" || e.held() || e.ID != "I1" || e.DataVersion != "11.8" {
			m.fail("after %s: runs %s, compose %v %v, listing %+v", what, m.runs("database"), imgs, h, e)
		}
		if !strings.Contains(out.String(), "database runs "+m.cur+"'s own image again") {
			m.fail("after %s: the hold's end is not said", what)
		}
	}

	stage := newSim(t, d, "app-stage", &out)
	held(stage)
	out.Reset()
	if err := syncInto(t, prod, stage, false); err != nil {
		stage.fail("the sync of 11.8 data into the held stage: %v", err)
	}
	dropped(stage, "the sync")
	if l, _ := readHistory(stage.base()).last("database"); l.ID != "I1" || l.Version != "11.8" {
		t.Errorf("stage's history ends on %+v", l)
	}

	dev := newSim(t, d, "app-dev", &out)
	held(dev)
	data := DataFor(prod.e.set.ExportData(prod.base(), prod.e.rel(prod.cur), "database"), "database")
	out.Reset()
	if err := dev.e.set.CheckLanding(dev.e.rel(dev.cur), dev.e.set.ImportLanding(data, "database")); err != nil {
		dev.fail("the import of 11.8 data into the held dev: %v", err)
	}
	if err := dev.e.set.Land(dev.base(), dev.e.rel(dev.cur), dev.e.set.ImportLanding(data, "database")); err != nil {
		dev.fail("the landing: %v", err)
	}
	dev.up()
	dropped(dev, "the import")

	test := newSim(t, d, "app-test", &out)
	held(test)
	out.Reset()
	if err := test.e.set.CheckLanding(test.e.rel(test.cur), test.e.set.ImportLanding("", "database")); err != nil {
		test.fail("the import of no record into the held test: %v", err)
	}
	if err := test.e.set.Land(test.base(), test.e.rel(test.cur), test.e.set.ImportLanding("", "database")); err != nil {
		test.fail("Imported: %v", err)
	}
	test.up()
	dropped(test, "the import of no record")

	// An import whose take-over was written but whose holds could not be
	// derived (its own name gone, the pull failing) keeps the hold until
	// --resolved derives them (review: it found the history taken over and
	// derived nothing, so the held image started on the imported data).
	uat := newSim(t, d, "app-uat", &out)
	held(uat)
	mark := time.Now().Add(-time.Minute)
	delete(d.names, "mariadb:11.8")
	d.fail = map[string]bool{"Pull": true}
	if err := uat.e.set.Land(uat.base(), uat.e.rel(uat.cur), uat.e.set.ImportLanding(data, "database")); err == nil || !strings.Contains(err.Error(), "what runs on it could not be derived from its history") {
		uat.fail("the landing with the pull failing: %v", err)
	}
	if heldIn(uat.e.rel(uat.cur))["database"] == "" {
		uat.fail("setup: the hold went with the pull failing")
	}
	out.Reset()
	if err := uat.e.set.Land(uat.base(), uat.e.rel(uat.cur), uat.e.set.ResolvedLanding("database", mark)); err != nil {
		uat.fail("--resolved: %v", err)
	}
	uat.up()
	dropped(uat, "--resolved")

	// The same with no record and for a reset's wipe: the history started
	// over, its holds not derived, says so, never that the history was not
	// written (review round 4).
	for i, start := range []func(m *sim) error{
		func(m *sim) error {
			return m.e.set.Land(m.base(), m.e.rel(m.cur), m.e.set.ImportLanding("", "database"))
		},
		func(m *sim) error { return m.e.set.Land(m.base(), m.e.rel(m.cur), m.e.set.ResetLanding()) },
	} {
		m := newSim(t, d, fmt.Sprintf("app-gone%d", i), &out)
		held(m)
		delete(d.names, "mariadb:11.8")
		d.fail = map[string]bool{"Pull": true}
		if err := start(m); err == nil || !strings.Contains(err.Error(), "what runs on it could not be derived from its history") || strings.Join(m.history("database"), " ") != "import::" {
			m.fail("a start over with the pull failing: %v, history %v", err, m.history("database"))
		}
		d.fail = nil
	}

	// A reset's wipe starts every history over: the hold goes with it.
	ci := newSim(t, d, "app-ci", &out)
	held(ci)
	out.Reset()
	if err := ci.e.set.Land(ci.base(), ci.e.rel(ci.cur), ci.e.set.ResetLanding()); err != nil {
		ci.fail("the wipe's landing: %v", err)
	}
	ci.up()
	dropped(ci, "the wipe")

	// Built: its own build gone while held, a sync and an import refuse
	// before anything stops.
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
	out.Reset()
	if err := qa.e.set.CheckLanding(qa.e.rel(qa.cur), qa.e.set.SyncLanding(prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur)), "")); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "a sync builds nothing: nothing changed") || !strings.Contains(out.String(), "devopsy @vm1:qa --release-sync\n") {
		qa.fail("a sync into a held qa whose own build is gone (%v)", err)
	}
	out.Reset()
	if err := qa.e.set.CheckLanding(qa.e.rel(qa.cur), qa.e.set.ImportLanding("", "database")); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "an import builds nothing: nothing imported") {
		qa.fail("an import into a held qa whose own build is gone (%v)", err)
	}
	// A reset starts every history over: refused alike, whatever the
	// parent copies.
	qa.e.set.Reset = true
	out.Reset()
	if err := qa.e.set.CheckLanding(qa.e.rel(qa.cur), qa.e.set.SyncLanding("", "")); !errors.Is(err, ErrRefused) {
		qa.fail("a reset of a held qa whose own build is gone (%v)", err)
	}
}

// An import reads its service's labels as a sync and a --no-code restore
// do, and refuses, as they do, one whose image they name otherwise than
// its release does (a variable set since): only a release fetches that
// image, and the listing's would run on data checked for another.
func TestDataImportImageChanged(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Address = "vm1:prod"
	d.publish("mariadb:11.8", "I1")
	m.release(false, db118)
	data := DataFor(m.e.set.ExportData(m.base(), m.e.rel(m.cur), "database"), "database")
	m.e.svcs[m.cur][0] = dbSvc("mariadb:12.1")
	out.Reset()
	if err := m.e.set.CheckLanding(m.e.rel(m.cur), m.e.set.ImportLanding(data, "database")); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "devopsy @vm1:prod --release\n") {
		m.fail("an import with its image changed since its release (%v)", err)
	}
}

// The restart's last resort judges a data service by its labels now, as
// Restore does for the restart it replaces: a label raised since its
// release (a variable, its image's name the same) is what the run step
// runs under, which the data allows.
func TestDataLastResortReadsLabels(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	r1, _ := m.release(false, db118)
	d.publish("mariadb:12.1", "J1")
	r2, _ := m.failRun(false, dbSvc("mariadb:12.1"))
	raised := db118
	raised.DataVersion, raised.Ref = "12.1", "mariadb:12.1"
	m.e.svcs[r1] = []Service{raised}
	if err := m.e.set.LastHold(m.e.rel(r1), m.e.rel(r2)); err != nil {
		m.fail("the last resort: %v", err)
	}
	if h := heldIn(m.e.rel(r1))["database"]; h != "" {
		t.Errorf("held on %s, its labels allowing it", h)
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
	if err := m.e.set.CheckLanding(m.e.rel(m.cur), m.e.set.RestoreLanding(snap, "s1", nil)); err != nil {
		m.fail("12.1 on the snapshot's 11.8 data: %v", err)
	}
	snap = m.e.set.SnapshotData(m.base(), m.e.rel(m.cur))
	m.release(false, dbSvc("mariadb:12.1"))
	m.release(true, db118)
	out.Reset()
	if err := m.e.set.CheckLanding(m.e.rel(m.cur), m.e.set.RestoreLanding(snap, "s2", nil)); !errors.Is(err, ErrRefused) || !strings.Contains(out.String(), "--snapshots restore s2 --no-code --accept-image database") {
		m.fail("11.8 on the snapshot's 12.1 data (%v)", err)
	}
	// A current release listing it without data, its labels making it
	// data now: judged by them, as an import.
	plain := dbSvc("mariadb:12.1")
	plain.Data, plain.DataVersion = false, ""
	m.release(false, plain)
	m.e.svcs[m.cur][0] = dbSvc("mariadb:12.1")
	if err := m.e.set.CheckLanding(m.e.rel(m.cur), m.e.set.RestoreLanding(snap, "s2", nil)); err != nil {
		m.fail("a --no-code restore into a service made data since its release: %v", err)
	}
}

// A hold says which release ran its image: one that only failed a fetch of
// it lists it too, and never ran it.
func TestDataHoldNamesWhatRan(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	m.release(false, db118)
	d.publish("mariadb:12.1", "J1")
	r2, _ := m.release(false, dbSvc("mariadb:12.1"))
	// A later release fetched the same image, and failed.
	r3 := m.e.release(dbSvc("mariadb:12.1"))
	m.fetchID(r3)
	if err := m.e.set.Record(m.e.rel(r3), m.prev()); err != nil {
		m.fail("record: %v", err)
	}
	os.WriteFile(filepath.Join(m.e.rel(r3), failedFile), nil, 0o644)
	_, from := m.e.set.source(m.e.rel(r2), "database", ran{ID: "J1", Version: "12.1", Tag: "12.1"})
	// r2 is the target here, so only r3 lists it; a target of another
	// release must find r2 first.
	if from != r3 {
		m.fail("setup: %s", from)
	}
	r4 := m.e.release(db118)
	if _, from := m.e.set.source(m.e.rel(r4), "database", ran{ID: "J1", Version: "12.1", Tag: "12.1"}); from != r2 {
		t.Errorf("the hold names release %s, not %s, which ran it", from, r2)
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
	searchOwn := m.runs("search")
	before := m.snapshot("20261102000000")
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
	lines := strings.Join(statusTexts(StatusLines(d, "app-prod", rel)), "\n")
	if !strings.Contains(lines, "database held on mariadb@J1, the image its data ran when snapshot 20261102000001 was taken") || !strings.Contains(lines, "search held on app-prod-search (") {
		t.Errorf("--status:\n%s", lines)
	}
	// The held data alone: the rule still refuses the release's own on it,
	// so its holds stay.
	if err := m.noCode(snap); err != nil || m.runs("database") != "J1" || heldIn(rel)["database"] == "" || heldIn(rel)["search"] == "" {
		m.fail("the --no-code restore of the held data (%v): database runs %s, held %v", err, m.runs("database"), heldIn(rel))
	}
	// The data from before the hold, alone: the held images would upgrade
	// it again, so the restored release's own run on it, its holds gone
	// (review: the hold stayed, 12.1 on the restored 11.8 data).
	out.Reset()
	if err := m.noCode(before); err != nil {
		m.fail("the --no-code restore of the data from before the hold: %v", err)
	}
	if m.runs("database") != "I1" || m.runs("search") != searchOwn || len(heldIn(rel)) > 0 {
		m.fail("after the --no-code restore: database runs %s (want I1), search %s (want %s), held %v", m.runs("database"), m.runs("search"), searchOwn, heldIn(rel))
	}
	for svc, v := range map[string]string{"database": "11.8", "search": "8"} {
		if e, _ := match(readList(rel), entry{Service: svc}); e.held() || e.DataVersion != v {
			t.Errorf("%s's listing after the --no-code restore: %+v", svc, e)
		}
		if l, _ := readHistory(m.base()).last(svc); l.Version != v {
			t.Errorf("%s's history ends on %+v, want %s", svc, l, v)
		}
	}
	if !strings.Contains(out.String(), "database runs "+id+"'s own image again") {
		m.fail("the hold's end is not said")
	}
}

// A --no-code restore that would run a held built service on its own
// image again, which is gone: refused before anything stops, a
// --no-code restore building nothing, the full restore named.
func TestDataNoCodeOwnBuildGone(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	m.release(false, builtDB("app-prod", "11.8"))
	own := m.runs("database")
	before := m.snapshot("20261102000000")
	m.failRun(false, builtDB("app-prod", "12.1"))
	held := m.runs("database")
	for n, id := range d.names {
		if id == own {
			delete(d.names, n)
		}
	}
	delete(d.images, own)
	out.Reset()
	if err := m.noCode(before); !errors.Is(err, ErrRefused) || m.runs("database") != held || !strings.Contains(out.String(), "a --no-code restore builds nothing: nothing restored") || !strings.Contains(out.String(), "devopsy @vm1:prod --snapshots restore 20261102000000\n") {
		m.fail("a --no-code restore whose own build is gone (%v): database runs %s", err, m.runs("database"))
	}
}

// Two environments release, fail in their run step after the database
// started or before (its container the one before, or none: restarted,
// as built now and then), fail in their fetch, roll back, sync from each
// other, import each other's exports, take and restore snapshots (whole
// or --no-code) and lose releases to pruning, their database pulled under
// several tags and versions, built, or labeled any, accepting now and
// then, while the registry moves the tags. A model of the data, kept by
// the test from what each database runs (the image its container runs,
// under the version and tag that image was fetched under, never devopsy's
// history or listings: review, a model of what each step meant to run
// passed a Restore retagging the release's own name from the leaving
// release's bookmark), says what the rule allows: devopsy refuses exactly
// what the model refuses unaccepted, a restart and a full restore hold
// exactly what the model refuses, and devopsy's window says the model's
// version. And every run the model records is checked against the
// model's window itself: no data ever runs a lower version, or another
// tag at its version, unaccepted. And every kept release's and snapshot's
// bookmarks hold what it lists; once all goes, nothing is left.
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

func dataRandom(t *testing.T, seed int64) { dataRandomMode(t, seed, ModeBuild) }

// dataRandomMode is dataRandom with every release in mode: in image mode
// a service with build: is pulled, the project's registry serving a new
// image under a commit tag per release (push), as CI pushes them.
func dataRandomMode(t *testing.T, seed int64, mode string) {
	rng := rand.New(rand.NewSource(seed))
	d := newFake()
	var out bytes.Buffer
	envs := []*sim{newSim(t, d, "app-prod", &out), newSim(t, d, "app-stage", &out)}
	left := filepath.Join(t.TempDir(), LeftFile)
	commit := 0
	for _, m := range envs {
		m.e.mode = mode
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
	// claimed is every claim (version and tag) each image was fetched
	// under in each environment, oldest first (the sim's fetch: a build's,
	// a pull's): what runs is judged by the image its container runs,
	// never by what the step meant to run.
	claimed := map[*sim]map[string][]Service{}
	for _, m := range envs {
		claimed[m] = map[string][]Service{}
		m.fetched = func(svc Service, id string) {
			if svc.Name == "database" && id != "" {
				claimed[m][id] = append(claimed[m][id], svc)
			}
		}
	}
	sameClaim := func(a, b Service) bool { return a.DataVersion == b.DataVersion && a.tag() == b.tag() }
	// actual is the claim m's database runs the image img under, pref
	// being what the step meant to run: pref when it pulls img by its name
	// (a pulled name runs what it holds now, under the running release's
	// label) or img was fetched under it; else the newest claim img was
	// fetched under (another release's image: a hold's, or one a bookmark
	// gave in place of the release's own build), one the data's window
	// allows first: a hold names a line the window has, and another release
	// may have failed a fetch of the same image under a lower claim (seed
	// 16423); else pref (an image no fetch got: a gone name pulled again).
	actual := func(m *sim, img string, pref Service) Service {
		if !pref.Build && img != "" && d.names[familiar(pref.Ref)] == img {
			return pref
		}
		cs := claimed[m][img]
		for _, c := range cs {
			if sameClaim(c, pref) {
				return pref
			}
		}
		for i := len(cs) - 1; i >= 0; i-- {
			if !verdict(model[m], cs[i]) {
				return cs[i]
			}
		}
		if len(cs) > 0 {
			return cs[len(cs)-1]
		}
		return pref
	}
	// run is a run of the image img on m's data in the model, under the
	// claim it was fetched under (actual; pref, what the step meant to
	// run, which accepted accepts): the invariant, checked against the
	// model's window whatever devopsy said, then the line, a forced one
	// when accepted and refused.
	run := func(m *sim, img string, pref Service, accepted bool) {
		t.Helper()
		svc := actual(m, img, pref)
		accepted = accepted && sameClaim(svc, pref)
		refused := verdict(model[m], svc)
		if refused && !accepted {
			t.Fatalf("after %v: %s runs its database on %s as %s=%s (tag %q) on data its window refuses it on, unaccepted (the step meant %s=%s, tag %q)\nmodel %v\n%s", log, m.e.set.Name, img, DataVersionLabel, svc.DataVersion, svc.tag(), DataVersionLabel, pref.DataVersion, pref.tag(), model[m].window("database"), out.String())
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
	// copyNeedsGone says whether a copy into m that its check lets
	// through (ok: refused but accepted, or not refused; a refusal is the
	// model's) needs m's own build of the database back and it is gone: m
	// held, its own built (a copy builds nothing).
	copyNeedsGone := func(m *sim, own Service, _ bool) bool {
		rel := m.e.rel(m.cur)
		cur, _ := match(readList(rel), entry{Service: "database"})
		return heldIn(rel)["database"] != "" && own.own(m.e.mode) && !d.images[cur.OwnID]
	}
	svcs := func() []Service {
		var db Service
		if rng.Intn(5) == 0 {
			db = builtDB(m.e.set.Name, versions[rng.Intn(len(versions))])
			if mode == ModeImage {
				commit++
				db.Ref += fmt.Sprintf(":c%d", commit)
				push(db.Ref)
			}
		} else {
			db = dbSvc(refs[rng.Intn(len(refs))])
			if rng.Intn(3) == 0 {
				db.DataVersion = versions[rng.Intn(len(versions))]
			}
		}
		app := Service{Name: "app", Ref: m.e.set.Name + "-app", Build: true}
		if mode == ModeImage {
			commit++
			app.Ref += fmt.Sprintf(":c%d", commit)
			push(app.Ref)
		}
		return []Service{app, db, proxySvc}
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
				run(m, m.runs("database"), dbOf(ss), yes)
			}
			check(m.e.set.Name + " release " + id)
		case 3, 11: // a run step failing after its database ran, or before (its container the one before, or none), restarted
			if m.cur == "" {
				continue
			}
			prev, ss := m.cur, svcs()
			yes := rng.Intn(4) == 0
			refused := verdict(model[m], dbOf(ss))
			heldBefore, before := heldIn(m.e.rel(prev))["database"] != "", m.runs("database")
			early, gone := rng.Intn(2) == 0, rng.Intn(2) == 0
			n := len(m.ups)
			var id string
			var err error
			ran := true
			if early {
				id, ran, err = m.failEarly(yes, gone, ss...)
			} else {
				id, err = m.failRun(yes, ss...)
			}
			what := map[bool]string{false: "failed release ", true: "release failed early "}[early]
			if !expect(what+id, err, refused, yes) {
				check(m.e.set.Name + " refused " + id)
				continue
			}
			failedImg, _ := match(readList(m.e.rel(id)), entry{Service: "database"})
			if ran {
				img := before
				if !early {
					img = m.ups[n]
				}
				run(m, img, dbOf(ss), yes)
			}
			held := verdict(model[m], dbOf(m.e.svcs[prev]))
			if held != (heldIn(m.e.rel(prev))["database"] != "") {
				t.Fatalf("after %v: the restart of %s held %v, the model says %v\n%s", log, prev, heldIn(m.e.rel(prev)), held, out.String())
			}
			// Held on the failed release's image when it ran, else on the
			// hold before, else on the failed release's.
			pref, want := dbOf(m.e.svcs[prev]), ""
			if held {
				pref, want = dbOf(ss), failedImg.ID
				if !ran && heldBefore {
					pref, want = running[m], before
				}
			}
			if held && m.runs("database") != want {
				t.Fatalf("after %v: the restart of %s runs its database on %s, not %s\n%s", log, prev, m.runs("database"), want, out.String())
			}
			run(m, m.runs("database"), pref, false)
			check(m.e.set.Name + " " + what + id)
		case 4: // a fetch failing: undone
			id := m.e.release(svcs()...)
			if m.e.set.Check(m.e.rel(id)) == nil {
				m.fetchID(id)
			}
			m.e.set.Undo(m.e.rel(id), false)
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
				run(m, m.runs("database"), dbOf(m.e.svcs[target]), yes)
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
			// Judged by the release's own image, which runs on the copy
			// whether or not it was held (the hold, derived after, goes:
			// the copy's window is the parent's); refused before anything
			// stops when it is a build gone.
			db := dbOf(m.e.svcs[m.cur])
			refused := verdict(model[o], db)
			gone := copyNeedsGone(m, db, refused && yes)
			err := syncInto(t, o, m, yes)
			if gone {
				if !errors.Is(err, ErrRefused) {
					t.Fatalf("after %v: a sync into %s whose own build is gone: %v\n%s", log, m.e.set.Name, err, out.String())
				}
			} else if expect("sync from "+o.e.set.Name, err, refused, yes) {
				if yes && refused {
					model[m] = history{claim(db, Forced)}
				} else {
					w := append(history{}, model[o].window("database")...)
					if len(w) == 0 {
						w = history{{Service: "database"}}
					}
					w[0].How = SyncedFrom + "x"
					model[m] = w
				}
				if heldIn(m.e.rel(m.cur))["database"] != "" {
					t.Fatalf("after %v: a sync into %s left it held %v\n%s", log, m.e.set.Name, heldIn(m.e.rel(m.cur)), out.String())
				}
				run(m, m.runs("database"), db, yes && refused)
			}
			check(m.e.set.Name + " sync from " + o.e.set.Name)
		case 9: // a release that syncs the other's data, its run step going through or failing before its database ran
			o := other()
			if m.cur == "" || o.cur == "" {
				continue
			}
			prev, ss := m.cur, svcs()
			db := dbOf(ss)
			yes := rng.Intn(3) == 0
			refused := verdict(model[o], db)
			data := o.e.set.ParentData(o.base(), o.e.rel(o.cur))
			m.e.set.SyncData = data
			if yes {
				m.e.set.Accept = acceptAll(ss)
			}
			id := m.e.release(ss...)
			rel := m.e.rel(id)
			err := m.e.set.Check(rel)
			if !expect("release that syncs "+id, err, refused, yes) {
				os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
				m.e.set.SyncData, m.e.set.Accept = "", nil
				check(m.e.set.Name + " refused release that syncs " + id)
				continue
			}
			m.fetchID(id)
			if err := m.e.set.Record(rel, m.prev()); err != nil {
				t.Fatalf("after %v: record %s: %v\n%s", log, id, err, out.String())
			}
			m.cur = id
			if err := m.e.set.Land(m.base(), rel, m.e.set.SyncLanding(data, "vm1:"+o.e.set.Name)); err != nil {
				t.Fatalf("after %v: sync-done %s: %v\n%s", log, id, err, out.String())
			}
			m.e.set.SyncData, m.e.set.Accept = "", nil
			if yes && refused {
				model[m] = history{claim(db, Forced)}
			} else {
				w := append(history{}, model[o].window("database")...)
				if len(w) == 0 {
					w = history{{Service: "database"}}
				}
				w[0].How = SyncedFrom + "x"
				model[m] = w
			}
			// Failing before its database ran, the restart holds the release
			// before on the accepted release's own image, or on nothing; a
			// hold on the parent's lines, whose images may not be this
			// server's, is the accepted case docs/images.md names, left out.
			held := verdict(model[m], dbOf(m.e.svcs[prev]))
			if rng.Intn(2) == 0 || (held && !(yes && refused)) {
				m.up()
				m.e.set.Ran(m.base(), rel)
				run(m, m.runs("database"), db, false)
				check(m.e.set.Name + " release that syncs " + id)
				continue
			}
			before := m.runs("database")
			ran := false
			for _, c := range m.d.containers {
				if c.project == m.e.set.Name && c.service == "database" && c.digest != "" && c.digest == labelsIn(rel)["database"] {
					ran = true
				}
			}
			failedImg, _ := match(readList(rel), entry{Service: "database"})
			m.e.set.Ran(m.base(), rel)
			os.WriteFile(filepath.Join(rel, failedFile), nil, 0o644)
			if ran {
				run(m, before, db, false)
			}
			m.restart(prev, id)
			held = verdict(model[m], dbOf(m.e.svcs[prev]))
			if held != (heldIn(m.e.rel(prev))["database"] != "") {
				t.Fatalf("after %v: the restart of %s after a release that syncs held %v, the model says %v\n%s", log, prev, heldIn(m.e.rel(prev)), held, out.String())
			}
			pref := dbOf(m.e.svcs[prev])
			if held {
				if m.runs("database") != failedImg.ID {
					t.Fatalf("after %v: the restart of %s after the release that syncs %s runs its database on %s, not %s\n%s", log, prev, id, m.runs("database"), failedImg.ID, out.String())
				}
				pref = db
			}
			run(m, m.runs("database"), pref, false)
			check(m.e.set.Name + " release that syncs failed " + id)
		case 10: // a complete import of the other's export, or of none
			o := other()
			if m.cur == "" {
				continue
			}
			// Judged by the release's own image, as a sync; then what the
			// import stopped is made again on what its compose file gives it
			// now (restoreServices' recreate), which the next activation's
			// img ran records.
			db := dbOf(m.e.svcs[m.cur])
			after := func(accepted bool) {
				t.Helper()
				if heldIn(m.e.rel(m.cur))["database"] != "" {
					t.Fatalf("after %v: an import into %s left it held %v\n%s", log, m.e.set.Name, heldIn(m.e.rel(m.cur)), out.String())
				}
				m.up()
				m.e.set.Ran(m.base(), m.e.rel(m.cur))
				run(m, m.runs("database"), db, accepted)
			}
			if o.cur == "" || rng.Intn(3) == 0 {
				if copyNeedsGone(m, db, true) {
					if err := m.e.set.CheckLanding(m.e.rel(m.cur), m.e.set.ImportLanding("", "database")); !errors.Is(err, ErrRefused) {
						t.Fatalf("after %v: an import of no record into %s whose own build is gone: %v\n%s", log, m.e.set.Name, err, out.String())
					}
					check(m.e.set.Name + " import refused")
					continue
				}
				if err := m.e.set.CheckLanding(m.e.rel(m.cur), m.e.set.ImportLanding("", "database")); err != nil {
					t.Fatalf("after %v: an import of no record: %v\n%s", log, err, out.String())
				}
				if err := m.e.set.Land(m.base(), m.e.rel(m.cur), m.e.set.ImportLanding("", "database")); err != nil {
					t.Fatal(err)
				}
				model[m] = history{{Service: "database", How: Imported}}
				after(false)
				check(m.e.set.Name + " import")
				continue
			}
			data := DataFor(o.e.set.ExportData(o.base(), o.e.rel(o.cur), "database"), "database")
			yes := rng.Intn(3) == 0
			if yes {
				m.e.set.Accept = map[string]bool{"database": true}
			}
			refused := verdict(model[o], db)
			gone := copyNeedsGone(m, db, refused && yes)
			err := m.e.set.CheckLanding(m.e.rel(m.cur), m.e.set.ImportLanding(data, "database"))
			if gone {
				if !errors.Is(err, ErrRefused) {
					t.Fatalf("after %v: an import into %s whose own build is gone: %v\n%s", log, m.e.set.Name, err, out.String())
				}
			} else if expect("import of "+o.e.set.Name+"'s export", err, refused, yes) {
				m.e.set.Land(m.base(), m.e.rel(m.cur), m.e.set.ImportLanding(data, "database"))
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
				m.e.set.Accept = nil
				after(yes && refused)
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
					run(m, m.runs("database"), sn.running, false)
				} else {
					run(m, m.runs("database"), dbOf(sn.svcs), false)
				}
				check(m.e.set.Name + " full restore " + filepath.Base(sn.dir))
				continue
			}
			db := running[m]
			yes := rng.Intn(3) == 0
			if yes {
				m.e.set.Accept = map[string]bool{"database": true}
			}
			// Held, and the release's own allowed on the snapshot's data, or
			// accepted where the check refuses: it runs its own again (its
			// build there, else refused: a --no-code restore builds nothing).
			rel := m.e.rel(m.cur)
			own := dbOf(m.e.svcs[m.cur])
			cur, _ := match(readList(rel), entry{Service: "database"})
			held := heldIn(rel)["database"] != ""
			unhold := held && !verdict(sn.model, own)
			refused := !unhold && verdict(sn.model, db)
			if held && yes && refused {
				unhold, db = true, own
			}
			gone := unhold && own.own(m.e.mode) && !d.images[cur.OwnID]
			err := m.noCode(sn.dir)
			if gone {
				if !errors.Is(err, ErrRefused) {
					t.Fatalf("after %v: a --no-code restore of %s whose own build is gone: %v\n%s", log, filepath.Base(sn.dir), err, out.String())
				}
			} else if expect("--no-code restore of "+filepath.Base(sn.dir), err, refused, yes) {
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
				pref := db
				if unhold {
					pref = own
				}
				if unhold && heldIn(rel)["database"] != "" {
					t.Fatalf("after %v: the --no-code restore of %s left held %v, the model says held no more: %v\n%s", log, filepath.Base(sn.dir), heldIn(rel), unhold, out.String())
				}
				run(m, m.runs("database"), pref, false)
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

// Environments that build under one name (an image: the compose file gives its build, which
// every environment of it builds: stage, a branch's, a pull request's) each run what their own
// release built: its file pins each built service by its id, so a plain up, a sync's start or
// any recreate runs that image, whichever environment built the name last.
func TestBuiltImagesPinnedByID(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-web", Build: true}
	prod := newSim(t, d, "app-prod", &out)
	stage := newSim(t, d, "app-stage", &out)
	if _, err := prod.release(false, web); err != nil {
		t.Fatal(err)
	}
	was := prod.runs("web")
	// The stage's release builds under the same name: it moves.
	if _, err := stage.release(false, web); err != nil {
		t.Fatal(err)
	}
	if id, _ := d.ID("shop-web"); id == was {
		t.Fatalf("the control: the name still holds prod's build %s", was)
	}
	prod.up()
	if got := prod.runs("web"); got != was {
		t.Errorf("a plain up of prod runs %s, the stage's build, not its own %s", got, was)
	}
	if got := statusTexts(StatusLines(d, "app-prod", prod.e.rel(prod.cur))); len(got) != 0 {
		t.Errorf("status lines: %q", got)
	}
	// What runs another image now came from outside the file (an override's image:, a
	// container made by hand): no name moved under a service the file pins.
	stageBuild := stage.runs("web")
	for i, c := range d.containers {
		if c.project == "app-prod" && c.service == "web" {
			d.containers[i].image = stageBuild
		}
	}
	if got := statusTexts(StatusLines(d, "app-prod", prod.e.rel(prod.cur))); len(got) != 1 || !strings.HasSuffix(got[0], ": something outside devopsy's compose file made it") {
		t.Errorf("status lines, another image by hand: %q", got)
	}
}

// A rollback, and the restart after a failed run step, of a service with no data runs its
// release's own build by that release's bookmark, whatever the shared name holds: another
// environment building under it last (and running it) changes neither.
func TestRollbackRunsItsOwnBuildUnderASharedName(t *testing.T) {
	t.Parallel()
	web := Service{Name: "web", Ref: "shop-web", Build: true}
	for _, how := range []string{"rollback", "restart"} {
		d := newFake()
		var out bytes.Buffer
		prod := newSim(t, d, "app-prod", &out)
		stage := newSim(t, d, "app-stage", &out)
		r1, err := prod.release(false, web)
		if err != nil {
			t.Fatal(err)
		}
		own := prod.runs("web")
		if how == "rollback" {
			if _, err := prod.release(false, web); err != nil {
				t.Fatal(err)
			}
		}
		failed := ""
		if how == "restart" {
			// Prod's next release goes live, and its run step fails after the stage's
			// release built: restarted on r1.
			failed = prod.e.release(web)
			prod.fetchID(failed)
			if err := prod.e.set.Record(prod.e.rel(failed), prod.prev()); err != nil {
				t.Fatal(err)
			}
			prod.cur = failed
			prod.up()
		}
		if _, err := stage.release(false, web); err != nil {
			t.Fatal(err)
		}
		theirs := stage.runs("web")
		if how == "rollback" {
			if err := prod.rollback(r1, false); err != nil {
				prod.fail("rollback: %v", err)
			}
		} else {
			prod.strict = true
			prod.restart(r1, failed)
		}
		if got := prod.runs("web"); got != own {
			t.Errorf("%s to %s: prod's web runs %s (the stage's build is %s), not its own %s, which its bookmark still holds:\n%s", how, r1, got, theirs, own, out.String())
		}
		if got := fileImage(d, prod.e.rel(r1), "web", "shop-web"); got != own {
			imgs, _, _ := composeFacts(prod.e.rel(r1))
			t.Errorf("%s to %s: its compose file runs web on %s (%q), not its own build %s", how, r1, got, imgs["web"], own)
		}
	}
}

// A release records what its own fetch built, by the fetch's tag, never what the shared
// name holds: fetches of two environments run side by side, and the other's build under
// that name may finish between this one's build and its record.
func TestRecordTakesItsOwnFetchBesideAnother(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-web", Build: true}
	prod := newSim(t, d, "app-prod", &out)
	stage := newSim(t, d, "app-stage", &out)
	id := prod.e.release(web)
	prod.fetchID(id)
	built, _ := d.ID(fetchTags(prod.e.rel(id))["web"])
	// The stage's fetch builds under the same name before prod's record.
	stage.fetch([]Service{web})
	if err := prod.e.set.Record(prod.e.rel(id), ""); err != nil {
		t.Fatal(err)
	}
	prod.cur = id
	prod.up()
	if got := prod.runs("web"); got != built {
		t.Errorf("prod runs %s, the stage's build, not %s, what its own fetch built:\n%s", got, built, out.String())
	}
}

// A copy that lands (a sync, an import, a --no-code restore) that changes nothing of what
// runs leaves devopsy's file as it is: every built service by its release's bookmark, so
// compose, which recreates on another image: reference, restarts nothing it did not touch.
func TestLandingKeepsTheFileAsItIs(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-web", Build: true}
	d.publish("mariadb:11.8", "I1")
	m := newSim(t, d, "app-prod", &out)
	if _, err := m.release(false, web, db118); err != nil {
		t.Fatal(err)
	}
	rel := m.e.rel(m.cur)
	was, _ := os.ReadFile(filepath.Join(rel, ComposeFile))
	built := m.runs("web")
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	snap := m.snapshot("1")
	if err := m.noCode(snap); err != nil {
		m.fail("the restore: %v", err)
	}
	now, _ := os.ReadFile(filepath.Join(rel, ComposeFile))
	if got := fileImage(d, rel, "web", "shop-web"); got != built {
		imgs, _, _ := composeFacts(rel)
		t.Errorf("after a --no-code restore, web runs %s (%q), not its build %s", got, imgs["web"], built)
	}
	if string(now) != string(was) {
		t.Errorf("the landing changed nothing, and rewrote the compose file (compose recreates on another image: reference):\n%s\nwas:\n%s", now, was)
	}
}

// A hold under another name than the service's own (a failed release that built the
// database over a previous one that pulls it) goes by the holding release's bookmark of the
// image, never that name: another environment building under it (the stage's, a branch's)
// would move it, and prod's next plain up run that build on prod's data.
func TestHoldUnderAnotherNameByItsBookmark(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	d.publish("mariadb:11.8", "I1")
	built := func(version string) Service {
		return Service{Name: "database", Ref: "shop-db", Build: true, Data: true, Method: CopyVolumes, DataVersion: version}
	}
	prod := newSim(t, d, "app-prod", &out)
	r1, err := prod.release(false, db118)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prod.failRun(false, built("12.1")); err != nil {
		t.Fatal(err)
	}
	held := prod.runs("database")
	if heldIn(prod.e.rel(r1))["database"] == "" || held == "I1" {
		prod.fail("setup: %s is not held on the failed release's build: runs %s", r1, held)
	}
	stage := newSim(t, d, "app-stage", &out)
	if _, err := stage.release(false, built("12.4")); err != nil {
		t.Fatal(err)
	}
	prod.up()
	if got := prod.runs("database"); got != held {
		imgs, _, _ := composeFacts(prod.e.rel(r1))
		t.Errorf("a plain up of prod runs its 12.1 data on %s, the stage's 12.4 build, not %s it is held on: its compose file holds it by image: %q", got, held, imgs["database"])
	}
}

// Services sharing one build (web builds shop-app, worker runs the same image: with
// pull_policy: never) run their release's bookmark of it, every profile's included, and
// another environment building under the name changes neither: devopsy's builds write no
// project name either could follow.
func TestBuiltImageSharedByServices(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-app", Build: true}
	worker := Service{Name: "worker", Ref: "shop-app", NeverPull: true}
	tool := Service{Name: "tool", Ref: "shop-app", NeverPull: true}
	prod := newSim(t, d, "app-prod", &out)
	// tool is under a profile: every profile's list has it, the release's has not.
	prod.e.set.ListAll = func(rel string) ([]Service, error) {
		return append(slices.Clone(prod.e.svcs[filepath.Base(rel)]), tool), nil
	}
	stage := newSim(t, d, "app-stage", &out)
	if _, err := prod.release(false, web, worker); err != nil {
		t.Fatal(err)
	}
	own := prod.runs("web")
	if prod.runs("worker") != own {
		t.Fatalf("the worker runs %s, not web's build %s", prod.runs("worker"), own)
	}
	if _, err := stage.release(false, web, worker); err != nil {
		t.Fatal(err)
	}
	prod.up()
	rel := prod.e.rel(prod.cur)
	for _, svc := range []string{"web", "worker", "tool"} {
		if got := fileImage(d, rel, svc, "shop-app"); got != own {
			imgs, _, _ := composeFacts(rel)
			t.Errorf("%s runs %s (%q), not prod's build %s", svc, got, imgs[svc], own)
		}
	}
	// What the fetch named goes once adopted: only bookmarks hold the build.
	for n := range d.names {
		if strings.Contains(n, ":devopsy-") && regexp.MustCompile(`-f[0-9a-z]+$`).MatchString(n) {
			t.Errorf("a fetch tag outlived its fetch: %s", n)
		}
	}
}

// A record reads what its fetch built by the fetch's own tag alone: missing, it fails,
// never reading the project's name, which another environment's fetch may hold.
func TestRecordNeedsItsFetch(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-app", Build: true}
	m := newSim(t, d, "app-prod", &out)
	d.pull("shop-app", "theirs")
	id := m.e.release(web)
	if err := m.e.set.WriteFetch(m.e.rel(id)); err != nil {
		t.Fatal(err)
	}
	if err := m.e.set.Record(m.e.rel(id), ""); err == nil || !errors.Is(err, errFetchedNone) {
		t.Errorf("a record whose fetch built nothing: %v, %+v", err, readList(m.e.rel(id)))
	}
	// Failed, the record leaves the fetch to the undo, which bookmarks by its tags first.
	m.e.set.Undo(m.e.rel(id), false)
	if _, err := os.Stat(filepath.Join(m.e.rel(id), FetchFile)); err == nil {
		t.Error("the fetch file outlived the undo")
	}
}

// A build FROM another service's image by its name is said before the fetch, with the fix:
// builds land under their release's own names, so the name holds another environment's
// build or none. One given as a build context, an ${ARG}, a stage's own name, an image no
// service builds and an inline Dockerfile say nothing.
func TestFromAnotherBuildIsSaid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, text string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return name
	}
	spec := func(dockerfile string, more string) json.RawMessage {
		return json.RawMessage(`{"context":` + strconv.Quote(dir) + `,"dockerfile":` + strconv.Quote(dockerfile) + more + `}`)
	}
	base := Service{Name: "base", Ref: "shop-base", Build: true, BuildSpec: spec(write("base.Dockerfile", "FROM busybox:1.37\n"), "")}
	for name, c := range map[string]struct {
		dockerfile, more string
		said             bool
	}{
		"by name":          {"FROM shop-base\nRUN true\n", "", true},
		"with a flag":      {"FROM --platform=linux/amd64 \\\n  shop-base AS app\n", "", true},
		"as a context":     {"FROM shop-base\n", `,"additional_contexts":{"shop-base":"service:base"}`, false},
		"an arg":           {"ARG BASE=shop-base\nFROM ${BASE}\n", "", false},
		"a stage":          {"FROM busybox:1.37 AS shop-base\nFROM shop-base\n", "", false},
		"a registry image": {"FROM busybox:1.37\n", "", false},
	} {
		app := Service{Name: "app", Ref: "shop-app", Build: true, BuildSpec: spec(write(strings.ReplaceAll(name, " ", "-")+".Dockerfile", c.dockerfile), c.more)}
		got := fromOtherBuilds([]Service{base, app})
		if said := len(got) == 1 && strings.Contains(got[0], `additional_contexts: shop-base: "service:base"`); said != c.said || !c.said && len(got) != 0 {
			t.Errorf("%s: %q", name, got)
		}
	}
	inline := Service{Name: "app", Ref: "shop-app", Build: true, BuildSpec: json.RawMessage(`{"context":` + strconv.Quote(dir) + `,"dockerfile_inline":"FROM shop-base"}`)}
	if got := fromOtherBuilds([]Service{base, inline}); len(got) != 0 {
		t.Errorf("an inline Dockerfile: %q", got)
	}
}

// fetchTagsLeft are the fetch tags Docker still has (fetchTag's -f<cksum>).
func fetchTagsLeft(d *fakeDocker) []string {
	var left []string
	for n := range d.names {
		if strings.Contains(n, ":devopsy-") && regexp.MustCompile(`-f[0-9a-z]+$`).MatchString(n) {
			left = append(left, n)
		}
	}
	sort.Strings(left)
	return left
}

// A release's removal takes what a fetch of it left: a rollback's fetch killed outright (the
// server rebooted, the build out of memory) writes no FetchingFile, so no img recover finds
// its fetch file, and once the release is pruned nothing reads it again.
func TestRemoveTakesAFetchLeftBehind(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-web", Build: true}
	m := newSim(t, d, "app-prod", &out)
	r1, err := m.release(false, web)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.release(false, web); err != nil {
		t.Fatal(err)
	}
	// r1's build is gone (pruned by hand): a rollback to it builds again.
	imgs, _, _ := composeFacts(m.e.rel(r1))
	if err := d.Remove(imgs["web"]); err != nil {
		t.Fatal(err)
	}
	if err := m.e.set.Restore(m.e.rel(r1), m.prev(), false); !errors.Is(err, ErrGone) {
		t.Fatalf("the control: the rollback's restore asks for no build: %v", err)
	}
	// Its fetch builds, and is killed before its restore --fetched or an undo.
	m.fetchID(r1)
	if len(fetchTagsLeft(d)) != 1 {
		t.Fatalf("the control: the fetch's tags: %q", fetchTagsLeft(d))
	}
	if err := m.e.set.Remove(m.e.rel(r1)); err != nil {
		t.Fatal(err)
	}
	if left := fetchTagsLeft(d); len(left) != 0 {
		t.Errorf("r1 is removed, and its fetch's tags stay, which no listing names: %q", left)
	}
}

// noTag is a Docker whose tag fails (a full disk, the daemon restarting).
type noTag struct{ *fakeDocker }

func (noTag) Tag(src, dst string) error { return errors.New("no space left on device") }

// A record whose bookmark could not be put fails, before the switch: the release's file runs
// each own image by that bookmark, so without it the run step has nothing to start.
func TestRecordFailsWithoutItsBookmark(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-web", Build: true}
	m := newSim(t, d, "app-prod", &out)
	id := m.e.release(web)
	m.fetchID(id)
	built, _ := d.ID(fetchTags(m.e.rel(id))["web"])
	m.e.set.Docker = noTag{d}
	err := m.e.set.Record(m.e.rel(id), "")
	if got := fileImage(d, m.e.rel(id), "web", "shop-web"); err == nil && got != built {
		imgs, _, _ := composeFacts(m.e.rel(id))
		t.Errorf("the record went through, and its file runs web by %q, which holds %q, not its build %s (still there: %v):\n%s", imgs["web"], got, built, d.images[built], out.String())
	}
}

// An undo after the record (the server's log could not be made, a fetch marker left between
// the record and its removal: undoLocked, img recover) keeps the listing the record wrote:
// the fetch's tags are gone by then, and the project's name is never the release's.
func TestUndoAfterRecordKeepsItsListing(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-web", Build: true}
	m := newSim(t, d, "app-prod", &out)
	id := m.e.release(web)
	rel := m.e.rel(id)
	m.fetchID(id)
	if err := m.e.set.Record(rel, ""); err != nil {
		t.Fatal(err)
	}
	built := fileImage(d, rel, "web", "")
	// What a plain docker compose elsewhere left under the project's name.
	d.pull("shop-web", "theirs")
	m.e.set.Undo(rel, false)
	if e, _ := match(readList(rel), entry{Service: "web"}); e.ID != built {
		t.Errorf("after the undo the listing has web on %q, not %s, what the release built", e.ID, built)
	}
	if got := fileImage(d, rel, "web", ""); got != built {
		t.Errorf("after the undo the release's bookmark holds %q, not its build %s", got, built)
	}
}

// The restart's last try (--as-built) gives a service its listing has no image of (a
// variable turned it on since) none: never what the project's name holds, which is no
// release's.
func TestAsBuiltNeverReadsTheProjectsName(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-web", Build: true}
	worker := Service{Name: "worker", Ref: "shop-worker", Build: true}
	m := newSim(t, d, "app-prod", &out)
	r1, err := m.release(false, web)
	if err != nil {
		t.Fatal(err)
	}
	failed := m.e.release(web, worker)
	m.fetchID(failed)
	if err := m.e.set.Record(m.e.rel(failed), m.prev()); err != nil {
		t.Fatal(err)
	}
	// r1's compose gives the worker too now, and its build failed again.
	m.e.svcs[r1] = []Service{web, worker}
	d.pull("shop-worker", "theirs")
	m.e.set.Restart, m.e.set.AsBuilt = true, true
	if err := m.e.set.Restore(m.e.rel(r1), m.e.rel(failed), false); err != nil {
		m.fail("restore --as-built: %v", err)
	}
	if got := fileImage(d, m.e.rel(r1), "worker", ""); got == "theirs" {
		imgs, _, _ := composeFacts(m.e.rel(r1))
		t.Errorf("r1 runs the worker by %q on %q, what the project's name held, which no release of prod built", imgs["worker"], got)
	}
}

// A service under a profile with the same image: and build: as a default one (one YAML
// anchor for both, a tool run with --profile) runs its release's bookmark of that build, as
// one with pull_policy: never does.
func TestProfileServiceBuildingTheSameImage(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-app", Build: true}
	tool := Service{Name: "tool", Ref: "shop-app", Build: true}
	m := newSim(t, d, "app-prod", &out)
	m.e.set.ListAll = func(rel string) ([]Service, error) {
		return append(slices.Clone(m.e.svcs[filepath.Base(rel)]), tool), nil
	}
	if _, err := m.release(false, web); err != nil {
		t.Fatal(err)
	}
	own := m.runs("web")
	// Another environment's plain build under the name.
	d.pull("shop-app", "theirs")
	if got := fileImage(d, m.e.rel(m.cur), "tool", "shop-app"); got != own {
		imgs, _, _ := composeFacts(m.e.rel(m.cur))
		t.Errorf("the tool runs %q (image: %q), not its release's build %s", got, imgs["tool"], own)
	}
}

// A service under a profile sharing a built data service's image keeps an image its
// release has while that service is held: the file named the build's bookmark for it before
// the hold.
func TestProfileServiceSharingAHeldBuild(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	tool := Service{Name: "dbtool", Ref: "shop-database", NeverPull: true}
	m := newSim(t, d, "app-prod", &out)
	m.e.set.ListAll = func(rel string) ([]Service, error) {
		return append(slices.Clone(m.e.svcs[filepath.Base(rel)]), tool), nil
	}
	r1, err := m.release(false, builtDB("shop", "11.8"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fileImage(d, m.e.rel(r1), "dbtool", "shop-database"); got == "" || got != m.runs("database") {
		t.Fatalf("the control: the tool runs %q, the database %q", got, m.runs("database"))
	}
	if _, err := m.failRun(false, builtDB("shop", "12.1")); err != nil {
		t.Fatal(err)
	}
	if heldIn(m.e.rel(r1))["database"] == "" {
		m.fail("the control: the database is not held")
	}
	if got := fileImage(d, m.e.rel(r1), "dbtool", "shop-database"); got == "" {
		imgs, _, _ := composeFacts(m.e.rel(r1))
		t.Errorf("with the database held, the tool has no image (image: %q, and nothing writes shop-database)", imgs["dbtool"])
	}
}

// A COPY --from of another service's image by its name reads that name as a FROM does, and
// is said with it.
func TestCopyFromAnotherBuildIsSaid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, text := range map[string]string{
		"base.Dockerfile": "FROM busybox:1.37\n",
		"app.Dockerfile":  "FROM busybox:1.37\nCOPY --from=shop-base /bin/tool /bin/tool\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	spec := func(dockerfile string) json.RawMessage {
		return json.RawMessage(`{"context":` + strconv.Quote(dir) + `,"dockerfile":` + strconv.Quote(dockerfile) + `}`)
	}
	base := Service{Name: "base", Ref: "shop-base", Build: true, BuildSpec: spec("base.Dockerfile")}
	app := Service{Name: "app", Ref: "shop-app", Build: true, BuildSpec: spec("app.Dockerfile")}
	if got := fromOtherBuilds([]Service{base, app}); len(got) != 1 {
		t.Errorf("a COPY --from=shop-base, which base builds: %q", got)
	}
}

// A project behind the door fetches with no fetch file (the door names its builds and
// refuses a tag of devopsy's), and runs each own build by its release's bookmark as any
// project does: a rollback then runs the build it restores, never what the name holds.
func TestDoorProjectFetchesWithoutAFetchFile(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-web", Build: true}
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Door = true
	id := m.e.release(web)
	if err := m.e.set.WriteFetch(m.e.rel(id)); err != nil || fetchTags(m.e.rel(id)) != nil {
		t.Fatalf("a door project's fetch file: %v, %v", err, fetchTags(m.e.rel(id)))
	}
	m.cur = ""
	if _, err := m.release(false, web); err != nil {
		t.Fatal(err)
	}
	if imgs, _, _ := composeFacts(m.e.rel(m.cur)); !isBookmark(imgs["web"]) || fileImage(d, m.e.rel(m.cur), "web", "") != m.runs("web") {
		t.Errorf("a door project's file runs web by %q", imgs["web"])
	}
}

// forget takes an image and every name of it away, as a docker image prune -a by hand does
// to one no container runs.
func forget(d *fakeDocker, id string) {
	for n, i := range d.names {
		if i == id {
			delete(d.names, n)
		}
	}
	delete(d.images, id)
}

// A service under a profile sharing one of two builds in one repository (shop:web and
// shop:tools) keeps its own build's bookmark through a rollback, which writes the file
// with no fetch file to read the sharing from.
func TestSharerKeepsItsBuildAmongOneRepositorys(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	app := Service{Name: "app", Ref: "shop:web", Build: true}
	worker := Service{Name: "worker", Ref: "shop:tools", Build: true}
	tool := Service{Name: "tool", Ref: "shop:tools", NeverPull: true}
	m := newSim(t, d, "app-prod", &out)
	m.e.set.ListAll = func(rel string) ([]Service, error) {
		return append(slices.Clone(m.e.svcs[filepath.Base(rel)]), tool), nil
	}
	r1, err := m.release(false, app, worker)
	if err != nil {
		t.Fatal(err)
	}
	tools := m.runs("worker")
	if got := fileImage(d, m.e.rel(r1), "tool", ""); got != tools {
		t.Fatalf("the control: the tool runs %q, not the worker's build %s", got, tools)
	}
	if _, err := m.release(false, app, worker); err != nil {
		t.Fatal(err)
	}
	if err := m.rollback(r1, false); err != nil {
		m.fail("rollback: %v", err)
	}
	if got := fileImage(d, m.e.rel(r1), "tool", ""); got != tools {
		imgs, _, _ := composeFacts(m.e.rel(r1))
		t.Errorf("after the rollback the tool runs %q (image: %q, the app's is %q), not the worker's build %s", got, imgs["tool"], imgs["app"], tools)
	}
}

// A full restore's release names an image for a profile's service sharing a build, as the
// release it restores did: its directory is new (no ComposeFile of an activation before)
// and its images come back by their bookmarks (no fetch, so no fetch file).
func TestFullRestoreNamesAProfilesSharer(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-app", Build: true}
	tool := Service{Name: "tool", Ref: "shop-app", NeverPull: true}
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	m.e.set.ListAll = func(rel string) ([]Service, error) {
		return append(slices.Clone(m.e.svcs[filepath.Base(rel)]), tool), nil
	}
	if _, err := m.release(false, web); err != nil {
		t.Fatal(err)
	}
	own := m.runs("web")
	snap := m.snapshot("20261102000000")
	id, err := m.fullRestore(snap, []Service{web})
	if err != nil {
		m.fail("the restore: %v", err)
	}
	if m.runs("web") != own {
		t.Fatalf("the control: web runs %s, not the snapshot's %s", m.runs("web"), own)
	}
	if got := fileImage(d, m.e.rel(id), "tool", "shop-app"); got != own {
		imgs, _, _ := composeFacts(m.e.rel(id))
		t.Errorf("the restored release runs the tool on %q (image: %q), not its build %s", got, imgs["tool"], own)
	}
}

// The restart's last try (--as-built) goes on with an own image gone, as its listing has it,
// said: it never fails on what it cannot have, and its holds are the ones a restore with
// Docker and the image lock can write.
func TestAsBuiltGoesOnWithAnImageGone(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-web", Build: true}
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	r1, err := m.release(false, web, db118)
	if err != nil {
		t.Fatal(err)
	}
	own := m.runs("web")
	failed := m.e.release(web, db118)
	m.fetchID(failed)
	if err := m.e.set.Record(m.e.rel(failed), m.prev()); err != nil {
		t.Fatal(err)
	}
	m.cur = failed
	m.up()
	// r1's build is gone, and building it again failed.
	forget(d, own)
	m.e.set.Restart, m.e.set.AsBuilt = true, true
	if err := m.e.set.Restore(m.e.rel(r1), m.e.rel(failed), false); err != nil {
		t.Errorf("restore --as-built with web's image gone: %v\n%s", err, out.String())
	}
}

// A snapshot of a release that lists an own image this server no longer has (a held
// service's own build, which no container runs, pruned by hand) is taken all the same: the
// snapshot lists it, and its restore builds it.
func TestSnapshotOfAReleaseWithAnOwnImageGone(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	r1, err := m.release(false, builtDB("shop", "11.8"))
	if err != nil {
		t.Fatal(err)
	}
	own := m.runs("database")
	if _, err := m.failRun(false, builtDB("shop", "12.1")); err != nil {
		t.Fatal(err)
	}
	if heldIn(m.e.rel(r1))["database"] == "" || m.runs("database") == own {
		m.fail("the control: the database is not held")
	}
	forget(d, own)
	dir := filepath.Join(m.e.set.Snapshots, "20261102000000")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.e.set.BookmarkFrom(dir, m.e.rel(r1)); err != nil {
		t.Errorf("the take's bookmarks, the held database's own build gone: %v\n%s", err, out.String())
	}
}

// A full restore of a snapshot whose built image is gone from the server makes its release
// and builds the image again (img restore's exit 1, then its fetch), as a rollback does.
func TestFullRestoreBuildsAnImageGone(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-web", Build: true}
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	if _, err := m.release(false, web); err != nil {
		t.Fatal(err)
	}
	own := m.runs("web")
	snap := m.snapshot("20261102000000")
	if _, err := m.release(false, web); err != nil {
		t.Fatal(err)
	}
	forget(d, own)
	id := m.e.release(web)
	rel := m.e.rel(id)
	os.WriteFile(filepath.Join(rel, RecordFile), []byte(`{"mode":"build","restore":"20261102000000"}`), 0o644)
	if err := m.e.set.BookmarkFrom(rel, snap); err != nil {
		t.Fatalf("make-release, the snapshot's build gone: %v\n%s", err, out.String())
	}
	m.e.set.NoCheck = true
	if err := m.e.set.Restore(rel, m.prev(), false); !errors.Is(err, ErrGone) {
		t.Errorf("the restore asks for no build: %v", err)
	}
}

// A door project's rollback runs the release it goes back to, or fails: its file names no
// image for its builds (the door builds under the project's name), and nothing puts that
// name back.
func TestDoorRollbackRunsItsOwnBuild(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-web", Build: true}
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Door = true
	r1, err := m.release(false, web)
	if err != nil {
		t.Fatal(err)
	}
	own := m.runs("web")
	if _, err := m.release(false, web); err != nil {
		t.Fatal(err)
	}
	left := m.runs("web")
	if err := m.rollback(r1, false); err != nil {
		return
	}
	if got := m.runs("web"); got != own {
		e, _ := match(readList(m.e.rel(r1)), entry{Service: "web"})
		t.Errorf("the rollback to %s went through, lists web on %s, and web runs %s (the release it left ran %s)", r1, e.ID, got, left)
	}
}

// A record fails on a build it cannot read, before the switch, whoever the project's user
// is: for any other project a fetch tag missing fails it (TestRecordNeedsItsFetch), since
// an own image runs by the bookmark its record puts.
func TestDoorRecordNeedsItsBuild(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: DoorBuild("app-prod", "web"), Build: true}
	m := newSim(t, d, "app-prod", &out)
	m.e.set.Door = true
	id := m.e.release(web)
	rel := m.e.rel(id)
	if err := m.e.set.WriteFetch(rel); err != nil {
		t.Fatal(err)
	}
	// The fetch built nothing the record can read.
	err := m.e.set.Record(rel, "")
	imgs, _, _ := composeFacts(rel)
	if err == nil {
		t.Errorf("a door project's record went through with no image of web: listed %+v, its file names %q", readList(rel), imgs["web"])
	}
}
