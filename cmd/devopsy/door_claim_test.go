package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/door"
)

// testLayout writes an OCI layout at dir as imagetools writes one, an image manifest of a config
// and layers (each a blob by its hash), and gives the manifest's digest.
func testLayout(t *testing.T, dir string, layers ...string) string {
	t.Helper()
	blob := func(data string) map[string]any {
		d := digestOf([]byte(data))
		write(t, filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(d, "sha256:")), data, 0o644)
		return map[string]any{"digest": d, "size": len(data)}
	}
	ls := []map[string]any{}
	for _, l := range layers {
		ls = append(ls, blob(l))
	}
	body, err := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": blob(`{"os":"linux"}`), "layers": ls})
	if err != nil {
		t.Fatal(err)
	}
	m := blob(string(body))["digest"].(string)
	write(t, filepath.Join(dir, "index.json"), `{"schemaVersion":2,"manifests":[{"digest":"`+m+`"}]}`, 0o644)
	return m
}

// A claim's copy loads only when it is the manifest asked and every blob that manifest names is
// there, a regular file whose sha256 is its name and whose size the manifest's: Docker skips a
// blob its store holds without a word, so a copy missing one would load an image of another's
// blobs. The tar the load reads holds the door's own index and those blobs, once each, nothing
// else of the directory.
func TestCheckLayoutTakesOnlyHashedBlobs(t *testing.T) {
	t.Parallel()
	path := func(dir, data string) string {
		return filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(digestOf([]byte(data)), "sha256:"))
	}
	for name, c := range map[string]struct {
		spoil func(dir, m string) string // the manifest asked
		want  string
	}{
		"another manifest": {func(dir, m string) string {
			write(t, filepath.Join(dir, "index.json"), `{"schemaVersion":2,"manifests":[{"digest":"`+digestOf([]byte("x"))+`"}]}`, 0o644)
			return m
		}, "names no manifest"},
		"a blob not its hash": {func(dir, m string) string { write(t, path(dir, "layer-a"), "layer-b", 0o644); return m }, "is not its hash"},
		"a blob missing": {func(dir, m string) string {
			if err := os.Remove(path(dir, "layer-a")); err != nil {
				t.Fatal(err)
			}
			return m
		}, "lacks the blob"},
		"a blob of another size": {func(dir, m string) string { write(t, path(dir, "layer-a"), "layer-a and more", 0o644); return m }, "lacks the blob"},
		"a link for a blob": {func(dir, m string) string {
			p := path(dir, "layer-a")
			write(t, filepath.Join(dir, "elsewhere"), "layer-a", 0o644)
			if err := os.Remove(p); err != nil || os.Symlink(filepath.Join(dir, "elsewhere"), p) != nil {
				t.Fatal(err)
			}
			return m
		}, "lacks the blob"},
		"an index": {func(dir, m string) string {
			index := `{"schemaVersion":2,"manifests":[{"digest":"` + m + `"}]}`
			write(t, path(dir, index), index, 0o644)
			write(t, filepath.Join(dir, "index.json"), `{"schemaVersion":2,"manifests":[{"digest":"`+digestOf([]byte(index))+`"}]}`, 0o644)
			return digestOf([]byte(index))
		}, "no image manifest"},
	} {
		dir := t.TempDir()
		m := testLayout(t, dir, "layer-a", "layer-b")
		if _, _, err := checkLayout(dir, c.spoil(dir, m)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	// The control: a manifest naming one layer twice, beside a blob it does not name.
	dir := t.TempDir()
	m := testLayout(t, dir, "layer-a", "layer-b", "layer-a")
	write(t, path(dir, "stray"), "stray", 0o644)
	blobs, mediaType, err := checkLayout(dir, m)
	if err != nil || mediaType != "application/vnd.oci.image.manifest.v1+json" || len(blobs) != 4 {
		t.Fatalf("the control: %v, %q, %v", err, mediaType, blobs)
	}
	var buf bytes.Buffer
	if err := writeLayout(&buf, dir, []byte(`{"door":"index"}`), blobs); err != nil {
		t.Fatal(err)
	}
	var names []string
	r := tar.NewReader(&buf)
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if data, _ := io.ReadAll(r); h.Name == "index.json" && string(data) != `{"door":"index"}` {
			t.Errorf("the tar's index is not the door's: %s", data)
		}
	}
	want := []string{"oci-layout", "index.json"}
	for _, b := range blobs {
		want = append(want, "blobs/sha256/"+strings.TrimPrefix(b, "sha256:"))
	}
	if !slices.Equal(names, want) {
		t.Errorf("the tar holds %v, want %v", names, want)
	}
}

