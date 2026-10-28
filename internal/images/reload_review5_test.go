package images

import (
	"bytes"
	"testing"
)

// Bug (review 5): a restore that builds an image again because its build:
// section changed (Restore's changed, then the script's fetch and
// Restore with fetched) records the key the image was built with before,
// not the one it was just built with, when the build comes out as the
// same image: keep carries an old key over to a listing again of the same
// image id, which is right for a bookmark put back but not for a fresh
// build. A build input that does not reach the image (an argument the
// Dockerfile never declares, cache_from, a build secret) gives the same
// image fully cached. The release then lists the old key: every reload is
// refused ("its build changed since the release built its image") though
// it was just built with today's values, and every later restore of it
// builds it again.
func TestReview5RestoreRebuiltSameImage(t *testing.T) {
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

// Bug (review 5): in image mode a service with a build: section and
// pull_policy: never is own (restored from its bookmark), and its listing
// carries its build key, but the fetch never builds it (image mode's is
// compose's pull, which skips it). A rollback whose build key changed asks
// for a build anyway (ErrGone, "building it again"): the script's fetch
// builds nothing, and the restore with fetched takes whatever the name
// holds now, the newer release's image, in place of the one its bookmark
// holds. Before the build key was checked, the rollback put its bookmark
// back: it ran the code it built.
func TestReview5RestoreImageModeNeverPull(t *testing.T) {
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
	if id, _ := d.ID("app-prod-tool"); err != nil || id != "sha256:t1" {
		t.Errorf("an image-mode rollback of a never-pulled build: %v, app-prod-tool is %s, want its bookmark's sha256:t1\n%s", err, id, out.String())
	}
}
