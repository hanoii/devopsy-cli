package images

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// StaleAfter is how long an image the project builds and a registry
// serves (image mode) may stay the same, while releases go on, before a
// release says its base may have patches.
const StaleAfter = 30 * 24 * time.Hour

// sayAll says a message with its details and fixes (report.Lines).
func (s *Set) sayAll(lv report.Level, msg string, details []string, fixes ...report.Remedy) {
	for _, l := range report.Lines(lv, msg, details, fixes) {
		_, _ = io.WriteString(s.Out, l+"\n")
	}
}

// Record writes what a new release runs, once its images are fetched:
// the listing (ListFile, with each image's digest and version), the
// bookmarks and its ComposeFile, which labels each service with the
// image it was given and holds each pulled data service by digest. prev
// is the release it replaces ("" for a first one): its listing says what
// moved, and what each data service's name was.
//
// A pulled data service's new image must not be older than the newest
// its data ran (HistoryFile): one its data moved past (a tag pushed
// again backward, a stale mirror, a pin lowered) is refused, and so is
// an image it never ran under a changed name (a series switched, a pin
// bumped or lowered), whose order the history cannot tell. A tag moving
// under the same name is forward. Refused, the names the fetch moved go
// back (SaveNames) and ErrRefused; with yes, it goes on, and the
// service's history starts over from that image once it runs
// (ForcedFile, Ran).
func (s *Set) Record(rel, prev string) error {
	es, err := s.entries(rel)
	if err != nil {
		s.say(report.ErrorLevel, "could not list the images of %s: %v", filepath.Base(rel), err)
		return err
	}
	old := readList(prev)
	base := baseOf(rel)
	h := s.seed(base, old, es)
	var refused, refusedSvcs, refusedRefs, accepted, forced []string
	holds := map[string]string{}
	// A release that syncs: the parent's data replaces the child's, so
	// its images are checked against the parent's (SyncCheck, below); a
	// sync that does not complete brings the release before back, on
	// its own images.
	parent := parseParentData(s.SyncData)
	for _, e := range es {
		// A pulled data service; or a built one whose data has a history
		// (it ran a pulled image before), checked by its name: a switch
		// between pulled and built is one its data never ran, its own
		// builds after go through (no digest to order them by). A
		// project that always built its database has no history.
		_, hasHistory := h.last(e.Service)
		if e.Kind != KindData && !(e.Kind == KindOwn && s.data[e.Service] && hasHistory) {
			continue
		}
		var img Image
		if e.Kind == KindData {
			holds[e.Service] = e.Digest
			if parent[e.Service] != nil {
				continue
			}
			img, err = s.Docker.Inspect(e.Digest)
		} else {
			img, err = s.Docker.Inspect(e.ID)
		}
		if err != nil {
			img = Image{ID: e.ID}
		}
		why := h.check(old, e, img)
		switch {
		case why == "":
		case s.Accept[e.Service]:
			accepted = append(accepted, why)
			forced = append(forced, e.Service+"\t"+img.ID)
		default:
			refused = append(refused, why)
			refusedSvcs = append(refusedSvcs, e.Service)
			refusedRefs = append(refusedRefs, e.Ref)
		}
	}
	if len(refused) > 0 {
		s.putNamesBack(rel, es, refusedRefs)
		fixes := []report.Remedy{
			report.Fix("release an image its data ran, or a newer one under the same name"),
			report.Fix("or, knowing its data takes it (an upgrade devopsy cannot order), accept it, its data's history starting over from it:", s.again("--release", refusedSvcs)),
		}
		s.sayAll(report.ErrorLevel, "the release's images would take data where it never was:", refused, fixes...)
		return ErrRefused
	}
	if len(accepted) > 0 {
		s.sayAll(report.WarnLevel, "accepted (--accept-image): running them anyway, the history of their data starting over from them:", accepted)
	}
	if err := writeForced(rel, forced); err != nil {
		s.say(report.ErrorLevel, "could not record what --accept-image let through: %v", err)
		return err
	}
	if prev != "" {
		s.moved(old, es)
	}
	s.stale(es, old)
	if err := s.keep(rel, es); err != nil {
		s.say(report.ErrorLevel, "could not record the images of %s: %v", filepath.Base(rel), err)
		return err
	}
	if err := writeCompose(rel, es, holds); err != nil {
		s.say(report.ErrorLevel, "could not write %s's %s: %v", filepath.Base(rel), ComposeFile, err)
		return err
	}
	// A release that syncs: the parent's data must take the child's
	// images, checked against what the release fetched.
	if s.SyncData != "" {
		refs, err := s.syncCheck(rel, s.SyncData, "--release")
		if errors.Is(err, ErrRefused) {
			s.putNamesBack(rel, es, refs)
		}
		return err
	}
	return nil
}

