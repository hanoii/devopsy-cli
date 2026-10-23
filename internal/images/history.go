package images

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// HistoryFile, in an environment's directory beside shared/ (never in a
// release, which pruning removes, nor in shared/, whose entries are
// linked into the project's tree and so into build contexts), is the
// history of the images that ran on each pulled data service's data
// (KindData): what keeps that data from ever meeting an image older than
// the newest it ran. One line per image change, oldest first, tab
// separated: time (Unix seconds), service, how (Ran, Forced, Imported,
// or "sync <parent's address>"), the reference it ran as, the image id,
// and its registry digests (<repository>@sha256:..., space separated).
// Unbounded: a pin lowered to an image that ran long ago must still be
// found. Written only under the environment's release lock, whole, by a
// rename.
const HistoryFile = ".devopsy-data-images"

// How an image came to run on a service's data. Every one but Ran starts
// the history over: a forced image, or data that came from elsewhere,
// makes what ran before stop counting.
const (
	Ran      = "ran"
	Forced   = "forced"
	Imported = "import"
	// SyncedFrom prefixes the parent's address: the copy brought the
	// parent's data, and its history with it.
	SyncedFrom = "sync "
)

// ran is one line of HistoryFile.
type ran struct {
	Time         int64
	Service, How string
	Ref, ID      string
	Digests      []string
}

func (r ran) line() string {
	return strings.Join([]string{strconv.FormatInt(r.Time, 10), r.Service, r.How, r.Ref, r.ID, strings.Join(r.Digests, " ")}, "\t") + "\n"
}

// is says whether img is the image of the line: the same id, or a
// registry digest in common, whatever the repository (a digest names one
// manifest anywhere: a mirror that keeps digests serves the same image).
func (r ran) is(img Image) bool {
	if img.ID != "" && img.ID == r.ID {
		return true
	}
	for _, a := range r.Digests {
		for _, b := range img.Digests {
			if digestOf(a) != "" && digestOf(a) == digestOf(b) {
				return true
			}
		}
	}
	return false
}

// hold is the line's image as a name compose can run: its digest in the
// line's reference's repository, else its first.
func (r ran) hold() string {
	for _, d := range r.Digests {
		if repoKey(d) == repoKey(r.Ref) {
			return d
		}
	}
	if len(r.Digests) > 0 {
		return r.Digests[0]
	}
	return ""
}

// shown is the line's image for messages.
func (r ran) shown() string {
	d := r.hold()
	if d == "" {
		return short(r.ID)
	}
	return repoOf(d) + "@" + short(digestOf(d))
}

type history []ran

func historyPath(base string) string { return filepath.Join(base, HistoryFile) }

// readHistory reads an environment's history: empty without one. A line
// cut short counts once it has its image.
func readHistory(base string) history {
	data, err := os.ReadFile(historyPath(base))
	if err != nil {
		return nil
	}
	var h history
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) < 5 || f[1] == "" || f[4] == "" {
			continue
		}
		t, _ := strconv.ParseInt(f[0], 10, 64)
		r := ran{Time: t, Service: f[1], How: f[2], Ref: f[3], ID: f[4]}
		if len(f) > 5 {
			r.Digests = strings.Fields(f[5])
		}
		h = append(h, r)
	}
	return h
}

func (h history) write(base string) error {
	var b strings.Builder
	for _, r := range h {
		b.WriteString(r.line())
	}
	return writeFile(historyPath(base), b.String())
}

// window is what counts of a service's history: its lines from the last
// that started it over (anything but Ran) on.
func (h history) window(svc string) []ran {
	var w []ran
	for _, r := range h {
		if r.Service != svc {
			continue
		}
		if r.How != Ran {
			w = w[:0]
		}
		w = append(w, r)
	}
	return w
}

// last is the newest image that ran on a service's data.
func (h history) last(svc string) (ran, bool) {
	w := h.window(svc)
	if len(w) == 0 {
		return ran{}, false
	}
	return w[len(w)-1], true
}

// movedPast says whether the data moved past img: it ran before the
// newest, which is another image.
func (h history) movedPast(svc string, img Image) bool {
	w := h.window(svc)
	if len(w) == 0 || w[len(w)-1].is(img) {
		return false
	}
	for _, r := range w[:len(w)-1] {
		if r.is(img) {
			return true
		}
	}
	return false
}

