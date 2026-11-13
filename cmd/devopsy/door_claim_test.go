package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/klauspost/compress/zstd"
)

// testLayout writes an OCI layout at dir as imagetools writes one, an image manifest of a config
// and plain layers (each a blob by its hash, its content its diff_id), and gives the manifest's
// digest.
func testLayout(t *testing.T, dir string, layers ...string) string {
	t.Helper()
	plain := make([]testLayer, len(layers))
	for i, l := range layers {
		plain[i] = testLayer{"application/vnd.oci.image.layer.v1.tar", []byte(l), l}
	}
	return testLayoutOf(t, dir, plain...)
}

// testLayer is a layer of testLayoutOf: its media type, its blob, and the content its config
// names (its diff_id's).
type testLayer struct {
	kind    string
	blob    []byte
	content string
}

// testLayoutOf is testLayout of layers as given.
func testLayoutOf(t *testing.T, dir string, layers ...testLayer) string {
	t.Helper()
	blob := func(data string) map[string]any {
		d := digestOf([]byte(data))
		write(t, filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(d, "sha256:")), data, 0o644)
		return map[string]any{"digest": d, "size": len(data)}
	}
	ls := []map[string]any{}
	diffIDs := []string{}
	for _, l := range layers {
		b := blob(string(l.blob))
		b["mediaType"] = l.kind
		ls = append(ls, b)
		diffIDs = append(diffIDs, digestOf([]byte(l.content)))
	}
	config, err := json.Marshal(map[string]any{"os": "linux", "rootfs": map[string]any{"type": "layers", "diff_ids": diffIDs}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": blob(string(config)), "layers": ls})
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
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: fake, imagesRoot: dir}
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

// A door cut by a signal while it copies an image (a release cancelled mid-fetch)
// stops what it runs and removes its work directories, the request's others too, which would
// otherwise hold the image for a day, and ends with the signal's exit. In a helper process (doorComposeHelper), since
// the door then exits.
func TestDoorClaimCutBySignalLeavesNoWork(t *testing.T) {
	t.Parallel()
	for slow, mark := range map[string]string{"slowcopy": "copying"} {
		dir := t.TempDir()
		doorImageFake(t, dir)
		private := "registry.example.com/blog/private"
		fakeManifest(t, dir, plainManifest("private"), private+":1")
		write(t, filepath.Join(dir, slow), "", 0o644)
		c := exec.Command(os.Args[0], "-test.run", "^$")
		c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_WORK=1", "DEVOPSY_TEST_DOOR_REQUEST=tag "+private+":1 busybox:devopsy-shop-prod-20260101000000")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(startWait); ; time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(dir, mark)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				_ = c.Process.Kill()
				t.Fatalf("%s: never started:\n%s", slow, readString(filepath.Join(dir, "calls")))
			}
		}
		if work, _ := os.ReadDir(filepath.Join(dir, "space", "work")); len(work) < 2 {
			t.Fatalf("%s: not the request's work directory and the claim's to remove: %v", slow, work)
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
				t.Errorf("%s: the door's exit on SIGTERM: %v", slow, err)
			}
		case <-time.After(10 * time.Second):
			_ = c.Process.Kill()
			t.Fatalf("%s: the door did not end on SIGTERM", slow)
		}
		if work, _ := os.ReadDir(filepath.Join(dir, "space", "work")); len(work) > 0 {
			t.Errorf("%s: the cut claim left work: %v", slow, work)
		}
		// What it ran went with it, the docker CLI's whole process group.
		pid, _ := strconv.Atoi(strings.TrimSpace(readString(filepath.Join(dir, "child"))))
		for deadline := time.Now().Add(10 * time.Second); pid > 0 && syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Errorf("%s: what the claim ran (%d) outlived the door", slow, pid)
				break
			}
		}
	}
}

