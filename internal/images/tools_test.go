package images

import (
	"bytes"
	"path/filepath"
	"slices"
	"testing"
)

// A tool whose image a release's listing takes over at a re-listing (a variable moved a
// listed service onto the tool's tag, then a restart) is recorded as left by nothing: the
// release's own listing lists and bookmarks that name, so "no release lists it any more"
// is false, and --releases shows a leftover that stays until the release goes.
//
// Review round 2, a bug: keepIn judges what a file dropped against that file's entries
// alone (named), so the tools' write records a name the listing's write just took, and
// the listing's a name the tools' write is about to take.
func TestToolListedIsNoNameLeft(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	e := newRecEnv(t, d, "app-prod", ModeImage, &out)
	next := Service{Name: "proxy-next", Ref: "caddy:2.8"}
	e.set.ListAll = func(rel string) ([]Service, error) {
		return append(slices.Clone(e.svcs[filepath.Base(rel)]), next), nil
	}
	d.pull(proxySvc.Ref, "sha256:c1")
	d.pull(next.Ref, "sha256:c2")
	r1 := e.release(proxySvc)
	if err := e.set.Record(e.rel(r1), ""); err != nil {
		t.Fatal(err)
	}
	if got := readTools(e.rel(r1)); len(got) != 1 || got[0].ID != "sha256:c2" {
		t.Fatalf("tools: %+v", got)
	}
	// The proxy's tag set to the tool's (a --vars set), then a restart of r1.
	e.svcs[r1] = []Service{{Name: proxySvc.Name, Ref: next.Ref}}
	e.set.Restart = true
	if err := e.set.Restore(e.rel(r1), "", false); err != nil {
		t.Fatal(err)
	}
	if l := readList(e.rel(r1)); len(l) != 1 || l[0].Ref != next.Ref || l[0].ID != "sha256:c2" {
		t.Fatalf("the listing after the restart: %+v", l)
	}
	for _, l := range readLeft(e.set.Left) {
		if familiar(l.ref) == familiar(next.Ref) {
			t.Errorf("%s recorded as left (%s) while %s's listing lists and bookmarks it\n%s", l.ref, l.why, r1, out.String())
		}
	}
}

// A newer release whose fetch failed after it pulled a tool's moved tag, undone and then
// removed, leaves the tag the live release's tool runs by: nothing pinned the tool, so
// its tag is all the live release runs it by.
//
// Review round 2, a bug: round 1's fix records a release's tools again only when its own
// listing is written (a restore), so a live release nothing restored keeps its bookmark on
// the image the tag had, the failed release alone holds the image the tag names now, and
// its removal strips the tag and deletes that image.
func TestFailedReleaseRemovalKeepsLiveTool(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	e := newRecEnv(t, d, "app-prod", ModeImage, &out)
	jq := Service{Name: "jq", Ref: "ghcr.io/jqlang/jq:1.8"}
	e.set.ListAll = func(rel string) ([]Service, error) {
		return append(slices.Clone(e.svcs[filepath.Base(rel)]), jq), nil
	}
	d.pull(proxySvc.Ref, "sha256:c1")
	d.pull(jq.Ref, "sha256:j1")
	r1 := e.release(proxySvc)
	if err := e.set.Record(e.rel(r1), ""); err != nil {
		t.Fatal(err)
	}
	// The registry moves jq:1.8 on; the next release pulls it, fails its fetch and is
	// undone, r1 staying live with no restore; then the failed release is removed.
	d.pull(jq.Ref, "sha256:j2")
	r2 := e.release(proxySvc)
	e.set.Undo(e.rel(r2), false)
	if got := d.names[toolTag("app-prod", r2, jq.Ref)]; got != "sha256:j2" {
		t.Fatalf("the failed release's tool bookmark holds %q", got)
	}
	if err := e.set.Remove(e.rel(r2)); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.names[familiar(jq.Ref)]; !ok {
		t.Errorf("%s is gone while the live release %s lists its tool jq: %v\n%s", jq.Ref, r1, d.names, out.String())
	}
}
