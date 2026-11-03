package images

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// A Landing is data arriving on an environment from elsewhere. Every path
// that brings some goes through the same two steps, whatever it brings:
// CheckLanding before anything stops, Land once the data is in. Each path
// only says what lands (its constructor):
//
//   - a sync (SyncLanding): the parent's windows;
//   - a reset's wipe (ResetLanding): every data service's history over;
//     the sync's own landing takes over what its copy wrote;
//   - an import (ImportLanding): its export's window, else data of no
//     known version;
//   - --resolved (ResolvedLanding): an import's, accepted as it is;
//   - a restore's data step, full or --no-code (RestoreLanding): a
//     snapshot's windows.
//
// What a service is, and the version and tag it claims, are its labels'
// now (labelsOf), on every path: in a sync's holder with the pending
// inherited variables, in a restore --with-env with the snapshot's. Its
// release's listing takes them as the data lands, so what runs on the
// data from then on (Ran) and every later derivation say the same. A name
// its release did not fetch is refused first (notFetched): the listing's
// image is what runs.
//
// A failure of Land has one policy, its callers': the error says what
// failed, the environment's mark stays and what the copy stopped stays
// down, until a copy lands or --resolved accepts it. Nothing is retried
// or warned past: data that landed halfway must not start.
//
// A release that syncs judges its new release's labels before its fetch
// (Check, Record, by SyncLanding's windows), and a full restore derives
// its holds before its data step (Restore, by RestoreLanding's); both then
// land as any copy does. The restart and its last resort land nothing:
// they read the labels the same way (ownEntries) and derive against the
// data's own window. Why (review rounds 3 and 4): each path read the
// labels or the listing, ordered its steps and chose what a failure meant
// in its own way, and every finding there was one of them differing.
type Landing struct {
	// windows are the source's data by service (ParentData's lines).
	windows map[string]*sourceData
	// over are services landing with no window of their own, data of no
	// known version (none recorded); all: every data service, the data
	// gone (a reset's wipe, and its sync's check of what that derives).
	over []string
	all  bool
	// first is how a window taken over reads, on its first line.
	first string
	// running: the source is the environment's own past (a snapshot), so
	// the image a service is held on counts as well as its release's own
	// (copyRule).
	running bool
	// since keeps a window that started with an import or an accepted
	// image since then (--resolved): only what came after it failed.
	since time.Time
	// asIs: the operator accepts the data as it is (--resolved): a hold
	// whose release's own image is gone stays, said, rather than leave no
	// way out of the mark (derive).
	asIs bool
	// words are what its check says.
	words checkWords
}

// window is what service's data has once this lands, and whether this
// lands any of it.
func (l Landing) window(service string) ([]ran, bool) {
	if p := l.windows[service]; p != nil {
		return p.window(), true
	}
	return nil, l.all || slices.Contains(l.over, service)
}

// after is the window each service's data has once this lands, the ones
// it does not write keeping now's.
func (l Landing) after(now func(string) []ran) func(string) []ran {
	return func(service string) []ran {
		if w, ok := l.window(service); ok {
			return w
		}
		return now(service)
	}
}

// SyncLanding is the parent's data (its ParentData, from its address),
// every data service's history starting over under a reset (Reset).
func (s *Set) SyncLanding(data, from string) Landing {
	return Landing{windows: parseParentData(data), all: s.Reset, first: SyncedFrom + from, words: checkWords{
		source: "the parent's", what: "the child would run the parent's data on an image its version does not allow", nothing: "nothing changed", flag: "--sync",
		builds: "a sync builds nothing", rebuild: report.Fix("release it with the sync, which builds its images:", s.cmd(ReleaseSync(s.Again))),
		fetch: report.Fix("release it with the sync, which fetches them:", s.cmd(ReleaseSync(s.Again)))}}
}

// ReleaseSync is the release that syncs a refused sync's fix names, from
// the source again (what the operator ran) names.
func ReleaseSync(again string) string {
	if _, from, ok := strings.Cut(again, " --from "); ok {
		return "--release-sync --from " + from
	}
	return "--release-sync"
}

// ResetLanding is a reset's wipe: the data every history described is
// gone, the ones the copy writes nothing for included (a
// devopsy.data.copy=none service, one whose entries are all missing on the
// parent), so the next release adopts their labels (review: a reset kept
// their windows, and a later lower label was refused for data that no
// longer existed). The sync's landing takes over what its copy writes
// once it is complete.
func (s *Set) ResetLanding() Landing {
	return Landing{all: true, first: Imported}
}

