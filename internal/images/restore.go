package images

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// ErrGone is an image the project builds gone with its bookmark: the
// release's images must be built again (img exits 1, and the script
// fetches, then restores with fetched).
var ErrGone = errors.New("an image the project builds is gone")

// ErrRefused is a check refusing an activation, said already (img exits
// 3).
var ErrRefused = errors.New("refused")

// baseOf is a release's environment directory: base/releases/<id>.
func baseOf(rel string) string { return filepath.Dir(filepath.Dir(rel)) }

// Restore makes target run in place of against: a rollback (against is
// the release it leaves), the restart after a failed run (Restart: the
// failed release), a full restore (NoCheck: the release made from a
// snapshot, against the current one), and writes what target runs now
// (its listing, bookmarks and ComposeFile), service by service, as
// compose resolves target's config now:
//
//   - a pulled image runs what target's name holds now, pulled when gone,
//     held by nothing: a pin or a commit tag is target's image anyway, a
//     floating tag its current (patched) one;
//   - an own image is target's, its bookmark put back on its name (no
//     bookmark: ErrGone; with fetched, the name's just-built image).
//
// A data service's image is chosen as any service's. A rollback runs the
// rule on each (rule, CheckRollback) before any pull: refused, nothing
// changes (ErrRefused); accepted, its history starts over from the image
// it runs (ForcedFile). A full restore checks nothing: its data and their
// history come back from the snapshot, and a data service the rule would
// refuse against the snapshot's window stays on the image the snapshot's
// listing ran on that data (restoreHolds). The restart cannot refuse: a
// data service the rule would refuse, when the failed release's image ran
// on its data, stays on that image (holds), whatever --accept-image says.
//
// Never a pulled name retagged: another environment may run it. Every
// service that does not go back says why. Every check comes before any
// change, so a failure changes nothing but pulls.
//
// Own against pulled, and whether a build changed, go by target's own
// mode (ModeOf): it was fetched in it, whatever mode the project is in
// now.
//
// AsBuilt (the restart's last try, after a build failed) never asks for
// a build and never fails on a pull: an own image gone stays as its name
// is, a hold that cannot be pulled is held all the same.
func (s *Set) Restore(target, against string, fetched bool) (err error) {
	// What it pulled by digest goes when it fails: a name by digest alone
	// that no listing names would never be removed.
	var pulled []string
	defer func() {
		if err != nil {
			for _, p := range pulled {
				_ = s.Docker.Remove(p)
			}
		}
	}()
	tid := filepath.Base(target)
	svcs, err := s.List(target)
	if err != nil {
		s.say(report.ErrorLevel, "could not list the images of %s: %v", tid, err)
		return err
	}
	old, ag := readList(target), readList(against)
	names := bookmarks(old, s.Name, tid)
	mode := ModeOf(target)
	var held map[string]restartHold
	var accepted []string
	switch {
	case s.Restart:
		held = s.holds(target, against, svcs, ag)
	case s.NoCheck:
		held = s.restoreHolds(target, svcs, old)
	default:
		if accepted, err = s.checkRollback(target, svcs, false); err != nil {
			return err
		}
	}

	type step struct {
		svc  Service
		kind string
		// retag is an own image's bookmark to put back on its name.
		retag string
		// asIs: as its listing has it, nothing moved (AsBuilt: its
		// bookmark gone, and nothing to build).
		asIs bool
	}
	var steps []step
	for _, svc := range svcs {
		probe := entry{Service: svc.Name, Ref: svc.Ref}
		o, haveOld := match(old, probe)
		st := step{svc: svc, kind: KindPulled}
		if _, ok := held[svc.Name]; ok {
			steps = append(steps, st)
			continue
		}
		if svc.own(mode) {
			st.kind = KindOwn
			if !fetched {
				i := indexOf(old, o)
				if !haveOld || o.ID == "" || i < 0 {
					if st.asIs = s.AsBuilt; !st.asIs {
						return s.gone(tid, svc)
					}
					s.say(report.WarnLevel, "%s's image of %s is gone, and nothing builds it now: %s stays as it is", tid, svc.Name, svc.Ref)
				} else {
					if s.changed(o, svc, mode) {
						if err := s.rebuilt(tid, o, svc); err != nil {
							return err
						}
					}
					if _, err := s.Docker.ID(names[i]); err != nil {
						if st.asIs = s.AsBuilt; !st.asIs {
							return s.gone(tid, svc)
						}
						s.say(report.WarnLevel, "%s's image of %s is gone, and nothing builds it now: %s stays as it is", tid, svc.Name, svc.Ref)
					} else {
						st.retag = names[i]
					}
				}
			}
		}
		steps = append(steps, st)
	}

	// The changes: holds, pulls, an own image's name put back, then the
	// record.
	es := make([]entry, 0, len(steps))
	holds := map[string]composeHold{}
	var lines []string
	own := s.own(target)
	for _, st := range steps {
		svc := st.svc
		o, haveOld := match(old, entry{Service: svc.Name, Ref: svc.Ref})
		if hd, ok := held[svc.Name]; ok {
			e, ch, err := s.hold(svc, hd, mode, own, &pulled)
			if err != nil {
				return err
			}
			es = append(es, e)
			holds[svc.Name] = ch
			lines = append(lines, fmt.Sprintf("%s stays on %s (%s, %s), as %s's would %s: a release that fixes forward is the way on", svc.Name, hd.what, shownEntry(e), shownData(hd.e), tid, hd.why))
			continue
		}
		switch {
		case st.asIs:
			es = append(es, s.entryOf(svc, digestRef(svc.Ref), mode))
		case st.kind == KindOwn && st.retag != "":
			bid, _ := s.Docker.ID(st.retag)
			if cur, err := s.Docker.ID(svc.Ref); err != nil || cur != bid {
				if err == nil {
					if who := s.others(cur, own); len(who) > 0 {
						s.say(report.WarnLevel, "%s stays on its newer image, which other environments use (%s): %s runs it too", svc.Ref, strings.Join(who, ", "), tid)
						es = append(es, s.entryOf(svc, svc.Ref, mode))
						continue
					}
				}
				if err := s.Docker.Tag(st.retag, svc.Ref); err != nil {
					s.say(report.ErrorLevel, "could not put %s back on %s's image: %v", svc.Ref, tid, err)
					return err
				}
			}
			// Its bookmark, put back: built with what its listing said then,
			// whatever the variables say now.
			e := s.entryOf(svc, svc.Ref, mode)
			if o.Build != "" && e.Build != "" {
				e.Build = o.Build
			}
			es = append(es, e)
		default:
			if _, err := s.Docker.ID(svc.Ref); errors.Is(err, ErrNoImage) {
				s.say(report.InfoLevel, "%s: %s is gone from this server: pulling it", svc.Name, svc.Ref)
				if err := s.Docker.Pull(svc.Ref); err != nil {
					s.say(report.ErrorLevel, "%s: could not pull %s: %v", svc.Name, svc.Ref, err)
					return err
				}
			}
			e := s.entryOf(svc, svc.Ref, mode)
			es = append(es, e)
			if e.Kind != KindOwn && haveOld && o.ID != "" && o.ID != e.ID {
				lines = append(lines, fmt.Sprintf("%s runs what %s holds now (%s), not the image %s ran (%s): pin a version for a rollback to take it back", svc.Name, svc.Ref, shownEntry(e), tid, shownEntry(o)))
			}
		}
	}
	// What --accept-image let through: its data's history starts over from
	// the image it runs, once it runs (Ran).
	var forced []string
	for _, e := range es {
		if slices.Contains(accepted, e.Service) {
			forced = append(forced, e.Service+"\t"+e.ID)
		}
	}
	if err := writeForced(target, forced); err != nil {
		s.say(report.ErrorLevel, "could not record what --accept-image let through: %v", err)
		return err
	}
	if err := s.keep(target, es); err != nil {
		s.say(report.ErrorLevel, "could not record the images of %s: %v", tid, err)
		return err
	}
	// A hold by target's own bookmark: the name keep just gave it.
	for i, b := range bookmarks(es, s.Name, tid) {
		if ch, ok := holds[es[i].Service]; ok && ch.bookmark {
			ch.Image = b
			holds[es[i].Service] = ch
		}
	}
	if err := writeCompose(target, es, holds); err != nil {
		s.say(report.ErrorLevel, "could not write %s's %s: %v", tid, ComposeFile, err)
		return err
	}
	switch {
	case len(held) > 0:
		s.sayAll(report.WarnLevel, "images of "+tid+" restored, but for what its data does not allow:", lines)
	case len(lines) > 0:
		s.sayAll(report.InfoLevel, "images of "+tid+" restored, but for what does not go back:", lines)
	default:
		s.say(report.InfoLevel, "images of %s restored", tid)
	}
	return nil
}