// Compose's pull verb, for a project user, is the door's own claim of each image it selects
// (door.PlanPull), compose's own reading of the profiles deciding what is active: compose's
// pull never runs, so Docker's daemon contacts no registry the project names. Each claim that
// fetches says so on stderr, and its time once through, none with -q; the builder is read once
// for all. A claim that fails (a tag its registry does not have) says so and ends the pull
// with 1, unlogged, the claims after it not made, or with --ignore-pull-failures each failure
// said and 0. A refusal (an image no pull may write, a service the file does not have) comes
// before any claim, exit 2, logged. In a helper process (doorComposeHelper), with the fake
// docker.
func TestDoorPullIsTheDoorsClaims(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	doorImageFake(t, dir)
	// Every read as the project user (its docker config's among them) is logged.
	write(t, filepath.Join(dir, "sudo"), "#!/bin/sh\nshift 5\necho \"$1\" >>"+filepath.Join(dir, "reads")+"\n[ \"$1\" = real ] || exit 1\nprintf '%s\\n' \"$2\"\n", 0o755)
	private, other, built := "registry.example.com/blog/private:1", "registry.example.com/other:1", "registry.example.com/built:1"
	fakeManifest(t, dir, plainManifest("private"), private)
	fakeManifest(t, dir, plainManifest("other"), other)
	svc := func(image, extra string) string {
		return `{"image":"` + image + `","user":"10001:10001","security_opt":["no-new-privileges"]` + extra + `}`
	}
	file := `{"name":"shop-prod","services":{"web":` + svc(private, "") + `,"extra":` + svc(other, `,"profiles":["debug"]`) +
		`,"built":` + svc(built, `,"build":{"context":"."}`) + `,"own":` + svc("devopsy.internal/shop-prod/own", "") + `}}`
	log := filepath.Join(dir, "refused.log")
	calls := filepath.Join(dir, "calls")
	pull := func(file, profiles, verb string) (string, int) {
		t.Helper()
		os.Remove(calls)
		os.Remove(log)
		os.Remove(filepath.Join(dir, "reads"))
		c := exec.Command(os.Args[0], "-test.run", "^$")
		c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_VERB="+verb, "DEVOPSY_TEST_DOOR_FILE="+file,
			"DEVOPSY_TEST_DOOR_PROFILES="+profiles, "DEVOPSY_TEST_DOOR_LOG="+log, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := c.CombinedOutput()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		if strings.Contains(readString(calls), "compose ") && strings.Contains(readString(calls), " pull") {
			t.Errorf("%s: compose's pull ran:\n%s", verb, readString(calls))
		}
		return string(out), c.ProcessState.ExitCode()
	}
	copies := func() int { return strings.Count(readString(calls), "imagetools create ") }
	if out, code := pull(file, "", "pull -q --ignore-buildable"); code != 0 || copies() != 1 || strings.Contains(out, "pull") {
		t.Errorf("a quiet pull (exit %d, %d copies):\n%s", code, copies(), out)
	}
	// Under the profile, extra is active too: web, claimed already, is no fetch.
	out, code := pull(file, "debug", "pull --ignore-buildable")
	if code != 0 || copies() != 1 || !strings.Contains(out, "devopsy: pulling "+other+"\n") || !strings.Contains(out, "devopsy: pulled "+other+" (") || strings.Contains(out, private) {
		t.Errorf("a pull under a profile (exit %d, %d copies):\n%s\n%s", code, copies(), out, readString(calls))
	}
	if n := strings.Count(readString(calls), "buildx create "); n != 1 {
		t.Errorf("the builder read %d times for two claims:\n%s", n, readString(calls))
	}
	if n := strings.Count(readString(filepath.Join(dir, "reads")), "cat\n"); n != 1 {
		t.Errorf("the logins read %d times for two claims", n)
	}
	// Compose's reading of the profiles gets no build block, which the pull's checks do not read.
	if got := readString(filepath.Join(dir, "configread")); got == "" || strings.Contains(got, `"build"`) {
		t.Errorf("compose's config read of the pull's file: %s", got)
	}
	// The built service's tag is not in its registry: first by name, it ends the pull.
	out, code = pull(file, "debug", "pull")
	if code != 1 || copies() != 0 || !strings.Contains(out, "devopsy: error: ") || readString(log) != "" {
		t.Errorf("a pull whose first claim fails (exit %d, %d copies, logged %q):\n%s", code, copies(), readString(log), out)
	}
	if out, code := pull(file, "debug", "pull --ignore-pull-failures"); code != 0 || !strings.Contains(out, built) || !strings.Contains(readString(calls), "imagetools inspect --raw "+private) {
		t.Errorf("a pull past its failures (exit %d):\n%s\n%s", code, out, readString(calls))
	}
	// A registry whose copy is not what it names: the door's refusal, logged, past
	// --ignore-pull-failures too.
	restore := spoilCopy(t, dir, images.RepoOf(other)+"@"+digestOf([]byte(plainManifest("other"))), `{"name":"other"}`)
	os.Remove(filepath.Join(dir, "space", "images", "devopsy-shop"))
	if out, code := pull(file, "debug", "pull --ignore-buildable --ignore-pull-failures"); code != 2 || readString(log) == "" {
		t.Errorf("a copy not its hash under the pull (exit %d, logged %q):\n%s", code, readString(log), out)
	}
	restore()
	for name, c := range map[string]struct{ file, verb string }{
		"an unknown service": {file, "pull web nosuch"},
		"another's hold":     {`{"name":"shop-prod","services":{"web":` + svc(private, "") + `,"held":` + svc("busybox:devopsy-blog-prod-20240101000000", "") + `}}`, "pull"},
	} {
		if out, code := pull(c.file, "", c.verb); code != 2 || strings.Contains(readString(calls), "imagetools") || readString(log) == "" {
			t.Errorf("%s (exit %d, logged %q):\n%s\n%s", name, code, readString(log), out, readString(calls))
		}
	}
}

