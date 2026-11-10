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

// BUG (review, round 1): a claim's proof checks a config's diff_ids only on a project builder
// whose BuildKit is 0.34.0 or later, and nothing reads the builder's BuildKit version. A
// builder is made once (ensureBuilder, its image whatever moby/buildkit:buildx-stable-1 was
// that day) and kept as long as its network is right, so one booted on an older BuildKit
// proves nothing of the second forgery (a config naming another's diff_ids over junk layers):
// `buildx build --output type=tar` of such an image exits 0 on BuildKit 0.13.2, 0.20.2,
// 0.26.3, 0.28.0, 0.30.0, 0.32.0 and 0.33.0, and fails with "failed to verify layer ...
// expected diffID" only from 0.34.0 (tried by hand against a registry serving a valid layer
// under a config naming another diff_id). The copy and the load that follow take it: every
// blob is there and is its hash, and Docker's unpack maps the diff_ids onto the snapshots it
// holds. The builder below answers `buildx inspect` as a real one on 0.33.0 does: its claim
// must refuse (or make the builder again) before anything loads.
func TestReviewFableClaimNeedsABuilderThatChecksDiffIDs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := doorImageFake(t, dir)
	fake := filepath.Join(dir, "docker-old-builder")
	write(t, fake, `#!/bin/sh
case "$*" in
  'buildx inspect '*)
    cat <<'EOF'
Name:          devopsy-shop
Driver:        docker-container
Last Activity: 2026-01-01 00:00:00 +0000 UTC

Nodes:
Name:                  devopsy-shop0
Endpoint:              unix:///var/run/docker.sock
Driver Options:        network="devopsy-shop-build"
Status:                running
BuildKit daemon flags: --allow-insecure-entitlement=network.host
BuildKit version:      v0.33.0
Platforms:             linux/amd64
EOF
    exit 0 ;;
esac
exec `+base+` "$@"
`, 0o755)
	app := "registry.example.com/blog/app"
	fakeManifest(t, dir, plainManifest("app"), app+":1")
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: fake}
	_, _, err := side.claim(app+":1", filepath.Join(dir, "none.json"))
	calls := readString(filepath.Join(dir, "calls"))
	if err == nil && strings.Contains(calls, "\nload") && !strings.Contains(calls, "buildx create") && !strings.Contains(calls, "buildx rm") {
		t.Errorf("a claim proven on a builder of BuildKit 0.33.0, which checks no diff_ids, loaded and recorded %q:\n%s", readString(side.policy.ImagesFile()), calls)
	}
}

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
