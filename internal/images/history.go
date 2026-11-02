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
// history of what ran on each data service's data (devopsy.data=true,
// built or pulled): what the one rule (rule) compares an activation's
// version and tag with. One line per change, oldest first, tab
// separated: time (Unix seconds), service, how (Ran, Forced, Imported,
// AnyRan, or "sync <parent's address>"), the reference it ran as, the
// image id, its registry digests (<repository>@sha256:..., space
// separated), the version (DataVersionLabel; "" for any, or data of no
// known version) and the tag (TagOf; "" for a service with build:, which
// has no tag check). A line of the format before the version, without
// those two columns, reads as one of no version: the first release after
// it adopts its label. Unbounded. Written only under the environment's
// release lock, whole, by a rename.
const HistoryFile = ".devopsy-data-images"

// How something came to run on a service's data. Every one but Ran
// starts the window over (window): a forced image, an any run, or data
// that came from elsewhere makes what ran before stop counting.
const (
	Ran      = "ran"
	Forced   = "forced"
	Imported = "import"
	// AnyRan is a run under devopsy.data.version=any: devopsy checks
	// nothing, so a version set later is adopted, never compared with one
	// from before.
	AnyRan = "any"
	// SyncedFrom prefixes the parent's address: the copy brought the
	// parent's data, and its window with it.
	SyncedFrom = "sync "
)

// ran is one line of HistoryFile.
type ran struct {
	Time         int64
	Service, How string
	Ref, ID      string
	Digests      []string
	Version, Tag string
}

func (r ran) line() string {
	return strings.Join([]string{strconv.FormatInt(r.Time, 10), r.Service, r.How, r.Ref, r.ID, strings.Join(r.Digests, " "), r.Version, r.Tag}, "\t") + "\n"
}

// same says whether r and o say the same of the data: the same image,
// version and tag.
func (r ran) same(o ran) bool {
	return r.ID == o.ID && r.Version == o.Version && r.Tag == o.Tag
}

// shown is the line's version and tag, for messages.
func (r ran) shown() string {
	v := r.Version
	if v == "" {
		v = "no version"
	}
	if r.Tag != "" {
		v += " (" + short(r.Tag) + ")"
	}
	return v
}

type history []ran

func historyPath(base string) string { return filepath.Join(base, HistoryFile) }

// readHistory reads an environment's history: empty without one.
func readHistory(base string) history {
	data, err := os.ReadFile(historyPath(base))
	if err != nil {
		return nil
	}
	return parseHistory(string(data))
}

