package images

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
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
// service whose files the copy writes (devopsy.data.copy=volumes, as the
// parent's config resolves it now, any included; one whose entries are
// all missing here, which the copy writes nothing for, left out; never a
// custom one, as for a custom import: a dump is loaded by the image that
// runs, so the data's files are that image's, never the parent's
// version, and the child's window and hold stay as they were), the
// window of its history (window), which the child checks
// its own version and tag against and takes over with the data (rule,
// CheckLanding, Land). Lines, tab separated: "data" and the service,
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
	svcs, err := s.written(rel)
	if err != nil {
		s.say(report.WarnLevel, "could not list the services of %s (%v): the child keeps its history", filepath.Base(rel), err)
		return ""
	}
	var copied []string
	for _, svc := range svcs {
		if !svc.Data || (only != "" && svc.Name != only) {
			continue
		}
		// A service with none of its entries or volumes here has nothing a
		// copy or a take writes, as they go by what is there: the receiving
		// side keeps its own data and history (review: a profile never up
		// gave the child's data its label's version, a restore forgot what a
		// volume it never held had run).
		if (all || svc.Method == CopyVolumes) && s.hasData(rel, svc) {
			copied = append(copied, svc.Name)
		}
	}
	// Each service's newest lines only, ParentHistoryLines in all (10 at
	// least each): the parent's data rides in a variable inlined in the
	// child's script, one argument, which Linux caps at 128KB.
	lines := ParentHistoryLines
	if len(copied) > 0 {
		lines = max(10, ParentHistoryLines/len(copied))
	}
	labels := map[string]Service{}
	for _, svc := range svcs {
		labels[svc.Name] = svc
	}
	var b strings.Builder
	for _, svc := range copied {
		b.WriteString("data\t" + svc + "\n")
		w := h.window(svc)
		if len(w) == 0 {
			// No history yet: what its label says, as a local export sends
			// (timeless: the push compares its checksum with the preflight's).
			l := labels[svc]
			w = []ran{ranOn(entry{Ref: l.Ref, Service: svc, DataVersion: l.DataVersion, Tag: l.tag()}, Ran, Image{}, time.Unix(0, 0))}
		}
		for _, r := range compact(w, lines) {
			b.WriteString("ran\t" + r.line())
		}
	}
	return b.String()
}

// hasData says whether any of svc's entries or volumes is there.
func (s *Set) hasData(rel string, svc Service) bool {
	if !svc.EntriesKnown || hasEntry(rel, svc.Entries) {
		return true
	}
	for _, key := range svc.Volumes {
		// A data service's volume is its compose project's, by its key
		// (external: and a fixed name: are refused on one).
		if s.Docker.VolumeExists(s.Name + "_" + key) {
			return true
		}
	}
	return false
}

// written are the services a copy of rel writes: every profile's, as the
// rsync, a take's tars and an export go by (cli.SyncServices), or a
// window would leave out data that lands with its files (review: a profile
// off there left the child's old window on the parent's data, and a restore
// forgot the data's version).
func (s *Set) written(rel string) ([]Service, error) {
	if s.ListAll != nil {
		return s.ListAll(rel)
	}
	return s.List(rel)
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
// the parent's (ImportLanding): on a target (rel, its current
// release) what a sync's parent would send, its window; locally (rel "")
// no history, its label's version and its tag alone, as one line. Empty
// when there is nothing to record (no data service of that name).
func (s *Set) ExportData(base, rel, service string) string {
	if rel != "" {
		return s.parentData(base, rel, service, false)
	}
	if s.List == nil && s.ListAll == nil {
		return ""
	}
	svcs, err := s.written("")
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
		if e.OwnDigest != "" {
			e.OwnDigest = repo + e.OwnDigest[len(repoOf(e.OwnDigest)):]
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
