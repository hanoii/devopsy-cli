package images

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A snapshot's directory bookmarks the images its release ran as s<id>
// (its id may be a release's too), counted as the environment's own: a
// rollback of a built app past a snapshot of the newer release puts its
// name back (the snapshot's bookmark on the newer image is no other
// environment's), and removing the snapshot frees only its own bookmark.
func TestSnapshotBookmarks(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	prod := newSim(t, d, "app-prod", &out)
	prod.e.set.Snapshots = filepath.Join(prod.base(), "snapshots")
	r1, err := prod.release(false, appSvc)
	if err != nil {
		t.Fatal(err)
	}
	old := prod.runs("app")
	r2, err := prod.release(false, appSvc)
	if err != nil || prod.runs("app") == old {
		t.Fatalf("a second release (%v): %s", err, prod.runs("app"))
	}
	newer := prod.runs("app")
	// The snapshot of r2, its directory named like r2 itself.
	snap := filepath.Join(prod.e.set.Snapshots, r2)
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prod.e.set.BookmarkFrom(snap, prod.e.rel(r2)); err != nil {
		t.Fatal(err)
	}
	if img, err := d.Inspect("app-prod-app:devopsy-app-prod-s" + r2); err != nil || img.ID != newer {
		t.Fatalf("the snapshot's bookmark: %+v %v\n%s", img, err, out.String())
	}
	if err := prod.rollback(r1, false); err != nil {
		t.Fatal(err)
	}
	if id := fileImage(d, prod.e.rel(r1), "app", "app-prod-app"); id != old || prod.runs("app") != old {
		t.Errorf("the rollback's file runs app on %s, runs %s, want %s:\n%s", id, prod.runs("app"), old, out.String())
	}
	// Removing the snapshot untags only its own bookmark: r2 still
	// holds the image.
	if err := prod.e.set.Remove(snap); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Inspect("app-prod-app:devopsy-app-prod-s" + r2); err == nil {
		t.Errorf("the snapshot's bookmark left after its removal")
	}
	if img, err := d.Inspect("app-prod-app:devopsy-app-prod-" + r2); err != nil || img.ID != newer {
		t.Errorf("r2's bookmark gone with the snapshot: %+v %v", img, err)
	}
}

// A snapshot's bookmark fits Docker's 128 characters as a release's does,
// its id one longer (s<id>): a long compose name, a repository named
// twice (its cksum), and the tag still fits.
func TestSnapshotBookmarkLength(t *testing.T) {
	t.Parallel()
	es := []entry{{Ref: "app:1", ID: "sha256:a"}, {Ref: "app:2", ID: "sha256:b"}}
	for _, name := range []string{strings.Repeat("b", 94), strings.Repeat("b", 120)} {
		for _, b := range bookmarks(es, name, "s20261025000000") {
			if _, tag, _ := strings.Cut(b, ":"); len(tag) > 128 {
				t.Errorf("a snapshot's bookmark tag of %d characters: %s", len(tag), tag)
			}
		}
	}
}

// An imported snapshot's held entry keeps the release's own name in the
// current release's repository, its id cleared as the entry's is: the
// other server's word, which a --no-code restore ending the hold would
// otherwise tag as the service's own build.
func TestFilterImportedOwn(t *testing.T) {
	t.Parallel()
	snap, cur := t.TempDir(), t.TempDir()
	if err := writeList(cur, []entry{{Ref: "app-prod-database", ID: "sha256:cur", Service: "database", Kind: KindOwn, DataVersion: "11.8"}}); err != nil {
		t.Fatal(err)
	}
	if err := writeList(snap, []entry{{Ref: "app-stage-database", ID: "sha256:held", Service: "database", Kind: KindOwn, DataVersion: "12.1", OwnRef: "app-stage-database", OwnID: "sha256:own", OwnBuild: "key"}}); err != nil {
		t.Fatal(err)
	}
	if err := FilterImported(snap, cur); err != nil {
		t.Fatal(err)
	}
	es := readList(snap)
	if len(es) != 1 || es[0].ID != "" || es[0].OwnID != "" || es[0].Ref != "app-prod-database" || es[0].OwnRef != "app-prod-database" || es[0].OwnBuild != "key" {
		t.Errorf("the imported listing: %+v", es)
	}
}
