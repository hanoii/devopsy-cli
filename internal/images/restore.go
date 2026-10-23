package images

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// the release it leaves), the restart after a failed run (the failed
// release), and writes what target runs now (its listing, bookmarks and
// ComposeFile), service by service, as compose resolves target's config
// now:
//
//   - a pulled data service runs the newest image its data ran (the
//     history's last, else what its container runs), held by digest,
//     whatever target's name: data only goes forward;
//   - a pulled service whose name is the same in both releases runs what
//     the name holds now (forward), held by nothing;
//   - a pulled service whose name changed (a commit tag, a pin) runs
//     target's image, held by digest when its name points elsewhere,
//     pulled by its digest when gone, else by its name, said;
//   - an own image is target's, its bookmark put back on its name (no
//     bookmark: ErrGone; with fetched, the name's just-built image).
//
// Never a pulled name retagged: another environment may run it. Every
// service that does not go back says why. Every check comes before any
// change, so a failure changes nothing but pulls.
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
	base := baseOf(target)
	h := readHistory(base)
	var cs map[string]Container

	type step struct {
		svc   Service
		kind  string
		hold  string // a digest name to run, pulled when gone
		retag string // an own image's bookmark to put back on its name
		tag   string // the name to pull by when the digest cannot be
		line  string
	}
	var steps []step
	for _, svc := range svcs {
		probe := entry{Service: svc.Name, Ref: svc.Ref}
		o, haveOld := match(old, probe)
		a, haveAg := match(ag, probe)
		st := step{svc: svc}
		own := svc.own(s.Mode)
		// A pulled data service in the release it leaves stays one, even
		// where the target builds it or never pulls it: its data ran the
		// pulled image, which an unordered own image must not replace.
		data := (!own && ((svc.Data && !svc.Build) || o.Kind == KindData)) || a.Kind == KindData
		switch {
		case data && own && !haveHistory(h, svc.Name):
			// Nothing known ran on its data: as the target builds it.
			st.kind = KindOwn
			if fetched {
				break
			}
			i := indexOf(old, o)
			if !haveOld || o.ID == "" || i < 0 {
				return s.gone(tid, svc)
			}
			if _, err := s.Docker.ID(names[i]); err != nil {
				return s.gone(tid, svc)
			}
			st.retag = names[i]
		case own && !data:
			st.kind = KindOwn
			if fetched {
				break
			}
			i := indexOf(old, o)
			if !haveOld || o.ID == "" || i < 0 {
				return s.gone(tid, svc)
			}
			if _, err := s.Docker.ID(names[i]); err != nil {
				return s.gone(tid, svc)
			}
			st.retag = names[i]
		case data:
			st.kind = KindData
			if last, ok := h.last(svc.Name); ok && last.hold() != "" {
				st.hold = last.hold()
				break
			}
			if cs == nil {
				cs = s.containers()
			}
			if c, ok := cs[svc.Name]; ok && c.started() {
				if img, err := s.Docker.Inspect(c.Image); err == nil {
					if d := digestIn(img, svc.Ref); d != "" {
						st.hold = d
					} else if len(img.Digests) > 0 {
						st.hold = img.Digests[0]
					}
				}
			}
			if st.hold == "" && haveAg && a.Digest != "" {
				st.hold = a.Digest
			}
			if st.hold == "" {
				// Nothing ever ran on its data that devopsy knows of: like any
				// pulled image whose name did not change.
				st.kind = KindPulled
			}
		default:
			st.kind = KindPulled
			if haveAg && familiar(a.Ref) != familiar(svc.Ref) && haveOld && o.ID != "" {
				cur, err := s.Docker.ID(svc.Ref)
				if err == nil && cur == o.ID {
					break
				}
				hold := o.Digest
				if img, err := s.Docker.Inspect(o.ID); err == nil {
					if d := digestIn(img, svc.Ref); d != "" {
						hold = d
					}
				} else {
					st.tag = svc.Ref
				}
				if hold == "" {
					// Never held by a digest: forward, as unchanged.
					st.tag = ""
					break
				}
				st.hold = hold
			}
		}
		steps = append(steps, st)
	}

	// The changes: pulls, an own image's name put back, then the record.
	es := make([]entry, 0, len(steps))
	holds := map[string]string{}
	var lines []string
	own := s.own(target)
	for _, st := range steps {
		svc := st.svc
		o, haveOld := match(old, entry{Service: svc.Name, Ref: svc.Ref})
		switch {
		case st.kind == KindOwn && st.retag != "":
			bid, _ := s.Docker.ID(st.retag)
			if cur, err := s.Docker.ID(svc.Ref); err != nil || cur != bid {
				if err == nil {
					if who := s.others(cur, own); len(who) > 0 {
						s.say(report.WarnLevel, "%s stays on its newer image, which other environments use (%s): %s runs it too", svc.Ref, strings.Join(who, ", "), tid)
						es = append(es, s.entryOf(svc, svc.Ref))
						continue
					}
				}
				if err := s.Docker.Tag(st.retag, svc.Ref); err != nil {
					s.say(report.ErrorLevel, "could not put %s back on %s's image: %v", svc.Ref, tid, err)
					return err
				}
			}
			es = append(es, s.entryOf(svc, svc.Ref))
		case st.hold != "":
			if _, err := s.Docker.Inspect(st.hold); err != nil {
				if err := s.Docker.Pull(st.hold); err != nil {
					if st.kind == KindData || st.tag == "" {
						s.sayAll(report.ErrorLevel, fmt.Sprintf("%s: %s is gone from this server and could not be pulled by its digest", svc.Name, st.hold),
							[]string{err.Error()}, report.Fix("the newest image its data ran must be there: never one older"))
						return ErrRefused
					}
					s.say(report.WarnLevel, "%s: %s is gone, and its digest could not be pulled (%v): pulling %s, whatever it is now", svc.Name, short(st.hold), err, st.tag)
					if err := s.Docker.Pull(st.tag); err != nil {
						s.say(report.ErrorLevel, "%s: could not pull %s: %v", svc.Name, st.tag, err)
						return err
					}
					es = append(es, s.entryOf(svc, svc.Ref))
					continue
				}
				pulled = append(pulled, st.hold)
				s.say(report.InfoLevel, "%s: pulled %s again by its digest", svc.Name, st.hold)
			}
			e := s.entryOf(svc, st.hold)
			e.Kind, e.Digest = st.kind, st.hold
			es = append(es, e)
			// No hold needed while the name still holds the image.
			if cur, err := s.Docker.ID(svc.Ref); st.kind == KindData || err != nil || cur != e.ID {
				holds[svc.Name] = st.hold
			}
			if st.kind == KindData && haveOld && o.ID != "" && o.ID != e.ID {
				lines = append(lines, fmt.Sprintf("%s stays on %s, the newest image its data ran, not %s's (%s): data only goes forward", svc.Name, shownEntry(e), tid, shownEntry(o)))
			}
		default:
			if _, err := s.Docker.ID(svc.Ref); errors.Is(err, ErrNoImage) {
				s.say(report.InfoLevel, "%s: %s is gone from this server: pulling it", svc.Name, svc.Ref)
				if err := s.Docker.Pull(svc.Ref); err != nil {
					s.say(report.ErrorLevel, "%s: could not pull %s: %v", svc.Name, svc.Ref, err)
					return err
				}
			}
			e := s.entryOf(svc, svc.Ref)
			if st.kind == KindData {
				e.Kind = KindData
			}
			es = append(es, e)
			if e.Kind != KindOwn && haveOld && o.ID != "" && o.ID != e.ID {
				lines = append(lines, fmt.Sprintf("%s stays on %s's current image (%s), not %s's (%s): its name did not change", svc.Name, svc.Ref, shownEntry(e), tid, shownEntry(o)))
			}
		}
	}
	if err := s.keep(target, es); err != nil {
		s.say(report.ErrorLevel, "could not record the images of %s: %v", tid, err)
		return err
	}
	if err := writeCompose(target, es, holds); err != nil {
		s.say(report.ErrorLevel, "could not write %s's %s: %v", tid, ComposeFile, err)
		return err
	}
	if len(lines) > 0 {
		s.sayAll(report.InfoLevel, "images of "+tid+" restored, but for what does not go back:", lines)
	} else {
		s.say(report.InfoLevel, "images of %s restored", tid)
	}
	return nil
}

