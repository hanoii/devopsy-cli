package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/door"
)

// BUG (review, round 2): a service's image pinned by the digest of a multi-platform index
// (`repo@sha256:<index>`, `repo:tag@sha256:<index>`: what a registry shows for a tag, and what
// tools that pin images write) never starts for a project user. The claim loads the platform's
// manifest under `repo@<manifest digest>`, never the index, and startImage then hands compose
// the reference as given (images.Pin keeps a ref that carries a digest), the index's, which
// nothing loaded: with pull_policy never the start fails on an image that is not there. And
// were that index in the store by another's pull, compose would run Docker's own choice from
// it, an image the door checked nothing of. A pull by that digest left the index, so this
// started before claims loaded.
func TestReview2FableStartOfAnIndexPinRunsWhatTheClaimLoaded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	repo := "registry.example.com/multi"
	amd := fakeManifest(t, dir, plainManifest("multi"), repo+":amd")
	index := fakeManifest(t, dir, `{"schemaVersion":2,"manifests":[{"digest":"`+amd+`","platform":{"os":"linux","architecture":"amd64"}}]}`, repo+":1")
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: dir}, real: fake}
	file := map[string]any{"name": "shop-prod", "services": map[string]any{"web": map[string]any{"image": repo + "@" + index}}}
	if err := side.checkImageLabels(file, filepath.Join(dir, "none.json")); err != nil {
		t.Fatalf("the start's check: %v", err)
	}
	run, _ := file["services"].(map[string]any)["web"].(map[string]any)["image"].(string)
	if !strings.Contains(readString(filepath.Join(dir, "calls")), "\nload "+repo+"@"+amd+"\n") {
		t.Fatalf("the claim loaded no %s@%s:\n%s", repo, amd, readString(filepath.Join(dir, "calls")))
	}
	if _, err := side.inspectImage(run); err != nil {
		t.Errorf("compose is to run %s, which the claim did not load (it loaded %s@%s): %v", run, repo, amd, err)
	}
}

// BUG (review, round 2, minor): checkLayout's blob takes a size below 0 for "any size" (the
// manifest's own blob), and a manifest's descriptor is the registry's to write: a config or a
// layer named with "size": -1 skips the size check the door says it makes of every blob.
func TestReview2FableLayoutChecksASizeBelowZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m := testLayout(t, dir, "a layer's tar")
	path := func(d string) string {
		return filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(d, "sha256:"))
	}
	var manifest map[string]any
	data, err := os.ReadFile(path(m))
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		t.Fatal(err)
	}
	manifest["layers"].([]any)[0].(map[string]any)["size"] = -1
	manifest["config"].(map[string]any)["size"] = -1
	body, _ := json.Marshal(manifest)
	m = digestOf(body)
	write(t, path(m), string(body), 0o644)
	write(t, filepath.Join(dir, "index.json"), `{"schemaVersion":2,"manifests":[{"digest":"`+m+`"}]}`, 0o644)
	if _, _, err := checkLayout(dir, m); err == nil {
		t.Errorf("a manifest naming its config and its layer with size -1 was taken")
	}
}

// BUG (review, round 2): the read of a registry's manifest (registryRead) is bounded by
// doorAnswer, which at its time kills the read's process group (kill(-pid)). That is a group
// only for a read in a session of its own (doorRead's sudo, Setsid); the docker CLI of
// registryRead is a plain command, in the door's own group, so the kill finds no such group
// and does nothing: a registry that accepts and then says nothing holds the door, and the
// release that waits on it, for as long as it likes (buildx sets no time on a response).
func TestReview2FableManifestReadEndsAtItsTime(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := filepath.Join(dir, "docker")
	write(t, fake, "#!/bin/sh\nexec sleep 5\n", 0o755)
	side := &doorSide{real: fake}
	// As registryRead builds it.
	c := side.docker("buildx", "imagetools", "inspect", "--raw", "registry.example.com/app:1")
	start := time.Now()
	_, err := doorAnswer(c, maxManifest, 200*time.Millisecond)
	if took := time.Since(start); err == nil || took > 3*time.Second {
		t.Errorf("a manifest's read bounded to 200ms ended after %s (%v)", took.Round(100*time.Millisecond), err)
	}
}

// BUG (review, round 2, minor): checkLayout keeps a layer's content by its digest alone, read
// as its first naming's kind, so a blob named twice, compressed then plain, is read once,
// decompressed, for both: the second, which containerd reads as it is (its content the blob's
// own hash), passes against a diff_id it does not unpack to. Alone, the same layer is refused
// ("plain read as plain"). No other's content gets in by it (each diff_id is still the hash of
// bytes its registry served), but the door's reading is not containerd's by position.
func TestReview2FableLayerNamedTwiceIsReadAsEachKind(t *testing.T) {
	t.Parallel()
	content := "a layer's tar"
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write([]byte(content))
	w.Close()
	const oci = "application/vnd.oci.image.layer.v1.tar"
	dir := t.TempDir()
	m := testLayoutOf(t, dir, testLayer{oci + "+gzip", gz.Bytes(), content}, testLayer{oci, gz.Bytes(), content})
	if _, _, err := checkLayout(dir, m); err == nil {
		t.Errorf("a gzip blob named again as a plain layer was taken for its decompressed content")
	}
}