// A claim takes only the bytes and the image it asked for: a ref by digest whose registry
// serves other bytes, a manifest larger than a registry's own limit (which the door would hold
// whole), and a load whose image goes by another id than the manifest's digest are refused,
// nothing recorded.
func TestDoorClaimTakesOnlyWhatItAsked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	private := "registry.example.com/blog/private"
	d := fakeManifest(t, dir, plainManifest("private"), private+":1")
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: fake, imagesRoot: dir}
	key := strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace
	other := digestOf([]byte("other"))
	write(t, filepath.Join(dir, "raw", key(private+"@"+other)), plainManifest("private"), 0o644)
	write(t, filepath.Join(dir, "raw", key(private+":big")), `{"config":{},"x":"`+strings.Repeat("x", maxManifest)+`"}`, 0o644)
	for name, c := range map[string]struct{ ref, want string }{
		"other bytes by digest": {private + "@" + other, "served other bytes"},
		"a manifest too large":  {private + ":big", "do not pull it"},
	} {
		if _, _, err := side.claim(c.ref, filepath.Join(dir, "none.json")); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	write(t, filepath.Join(dir, "loadid"), "sha256:another", 0o644)
	if _, _, err := side.claim(private+":1", filepath.Join(dir, "none.json")); err == nil || !strings.Contains(err.Error(), "not the "+d+" loaded") {
		t.Errorf("a load under another id: %v", err)
	}
	if got := readString(side.policy.ImagesFile()); got != "" {
		t.Errorf("recorded: %q", got)
	}
}

// A layer's content, decompressed as containerd unpacks it, is the one its config names, by
// position: a gzip, a zstd (behind a skippable frame too) and a plain layer are taken; a config
// naming another layer's content over a layer of junk (whose blob is still its hash) is
// refused, so a load never lands on the layers the server holds for another's diff_ids; and so
// are a count of layers the config does not name, and a layer of a kind containerd would not
// pair with a diff_id (a foreign one, an encrypted one).
func TestCheckLayoutTakesOnlyTheContentItsConfigNames(t *testing.T) {
	t.Parallel()
	content := "a layer's tar"
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write([]byte(content))
	w.Close()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	zs := enc.EncodeAll([]byte(content), nil)
	skippable := append([]byte{0x50, 0x2a, 0x4d, 0x18, 4, 0, 0, 0, 'n', 'o', 't', 'e'}, zs...)
	const (
		oci  = "application/vnd.oci.image.layer.v1.tar"
		ogz  = oci + "+gzip"
		ozst = oci + "+zstd"
		dgz  = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	)
	taken := []testLayer{{ogz, gz.Bytes(), content}, {dgz, gz.Bytes(), content}, {ozst, zs, content}, {ozst, skippable, content}, {oci, []byte(content), content}}
	dir := t.TempDir()
	if _, _, err := checkLayout(dir, testLayoutOf(t, dir, taken...)); err != nil {
		t.Errorf("gzip, zstd and plain layers: %v", err)
	}
	for name, c := range map[string]struct {
		layers []testLayer
		want   string
	}{
		"another's content over junk": {[]testLayer{{ogz, gz.Bytes(), "another project's layer"}}, "is not the content its config names"},
		"plain read as plain":         {[]testLayer{{oci, gz.Bytes(), content}}, "is not the content its config names"},
		"a foreign layer":             {[]testLayer{{"application/vnd.docker.image.rootfs.foreign.diff.tar.gzip", gz.Bytes(), content}}, "of a kind the door does not take"},
		"an encrypted layer":          {[]testLayer{{ogz + "+encrypted", gz.Bytes(), content}}, "of a kind the door does not take"},
	} {
		dir := t.TempDir()
		if _, _, err := checkLayout(dir, testLayoutOf(t, dir, c.layers...)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	// A manifest naming more layers than its config.
	dir = t.TempDir()
	m := testLayoutOf(t, dir, testLayer{oci, []byte(content), content})
	var manifest map[string]any
	data, _ := os.ReadFile(filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(m, "sha256:")))
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["layers"] = append(manifest["layers"].([]any), manifest["layers"].([]any)[0])
	more, _ := json.Marshal(manifest)
	m = digestOf(more)
	write(t, filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(m, "sha256:")), string(more), 0o644)
	write(t, filepath.Join(dir, "index.json"), `{"schemaVersion":2,"manifests":[{"digest":"`+m+`"}]}`, 0o644)
	if _, _, err := checkLayout(dir, m); err == nil || !strings.Contains(err.Error(), "names 2 layers and its config 1") {
		t.Errorf("more layers than its config names: %v", err)
	}
}

