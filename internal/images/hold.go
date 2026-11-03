package images

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// A data service is held (runs another image than its release's own)
// exactly when the rule would refuse its release's own label and tag
// against its data's window: on the image the window ends on (holdLine),
// a pulled one by its registry digest, a built one by its own name or a
// bookmark. A hold is never carried from one activation to the next:
// wherever devopsy writes a release's ComposeFile or decides what a data
// service runs, it derives it again from the history (holdsFor, then
// applyHolds): a release and a rollback (Record, Restore), which are
// refused or accepted before they run and so hold nothing new; the
// restart after a failed run step and its last resort (Restore with
// Restart, LastHold); a full restore (Restore with NoCheck, against the
// snapshot's window it brings back); and every copy's take-over of the
// history, a sync's, an import's, a --no-code restore's, a reset's
// (Rederive, from SyncDone, ImportDone, Imported, Resolved, Wiped), whose
// ComposeFile restoreServices' recreate then follows. Why (review rounds 1
// and 2): carried holds vanished or went stale on the paths that forgot
// them, one path at a time.

// hold is a data service the rule refuses its release's own image on:
// the window's line whose image it runs instead, and why.
type hold struct {
	line ran
	why  string
}

// holdsFor are the services among own (each data service's release's own
// image, its version and tag the release's: ownOf) that the rule refuses
// against window(service): the window their data has, or will have once
// the activation asking runs (none for one whose history starts over:
// --accept-image, data of no version).
func holdsFor(own []entry, window func(string) []ran) map[string]hold {
	held := map[string]hold{}
	for _, o := range own {
		if o.Service == "" || o.DataVersion == "" {
			continue
		}
		if _, ok := held[o.Service]; ok {
			continue
		}
		w := window(o.Service)
		if why := rule(w, o.DataVersion, o.Tag, "its"); why != "" {
			held[o.Service] = hold{line: holdLine(w), why: why}
		}
	}
	return held
}

// holdLine is the window's newest line at its version: the image its
// data ran last (its last line, in any window devopsy writes, whose
// versions only rise), which the rule allows on it.
func holdLine(w []ran) ran {
	dv := windowVersion(w)
	var at ran
	for _, r := range w {
		if IsVersion(r.Version) && CompareVersions(r.Version, dv) == 0 {
			at = r
		}
	}
	return at
}

// windows is the window each data service's data has for rel, from the
// environment's history (base's): none for a service rel's ForcedFile
// names (--accept-image let its image through: its history starts over
// once it runs).
func windows(base, rel string) func(string) []ran {
	h, forced := readHistory(base), readForced(rel)
	return func(svc string) []ran {
		if forced[svc] != "" {
			return nil
		}
		return h.window(svc)
	}
}

// ownOf is the release's own image of e's service: e itself, or the own
// image a held entry records (entry.OwnRef), pulled when it has a digest.
func ownOf(e entry) entry {
	if !e.held() {
		return e
	}
	o := entry{Ref: e.OwnRef, ID: e.OwnID, Service: e.Service, Kind: KindOwn, Digest: e.OwnDigest, Build: e.OwnBuild, DataVersion: e.OwnDataVersion, Tag: e.OwnTag}
	if o.Digest != "" {
		o.Kind = KindPulled
	}
	return o
}

// setOwn records o, the release's own image, in e, held on another.
func setOwn(e *entry, o entry) {
	e.OwnRef, e.OwnID, e.OwnBuild, e.OwnDigest, e.OwnDataVersion, e.OwnTag = o.Ref, o.ID, o.Build, o.Digest, o.DataVersion, o.Tag
	if o.Kind == KindOwn {
		e.OwnDigest = ""
	}
}

