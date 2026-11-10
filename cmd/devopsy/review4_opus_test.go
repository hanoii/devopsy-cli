package main

import (
	"io"
	"os"
	"os/exec"
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

// Review 4 (Opus): what the claim above does on a real server, past reading. buildx's
// imagetools makes the directory an oci-layout:// reference names, as whoever runs it (here
// the deploy user), before it finds no index there. So a project user's `docker pull -q
// oci-layout://<DoorDir>/sync-keys/devopsy-blog:x` makes another project user's sync keys
// file a directory, which --authorized-keys then fails on (printAuthorizedKeys, below), and
// sshd takes no key of that user's: one project locks another out of the server, and any
// path the deploy user may write that is not there yet (another's record of claimed images,
// its logins' config.json, an entry) becomes a directory. Skipped without docker buildx.
func TestReview4BuildxMakesTheLayoutItIsNamed(t *testing.T) {
	t.Parallel()
	if exec.Command("docker", "buildx", "version").Run() != nil {
		t.Skip("no docker buildx")
	}
	dir := t.TempDir()
	named := filepath.Join(dir, "door", "sync-keys", "devopsy-blog")
	_ = exec.Command("docker", "buildx", "imagetools", "inspect", "--raw", "oci-layout://"+named+":x").Run()
	if fi, err := os.Stat(named); err != nil || !fi.IsDir() {
		t.Skipf("this buildx made no directory there (%v): the read alone is the bug", err)
	}
	if code := printAuthorizedKeys(io.Discard, named); code == 0 {
		t.Errorf("--authorized-keys took a directory for a user's sync keys")
	}
	t.Errorf("buildx made %s, as the deploy user would on a server, and --authorized-keys now fails for that user", named)
}
