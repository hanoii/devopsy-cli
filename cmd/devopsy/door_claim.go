package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/klauspost/compress/zstd"
)

// claim makes ref this project's, with its registry logins (config, the caller's docker
// config), its record of claimed images (door.Policy.Images, ImagesFile) saying so after: an
// image another project pulled with its own logins is this one's only once its own claim of
// it goes through, a public one at the cost of one fetch. Docker's daemon contacts no
// registry for it, which a registry the project names could send anywhere, another project's
// containers included: the door's client reads the manifest for the daemon's platform
// (platformManifest), the door's client copies the registry's own bytes (copyImage), the door
// checks every blob and each layer's content against the config (checkLayout), and loads them
// (loadImage), which
// leaves Docker's containerd store as a pull by digest does. An image of that digest already
// the project's, in the repository asked, is taken as it is (the skip). It gives the digest
// and that image, read by that digest: the name is anyone's on the server to move right after,
// so nothing reads it again.
func (s *doorSide) claim(ref, config string) (string, images.Image, error) {
	// Every claim's name is one the door may claim, whoever asked: a request and the pull verb
	// refuse before anything runs, and this holds for every caller.
	if err := s.policy.Claimable(ref); err != nil {
		return "", images.Image{}, err
	}
	d, err := s.logins(config)
	if err != nil {
		return "", images.Image{}, err
	}
	refused := fmt.Errorf("refused: %s is not this project's, and its registry logins do not pull it", ref)
	g := s.claimCut()
	defer g.end()
	m, err := s.platformManifest(g, ref, d)
	if err != nil {
		if err == errNoRead {
			return "", images.Image{}, registryFailure{refused}
		}
		return "", images.Image{}, fmt.Errorf("refused: %s: %w", ref, err)
	}
	pinned := images.ByDigest(ref, m)
	// The skip: that digest in the repository asked, an image the project claimed. A bare id
	// is no key: a name by digest in another repository is no tag's to make.
	if img, err := s.inspectImage(pinned); err == nil && slices.Contains(s.policy.Images, img.ID) {
		s.nameAsAsked(ref, pinned)
		return m, img, nil
	}
	if err := s.hostReady(d); err != nil {
		return "", images.Image{}, fmt.Errorf("refused: %s: %w", ref, err)
	}
	if s.onFetch != nil {
		s.onFetch()
	}
	if loaded, err := s.fetch(g, pinned, m, d); err != nil {
		// A load that failed may have left the name: no later claim skips onto it. Nothing
		// before the load touched the store, so a failed copy leaves the record as it was.
		if loaded {
			_ = s.unrecord(m)
		}
		return "", images.Image{}, fmt.Errorf("refused: %s: %w", ref, err)
	}
	img, err := s.inspectImage(pinned)
	if err != nil {
		return "", images.Image{}, fmt.Errorf("refused: %s: %v", ref, err)
	}
	// On the containerd store an image's id is its manifest's digest: any other is not what
	// the door loaded.
	if img.ID != m {
		return "", images.Image{}, fmt.Errorf("refused: %s: %s goes by %s, not the %s loaded", ref, pinned, img.ID, m)
	}
	if err := s.record(img.ID); err != nil {
		return "", images.Image{}, err
	}
	s.nameAsAsked(ref, pinned)
	return m, img, nil
}

// registryFailure is a claim's failure that is its registry's (a tag or a platform it does not
// have, logins it refuses, an image it does not serve to the copy), never the door's refusal of
// what it served: a release's event, which the pull verb says and does not log (doorSide.pull).
type registryFailure struct{ error }

func (r registryFailure) Unwrap() error { return r.error }

// nameAsAsked tags the name asked onto the image claimed, the registry's content checked under
// it, so a start finds it here and asks the registry nothing.
func (s *doorSide) nameAsAsked(ref, pinned string) {
	if !images.Pinned(ref) {
		_ = s.docker("tag", pinned, ref).Run()
	}
}

// hostReady is doorHostReady for this door process, read once, its buildx the one that copies,
// under the logins' directory.
func (s *doorSide) hostReady(logins string) error {
	_, err := s.hostChecked.get(func() (struct{}, error) { return struct{}{}, doorHostReady(s.real, s.env, logins) })
	return err
}