// ownEntries are rel's listing's entries as its release's own images
// (ownOf), in order, and the services its compose gives now (List: only
// when a held entry or labels asks, none when it cannot be read). A held
// entry's own kind is compose's now (Service.own); with labels (a sync's
// check, whose run step runs under them: in the holder with the pending
// inherited variables) every data service's version and tag are the
// labels' now, else the listing's own (the labels' for a held entry that
// records none).
func (s *Set) ownEntries(rel string, labels bool) ([]entry, map[string]Service) {
	es := readList(rel)
	svcs := map[string]Service{}
	need := labels
	for _, e := range es {
		need = need || e.held()
	}
	if need && s.List != nil {
		if list, err := s.List(rel); err != nil {
			s.say(report.WarnLevel, "could not read the labels of %s (%v): its data services' versions are its listing's", filepath.Base(rel), err)
		} else {
			for _, svc := range list {
				svcs[svc.Name] = svc
			}
		}
	}
	mode := ModeOf(rel)
	own := make([]entry, len(es))
	for i, e := range es {
		o := ownOf(e)
		if svc, ok := svcs[e.Service]; ok && svc.Data && (e.DataVersion != "" || e.held()) {
			if e.held() {
				o.Kind = map[bool]string{true: KindOwn, false: KindPulled}[svc.own(mode)]
				o.Ref = cmp.Or(o.Ref, svc.Ref)
			}
			if labels || o.DataVersion == "" {
				o.DataVersion, o.Tag = svc.DataVersion, svc.tag()
			}
		}
		own[i] = o
	}
	return own, svcs
}

// historyHeld is HeldLabel's value for a hold on an image no listing of
// the environment names any more: its data's history alone says it ran.
const historyHeld = "history"

// heldWhat is what a HeldLabel's value says of the image, for messages.
func heldWhat(from string) string {
	switch snap, ok := strings.CutPrefix(from, snapshotHeld); {
	case ok:
		return "the image its data ran when snapshot " + snap + " was taken"
	case from == "" || from == historyHeld:
		return "the image its data's history ends on"
	}
	return "the image release " + from + " ran on its data"
}

// source is the image a hold's line l names as a listing of the
// environment lists it (its kind, registry digest and name): first the
// snapshot a full restore made rel from (its data came from there), then
// the release that ran it (newest first, never rel, nor an entry held
// there), a snapshot's, rel's own; without any, the line alone (with a
// tag pulled, by its digest in its repository, else built, by its id).
// from is what HeldLabel says of it: that release's id, "snapshot <id>",
// rel's HeldLabel when only rel lists it, else historyHeld.
func (s *Set) source(rel, service string, l ran) (entry, string) {
	find := func(dir string, heldToo bool) (entry, bool) {
		for _, e := range readList(dir) {
			if e.Service == service && e.ID != "" && e.ID == l.ID && (heldToo || !e.held()) {
				return e, true
			}
		}
		return entry{}, false
	}
	newest := func(pattern string) []string {
		dirs, _ := filepath.Glob(pattern)
		sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
		return dirs
	}
	snaps := s.snapshots()
	if snap := restoreOf(rel); snap != "" && snaps != "" {
		if e, ok := find(filepath.Join(snaps, snap), true); ok {
			return e, snapshotHeld + snap
		}
	}
	if l.ID != "" {
		for _, d := range newest(filepath.Join(filepath.Dir(rel), "*")) {
			if filepath.Clean(d) == filepath.Clean(rel) || strings.HasSuffix(d, ".tmp") {
				continue
			}
			if e, ok := find(d, false); ok {
				return e, filepath.Base(d)
			}
		}
		if snaps != "" {
			for _, d := range newest(filepath.Join(snaps, "*")) {
				if e, ok := find(d, false); ok && !strings.HasSuffix(d, ".tmp") {
					return e, snapshotHeld + filepath.Base(d)
				}
			}
		}
		if e, ok := find(rel, true); ok {
			return e, cmp.Or(heldIn(rel)[service], historyHeld)
		}
	}
	src := entry{Ref: l.Ref, ID: l.ID, Service: service, Kind: KindOwn}
	if l.Tag != "" {
		if d := digestIn(Image{Digests: l.Digests}, l.Ref); d != "" {
			src.Kind, src.Digest = KindPulled, d
		}
	}
	return src, historyHeld
}

