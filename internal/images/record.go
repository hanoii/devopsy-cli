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
	// <export>", as the README writes an import).
	input string
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
		}
	}
	if len(refused) > 0 {
		// A rollback's code is the older release's: no label to raise there,
		// only a release that fixes forward or the data from before.
		fixes := []report.Remedy{report.Fix("raise " + DataVersionLabel + " to the image's version, when it is newer than the data's (one variable for the image and the label keeps them together)")}
		if w.rollback {
			fixes = []report.Remedy{
				report.Fix("fix forward: a release, which never takes the data back", s.cmd("--release")),
				s.snapshotFix(target)}
		}
		flag := w.flag
		if s.Again != "" {
			flag = s.Again
		}
		fixes = append(fixes, report.Fix("or, knowing its data takes it, accept it, its data's history starting over from it:", s.again(flag, refusedSvcs)+w.input))
		s.sayAll(report.ErrorLevel, w.what+": "+w.nothing, refused, fixes...)
		return nil, ErrRefused
	}
	if len(accepted) > 0 && !quiet {
		s.sayAll(report.WarnLevel, "accepted (--accept-image): running them anyway, the history of their data starting over from them:", accepted)
	}
	return forced, nil
}

// snapshotFix is a rollback's way back to the data before the change:
// the newest release snapshot of target's data (its manifest's release),
// else the snapshots to choose from.
func (s *Set) snapshotFix(target string) report.Remedy {
	tid := filepath.Base(target)
	newest := ""
	if dir := s.snapshots(); dir != "" {
		ms, _ := filepath.Glob(filepath.Join(dir, "*", SnapshotManifest))
		for _, m := range ms {
			data, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			kind, rel := "", ""
			for _, line := range strings.Split(string(data), "\n") {
				k, v, _ := strings.Cut(line, "\t")
				switch k {
				case "kind":
					kind = v
				case "release":
					rel = v
				}
			}
			if id := filepath.Base(filepath.Dir(m)); kind == "release" && rel == tid && id > newest {
				newest = id
			}
		}
	}
	if newest != "" {
		return report.Fix("or restore the snapshot of "+tid+"'s data, taken by the release that replaced it, with its code:", s.cmd("--snapshots restore "+newest))
	}
	return report.Fix("or restore a snapshot from before the change, with its code:", s.cmd("--snapshots"))
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
	parent := parseParentData(s.SyncData)
	var js []judged
	for _, svc := range svcs {
		if !svc.Data {
			continue
		}
		w, source := h.window(svc.Name), "its"
		if p := parent[svc.Name]; p != nil {
			w, source = p.window(), "the parent's"
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
// a service a release that syncs copies, whose sync does that
// (SyncDone).
func (s *Set) Record(rel, prev string) error {
	es, err := s.entries(rel)
	if err != nil {
		s.say(report.ErrorLevel, "could not list the images of %s: %v", filepath.Base(rel), err)
		return err
	}
	old := readList(prev)
	var forced []string
	if len(s.Accept) > 0 {
		h := readHistory(baseOf(rel))
		parent := parseParentData(s.SyncData)
		for _, e := range es {
			if s.Accept[e.Service] && e.DataVersion != "" && parent[e.Service] == nil && rule(h.window(e.Service), e.DataVersion, e.Tag, "its") != "" {
				forced = append(forced, e.Service+"\t"+e.ID)
			}
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
	if err := s.keep(rel, es); err != nil {
		s.say(report.ErrorLevel, "could not record the images of %s: %v", filepath.Base(rel), err)
		return err
	}
	if err := writeCompose(rel, es, nil); err != nil {
		s.say(report.ErrorLevel, "could not write %s's %s: %v", filepath.Base(rel), ComposeFile, err)
		return err
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
// release before: a pulled one by version when both have one, else by
// short digest, an own one rebuilt; and an image the project builds that
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
		lines = append(lines, fmt.Sprintf("%s: %s -> %s", name, shown(o), shown(e)))
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

// again is the command that runs it again accepting svcs: devopsy
// @<address> <flag> --accept-image <svcs>.
func (s *Set) again(flag string, svcs []string) string {
	return s.cmd(flag + " --accept-image " + strings.Join(svcs, ","))
}

// cmd is devopsy @<address> <args>, or args alone without an address.
func (s *Set) cmd(args string) string {
	if s.Address != "" {
		return "devopsy @" + s.Address + " " + args
	}
	return args
}