// CheckRollback runs the rule on target's data services, as a rollback
// to it would run them (compose's config in its code now), before
// anything changes: img rollback-target, ahead of the rollback's before
// and prepare steps, leniently (a config that cannot be read there, a
// variable left unset, refuses nothing: --missing-vars says so later);
// Restore checks for real.
func (s *Set) CheckRollback(target string) error {
	svcs, err := s.List(target)
	if err != nil {
		return nil
	}
	_, err = s.checkRollback(target, svcs, true)
	return err
}

// checkRollback is the rollback's check of svcs, target's: the services
// --accept-image let through (judge), quiet but for a refusal.
func (s *Set) checkRollback(target string, svcs []Service, quiet bool) ([]string, error) {
	h := readHistory(baseOf(target))
	var js []judged
	for _, svc := range svcs {
		if svc.Data {
			js = append(js, judged{service: svc.Name, why: rule(h.window(svc.Name), svc.DataVersion, svc.tag(), "its")})
		}
	}
	return s.judge(js, checkWords{source: "its", what: "the rollback to " + filepath.Base(target) + " would run its data on an image its version does not allow", nothing: "nothing changed", flag: "--rollback", rollback: true}, target, quiet)
}

// restartHold is a data service the restart keeps on the failed
// release's image, or a full restore on the one its snapshot's data ran:
// its entry there (e), and why the restored release's own would not do.
type restartHold struct {
	e entry
	// dir is the directory whose bookmark holds e's image (the failed
	// release's, the restored release's own, bookmarked from the
	// snapshot); from what ComposeFile's HeldLabel says (the failed
	// release's id, "snapshot <id>"); what the image, for messages.
	dir, from, what, why string
}

