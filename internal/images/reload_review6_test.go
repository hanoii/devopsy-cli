package images

import (
	"bytes"
	"testing"
)

// Bug (review 6): a restore that builds an image again because its
// build: section changed (Restore's changed, the script's fetch, then
// Restore with fetched) leaves the image the release ran before with no
// name at all: keep moves the release's bookmark to the new image (the
// same bookmark name, another id), the service's name moved with the
// build, and nothing records or removes the old image, which stays on
// the server as a dangling image that no listing, bookmark or removal
// ever reaches ("Every image a release runs stays until its last release
// goes"; "A removal frees only what nothing else holds": here nothing
// holds it, and nothing frees it). Before build keys a restore only
// fetched when the image was gone, so there was nothing to leave.
func TestReview6RestoreRebuiltLeavesOldImage(t *testing.T) {
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
	if tags, _ := d.Tags("sha256:a1"); d.images["sha256:a1"] && len(tags) == 0 {
		t.Errorf("the image first ran before its build again is left with no name: nothing lists, holds or removes it\n%s", out.String())
	}
	// Once every release goes, nothing stays.
	for _, id := range []string{second, first} {
		if err := e.set.Remove(e.rel(id)); err != nil {
			t.Fatalf("removing %s: %v\n%s", id, err, out.String())
		}
	}
	if len(d.images) != 0 {
		t.Errorf("images left once every release went: %v\n%s", d.images, out.String())
	}
}
