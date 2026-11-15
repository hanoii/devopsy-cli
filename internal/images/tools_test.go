package images

import (
	"bytes"
	"path/filepath"
	"slices"
	"testing"
)

// A release a rollback (or a restart) brings back keeps the name its tools run by: a tool is
// never pinned and runs what its tag holds now, so once the newer release that pulled the
// moved tag goes, the tool's name must still name an image.
//
// Bug (review round 1 of feat/tool-bookmarks): only a new release's record bookmarks its
// tools, and a restore leaves them as they were, so the restored release's tool bookmark
// holds the image its tag had at its record, which nothing runs, while the image the tag
// holds now is held only by the newer release's tool bookmark. Removing that release (a
// --releases remove failed after the failed run restarted the previous one) takes jq:1.8
// itself: the current release's tool then pulls at its next run (or fails, offline), where
// before the branch the name stayed. A listed pulled image does not lose its name so: the
// restore moves its bookmark to what the name holds now.
func TestRestoredToolKeepsItsName(t *testing.T) {
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
	// The registry moves jq:1.8 on; the next release pulls and records it, then fails its
	// run, and the restart brings r1 back.
	d.pull(jq.Ref, "sha256:j2")
	r2 := e.release(proxySvc)
	if err := e.set.Record(e.rel(r2), e.rel(r1)); err != nil {
		t.Fatal(err)
	}
	if err := e.set.Restore(e.rel(r1), e.rel(r2), false); err != nil {
		t.Fatal(err)
	}
	if err := e.set.Remove(e.rel(r2)); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.names[familiar(jq.Ref)]; !ok {
		t.Errorf("%s is gone while the current release %s lists its tool jq: %v\n%s", jq.Ref, r1, d.names, out.String())
	}
}
