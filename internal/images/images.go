// Package images gives each release on a server its own images. Release
// scripts run it on the server (devopsy --images, under the server's
// image lock, which the scripts take), so one devopsy, the server's,
// applies the rules to images every project there shares.
//
// A release bookmarks every image it runs with a second tag of its own,
// <repository>:devopsy-<compose name>-<release id> (Bookmark). The
// bookmarks are the reference count: a release going (Remove) untags
// its bookmarks, and an image no other bookmark holds loses the names
// the release ran, so Docker deletes it. A rollback or a restart puts
// what a release built back from its bookmarks and holds what must not
// move by digest in devopsy's own compose file (Restore): a pulled data
// service only ever goes forward (HistoryFile).
package images

import (
	"bufio"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// Files in a release's directory, beside its code, and the project's
// record.
const (
	// ListFile lists the images a release runs, one per service, tab
	// separated: the name (compose's image, as it resolved at the
	// activation), the image it had when bookmarked ("" when Docker had
	// none), the service, its kind (KindOwn, KindPulled, KindData), the
	// registry's digest of a pulled image (<repository>@sha256:...) and
	// its version label (org.opencontainers.image.version), for display.
	ListFile = "devopsy-images"
	// FetchingFile marks a new release while it fetches its images: one
	// left behind was cut off without a signal (a CI session that
	// dropped), and Recover finishes what the release script would have.
	FetchingFile = "devopsy-fetching"
	// PendingFile marks a release from its upload until its switch: one
	// left was never made current (still under way, or cut off before).
	PendingFile = "devopsy-pending"
	// LeftFile, in a project's directory, records the names a removal
	// could not remove (Docker refused: a container, even a stopped one,
	// uses the image), one tab-separated line each: time, compose name,
	// release id, name, image id, reason. The project's later removals
	// retry them (Prune).
	LeftFile = ".devopsy-images-left"

	failedFile = "devopsy-failed"
	codeDir    = "code"
)

// ErrNoImage is Docker saying an image or a name does not exist.
var ErrNoImage = errors.New("no such image")

// Docker is what the rules need from the server's Docker. Names come back
// as Docker shows them (nginx, not docker.io/library/nginx): compare
// through familiar.
type Docker interface {
	// ID is the image a name or id points to, or ErrNoImage.
	ID(ref string) (string, error)
	// Tags are the image's names (RepoTags).
	Tags(id string) ([]string, error)
	// Tag points dst at src's image (a name or an id).
	Tag(src, dst string) error
	// Remove removes a name: Docker untags it while the image has other
	// names, and deletes the image with its last one, refusing that while
	// a container uses it. Never forced. The error carries Docker's
	// reason.
	Remove(ref string) error
	// Users are the compose projects with a container, even a stopped
	// one, of the image or of one built on it; "" for a container
	// outside Compose.
	Users(id string) ([]string, error)
	// Prune removes dangling images no container uses, returning the
	// space freed as Docker says it.
	Prune() (string, error)
	// Inspect is what Docker knows of an image, by name, digest or id, or
	// ErrNoImage.
	Inspect(ref string) (Image, error)
	// Pull pulls an image by name or digest.
	Pull(ref string) error
	// Containers are a compose project's containers, stopped ones too.
	Containers(project string) ([]Container, error)
}

// Container is one of an environment's containers, as the rules read it.
type Container struct {
	Name, Service string
	// Image is its image's id, Ref the name it was created with
	// (Config.Image).
	Image, Ref string
	// State is Docker's: created (never started), running, exited...
	State string
	// OneOff: a compose run's.
	OneOff bool
	// Digest is its devopsy.digest label: the image its activation gave
	// it.
	Digest string
}

// DigestLabel is the label devopsy's own compose file gives every
// service it fetched: the image the activation gave it, as its
// registry digest (<repository>@sha256:...) or, for an own image, its id.
const DigestLabel = "devopsy.digest"

// Image is what the rules read of an image.
type Image struct {
	ID string
	// Digests are its <repository>@sha256:... names (RepoDigests). On
	// Docker's containerd store an image built or loaded has them too, so
	// they never tell a pulled image from an own one (Service.own does).
	Digests []string
	Created time.Time
	// Version is its org.opencontainers.image.version label: display
	// only, never compared (images have no common version field).
	Version string
}

// Modes, as a project's config names them: what fetches a release's
// images (remote.ModeBuild, remote.ModeImage).
const (
	ModeBuild = "build"
	ModeImage = "image"
)

// Kinds of a listed image.
const (
	// KindOwn is an image the project builds (build mode), or one compose
	// never pulls (pull_policy: never): a rollback restores it exactly,
	// from its bookmark.
	KindOwn = "own"
	// KindPulled is an image compose pulls from a registry.
	KindPulled = "pulled"
	// KindData is a pulled image of a service with a devopsy.data label
	// (any value, none included) that compose does not build: its data
	// is changed in place by what runs it.
	KindData = "data"
)

// Set is one environment's images on a server.
type Set struct {
	Docker Docker
	// Name is the environment's compose name, which its bookmarks carry.
	Name string
	// Left is its project's LeftFile.
	Left string
	// Out takes what it says (report.Line).
	Out io.Writer
	// List lists the services a release runs (ListCompose).
	List func(rel string) ([]Service, error)
	// Mode is the environment's (ModeBuild when empty).
	Mode string
	// Now is the time, for tests (time.Now when nil).
	Now func() time.Time
	// Accept are the services whose image Record or a sync's check lets
	// through what it would refuse (--accept-image); Address is the
	// target as typed, for the fix lines.
	Accept  map[string]bool
	Address string
	// SyncData is a parent's ParentData, in a sync's activation or
	// holder: Record and SyncCheck check the child against it.
	SyncData string
	// AsBuilt has Restore put own images' bookmarks back even when their
	// build changed since (changed): the restart after a failed run step
	// whose build again failed runs what the release built, never the
	// failed release's image under the name.
	AsBuilt bool
	// Base and Snapshots are the environment's directory and where its
	// snapshots go (SnapshotsEnv, BaseEnv when empty, as every script
	// exports them): a snapshot's directory holds a release's listing
	// and bookmarks as s<id> (idOf), and both kinds are the
	// environment's own (own).
	Base, Snapshots string

	// built are the services with a build: section, data those with a
	// devopsy.data label, from the last List.
	built, data map[string]bool
}

// Service is one of the services a release runs, as compose resolves it
// (compose config, without profiles).
type Service struct {
	Name string
	// Ref is its image: image:, else the name compose gives a build
	// (<compose name>-<service>).
	Ref string
	// Build: it has a build: section. Data: devopsy.data=true, Method
	// how a copy carries it (devopsy.data.copy, volumes by default).
	// NeverPull: pull_policy: never.
	Build, Data, NeverPull bool
	Method                 string
	// BuildSpec is its build: section as compose resolves it, BuildKey
	// what it comes to (BuildKey, from ListCompose, which knows the
	// release's code).
	BuildSpec json.RawMessage
	BuildKey  string
	// Entries are the entries of mnt it bind-mounts, less its excluded
	// ones (. for mnt whole), Volumes its named volumes by key, less its
	// excluded ones; EntriesKnown: read from compose's config.
	Entries      []string
	Volumes      []string
	EntriesKnown bool
}

// own says whether a service's image is the project's own: built (in
// build mode, where devopsy builds what has a build: section) or never
// pulled. Decided by compose and the mode, never by the image: Docker's
// containerd store gives built and loaded images registry digests too.
func (svc Service) own(mode string) bool {
	return svc.NeverPull || (svc.Build && mode != ModeImage)
}

// entry is one image a release runs.
type entry struct {
	Ref string
	// ID is the image Ref had when bookmarked; "" when Docker had none.
	ID                             string
	Service, Kind, Digest, Version string
	// Build is the BuildKey it was built with, for a service the release
	// built: what a reload checks (ReloadCheck); "" for any other.
	Build string
}

func (s *Set) say(lv report.Level, format string, args ...any) {
	// A session gone takes these lines with it: never a reason to stop.
	_, _ = io.WriteString(s.Out, report.Line(lv, fmt.Sprintf(format, args...))+"\n")
}

// bookmarkPrefix is the start of a release's bookmarks, the release id
// following it: devopsy-<compose name>-, then the 14-digit id, then,
// when the release names a repository more than once, - and the ref's
// cksum (up to 10 digits). Docker's tags end at 128 characters, so a
// name over 94 is cut to 85 and followed by the first 8 hex digits of its
// sha256, which keeps names that differ only past the cut apart. A
// snapshot's id is one longer (s<id>), so its cksum is in base 36 (up to
// 7 characters): every environment's bookmarks keep the one prefix.
func bookmarkPrefix(composeName string) string {
	if composeName == "" {
		return "devopsy-"
	}
	if len(composeName) > 94 {
		sum := sha256.Sum256([]byte(composeName))
		composeName = composeName[:85] + "-" + hex.EncodeToString(sum[:4])
	}
	return "devopsy-" + composeName + "-"
}

// repoOf is a ref's repository: no digest, no tag; a registry's port stays.
func repoOf(ref string) string {
	r, _, _ := strings.Cut(ref, "@")
	slash := strings.LastIndex(r, "/")
	if colon := strings.LastIndex(r, ":"); colon > slash {
		r = r[:colon]
	}
	return r
}

// repoKey is the repository as Docker shows it: docker.io/library/nginx
// and library/nginx are nginx, docker.io/bitnami/redis is bitnami/redis.
func repoKey(ref string) string {
	r := repoOf(ref)
	r = strings.TrimPrefix(r, "index.docker.io/")
	r = strings.TrimPrefix(r, "docker.io/")
	return strings.TrimPrefix(r, "library/")
}

// familiar is a name as Docker shows it, so a name devopsy computed and
// one Docker reports compare equal: nginx is nginx:latest, and
// nginx:1.27@sha256:... is nginx@sha256:... (a digest wins over a tag).
func familiar(ref string) string {
	if _, digest, ok := strings.Cut(ref, "@"); ok {
		return repoKey(ref) + "@" + digest
	}
	rest := ref[len(repoOf(ref)):]
	if rest == "" {
		rest = ":latest"
	}
	return repoKey(ref) + rest
}

// digestRef is a ref by digest as Docker takes it: the tag beside the
// digest dropped (nginx@sha256:..., not nginx:1.27@sha256:...).
func digestRef(ref string) string {
	if _, digest, ok := strings.Cut(ref, "@"); ok {
		return repoOf(ref) + "@" + digest
	}
	return ref
}

// isBookmark says whether a tag (repo:tag) is a release's bookmark.
func isBookmark(tag string) bool {
	r := repoOf(tag)
	return len(tag) > len(r) && strings.HasPrefix(tag[len(r)+1:], "devopsy-")
}

// bookmarks are the bookmark names of a release's entries, in order: a
// repository named more than once gets the ref's cksum, so no bookmark
// overwrites another, and a name two services run on different images
// (one held by digest) the cksum of the ref and the image.
func bookmarks(entries []entry, name, id string) []string {
	count := map[string]int{}
	images := map[string]map[string]bool{}
	for _, e := range entries {
		count[repoKey(e.Ref)]++
		k := familiar(e.Ref)
		if images[k] == nil {
			images[k] = map[string]bool{}
		}
		images[k][e.ID] = true
	}
	prefix := bookmarkPrefix(name)
	bs := make([]string, len(entries))
	for i, e := range entries {
		b := repoOf(e.Ref) + ":" + prefix + id
		if count[repoKey(e.Ref)] > 1 {
			sum := e.Ref
			if len(images[familiar(e.Ref)]) > 1 {
				sum += " " + e.ID
			}
			base := 10
			if strings.HasPrefix(id, "s") {
				base = 36
			}
			b += "-" + strconv.FormatUint(uint64(cksum([]byte(sum))), base)
		}
		bs[i] = b
	}
	return bs
}

// readList reads a release's ListFile: nil when it has none. Lines
// without an id are an image Docker did not have, or v0.38's.
func readList(rel string) []entry {
	data, err := os.ReadFile(filepath.Join(rel, ListFile))
	if err != nil {
		return nil
	}
	var es []entry
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		col := func(i int) string {
			if i < len(f) {
				return strings.TrimSpace(f[i])
			}
			return ""
		}
		if col(0) != "" {
			es = append(es, entry{Ref: col(0), ID: col(1), Service: col(2), Kind: col(3), Digest: col(4), Version: col(5), Build: col(6)})
		}
	}
	return es
}

