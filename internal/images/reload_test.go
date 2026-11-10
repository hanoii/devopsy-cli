package images

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A build: section's key is the same whichever way the release's code
// is reached (compose prints its paths absolute: releases/<id>/code at
// the fetch, current at a reload) and in whatever order compose prints
// its keys; it changes with any argument, which it names without its
// value, and tells an unset argument from an empty one.
func TestBuildKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	code := filepath.Join(dir, "releases", "1", "code")
	if err := os.MkdirAll(filepath.Join(code, ".devopsy"), 0o755); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(dir, "current")
	if err := os.Symlink(code, current); err != nil {
		t.Fatal(err)
	}
	spec := func(root, args string) json.RawMessage {
		return json.RawMessage(`{"context":"` + root + `","dockerfile":".devopsy/Dockerfile","args":{` + args + `}}`)
	}
	fetched := BuildKey(spec(code, `"APP_UID":"10001","PHP":"8.4"`), code)
	if fetched == "" || strings.Contains(fetched, "10001") {
		t.Fatalf("key %q: empty, or a value in clear", fetched)
	}
	if now := BuildKey(json.RawMessage(`{"args":{"PHP":"8.4","APP_UID":"10001"},"dockerfile":".devopsy/Dockerfile","context":"`+current+`"}`), current); now != fetched {
		t.Errorf("the same build through current is %q, want %q", now, fetched)
	}
	changed := BuildKey(spec(current, `"APP_UID":"10002","PHP":"8.4"`), current)
	if changed == fetched || changedArgs(fetched, changed) != "APP_UID" {
		t.Errorf("APP_UID changed: %q (named %q), want another key naming APP_UID", changed, changedArgs(fetched, changed))
	}
	if got := changedArgs(fetched, BuildKey(spec(code, `"APP_UID":"10001"`), code)); got != "PHP" {
		t.Errorf("PHP dropped is named %q, want PHP", got)
	}
	if BuildKey(spec(code, `"X":null`), code) == BuildKey(spec(code, `"X":""`), code) {
		t.Errorf("an unset argument and an empty one give one key")
	}
	other := BuildKey(json.RawMessage(`{"context":"`+code+`","dockerfile":".devopsy/Other","args":{"APP_UID":"10001","PHP":"8.4"}}`), code)
	if other == fetched || changedArgs(fetched, other) != "its build: section" {
		t.Errorf("another Dockerfile: %q, named %q", other, changedArgs(fetched, other))
	}
	if BuildKey(nil, code) != "" || BuildKey(json.RawMessage("null"), code) != "" {
		t.Errorf("no build: section gives a key")
	}
}

// A reload runs what the release fetched: the check passes while the
// services resolve as they did, and names a changed image, a changed
// build argument and a service the release has no image for. A rollback
// listing an image again keeps the key it was built with.
func TestReloadCheck(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	e := newRecEnv(t, d, "app-prod", ModeBuild, &out)
	app := appSvc
	app.BuildKey = "k1;APP_UID=aaaaaaaa"
	d.pull("app-prod-app", "sha256:a1")
	d.pull("caddy:2-alpine", "sha256:c1")
	id := e.release(app, proxySvc)
	if err := e.set.Record(e.rel(id), ""); err != nil {
		t.Fatal(err)
	}
	if got, err := e.set.ReloadCheck(e.rel(id)); err != nil || len(got) != 0 {
		t.Fatalf("unchanged: %q (%v), want nothing", got, err)
	}

	changed := app
	changed.BuildKey = "k2;APP_UID=bbbbbbbb"
	proxy := proxySvc
	proxy.Ref = "caddy:3-alpine"
	tool := Service{Name: "tool", Ref: "busybox:1.37"}
	e.svcs[id] = []Service{changed, proxy, tool}
	got, err := e.set.ReloadCheck(e.rel(id))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app's build changed since the release built its image: APP_UID", "alwaysup's image is caddy:3-alpine now, the release fetched caddy:2-alpine", "tool is not one of the services"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %d lines", got, len(want))
	}
	for _, w := range want {
		if !strings.Contains(strings.Join(got, "\n"), w) {
			t.Errorf("no %q in %q", w, got)
		}
	}

	// Listed again after a fetch (Bookmark), the build just made is
	// today's: its key is the one recorded.
	e.svcs[id] = []Service{changed, proxySvc}
	e.set.Bookmark(e.rel(id))
	if got, _ := e.set.ReloadCheck(e.rel(id)); len(got) != 0 {
		t.Errorf("after a fetch: %q, want nothing", got)
	}

	if got, _ := e.set.ReloadCheck(e.rel(e.release(app))); len(got) != 1 || !strings.Contains(got[0], "no record") {
		t.Errorf("a release with no listing: %q", got)
	}
}