// fetch copies the image pinned names (<repo>@m) from its registry as it is, with the logins
// in the DOCKER_CONFIG dir logins, and loads it, in a work directory of the claim's (g). It says
// whether the load ran.
func (s *doorSide) fetch(g *claimCut, pinned, m, logins string) (bool, error) {
	work, err := g.workDir()
	if err != nil {
		return false, err
	}
	layout := filepath.Join(work, "layout")
	if err := s.copyImage(g.run, pinned, layout, logins); err != nil {
		return false, err
	}
	blobs, manifest, err := checkLayout(layout, m)
	if err != nil {
		return false, err
	}
	return true, s.loadImage(g.run, pinned, m, manifest, layout, blobs)
}

// claimCut is what a claim has running and on disk while it reads, copies and loads: its work
// directories, which go when it ends (claimCut.end), on a failure too, and the one child it
// runs at a time. A SIGINT, SIGTERM or SIGHUP meanwhile stops that child's process group (the
// docker CLI and the plugin it runs, buildx, which a signal to the door alone never reaches),
// starts no other, removes the request's work directories, the claim's among them, and ends
// the door with the signal's exit: a
// release cancelled mid-fetch would otherwise leave the whole image in <space>/work for a day.
type claimCut struct {
	s     *doorSide
	mu    sync.Mutex
	child *exec.Cmd
	work  []string
	cut   bool
	ch    chan os.Signal
	done  chan struct{}
}

// claimCut starts a claim's watch for the signals.
func (s *doorSide) claimCut() *claimCut {
	g := &claimCut{s: s, ch: make(chan os.Signal, 1), done: make(chan struct{})}
	signal.Notify(g.ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		select {
		case sig := <-g.ch:
			g.cutBy(sig)
		case <-g.done:
		}
	}()
	return g
}

// cutBy ends the door on sig: the child's group stopped, the request's work directories, the
// claim's among them, removed, and the signal's own exit.
func (g *claimCut) cutBy(sig os.Signal) {
	g.mu.Lock()
	g.cut = true
	if g.child != nil && g.child.Process != nil {
		_ = syscall.Kill(-g.child.Process.Pid, syscall.SIGKILL)
	}
	g.s.madeMu.Lock()
	for _, d := range g.s.made {
		g.s.removeWork(d)
	}
	os.Exit(128 + int(sig.(syscall.Signal)))
}

// workDir is a work directory of the door's (workDir) that goes with the claim, made under the
// claim's lock, so a signal finds it recorded once it is there.
func (g *claimCut) workDir() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d, err := g.s.workDir("")
	if err == nil {
		g.work = append(g.work, d)
	}
	return d, err
}

// run runs c in a process group of its own, which a signal stops whole, or not at all once a
// signal came.
func (g *claimCut) run(c *exec.Cmd) error { return g.runWithin(c, 0) }

// runWithin is run, its process group killed once within passed (none when 0).
func (g *claimCut) runWithin(c *exec.Cmd, within time.Duration) error {
	g.mu.Lock()
	if g.cut {
		g.mu.Unlock()
		return errors.New("cut by a signal")
	}
	ownGroup(c)
	if err := c.Start(); err != nil {
		g.mu.Unlock()
		return err
	}
	g.child = c
	g.mu.Unlock()
	defer killGroupAfter(c, within)()
	err := c.Wait()
	g.mu.Lock()
	g.child = nil
	g.mu.Unlock()
	return err
}

// end stops the watch and removes the claim's work directories.
func (g *claimCut) end() {
	signal.Stop(g.ch)
	// A signal that came as the claim ended, still unread (the watch might take done first):
	// it ends the door as it would have a moment before, never lost.
	select {
	case sig := <-g.ch:
		g.cutBy(sig)
	default:
	}
	close(g.done)
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, d := range g.work {
		g.s.removeWork(d)
	}
	g.work = nil
}

// copyImage writes the registry's own bytes of pinned into an OCI layout at dir, with the door's
// client, as the manifest's read: buildx imagetools, with its builder's settings (a mirror, a
// CA) and the project's logins, under the door's group, which the firewall holds.
// --prefer-index=false keeps pinned's manifest as it is, never wrapped in an index of another
// digest. What it says goes to stderr on a failure.
func (s *doorSide) copyImage(run func(*exec.Cmd) error, pinned, dir, logins string) error {
	b, err := s.builder()
	if err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), "refused: "))
	}
	var said bytes.Buffer
	c := s.docker("buildx", "imagetools", "create", "--prefer-index=false", "--tag", "oci-layout://"+dir+":claim", pinned)
	c.Env = append(append(slices.Clone(s.env), b...), "DOCKER_CONFIG="+logins)
	c.Stdout, c.Stderr = &said, &said
	if run(c) != nil {
		os.Stderr.Write(said.Bytes())
		return registryFailure{errors.New("its registry did not serve the image to this project's logins")}
	}
	return nil
}