// check says why img, e's new image, must not run on its data: "" when
// it may. old is the release before's listing.
func (h history) check(old []entry, e entry, img Image) string {
	last, ok := h.last(e.Service)
	if !ok {
		// Nothing ran on its data that devopsy knows of: a new name for it
		// cannot be ordered.
		if o, ok := match(old, e); ok && familiar(o.Ref) != familiar(e.Ref) {
			return fmt.Sprintf("%s: %s (%s) never ran on its data, and its image was %s", e.Service, e.Ref, shownEntry(e), o.Ref)
		}
		return ""
	}
	switch {
	case last.is(img):
		return ""
	case h.movedPast(e.Service, img):
		return fmt.Sprintf("%s: %s is %s, an image its data moved past: it last ran %s", e.Service, e.Ref, shownEntry(e), last.shown())
	case familiar(last.Ref) != familiar(e.Ref):
		return fmt.Sprintf("%s: %s (%s) never ran on its data, which last ran %s as %s: devopsy cannot tell a newer image from an older one under another name", e.Service, e.Ref, shownEntry(e), last.shown(), last.Ref)
	}
	return ""
}

// seed starts the history of a pulled data service that has none with
// what its container runs, once started: a service that just gained its
// label. Written under the release lock.
func (s *Set) seed(base string, old, es []entry) history {
	h := readHistory(base)
	var cs map[string]Container
	changed := false
	for _, e := range es {
		if e.Kind != KindData {
			continue
		}
		if _, ok := h.last(e.Service); ok {
			continue
		}
		if cs == nil {
			cs = s.containers()
		}
		c, ok := cs[e.Service]
		if !ok || !c.started() {
			continue
		}
		img, err := s.Docker.Inspect(c.Image)
		if err != nil {
			continue
		}
		// The name it ran as: its container's, else the release before's.
		ref := e.Ref
		if c.Ref != "" && !strings.Contains(c.Ref, "@") {
			ref = c.Ref
		} else if o, ok := match(old, e); ok {
			ref = o.Ref
		}
		h = append(h, ranOn(e.Service, Ran, ref, img, s.now()))
		changed = true
	}
	if changed {
		if err := h.write(base); err != nil {
			s.say(report.WarnLevel, "could not write the history of the images that ran on the data: %v", err)
		}
	}
	return h
}

func writeForced(rel string, lines []string) error {
	path := filepath.Join(rel, ForcedFile)
	if len(lines) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeFile(path, strings.Join(lines, "\n")+"\n")
}

// NamesFile, in a new release's directory, is what its pulled names held
// before its fetch (SaveNames): a refused release puts them back, since
// its pull moved them for every environment on the server.
const NamesFile = "devopsy-names"

// SaveNames records what each pulled name of a release held, before its
// fetch moves them.
func (s *Set) SaveNames(rel string) error {
	svcs, err := s.List(rel)
	if err != nil {
		s.say(report.ErrorLevel, "could not list the images of %s: %v", filepath.Base(rel), err)
		return err
	}
	var b strings.Builder
	for _, svc := range svcs {
		if svc.own(s.Mode) || strings.Contains(svc.Ref, "@") {
			continue
		}
		// Docker failing: unknown, so never put back (a refusal would take
		// a name that exists for one that was not there).
		id, err := s.Docker.ID(svc.Ref)
		if err != nil && !errors.Is(err, ErrNoImage) {
			continue
		}
		b.WriteString(svc.Ref + "\t" + id + "\n")
	}
	return writeFile(filepath.Join(rel, NamesFile), b.String())
}