// A claim's read of a registry ends at its time, what it ran (the docker CLI and the plugin it
// runs) killed whole: a registry that answers and then says nothing would hold the door, and
// the release, for good.
func TestDoorClaimReadEndsAtItsTime(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	side := &doorSide{policy: door.Policy{Project: "shop", Space: filepath.Join(dir, "space")}}
	g := side.claimCut()
	defer g.end()
	c := exec.Command("sh", "-c", "sleep 30 & echo $! >"+filepath.Join(dir, "child")+"; wait")
	start := time.Now()
	if err := g.runWithin(c, 200*time.Millisecond); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("a read bounded to 200ms ended after %s (%v)", time.Since(start).Round(100*time.Millisecond), err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(readString(filepath.Join(dir, "child"))))
	for deadline := time.Now().Add(10 * time.Second); pid > 0 && syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("the read's child (%d) outlived its time", pid)
			break
		}
	}
}

// A claim whose copy fails (a registry that is down, a full disk) leaves the project's record
// as it was: nothing was loaded, so no name was left for a skip to take, and an image the
// project claimed and still has under one repository stays its own when a claim of the same
// digest in another fails at its copy.
func TestDoorClaimFailedCopyKeepsTheRecord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	app, mirror := "registry.example.com/blog/app", "mirror.example.com/blog/app"
	d := fakeManifest(t, dir, plainManifest("app"), app+":1")
	fakeManifest(t, dir, plainManifest("app"), mirror+":1")
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: fake, imagesRoot: dir}
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

// A layout's check and its tar stream each blob, never holding one whole: a layer may be
// larger than the door's memory, and an image a project names could push it towards the OOM
// killer, taking other projects' processes with it.
//
// Not parallel: it measures the process's allocations (runtime.MemStats).
func TestCheckLayoutStreamsItsBlobs(t *testing.T) {
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

// The door's pull skips a service under pull_policy missing (or if_not_present) whose image is
// here and the project's, as compose's pull skips one it has ("Skipped Image is already present
// locally"), asking no registry: a release's fetch keeps the image it has, even when its
// registry moved the tag or no longer has it.
func TestDoorPullKeepsAPresentImageUnderMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	doorImageFake(t, dir)
	write(t, filepath.Join(dir, "sudo"), "#!/bin/sh\nshift 5\n[ \"$1\" = real ] || exit 1\nprintf '%s\\n' \"$2\"\n", 0o755)
	private := "registry.example.com/blog/private:1"
	d := fakeManifest(t, dir, plainManifest("private"), private)
	// Here, by its tag and its digest, and the project's.
	repo := images.RepoOf(private)
	write(t, filepath.Join(dir, "store"), private+"|"+d+"|"+repo+"@"+d+"|"+private+"|{}\n"+repo+"@"+d+"|"+d+"|"+repo+"@"+d+"|"+private+"|{}\n", 0o644)
	side := door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}
	write(t, side.ImagesFile(), d+"\n", 0o600)
	// Its registry moved the tag since.
	fakeManifest(t, dir, plainManifest("private2"), private)
	file := `{"name":"shop-prod","services":{"web":{"image":"` + private + `","pull_policy":"missing","user":"10001:10001","security_opt":["no-new-privileges"]}}}`
	c := exec.Command(os.Args[0], "-test.run", "^$")
	c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_VERB=pull", "DEVOPSY_TEST_DOOR_FILE="+file,
		"DEVOPSY_TEST_DOOR_LOG="+filepath.Join(dir, "refused.log"), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := c.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	if calls := readString(filepath.Join(dir, "calls")); strings.Contains(calls, "imagetools") {
		t.Errorf("a pull under pull_policy missing of an image here asked its registry, as compose's does not (exit %d):\n%s\n%s", c.ProcessState.ExitCode(), out, calls)
	}
}

