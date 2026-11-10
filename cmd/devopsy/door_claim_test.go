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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/images"
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

// A door cut by a signal while it proves or copies an image (a release cancelled mid-fetch)
// stops what it runs and removes its work directories, which would otherwise hold the image
// for a day, and ends with the signal's exit. In a helper process (doorComposeHelper), since
// the door then exits.
func TestDoorClaimCutBySignalLeavesNoWork(t *testing.T) {
	t.Parallel()
	for slow, mark := range map[string]string{"slowproof": "proving", "slowcopy": "copying"} {
		dir := t.TempDir()
		doorImageFake(t, dir)
		private := "registry.example.com/blog/private"
		fakeManifest(t, dir, plainManifest("private"), private+":1")
		write(t, filepath.Join(dir, slow), "", 0o644)
		c := exec.Command(os.Args[0], "-test.run", "^$")
		c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_REQUEST=tag "+private+":1 busybox:devopsy-shop-prod-20260101000000")
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
		if work, _ := os.ReadDir(filepath.Join(dir, "space", "work")); len(work) == 0 {
			t.Fatalf("%s: no work directory to remove", slow)
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
	// The built service's tag is not in its registry: first by name, it ends the pull.
	out, code = pull(file, "debug", "pull")
	if code != 1 || copies() != 0 || !strings.Contains(out, "devopsy: error: ") || readString(log) != "" {
		t.Errorf("a pull whose first claim fails (exit %d, %d copies, logged %q):\n%s", code, copies(), readString(log), out)
	}
	if out, code := pull(file, "debug", "pull --ignore-pull-failures"); code != 0 || !strings.Contains(out, built) || !strings.Contains(readString(calls), "imagetools inspect --raw "+private) {
		t.Errorf("a pull past its failures (exit %d):\n%s\n%s", code, out, readString(calls))
	}
	// A registry that does not serve what it names, the proof failing: the door's refusal,
	// logged, past --ignore-pull-failures too.
	write(t, filepath.Join(dir, "unserved"), images.RepoOf(other)+"@"+digestOf([]byte(plainManifest("other")))+"\n", 0o644)
	os.Remove(filepath.Join(dir, "space", "images", "devopsy-shop"))
	if out, code := pull(file, "debug", "pull --ignore-buildable --ignore-pull-failures"); code != 2 || readString(log) == "" {
		t.Errorf("a proof that fails under the pull (exit %d, logged %q):\n%s", code, readString(log), out)
	}
	os.Remove(filepath.Join(dir, "unserved"))
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
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: fake}
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
