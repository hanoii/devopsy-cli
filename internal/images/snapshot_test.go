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

// An imported snapshot's removal takes no name from its tools file
// (ToolsFile), which is the other server's word as its listing is: the
// listing is cut to the current release's services with its ids cleared
// (FilterImported), and the tools must not stay as they came.
//
// Bug (review round 1 of feat/tool-bookmarks): a take copies its release's
// files, devopsy-tools among them, an import keeps the tar's, and
// FilterImported rewrites the listing alone, so Remove of the imported
// snapshot reads a crafted line (any name, any image id on this server)
// and strips that name from an image no bookmark holds and no container
// uses: the admin's own image here.
func TestImportedSnapshotTools(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	stage := newSim(t, d, "app-stage", &out)
	stage.e.set.Snapshots = filepath.Join(stage.base(), "snapshots")
	if _, err := stage.release(false, appSvc); err != nil {
		t.Fatal(err)
	}
	// An image of the server's that no release of devopsy's holds.
	d.pull("nginx:1.27", "sha256:n1")
	// The snapshot as an import extracted it: a listing and a tools file,
	// both whoever made the tar's.
	dir := filepath.Join(stage.e.set.Snapshots, "20261017000002")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeList(dir, []entry{{Ref: "app-prod-app", ID: "sha256:other", Service: "app", Kind: KindOwn}}); err != nil {
		t.Fatal(err)
	}
	if err := writeEntries(filepath.Join(dir, ToolsFile), []entry{{Ref: "nginx:1.27", ID: "sha256:n1", Service: "tool", Kind: KindPulled}}); err != nil {
		t.Fatal(err)
	}
	if err := FilterImported(dir, stage.e.rel(stage.cur)); err != nil {
		t.Fatal(err)
	}
	if err := stage.e.set.BookmarkFrom(dir, dir); err != nil {
		t.Fatal(err)
	}
	if err := stage.e.set.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if d.names["nginx:1.27"] != "sha256:n1" {
		t.Errorf("the imported snapshot's removal took nginx:1.27, which its tools file alone named: %v\n%s", d.names, out.String())
	}
}