func (s *Set) gone(tid string, svc Service) error {
	s.say(report.InfoLevel, "%s's image of %s is gone: building it again", tid, svc.Name)
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

// HoldData holds rel's pulled data services on the newest image their
// data ran (the history's last), in its ComposeFile, when nothing else
// could restore its images (the restart after a failed run whose fetch
// failed): what its last activation held may be older. An image gone is
// pulled by compose's up, by its digest.
func (s *Set) HoldData(base, rel string) error {
	h := readHistory(base)
	holds, _ := composeFacts(rel)
	if holds == nil {
		holds = map[string]string{}
	}
	es := readList(rel)
	changed := false
	for i, e := range es {
		last, ok := h.last(e.Service)
		if e.Kind != KindData || !ok || last.hold() == "" || holds[e.Service] == last.hold() {
			continue
		}
		holds[e.Service] = last.hold()
		es[i].Digest, es[i].ID = last.hold(), last.ID
		changed = true
	}
	if !changed {
		return nil
	}
	return writeCompose(rel, es, holds)
}

// RestoreOwn puts a release's own images back on their names, after a
// release's fetch failed or was cut off (Undo, Recover): a build moved
// them, and a plain up in the live release would run what the failed
// one built. Pulled names stay where the fetch left them: devopsy never
// moves a pulled name back (another environment may run it), and the
// live release's ComposeFile holds what must not move.
func (s *Set) RestoreOwn(rel string) {
	id := filepath.Base(rel)
	es := readList(rel)
	if len(es) == 0 {
		return
	}
	builds := map[string]bool{}
	if svcs, err := s.List(rel); err == nil {
		for _, svc := range svcs {
			builds[familiar(svc.Ref)] = svc.own(s.Mode)
		}
	}
	names := bookmarks(es, s.Name, id)
	own := s.own(rel)
	for i, e := range es {
		isOwn := e.Kind == KindOwn
		if e.Kind == "" {
			b, ok := builds[familiar(e.Ref)]
			isOwn = !ok || b
		}
		if !isOwn || e.ID == "" || strings.Contains(e.Ref, "@") {
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

func haveHistory(h history, svc string) bool {
	_, ok := h.last(svc)
	return ok
}
