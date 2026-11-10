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
// a pulled one by its registry digest, a built one by the holding
// release's bookmark of it. A hold is never carried from one activation to the next:
// wherever devopsy writes a release's ComposeFile or decides what a data
// service runs, it derives it again from the history (holdsFor, then
// applyHolds): a release and a rollback (Record, Restore), which are
// refused or accepted before they run and so hold nothing new; the
// restart after a failed run step and its last resort (Restore with
// Restart, LastHold); a full restore (Restore with NoCheck, against the
// snapshot's window it brings back); and every copy that lands (derive,
// from Land), whose ComposeFile restoreServices' recreate then follows. Why (review rounds 1
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
		// A failed release last: one that failed its fetch lists an image
		// it never ran, which the words would say it did.
		dirs := newest(filepath.Join(filepath.Dir(rel), "*"))
		slices.SortStableFunc(dirs, func(a, b string) int {
			return cmp.Compare(failedRank(a), failedRank(b))
		})
		for _, d := range dirs {
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
	// With a tag pulled; in image mode a build: service's too, which has no
	// tag and which the registry serves (its digests are no local build's).
	// A service that pulls holds it in its own repository, whichever the
	// line names (a mirror has the digest under the name it reaches); one
	// the project builds has no repository a registry serves, so in the
	// line's (Land keeps only what is safe of a line from an export's
	// header: review).
	if l.Tag != "" || ModeOf(rel) == ModeImage {
		if d := DigestIn(Image{Digests: l.Digests}, l.Ref); d != "" {
			src.Kind, src.Digest = KindPulled, d
			if e, ok := match(readList(rel), entry{Service: service}); ok && ownOf(e).Kind != KindOwn {
				src.Digest = RepoOf(ownOf(e).Ref) + "@" + digestOf(d)
			}
		}
	}
	return src, historyHeld
}

// here says whether Docker has src's image.
func (s *Set) here(src entry) bool {
	if src.Kind != KindOwn && src.Digest != "" {
		_, err := s.Docker.Inspect(src.Digest)
		return err == nil
	}
	return s.imagePresent(src.ID)
}

// allowedHere finds, for the restart's last try, an image a release of the
// environment lists on this server that the rule allows on data whose
// window ends at line (a one-line window reads the same): what a hold on an
// image this server does not have (the parent's build, its data landed by
// the failed release's sync) can run instead of the release's older own,
// newest release first, the failed one's own usually (review).
func (s *Set) allowedHere(rel, service string, line ran) (entry, string, bool) {
	dirs, _ := filepath.Glob(filepath.Join(filepath.Dir(rel), "*"))
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		if filepath.Clean(d) == filepath.Clean(rel) || strings.HasSuffix(d, ".tmp") {
			continue
		}
		for _, e := range readList(d) {
			// Never one that failed its fetch: its listing names what the
			// names held then, under labels they never had (FetchFailedFile).
			if _, err := os.Stat(filepath.Join(d, FetchFailedFile)); err == nil {
				break
			}
			if e.Service == service && !e.held() && e.DataVersion != "" && e.DataVersion != AnyVersion && s.here(e) && rule([]ran{line}, e.DataVersion, e.Tag, "its") == "" {
				return e, filepath.Base(d), true
			}
		}
	}
	return entry{}, "", false
}

