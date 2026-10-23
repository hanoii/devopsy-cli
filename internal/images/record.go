package images

import (
	"fmt"
	"io"
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
// the listing (ListFile, with each image's digest and version) and the
// bookmarks. prev is the release it replaces ("" for a first one), whose
// listing says what moved.
func (s *Set) Record(rel, prev string) error {
	es, err := s.entries(rel)
	if err != nil {
		s.say(report.ErrorLevel, "could not list the images of %s: %v", filepath.Base(rel), err)
		return err
	}
	if prev != "" {
		s.moved(readList(prev), es)
	}
	s.stale(es, readList(prev))
	if err := s.keep(rel, es); err != nil {
		s.say(report.ErrorLevel, "could not record the images of %s: %v", filepath.Base(rel), err)
		return err
	}
	return nil
}

// match is the entry of old for the service of e: by service, else (a
// listing from before the service column) by name.
func match(old []entry, e entry) (entry, bool) {
	for _, o := range old {
		if o.Service != "" && o.Service == e.Service {
			return o, true
		}
	}
	for _, o := range old {
		if o.Service == "" && familiar(o.Ref) == familiar(e.Ref) {
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
