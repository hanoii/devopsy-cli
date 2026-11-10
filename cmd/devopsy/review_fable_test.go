package main

import (
	"io"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
)

// BUG (review, round 1): a claim drops the digest from the project's record on any failure of
// its fetch, the copy's included (a registry that is down, a full disk), not only a load that
// failed: nothing was loaded, so no name was left for a skip to take. An image the project
// claimed and still has, under one repository, stops being its own when a claim of the same
// digest in another repository (a mirror, a promoted tag) fails at its copy: its next start
// is a whole claim again, and refused while its registry is away, where the image on disk
// would have started.
func TestReviewFableFailedCopyKeepsTheRecord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	app, mirror := "registry.example.com/blog/app", "mirror.example.com/blog/app"
	d := fakeManifest(t, dir, plainManifest("app"), app+":1")
	fakeManifest(t, dir, plainManifest("app"), mirror+":1")
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: fake}
	if _, _, err := side.claim(app+":1", filepath.Join(dir, "none.json")); err != nil || readString(side.policy.ImagesFile()) != d+"\n" {
		t.Fatalf("the control, a first claim (%v): %q", err, readString(side.policy.ImagesFile()))
	}
	// The mirror serves the manifest, then not its blobs.
	write(t, filepath.Join(dir, "uncopied"), mirror+"@"+d+"\n", 0o644)
	if _, _, err := side.claim(mirror+":1", filepath.Join(dir, "none.json")); err == nil || !strings.Contains(err.Error(), "did not serve") {
		t.Fatalf("the control, a copy that fails: %v", err)
	}
	if strings.Contains(readString(filepath.Join(dir, "calls")), "\nload "+mirror) {
		t.Fatal("the control: the failed copy loaded")
	}
	if got := readString(side.policy.ImagesFile()); got != d+"\n" || !slices.Contains(side.policy.Images, d) {
		t.Errorf("a copy that failed, nothing loaded, took the project's image %s out of its record: %q", d, got)
	}
}

// BUG (review, round 1): checkLayout and writeLayout read each blob whole into memory
// (os.ReadFile), once to hash it and once more to write it into the tar, in the deploy user's
// door: a layer of several GB (not rare, and any project user can name an image that has one)
// needs that much memory twice over, on a server whose other projects share it. A blob is
// hashed and copied as a stream. Here a layer of 256 MB costs the check and the tar more than
// 32 MB of allocations.
//
// Not parallel: it reads the process's allocations.
func TestReviewFableLayoutStreamsItsBlobs(t *testing.T) {
	dir := t.TempDir()
	layer := strings.Repeat("0123456789abcdef", 16<<20)
	m := testLayout(t, dir, layer)
	layer = ""
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	blobs, _, err := checkLayout(dir, m)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLayout(io.Discard, dir, []byte(`{}`), blobs); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if got := (after.TotalAlloc - before.TotalAlloc) >> 20; got > 32 {
		t.Errorf("checking and writing a layout of one 256 MB layer allocated %d MB: its blobs are read whole", got)
	}
}