func writeList(rel string, es []entry) error {
	var b strings.Builder
	for _, e := range es {
		cols := []string{e.Ref, e.ID, e.Service, e.Kind, e.Digest, e.Version, e.Build}
		for len(cols) > 1 && cols[len(cols)-1] == "" {
			cols = cols[:len(cols)-1]
		}
		b.WriteString(strings.Join(cols, "\t") + "\n")
	}
	return writeFile(filepath.Join(rel, ListFile), b.String())
}

// writeFile replaces a file whole, so a reader never sees half of it.
func writeFile(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Bookmark lists the images a release runs (List), records them with
// their ids and tags each image with its bookmark. A release listed
// before (one fetched again: a rollback or a restart whose bookmarks were
// gone) gives up what the new listing no longer has: its bookmarks go,
// or they would hold their images forever, and the names it dropped are
// recorded (LeftFile) for the next pruning, which holds the lock alone:
// this runs beside other releases' fetches, which may be pulling them.
// A listing that fails keeps the previous one, so its bookmarks are
// still found.
func (s *Set) Bookmark(rel string) {
	id := filepath.Base(rel)
	es, err := s.entries(rel)
	if err != nil {
		s.say(report.WarnLevel, "could not list the images of %s: a rollback to it builds or pulls them again", id)
		return
	}
	if err := s.keep(rel, es); err != nil {
		s.say(report.WarnLevel, "could not record the images of %s (%v): a rollback to it builds or pulls them again", id, err)
	}
}

// entries are the images a release runs now, one per service (List),
// each as Docker has it under its name.
func (s *Set) entries(rel string) ([]entry, error) {
	svcs, err := s.List(rel)
	if err != nil {
		return nil, err
	}
	es := make([]entry, 0, len(svcs))
	s.built, s.data = map[string]bool{}, map[string]bool{}
	for _, svc := range svcs {
		s.built[svc.Name], s.data[svc.Name] = svc.Build, svc.Data
		es = append(es, s.entryOf(svc, digestRef(svc.Ref)))
	}
	return es, nil
}

// entryOf is svc's entry, its image the one Docker has as ref (its own
// name, or a digest holding it).
func (s *Set) entryOf(svc Service, ref string) entry {
	e := entry{Ref: svc.Ref, Service: svc.Name, Kind: KindPulled}
	if svc.own(s.Mode) {
		e.Kind = KindOwn
		if svc.Build {
			e.Build = svc.BuildKey
		}
	}
	img, err := s.Docker.Inspect(ref)
	if err != nil {
		return e
	}
	e.ID = img.ID
	if e.Kind != KindOwn {
		e.Digest = digestIn(img, svc.Ref)
		e.Version = sanitize(img.Version)
		if e.Digest == "" {
			// Nothing to hold it by: restored like an own image.
			e.Kind = KindOwn
		} else if svc.Data && !svc.Build {
			e.Kind = KindData
		}
	}
	return e
}

// digestIn is img's registry digest for ref's repository, as a name,
// spelled as Docker shows it (mariadb@sha256:..., not
// docker.io/library/mariadb@...): every hold written the same way, so a
// rollback holding the same image never changes the reference compose
// sees, which would recreate the container; "" without one.
func digestIn(img Image, ref string) string {
	for _, d := range img.Digests {
		if repoKey(d) == repoKey(ref) {
			return repoKey(ref) + "@" + digestOf(d)
		}
	}
	return ""
}

// digestOf is a name's digest, sha256:...; "" without one.
func digestOf(ref string) string {
	_, d, _ := strings.Cut(ref, "@")
	return d
}

// keep records a release's entries (ListFile) and tags each image with
// its bookmark, giving up what an earlier listing of it no longer has.
func (s *Set) keep(rel string, es []entry) error {
	id := s.idOf(rel)
	old := readList(rel)
	// A name and the image it had: a listing again under the same name
	// may give it another image (a rollback holding a newer one), which
	// leaves the old pair to nothing that would remove it.
	listed := map[string]bool{}
	// The names es still gives an image in its refs' repositories, by
	// name and id, which its removal takes (Remove): one of them is no
	// name left behind (review: an import's sync:<service> entry,
	// dropped, recorded the release's own mariadb:11.8 and its hold). A
	// digest in another repository (a sync's hold through the parent's
	// mirror) no removal of es takes: it is recorded (review: it outlived
	// every release).
	named := map[string]bool{}
	for _, e := range es {
		listed[familiar(e.Ref)+" "+e.ID] = true
		named[familiar(digestRef(e.Ref))+" "+e.ID] = true
		if e.Digest != "" && repoKey(e.Digest) == repoKey(e.Ref) {
			named[familiar(e.Digest)+" "+e.ID] = true
		}
	}
	if err := writeList(rel, es); err != nil {
		return err
	}
	names := bookmarks(es, s.Name, id)
	keep := map[string]bool{}
	for i, e := range es {
		keep[familiar(names[i])] = true
		// By id: another release's fetch may move the name meanwhile.
		if e.ID != "" {
			if err := s.Docker.Tag(e.ID, names[i]); err != nil {
				s.say(report.WarnLevel, "could not bookmark %s as %s (%v): a rollback to %s builds or pulls it again", e.Ref, names[i], err, id)
			}
		}
	}
	oldNames := bookmarks(old, s.Name, id)
	for i, e := range old {
		if b := oldNames[i]; !keep[familiar(b)] {
			if img, err := s.Docker.ID(b); err == nil {
				if err := s.Docker.Remove(b); err != nil {
					s.leftAdd(b, img, id, err)
				}
			}
		}
		if e.ID != "" && !listed[familiar(e.Ref)+" "+e.ID] {
			// Its names, this one and those other releases of its
			// repositories counted on it to take (Remove): the next pruning
			// retries them, removing a name only once nothing holds or uses
			// its image.
			if cur, err := s.Docker.ID(digestRef(e.Ref)); err == nil && cur == e.ID && !named[familiar(digestRef(e.Ref))+" "+e.ID] {
				s.record(digestRef(e.Ref), e.ID, id, "no release lists it any more")
			}
			tags, _ := s.Docker.Tags(e.ID)
			for _, t := range tags {
				if !isBookmark(t) && repoKey(t) == repoKey(e.Ref) && !named[familiar(t)+" "+e.ID] {
					s.record(t, e.ID, id, "no release lists it any more")
				}
			}
		}
	}
	return nil
}

// own is every bookmark the environment's releases and snapshots make,
// rel's siblings and rel, as Docker shows them: what a restore may move
// without changing another environment. Without a snapshot's own, every
// rollback of a built image after a snapshot would take its bookmark for
// another environment's, and leave the name on the newer image (third
// review of the snapshots plan).
func (s *Set) own(rel string) map[string]bool {
	own := map[string]bool{}
	dirs, _ := filepath.Glob(filepath.Join(filepath.Dir(rel), "*"))
	if b := s.base(); b != "" {
		more, _ := filepath.Glob(filepath.Join(b, "releases", "*"))
		dirs = append(dirs, more...)
	}
	if sn := s.snapshots(); sn != "" {
		more, _ := filepath.Glob(filepath.Join(sn, "*"))
		dirs = append(dirs, more...)
	}
	for _, d := range dirs {
		for _, b := range bookmarks(readList(d), s.Name, s.idOf(d)) {
			own[familiar(b)] = true
		}
	}
	return own
}

// SnapshotsEnv and BaseEnv are where an environment's snapshots go and
// its directory, which every script exports (remote's basePrelude) for
// the devopsy it runs: a Set without its own reads them.
const (
	SnapshotsEnv = "DEVOPSY_INTERNAL_SNAPSHOTS"
	BaseEnv      = "DEVOPSY_INTERNAL_BASE"
)

func (s *Set) snapshots() string { return cmp.Or(s.Snapshots, os.Getenv(SnapshotsEnv)) }
func (s *Set) base() string      { return cmp.Or(s.Base, os.Getenv(BaseEnv)) }

// idOf is the id rel's bookmarks carry: a release's id, s<id> for a
// snapshot's directory (the two kinds' ids may be equal).
func (s *Set) idOf(rel string) string {
	if sn := s.snapshots(); sn != "" && filepath.Clean(filepath.Dir(rel)) == filepath.Clean(sn) {
		// A snapshot being made (<id>.tmp) bookmarks as the snapshot
		// it becomes: its leftover's removal frees them.
		return "s" + strings.TrimSuffix(filepath.Base(rel), ".tmp")
	}
	return filepath.Base(rel)
}

// BookmarkFrom bookmarks the images release rel lists (ListFile) for
// the snapshot directory dir (s<id>), listing them there too: what
// Remove(dir) frees when the snapshot goes.
func (s *Set) BookmarkFrom(dir, rel string) error {
	es := readList(rel)
	if len(es) == 0 {
		return errors.New("release " + filepath.Base(rel) + " lists no images")
	}
	return s.keep(dir, es)
}

// others are who besides this environment uses an image: other compose
// projects' containers, and bookmarks none of its releases made. When
// Docker cannot tell, that is said as a user, so the name stays.
func (s *Set) others(img string, own map[string]bool) []string {
	users, err := s.Docker.Users(img)
	if err != nil {
		return []string{"Docker could not tell: " + err.Error()}
	}
	var others []string
	for _, u := range users {
		if u != s.Name {
			others = append(others, u)
		}
	}
	who := describe(others)
	tags, err := s.Docker.Tags(img)
	if err != nil {
		return append(who, "Docker could not tell: "+err.Error())
	}
	return append(who, holding(tags, own, s.leftBookmarks()[img])...)
}

// Remove takes a going release's images: its bookmarks are untagged, and
// each image it ran that no other bookmark holds (any release,
// environment or project) and no container uses loses the names in the
// release's repositories that still point to it, so Docker deletes it.
// A name that moved on to another image stays; one whose image a
// container uses is recorded (LeftFile), never stripped from it. Every
// decision comes from Docker's state and the release's listing, so a
// removal cut off anywhere is finished by the next one. An error from
// Docker other than a missing image stops it: the release must stay, so
// the next removal finds its listing again.
func (s *Set) Remove(rel string) error {
	id := s.idOf(rel)
	es := readList(rel)
	if len(es) == 0 {
		return nil
	}
	names := bookmarks(es, s.Name, id)
	mine := map[string]bool{}
	imgs := make([]string, len(es))
	for i, e := range es {
		imgs[i] = e.ID
		b := names[i]
		if mine[familiar(b)] {
			continue // a ref listed twice
		}
		mine[familiar(b)] = true
		bm, err := s.Docker.ID(b)
		if errors.Is(err, ErrNoImage) {
			continue
		}
		if err != nil {
			return err
		}
		if imgs[i] == "" { // a v0.38 listing, without ids
			imgs[i] = bm
		}
		if err := s.Docker.Remove(b); err != nil && !errors.Is(err, ErrNoImage) {
			s.leftAdd(b, bm, id, err)
		}
	}
	var order []string
	refs := map[string][]string{}
	for i, img := range imgs {
		if img == "" {
			continue
		}
		if _, ok := refs[img]; !ok {
			order = append(order, img)
		}
		refs[img] = appendNew(refs[img], es[i].Ref)
	}
	var stay, gone []string
	dead := s.leftBookmarks()
	for _, img := range order {
		tags, err := s.Docker.Tags(img)
		if errors.Is(err, ErrNoImage) {
			continue // gone with its bookmark
		}
		if err != nil {
			return err
		}
		// Its names in the release's repositories: the ones it listed that
		// still point to the image, and the image's other names there (one
		// release ran mariadb:11, another mariadb:11.4: whichever goes last
		// takes both).
		keys := map[string]bool{}
		var rm []string
		for _, r := range refs[img] {
			keys[repoKey(r)] = true
			if strings.Contains(r, "@") {
				if cur, err := s.Docker.ID(digestRef(r)); err == nil && cur == img {
					rm = appendNew(rm, digestRef(r))
				}
			}
		}
		for _, t := range tags {
			if !isBookmark(t) && keys[repoKey(t)] {
				rm = appendNew(rm, t)
			}
		}
		if holders := holding(tags, mine, dead[img]); len(holders) > 0 {
			// The releases holding it take the names in their own
			// repositories when they go; the ones in others (a mirror
			// beside bitnami/redis, a digest) would outlive them all:
			// recorded, retried once nothing holds the image.
			repos := map[string]bool{}
			for _, h := range holders {
				repos[repoKey(h)] = true
			}
			for _, r := range refs[img] {
				stay = appendNew(stay, r)
			}
			for _, r := range rm {
				if !repos[repoKey(r)] || strings.Contains(r, "@") {
					s.record(r, img, id, "another release holds its image under another name")
				}
			}
			continue
		}
		if len(rm) == 0 {
			continue
		}
		// A container keeps the image whatever happens to its names: taking
		// all but the last would only cost whoever runs them a pull.
		users, err := s.Docker.Users(img)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			why := "a container of " + strings.Join(describe(users), ", ") + " uses its image"
			for _, r := range rm {
				s.record(r, img, id, why)
			}
			s.say(report.WarnLevel, "%s stay: %s (recorded: --releases lists them, the next pruning retries)", strings.Join(rm, ", "), why)
			continue
		}
		for _, r := range rm {
			// Gone already: removing a sibling name took the image.
			if err := s.Docker.Remove(r); err != nil && !errors.Is(err, ErrNoImage) {
				s.leftAdd(r, img, id, err)
			} else {
				gone = append(gone, r)
			}
		}
	}
	if len(stay) > 0 {
		s.say(report.InfoLevel, "the images of %s that other releases hold stay: %s", id, strings.Join(stay, ", "))
	}
	if len(gone) > 0 {
		s.say(report.InfoLevel, "removed %s, which only %s held", strings.Join(gone, ", "), id)
	}
	return nil
}