// holds are the data services of target (svcs) the restart after the
// failed release against keeps on against's image: those the rule refuses
// against their data's history, once against's image ran on it (its
// img ran came before: the window's last line is that image). The rule
// is the one every check runs, so a later rollback finds the same data.
// --accept-image never reaches it: the release accepted its new image,
// not the old one back on the new data.
func (s *Set) holds(target, against string, svcs []Service, ag []entry) map[string]restartHold {
	h := readHistory(baseOf(target))
	held := map[string]restartHold{}
	for _, svc := range svcs {
		if !svc.Data {
			continue
		}
		fe, ok := match(ag, entry{Service: svc.Name})
		last, ran := h.last(svc.Name)
		if !ok || fe.ID == "" || fe.DataVersion == "" || !ran || last.ID != fe.ID {
			continue
		}
		if why := rule(h.window(svc.Name), svc.DataVersion, svc.tag(), "its"); why != "" {
			held[svc.Name] = restartHold{e: fe, dir: against, from: filepath.Base(against), what: "the image release " + filepath.Base(against) + " ran on its data", why: "run it on data it does not allow (" + why + ")"}
		}
	}
	return held
}

// restoreHolds are the data services of target, the release a full
// restore made from a snapshot (its record's restore), that the rule
// refuses against the snapshot's window (its data's images: the window
// the data step brings back), held on the image the snapshot's listing
// (old: target's own, copied from it) ran on that data, as the restart
// holds. Why (review): a snapshot taken while the restart held a database
// on a failed release's newer image has the previous release's code, so
// its restore ran the older image on the newer data, unchecked; a
// release snapshot taken by the release fixing forward is the one a
// refused rollback names. A service the snapshot carries no window for
// is judged against its listing's own version and tag.
func (s *Set) restoreHolds(target string, svcs []Service, old []entry) map[string]restartHold {
	snap := restoreOf(target)
	var ps map[string]*sourceData
	if dir := s.snapshots(); snap != "" && dir != "" {
		data, _ := os.ReadFile(filepath.Join(dir, snap, snapshotDataDir, snapshotImages))
		ps = parseParentData(string(data))
	}
	held := map[string]restartHold{}
	for _, svc := range svcs {
		if !svc.Data {
			continue
		}
		o, ok := match(old, entry{Service: svc.Name})
		if !ok || o.DataVersion == "" || o.ID == "" && o.Digest == "" {
			continue
		}
		w := []ran{{Service: svc.Name, How: Ran, Version: o.DataVersion, Tag: o.Tag}}
		if p := ps[svc.Name]; p != nil {
			w = p.window()
		}
		if why := rule(w, svc.DataVersion, svc.tag(), "the snapshot's"); why != "" {
			held[svc.Name] = restartHold{e: o, dir: target, from: snapshotHeld + snap, what: "the image its data ran when snapshot " + snap + " was taken", why: "run it on data it does not allow (" + why + ")"}
		}
	}
	return held
}