// applyHolds holds, in es (rel's entries, each service's release's own
// image), every service of held on the image its line names (source,
// holdOn): the holds ComposeFile writes, and a line each saying so.
// pulled collects what it pulled by digest.
func (s *Set) applyHolds(rel string, es []entry, held map[string]hold, pulled *[]string) (map[string]composeHold, []string, error) {
	holds := map[string]composeHold{}
	if len(held) == 0 {
		return holds, nil, nil
	}
	tid := s.idOf(rel)
	own := s.own(rel)
	var lines []string
	for i, o := range es {
		h, ok := held[o.Service]
		if !ok {
			continue
		}
		src, from := s.source(rel, o.Service, h.line)
		e, ch, err := s.holdOn(o, h, src, from, own, pulled)
		if err != nil {
			return nil, nil, err
		}
		es[i] = e
		holds[o.Service] = ch
		lines = append(lines, fmt.Sprintf("%s stays on %s (%s, %s), as %s's would run it on data it does not allow (%s): a release that fixes forward is the way on", o.Service, heldWhat(from), shownEntry(e), shownData(e), tid, h.why))
	}
	return holds, lines, nil
}

// holdOn gets the service whose release's own image is o onto h's image,
// src (source), from what HeldLabel says: the entry it is listed as (h's
// line's version and tag, which Ran records, and o as its own, which
// every later derivation judges) and how ComposeFile holds it:
//
//   - pulled (a registry digest): by digest, pulled when gone;
//   - built, under o's name: that name put on the image (its id), through
//     others (a shared pull_policy: never name never moves another
//     environment's: held by the release's own bookmark instead), nothing
//     in the compose file but the mark;
//   - built under another name (o pulls it, or names it otherwise):
//     image: that name, put on the image, never a retag of o's pulled
//     one, the listing recording that image so the release's bookmark
//     keeps it.
//
// An image gone that cannot be had: ErrRefused, said; AsBuilt (the
// restart's last try) holds all the same, said, naming the release that
// fixes forward.
func (s *Set) holdOn(o entry, h hold, src entry, from string, own map[string]bool, pulled *[]string) (entry, composeHold, error) {
	ch := composeHold{From: cmp.Or(from, historyHeld)}
	what := heldWhat(from)
	svc := o.Service
	var e entry
	if src.Kind != KindOwn && src.Digest != "" {
		d := src.Digest
		img, err := s.Docker.Inspect(d)
		if err != nil {
			switch perr := s.Docker.Pull(d); {
			case perr != nil && !s.AsBuilt:
				s.sayAll(report.ErrorLevel, fmt.Sprintf("%s: %s, %s, is gone from this server and could not be pulled by its digest", svc, d, what),
					[]string{perr.Error()}, report.Fix("the image its data ran must be there: never one its data does not allow"))
				return entry{}, ch, ErrRefused
			case perr != nil:
				s.say(report.WarnLevel, "%s: %s is gone from this server and could not be pulled by its digest (%v): held on it anyway, for the run step to pull", svc, d, perr)
			default:
				if pulled != nil {
					*pulled = append(*pulled, d)
				}
				img, _ = s.Docker.Inspect(d)
			}
		}
		e = entry{Ref: o.Ref, ID: cmp.Or(img.ID, src.ID, h.line.ID), Service: svc, Kind: KindPulled, Digest: d, Version: cmp.Or(sanitize(img.Version), src.Version)}
		ch.Image = d
	} else {
		name := o.Ref
		if src.Ref != "" && familiar(src.Ref) != familiar(o.Ref) {
			name, ch.Image = src.Ref, src.Ref
		}
		id := cmp.Or(src.ID, h.line.ID)
		if cur, err := s.Docker.ID(name); err != nil || cur != id {
			if _, gerr := s.Docker.ID(id); id == "" || gerr != nil {
				if !s.AsBuilt {
					s.say(report.ErrorLevel, "%s: %s, built by the project, is gone from this server", svc, what)
					return entry{}, ch, ErrRefused
				}
				s.sayAll(report.WarnLevel, fmt.Sprintf("%s: %s, built by the project, is gone from this server: it runs what %s holds", svc, what, name),
					nil, report.Fix("fix forward: a release, which never takes the data back", s.cmd("--release")))
			} else if who := s.others(cur, own); err == nil && len(who) > 0 {
				// A name another environment runs never moves: held by the
				// release's own bookmark of the image instead (writeHeld names
				// it).
				s.say(report.InfoLevel, "%s stays where it is, which other environments use (%s): %s is held on the image by its bookmark", name, strings.Join(who, ", "), svc)
				ch.Image, ch.bookmark = "", true
			} else if err := s.Docker.Tag(id, name); err != nil {
				s.say(report.ErrorLevel, "could not put %s on the image its data ran: %v", name, err)
				return entry{}, ch, err
			}
		}
		e = entry{Ref: name, ID: id, Service: svc, Kind: KindOwn, Version: src.Version}
		if ch.bookmark {
			e.Ref = o.Ref
		}
	}
	e.DataVersion, e.Tag = h.line.Version, h.line.Tag
	setOwn(&e, o)
	return e, ch, nil
}