// A claim checks buildx's version (hostReady, dockerFloor) under the DOCKER_CONFIG its copy
// runs with: Docker finds a plugin in $DOCKER_CONFIG/cli-plugins first, so the deploy user's own
// ~/.docker could hold another buildx than the one that copies, and an older one there fails
// the copy in words the floor is there to replace.
func TestDoorClaimChecksTheBuildxThatCopies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	envCalls := filepath.Join(dir, "envcalls")
	wrap := filepath.Join(dir, "docker-wrap")
	write(t, wrap, "#!/bin/sh\necho \"${DOCKER_CONFIG-unset}|$*\" >>"+envCalls+"\nexec "+fake+" \"$@\"\n", 0o755)
	private := "registry.example.com/blog/private"
	fakeManifest(t, dir, plainManifest("private"), private+":1")
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: wrap, imagesRoot: dir}
	if _, _, err := side.claim(private+":1", filepath.Join(dir, "none.json")); err != nil {
		t.Fatal(err)
	}
	config := map[string]string{}
	for _, line := range strings.Split(readString(envCalls), "\n") {
		cfg, args, _ := strings.Cut(line, "|")
		switch {
		case args == "buildx version":
			config["version"] = cfg
		case strings.HasPrefix(args, "buildx imagetools create "):
			config["copy"] = cfg
		}
	}
	if config["version"] != config["copy"] {
		t.Errorf("buildx's version read with DOCKER_CONFIG %q, its copy run with %q:\n%s", config["version"], config["copy"], readString(envCalls))
	}
}

// A descriptor's size below 0 (a config's, a layer's, the registry's to write) is refused,
// never read as "any size": every blob's size is checked.
func TestCheckLayoutRefusesASizeBelowZero(t *testing.T) {
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

// A bounded read (doorAnswer) with no session of its own still ends at its time: its kill goes
// to the read's own process group, which it gets.
func TestDoorAnswerEndsAReadWithNoSessionAtItsTime(t *testing.T) {
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

// A blob a manifest names twice, as two kinds, is read as each (containerd reads each position
// by its own media type): one named compressed and then plain is checked as plain the second
// time, and refused when that content is not its config's.
func TestCheckLayoutReadsALayerNamedTwiceAsEachKind(t *testing.T) {
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

// A ref pinned by an index's digest takes only the index of those bytes: one its registry
// serves otherwise is refused before any entry of it is read, so the registry never chooses
// the image a tag or an inspect then acts on.
func TestDoorClaimChecksAPinnedIndexsBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	repo := "registry.example.com/multi"
	amd := fakeManifest(t, dir, plainManifest("substituted"), repo+":amd64")
	index := `{"schemaVersion":2,"manifests":[{"digest":"` + amd + `","platform":{"os":"linux","architecture":"amd64"}}]}`
	pinned := repo + "@" + digestOf([]byte("the index the project pinned"))
	key := strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace
	write(t, filepath.Join(dir, "raw", key(pinned)), index, 0o644)
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: fake, imagesRoot: dir}
	if d, _, err := side.claim(pinned, filepath.Join(dir, "none.json")); err == nil {
		t.Errorf("%s, its registry serving another index, claimed as %s", pinned, d)
	}
}

// Under --ignore-pull-failures the pull says each failed claim as a warning and exits 0: an
// error is what fails the command, and a line in red in a release that succeeded reads as a
// failure.
func TestDoorPullPastFailuresWarns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	doorImageFake(t, dir)
	write(t, filepath.Join(dir, "sudo"), "#!/bin/sh\nshift 5\n[ \"$1\" = real ] || exit 1\nprintf '%s\\n' \"$2\"\n", 0o755)
	file := `{"name":"shop-prod","services":{"web":{"image":"registry.example.com/gone:1","user":"10001:10001","security_opt":["no-new-privileges"]}}}`
	c := exec.Command(os.Args[0], "-test.run", "^$")
	c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_VERB=pull --ignore-pull-failures", "DEVOPSY_TEST_DOOR_FILE="+file,
		"DEVOPSY_TEST_DOOR_LOG="+filepath.Join(dir, "refused.log"), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := c.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	if c.ProcessState.ExitCode() != 0 || strings.Contains(string(out), "devopsy: error: ") || !strings.Contains(string(out), "devopsy: warning: ") {
		t.Errorf("a pull past its failures (exit %d) says:\n%s", c.ProcessState.ExitCode(), out)
	}
}