// A load that does not say it loaded the image asked (Docker's load exits 0 with its unpacking
// error said, or names another image) records nothing and removes nothing, drops the digest if
// the project had it recorded (the name it may have left is no claim), and leaves no work
// directory; the next claim is a whole one again. One that loads leaves none either. An image
// the project claimed that is gone, its id still recorded, is claimed again in full, and so is
// its digest asked in another repository: a skip is that digest, in the repository asked, of an
// image the project claimed.
func TestDoorClaimThatDoesNotLoadLeavesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	private := "registry.example.com/blog/private"
	d := fakeManifest(t, dir, plainManifest("private"), private+":1")
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: fake}
	write(t, side.policy.ImagesFile(), d+"\n", 0o600)
	side.policy.Images = []string{d}
	calls := filepath.Join(dir, "calls")
	claim := func(ref string) error {
		t.Helper()
		os.Remove(calls)
		_, _, err := side.claim(ref, filepath.Join(dir, "none.json"))
		if work, _ := os.ReadDir(filepath.Join(side.policy.Space, "work")); len(work) > 0 {
			t.Errorf("%s left work: %v", ref, work)
		}
		return err
	}
	copied := func() bool { return strings.Contains(readString(calls), "imagetools create ") }
	for name, says := range map[string]string{
		"unpacked": "Loaded image: " + private + "@" + d + "\nError unpacking image " + private + "@" + d + ": content digest not found\n",
		"another":  "Loaded image: registry.example.com/blog/other@" + d + "\n",
	} {
		write(t, filepath.Join(dir, "loadsays"), says, 0o644)
		if err := claim(private + ":1"); err == nil || !strings.Contains(err.Error(), "did not load") || !copied() {
			t.Errorf("%s: a load that did not say the image (%v):\n%s", name, err, readString(calls))
		}
		if got := readString(side.policy.ImagesFile()); got != "" || slices.Contains(side.policy.Images, d) {
			t.Errorf("%s: recorded after a failed load: %q %v", name, got, side.policy.Images)
		}
		if strings.Contains(readString(calls), "image rm ") || strings.Contains(readString(calls), "\ntag ") {
			t.Errorf("%s: a failed load removed or tagged:\n%s", name, readString(calls))
		}
	}
	// The name the failed loads left is no skip.
	os.Remove(filepath.Join(dir, "loadsays"))
	if err := claim(private + ":1"); err != nil || !copied() || readString(side.policy.ImagesFile()) != d+"\n" {
		t.Errorf("the claim after a failed load (%v), copied %v, recorded %q:\n%s", err, copied(), readString(side.policy.ImagesFile()), readString(calls))
	}
	// Its digest in another repository, which the registry serves there too.
	fakeManifest(t, dir, plainManifest("private"), "registry.example.com/mirror:1")
	if err := claim("registry.example.com/mirror:1"); err != nil || !copied() {
		t.Errorf("the digest asked in another repository was taken without its claim (%v):\n%s", err, readString(calls))
	}
}

// A door cut by a signal while it copies an image (a release cancelled mid-fetch) stops the
// copy and removes its work directory, which would otherwise hold the image for a day, and ends
// with the signal's exit. In a helper process (doorComposeHelper), since the door then exits.
func TestDoorClaimCutBySignalLeavesNoWork(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	doorImageFake(t, dir)
	private := "registry.example.com/blog/private"
	fakeManifest(t, dir, plainManifest("private"), private+":1")
	write(t, filepath.Join(dir, "slowcopy"), "", 0o644)
	c := exec.Command(os.Args[0], "-test.run", "^$")
	c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_REQUEST=tag "+private+":1 busybox:devopsy-shop-prod-20260101000000")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(startWait); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "copying")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = c.Process.Kill()
			t.Fatalf("the copy never started:\n%s", readString(filepath.Join(dir, "calls")))
		}
	}
	if work, _ := os.ReadDir(filepath.Join(dir, "space", "work")); len(work) == 0 {
		t.Fatal("the copy has no work directory to remove")
	}
	if err := c.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 128+int(syscall.SIGTERM) {
			t.Errorf("the door's exit on SIGTERM: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = c.Process.Kill()
		t.Fatal("the door did not end on SIGTERM")
	}
	if work, _ := os.ReadDir(filepath.Join(dir, "space", "work")); len(work) > 0 {
		t.Errorf("the cut copy left work: %v", work)
	}
}