// resolveBookmarks names, in holds, the bookmark each hold by bookmark
// (and each own image by bookmark) is, as rel's listing es gives it.
func (s *Set) resolveBookmarks(rel string, es []entry, holds map[string]composeHold) {
	for i, b := range bookmarks(es, s.Name, s.idOf(rel)) {
		if ch, ok := holds[es[i].Service]; ok && ch.bookmark {
			ch.Image = b
			holds[es[i].Service] = ch
		}
	}
}

// writeHeld records rel's entries (keep: the listing and its bookmarks)
// and its ComposeFile with holds.
func (s *Set) writeHeld(rel string, es []entry, holds map[string]composeHold) error {
	tid := s.idOf(rel)
	s.resolveBookmarks(rel, es, holds)
	if err := s.keep(rel, es); err != nil {
		s.say(report.ErrorLevel, "could not record the images of %s: %v", tid, err)
		return err
	}
	if err := writeCompose(rel, es, holds); err != nil {
		s.say(report.ErrorLevel, "could not write %s's %s: %v", tid, ComposeFile, err)
		return err
	}
	return nil
}

// Rederive derives rel's holds again (holdsFor) from its data's history
// as it is now, once a copy took the history over or started it over (a
// sync, an import, a --no-code or full restore's data step, a reset, an
// import of no record, --resolved): a service the rule allows its
// release's own image on runs that again (unhold), one it refuses is held
// on the image the window ends on (applyHolds), whatever it ran before.
// Under the image lock; quiet when nothing changes. A built own image
// gone, which only a build gives back: ErrRefused, said (the checks
// before a copy refuse it first: copyNeeds).
func (s *Set) Rederive(base, rel string) error {
	es := readList(rel)
	if len(es) == 0 {
		return nil
	}
	tid := s.idOf(rel)
	own, svcs := s.ownEntries(rel, false)
	held := holdsFor(own, windows(base, rel))
	imgs, _, wasHeld := composeFacts(rel)
	before := append([]entry{}, es...)
	mode := ModeOf(rel)
	ownNames := s.own(rel)
	var lines []string
	kept := map[string]composeHold{}
	for i, e := range es {
		if _, ok := held[e.Service]; ok {
			es[i] = own[i]
			continue
		}
		if !e.held() {
			// Its own image by bookmark, another environment running its name.
			if img := imgs[e.Service]; img != "" && wasHeld[e.Service] == "" {
				kept[e.Service] = composeHold{Image: img}
			}
			continue
		}
		svc, ok := svcs[e.Service]
		if !ok {
			s.say(report.ErrorLevel, "%s: %s's labels could not be read: it stays on the image it is held on until its labels read", e.Service, tid)
			return ErrRefused
		}
		ne, ch, err := s.unhold(rel, e, own[i], svc, mode, ownNames)
		if err != nil {
			return err
		}
		es[i] = ne
		if ch.bookmark {
			kept[e.Service] = ch
		}
		lines = append(lines, fmt.Sprintf("%s runs %s's own image again (%s, %s), not %s (%s): its data's version allows it", e.Service, tid, shownEntry(ne), shownData(ne), heldWhat(wasHeld[e.Service]), shownData(e)))
	}
	var pulled []string
	holds, heldLines, err := s.applyHolds(rel, es, held, &pulled)
	if err != nil {
		for _, p := range pulled {
			_ = s.Docker.Remove(p)
		}
		return err
	}
	for svc, ch := range kept {
		holds[svc] = ch
	}
	s.resolveBookmarks(rel, es, holds)
	if was, err := os.ReadFile(filepath.Join(rel, ComposeFile)); err == nil && slices.Equal(before, es) && string(was) == composeText(es, holds) {
		return nil
	}
	if err := s.writeHeld(rel, es, holds); err != nil {
		return err
	}
	if len(heldLines) > 0 {
		s.sayAll(report.WarnLevel, "held where its data does not allow "+tid+"'s own:", heldLines)
	}
	if len(lines) > 0 {
		s.sayAll(report.InfoLevel, "held no more, its data allowing "+tid+"'s own:", lines)
	}
	return nil
}