// What the door keeps for its user (its logins, its record of claimed images) is written only
// while the user has a key, under the record's lock: --project-users remove empties its keys
// and then takes that lock to remove both, so a request past its sudo before the removal
// brings neither back.
func TestDoorKeepsNothingOfAUserWithNoKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, keysFile: keys}
	config := filepath.Join(dir, "none.json")
	for name, body := range map[string]string{"no keys file": "", "keys emptied": "# none\n"} {
		if body != "" {
			write(t, keys, body, 0o600)
		}
		if err := side.record("sha256:" + strings.Repeat("ab", 32)); err == nil || !strings.Contains(err.Error(), "no key left") {
			t.Errorf("%s: the record written (%v)", name, err)
		}
		if _, err := (&doorSide{policy: side.policy, keysFile: keys}).writeLogins(config); err == nil {
			t.Errorf("%s: the logins written", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "space", "logins", "shop", "config.json")); err == nil {
			t.Errorf("%s: a logins' config is there", name)
		}
		if got := readString(side.policy.ImagesFile()); got != "" {
			t.Errorf("%s: a record is there: %q", name, got)
		}
	}
	// The control: with a key, the keys file as the request found it, both are written.
	write(t, keys, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g op@laptop\n", 0o600)
	at, err := os.Stat(keys)
	if err != nil {
		t.Fatal(err)
	}
	side.keysAt = at
	if err := side.record("sha256:" + strings.Repeat("ab", 32)); err != nil {
		t.Errorf("the control: the record (%v)", err)
	}
	if _, err := (&doorSide{policy: side.policy, keysFile: keys, keysAt: at}).writeLogins(config); err != nil {
		t.Errorf("the control: the logins (%v)", err)
	}
}

// A claim hands buildx only a name of Docker's (Claimable, which claim runs for every
// caller): an `oci-layout://<path>:<tag>` name, which imagetools would read as a local layout
// as the deploy user, and make when it is not there, never reaches it.
func TestDoorClaimReadsNoLocalLayout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: fake, imagesRoot: dir}
	layout := "oci-layout://" + filepath.Join(dir, "space", "work", "blog-123", "layout") + ":claim"
	_, _, _ = side.claim(layout, filepath.Join(dir, "none.json"))
	if c := readString(filepath.Join(dir, "calls")); strings.Contains(c, "imagetools inspect --raw oci-layout://") {
		t.Errorf("the deploy user's buildx was asked to read a local layout:\n%s", c)
	}
}