// ociDigest is a blob's name in a layout, a sha256.
var ociDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// layoutManifest is the part of an image manifest a load reads: its own media type, its config
// and its layers, each a blob by digest and size, a layer's of its kind. An index has no config.
type layoutManifest struct {
	MediaType string `json:"mediaType"`
	Config    *struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	} `json:"config"`
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"layers"`
	Manifests []json.RawMessage `json:"manifests"`
}

// layerKinds are the layers a claim takes, by media type, and whether each is compressed: any
// other (a foreign or nondistributable layer, an encrypted one, one containerd does not take
// for a layer and would leave out of its pairing with the config's diff_ids) refuses it.
var layerKinds = map[string]bool{
	"application/vnd.oci.image.layer.v1.tar":            false,
	"application/vnd.oci.image.layer.v1.tar+gzip":       true,
	"application/vnd.oci.image.layer.v1.tar+zstd":       true,
	"application/vnd.docker.image.rootfs.diff.tar.gzip": true,
}

// layerRead is how checkLayout reads a blob: no layer, a layer as it is, or a compressed one.
type layerRead int

const (
	notALayer layerRead = iota
	plainLayer
	compressedLayer
)

// layerContent is the sha256 of a layer's content, read from r as containerd unpacks it: a
// layer of a compressed kind decompressed by what its first bytes say (gzip, zstd, a zstd
// skippable frame, or none), one of a plain kind as it is. Read otherwise than containerd
// does, a layer would at worst be refused, never another's: its content is a function of the
// bytes its registry served.
func layerContent(r io.Reader, compressed bool) (string, error) {
	content := r
	if compressed {
		b := bufio.NewReader(r)
		head, _ := b.Peek(4)
		content = b
		switch {
		case len(head) >= 3 && head[0] == 0x1f && head[1] == 0x8b && head[2] == 0x08:
			z, err := gzip.NewReader(b)
			if err != nil {
				return "", err
			}
			defer z.Close()
			content = z
		case len(head) == 4 && (bytes.Equal(head, []byte{0x28, 0xb5, 0x2f, 0xfd}) || head[0]&0xf0 == 0x50 && bytes.Equal(head[1:], []byte{0x2a, 0x4d, 0x18})):
			z, err := zstd.NewReader(b, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(zstdWindow))
			if err != nil {
				return "", err
			}
			defer z.Close()
			content = z
		}
	}
	h := sha256.New()
	if _, err := io.Copy(h, content); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// anySize is the size checkLayout takes for a blob whose size no descriptor of the layout says.
const anySize = -1 << 62

// maxConfig is what the door reads at most of an image's config, which it holds whole.
const maxConfig = 16 << 20

// zstdWindow bounds the memory a zstd layer's decoder takes: zstd's highest levels, --ultra and
// --long, use windows up to 128 MiB.
const zstdWindow = 128 << 20

// checkLayout checks the layout imagetools wrote at dir for the manifest m, and gives the blobs
// a load takes (m's own, its config's, each layer's once) and m's media type: imagetools'
// index names m, m's bytes are m, it is an image manifest, each blob it names is a regular
// file whose sha256 is its name and whose size is the manifest's, each layer of a kind the
// door takes (layerKinds), and each layer's content, decompressed, the one its config names
// (rootfs.diff_ids, by position). Docker skips a blob its store holds without a word, so a
// layout missing one would load an image of another's blobs; and a load lands on the layers
// the server holds for the config's diff_ids without unpacking them, so a config naming
// another's over layers of junk would run another's files. With every layer's content its
// diff_id, an image lands only on content its registry served whole.
func checkLayout(dir, m string) ([]string, string, error) {
	var index struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil || json.Unmarshal(data, &index) != nil || !slices.ContainsFunc(index.Manifests, func(d struct {
		Digest string `json:"digest"`
	}) bool {
		return d.Digest == m
	}) {
		return nil, "", fmt.Errorf("the copy of its registry's image names no manifest %s", m)
	}
	// blob checks one blob, hashed as it streams (a layer may be larger than the door's
	// memory): its size the manifest's (any up to max for the manifest's own, whose size is
	// the index's to say, anySize), its bytes given when max is above 0 (the manifest's, the
	// config's), and for a layer (unpack) the sha256 of its content as containerd unpacks it
	// (layerContent), in the same read. A size below 0 is no descriptor's.
	blob := func(digest string, size, max int64, unpack layerRead) ([]byte, string, error) {
		if !ociDigest.MatchString(digest) {
			return nil, "", fmt.Errorf("a blob of no sha256: %q", digest)
		}
		if size < 0 && size != anySize {
			return nil, "", fmt.Errorf("the blob %s has a size below 0", digest)
		}
		path := filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() || size != anySize && fi.Size() != size || max > 0 && fi.Size() > max {
			return nil, "", fmt.Errorf("the copy of its registry's image lacks the blob %s", digest)
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, "", err
		}
		defer f.Close()
		h := sha256.New()
		var kept bytes.Buffer
		w := io.Writer(h)
		if max > 0 {
			w = io.MultiWriter(h, &kept)
		}
		r := io.TeeReader(f, w)
		content := ""
		if unpack != notALayer {
			if content, err = layerContent(r, unpack == compressedLayer); err != nil {
				return nil, "", fmt.Errorf("the layer %s of the copy does not unpack: %v", digest, err)
			}
		}
		// What the decompressor left unread is the blob's too.
		if _, err := io.Copy(io.Discard, r); err != nil {
			return nil, "", err
		}
		if "sha256:"+hex.EncodeToString(h.Sum(nil)) != digest {
			return nil, "", fmt.Errorf("the blob %s of the copy is not its hash", digest)
		}
		return kept.Bytes(), content, nil
	}
	data, _, err = blob(m, anySize, maxManifest, notALayer)
	if err != nil {
		return nil, "", err
	}
	var manifest layoutManifest
	if json.Unmarshal(data, &manifest) != nil || len(manifest.Manifests) > 0 || manifest.Config == nil {
		return nil, "", fmt.Errorf("%s is no image manifest", m)
	}
	config, _, err := blob(manifest.Config.Digest, manifest.Config.Size, maxConfig, notALayer)
	if err != nil {
		return nil, "", err
	}
	// As containerd reads it (encoding/json, its own image config's shape).
	var image struct {
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(config, &image); err != nil {
		return nil, "", fmt.Errorf("%s's config is no image config", m)
	}
	if len(image.RootFS.DiffIDs) != len(manifest.Layers) {
		return nil, "", fmt.Errorf("%s names %d layers and its config %d", m, len(manifest.Layers), len(image.RootFS.DiffIDs))
	}
	blobs := []string{m, manifest.Config.Digest}
	// A blob named twice is read as each kind it is named (containerd reads each position by
	// its own media type), once each.
	type read struct {
		digest string
		as     layerRead
	}
	contents := map[read]string{}
	for i, l := range manifest.Layers {
		compressed, ok := layerKinds[l.MediaType]
		if !ok {
			return nil, "", fmt.Errorf("the layer %s is of a kind the door does not take: %q", l.Digest, l.MediaType)
		}
		key := read{l.Digest, plainLayer}
		if compressed {
			key.as = compressedLayer
		}
		content, seen := contents[key]
		if !seen {
			var err error
			if _, content, err = blob(l.Digest, l.Size, 0, key.as); err != nil {
				return nil, "", err
			}
			contents[key] = content
			if !slices.Contains(blobs, l.Digest) {
				blobs = append(blobs, l.Digest)
			}
		}
		if content != image.RootFS.DiffIDs[i] {
			return nil, "", fmt.Errorf("the layer %s is not the content its config names (%s): it unpacks to %s", l.Digest, image.RootFS.DiffIDs[i], content)
		}
	}
	mediaType := manifest.MediaType
	if mediaType == "" {
		mediaType = "application/vnd.oci.image.manifest.v1+json"
	}
	return blobs, mediaType, nil
}

// loadImage loads the image m of pinned from the layout at dir into Docker's store: a tar the
// door writes itself, its own oci-layout and index.json, naming m alone by its canonical name
// (images.Canonical: a familiar one loads with no registry digest), and the blobs checkLayout
// hashed, nothing else of the directory. A load succeeded only when it exits 0 and says
// exactly one line, `Loaded image: ` and the familiar name: docker load exits 0 when a blob is
// missing, its unpacking failed said on stdout.
func (s *doorSide) loadImage(run func(*exec.Cmd) error, pinned, m, mediaType, dir string, blobs []string) error {
	size := func(digest string) int64 {
		fi, err := os.Stat(filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:")))
		if err != nil {
			return 0
		}
		return fi.Size()
	}
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"manifests": []map[string]any{{
			"mediaType":   mediaType,
			"digest":      m,
			"size":        size(m),
			"annotations": map[string]string{"io.containerd.image.name": images.Canonical(pinned)},
		}},
	})
	if err != nil {
		return err
	}
	r, w := io.Pipe()
	go func() { w.CloseWithError(writeLayout(w, dir, index, blobs)) }()
	var out, said bytes.Buffer
	c := s.docker("load")
	c.Stdin, c.Stdout, c.Stderr = r, &out, &said
	err = run(c)
	r.Close()
	want := "Loaded image: " + images.Familiar(pinned)
	if err != nil || strings.TrimSpace(out.String()) != want {
		os.Stderr.Write(said.Bytes())
		os.Stderr.Write(out.Bytes())
		return errors.New("its image did not load")
	}
	return nil
}

// writeLayout writes a tar of an OCI layout to w: oci-layout, index and the blobs, streamed
// from dir, each once.
func writeLayout(w io.Writer, dir string, index []byte, blobs []string) error {
	t := tar.NewWriter(w)
	file := func(name string, size int64, r io.Reader) error {
		if err := t.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: size, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := io.Copy(t, r)
		return err
	}
	layout := []byte(`{"imageLayoutVersion":"1.0.0"}`)
	if err := file("oci-layout", int64(len(layout)), bytes.NewReader(layout)); err != nil {
		return err
	}
	if err := file("index.json", int64(len(index)), bytes.NewReader(index)); err != nil {
		return err
	}
	for _, d := range blobs {
		name := "blobs/sha256/" + strings.TrimPrefix(d, "sha256:")
		if err := func() error {
			f, err := os.Open(filepath.Join(dir, name))
			if err != nil {
				return err
			}
			defer f.Close()
			fi, err := f.Stat()
			if err != nil {
				return err
			}
			return file(name, fi.Size(), f)
		}(); err != nil {
			return err
		}
	}
	return t.Close()
}

// record adds id to this project's claimed images (Policy.ImagesFile), under the record's lock.
func (s *doorSide) record(id string) error {
	err := s.lockedRecord(func(ids []string) []string {
		if slices.Contains(ids, id) {
			return ids
		}
		return append(ids, id)
	})
	if err == nil && !slices.Contains(s.policy.Images, id) {
		s.policy.Images = append(s.policy.Images, id)
	}
	return err
}

// unrecord drops id from this project's claimed images, under the record's lock: a failed load
// leaves the name it loaded, which a skip would otherwise take for the image claimed.
func (s *doorSide) unrecord(id string) error {
	s.policy.Images = slices.DeleteFunc(s.policy.Images, func(i string) bool { return i == id })
	return s.lockedRecord(func(ids []string) []string {
		return slices.DeleteFunc(ids, func(i string) bool { return i == id })
	})
}

// lockedRecord edits the project's record of claimed images under a flock beside it, written to
// a file beside it and renamed over it, so a concurrent claim's id is never lost.
func (s *doorSide) lockedRecord(edit func([]string) []string) error {
	file := s.policy.ImagesFile()
	return s.whileAUser(func() error { return writeRecord(file, edit) })
}

// whileAUser runs write, what the door keeps for its user (its logins, its record), under the
// record's lock and only while the user is as the request found it (stillAsked): --project-users remove empties
// its keys, then takes that lock to remove what was kept, so a request past its sudo before
// the removal writes none of it back after.
func (s *doorSide) whileAUser(write func() error) error {
	file := s.policy.ImagesFile()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(file+".lock", os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	if err := s.stillAsked(); err != nil {
		return fmt.Errorf("refused: %w", err)
	}
	return write()
}

// writeRecord writes file, a record of claimed images, as edit makes it, by rename.
func writeRecord(file string, edit func([]string) []string) error {
	ids := edit(claimedImages(file))
	tmp := file + ".tmp"
	data := ""
	if len(ids) > 0 {
		data = strings.Join(ids, "\n") + "\n"
	}
	if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}