// snapshotHeld prefixes HeldLabel's value for a full restore's hold: the
// snapshot's id follows.
const snapshotHeld = "snapshot "

// A snapshot's data directory and its images file (remote's
// SnapshotData, the take's data/images: SnapshotData's windows).
const (
	snapshotDataDir = "data"
	snapshotImages  = "images"
)

// restoreOf is the snapshot a full restore made rel from (its record's
// restore), "" for any other release.
func restoreOf(rel string) string {
	var r struct {
		Restore string `json:"restore"`
	}
	data, err := os.ReadFile(filepath.Join(rel, RecordFile))
	if err != nil || json.Unmarshal(data, &r) != nil {
		return ""
	}
	return r.Restore
}

// hold gets svc onto the failed release's image hd.e, the entry it is
// listed as and what devopsy's compose file holds it by:
//
//   - pulled (a registry digest): by digest, pulled when gone;
//   - built, under target's own name: that name retagged from the failed
//     release's bookmark, through others (a shared pull_policy: never
//     name never moves another environment's), nothing in the compose
//     file but the mark;
//   - built under another name (target pulls it, or names it otherwise):
//     image: the failed release's own name, never a retag of target's
//     pulled one, the listing recording that image so target's bookmark
//     keeps it.
//
// The entry carries the held image's own version and tag, never
// target's: what Ran records and every later check compares.
func (s *Set) hold(svc Service, hd restartHold, mode string, own map[string]bool, pulled *[]string) (entry, composeHold, error) {
	fe := hd.e
	ch := composeHold{From: hd.from}
	var e entry
	switch {
	case fe.Kind != KindOwn && fe.Digest != "":
		if _, err := s.Docker.Inspect(fe.Digest); err != nil {
			if err := s.Docker.Pull(fe.Digest); err != nil && !s.AsBuilt {
				s.sayAll(report.ErrorLevel, fmt.Sprintf("%s: %s, the image its data ran, is gone from this server and could not be pulled by its digest", svc.Name, fe.Digest),
					[]string{err.Error()}, report.Fix("the image its data ran must be there: never one its data does not allow"))
				return entry{}, ch, ErrRefused
			} else if err != nil {
				s.say(report.WarnLevel, "%s: %s is gone from this server and could not be pulled by its digest (%v): held on it anyway, for the run step to pull", svc.Name, fe.Digest, err)
			} else {
				*pulled = append(*pulled, fe.Digest)
			}
		}
		e = s.entryOf(svc, fe.Digest, mode)
		e.Kind, e.Digest = KindPulled, fe.Digest
		if e.ID == "" {
			e.ID = fe.ID
		}
		ch.Image = fe.Digest
	default:
		// Built: its own name, or the failed release's when target names
		// it otherwise (pulls it), which compose's image: then holds.
		name := svc.Ref
		if familiar(fe.Ref) != familiar(svc.Ref) {
			name, ch.Image = fe.Ref, fe.Ref
		}
		if cur, err := s.Docker.ID(name); err != nil || cur != fe.ID {
			src := bookmarkOf(hd.dir, s.Name, svc.Name)
			if _, berr := s.Docker.ID(src); src == "" || berr != nil {
				if !s.AsBuilt {
					s.say(report.ErrorLevel, "%s: %s, built by the project, is gone from this server", svc.Name, hd.what)
					return entry{}, ch, ErrRefused
				}
				s.say(report.WarnLevel, "%s: %s, built by the project, is gone from this server: it runs what %s holds", svc.Name, hd.what, name)
			} else if who := s.others(cur, own); err == nil && len(who) > 0 {
				// A name another environment runs never moves: held by
				// target's own bookmark of the image instead (Restore names
				// it once the listing is kept).
				s.say(report.InfoLevel, "%s stays where it is, which other environments use (%s): %s is held on the image by its bookmark", name, strings.Join(who, ", "), svc.Name)
				ch.Image, ch.bookmark = "", true
			} else if err := s.Docker.Tag(src, name); err != nil {
				s.say(report.ErrorLevel, "could not put %s on the image its data ran: %v", name, err)
				return entry{}, ch, err
			}
		}
		e = entry{Ref: name, ID: fe.ID, Service: svc.Name, Kind: KindOwn, Build: fe.Build, Version: fe.Version}
		if ch.bookmark {
			e.Ref = svc.Ref
		}
	}
	e.DataVersion, e.Tag = fe.DataVersion, fe.Tag
	return e, ch, nil
}