// holding are the bookmarks among an image's tags that are not in mine
// nor dead.
func holding(tags []string, mine, dead map[string]bool) []string {
	var hs []string
	for _, t := range tags {
		if isBookmark(t) && !mine[familiar(t)] && !dead[familiar(t)] {
			hs = append(hs, t)
		}
	}
	return hs
}

// describe names compose projects for messages, once each.
func describe(users []string) []string {
	var ds []string
	for _, u := range users {
		if u == "" {
			u = "a project outside Compose"
		}
		ds = appendNew(ds, u)
	}
	return ds
}

func appendNew(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// Prune retries the project's record, then removes the dangling images
// removals left (docker image prune -f, server-wide: an untagged image
// built by hand goes too).
func (s *Set) Prune() {
	s.retry()
	if freed, err := s.Docker.Prune(); err == nil && freed != "" && freed != "0B" {
		s.say(report.InfoLevel, "dangling images pruned, %s freed", freed)
	}
}

// left is one line of LeftFile.
type left struct {
	time, name, release, ref, id, why string
}

func (l left) line() string {
	return strings.Join([]string{l.time, l.name, l.release, l.ref, l.id, l.why}, "\t") + "\n"
}

// readLeft reads a record: a last line cut short of its newline counts.
func readLeft(path string) []left {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var ls []left
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.SplitN(sc.Text(), "\t", 6)
		if len(p) < 6 || p[3] == "" {
			continue
		}
		ls = append(ls, left{p[0], p[1], p[2], p[3], p[4], p[5]})
	}
	return ls
}