// ranOn makes a line for img running as ref on a service's data.
func ranOn(svc, how, ref string, img Image, now time.Time) ran {
	ds := append([]string{}, img.Digests...)
	return ran{Time: now.Unix(), Service: svc, How: how, Ref: ref, ID: img.ID, Digests: ds}
}

// started says whether a container ever ran: created alone runs nothing
// on the data.
func (c Container) started() bool { return c.State != "" && c.State != "created" }

// containers are the environment's containers of each service (no
// one-offs): nil when Docker cannot tell.
func (s *Set) containers() map[string]Container {
	cs, err := s.Docker.Containers(s.Name)
	if err != nil {
		s.say(report.WarnLevel, "could not list %s's containers: %v", s.Name, err)
		return nil
	}
	m := map[string]Container{}
	for _, c := range cs {
		if !c.OneOff && c.Service != "" {
			m[c.Service] = c
		}
	}
	return m
}

// Ran appends to the environment's history the image each pulled data
// service's container runs, once started, when it is not the last one
// there: after a run step (failed or not), a sync, an import. forced
// are the services a --yes let through (ForcedFile), whose image starts
// their history over. One that runs an image its data moved past went
// back outside devopsy: said, never written, so the history keeps the
// newest. rel is the release whose listing names the data services;
// those with a history count too.
func (s *Set) Ran(base, rel string) error {
	svcs := map[string]bool{}
	for _, e := range readList(rel) {
		if e.Kind == KindData {
			svcs[e.Service] = true
		}
	}
	h := readHistory(base)
	for _, r := range h {
		svcs[r.Service] = true
	}
	if len(svcs) == 0 {
		return nil
	}
	forced := readForced(rel)
	cs := s.containers()
	refs := map[string]string{}
	for _, e := range readList(rel) {
		refs[e.Service] = e.Ref
	}
	changed := false
	for _, svc := range sortedKeys(svcs) {
		c, ok := cs[svc]
		if !ok || !c.started() {
			continue
		}
		img, err := s.Docker.Inspect(c.Image)
		if err != nil {
			s.say(report.WarnLevel, "could not read the image of %s (%v): its data's history misses it", c.Name, err)
			continue
		}
		ref := refs[svc]
		if ref == "" {
			ref = repoOf(c.Digest)
		}
		how := Ran
		if forced[svc] {
			how = Forced
		}
		if last, ok := h.last(svc); ok && last.is(img) {
			if how != Forced || last.How == Forced {
				continue
			}
		} else if how == Ran && h.movedPast(svc, img) {
			s.say(report.WarnLevel, "%s runs %s, older than the newest image its data ran (%s): it went back outside devopsy, and the next activation brings the newest back", svc, shownImage(ref, img), mustLast(h, svc).shown())
			continue
		}
		h = append(h, ranOn(svc, how, ref, img, s.now()))
		changed = true
	}
	if !changed {
		return nil
	}
	if err := h.write(base); err != nil {
		s.say(report.ErrorLevel, "could not write the history of the images that ran on the data (%v): the next release's checks may miss them", err)
		return err
	}
	return nil
}

func mustLast(h history, svc string) ran {
	r, _ := h.last(svc)
	return r
}

// shownImage is an image for messages: its digest in ref's repository,
// short, else its short id.
func shownImage(ref string, img Image) string {
	if d := digestIn(img, ref); d != "" {
		return repoOf(d) + "@" + short(digestOf(d))
	}
	return short(img.ID)
}

// ForcedFile, in a release's directory, names the services whose image
// a --yes let through its checks (Record): their history starts over
// from it once it runs (Ran).
const ForcedFile = "devopsy-forced"

func readForced(rel string) map[string]bool {
	data, err := os.ReadFile(filepath.Join(rel, ForcedFile))
	if err != nil {
		return nil
	}
	m := map[string]bool{}
	for _, f := range strings.Fields(string(data)) {
		m[f] = true
	}
	return m
}

func sortedKeys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sortStrings(ks)
	return ks
}

// RemoveHistory forgets the history with the data it described
// (--destroy-data).
func RemoveHistory(base string) error {
	if err := os.Remove(historyPath(base)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("could not remove %s: %w", historyPath(base), err)
	}
	return nil
}
