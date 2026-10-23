package images

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recEnv is one environment whose releases list services given by the
// test: a build-mode app, a pulled database with data, a stateless
// pulled proxy.
type recEnv struct {
	*env
	svcs map[string][]Service
}

func newRecEnv(t *testing.T, d *fakeDocker, name, mode string, out *bytes.Buffer) *recEnv {
	e := &recEnv{env: newEnv(t, d, name, filepath.Join(t.TempDir(), LeftFile), out), svcs: map[string][]Service{}}
	e.set.Mode = mode
	e.set.List = func(rel string) ([]Service, error) { return e.svcs[filepath.Base(rel)], nil }
	return e
}

func (e *recEnv) release(svcs ...Service) string {
	id := e.newRelease()
	e.svcs[id] = svcs
	return id
}

var (
	appSvc   = Service{Name: "app", Ref: "app-prod-app", Build: true, Data: true}
	dbSvc    = func(ref string) Service { return Service{Name: "database", Ref: ref, Data: true} }
	proxySvc = Service{Name: "alwaysup", Ref: "caddy:2-alpine"}
)

// The listing records each service's kind, digest and version: the app
// is built (own, whatever its data label), the database a pulled data
// service, the proxy pulled; a listing from before them still reads.
func TestRecordListing(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	e := newRecEnv(t, d, "app-prod", ModeBuild, &out)
	d.pull("app-prod-app", "sha256:a1")
	d.pull("mariadb:11.8", "sha256:m1")
	d.versions["sha256:m1"] = "11.8.9"
	d.pull("caddy:2-alpine", "sha256:c1")
	id := e.release(appSvc, dbSvc("mariadb:11.8"), proxySvc)
	if err := e.set.Record(e.rel(id), ""); err != nil {
		t.Fatal(err)
	}
	got := readList(e.rel(id))
	want := []entry{
		{Ref: "app-prod-app", ID: "sha256:a1", Service: "app", Kind: KindOwn},
		{Ref: "mariadb:11.8", ID: "sha256:m1", Service: "database", Kind: KindData, Digest: "mariadb@sha256:m1", Version: "11.8.9"},
		{Ref: "caddy:2-alpine", ID: "sha256:c1", Service: "alwaysup", Kind: KindPulled, Digest: "caddy@sha256:c1"},
	}
	if len(got) != len(want) {
		t.Fatalf("listing: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if d.names["mariadb:"+bookmarkPrefix("app-prod")+id] != "sha256:m1" {
		t.Errorf("not bookmarked: %v", d.names)
	}
	// In image mode the app is pulled: no data service for having a
	// build: section.
	e2 := newRecEnv(t, d, "app-stage", ModeImage, &out)
	id2 := e2.release(appSvc)
	if err := e2.set.Record(e2.rel(id2), ""); err != nil {
		t.Fatal(err)
	}
	if got := readList(e2.rel(id2)); got[0].Kind != KindPulled || got[0].Digest != "app-prod-app@sha256:a1" {
		t.Errorf("image mode app: %+v", got[0])
	}
	// v0.48's listing: name and id only.
	os.WriteFile(filepath.Join(e.rel(id), ListFile), []byte("mariadb:11\tsha256:m1\nx\n"), 0o644)
	if got := readList(e.rel(id)); len(got) != 2 || got[0] != (entry{Ref: "mariadb:11", ID: "sha256:m1"}) || got[1] != (entry{Ref: "x"}) {
		t.Errorf("old listing: %+v", got)
	}
}

// A release says what moved since the one it replaces: a pulled image by
// version, else by short digest, an own one rebuilt, nothing for what
// stayed; an image the project builds that changed under the same name
// in image mode is said to stay forward on a rollback.
func TestRecordMoved(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	e := newRecEnv(t, d, "app-prod", ModeBuild, &out)
	d.pull("app-prod-app", "sha256:a1")
	d.pull("mariadb:11.8", "sha256:m1")
	d.versions["sha256:m1"] = "11.8.9"
	d.pull("caddy:2-alpine", "sha256:c1aaaaaaaaaaaaaaaa")
	r1 := e.release(appSvc, dbSvc("mariadb:11.8"), proxySvc)
	if err := e.set.Record(e.rel(r1), ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "changed") {
		t.Errorf("a first release says what changed:\n%s", out.String())
	}
	d.pull("app-prod-app", "sha256:a2")
	d.pull("mariadb:11.8", "sha256:m2")
	d.versions["sha256:m2"] = "11.8.10"
	d.pull("caddy:2-alpine", "sha256:c2bbbbbbbbbbbbbbbb")
	r2 := e.release(appSvc, dbSvc("mariadb:11.8"), proxySvc)
	out.Reset()
	if err := e.set.Record(e.rel(r2), e.rel(r1)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"images that changed:", "| app: rebuilt", "| mariadb:11.8: 11.8.9 -> 11.8.10", "| caddy:2-alpine: sha256:c1aaaaaaaaaa -> sha256:c2bbbbbbbbbb"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	r3 := e.release(appSvc, dbSvc("mariadb:11.8"), proxySvc)
	out.Reset()
	if err := e.set.Record(e.rel(r3), e.rel(r2)); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("nothing moved, yet:\n%s", out.String())
	}

	// Image mode: the app is a registry's image under a fixed name.
	i := newRecEnv(t, d, "app-stage", ModeImage, &out)
	a := Service{Name: "app", Ref: "registry/app:prod", Build: true}
	d.pull("registry/app:prod", "sha256:p1")
	i1 := i.release(a)
	i.set.Record(i.rel(i1), "")
	d.pull("registry/app:prod", "sha256:p2")
	i2 := i.release(a)
	out.Reset()
	i.set.Record(i.rel(i2), i.rel(i1))
	if !strings.Contains(out.String(), "warning: app: a new image under the same name registry/app:prod, which no rollback takes back") || !strings.Contains(out.String(), "${DEVOPSY_RELEASE_COMMIT}") {
		t.Errorf("same tag:\n%s", out.String())
	}
	// Under a commit tag, nothing to say but what moved.
	d.pull("registry/app:c3", "sha256:p3")
	i3 := i.release(Service{Name: "app", Ref: "registry/app:c3", Build: true})
	out.Reset()
	i.set.Record(i.rel(i3), i.rel(i2))
	if strings.Contains(out.String(), "same name") || !strings.Contains(out.String(), "registry/app:prod -> registry/app:c3") {
		t.Errorf("commit tag:\n%s", out.String())
	}
}

// An image the project builds and pulls (image mode) that stayed the
// same for over StaleAfter while releases go on: a rebuild would bring
// its base's patches. Never in build mode, which rebuilds on every
// release, nor for a third party's image.
func TestRecordStale(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	for _, mode := range []string{ModeImage, ModeBuild} {
		e := newRecEnv(t, d, "app-"+mode, mode, &out)
		e.set.Now = func() time.Time { return now }
		a := Service{Name: "app", Ref: "registry/app:c1", Build: true}
		d.pull("registry/app:c1", "sha256:p1")
		d.created["sha256:p1"] = now.Add(-41 * 24 * time.Hour)
		d.pull("caddy:2-alpine", "sha256:c1")
		d.created["sha256:c1"] = now.Add(-100 * 24 * time.Hour)
		r1 := e.release(a, proxySvc)
		e.set.Record(e.rel(r1), "")
		r2 := e.release(a, proxySvc)
		out.Reset()
		e.set.Record(e.rel(r2), e.rel(r1))
		stale := strings.Contains(out.String(), "warning: app's image was built 41 days ago: its base may have patches")
		if stale != (mode == ModeImage) || strings.Contains(out.String(), "alwaysup") {
			t.Errorf("%s mode:\n%s", mode, out.String())
		}
	}
}

func TestParseComposeConfig(t *testing.T) {
	svcs, err := ParseComposeConfig([]byte(`{"name":"x-prod","services":{
		"db":{"image":"mariadb:11.8","labels":{"devopsy.data":"none"}},
		"app":{"build":{"context":"."},"labels":{"devopsy.data":"mnt"}},
		"tool":{"image":"local/tool","pull_policy":"never"},
		"web":{"image":"caddy:2"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Service{
		{Name: "app", Ref: "x-prod-app", Build: true, Data: true},
		{Name: "db", Ref: "mariadb:11.8", Data: true},
		{Name: "tool", Ref: "local/tool", NeverPull: true},
		{Name: "web", Ref: "caddy:2"},
	}
	if len(svcs) != len(want) {
		t.Fatalf("%+v", svcs)
	}
	for i := range want {
		if svcs[i] != want[i] {
			t.Errorf("%d: %+v, want %+v", i, svcs[i], want[i])
		}
	}
	if _, err := ParseComposeConfig([]byte("")); err == nil {
		t.Error("no error for no config")
	}
}