// parseHistory reads history lines: one without its service or how is
// none.
func parseHistory(text string) history {
	var h history
	for _, line := range strings.Split(text, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) < 3 || f[1] == "" || f[2] == "" {
			continue
		}
		col := func(i int) string {
			if i < len(f) {
				return f[i]
			}
			return ""
		}
		t, _ := strconv.ParseInt(f[0], 10, 64)
		h = append(h, ran{Time: t, Service: f[1], How: f[2], Ref: col(3), ID: col(4), Digests: strings.Fields(col(5)), Version: col(6), Tag: col(7)})
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

// last is the newest line of a service's history.
func (h history) last(svc string) (ran, bool) {
	w := h.window(svc)
	if len(w) == 0 {
		return ran{}, false
	}
	return w[len(w)-1], true
}

// without is h less svc's lines.
func (h history) without(svc string) history {
	var kept history
	for _, r := range h {
		if r.Service != svc {
			kept = append(kept, r)
		}
	}
	return kept
}

// rule is the one rule over a service's data and the image an activation
// gives it, which every place data meets an image runs (a release, a
// rollback, a sync, an import, a --no-code restore, a release that
// syncs, and the restart after a failed run step, which holds rather
// than refuses): w is the data's window (window), version the
// activation's DataVersionLabel and tag its TagOf ("" for a service with
// build:, which has no tag check in either mode: image mode's tag is
// usually the commit's, new every release). source is whose data it is,
// for the reason ("its", "the parent's"). "" when allowed, else why not:
//
//   - any (or no version at all, a release from before the label):
//     nothing checked;
//   - no version in the window (its first run, after any, after data of
//     unknown version): allowed, the label adopted;
//   - higher than the window's, the highest of its lines: allowed;
//   - lower: refused;
//   - the same: allowed, unless pulled and its tag differs from the tag
//     of the window's newest line at that version (a line without one, a
//     built service's, means no tag check): an image moved to another tag
//     with the label left as it was is a version forgotten. A floating
//     tag moving upstream is the same tag.
func rule(w []ran, version, tag, source string) string {
	if !IsVersion(version) {
		return ""
	}
	dv := windowVersion(w)
	if dv == "" {
		return ""
	}
	switch c := CompareVersions(version, dv); {
	case c > 0:
		return ""
	case c < 0:
		return fmt.Sprintf("%s=%s is lower than %s data's version, %s", DataVersionLabel, version, source, dv)
	}
	if tag == "" {
		return ""
	}
	var at ran
	for _, r := range w {
		if IsVersion(r.Version) && CompareVersions(r.Version, dv) == 0 {
			at = r
		}
	}
	if at.Tag == "" || at.Tag == tag {
		return ""
	}
	return fmt.Sprintf("its tag is %s, and %s data ran %s at the same %s=%s: an image moved to another tag with the label left as it was", short(tag), source, short(at.Tag), DataVersionLabel, version)
}

// windowVersion is the data's version: the highest of the window's lines,
// those without one ignored; "" without any.
func windowVersion(w []ran) string {
	v := ""
	for _, r := range w {
		if IsVersion(r.Version) && (v == "" || CompareVersions(r.Version, v) > 0) {
			v = r.Version
		}
	}
	return v
}

// ranOn makes a line for img running as e (its reference, version and
// tag) on its service's data.
func ranOn(e entry, how string, img Image, now time.Time) ran {
	r := ran{Time: now.Unix(), Service: e.Service, How: how, Ref: e.Ref, ID: img.ID, Digests: append([]string{}, img.Digests...), Version: e.DataVersion, Tag: e.Tag}
	if e.DataVersion == AnyVersion {
		r.How, r.Version = AnyRan, ""
	}
	return r
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

// Ran appends to the environment's history what each data service of
// rel runs (its listing's version and tag, DataVersion, Tag), for every
// one whose container, once started, runs the image rel's activation
// gave it (its DigestLabel against rel's ComposeFile, as Recreate
// compares), when the last line differs (another image, version or tag):
// after a run step (failed or not), a sync, an import. A service running
// the very image --accept-image let through (ForcedFile) starts its
// history over from it; the file is the activation's and goes once read,
// so a later rollback to the release forces nothing.
func (s *Set) Ran(base, rel string) error { return s.ran(base, rel, false) }

// RanCut is Ran on a release whose activation was cut off past its
// switch (an activation's start, a settle that made services again): a
// ForcedFile line goes only once its service runs that image, so one
// still on the image before (stopped for a release snapshot) keeps it
// for the settle that makes it again.
func (s *Set) RanCut(base, rel string) error { return s.ran(base, rel, true) }

func (s *Set) ran(base, rel string, cut bool) error {
	var data []entry
	for _, e := range readList(rel) {
		if e.DataVersion != "" {
			data = append(data, e)
		}
	}
	forced := readForced(rel)
	if len(data) == 0 {
		if forced != nil && !cut {
			os.Remove(filepath.Join(rel, ForcedFile))
		}
		return nil
	}
	h := readHistory(base)
	cs := s.containers()
	labels := labelsIn(rel)
	changed := false
	ranForced := map[string]bool{}
	for _, e := range data {
		c, ok := cs[e.Service]
		if !ok || !c.started() || labels[e.Service] == "" || c.Digest != labels[e.Service] {
			continue
		}
		img, err := s.Docker.Inspect(c.Image)
		if err != nil {
			s.say(report.WarnLevel, "could not read the image of %s (%v): its data's history misses it", c.Name, err)
			continue
		}
		how := Ran
		if id := forced[e.Service]; id != "" && id == img.ID {
			how = Forced
			ranForced[e.Service] = true
		}
		r := ranOn(e, how, img, s.now())
		if last, ok := h.last(e.Service); ok && last.same(r) && (r.How == Ran || r.How == last.How) {
			continue
		}
		h = append(h, r)
		changed = true
	}
	if changed {
		if err := h.write(base); err != nil {
			s.say(report.ErrorLevel, "could not write the history of the images that ran on the data (%v): the next release's checks may miss them", err)
			return err
		}
	}
	if forced != nil && !cut {
		if err := os.Remove(filepath.Join(rel, ForcedFile)); err != nil && !os.IsNotExist(err) {
			s.say(report.WarnLevel, "could not remove %s: %v", ForcedFile, err)
		}
	} else if forced != nil {
		s.keepForced(rel, forced, ranForced)
	}
	return nil
}

// keepForced rewrites rel's ForcedFile with the lines of the services
// not seen on their forced image (ran), removing it once none is left.
func (s *Set) keepForced(rel string, forced map[string]string, ran map[string]bool) {
	var left []string
	for _, svc := range sortedKeysOf(forced) {
		if !ran[svc] {
			left = append(left, svc+"\t"+forced[svc])
		}
	}
	if len(left) == len(forced) {
		return
	}
	if err := writeForced(rel, left); err != nil {
		s.say(report.WarnLevel, "could not rewrite %s: %v", ForcedFile, err)
	}
}

// ForcedFile, in a release's directory, names each service whose image
// --accept-image let through a check that refused it, with that image's
// id, tab separated: its history starts over from it once that image
// runs (Ran), after the activation that wrote it, never later.
const ForcedFile = "devopsy-forced"

func readForced(rel string) map[string]string {
	data, err := os.ReadFile(filepath.Join(rel, ForcedFile))
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if svc, id, ok := strings.Cut(line, "\t"); ok && svc != "" {
			m[svc] = id
		}
	}
	return m
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

func sortedKeysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sortStrings(ks)
	return ks
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