// failedRank orders a failed release after any other.
func failedRank(rel string) int {
	if _, err := os.Stat(filepath.Join(rel, failedFile)); err == nil {
		return 1
	}
	return 0
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
	var lines []string
	for i, o := range es {
		h, ok := held[o.Service]
		if !ok {
			continue
		}
		src, from := s.source(rel, o.Service, h.line)
		if s.AsBuilt && !s.here(src) {
			if alt, altFrom, ok := s.allowedHere(rel, o.Service, h.line); ok {
				src, from = alt, altFrom
				h.line = ran{Service: o.Service, Ref: alt.Ref, ID: alt.ID, Version: alt.DataVersion, Tag: alt.Tag}
			}
		}
		e, ch, err := s.holdOn(o, h, src, from, pulled)
		if err != nil {
			return nil, nil, err
		}
		es[i] = e
		if !e.held() && !ch.bookmark {
			continue // said (ownInstead)
		}
		holds[o.Service] = ch
		if !e.held() {
			continue
		}
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
//   - built: by the release's own bookmark of the held entry, which only
//     the release writes (resolveBookmarks), never a name another
//     environment's build or pull moves, nor another release's bookmark,
//     which goes with that release.
//
// An image gone that cannot be had: ErrRefused, said; AsBuilt (the
// restart's last try): a pulled one held all the same, for the run step to
// pull, a built one its release's own instead (ownInstead), said, naming
// the release that fixes forward.
func (s *Set) holdOn(o entry, h hold, src entry, from string, pulled *[]string) (entry, composeHold, error) {
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
			case perr != nil && h.line.Tag == "":
				// A build: service's line, in image mode: the digest may be a
				// parent's local build, which no registry serves.
				return s.ownInstead(o, h, what)
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
		id := cmp.Or(src.ID, h.line.ID)
		if !s.imagePresent(id) {
			if !s.AsBuilt {
				s.say(report.ErrorLevel, "%s: %s, built by the project, is gone from this server", svc, what)
				return entry{}, ch, ErrRefused
			}
			return s.ownInstead(o, h, what)
		}
		// By the release's own bookmark of it (writeHeld names it), in the
		// repository it was built under (o pulls it, or names it otherwise).
		name := o.Ref
		if src.Ref != "" && familiar(src.Ref) != familiar(o.Ref) {
			name = src.Ref
		}
		ch.bookmark = true
		e = entry{Ref: name, ID: id, Service: svc, Kind: KindOwn, Version: src.Version}
	}
	e.DataVersion, e.Tag = h.line.Version, h.line.Tag
	setOwn(&e, o)
	return e, ch, nil
}

// ownInstead is holdOn's last try (AsBuilt) when the image cannot be had
// (built on the parent's server, pruned): nothing here is held, its
// release's own image runs, by its bookmark as any own image's, said, with
// the release that fixes forward. Never the hold's own name in its compose
// file, which nothing here holds (review: the run step's up could not start
// it, and the release's own bookmark went).
func (s *Set) ownInstead(o entry, h hold, what string) (entry, composeHold, error) {
	var ch composeHold
	s.sayAll(report.WarnLevel, fmt.Sprintf("%s: %s, built by the project, is gone from this server: its release's own image runs on data it does not allow (%s)", o.Service, what, h.why),
		nil, report.Fix("fix forward: a release, which never takes the data back", s.cmd("--release")))
	return o, ch, nil
}

// resolveBookmarks names, in holds, the bookmark each own image runs by,
// held or not, as rel's listing es gives it: the release's own name for
// its image, which only it writes, never the project's, which every
// environment building or pulling under it moves for the others (stage, a
// branch's, a pull request's), nor the id, which a build cannot tag. A
// hold on another reference (a digest) stays as it is. A service of no
// entry sharing an own image (sharers: one under a profile) runs that
// image's bookmark, its release's own build's while the entry is held.
func (s *Set) resolveBookmarks(rel string, es []entry, holds map[string]composeHold) {
	names := bookmarks(es, s.Name, s.idOf(rel))
	byRef := map[string]string{}
	listed := map[string]bool{}
	for i, b := range names {
		e := es[i]
		if e.Service == "" {
			continue
		}
		listed[e.Service] = true
		ch, ok := holds[e.Service]
		if ok && !ch.bookmark || !ok && (e.Kind != KindOwn || e.ID == "") {
			continue
		}
		ch.Image, ch.bookmark = b, true
		holds[e.Service] = ch
		if !e.held() {
			byRef[familiar(e.Ref)] = b
		}
	}
	// A held service's own build (withOwnBuilds), for what shares it.
	for i, e := range es {
		if _, ok := byRef[familiar(e.Ref)]; !ok && e.Service == "" && e.Kind == KindOwn {
			byRef[familiar(e.Ref)] = names[i]
		}
	}
	for svc, ref := range s.sharers(rel, es) {
		if b, ok := byRef[ref]; ok && !listed[svc] {
			holds[svc] = composeHold{Image: b, bookmark: true}
		}
	}
}

// sharers are rel's services of no entry in es (one under a profile: the
// listing is the default services') whose image is exactly an own entry's,
// by that entry's familiar ref: from its fetch file, which names every
// profile's service sharing a build under that build's fetch tag, else
// every profile's listing now (ListAll), never what an earlier file said.
func (s *Set) sharers(rel string, es []entry) map[string]string {
	out := map[string]string{}
	listed := map[string]bool{}
	own := map[string]bool{}
	for _, e := range es {
		listed[e.Service] = true
		if e.Kind == KindOwn {
			own[familiar(e.Ref)] = true
		}
	}
	if tags := fetchTags(rel); tags != nil {
		byTag := map[string]string{}
		for _, e := range es {
			if t := tags[e.Service]; t != "" && e.Service != "" {
				byTag[t] = familiar(e.Ref)
			}
		}
		for svc, t := range tags {
			if ref, ok := byTag[t]; ok && !listed[svc] {
				out[svc] = ref
			}
		}
		return out
	}
	if s.ListAll == nil || len(own) == 0 {
		return out
	}
	all, err := s.ListAll(rel)
	if err != nil {
		s.say(report.WarnLevel, "could not list every profile's services of %s (%v): one sharing a build's image runs what its name holds", s.idOf(rel), err)
		return out
	}
	for _, svc := range all {
		if ref := familiar(svc.Ref); own[ref] && !listed[svc.Name] {
			out[svc.Name] = ref
		}
	}
	return out
}

// writeHeld records rel's entries (keep: the listing and its bookmarks)
// and its ComposeFile with holds.
func (s *Set) writeHeld(rel string, es []entry, holds map[string]composeHold) error {
	es = withOwnBuilds(es)
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

// derive derives rel's holds again (holdsFor) from its data's history as
// it is now, a copy having just landed (Land), its labels svcs: a service
// the rule allows its release's own image on runs that again (unhold), one
// it refuses is held on the image the window ends on (applyHolds),
// whatever it ran before. Under the image lock; quiet when nothing
// changes. A built own image gone, which only a build gives back:
// ErrRefused, said (the check before a copy refuses it first: copyNeeds),
// but with asIs (--resolved: ResolvedLanding), where it stays held, said.
func (s *Set) derive(base, rel string, svcs map[string]Service, asIs bool) error {
	es := readList(rel)
	if len(es) == 0 {
		return nil
	}
	tid := s.idOf(rel)
	own := ownEntries(rel, es, svcs, nil)
	held := holdsFor(own, windows(base, rel))
	imgs, _, wasHeld := composeFacts(rel)
	before := append([]entry{}, es...)
	mode := ModeOf(rel)
	var lines []string
	kept := map[string]composeHold{}
	for i, e := range es {
		if _, ok := held[e.Service]; ok {
			es[i] = own[i]
			continue
		}
		if !e.held() {
			// Its own image by its bookmark, as every own image (resolveBookmarks).
			continue
		}
		svc, ok := svcs[e.Service]
		if !ok {
			// Its compose no longer gives it (a profile turned off), so
			// nothing runs it now and nothing pulls it: the rule allows its
			// own, so it goes back to that, for a plain up or a reload once
			// the profile is on (review: the hold outlived the data it was
			// for, its image upgrading the copy). A built own image gone
			// stays, said: only a release builds it.
			o := own[i]
			if o.Kind == KindOwn && !s.imagePresent(o.ID) {
				s.say(report.WarnLevel, "%s: %s's own image of it, built by the project, is gone from this server, and its compose no longer gives it: it stays on %s until a release", e.Service, tid, heldWhat(wasHeld[e.Service]))
				kept[e.Service] = composeHold{Image: imgs[e.Service], From: wasHeld[e.Service]}
				continue
			}
			es[i] = o
			lines = append(lines, fmt.Sprintf("%s goes back to %s's own image (%s, %s), not %s: its compose no longer gives it", e.Service, tid, shownEntry(o), shownData(o), heldWhat(wasHeld[e.Service])))
			continue
		}
		if asIs && svc.own(mode) && !s.imagePresent(own[i].ID) {
			// That image then runs on the data, so a release is judged by what
			// it ran: a plain one refused unless its label reaches that.
			s.sayAll(report.WarnLevel, fmt.Sprintf("%s: %s's own image of it, built by the project, is gone from this server: it stays on %s (%s), on the data as it is", e.Service, tid, heldWhat(wasHeld[e.Service]), shownData(e)),
				nil, report.Fix("a release builds it, its label raised to the data's version:", s.cmd("--release")),
				report.Fix("knowing its data takes the older image, accept it:", s.again("--release", []string{e.Service})))
			kept[e.Service] = composeHold{Image: imgs[e.Service], From: wasHeld[e.Service]}
			continue
		}
		ne, err := s.unhold(rel, e, own[i], svc, mode)
		if err != nil {
			return err
		}
		es[i] = ne
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
// as it holds now, pulled when gone; a built one its own build (o's id),
// by its bookmark as every own image; gone: ErrRefused, said.
func (s *Set) unhold(rel string, e, o entry, svc Service, mode string) (entry, error) {
	tid := s.idOf(rel)
	if !svc.own(mode) {
		if _, err := s.Docker.ID(svc.Ref); errors.Is(err, ErrNoImage) {
			s.say(report.InfoLevel, "%s: %s is gone from this server: pulling it", svc.Name, svc.Ref)
			if err := s.Docker.Pull(svc.Ref); err != nil {
				s.say(report.ErrorLevel, "%s: could not pull %s: %v", svc.Name, svc.Ref, err)
				return entry{}, err
			}
		}
		ne := s.entryOf(svc, svc.Ref, mode)
		ne.DataVersion, ne.Tag = o.DataVersion, o.Tag
		return ne, nil
	}
	if _, err := s.Docker.ID(o.ID); o.ID == "" || err != nil {
		s.sayAll(report.ErrorLevel, fmt.Sprintf("%s: %s's own image of it, built by the project, is gone from this server, and the image it is held on (%s) would upgrade its data again", svc.Name, tid, shownData(e)),
			nil, report.Fix("a release builds it:", s.cmd("--release")))
		return entry{}, ErrRefused
	}
	return entry{Ref: svc.Ref, ID: o.ID, Service: svc.Name, Kind: KindOwn, Build: o.Build, DataVersion: o.DataVersion, Tag: o.Tag}, nil
}

// LastHold is the restart's last resort, once get_images failed (the
// image lock's wait, a docker tag, a bookmark gone, compose's config
// unreadable): the holds derived as everywhere (holdsFor, from target's
// listing and its data's history), written into target's listing and
// ComposeFile without the image lock and without Docker, where a file can
// write them: a pulled image held by its registry digest, which the run
// step's up pulls when gone, the image it replaces in the listing
// recorded (LeftFile) for the next pruning; a pulled own image back as
// every derivation writes an unheld one (composeText: its own digest's
// DigestLabel, its name pinned to that digest). A built one, held or given back, by
// a bookmark that already names it, as image: (no name moved, which only
// Docker does); with none, said, naming the service and the way on, a
// release that fixes forward. Why (review): the restart went on with
// target's own ComposeFile, its older image on the data the newer one ran.
func (s *Set) LastHold(target, against string) error {
	tid := filepath.Base(target)
	es := readList(target)
	// Its labels now, as the restart it replaces reads them (Restore): the
	// run step runs under them. Compose's config unreadable, the listing's.
	svcs, err := s.labelsOf(target)
	if err != nil {
		s.say(report.WarnLevel, "could not read the labels of %s (%v): its data services' versions are its listing's", tid, err)
	}
	// Its data's window, as the restart's: a ForcedFile a cut activation
	// left in target is no acceptance of it now (review).
	own := ownEntries(target, es, svcs, svcs)
	held := holdsFor(own, readHistory(baseOf(target)).window)
	imgs, _, wasHeld := composeFacts(target)
	// What the file holds now stays but for what this changes, its own
	// images by their bookmarks and the services sharing them included.
	holds := map[string]composeHold{}
	for svc, img := range imgs {
		holds[svc] = composeHold{Image: img, From: wasHeld[svc]}
	}
	for svc, from := range wasHeld {
		holds[svc] = composeHold{Image: imgs[svc], From: from}
	}
	var lines, unheld []string
	listed := append([]entry{}, es...)
	listedNames := bookmarks(listed, s.Name, tid)
	for i, e := range es {
		o := own[i]
		h, ok := held[e.Service]
		switch {
		case ok:
			if e.held() && e.ID == h.line.ID && wasHeld[e.Service] != "" {
				continue // held on that image already
			}
			src, from := s.source(target, e.Service, h.line)
			// A built one by the bookmark of the release that lists it: a name
			// nothing moves, which a file can name without Docker.
			by := src.Digest
			if src.Kind == KindOwn || src.Digest == "" {
				if by = s.bookmarkOf(target, e.Service, src.ID, from); by == "" {
					unheld = append(unheld, fmt.Sprintf("%s: %s's image would run it on data it does not allow (%s), and %s, built by the project, has no bookmark here: it runs what %s holds", e.Service, tid, h.why, heldWhat(from), e.Ref))
					continue
				}
			}
			// The image it replaces in the listing keeps its names, which no
			// listing gives it now: recorded, as keep records them, or they
			// would outlive every release (no Docker here: the next pruning
			// retries them).
			if e.ID != "" && e.ID != src.ID && !e.held() {
				s.record(digestRef(e.Ref), e.ID, tid, "no release lists it any more")
			}
			ne := entry{Ref: o.Ref, ID: cmp.Or(src.ID, h.line.ID), Service: e.Service, Kind: src.Kind, Digest: src.Digest, Version: src.Version, DataVersion: h.line.Version, Tag: h.line.Tag}
			setOwn(&ne, o)
			es[i] = ne
			holds[e.Service] = composeHold{Image: by, From: cmp.Or(from, historyHeld)}
			lines = append(lines, fmt.Sprintf("%s stays on %s (%s, %s), as %s's would run it on data it does not allow (%s): a release that fixes forward is the way on", e.Service, heldWhat(from), shownEntry(ne), shownData(ne), tid, h.why))
		case e.held():
			if o.Kind == KindOwn || o.Digest == "" {
				// Its own build by the bookmark it kept (withOwnBuilds).
				b := ownBuildBookmark(listed, listedNames, e.OwnID)
				if b == "" {
					unheld = append(unheld, fmt.Sprintf("%s: its data allows %s's own image again, which has no bookmark here: it stays on %s", e.Service, tid, heldWhat(wasHeld[e.Service])))
					continue
				}
				es[i] = o
				holds[e.Service] = composeHold{Image: b}
				lines = append(lines, fmt.Sprintf("%s runs %s's own image again (%s, %s): its data's version allows it", e.Service, tid, shownEntry(o), shownData(o)))
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

// bookmarkOf is the bookmark of the image id of service in the listing of
// from (what source says of the image: a release of rel's environment, or
// a snapshot), "" without one.
func (s *Set) bookmarkOf(rel, service, id, from string) string {
	dir := filepath.Join(filepath.Dir(rel), from)
	if snap, ok := strings.CutPrefix(from, snapshotHeld); ok {
		dir = filepath.Join(s.snapshots(), snap)
	}
	es := readList(dir)
	names := bookmarks(es, s.Name, s.idOf(dir))
	for i, e := range es {
		if e.Service == service && e.ID == id && id != "" && !e.held() {
			return names[i]
		}
	}
	return ""
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