// A start held on its registry's copy (a registry that serves the manifest and stalls a
// blob) acts no further once a key's removal ended the user meanwhile (stillAsked): the end
// renews the keys file and reaches none of the door's processes, the deploy user's, so the
// request, which found the old file, records and pins nothing and execs no compose.
func TestDoorClaimStopsAtTheUsersEnd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	image := "registry.example.com/shop/web"
	fakeManifest(t, dir, plainManifest("web"), image+":1")
	// Its registry holds the copy until the test lets it go.
	write(t, filepath.Join(dir, "slowcopy"), "", 0o644)
	keys := filepath.Join(dir, "keys")
	leaked := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g ci\n"
	kept := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPkVz0SeYpQpDNG2uM1BtNSy2LkZ2w3gYBGsgWAIEtxd laptop\n"
	write(t, keys, leaked+kept, 0o600)
	at, err := os.Stat(keys)
	if err != nil {
		t.Fatal(err)
	}
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: fake, imagesRoot: dir,
		env: []string{"PATH=" + os.Getenv("PATH")}, keysFile: keys, keysAt: at}
	file := map[string]any{"name": "shop-prod", "services": map[string]any{"web": map[string]any{"image": image + ":1"}}}
	// What compose's up runs before the door execs compose (doorSide.compose).
	started := make(chan error, 1)
	go func() { started <- side.checkImageLabels(file, filepath.Join(dir, "none.json")) }()
	for deadline := time.Now().Add(startWait); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "copying")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the copy never started:\n%s", readString(filepath.Join(dir, "calls")))
		}
	}
	// The leaked key's removal, as KeysRemoveScript and endProjectUser leave the keys: the kept
	// key alone, under a new inode. The end's kill reaches none of the door's processes.
	write(t, keys, kept, 0o600)
	if err := renewKeys(keys); err != nil {
		t.Fatal(err)
	}
	if now, err := os.Stat(keys); err != nil || os.SameFile(now, at) {
		t.Fatalf("the control: the keys file is the one the request found (%v)", err)
	}
	// The registry lets the copy go.
	pid, _ := strconv.Atoi(strings.TrimSpace(readString(filepath.Join(dir, "child"))))
	if pid <= 0 || syscall.Kill(pid, syscall.SIGKILL) != nil {
		t.Fatalf("the control: no copy to let go (%d)", pid)
	}
	select {
	case err := <-started:
		if err == nil {
			svc := file["services"].(map[string]any)["web"].(map[string]any)
			t.Errorf("a start parked on its registry went on after a key's removal ended the user: compose would run %v, the image recorded as the project's (%q)",
				svc["image"], strings.TrimSpace(readString(side.policy.ImagesFile())))
		}
	case <-time.After(startWait):
		t.Fatal("the start never ended")
	}
}

// A signal that came as a claim ended, still unread by its watch, ends the door as it would a
// moment before (claimCut.end): never lost, so a release cancelled at that moment goes on to
// no compose. In a helper process (TestMain), since the door then exits.
func TestDoorClaimEndKeepsALateSignal(t *testing.T) {
	t.Parallel()
	space := t.TempDir()
	c := exec.Command(os.Args[0], "-test.run", "^$")
	c.Env = append(os.Environ(), "DEVOPSY_TEST_CLAIM_END="+space)
	err := c.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 128+int(syscall.SIGTERM) {
		t.Errorf("a claim ended with a signal waiting went on (%v)", err)
	}
	if work, _ := os.ReadDir(filepath.Join(space, "work")); len(work) > 0 {
		t.Errorf("it left work: %v", work)
	}
}

// A claim holds the image it got with a hold of the project's (images.ClaimHold), which every
// removal counts, and gets it under the server's image lock (the deploy user's, in its release
// root) shared, from the load to the hold: a removal of the deploy user's, which takes the
// lock exclusively, never runs between the image landing and its hold, so it never deletes an
// image the project claimed for a release and has not bookmarked yet. The claim waits for a
// removal holding the lock, saying so, and names itself in the lock's holders while it holds
// it. A skip holds the image too, whose bookmark may be gone; a claim
// whose record fails after its hold removes the hold.
func TestDoorClaimHoldsItsImage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	private := "registry.example.com/blog/private"
	d := fakeManifest(t, dir, plainManifest("private"), private+":1")
	hold := private + ":devopsy-shop-claim." + strings.TrimPrefix(d, "sha256:")[:12]
	// The holders as the claim's hold finds them.
	wrap := filepath.Join(dir, "wrap")
	write(t, wrap, "#!/bin/sh\n[ \"$1\" = tag ] && cat "+filepath.Join(dir, remote.ImagesHolders)+"/* >>"+filepath.Join(dir, "holders")+" 2>/dev/null\nexec "+fake+" \"$@\"\n", 0o755)
	said, err := os.Create(filepath.Join(dir, "said"))
	if err != nil {
		t.Fatal(err)
	}
	defer said.Close()
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: wrap, imagesRoot: dir, log: report.New(said, false, 0)}
	calls := filepath.Join(dir, "calls")
	// A removal holds the server's images.
	lock, err := os.OpenFile(filepath.Join(dir, remote.ImagesLock), os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := side.claim(private+":1", filepath.Join(dir, "none.json"))
		done <- err
	}()
	for start := time.Now(); !strings.Contains(readString(said.Name()), "waiting for the server's images, held by a removal"); time.Sleep(50 * time.Millisecond) {
		if time.Since(start) > startWait {
			t.Fatalf("the claim never waited for the removal:\n%s\n%s", readString(said.Name()), readString(calls))
		}
	}
	if c := readString(calls); strings.Contains(c, "load ") || strings.Contains(c, "\ntag ") {
		t.Errorf("the claim loaded or held while a removal held the server's images:\n%s", c)
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the claim, once the removal let go: %v", err)
		}
	case <-time.After(startWait):
		t.Fatal("the claim never went on once the removal let go")
	}
	c := readString(calls)
	if load, held := strings.Index(c, "load "), strings.Index(c, "\ntag "+private+"@"+d+" "+hold+"\n"); load < 0 || held < load {
		t.Errorf("the claim does not hold what it loaded as %s:\n%s", hold, c)
	}
	if h := readString(filepath.Join(dir, "holders")); !strings.Contains(h, " shop claiming\n") {
		t.Errorf("the claim's holder, as its hold found the holders: %q", h)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, remote.ImagesHolders)); len(left) > 0 {
		t.Errorf("the claim's holder outlived it: %v", left)
	}
	// The skip: the image recorded, a claim holds it again.
	side.policy.Images = []string{d}
	write(t, filepath.Join(dir, "store"), private+"@"+d+"|"+d+"|"+private+"@"+d+"||{}\n", 0o644)
	os.Remove(calls)
	if _, _, err := side.claim(private+":1", filepath.Join(dir, "none.json")); err != nil || strings.Contains(readString(calls), "load ") || !strings.Contains(readString(calls), "tag "+private+"@"+d+" "+hold+"\n") {
		t.Errorf("a skip (%v) does not hold the image:\n%s", err, readString(calls))
	}
	// A record that fails after the hold: nothing claimed, the hold goes.
	side.policy.Images = nil
	os.Remove(filepath.Join(dir, "store"))
	os.RemoveAll(filepath.Join(dir, "space", "images"))
	write(t, filepath.Join(dir, "space", "images"), "not a directory", 0o644)
	os.Remove(calls)
	if _, _, err := side.claim(private+":1", filepath.Join(dir, "none.json")); err == nil || !strings.Contains(readString(calls), "image rm "+hold+"\n") {
		t.Errorf("a claim whose record failed (%v) keeps its hold:\n%s", err, readString(calls))
	}
}

