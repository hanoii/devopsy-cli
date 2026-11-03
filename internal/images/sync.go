package images

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// SyncImagesEnv carries a parent's data's windows (ParentData, base64)
// to the child's side of a sync, SyncImagesSumEnv their checksum to the
// parent's push, which refuses when its data's windows changed since (a
// release of the parent between the preflight and the push).
const (
	SyncImagesEnv    = "DEVOPSY_INTERNAL_SYNC_IMAGES"
	SyncImagesSumEnv = "DEVOPSY_INTERNAL_SYNC_IMAGES_SUM"
)

// ParentData is what a child's sync needs of a parent's data: per data
// service whose data the copy writes (devopsy.data.copy volumes or
// custom, as the parent's config resolves it now, any included: a dump
// at the parent's version is data of that version; a volumes service
// whose entries are all missing here, which the copy writes nothing for,
// left out), the window of its history (window), which the child checks
// its own version and tag against and takes over with the data (rule,
// SyncCheck, SyncDone). Lines, tab separated: "data" and the service,
// then "ran" and a history line, per line of its window.
func (s *Set) ParentData(base, rel string) string {
	return s.parentData(base, rel, "", false)
}

// SnapshotData is ParentData of every data service whatever its copy
// label (a snapshot takes them all as files), for a snapshot's manifest:
// the window a --no-code restore checks the current release against, and
// that a restore brings back with the data.
func (s *Set) SnapshotData(base, rel string) string {
	return s.parentData(base, rel, "", true)
}

// parentData is ParentData, of the service only when only is set, of
// every data service with all.
func (s *Set) parentData(base, rel, only string, all bool) string {
	h := readHistory(base)
	svcs, err := s.List(rel)
	if err != nil {
		s.say(report.WarnLevel, "could not list the services of %s (%v): the child keeps its history", filepath.Base(rel), err)
		return ""
	}
	var copied []string
	for _, svc := range svcs {
		if !svc.Data || (only != "" && svc.Name != only) {
			continue
		}
		switch {
		case all:
		case svc.Method == CopyCustom:
		case svc.Method == CopyVolumes:
			// A service whose entries are all missing here has nothing a
			// copy writes: the child keeps its own data and history. A
			// named volume counts as there.
			if svc.EntriesKnown && !hasEntry(rel, svc.Entries) && len(svc.Volumes) == 0 {
				continue
			}
		default:
			continue
		}
		copied = append(copied, svc.Name)
	}
	// Each service's newest lines only, ParentHistoryLines in all (10 at
	// least each): the parent's data rides in a variable inlined in the
	// child's script, one argument, which Linux caps at 128KB.
	lines := ParentHistoryLines
	if len(copied) > 0 {
		lines = max(10, ParentHistoryLines/len(copied))
	}
	var b strings.Builder
	for _, svc := range copied {
		b.WriteString("data\t" + svc + "\n")
		for _, r := range compact(h.window(svc), lines) {
			b.WriteString("ran\t" + r.line())
		}
	}
	return b.String()
}

// ParentHistoryLines is how many lines of their windows, in all,
// ParentData carries for the services it lists (10 at least each), cut
// so the rule reads them as it reads the whole (compact).
const ParentHistoryLines = 100

// compact cuts a window to n lines, in order, keeping what the rule
// reads: its first line (what started it), its newest line at its
// highest version (the version and the tag there), then the newest.
func compact(w []ran, n int) []ran {
	if len(w) <= n {
		return w
	}
	keep := map[int]bool{0: true}
	if dv := windowVersion(w); dv != "" {
		at := 0
		for i, r := range w {
			if IsVersion(r.Version) && CompareVersions(r.Version, dv) == 0 {
				at = i
			}
		}
		keep[at] = true
	}
	for i := len(w) - 1; i >= 0 && len(keep) < n; i-- {
		keep[i] = true
	}
	var out []ran
	for i, r := range w {
		if keep[i] {
			out = append(out, r)
		}
	}
	return out
}

