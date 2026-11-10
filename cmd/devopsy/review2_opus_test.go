package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
)

// Review round 2 (Opus). Bug: a claim of a reference pinned to an index's digest
// (<repo>@<index digest>, the usual pin of a multi-platform image) never checks the index's
// bytes against that digest (platformManifest checks a pinned manifest's bytes only): its
// registry serves any index, the door copies and loads the entry it names for the platform,
// and a request's Docker command then takes that entry's digest for the pin (a tag's source,
// an inspect's target), so a hold of the project's is tagged onto an image its pin never named.
func TestReviewOpusClaimChecksAPinnedIndexsBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	repo := "registry.example.com/multi"
	amd := fakeManifest(t, dir, plainManifest("substituted"), repo+":amd64")
	index := `{"schemaVersion":2,"manifests":[{"digest":"` + amd + `","platform":{"os":"linux","architecture":"amd64"}}]}`
	pinned := repo + "@" + digestOf([]byte("the index the project pinned"))
	key := strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace
	write(t, filepath.Join(dir, "raw", key(pinned)), index, 0o644)
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: fake}
	if d, _, err := side.claim(pinned, filepath.Join(dir, "none.json")); err == nil {
		t.Errorf("%s, its registry serving another index, claimed as %s", pinned, d)
	}
}

// Review round 2 (Opus). Bug: a start of an image pinned to an index's digest
// (busybox:1.37@sha256:<index>) claims the index's entry for the platform, loaded as
// <repo>@<entry>, and hands compose the reference as given (images.Pin keeps a pinned one),
// which names no image the store has: Docker's containerd store resolves <repo>@<digest> only
// by an image whose target is that digest (checked on Docker 29.8 with the containerd store:
// after a pull of busybox@<arm64 entry>, `docker run --pull never busybox@<index>` says "No
// such image"), so every start of such a service fails under pull_policy never.
func TestReviewOpusStartOfAPinnedIndexRunsWhatItLoaded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	repo := "registry.example.com/multi"
	amd := fakeManifest(t, dir, plainManifest("multi-amd64"), repo+":amd64")
	index := `{"schemaVersion":2,"manifests":[{"digest":"` + amd + `","platform":{"os":"linux","architecture":"amd64"}}]}`
	idx := fakeManifest(t, dir, index, repo+":1")
	// The registry serves it by its tag and digest too, as a registry does.
	write(t, filepath.Join(dir, "raw", strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(repo+":1@"+idx)), index, 0o644)
	write(t, filepath.Join(dir, "store"), "", 0o644)
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: fake}
	file := map[string]any{"name": "shop-prod", "services": map[string]any{"web": map[string]any{"image": repo + ":1@" + idx}}}
	if err := side.checkImageLabels(file, filepath.Join(dir, "none.json")); err != nil {
		t.Fatal(err)
	}
	got, _ := file["services"].(map[string]any)["web"].(map[string]any)["image"].(string)
	if _, err := side.inspectImage(got); err != nil {
		t.Errorf("compose gets %q, which the store does not have after the claim loaded %s@%s:\n%s", got, repo, amd, readString(filepath.Join(dir, "store")))
	}
}
