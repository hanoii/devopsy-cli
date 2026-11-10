package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
)

// Review 4 (Opus), a failing test proving a bug: a claim hands buildx's imagetools a name
// Docker's reference grammar refuses and buildx reads as a local path, `oci-layout://<dir>:
// <tag>` (buildx 0.37.2 reads such a layout with `imagetools inspect --raw`), so the deploy
// user's buildx reads and copies a layout the project user names: another project's claim
// under <space>/work while it loads, or any of the deploy user's own files
// (door.TestReview4ClaimTakesOnlyADockerReference).
func TestReview4DoorClaimReadsNoLocalLayout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: fake}
	layout := "oci-layout://" + filepath.Join(dir, "space", "work", "blog-123", "layout") + ":claim"
	_, _, _ = side.claim(layout, filepath.Join(dir, "none.json"))
	if c := readString(filepath.Join(dir, "calls")); strings.Contains(c, "imagetools inspect --raw oci-layout://") {
		t.Errorf("the deploy user's buildx was asked to read a local layout:\n%s", c)
	}
}