// A hold of the project's on an image it claimed (a release's bookmark) replaces the claim's
// hold, which only bridges the claim to that bookmark, and another project's claim hold
// stays; the project's pruning takes its claim holds left (a cut claim's, an inspect's), never
// another's or a bookmark. In a helper process (doorComposeHelper), since the door execs
// Docker, here the fake, which logs its calls.
func TestDoorClaimHoldGoesWithTheBookmark(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	doorImageFake(t, dir)
	private := "registry.example.com/blog/private"
	p1 := fakeManifest(t, dir, plainManifest("private"), private+":1")
	mine, others := images.ClaimHold(private, "shop", p1), images.ClaimHold(private, "blog", p1)
	bookmark := "busybox:devopsy-shop-prod-20260101000000"
	write(t, filepath.Join(dir, "store"), private+"@"+p1+"|"+p1+"|"+private+"@"+p1+"||{}\n"+bookmark+"|"+p1+"|"+private+"@"+p1+"|"+bookmark+" "+mine+" "+others+"|{}\n", 0o644)
	request := func(req string) string {
		t.Helper()
		os.Remove(filepath.Join(dir, "calls"))
		c := exec.Command(os.Args[0], "-test.run", "^$")
		c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_REQUEST="+req)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s (%v):\n%s", req, err, out)
		}
		return readString(filepath.Join(dir, "calls"))
	}
	calls := request("tag " + private + ":1 " + bookmark)
	if tag, rm := strings.Index(calls, "\ntag "+private+"@"+p1+" "+bookmark+"\n"), strings.Index(calls, "image rm "+mine+"\n"); tag < 0 || rm < tag {
		t.Errorf("the bookmark does not replace the claim's hold:\n%s", calls)
	}
	if strings.Contains(calls, "image rm "+others) || strings.Contains(calls, "image rm "+bookmark) {
		t.Errorf("the bookmark took another's hold:\n%s", calls)
	}
	write(t, filepath.Join(dir, "names"), strings.Join([]string{mine, others, bookmark, private + ":1", "busybox:devopsy-shop-claim-20260101000000"}, "\n")+"\n", 0o644)
	calls = request("image prune -f")
	if strings.Count(calls, "image rm ") != 1 || !strings.Contains(calls, "image rm "+mine+"\n") {
		t.Errorf("the pruning takes other than its own claim's hold:\n%s", calls)
	}
}
