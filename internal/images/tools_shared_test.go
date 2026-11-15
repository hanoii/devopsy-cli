package images

import (
	"bytes"
	"path/filepath"
	"slices"
	"testing"
)

// A tool two environments run by one floating tag keeps its name while one of them lists it:
// the environment that pulled the tag last going (its --destroy, its pruning) leaves the tag
// the other's live release runs its tool by, as that release has its bookmark.
//
// Review round 2, a bug: a tool runs by its name, never a pin, but each release bookmarks
// only the image its name held at its own write, so the release that pulled the moved tag
// last is the one bookmark on the image the tag names now, and its removal strips the tag
// from under every other environment's live release (the same cause as
// TestFailedReleaseRemovalKeepsLiveTool, across environments: no write of the other
// environment's listing comes in between to bookmark its tool again).
func TestToolNameOutlivesTheEnvironmentThatPulledIt(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	left := filepath.Join(t.TempDir(), LeftFile)
	jq := Service{Name: "jq", Ref: "ghcr.io/jqlang/jq:1.8"}
	a, b := newRecEnv(t, d, "app-a", ModeImage, &out), newRecEnv(t, d, "app-b", ModeImage, &out)
	for _, e := range []*recEnv{a, b} {
		e.set.Left = left
		e.set.ListAll = func(rel string) ([]Service, error) {
			return append(slices.Clone(e.svcs[filepath.Base(rel)]), jq), nil
		}
	}
	d.pull(proxySvc.Ref, "sha256:c1")
	d.pull(jq.Ref, "sha256:j1")
	ra := a.release(proxySvc)
	if err := a.set.Record(a.rel(ra), ""); err != nil {
		t.Fatal(err)
	}
	// The registry moves jq:1.8 on; app-b's release pulls it and records its tool there.
	d.pull(jq.Ref, "sha256:j2")
	rb := b.release(proxySvc)
	if err := b.set.Record(b.rel(rb), ""); err != nil {
		t.Fatal(err)
	}
	// app-b goes (its --destroy removes every release), app-a's release staying live.
	if err := b.set.Remove(b.rel(rb)); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.names[familiar(jq.Ref)]; !ok {
		t.Errorf("%s is gone while app-a's live release %s lists its tool jq: %v\n%s", jq.Ref, ra, d.names, out.String())
	}
}

// A release whose record fails on its own image's bookmark, then undone, still records the
// tools its fetch pulled: its listing is written, so the undo keeps it and bookmarks nothing,
// and the record is that release's one write of its tools.
//
// Review round 2, a bug: writeHeld returns on keep's error before keepTools, though keep wrote
// the listing (keepIn writes the file before it tags), so the failed release's moved tool tag
// is held by no release, the leak the tools file was made for.
func TestFailedRecordKeepsItsTools(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	e := newRecEnv(t, d, "app-prod", ModeImage, &out)
	local := Service{Name: "local", Ref: "local-app", NeverPull: true}
	jq := Service{Name: "jq", Ref: "ghcr.io/jqlang/jq:1.8"}
	e.set.ListAll = func(rel string) ([]Service, error) {
		return append(slices.Clone(e.svcs[filepath.Base(rel)]), jq), nil
	}
	d.pull(local.Ref, "sha256:l1")
	d.pull(jq.Ref, "sha256:j1")
	r1 := e.release(local)
	if err := e.set.Record(e.rel(r1), ""); err != nil {
		t.Fatal(err)
	}
	// The next release's fetch pulls jq:1.8's new image; its own image's bookmark is refused
	// (the first Tag), so its record fails, and the release is undone.
	d.pull(jq.Ref, "sha256:j2")
	r2 := e.release(local)
	d.fail = map[string]bool{"Tag": true}
	if err := e.set.Record(e.rel(r2), e.rel(r1)); err == nil {
		t.Fatal("the record went through a refused own bookmark")
	}
	e.set.Undo(e.rel(r2), false)
	if got := d.names[toolTag("app-prod", r2, jq.Ref)]; got != "sha256:j2" {
		t.Errorf("the failed release's tool bookmark holds %q, want the image its fetch pulled: %v\n%s", got, d.names, out.String())
	}
}