// LastHold is the restart's last resort, once get_images failed (the
// image lock's wait, a docker tag, a bookmark gone, compose's config
// unreadable): without the image lock and without Docker, from the
// listings and the history alone, it writes into target's listing and
// ComposeFile the holds the restart would have made where a file can
// make them: a data service whose version and tag in target's listing
// the rule refuses against its data's window, once against's image ran on
// it, held by against's registry digest (pulled), which the run step's up
// pulls when gone. A built one needs its own name moved onto the image,
// which only Docker does: said, naming the service and the way on, a
// release that fixes forward. Why (review): the restart went on with
// target's own ComposeFile, its older image on the data the newer one
// ran. Holds already in the file stay.
func (s *Set) LastHold(target, against string) error {
	tid, from := filepath.Base(target), filepath.Base(against)
	es, ag := readList(target), readList(against)
	h := readHistory(baseOf(target))
	imgs, _, held := composeFacts(target)
	holds := map[string]composeHold{}
	for svc, f := range held {
		holds[svc] = composeHold{Image: imgs[svc], From: f}
	}
	var lines, unheld []string
	for i, e := range es {
		if e.Service == "" || e.DataVersion == "" || holds[e.Service].From != "" {
			continue
		}
		fe, ok := match(ag, entry{Service: e.Service})
		last, ran := h.last(e.Service)
		if !ok || fe.ID == "" || fe.DataVersion == "" || !ran || last.ID != fe.ID {
			continue
		}
		why := rule(h.window(e.Service), e.DataVersion, e.Tag, "its")
		if why == "" {
			continue
		}
		if fe.Kind == KindOwn || fe.Digest == "" {
			unheld = append(unheld, fmt.Sprintf("%s: %s's image would run it on data it does not allow (%s), and the image release %s built, which its data ran, is held only by moving a name, which needs the images: it runs what %s holds", e.Service, tid, why, from, e.Ref))
			continue
		}
		e.ID, e.Kind, e.Digest, e.Version, e.Build, e.DataVersion, e.Tag = fe.ID, KindPulled, fe.Digest, fe.Version, "", fe.DataVersion, fe.Tag
		es[i] = e
		holds[e.Service] = composeHold{Image: fe.Digest, From: from}
		lines = append(lines, fmt.Sprintf("%s stays on the image release %s ran on its data (%s, %s), as %s's would run it on data it does not allow (%s): a release that fixes forward is the way on", e.Service, from, shownEntry(e), shownData(fe), tid, why))
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

// bookmarkOf is the bookmark of service in the release rel's listing,
// "" when it lists none.
func bookmarkOf(rel, name, service string) string {
	es := readList(rel)
	for i, b := range bookmarks(es, name, filepath.Base(rel)) {
		if es[i].Service == service && es[i].ID != "" {
			return b
		}
	}
	return ""
}

func (s *Set) gone(tid string, svc Service) error {
	s.say(report.InfoLevel, "%s's image of %s is gone: building it again", tid, svc.Name)
	return ErrGone
}

// changed says whether svc's build: section resolves otherwise now than
// when o's image was built (a build argument changed since): its bookmark
// holds code built for the old values, which would run under the new
// ones (an image built for APP_UID 10001 run as 10002). Only where the
// release's mode's fetch builds it: image mode's only pulls (a
// pull_policy: never service with a build: is never built on the
// server).
func (s *Set) changed(o entry, svc Service, mode string) bool {
	return svc.Build && mode != ModeImage && o.Build != "" && svc.BuildKey != "" && o.Build != svc.BuildKey
}

// rebuilt says that o's image is built again for what its build: section
// gives now, and asks for the build (ErrGone, as for an image gone). As
// built (AsBuilt: that build failed), its bookmark goes back all the
// same, said, and nil.
func (s *Set) rebuilt(tid string, o entry, svc Service) error {
	what := changedArgs(o.Build, svc.BuildKey)
	if s.AsBuilt {
		s.say(report.WarnLevel, "%s's image of %s goes back as built, with other values of %s than now: a release builds it for them", tid, svc.Name, what)
		return nil
	}
	s.say(report.InfoLevel, "%s's image of %s was built with other values of %s: building it again", tid, svc.Name, what)
	return ErrGone
}

func indexOf(es []entry, e entry) int {
	for i, x := range es {
		if x == e {
			return i
		}
	}
	return -1
}

// shownData is a data service's version and tag, for messages.
func shownData(e entry) string {
	v := DataVersionLabel + "=" + e.DataVersion
	if e.Tag != "" {
		v += ", tag " + short(e.Tag)
	}
	return v
}

// shownEntry is an entry's image for messages: its version, else its
// digest name, short.
func shownEntry(e entry) string {
	if e.Digest == "" {
		return short(e.ID)
	}
	s := repoOf(e.Digest) + "@" + short(digestOf(e.Digest))
	if e.Version != "" {
		s += " (" + e.Version + ")"
	}
	return s
}

// RestoreOwn puts a release's own images back on their names, after a
// release's fetch failed or was cut off (Undo, Recover): a build moved
// them, and a plain up in the live release would run what the failed
// one built. Pulled names stay where the fetch left them: devopsy never
// moves a pulled name back (another environment may run it), and the
// live release's ComposeFile holds what must not move.
func (s *Set) RestoreOwn(rel string) {
	id := s.idOf(rel)
	es := readList(rel)
	if len(es) == 0 {
		return
	}
	names := bookmarks(es, s.Name, id)
	own := s.own(rel)
	for i, e := range es {
		if e.Kind != KindOwn || e.ID == "" || strings.Contains(e.Ref, "@") {
			continue
		}
		bid, err := s.Docker.ID(names[i])
		if err != nil {
			continue
		}
		cur, err := s.Docker.ID(e.Ref)
		if err == nil && cur == bid {
			continue
		}
		if err == nil {
			if who := s.others(cur, own); len(who) > 0 {
				s.say(report.WarnLevel, "%s stays on its newer image, which other environments use (%s): %s runs it too", e.Ref, strings.Join(who, ", "), id)
				continue
			}
		}
		if err := s.Docker.Tag(names[i], e.Ref); err != nil {
			s.say(report.WarnLevel, "could not put %s back on %s's image: %v", e.Ref, id, err)
		}
	}
}

// Undo puts the live release's own names back after a release's fetch
// failed or was interrupted: a new release's own images are bookmarked
// first, so they go with it; a rollback's are not (the names may hold the
// live release's images, which would become the target's).
func (s *Set) Undo(rel, prev string, rollback bool) {
	if !rollback {
		s.Bookmark(rel)
	}
	if prev != "" {
		s.RestoreOwn(prev)
	}
}

// Recover finishes what a release cut off mid-fetch without a signal
// left (FetchingFile): marked failed, what it got bookmarked, and the
// current release's own names (prev, when there is one) back.
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