// ImportLanding is an import of service from an export's data (ExportData:
// data carries its window), its first line timed at the import, which
// --resolved keeps a history by; without a record of it (a tar made by
// hand) the data is of no known version. A custom import is no landing:
// loaded through the image that runs, it leaves the history and the hold
// as they were.
func (s *Set) ImportLanding(data, service string) Landing {
	l := Landing{windows: parseParentData(DataFor(data, service)), first: Imported, words: checkWords{
		source: "the export's", what: "this environment would run the export's data on an image its version does not allow", nothing: "nothing imported", flag: "--import " + service, input: " < <export>",
		builds: "an import builds nothing", rebuild: report.Fix("release it first, which builds its images, then import again:", s.cmd("--release")),
		fetch: report.Fix("release it first, which fetches them, then import again:", s.cmd("--release"))}}
	if len(l.windows) == 0 {
		l.over = []string{service}
	}
	return l
}

// ResolvedLanding is --resolved of an import of service whose mark is
// since: the data accepted as it is. Its history stays when its window
// starts with an import line since (the import took the export's window,
// and only what came after failed: the data is complete, and starting over
// would forget its version) or a forced one (the import was accepted: a
// line of no version would undo that, letting the next release adopt any
// label: review), else it starts over. A hold it would end stays when its
// release's own build is gone: refused, nothing would have a way out, as a
// marked environment refuses a release, the only thing that builds it, and
// an import refuses what it cannot derive (review). The held image runs on
// the accepted data, said, and a release builds the release's own.
func (s *Set) ResolvedLanding(service string, since time.Time) Landing {
	return Landing{over: []string{service}, first: Imported, since: since, asIs: true}
}

// RestoreLanding is a restore's data step: snapshot's windows (its
// SnapshotData), the environment's own past, so a service whose release's
// own image they refuse passes when the image it runs now (held) is
// allowed, held after on the image the window ends on, which the snapshot
// bookmarks. services are the ones the snapshot writes (its manifest's):
// one it has no window of is data of no known version. A --no-code
// restore builds nothing: a built image the data step needs, gone, refuses
// it, naming the full restore, which builds.
func (s *Set) RestoreLanding(data, snapshot string, services []string) Landing {
	l := Landing{windows: parseParentData(data), first: Imported, running: true, words: checkWords{
		source: "the snapshot's", what: "the current release would run the snapshot's data on an image its version does not allow", nothing: "nothing restored", flag: "--snapshots restore " + snapshot + " --no-code",
		builds: "a --no-code restore builds nothing", rebuild: report.Fix("restore it with its code, which builds its images:", s.cmd("--snapshots restore "+snapshot)),
		fetch: report.Fix("release it first, which fetches them, then restore again:", s.cmd("--release"))}}
	for _, svc := range services {
		if l.windows[svc] == nil {
			l.over = append(l.over, svc)
		}
	}
	return l
}

// labelsOf are rel's services as their labels give them now, by name.
func (s *Set) labelsOf(rel string) (map[string]Service, error) {
	list, err := s.List(rel)
	if err != nil {
		return nil, err
	}
	svcs := make(map[string]Service, len(list))
	for _, svc := range list {
		svcs[svc.Name] = svc
	}
	return svcs, nil
}

// claim is what svc says of its data: its version and tag, none without
// data.
func (svc Service) claim() (version, tag string) {
	if !svc.Data {
		return "", ""
	}
	return svc.DataVersion, svc.tag()
}

// withClaims is es with each data service's version and tag as svcs give
// them, a held one's as its own (entry.OwnDataVersion).
func withClaims(es []entry, svcs map[string]Service) []entry {
	out := append([]entry{}, es...)
	for i, e := range es {
		svc, ok := svcs[e.Service]
		if !ok {
			continue
		}
		if e.held() {
			out[i].OwnDataVersion, out[i].OwnTag = svc.claim()
		} else {
			out[i].DataVersion, out[i].Tag = svc.claim()
		}
	}
	return out
}