// unhold is the entry of a service held on another image (e) that runs
// its release's own (o, svc as compose gives it now) again: a pulled name
// as it holds now, pulled when gone; a built one its own build put back
// on its name (o's id), by its bookmark when another environment runs
// the name's image (a composeHold by bookmark, not a hold); gone:
// ErrRefused, said.
func (s *Set) unhold(rel string, e, o entry, svc Service, mode string, own map[string]bool) (entry, composeHold, error) {
	tid := s.idOf(rel)
	var ch composeHold
	if !svc.own(mode) {
		if _, err := s.Docker.ID(svc.Ref); errors.Is(err, ErrNoImage) {
			s.say(report.InfoLevel, "%s: %s is gone from this server: pulling it", svc.Name, svc.Ref)
			if err := s.Docker.Pull(svc.Ref); err != nil {
				s.say(report.ErrorLevel, "%s: could not pull %s: %v", svc.Name, svc.Ref, err)
				return entry{}, ch, err
			}
		}
		ne := s.entryOf(svc, svc.Ref, mode)
		ne.DataVersion, ne.Tag = o.DataVersion, o.Tag
		return ne, ch, nil
	}
	if _, err := s.Docker.ID(o.ID); o.ID == "" || err != nil {
		s.sayAll(report.ErrorLevel, fmt.Sprintf("%s: %s's own image of it, built by the project, is gone from this server, and the image it is held on (%s) would upgrade its data again", svc.Name, tid, shownData(e)),
			nil, report.Fix("a release builds it:", s.cmd("--release")))
		return entry{}, ch, ErrRefused
	}
	if cur, err := s.Docker.ID(svc.Ref); err != nil || cur != o.ID {
		if who := s.others(cur, own); err == nil && len(who) > 0 {
			s.say(report.InfoLevel, "%s stays where it is, which other environments use (%s): %s runs its own image by its bookmark", svc.Ref, strings.Join(who, ", "), svc.Name)
			ch.bookmark = true
		} else if err := s.Docker.Tag(o.ID, svc.Ref); err != nil {
			s.say(report.ErrorLevel, "could not put %s back on %s's own image: %v", svc.Ref, tid, err)
			return entry{}, ch, err
		}
	}
	return entry{Ref: svc.Ref, ID: o.ID, Service: svc.Name, Kind: KindOwn, Build: o.Build, DataVersion: o.DataVersion, Tag: o.Tag}, ch, nil
}