// A restore (a rollback, the restart after a failed run, a full restore)
// builds an own image again when its build: section resolves otherwise
// than when it was built (a build argument changed since): its bookmark
// would run code built for the old value under the new one (an image
// built for APP_UID 10001 run as 10002). Unchanged, its bookmark goes back.
func TestRestoreRebuildsChangedBuild(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	e := newRecEnv(t, d, "app-prod", ModeBuild, &out)
	built := appSvc
	built.Data = false
	built.BuildKey = "k1;APP_UID=aaaaaaaa"
	d.pull("app-prod-app", "sha256:a1")
	first := e.release(built)
	if err := e.set.Record(e.rel(first), ""); err != nil {
		t.Fatal(err)
	}
	changed := built
	changed.BuildKey = "k2;APP_UID=bbbbbbbb"
	d.pull("app-prod-app", "sha256:a2")
	second := e.release(changed)
	if err := e.set.Record(e.rel(second), e.rel(first)); err != nil {
		t.Fatal(err)
	}
	// Back to first, the variables still as second's.
	e.svcs[first] = []Service{changed}
	if err := e.set.Restore(e.rel(first), e.rel(second), false); err != ErrGone || !strings.Contains(out.String(), "APP_UID") {
		t.Errorf("a restore whose build changed: %v, want ErrGone naming APP_UID\n%s", err, out.String())
	}
	out.Reset()
	e.svcs[first] = []Service{built}
	err := e.set.Restore(e.rel(first), e.rel(second), false)
	if id := fileImage(d, e.rel(first), "app", "app-prod-app"); err != nil || id != "sha256:a1" {
		t.Errorf("a restore whose build did not change: %v, app runs %s, want sha256:a1\n%s", err, id, out.String())
	}
}

// The restart after a failed run step whose build again failed puts the
// release's bookmarks back as built (AsBuilt), whatever the build keys
// say: never the failed release's image left on the name. It says what
// it runs with the old values.
func TestRestoreAsBuilt(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	e := newRecEnv(t, d, "app-prod", ModeBuild, &out)
	built := appSvc
	built.Data = false
	built.BuildKey = "k1;APP_UID=aaaaaaaa"
	d.pull("app-prod-app", "sha256:a1")
	first := e.release(built)
	if err := e.set.Record(e.rel(first), ""); err != nil {
		t.Fatal(err)
	}
	changed := built
	changed.BuildKey = "k2;APP_UID=bbbbbbbb"
	d.pull("app-prod-app", "sha256:a2")
	second := e.release(changed)
	if err := e.set.Record(e.rel(second), e.rel(first)); err != nil {
		t.Fatal(err)
	}
	e.svcs[first] = []Service{changed}
	e.set.AsBuilt = true
	err := e.set.Restore(e.rel(first), e.rel(second), false)
	if id := fileImage(d, e.rel(first), "app", "app-prod-app"); err != nil || id != "sha256:a1" {
		t.Errorf("as built: %v, app runs %s, want sha256:a1\n%s", err, id, out.String())
	}
	if !strings.Contains(out.String(), "warning: ") || !strings.Contains(out.String(), "APP_UID") {
		t.Errorf("as built, nothing says app runs with the old APP_UID:\n%s", out.String())
	}
}

// A restore that builds an image again because its build: section changed
// (Restore's changed, then the script's fetch and Restore with fetched)
// records the key it was just built with, even when the build comes out as
// the same image, as a build input that does not reach the image gives (an
// argument the Dockerfile never declares, cache_from, a build secret,
// fully cached). keep carrying the old key over would refuse every reload
// ("its build changed since the release built its image") and build it
// again at every later restore.
func TestRestoreRebuiltSameImage(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	e := newRecEnv(t, d, "app-prod", ModeBuild, &out)
	built := appSvc
	built.Data = false
	built.BuildKey = "k1;APP_UID=aaaaaaaa"
	d.pull("app-prod-app", "sha256:a1")
	first := e.release(built)
	if err := e.set.Record(e.rel(first), ""); err != nil {
		t.Fatal(err)
	}
	changed := built
	changed.BuildKey = "k2;APP_UID=bbbbbbbb"
	d.pull("app-prod-app", "sha256:a2")
	second := e.release(changed)
	if err := e.set.Record(e.rel(second), e.rel(first)); err != nil {
		t.Fatal(err)
	}
	// Back to first, today's variables second's: built again.
	e.svcs[first] = []Service{changed}
	if err := e.set.Restore(e.rel(first), e.rel(second), false); err != ErrGone {
		t.Fatalf("a restore whose build changed: %v, want ErrGone", err)
	}
	// The fetch builds first's code with today's values: the argument
	// reaches nothing in the image, so the cache gives the same image.
	d.pull("app-prod-app", "sha256:a1")
	if err := e.set.Restore(e.rel(first), e.rel(second), true); err != nil {
		t.Fatalf("the restore after the build: %v\n%s", err, out.String())
	}
	got, err := e.set.ReloadCheck(e.rel(first))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 0 {
		t.Errorf("a release just built with today's values is refused a reload: %q (listing %+v)", got, readList(e.rel(first)))
	}
	out.Reset()
	if err := e.set.Restore(e.rel(first), e.rel(second), false); err != nil {
		t.Errorf("a restore of what was just built with today's values builds it again: %v\n%s", err, out.String())
	}
}

