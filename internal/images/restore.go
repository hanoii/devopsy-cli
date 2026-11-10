package images

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

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
//   - an own image is target's, by its bookmark, which only target writes
//     (no bookmark: ErrGone; with fetched, the name's just-built image).
//
// A data service's image is chosen as any service's, then its hold
// derived (holdsFor) against the window its data has when target runs:
// a rollback runs the rule on each (rule, CheckRollback) before any pull,
// refused, nothing changes (ErrRefused), accepted, its history starts
// over from the image it runs (ForcedFile), so it holds nothing new; the
// restart cannot refuse, and holds what the rule refuses on the image
// its data's history ends on, whatever --accept-image says; a full
// restore checks nothing, its data and their history coming back from
// the snapshot, whose window it derives against.
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
// a build and never fails on a pull: an own image gone stays as its
// listing has it, a hold that cannot be had is held all the same.
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
	old := readList(target)
	names := bookmarks(old, s.Name, tid)
	mode := ModeOf(target)
	h := readHistory(baseOf(target))
	window := h.window
	var accepted []string
	switch {
	case s.Restart:
	case s.NoCheck:
		// The window the data step brings back with the snapshot's data.
		window = s.restoreLanding(restoreOf(target)).after(h.window)
	default:
		if accepted, err = s.checkRollback(target, svcs, false); err != nil {
			return err
		}
		window = func(svc string) []ran {
			if slices.Contains(accepted, svc) {
				return nil
			}
			return h.window(svc)
		}
	}
	var claims []entry
	for _, svc := range svcs {
		if svc.Data {
			claims = append(claims, entry{Service: svc.Name, DataVersion: svc.DataVersion, Tag: svc.tag()})
		}
	}
	held := holdsFor(claims, window)

	type step struct {
		svc  Service
		kind string
		// retag is an own image's bookmark, which it runs by.
		retag string
		// asIs: as its listing has it (AsBuilt: its bookmark gone, and
		// nothing to build).
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
				if haveOld && o.held() {
					// Its entry's bookmark holds the image it was held on, never
					// its own build (review: a rollback to a release the restart
					// held ran the failed release's build as its own): that one
					// is the entry of no service's (withOwnBuilds).
					b := ownBuildBookmark(old, names, o.OwnID)
					if o.OwnID != "" && o.OwnID == o.ID && i >= 0 {
						b = names[i] // held on the very image: its entry's bookmark
					}
					if b != "" {
						if _, err := s.Docker.ID(b); err == nil {
							if s.changed(ownOf(o), svc, mode) {
								if err := s.rebuilt(tid, ownOf(o), svc); err != nil {
									return err
								}
							}
							st.retag = b
						}
					}
					if st.retag == "" {
						if st.asIs = s.AsBuilt; !st.asIs {
							s.say(report.InfoLevel, "%s's image of %s was held on another image: building its own again", tid, svc.Name)
							return ErrGone
						}
						s.say(report.WarnLevel, "%s's own image of %s is gone, and nothing builds it now: %s stays as it is", tid, svc.Name, svc.Ref)
					}
				} else if !haveOld || o.ID == "" || i < 0 {
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

	// The changes: pulls, an own image's name put back, the holds, then
	// the record.
	es := make([]entry, 0, len(steps))
	var lines []string
	for _, st := range steps {
		svc := st.svc
		o, haveOld := match(old, entry{Service: svc.Name, Ref: svc.Ref})
		if _, ok := held[svc.Name]; ok {
			// Its own image as target's listing has it (no build, no pull:
			// it runs another), held below.
			e := entry{Ref: svc.Ref, Service: svc.Name, Kind: KindPulled}
			if haveOld {
				e = ownOf(o)
			}
			if svc.own(mode) {
				e.Kind = KindOwn
			}
			e.DataVersion, e.Tag = svc.DataVersion, svc.tag()
			es = append(es, e)
			continue
		}
		switch {
		case st.asIs:
			// As its listing had it, never what a name holds now: no name but
			// its bookmark is target's.
			e := s.entryOf(svc, digestRef(svc.Ref), mode)
			if haveOld && o.ID != "" {
				e = ownOf(o)
				e.Service, e.DataVersion, e.Tag = svc.Name, svc.DataVersion, svc.tag()
			}
			es = append(es, e)
		case st.kind == KindOwn && st.retag != "":
			// Its bookmark: built with what its listing said then, whatever
			// the variables say now, and run by it, never by a name another
			// environment's build moves (stage, a branch's, a pull request's).
			e := s.entryOf(svc, st.retag, mode)
			e.Ref = svc.Ref
			if b := ownOf(o).Build; b != "" && e.Build != "" {
				e.Build = b
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
			if was := ownOf(o); e.Kind != KindOwn && haveOld && was.ID != "" && was.ID != e.ID {
				lines = append(lines, fmt.Sprintf("%s runs what %s holds now (%s), not the image %s ran (%s): pin a version for a rollback to take it back", svc.Name, svc.Ref, shownEntry(e), tid, shownEntry(was)))
			}
		}
	}
	holds, heldLines, err := s.applyHolds(target, es, held, &pulled)
	if err != nil {
		return err
	}
	lines = append(heldLines, lines...)
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
	if err := s.writeHeld(target, es, holds); err != nil {
		return err
	}
	switch {
	case len(heldLines) > 0:
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
			js = append(js, judged{service: svc.Name, why: rule(h.window(svc.Name), svc.DataVersion, svc.tag(), "its"), version: svc.DataVersion, tag: svc.tag()})
		}
	}
	return s.judge(js, checkWords{source: "its", what: "the rollback to " + filepath.Base(target) + " would run its data on an image its version does not allow", nothing: "nothing changed", flag: "--rollback", rollback: true}, target, quiet)
}

// snapshotHeld prefixes HeldLabel's value for a hold on an image a
// snapshot's listing names: the snapshot's id follows.
const snapshotHeld = "snapshot "

// A snapshot's data directory and its images file (remote's
// SnapshotData, the take's data/images: SnapshotData's windows).
const (
	snapshotDataDir = "data"
	snapshotImages  = "images"
)

// restoreLanding is what snapshot snap's data step lands (RestoreLanding),
// its windows from its images file (SnapshotData): none without it.
func (s *Set) restoreLanding(snap string) Landing {
	var data []byte
	if dir := s.snapshots(); snap != "" && dir != "" {
		data, _ = os.ReadFile(filepath.Join(dir, snap, snapshotDataDir, snapshotImages))
	}
	return s.RestoreLanding(string(data), snap, nil)
}

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

// ownBuildBookmark is the bookmark of a held service's own build, id, in a
// listing (withOwnBuilds) and its bookmarks; "" without one.
func ownBuildBookmark(es []entry, names []string, id string) string {
	for i, e := range es {
		if e.Service == "" && e.Kind == KindOwn && id != "" && e.ID == id {
			return names[i]
		}
	}
	return ""
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

// FetchFailedFile, in a release's directory, says its fetch never finished
// (Undo): its listing bookmarks what its names held then, under its labels,
// so no hold takes an image from it (allowedHere).
const FetchFailedFile = "devopsy-fetch-failed"

// Undo settles a release's fetch that failed or was interrupted: a new
// release's own images are bookmarked, so they go with it; a rollback's
// are not (the names may hold the live release's images, which would
// become the target's). The live release runs its own by its bookmarks,
// which no fetch moves.
func (s *Set) Undo(rel string, rollback bool) {
	if !rollback {
		s.Bookmark(rel)
		if f, err := os.Create(filepath.Join(rel, FetchFailedFile)); err == nil {
			f.Close()
		}
	}
}

// Recover finishes what a release cut off mid-fetch without a signal
// left (FetchingFile): marked failed, what it got bookmarked.
func (s *Set) Recover(base string) {
	marks, _ := filepath.Glob(filepath.Join(base, "releases", "*", FetchingFile))
	for _, m := range marks {
		rel := filepath.Dir(m)
		s.say(report.WarnLevel, "the images' fetch of release %s was interrupted: marking it failed, its images bookmarked", filepath.Base(rel))
		s.Undo(rel, false)
		if f, err := os.Create(filepath.Join(rel, failedFile)); err == nil {
			f.Close()
		}
		os.Remove(m)
	}
}