// Sum is a ParentData's checksum.
func Sum(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// EncodeData and DecodeData carry a ParentData in a variable.
func EncodeData(data string) string { return base64.StdEncoding.EncodeToString([]byte(data)) }

func DecodeData(v string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(v)
	return string(b), err
}

// ExportKey is the PAX record a volumes export's tar carries its
// service's data's window in (ExportData): in one global header, before
// the members, which tar itself skips.
const ExportKey = "DEVOPSY.data-images"

// ExportData is what a volumes export of service records of its data, in
// ParentData's lines, for an import to check and take over as a sync does
// the parent's (ImportCheck, ImportDone): on a target (rel, its current
// release) what a sync's parent would send, its window; locally (rel "")
// no history, its label's version and its tag alone, as one line. Empty
// when there is nothing to record (no data service of that name).
func (s *Set) ExportData(base, rel, service string) string {
	if rel != "" {
		return s.parentData(base, rel, service, false)
	}
	if s.List == nil {
		return ""
	}
	svcs, err := s.List("")
	if err != nil {
		return ""
	}
	for _, svc := range svcs {
		if svc.Name != service || !svc.Data {
			continue
		}
		e := entry{Ref: svc.Ref, Service: svc.Name, DataVersion: svc.DataVersion, Tag: svc.tag()}
		img := Image{}
		if c, ok := s.containers()[service]; ok && c.started() {
			img, _ = s.Docker.Inspect(c.Image)
		}
		return "data\t" + service + "\nran\t" + ranOn(e, Ran, img, s.now()).line()
	}
	return ""
}

// DataFor is the part of an export's data that is service's: "" when it
// has none for it (another service's export, none recorded), so the
// import starts the history over.
func DataFor(data, service string) string {
	var b strings.Builder
	found := false
	for _, line := range strings.SplitAfter(data, "\n") {
		kind, rest, _ := strings.Cut(line, "\t")
		f := strings.Split(strings.TrimRight(rest, "\n"), "\t")
		switch {
		case kind == "data" && f[0] == service:
			found = true
			b.WriteString(line)
		case kind == "ran" && len(f) > 1 && f[1] == service:
			b.WriteString(line)
		}
	}
	if !found {
		return ""
	}
	return b.String()
}

// sourceData is a service's part of a ParentData: the window of the
// source's history.
type sourceData struct {
	Service string
	History history
}

// window is the source's window for its service.
func (p *sourceData) window() []ran { return p.History.window(p.Service) }

func parseParentData(data string) map[string]*sourceData {
	ps := map[string]*sourceData{}
	for _, line := range strings.Split(data, "\n") {
		kind, rest, _ := strings.Cut(line, "\t")
		switch kind {
		case "data":
			if rest != "" {
				ps[rest] = &sourceData{Service: rest}
			}
		case "ran":
			for _, r := range parseHistory(rest) {
				if p := ps[r.Service]; p != nil {
					p.History = append(p.History, r)
				}
			}
		}
	}
	return ps
}

// SyncCheck checks, before a sync stops anything, each of the child's
// data services the parent copies (rel, the child's current release, its
// version and tag as its labels give them now, claims) against the
// parent's window (rule): the
// copy replaces the child's data, which then has the parent's version.
// Refused: ErrRefused, said, nothing changed; --accept-image lets a
// refused one through, its history starting over (SyncDone). The child
// always runs its own image on the copy.
func (s *Set) SyncCheck(rel, data string) error {
	return s.copyCheck(rel, data, checkWords{source: "the parent's", what: "the child would run the parent's data on an image its version does not allow", nothing: "nothing changed", flag: "--sync"}, true, nil)
}

// ImportCheck is SyncCheck for an import of one service's data, the
// export's (ExportData, DataFor) in place of a parent's, before the
// import stops anything.
func (s *Set) ImportCheck(rel, data, service string) error {
	return s.copyCheck(rel, data, checkWords{source: "the export's", what: "this environment would run the export's data on an image its version does not allow", nothing: "nothing imported", flag: "--import " + service, input: " < <export>"}, false, nil)
}

// RestoreCheck is SyncCheck for a --no-code restore of snapshot, its
// data (SnapshotData) against the current release (rel), before anything
// stops.
//
// A service held on another release's image (the restart's, a full
// restore's) that the current release's own label and tag allow on the
// snapshot's data is judged by them: the data step runs it on its own
// image again (Unhold), whose build, when the project builds it, must be
// there (a --no-code restore builds nothing: refused, the full restore
// the way, which builds).
func (s *Set) RestoreCheck(rel, data, snapshot string) error {
	ps := parseParentData(data)
	h := readHistory(baseOf(rel))
	drop := s.unholdable(rel, func(svc string) []ran {
		if p := ps[svc]; p != nil {
			return p.window()
		}
		return h.window(svc)
	})
	mode := ModeOf(rel)
	var gone []string
	for _, e := range readList(rel) {
		svc, ok := drop[e.Service]
		if !ok || !svc.own(mode) {
			continue
		}
		if _, err := s.Docker.ID(e.OwnID); e.OwnID == "" || err != nil {
			gone = append(gone, svc.Name+": the current release's own image of it, built by the project, is gone from this server, and the image it is held on ("+shownData(e)+") would upgrade the snapshot's data again")
		}
	}
	if len(gone) > 0 {
		s.sayAll(report.ErrorLevel, "a --no-code restore builds nothing: nothing restored", gone,
			report.Fix("restore it with its code, which builds its images:", s.cmd("--snapshots restore "+snapshot)))
		return ErrRefused
	}
	return s.copyCheck(rel, data, checkWords{source: "the snapshot's", what: "the current release would run the snapshot's data on an image its version does not allow", nothing: "nothing restored", flag: "--snapshots restore " + snapshot + " --no-code"}, false, drop)
}

// claims are rel's listing with each data service's version and tag as
// its labels give them now (List: in a sync's holder, with the pending
// inherited variables, InheritPendingEnv, so a label from an inherited
// variable never lags one sync: review), but for a service its
// ComposeFile holds (the restart's, a full restore's), whose held image's
// own version and tag, the listing's, are what runs. Labels that cannot
// be read leave the listing's, said.
func (s *Set) claims(rel string) []entry {
	es := readList(rel)
	if s.List == nil {
		return es
	}
	svcs, err := s.List(rel)
	if err != nil {
		s.say(report.WarnLevel, "could not read the labels of %s (%v): its data services' versions are its listing's", filepath.Base(rel), err)
		return es
	}
	held := heldIn(rel)
	for i, e := range es {
		if e.Service == "" || e.DataVersion == "" || held[e.Service] != "" {
			continue
		}
		for _, svc := range svcs {
			if svc.Name == e.Service && svc.Data {
				es[i].DataVersion, es[i].Tag = svc.DataVersion, svc.tag()
			}
		}
	}
	return es
}

// copyCheck is SyncCheck in w's words: by rel's labels now with labels
// (a sync, whose run step runs under them), else by its listing (what its
// last activation runs: an import and a --no-code restore check no image
// names, so a label changed with its image since would pass), but for the
// services in own, judged by their own label and tag (RestoreCheck).
func (s *Set) copyCheck(rel, data string, w checkWords, labels bool, own map[string]Service) error {
	ps := parseParentData(data)
	var js []judged
	es := readList(rel)
	if labels {
		es = s.claims(rel)
	}
	for _, e := range es {
		if p := ps[e.Service]; p != nil && e.DataVersion != "" {
			// One the data step runs on its own image (Unhold): its own
			// label and tag, which the data allows.
			if svc, ok := own[e.Service]; ok {
				e.DataVersion, e.Tag = svc.DataVersion, svc.tag()
			}
			js = append(js, judged{service: e.Service, why: rule(p.window(), e.DataVersion, e.Tag, w.source)})
		}
	}
	forced, err := s.judge(js, w, rel, false)
	for _, svc := range forced {
		s.say(report.WarnLevel, "%s: accepted (--accept-image): it runs its own image on the %s data, its history starting over from it", svc, strings.TrimPrefix(strings.TrimSuffix(w.source, "'s"), "the "))
	}
	return err
}

// SyncDone takes over the parent's data's history once a sync's copy is
// complete: each data service the parent copied gets the parent's window
// in place of its own, its first line "sync <parent>" (or one line of no
// version when the parent's has none: the next release adopts its label),
// or, refused and let through by --accept-image, one Forced line of its
// own image. from is the parent's address. What runs is the child's own
// image, untouched.
func (s *Set) SyncDone(base, rel, data, from string) error {
	return s.syncDone(base, rel, data, SyncedFrom+from, "the parent's", true)
}

// ImportDone is SyncDone for a complete import of the data an export
// recorded (ImportCheck), or a restore's of a snapshot's: the window the
// export's, first line Imported, timed at the import (--resolved keeps a
// history whose first line came since the import's mark).
func (s *Set) ImportDone(base, rel, data string) error {
	return s.syncDone(base, rel, data, Imported, "the export's", false)
}

// syncDone is SyncDone, the window's first line how first. With labels
// (a sync), the listing takes the data services' versions and tags as the
// labels give them now (claims): what img ran then records, so a label
// from an inherited variable is the copy's version from this sync on.
func (s *Set) syncDone(base, rel, data, first, source string, labels bool) error {
	ps := parseParentData(data)
	h := readHistory(base)
	changed := false
	var cs map[string]Container
	es := readList(rel)
	if labels {
		es = s.claims(rel)
		if old := readList(rel); !slices.Equal(old, es) {
			if err := writeList(rel, es); err != nil {
				s.say(report.ErrorLevel, "could not record the data's versions in %s's listing: %v", filepath.Base(rel), err)
				return err
			}
		}
	}
	for _, e := range es {
		p := ps[e.Service]
		if p == nil || e.DataVersion == "" {
			continue
		}
		w := append(history{}, p.window()...)
		if s.Accept[e.Service] && rule(w, e.DataVersion, e.Tag, source) != "" {
			// Its own image on the copy, accepted: its history over from it.
			if cs == nil {
				cs = s.containers()
			}
			img := Image{ID: e.ID}
			if c, ok := cs[e.Service]; ok && c.started() {
				if i, err := s.Docker.Inspect(c.Image); err == nil {
					img = i
				}
			} else if i, err := s.Docker.Inspect(e.ID); err == nil {
				img = i
			}
			h = append(h.without(e.Service), ranOn(e, Forced, img, s.now()))
			changed = true
			continue
		}
		if len(w) == 0 {
			w = history{{Service: e.Service, Ref: e.Ref}}
		}
		w[0].How = first
		if first == Imported || len(w) == 1 && w[0].Time == 0 {
			w[0].Time = s.now().Unix()
		}
		h = append(h.without(e.Service), w...)
		changed = true
	}
	if !changed {
		return nil
	}
	if err := h.write(base); err != nil {
		s.say(report.ErrorLevel, "could not take over %s history of what ran on the data: %v", source, err)
		return err
	}
	return nil
}

// Recreate are the services of rel whose container its activation gave
// another image than rel's ComposeFile now gives it (another
// activation's, the restart's hold). Started with compose start, they
// would run the old image on the data, the finish hook first.
func Recreate(d Docker, project, rel string) []string {
	labels := labelsIn(rel)
	cs, err := d.Containers(project)
	if err != nil {
		return nil
	}
	var svcs []string
	seen := map[string]bool{}
	for _, c := range cs {
		if c.OneOff || seen[c.Service] {
			continue
		}
		seen[c.Service] = true
		if l := labels[c.Service]; l != "" && c.Digest != l {
			svcs = append(svcs, c.Service)
		}
	}
	return svcs
}

// Imported starts a service's history over, once a complete import of
// its data without a record of it (a custom one, a tar without the
// header, a local one) or --resolved is in: the data came from elsewhere,
// of no known version, so the next release adopts its label.
func (s *Set) Imported(base, rel, service string) error {
	e, ok := match(readList(rel), entry{Service: service})
	if !ok || e.DataVersion == "" {
		return nil
	}
	img := Image{ID: e.ID}
	if c, ok := s.containers()[service]; ok {
		if i, err := s.Docker.Inspect(c.Image); err == nil {
			img = i
		}
	}
	h := readHistory(base)
	r := ranOn(e, Imported, img, s.now())
	r.How, r.Version, r.Tag = Imported, "", ""
	return append(h, r).write(base)
}

// Wiped starts every data service's history over, once a reset wiped
// the environment's data (every entry and volume): the data each
// described is gone, the ones the copy writes nothing for included (a
// devopsy.data.copy=none service, one whose entries are all missing on
// the parent), so the next release adopts their labels (review: a reset
// kept their windows, and a later lower label was refused for data that
// no longer existed). The copy's own take over theirs (SyncDone) once it
// is complete.
func (s *Set) Wiped(base, rel string) error {
	h := readHistory(base)
	changed := false
	for _, e := range readList(rel) {
		if e.Service == "" || e.DataVersion == "" {
			continue
		}
		if w := h.window(e.Service); len(w) == 1 && w[0].How == Imported && w[0].Version == "" {
			continue
		}
		r := ranOn(e, Imported, Image{}, s.now())
		r.How, r.Version, r.Tag = Imported, "", ""
		h = append(h, r)
		changed = true
	}
	if !changed {
		return nil
	}
	return h.write(base)
}

// Resolved is Imported for --resolved, since the time of the import's
// mark: nothing when the history's window starts with an import line
// written since (the import took the export's window, and only what came
// after failed: the data is complete, and starting over would forget its
// version), or a forced one (the import was accepted: its history over
// from its own image, which a line of no version would undo, letting the
// next release adopt any label: review), else Imported, as before.
func (s *Set) Resolved(base, rel, service string, since time.Time) error {
	if w := readHistory(base).window(service); len(w) > 0 && (w[0].How == Imported || w[0].How == Forced) && !since.IsZero() && w[0].Time >= since.Unix() {
		return nil
	}
	return s.Imported(base, rel, service)
}

// FilterImported keeps of an imported snapshot's listing (snap's, anyone's
// file) only the entries of a service the current release (cur) lists
// too, in that one's repository: what a later restore's pull, its
// bookmarks (BookmarkFrom) and its removal take names
// from (review: a crafted listing had the server pull and tag another
// repository's image, and its removal strip names of images nothing
// else held).
//
// An entry of a service the current release lists under another
// repository (a snapshot moved from another environment, whose own
// images are named after its compose project, a mirror's name) is kept
// under the current one's repository, its tag, digest and id as they
// were: never dropped, or a full restore of it would find a listing of
// nothing, nor kept elsewhere (review).
func FilterImported(snap, cur string) error {
	repos := map[string]string{}
	for _, e := range readList(cur) {
		if e.Service != "" {
			repos[e.Service] = repoOf(e.Ref)
		}
	}
	var kept []entry
	for _, e := range readList(snap) {
		repo, ok := repos[e.Service]
		if e.Service == "" || !ok {
			continue
		}
		e.Ref = repo + e.Ref[len(repoOf(e.Ref)):]
		if e.Digest != "" {
			e.Digest = repo + e.Digest[len(repoOf(e.Digest)):]
		}
		if e.OwnRef != "" {
			e.OwnRef = repo + e.OwnRef[len(repoOf(e.OwnRef)):]
		}
		// Its id is the other server's word: bookmarked by it, any local
		// image of another repository it names would be tagged into the
		// service's, which Docker's containerd store then resolves by
		// digest there (review round 5). Without, nothing is tagged until
		// a restore pulls it by digest in that repository, or builds it.
		e.ID, e.OwnID = "", ""
		kept = append(kept, e)
	}
	return writeList(snap, kept)
}

// hasEntry says whether any of entries is in rel's mnt (its shared/mnt,
// through the release's link).
func hasEntry(rel string, entries []string) bool {
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(rel, codeDir, ".devopsy", "mnt", e)); err == nil {
			return true
		}
	}
	return false
}