// In image mode a service with a build: section and pull_policy: never is
// own (restored from its bookmark), and its listing carries its build key,
// but the fetch never builds it (image mode's is compose's pull, which
// skips it). A rollback whose build key changed puts its bookmark back,
// the code it built: asking for a build (ErrGone) would build nothing, and
// the restore with fetched would take whatever the name holds now, the
// newer release's image.
func TestRestoreImageModeNeverPull(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	e := newRecEnv(t, d, "app-prod", ModeImage, &out)
	tool := Service{Name: "tool", Ref: "app-prod-tool", Build: true, NeverPull: true, BuildKey: "k1;APP_UID=aaaaaaaa"}
	d.pull("app-prod-tool", "sha256:t1")
	first := e.release(tool)
	if err := e.set.Record(e.rel(first), ""); err != nil {
		t.Fatal(err)
	}
	changed := tool
	changed.BuildKey = "k2;APP_UID=bbbbbbbb"
	d.pull("app-prod-tool", "sha256:t2")
	second := e.release(changed)
	if err := e.set.Record(e.rel(second), e.rel(first)); err != nil {
		t.Fatal(err)
	}
	e.svcs[first] = []Service{changed}
	err := e.set.Restore(e.rel(first), e.rel(second), false)
	if err == ErrGone {
		// What the script does on exit 1: image mode's fetch (a pull,
		// which builds nothing), then the restore with fetched.
		err = e.set.Restore(e.rel(first), e.rel(second), true)
	}
	if id := fileImage(d, e.rel(first), "tool", "app-prod-tool"); err != nil || id != "sha256:t1" {
		t.Errorf("an image-mode rollback of a never-pulled build: %v, tool runs %s, want its bookmark's sha256:t1\n%s", err, id, out.String())
	}
}

// A restore that builds an image again because its build: section changed
// leaves the image the release ran before reachable: keep moves the
// release's bookmark to the new image, the service's name moves with the
// build, and the old image must still be recorded, or it stays on the
// server as a dangling image that no listing, bookmark or removal ever
// reaches. Once every release is gone, no image is left.
func TestRestoreRebuiltLeavesOldImage(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	e := newRecEnv(t, d, "app-prod", ModeBuild, &out)
	built := appSvc
	built.Data = false
	built.BuildKey = "k1;APP_UID=aaaaaaaa"
	d.pull("app-prod-app", "sha256:a1")
	first := e.release(built)
	if err := e.set.Record(e.rel(first), ""); err != nil {
		t.Fatal(err)
	}
	changed := built
	changed.BuildKey = "k2;APP_UID=bbbbbbbb"
	d.pull("app-prod-app", "sha256:a2")
	second := e.release(changed)
	if err := e.set.Record(e.rel(second), e.rel(first)); err != nil {
		t.Fatal(err)
	}
	// Back to first, today's variables second's: built again, another
	// image (APP_UID reaches the image's files).
	e.svcs[first] = []Service{changed}
	if err := e.set.Restore(e.rel(first), e.rel(second), false); err != ErrGone {
		t.Fatalf("a restore whose build changed: %v, want ErrGone", err)
	}
	d.pull("app-prod-app", "sha256:a3")
	if err := e.set.Restore(e.rel(first), e.rel(second), true); err != nil {
		t.Fatalf("the restore after the build: %v\n%s", err, out.String())
	}
	// Once every release goes, nothing stays: the image first ran before
	// its build again is left with no name, which the pruning after any
	// removal frees (img prune: docker image prune -f), unused.
	for _, id := range []string{second, first} {
		if err := e.set.Remove(e.rel(id)); err != nil {
			t.Fatalf("removing %s: %v\n%s", id, err, out.String())
		}
	}
	e.set.Prune()
	if len(d.images) != 0 {
		t.Errorf("images left once every release went: %v\n%s", d.images, out.String())
	}
}
