package images

import (
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

// judged is one data service as a check judged it: why the rule refused
// it ("" when it allowed it).
type judged struct {
	service, why string
	// version and tag are what it claims (a rollback's, for the snapshot
	// its refusal names: snapshotFix).
	version, tag string
}

// checkWords are what a check says of the data's source and of what it
// stops: a release's own data, a sync's parent, an import's export, a
// rollback's.
type checkWords struct {
	// source is the data's source, possessive ("its", "the parent's").
	source string
	// what is what the refusal says ("the release would run its data on
	// an image its version does not allow"), nothing what it says
	// changed ("nothing changed").
	what, nothing string
	// flag runs it again (--release, --sync, --import <service>,
	// --rollback, --snapshots restore <id> --no-code).
	flag string
	// rollback adds a rollback's ways out: a release that fixes forward,
	// the release snapshot from before the change.
	rollback bool
	// input follows the command run again: an import's data (" <
	// <export>", as docs/images.md writes an import).
	input string
	// builds is what a copy's refusal says when an image its take-over
	// needs is gone (copyNeeds: "a sync builds nothing"), rebuild what
	// builds it.
	builds  string
	rebuild report.Remedy
	// fetch is what fetches an image the labels name and the current
	// release did not (notFetched): a release.
	fetch report.Remedy
}

// judge says what the rule decided over js in w's words: refused (and
// not accepted: ErrRefused, with the ways out), accepted (--accept-image
// lets through only what was refused: the services whose history starts
// over, returned), or an --accept-image naming a service the rule let
// through (said, nothing forced). quiet says only a refusal: a rollback's
// lenient check, whose img restore says the rest once (review: the
// accepted warning came twice).
func (s *Set) judge(js []judged, w checkWords, target string, quiet bool) ([]string, error) {
	var refused, refusedSvcs, accepted, forced []string
	var refusals []judged
	for _, j := range js {
		switch {
		case j.why == "":
			if s.Accept[j.service] && !quiet {
				s.say(report.InfoLevel, "%s: --accept-image lets through only what the check refuses, and it refuses nothing: it runs as it would", j.service)
			}
		case s.Accept[j.service]:
			accepted = append(accepted, j.service+": "+j.why)
			forced = append(forced, j.service)
		default:
			refused = append(refused, j.service+": "+j.why)
			refusedSvcs = append(refusedSvcs, j.service)
			refusals = append(refusals, j)
		}
	}
	if len(refused) > 0 {
		// A rollback's code is the older release's: no label to raise there,
		// only a release that fixes forward or the data from before.
		fixes := []report.Remedy{report.Fix("raise " + DataVersionLabel + " to the image's version, when it is newer than the data's (one variable for the image and the label keeps them together)")}
		if w.rollback {
			fixes = []report.Remedy{
				s.fix("fix forward: a release, which never takes the data back", "--release"),
				s.snapshotFix(refusals)}
		}
		flag := w.flag
		if s.Again != "" {
			flag = s.Again
		}
		// What it accepted too: a line naming only the rest would be refused
		// on these (review: copied lines took turns, none going through).
		fixes = append(fixes, s.fix("knowing its data takes it, accept it, its data's history starting over from it:", again(flag, append(forced, refusedSvcs...))+w.input))
		s.sayAll(report.ErrorLevel, w.what+": "+w.nothing, refused, fixes...)
		return nil, ErrRefused
	}
	if len(accepted) > 0 && !quiet {
		s.sayAll(report.WarnLevel, "accepted (--accept-image): running them anyway, the history of their data starting over from them:", accepted)
	}
	return forced, nil
}

// snapshotFix is a rollback's way back to the data before the change: the
// newest release snapshot whose data allows what the rollback's refusals
// claim (the rule against its windows), so nothing written since it that
// the rollback could run on is lost (review: naming the snapshot of the
// target's own data lost everything written under the release a failed
// one's restart holds), else the snapshots to choose from.
func (s *Set) snapshotFix(refusals []judged) report.Remedy {
	newest := ""
	if dir := s.snapshots(); dir != "" {
		ms, _ := filepath.Glob(filepath.Join(dir, "*", SnapshotManifest))
		for _, m := range ms {
			data, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			kind := ""
			for _, line := range strings.Split(string(data), "\n") {
				if k, v, _ := strings.Cut(line, "\t"); k == "kind" {
					kind = v
				}
			}
			id := filepath.Base(filepath.Dir(m))
			if kind != "release" || id <= newest || strings.HasSuffix(id, ".tmp") {
				continue
			}
			windows, _ := os.ReadFile(filepath.Join(filepath.Dir(m), snapshotDataDir, snapshotImages))
			ps := parseParentData(string(windows))
			allowed := true
			for _, j := range refusals {
				if p := ps[j.service]; p != nil && rule(p.window(), j.version, j.tag, "its") != "" {
					allowed = false
				}
			}
			if allowed {
				newest = id
			}
		}
	}
	if newest != "" {
		return s.fix("restore the newest snapshot of the data from before the change, with its code:", "--snapshots restore "+newest)
	}
	return s.fix("restore a snapshot from before the change, with its code:", "--snapshots")
}

// SnapshotManifest is a snapshot's manifest (remote's), read for the
// release snapshot a rollback's refusal names.
const SnapshotManifest = "manifest"

// Check runs the rule on a new release's data services before anything
// is built or pulled (img check, before the fetch): their versions come
// from the labels and their tags from compose's image:, both known
// before, so a refused release fetches nothing and moves no names. Each
// against its own data's window, or in a release that syncs (SyncData),
// one the parent copies against the parent's window: the copy replaces
// the data. Refused: ErrRefused, said. Accepted services are Record's to
// force, once their images are there.
func (s *Set) Check(rel string) error {
	js, err := s.judgeRelease(rel)
	if err != nil {
		return err
	}
	_, err = s.judge(js, checkWords{source: "its", what: "the release would run its data on an image its version does not allow", nothing: "nothing changed", flag: "--release"}, rel, false)
	return err
}

// judgeRelease is Check's judgement of rel's data services, from its
// compose (List).
func (s *Set) judgeRelease(rel string) ([]judged, error) {
	svcs, err := s.List(rel)
	if err != nil {
		s.say(report.ErrorLevel, "could not list the services of %s: %v", filepath.Base(rel), err)
		return nil, err
	}
	h := readHistory(baseOf(rel))
	l := s.SyncLanding(s.SyncData, "")
	var js []judged
	for _, svc := range svcs {
		if !svc.Data {
			continue
		}
		w, source := h.window(svc.Name), "its"
		if lw, ok := l.window(svc.Name); ok {
			w, source = lw, l.words.source
		}
		js = append(js, judged{service: svc.Name, why: rule(w, svc.DataVersion, svc.tag(), source)})
	}
	return js, nil
}

// Record writes what a new release runs, once its images are fetched:
// the listing (ListFile, with each image's digest and version, and each
// data service's version and tag), the bookmarks and its ComposeFile,
// which labels each service with the image it was given. prev is the
// release it replaces ("" for a first one): its listing says what moved.
// Check came before the fetch; what it let through by --accept-image
// starts its data's history over once it runs (ForcedFile, Ran), but for
// a service a release that syncs copies, whose landing does that (Land).
func (s *Set) Record(rel, prev string) (err error) {
	// Adopted, the fetch's tags go; failed, the undo bookmarks by them first.
	defer func() {
		if err == nil {
			s.dropFetch(rel)
		}
	}()
	es, err := s.entries(rel, true)
	if err != nil {
		s.say(report.ErrorLevel, "could not list the images of %s: %v", filepath.Base(rel), err)
		return err
	}
	old := readList(prev)
	h := readHistory(baseOf(rel))
	l := s.SyncLanding(s.SyncData, "")
	// What starts its history over at this release's run: what
	// --accept-image let through, a service the parent copies by the
	// sync's own landing.
	over := map[string]bool{}
	var forced []string
	for _, e := range es {
		if !s.Accept[e.Service] || e.DataVersion == "" {
			continue
		}
		if w, ok := l.window(e.Service); ok {
			over[e.Service] = rule(w, e.DataVersion, e.Tag, l.words.source) != ""
		} else if rule(h.window(e.Service), e.DataVersion, e.Tag, "its") != "" {
			over[e.Service] = true
			forced = append(forced, e.Service+"\t"+e.ID)
		}
	}
	if err := writeForced(rel, forced); err != nil {
		s.say(report.ErrorLevel, "could not record what --accept-image let through: %v", err)
		return err
	}
	mode := ModeOf(rel)
	if prev != "" {
		s.moved(old, es, mode)
	}
	s.stale(es, old, mode)
	// Its holds, derived against the window its data has when it runs (a
	// copied service's the parent's): none, Check having refused or
	// accepted it before, which this agrees with.
	after := l.after(h.window)
	held := holdsFor(es, func(svc string) []ran {
		if over[svc] {
			return nil
		}
		return after(svc)
	})
	holds, lines, err := s.applyHolds(rel, es, held, nil)
	if err != nil {
		return err
	}
	if err := s.writeHeld(rel, es, holds); err != nil {
		return err
	}
	if len(lines) > 0 {
		s.sayAll(report.WarnLevel, "images of "+filepath.Base(rel)+" recorded, but for what its data does not allow:", lines)
	}
	return nil
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
// release before: a pulled one by version when both have one, with its
// short digests when the version is the same, else by short digest, an
// own one rebuilt; and an image the project builds that
// changed under the same name in image mode (mode: the new release's),
// which no rollback takes back.
func (s *Set) moved(old, es []entry, mode string) {
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
		change := shown(o) + " -> " + shown(e)
		if shown(o) == shown(e) {
			// A new build of the same version: its digests say it.
			change = shown(e) + " (" + shortDigest(o) + " -> " + shortDigest(e) + ")"
		}
		lines = append(lines, name+": "+change)
		if mode == ModeImage && s.builds(e) && familiar(o.Ref) == familiar(e.Ref) {
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
	return shortDigest(e)
}

// shortDigest is an entry's digest, else its id, short.
func shortDigest(e entry) string {
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

// stale says when an image the project builds and pulls (image mode:
// the new release's) is the release before's, unchanged for StaleAfter:
// a rebuild would take its base's patches.
func (s *Set) stale(es, old []entry, mode string) {
	if mode != ModeImage {
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

// again is the arguments that run it again accepting svcs: <flag> --accept-image <svcs>.
func again(flag string, svcs []string) string {
	return flag + " --accept-image " + strings.Join(svcs, ",")
}

// fix is a fix offering devopsy @<address> <args> under text, or, without an address, text
// naming args in words: never args alone as a command, which copied fails.
func (s *Set) fix(text, args string) report.Remedy {
	if s.Address != "" {
		return report.Fix(text, "devopsy @"+s.Address+" "+args)
	}
	return report.Fix(strings.TrimSuffix(text, ":") + " (" + args + " on this environment)")
}