// leftAdd records a name Docker refused to remove, then says so: a
// session gone must not cost the record.
func (s *Set) leftAdd(ref, img, release string, err error) {
	why := strings.Join(strings.Fields(err.Error()), " ")
	s.record(ref, img, release, why)
	s.say(report.WarnLevel, "%s stays: %s (recorded: --releases lists it, the next pruning retries)", ref, why)
}

// record appends a name to the project's record, once. One write with
// O_APPEND, never a rewrite: environments of one project record beside
// each other under the shared lock; only retry, alone under the lock,
// rewrites it.
func (s *Set) record(ref, img, release, why string) {
	for _, l := range readLeft(s.Left) {
		if l.ref == ref && l.id == img {
			return
		}
	}
	name := strings.ReplaceAll(s.Name, " ", "_")
	if name == "" {
		name = "a release"
	}
	line := left{strconv.FormatInt(time.Now().Unix(), 10), name, release, ref, img, strings.Join(strings.Fields(why), " ")}.line()
	err := os.MkdirAll(filepath.Dir(s.Left), 0o755)
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(s.Left, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644); err == nil {
			_, err = f.WriteString(line)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		s.say(report.WarnLevel, "could not record %s (%v): remove it by hand once nothing uses it", ref, err)
	}
}

// retry goes over the project's record: a line goes once its name is
// gone, points to another image or is removed. A name a release holds
// again stays recorded, since that release's removal only removes the
// names it listed, and so does one whose image a container uses (taking
// a name from it frees nothing). Any error but a missing image keeps the
// line.
func (s *Set) retry() {
	ls := readLeft(s.Left)
	if len(ls) == 0 {
		return
	}
	var keep []left
	for _, l := range ls {
		cur, err := s.Docker.ID(l.ref)
		if errors.Is(err, ErrNoImage) {
			continue
		}
		if err != nil {
			keep = append(keep, l)
			continue
		}
		if cur != l.id {
			continue
		}
		if !isBookmark(l.ref) && (s.held(l.id) || s.used(l.id)) {
			keep = append(keep, l)
			continue
		}
		if err := s.Docker.Remove(l.ref); err != nil {
			keep = append(keep, l)
			continue
		}
		s.say(report.InfoLevel, "removed %s, left by release %s of %s", l.ref, l.release, l.name)
	}
	if len(keep) == 0 {
		os.Remove(s.Left)
		return
	}
	var b strings.Builder
	for _, l := range keep {
		b.WriteString(l.line())
	}
	if err := writeFile(s.Left, b.String()); err != nil {
		s.say(report.WarnLevel, "could not rewrite %s (%v): it is retried as it was", s.Left, err)
	}
}

// held says whether a bookmark holds an image; when Docker cannot tell,
// it does.
func (s *Set) held(img string) bool {
	tags, err := s.Docker.Tags(img)
	return err != nil || len(holding(tags, nil, s.leftBookmarks()[img])) > 0
}

// leftBookmarks are the bookmarks the project's record holds, per image
// they were recorded on: their release went, or no longer counts on
// them, so they hold nothing (retry removes them). Taken for holders, a
// release of the project running that image again kept its names for
// them, and once the retry removed the bookmark the names outlived every
// release (found by TestDataRandom).
func (s *Set) leftBookmarks() map[string]map[string]bool {
	dead := map[string]map[string]bool{}
	for _, l := range readLeft(s.Left) {
		if isBookmark(l.ref) {
			if dead[l.id] == nil {
				dead[l.id] = map[string]bool{}
			}
			dead[l.id][familiar(l.ref)] = true
		}
	}
	return dead
}

// used says whether a container uses an image; when Docker cannot tell,
// one does.
func (s *Set) used(img string) bool {
	users, err := s.Docker.Users(img)
	return err != nil || len(users) > 0
}

func sortStrings(ss []string) { sort.Strings(ss) }