// ownEntries are es (rel's listing) as their release's own images
// (ownOf), each one's version and tag as svcs give them; without svcs (the
// restart's last resort, compose's config unreadable), the listing's own.
func ownEntries(rel string, es []entry, svcs map[string]Service) []entry {
	mode := ModeOf(rel)
	own := make([]entry, len(es))
	for i, e := range es {
		o := ownOf(e)
		if svc, ok := svcs[e.Service]; ok {
			if e.held() {
				o.Kind = map[bool]string{true: KindOwn, false: KindPulled}[svc.own(mode)]
				o.Ref = cmp.Or(o.Ref, svc.Ref)
			}
			o.DataVersion, o.Tag = svc.claim()
		}
		own[i] = o
	}
	return own
}

// CheckLanding checks, before anything stops, that l can land on rel (the
// current release): every data service it writes is judged by the rule
// against the window it brings, its version and tag its labels' now, and
// what the holds then derived need is there (copyNeeds). Refused:
// ErrRefused, said, nothing changed; --accept-image lets a refused one
// through, its history starting over (Land).
func (s *Set) CheckLanding(rel string, l Landing) error {
	w := l.words
	svcs, err := s.labelsOf(rel)
	if err != nil {
		s.say(report.ErrorLevel, "the labels of %s could not be read (%v): %s", filepath.Base(rel), err, w.nothing)
		return ErrRefused
	}
	es := readList(rel)
	own := ownEntries(rel, es, svcs)
	if err := s.notFetched(es, own, svcs, l); err != nil {
		return err
	}
	var js []judged
	for i, e := range es {
		win, lands := l.window(e.Service)
		if !lands || own[i].DataVersion == "" {
			continue
		}
		js = append(js, judged{service: e.Service, why: copyRule(win, e, own[i], w.source, l.running)})
	}
	forced, err := s.judge(js, w, rel, false)
	if err != nil {
		return err
	}
	for _, svc := range forced {
		s.say(report.WarnLevel, "%s: accepted (--accept-image): it runs its own image on the %s data, its history starting over from it", svc, strings.TrimPrefix(strings.TrimSuffix(w.source, "'s"), "the "))
	}
	return s.copyNeeds(rel, es, own, svcs, l, forced)
}