// LastHold is the restart's last resort, once get_images failed (the
// image lock's wait, a docker tag, a bookmark gone, compose's config
// unreadable): the holds derived as everywhere (holdsFor, from target's
// listing and its data's history), written into target's listing and
// ComposeFile without the image lock and without Docker, where a file can
// write them: a pulled image held by its registry digest, which the run
// step's up pulls when gone, the image it replaces in the listing
// recorded (LeftFile) for the next pruning; a pulled own image back by
// its own digest. A built one, held or given back, needs a name moved,
// which only Docker does: said, naming the service and the way on, a
// release that fixes forward. Why (review): the restart went on with
// target's own ComposeFile, its older image on the data the newer one ran.
func (s *Set) LastHold(target, against string) error {
	tid := filepath.Base(target)
	es := readList(target)
	own, _ := s.ownEntries(target, false)
	held := holdsFor(own, windows(baseOf(target), target))
	imgs, _, wasHeld := composeFacts(target)
	// What the file holds now stays but for what this changes: a hold by
	// its own name has no image:, only its mark.
	holds := map[string]composeHold{}
	for svc, img := range imgs {
		holds[svc] = composeHold{Image: img, From: wasHeld[svc]}
	}
	for svc, from := range wasHeld {
		holds[svc] = composeHold{Image: imgs[svc], From: from}
	}
	var lines, unheld []string
	for i, e := range es {
		o := own[i]
		h, ok := held[e.Service]
		switch {
		case ok:
			if e.held() && e.ID == h.line.ID && wasHeld[e.Service] != "" {
				continue // held on that image already
			}
			src, from := s.source(target, e.Service, h.line)
			if src.Kind == KindOwn || src.Digest == "" {
				unheld = append(unheld, fmt.Sprintf("%s: %s's image would run it on data it does not allow (%s), and %s, built by the project, is held only by moving a name, which needs the images: it runs what %s holds", e.Service, tid, h.why, heldWhat(from), e.Ref))
				continue
			}
			// The image it replaces in the listing keeps its names, which no
			// listing gives it now: recorded, as keep records them, or they
			// would outlive every release (no Docker here: the next pruning
			// retries them).
			if e.ID != "" && e.ID != src.ID && !e.held() {
				s.record(digestRef(e.Ref), e.ID, tid, "no release lists it any more")
			}
			ne := entry{Ref: o.Ref, ID: cmp.Or(src.ID, h.line.ID), Service: e.Service, Kind: KindPulled, Digest: src.Digest, Version: src.Version, DataVersion: h.line.Version, Tag: h.line.Tag}
			setOwn(&ne, o)
			es[i] = ne
			holds[e.Service] = composeHold{Image: src.Digest, From: cmp.Or(from, historyHeld)}
			lines = append(lines, fmt.Sprintf("%s stays on %s (%s, %s), as %s's would run it on data it does not allow (%s): a release that fixes forward is the way on", e.Service, heldWhat(from), shownEntry(ne), shownData(ne), tid, h.why))
		case e.held():
			if o.Kind == KindOwn || o.Digest == "" {
				unheld = append(unheld, fmt.Sprintf("%s: its data allows %s's own image again, which goes back only by moving a name, which needs the images: it stays on %s", e.Service, tid, heldWhat(wasHeld[e.Service])))
				continue
			}
			es[i] = o
			delete(holds, e.Service)
			lines = append(lines, fmt.Sprintf("%s runs %s's own image again (%s, %s): its data's version allows it", e.Service, tid, shownEntry(o), shownData(o)))
		}
	}
	if len(lines) > 0 {
		if err := writeList(target, es); err != nil {
			s.say(report.ErrorLevel, "could not record the images of %s: %v", tid, err)
			return err
		}
		if err := writeCompose(target, es, holds); err != nil {
			s.say(report.ErrorLevel, "could not write %s's %s: %v", tid, ComposeFile, err)
			return err
		}
		s.sayAll(report.WarnLevel, "images of "+tid+" not restored, but held where its data does not allow its own:", lines)
	}
	if len(unheld) > 0 {
		s.sayAll(report.ErrorLevel, "images of "+tid+" not restored, and its data runs an image it does not allow:", unheld,
			report.Fix("fix forward: a release, which never takes the data back", s.cmd("--release")))
		return ErrRefused
	}
	return nil
}

// imagePresent says whether Docker has the image id (a built one's: what
// a copy's check needs before it unholds or holds on it).
func (s *Set) imagePresent(id string) bool {
	if id == "" {
		return false
	}
	_, err := s.Docker.ID(id)
	return err == nil
}
