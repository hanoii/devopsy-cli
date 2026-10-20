// Package images gives each release on a server its own images. Release
// scripts run it on the server (devopsy --images, under the server's
// image lock, which the scripts take), so one devopsy, the server's,
// applies the rules to images every project there shares.
//
// A release bookmarks every image it runs with a second tag of its own,
// <repository>:devopsy-<compose name>-<release id> (Bookmark). The
// bookmarks are the reference count: a release going (Remove) untags
// its bookmarks, and an image no other bookmark holds loses the names
// the release ran, so Docker deletes it. A rollback or a restart points
// the names back at a release's bookmarks (Restore), so it runs exactly
// the images it ran without building or pulling.
package images

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// Files in a release's directory, beside its code, and the project's
// record.
const (
	// ListFile lists the images a release runs (compose config
	// --images), one per line: the name, then a tab and the image it had
	// when bookmarked, when Docker had it.
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
}

// Set is one environment's images on a server.
type Set struct {
	Docker Docker
	// Name is the environment's compose name, which its bookmarks carry.
	Name string
	// Left is its project's LeftFile.
	Left string
	// Out takes what it says (report.Line).
	Out io.Writer
	// List lists the images a release runs (ListCompose).
	List func(rel string) ([]string, error)
}

// entry is one image a release runs.
type entry struct {
	Ref string
	// ID is the image Ref had when bookmarked; "" when Docker had none.
	ID string
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
// sha256, which keeps names that differ only past the cut apart.
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
// overwrites another.
func bookmarks(entries []entry, name, id string) []string {
	count := map[string]int{}
	for _, e := range entries {
		count[repoKey(e.Ref)]++
	}
	prefix := bookmarkPrefix(name)
	bs := make([]string, len(entries))
	for i, e := range entries {
		b := repoOf(e.Ref) + ":" + prefix + id
		if count[repoKey(e.Ref)] > 1 {
			b += "-" + strconv.FormatUint(uint64(cksum([]byte(e.Ref))), 10)
		}
		bs[i] = b
	}
	return bs
}

// readList reads a release's ListFile: nil when it has none. Lines
// without an id are v0.38's, or an image Docker did not have.
func readList(rel string) []entry {
	data, err := os.ReadFile(filepath.Join(rel, ListFile))
	if err != nil {
		return nil
	}
	var es []entry
	for _, line := range strings.Split(string(data), "\n") {
		ref, id, _ := strings.Cut(strings.TrimSpace(line), "\t")
		if ref != "" {
			es = append(es, entry{Ref: ref, ID: id})
		}
	}
	return es
}

func writeList(rel string, es []entry) error {
	var b strings.Builder
	for _, e := range es {
		b.WriteString(e.Ref)
		if e.ID != "" {
			b.WriteString("\t" + e.ID)
		}
		b.WriteString("\n")
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
	refs, err := s.List(rel)
	if err != nil {
		s.say(report.WarnLevel, "could not list the images of %s: a rollback to it builds or pulls them again", id)
		return
	}
	old := readList(rel)
	es := make([]entry, 0, len(refs))
	listed := map[string]bool{}
	for _, ref := range refs {
		img, _ := s.Docker.ID(digestRef(ref))
		es = append(es, entry{Ref: ref, ID: img})
		listed[familiar(ref)] = true
	}
	if err := writeList(rel, es); err != nil {
		s.say(report.WarnLevel, "could not record the images of %s (%v): a rollback to it builds or pulls them again", id, err)
		return
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
		if e.ID != "" && !listed[familiar(e.Ref)] {
			if cur, err := s.Docker.ID(digestRef(e.Ref)); err == nil && cur == e.ID {
				s.record(digestRef(e.Ref), e.ID, id, "no release lists it any more")
			}
		}
	}
}

// own is every bookmark the environment's releases make, rel's siblings
// and rel, as Docker shows them: what a restore may move without
// changing another environment.
func (s *Set) own(rel string) map[string]bool {
	own := map[string]bool{}
	dirs, _ := filepath.Glob(filepath.Join(filepath.Dir(rel), "*"))
	for _, d := range dirs {
		for _, b := range bookmarks(readList(d), s.Name, filepath.Base(d)) {
			own[familiar(b)] = true
		}
	}
	return own
}

// Restore points a release's names back at its bookmarks, so it runs
// exactly the images it ran: false, changing nothing, when it has no
// listing or a bookmark is gone. Digests are exact already. A name whose
// current image another environment uses (a container of another compose
// project, any project or none, or a bookmark this environment's
// releases did not make) stays on it, said: moving a shared name like
// mariadb:11 back would change that environment at its next recreate.
// When Docker cannot tell, it stays too.
func (s *Set) Restore(rel string) bool {
	id := filepath.Base(rel)
	es := readList(rel)
	if len(es) == 0 {
		return false
	}
	names := bookmarks(es, s.Name, id)
	marks := make([]string, len(es))
	for i, e := range es {
		// A digest is exact, but its image must still be there: a restore
		// that leaves it to the run step's up would pull after the switch.
		// One Docker never had (no id) was never bookmarked.
		if strings.Contains(e.Ref, "@") && e.ID == "" {
			continue
		}
		img, err := s.Docker.ID(names[i])
		if err != nil {
			return false
		}
		marks[i] = img
	}
	// Every check before any tag, so a Docker error changes nothing.
	own := s.own(rel)
	var stayed []string
	var tag []int
	for i, e := range es {
		if strings.Contains(e.Ref, "@") {
			continue
		}
		cur, err := s.Docker.ID(e.Ref)
		if err != nil && !errors.Is(err, ErrNoImage) {
			return false
		}
		if err == nil && cur != marks[i] {
			if who := s.others(cur, own); len(who) > 0 {
				s.say(report.WarnLevel, "%s stays on its newer image, which other environments use (%s): %s runs it too", e.Ref, strings.Join(who, ", "), id)
				stayed = append(stayed, e.Ref)
				continue
			}
		}
		tag = append(tag, i)
	}
	for _, i := range tag {
		if err := s.Docker.Tag(names[i], es[i].Ref); err != nil {
			return false
		}
	}
	if len(stayed) > 0 {
		s.say(report.WarnLevel, "images of %s restored, but for %s", id, strings.Join(stayed, ", "))
	} else {
		s.say(report.InfoLevel, "images of %s restored", id)
	}
	return true
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
	return append(who, holding(tags, own)...)
}

// Undo puts the live release's names back after a release's fetch failed
// or was interrupted: a new release's own images are bookmarked first,
// so they go with it; a rollback's are not (the names may hold the live
// release's images, which would become the target's).
func (s *Set) Undo(rel, prev string, rollback bool) {
	if !rollback {
		s.Bookmark(rel)
	}
	if prev != "" {
		s.Restore(prev)
	}
}

// Recover finishes what a release cut off mid-fetch without a signal
// left (FetchingFile): marked failed, what it got bookmarked, and the
// current release's names (prev, when there is one) back.
func (s *Set) Recover(base, prev string) {
	marks, _ := filepath.Glob(filepath.Join(base, "releases", "*", FetchingFile))
	for _, m := range marks {
		rel := filepath.Dir(m)
		s.say(report.WarnLevel, "the images' fetch of release %s was interrupted: marking it failed, its images bookmarked", filepath.Base(rel))
		s.Undo(rel, prev, false)
		if f, err := os.Create(filepath.Join(rel, failedFile)); err == nil {
			f.Close()
		}
		os.Remove(m)
	}
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
	id := filepath.Base(rel)
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
		if holders := holding(tags, mine); len(holders) > 0 {
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

// holding are the bookmarks among an image's tags that are not in mine.
func holding(tags []string, mine map[string]bool) []string {
	var hs []string
	for _, t := range tags {
		if isBookmark(t) && !mine[familiar(t)] {
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
	return err != nil || len(holding(tags, nil)) > 0
}

// used says whether a container uses an image; when Docker cannot tell,
// one does.
func (s *Set) used(img string) bool {
	users, err := s.Docker.Users(img)
	return err != nil || len(users) > 0
}