// putNamesBack points the names of a refused release's refused images
// (refs) back at what they held before its fetch (NamesFile), so another
// environment's plain up never runs what was refused here: only while a
// name still holds the image this release's fetch gave it (es), never
// one another environment's fetch moved since, and never another name
// of the release (a stateless image's patch stays). A name that was not
// there before goes.
func (s *Set) putNamesBack(rel string, es []entry, refs []string) {
	data, err := os.ReadFile(filepath.Join(rel, NamesFile))
	if err != nil {
		return
	}
	fetched := map[string]string{}
	for _, e := range es {
		fetched[familiar(e.Ref)] = e.ID
	}
	refused := map[string]bool{}
	for _, r := range refs {
		refused[familiar(r)] = true
	}
	for _, line := range strings.Split(string(data), "\n") {
		ref, id, _ := strings.Cut(line, "\t")
		if ref == "" || !refused[familiar(ref)] {
			continue
		}
		cur, err := s.Docker.ID(ref)
		switch {
		case err != nil || cur == id || cur != fetched[familiar(ref)]:
		case id == "":
			_ = s.Docker.Remove(ref)
		default:
			if err := s.Docker.Tag(id, ref); err != nil {
				s.say(report.WarnLevel, "could not put %s back on the image it held (%v): other environments' next up may run what was refused here", ref, err)
			}
		}
	}
}

// match is the entry of old for the service of e.
func match(old []entry, e entry) (entry, bool) {
	for _, o := range old {
		if o.Service != "" && o.Service == e.Service {
			return o, true
		}
	}
	return entry{}, false
}

// moved says, one line per service, which images changed from the
// release before: a pulled one by version when both have one, else by
// short digest, an own one rebuilt; and an image the project builds that
// changed under the same name in image mode, which no rollback takes
// back.
func (s *Set) moved(old, es []entry) {
	var lines []string
	var sameTag []string
	for _, e := range es {
		o, ok := match(old, e)
		if !ok || o.ID == "" || e.ID == "" || o.ID == e.ID {
			continue
		}
		if e.Kind == KindOwn {
			lines = append(lines, e.Service+": rebuilt")
			continue
		}
		name := e.Ref
		if familiar(o.Ref) != familiar(e.Ref) {
			name = o.Ref + " -> " + e.Ref
		}
		lines = append(lines, fmt.Sprintf("%s: %s -> %s", name, shown(o), shown(e)))
		if s.Mode == ModeImage && s.builds(e) && familiar(o.Ref) == familiar(e.Ref) {
			sameTag = append(sameTag, e.Service)
		}
	}
	if len(lines) > 0 {
		s.sayAll(report.InfoLevel, "images that changed:", lines)
	}
	for _, svc := range sameTag {
		e, _ := match(es, entry{Service: svc})
		s.sayAll(report.WarnLevel, fmt.Sprintf("%s: a new image under the same name %s, which no rollback takes back", svc, e.Ref),
			nil, report.Fix("tag the project's images by commit (${DEVOPSY_RELEASE_COMMIT}) in image mode"))
	}
}

// builds says whether the project builds e's image (a build: section),
// from the services List gave this run.
func (s *Set) builds(e entry) bool { return s.built[e.Service] }

// shown is an image for messages: its version when it has one, else its
// short digest, else its short id.
func shown(e entry) string {
	if e.Version != "" {
		return e.Version
	}
	d := digestOf(e.Digest)
	if d == "" {
		d = e.ID
	}
	return short(d)
}

// short is an image digest or id cut to sha256: and 12 hex digits.
func short(d string) string {
	if len(d) > len("sha256:")+12 {
		return d[:len("sha256:")+12]
	}
	return d
}

// stale says when an image the project builds and pulls (image mode)
// is the release before's, unchanged for StaleAfter: a rebuild would
// take its base's patches.
func (s *Set) stale(es, old []entry) {
	if s.Mode != ModeImage {
		return
	}
	for _, e := range es {
		o, ok := match(old, e)
		if !s.builds(e) || !ok || o.ID != e.ID || e.ID == "" {
			continue
		}
		img, err := s.Docker.Inspect(e.ID)
		if err != nil || img.Created.IsZero() {
			continue
		}
		if age := s.now().Sub(img.Created); age > StaleAfter {
			s.sayAll(report.WarnLevel, fmt.Sprintf("%s's image was built %d days ago: its base may have patches", e.Service, int(age.Hours()/24)),
				nil, report.Fix("rebuild and push it (build --pull), then release"))
		}
	}
}

func (s *Set) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// sanitize keeps a value on one listing column.
func sanitize(v string) string { return strings.Join(strings.Fields(v), "_") }

// again is the command that runs it again accepting svcs: devopsy
// @<address> <flag> --accept-image <svcs>.
func (s *Set) again(flag string, svcs []string) string {
	cmd := flag + " --accept-image " + strings.Join(svcs, ",")
	if s.Address != "" {
		cmd = "devopsy @" + s.Address + " " + cmd
	}
	return cmd
}