// notFetched refuses a landing into a data service whose labels name an
// image its release did not fetch (a variable set since, a profile turned
// on): the rule would judge what its labels say, and the image it listed
// would run. Only a release fetches it (w.fetch), as a reload and a
// sync's or a --no-code restore's run step refuse (ReloadCheck).
func (s *Set) notFetched(es, own []entry, svcs map[string]Service, l Landing) error {
	recorded := map[string]entry{}
	for i, e := range es {
		if e.Service != "" {
			recorded[e.Service] = own[i]
		}
	}
	names := make([]string, 0, len(svcs))
	for name := range svcs {
		names = append(names, name)
	}
	sort.Strings(names)
	var lines []string
	for _, name := range names {
		svc := svcs[name]
		if _, lands := l.window(name); !lands || !svc.Data {
			continue
		}
		switch e, ok := recorded[name]; {
		case !ok:
			lines = append(lines, name+" is not one of the services the release fetched images for")
		case familiar(e.Ref) != familiar(svc.Ref):
			lines = append(lines, fmt.Sprintf("%s's image is %s now, the release fetched %s", name, svc.Ref, e.Ref))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	s.sayAll(report.ErrorLevel, "the current release did not fetch what its data services' labels name: "+l.words.nothing, lines, l.words.fetch)
	return ErrRefused
}

// copyRule is the rule a landing runs on a data service whose entry is e
// and its release's own image o, against the window w it brings: by o, or
// with running (a snapshot) by e, the image it runs now, when o is refused
// and e is held. Land forces what it refuses, accepted.
func copyRule(w []ran, e, o entry, source string, running bool) string {
	why := rule(w, o.DataVersion, o.Tag, source)
	if why != "" && running && e.held() && rule(w, e.DataVersion, e.Tag, source) == "" {
		return ""
	}
	return why
}

// copyNeeds refuses, before a copy stops anything, one whose landing would
// derive (holdsFor, against the windows it leaves, none for an accepted
// one) a built image Docker no longer has: the release's own of a held
// service the copy lets run it again, or the one a hold would be on. A
// copy builds nothing (w.builds), w.rebuild does.
func (s *Set) copyNeeds(rel string, es, own []entry, svcs map[string]Service, l Landing, forced []string) error {
	w := l.words
	after := l.after(windows(baseOf(rel), rel))
	held := holdsFor(own, func(svc string) []ran {
		if slices.Contains(forced, svc) {
			return nil
		}
		return after(svc)
	})
	mode := ModeOf(rel)
	source := strings.TrimPrefix(strings.TrimSuffix(w.source, "'s"), "the ")
	var gone []string
	for i, e := range es {
		o := own[i]
		if h, ok := held[e.Service]; ok {
			src, from := s.source(rel, e.Service, h.line)
			if (src.Kind == KindOwn || src.Digest == "") && !s.imagePresent(cmp.Or(src.ID, h.line.ID)) {
				gone = append(gone, e.Service+": "+heldWhat(from)+", built by the project, is gone from this server, and its data allows the release's own on it no more")
			}
			continue
		}
		if !e.held() {
			continue
		}
		svc, listed := svcs[e.Service]
		if (listed && svc.own(mode) || !listed && o.Kind == KindOwn) && !s.imagePresent(o.ID) {
			gone = append(gone, e.Service+": the current release's own image of it, built by the project, is gone from this server, and the image it is held on ("+shownData(e)+") would upgrade the "+source+" data again")
		}
	}
	if len(gone) == 0 {
		return nil
	}
	s.sayAll(report.ErrorLevel, w.builds+": "+w.nothing, gone, w.rebuild)
	return ErrRefused
}

// Land takes l's data over on rel (the release that runs on it), in one
// order, once the data is in and before anything starts on it:
//
//  1. the labels, which its listing takes (withClaims);
//  2. each data service l writes gets its window in place of its history:
//     the source's, its first line l.first; of no known version, one such
//     line (the next release adopts its label); refused and let through by
//     --accept-image, one Forced line of its own image, from which its
//     history starts over;
//  3. its holds derived again (derive), under the image lock.
//
// Its error says what failed, in words that follow "...completed, but":
// the callers keep the mark and what they stopped down (Landing).
func (s *Set) Land(base, rel string, l Landing) error {
	svcs, err := s.labelsOf(rel)
	if err != nil {
		return fmt.Errorf("its labels could not be read: %w", err)
	}
	listed := readList(rel)
	es := withClaims(listed, svcs)
	if !slices.Equal(listed, es) {
		if err := writeList(rel, es); err != nil {
			return fmt.Errorf("its version could not be recorded in the release's listing: %w", err)
		}
	}
	own := ownEntries(rel, es, svcs)
	h := readHistory(base)
	changed := false
	for i, e := range es {
		w, lands := l.window(e.Service)
		o := own[i]
		if !lands || o.DataVersion == "" || l.kept(h, e.Service) {
			continue
		}
		var lines history
		if s.Accept[e.Service] && copyRule(w, e, o, l.words.source, l.running) != "" {
			// Its own image on the copy, accepted: its history over from it,
			// the release's own (as Record's ForcedFile names it), never the
			// container's, which in a release that syncs runs the release
			// before's until the run step (review: a run step failing before
			// the database ran restarted that one on it).
			img := Image{ID: o.ID}
			if i, err := s.Docker.Inspect(o.ID); err == nil {
				img = i
			}
			lines = history{ranOn(o, Forced, img, s.now())}
		} else {
			lines = append(lines, w...)
			if len(lines) == 0 {
				lines = history{{Service: e.Service, Ref: o.Ref, Time: s.now().Unix()}}
			}
			lines[0].How = l.first
			if l.first == Imported {
				lines[0].Time = s.now().Unix()
			}
		}
		h = append(h.without(e.Service), lines...)
		changed = true
	}
	if changed {
		if err := h.write(base); err != nil {
			return fmt.Errorf("its history could not be written: %w", err)
		}
	}
	if err := s.derive(base, rel, svcs, l.asIs); err != nil {
		return fmt.Errorf("what runs on it could not be derived from its history: %w", err)
	}
	return nil
}

// kept says whether service's history stays as it is: its window started
// with an import or an accepted image since l.since (ResolvedLanding).
func (l Landing) kept(h history, service string) bool {
	if l.since.IsZero() {
		return false
	}
	w := h.window(service)
	return len(w) > 0 && (w[0].How == Imported || w[0].How == Forced) && w[0].Time >= l.since.Unix()
}
