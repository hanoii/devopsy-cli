package images

import (
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// SyncImagesEnv carries a parent's data images (ParentData, base64) to
// the child's side of a sync, SyncImagesSumEnv their checksum to the
// parent's push, which refuses when its data images changed since (a
// release of the parent between the preflight and the push).
const (
	SyncImagesEnv    = "DEVOPSY_INTERNAL_SYNC_IMAGES"
	SyncImagesSumEnv = "DEVOPSY_INTERNAL_SYNC_IMAGES_SUM"
)

// ParentData is what a child's sync needs of a parent's data images: per
// pulled data service whose files a sync copies (devopsy.data.copy=volumes,
// as the parent's config resolves it now: a custom copy goes through the child's
// own image, which keeps its history), the image that wrote its data (its container's,
// once started: a parent that drifted runs what wrote it; else its
// history's last), then the history that counts for it (its window).
// Lines, tab separated: "image", service, reference, id, digests; then
// "ran" and a history line.
func (s *Set) ParentData(base, rel string) string {
	return s.parentData(base, rel, "", false)
}

// SnapshotData is ParentData of every pulled data service whatever its
// copy label (a snapshot takes them all as files), for a snapshot's
// manifest: what a restore holds each on.
func (s *Set) SnapshotData(base, rel string) string {
	return s.parentData(base, rel, "", true)
}

// parentData is ParentData, of the service only when only is set, of
// every data service with all.
func (s *Set) parentData(base, rel, only string, all bool) string {
	h := readHistory(base)
	var cs map[string]Container
	var b strings.Builder
	svcs, err := s.List(rel)
	if err != nil {
		s.say(report.WarnLevel, "could not list the services of %s (%v): the child keeps its images", filepath.Base(rel), err)
		return ""
	}
	copied := map[string]bool{}
	pulled := false
	for _, svc := range svcs {
		// A service whose entries are all missing here has nothing a
		// copy writes: the child keeps its own data, and its image.
		if only != "" && svc.Name != only {
			continue
		}
		// A named volume counts as there: one a service never ran with
		// leaves no image to send anyway.
		copied[svc.Name] = (all && svc.Data || svc.Method == CopyVolumes) && (!svc.EntriesKnown || hasEntry(rel, svc.Entries) || len(svc.Volumes) > 0)
		pulled = pulled || (copied[svc.Name] && !svc.Build && !svc.own(s.Mode))
	}
	es := readList(rel)
	// A release an older devopsy recorded names no services: which one
	// runs a database cannot be told, so the child's side refuses.
	if pulled && len(es) > 0 && es[0].Service == "" {
		return "old\t" + filepath.Base(rel) + "\n"
	}
	var data []entry
	for _, e := range es {
		if e.Kind == KindData && copied[e.Service] {
			data = append(data, e)
		}
	}
	// Each service's newest lines only, ParentHistoryLines in all (10 at
	// least each): the parent's images ride in a variable inlined in the
	// child's script, one argument, which Linux caps at 128KB.
	lines := ParentHistoryLines
	if len(data) > 0 {
		lines = max(10, ParentHistoryLines/len(data))
	}
	for _, e := range data {
		img := Image{}
		ref := e.Ref
		if cs == nil {
			cs = s.containers()
		}
		if c, ok := cs[e.Service]; ok && c.started() {
			if i, err := s.Docker.Inspect(c.Image); err == nil {
				img = i
			}
		}
		if img.ID == "" {
			if last, ok := h.last(e.Service); ok {
				img = Image{ID: last.ID, Digests: last.Digests}
				ref = last.Ref
			} else if i, err := s.Docker.Inspect(e.ID); err == nil {
				img = i
			}
		}
		if img.ID == "" {
			continue
		}
		fmt.Fprintf(&b, "image\t%s\t%s\t%s\t%s\n", e.Service, ref, img.ID, strings.Join(img.Digests, " "))
		w := h.window(e.Service)
		if len(w) > lines {
			w = w[len(w)-lines:]
		}
		for _, r := range w {
			b.WriteString("ran\t" + r.line())
		}
	}
	return b.String()
}

// ParentHistoryLines is how many of their newest history lines, in all,
// ParentData carries for the services it lists (10 at least each): an
// image that ran before them is not known as one the parent's data moved
// past, only as one it never ran under its name (refused the same, but
// for a name it ran under).
const ParentHistoryLines = 100

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

// ExportKey is the PAX record an mnt export's tar carries its service's
// data images in (ExportData): in one global header, before the
// members, which tar itself skips.
const ExportKey = "DEVOPSY.data-images"

// ExportData is what an mnt export of service records of the images
// that ran on its data, in ParentData's lines, for an import to check
// as a sync checks the parent's (ImportCheck): on a target (rel, its
// current release) what a sync's parent would send; locally (rel "")
// the image its started container runs, when that has a registry
// digest, with no history. Empty when there is nothing to record (a
// built or custom service, no container, a release an older devopsy
// recorded, which names no services).
func (s *Set) ExportData(base, rel, service string) string {
	if rel != "" {
		data := s.parentData(base, rel, service, false)
		if strings.HasPrefix(data, "old\t") {
			s.say(report.WarnLevel, "%s: release %s predates devopsy's record of each service's image: the export carries no image, and an import starts its history over", service, filepath.Base(rel))
			return ""
		}
		return data
	}
	// Only a pulled data service: a built one's digest, which Docker's
	// containerd store gives built images too, is a local build's, which
	// no registry has.
	if s.List == nil {
		return ""
	}
	svcs, err := s.List("")
	if err != nil {
		return ""
	}
	pulled := false
	for _, svc := range svcs {
		pulled = pulled || (svc.Name == service && svc.Method == CopyVolumes && !svc.own(s.Mode))
	}
	if !pulled {
		return ""
	}
	c, ok := s.containers()[service]
	if !ok || !c.started() || c.Ref == "" {
		return ""
	}
	img, err := s.Docker.Inspect(c.Image)
	if err != nil || len(img.Digests) == 0 {
		return ""
	}
	return fmt.Sprintf("image\t%s\t%s\t%s\t%s\n", service, c.Ref, img.ID, strings.Join(img.Digests, " "))
}

// hasDigest says whether digests, space separated, has a registry digest
// (<repository>@sha256:...), not only names.
func hasDigest(digests string) bool {
	for _, d := range strings.Fields(digests) {
		if digestOf(d) != "" {
			return true
		}
	}
	return false
}

// DataFor is the part of an export's data that is service's: "" when it
// has no complete image line for it (another service's export, none
// recorded, a line without its reference, id or a digest, which nothing
// could check or hold), so the import starts the history over.
func DataFor(data, service string) string {
	var b strings.Builder
	found := false
	for _, line := range strings.SplitAfter(data, "\n") {
		kind, rest, _ := strings.Cut(line, "\t")
		f := strings.Split(rest, "\t")
		switch {
		case kind == "image" && f[0] == service && len(f) > 3 && f[1] != "" && f[2] != "" && hasDigest(f[3]):
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

// parentImage is one image line of a ParentData, with the parent's
// history for its service.
type parentImage struct {
	Service, Ref string
	Image        Image
	History      history
}

func parseParentData(data string) map[string]*parentImage {
	ps := map[string]*parentImage{}
	for _, line := range strings.Split(data, "\n") {
		kind, rest, _ := strings.Cut(line, "\t")
		switch kind {
		case "old":
			ps[""] = &parentImage{Ref: rest}
		case "image":
			f := strings.Split(rest, "\t")
			if len(f) < 3 {
				continue
			}
			p := &parentImage{Service: f[0], Ref: f[1], Image: Image{ID: f[2]}}
			// Registry digests only: a name in their place (a record made
			// by hand) is nothing to hold by.
			if len(f) > 3 {
				for _, d := range strings.Fields(f[3]) {
					if digestOf(d) != "" {
						p.Image.Digests = append(p.Image.Digests, d)
					}
				}
			}
			ps[p.Service] = p
		case "ran":
			for _, r := range readHistoryText(rest) {
				if p := ps[r.Service]; p != nil {
					p.History = append(p.History, r)
				}
			}
		}
	}
	return ps
}

// hold is the parent's image as a name to run.
func (p *parentImage) hold() string {
	return ran{Ref: p.Ref, Digests: p.Image.Digests}.hold()
}

// SyncCheck checks, before a sync stops anything, that the parent's data
// may take each of rel's pulled data services the parent has, and gets
// the parent's image onto this server, bookmarked as rel's, since the
// copy brings data that image wrote:
//
//   - the child's image is the parent's: nothing;
//   - under the same name, another image: it will be held on the
//     parent's;
//   - an image the parent's data moved past, or a name the parent's data
//     never ran: refused, unless yes (every sync).
//
// The parent's image is pulled by its digest when missing (never trusted
// from a local name, which may be anything), refused when it cannot be.
func (s *Set) SyncCheck(rel, data string) error {
	_, err := s.syncCheck(rel, data, "--sync")
	return err
}

// ImportCheck is SyncCheck for an import of one service's data, the
// export's (ExportData, DataFor) in place of a parent's, before the
// import stops anything. The export's data comes from whoever made the
// tar, so it never chooses what runs: its image is held only by a digest
// in the repository of the service's own reference, and one in another
// repository is a name the data never ran (refused, unless accepted).
func (s *Set) ImportCheck(rel, data, service string) error {
	_, err := s.check(rel, data, "--import "+service, importWords)
	return err
}

// checkWords are what a check says of the data's source and of where it
// lands: a sync's parent and child, an import's export and environment.
type checkWords struct {
	importing bool
	// source is the data's source, possessive ("the parent's"); nothing
	// what is said when it refuses ("nothing changed").
	source, nothing string
}

var (
	syncWords   = checkWords{source: "the parent's", nothing: "nothing changed"}
	importWords = checkWords{importing: true, source: "the export's", nothing: "nothing imported"}
)

// syncCheck is SyncCheck, returning the names of the images it refused;
// again is the flag that runs it again (--sync, --release).
func (s *Set) syncCheck(rel, data, again string) ([]string, error) {
	return s.check(rel, data, again, syncWords)
}

// check is syncCheck in w's words.
func (s *Set) check(rel, data, again string, w checkWords) ([]string, error) {
	ps := parseParentData(data)
	es := readList(rel)
	// A release recorded by an older devopsy names no services: which
	// one runs a database cannot be told, so its image could not follow
	// the data. Released again first.
	if old := ps[""]; old != nil {
		s.sayAll(report.ErrorLevel, "the parent's release "+old.Ref+" predates devopsy's record of each service's image: nothing changed", nil,
			report.Fix("release the parent first, then sync"))
		return nil, ErrRefused
	}
	if len(ps) > 0 && len(es) > 0 && es[0].Service == "" {
		if w.importing {
			s.sayAll(report.ErrorLevel, "release "+filepath.Base(rel)+" predates devopsy's record of each service's image: nothing imported", nil,
				report.Fix("release it again first, then import:", "devopsy @"+cmp.Or(s.Address, "<target>")+" --release"))
			return nil, ErrRefused
		}
		s.sayAll(report.ErrorLevel, "the child's release "+filepath.Base(rel)+" predates devopsy's record of each service's image: nothing changed", nil,
			report.Fix("release the child first, with the sync if you want it:", "devopsy @"+cmp.Or(s.Address, "<child>")+" --release-sync"))
		return nil, ErrRefused
	}
	var refused, refusedSvcs, refusedRefs []string
	var mine []entry
	for _, e := range es {
		p := ps[e.Service]
		if e.Kind != KindData || p == nil {
			continue
		}
		// Accepted: the child runs its own image on the parent's data
		// (SyncDone), nothing of the parent's to check or pull.
		if s.Accept[e.Service] {
			data := "the parent's data"
			if w.importing {
				data = "the imported data"
			}
			s.say(report.WarnLevel, "%s: accepted (--accept-image): it runs its own image, %s, on %s, its history starting over from it", e.Service, e.Ref, data)
			continue
		}
		mine = append(mine, e)
		n := len(refused)
		img := s.ownImage(e)
		last := ran{Ref: p.Ref, ID: p.Image.ID, Digests: p.Image.Digests}
		switch {
		case w.importing && repoKey(p.Ref) != repoKey(e.Ref):
			refused = append(refused, fmt.Sprintf("%s: the export's data last ran %s, from another repository than %s", e.Service, p.Ref, e.Ref))
		case last.is(img), familiar(p.Ref) == familiar(e.Ref):
		case p.History.movedPast(e.Service, img):
			refused = append(refused, fmt.Sprintf("%s: %s (%s) is an image %s data moved past: it last ran %s", e.Service, e.Ref, shownEntry(e), w.source, last.shown()))
		default:
			known := false
			for _, r := range p.History.window(e.Service) {
				known = known || familiar(r.Ref) == familiar(e.Ref)
			}
			if !known {
				refused = append(refused, fmt.Sprintf("%s: %s (%s) never ran on %s data, which last ran %s as %s", e.Service, e.Ref, shownEntry(e), w.source, last.shown(), p.Ref))
			}
		}
		if len(refused) > n {
			refusedSvcs = append(refusedSvcs, e.Service)
			refusedRefs = append(refusedRefs, e.Ref)
		}
	}
	if len(refused) > 0 && w.importing {
		s.sayAll(report.ErrorLevel, "this environment runs an image the export's data never ran, or moved past: nothing imported", refused,
			report.Fix("release it with the export's image (the same name), then import"),
			report.Fix("or, knowing its data takes it (an upgrade to try), run its own image on the imported data, its history starting over from it:", s.again(again, refusedSvcs)))
		return refusedRefs, ErrRefused
	}
	if len(refused) > 0 {
		s.sayAll(report.ErrorLevel, "the child runs images the parent's data never ran, or moved past: nothing changed", refused,
			report.Fix("release the child with the parent's image (the same name), then sync, or release it with the sync (--release-sync)"),
			report.Fix("or, knowing its data takes it (an upgrade to try), run the child's own image on the parent's data, its history starting over from it:", s.again(again, refusedSvcs)))
		return refusedRefs, ErrRefused
	}
	var extra []entry
	var pulled []string
	for _, e := range mine {
		hold := s.localHold(e, ps[e.Service])
		if w.importing {
			hold = s.ownHold(e, ps[e.Service])
		}
		if hold == "" {
			continue
		}
		if _, err := s.Docker.Inspect(hold); err != nil {
			if err := s.Docker.Pull(hold); err != nil {
				fix := report.Fix("the child's server needs the registry's image the parent's data last ran")
				if w.importing {
					fix = report.Fix("this server needs the registry's image the export's data last ran, or its own accepted:", s.again(again, []string{e.Service}))
				}
				msg := fmt.Sprintf("%s: %s image %s could not be pulled here: %s", e.Service, w.source, hold, w.nothing)
				// S72: the image is there, for other platforms only (a
				// parent on amd64, a child on arm64): Docker's words say no
				// more than that its manifest list has no match.
				if otherPlatform(err) {
					msg = fmt.Sprintf("%s: %s image %s has no build for this server's platform, %s/%s: %s", e.Service, w.source, hold, runtime.GOOS, runtime.GOARCH, w.nothing)
					fix = report.Fix("build and push it for both platforms, or run this server's own image on the data, its history starting over from it:", s.again(again, []string{e.Service}))
				}
				s.sayAll(report.ErrorLevel, msg, []string{err.Error()}, fix)
				// What it pulled before goes: a name by digest alone nothing
				// would ever remove.
				for _, p := range pulled {
					_ = s.Docker.Remove(p)
				}
				return nil, ErrRefused
			}
			pulled = append(pulled, hold)
			s.say(report.InfoLevel, "%s: pulled %s image %s", e.Service, w.source, hold)
		}
		extra = append(extra, s.entryOf(Service{Name: syncPrefix + e.Service, Ref: hold}, hold))
	}
	if len(extra) == 0 {
		return nil, nil
	}
	// Bookmarked as rel's at once: an image pulled by digest has no other
	// name a removal would take, nor anything that keeps it until the copy.
	kept := make([]entry, 0, len(es)+len(extra))
	for _, e := range es {
		if !strings.HasPrefix(e.Service, syncPrefix) {
			kept = append(kept, e)
		}
	}
	return nil, s.keep(rel, append(kept, extra...))
}

// localHold is the name to hold e's service on the parent's image by: a
// digest of it in the child's own repository when this server has it
// there (a mirror of its own, which keeps digests), else in the
// parent's. Docker finds a digest only in the repository it is named
// with, and a child's server may reach only its mirror.
func (s *Set) localHold(e entry, p *parentImage) string {
	for _, d := range p.Image.Digests {
		mine := repoKey(e.Ref) + "@" + digestOf(d)
		if _, err := s.Docker.Inspect(mine); err == nil {
			return mine
		}
	}
	return p.hold()
}

// otherPlatform says whether a pull failed because the image has no
// build for this platform: Docker's classic store and its containerd
// store word it differently.
func otherPlatform(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "no matching manifest for") || strings.Contains(msg, "no match for platform")
}

// ownHold is localHold for an import: always in the repository of e's
// own reference, whatever the export's digests name, so a record crafted
// with another repository's digest (mariadb:11.8 with only
// evil.example/x's) never makes the service pull or run that one: under
// e's repository the registry serves only e's images.
func (s *Set) ownHold(e entry, p *parentImage) string {
	holds := ownHolds(e, p)
	for _, h := range holds {
		if _, err := s.Docker.Inspect(h); err == nil {
			return h
		}
	}
	if len(holds) == 0 {
		return ""
	}
	return holds[0]
}

// ownHolds are the references ownHold chooses from, in its order: p's
// digests in e's own repository.
func ownHolds(e entry, p *parentImage) []string {
	// The digests in e's own repository first: on Docker's classic store
	// each repository's manifest has its own (an image pushed to a mirror
	// too), and the registry serves e's repository only by its own.
	var holds, others []string
	for _, d := range p.Image.Digests {
		if digestOf(d) == "" {
			continue
		}
		if repoKey(d) == repoKey(e.Ref) {
			holds = appendNew(holds, repoKey(e.Ref)+"@"+digestOf(d))
		} else {
			others = appendNew(others, repoKey(e.Ref)+"@"+digestOf(d))
		}
	}
	for _, o := range others {
		holds = appendNew(holds, o)
	}
	return holds
}

// SnapshotPull gets the images a restore of snap will hold its data
// services on (data, the snapshot's record of them, SnapshotData) when
// this server lacks them: pulled by digest in each service's own
// repository, as snap's listing names it, exactly what the restore's
// hold then takes (ownHold), never a repository the record names (an
// imported snapshot's record is anyone's). A service the listing does
// not have, or whose image no digest brings, is refused. Each image it
// pulls is bookmarked as the snapshot's at once (its listing's name), so
// another environment's removal meanwhile, which takes an image's names
// no bookmark holds, never strips it before the restore's data step
// holds it (review); the caller holds the server's image lock, shared.
func (s *Set) SnapshotPull(snap, data string) error {
	ps := parseParentData(data)
	es := readList(snap)
	names := bookmarks(es, s.Name, s.idOf(snap))
	for i, e := range es {
		p := ps[e.Service]
		if e.Kind != KindData || p == nil || p.Image.ID == "" {
			continue
		}
		// There already only when its own repository has it by digest,
		// never by the record's id (an imported record's is anyone's).
		pulled := ""
		for _, h := range ownHolds(e, p) {
			if _, err := s.Docker.Inspect(h); err == nil {
				pulled = h
				break
			}
		}
		for _, h := range ownHolds(e, p) {
			if pulled == "" && s.Docker.Pull(h) == nil {
				pulled = h
			}
		}
		if pulled == "" {
			return fmt.Errorf("the image %s's data ran when the snapshot was taken (%s) is gone and could not be pulled by its digest in %s: nothing restored", e.Service, p.Ref, repoKey(e.Ref))
		}
		// Only a snapshot among the environment's (s<id>): an import's,
		// checked where it was extracted, bookmarks its images once it
		// is in place (BookmarkFrom), and a bookmark of its directory's
		// id would name a release nothing ever removes.
		if !strings.HasPrefix(s.idOf(snap), "s") {
			continue
		}
		if err := s.Docker.Tag(pulled, names[i]); err != nil {
			return fmt.Errorf("bookmarking %s as %s: %w: nothing restored", pulled, names[i], err)
		}
	}
	return nil
}

// FilterImported keeps of an imported snapshot's listing (snap's, anyone's
// file) only the entries of a service the current release (cur) lists
// too, in that one's repository: what a later pull (SnapshotPull's own
// repository), its bookmarks (BookmarkFrom) and its removal take names
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
		// Its id is the other server's word: bookmarked by it, any local
		// image of another repository it names would be tagged into the
		// service's, which Docker's containerd store then resolves by
		// digest there (review round 5). Without, nothing is tagged until
		// a restore pulls it by digest in that repository, or builds it.
		e.ID = ""
		kept = append(kept, e)
	}
	return writeList(snap, kept)
}

// syncPrefix names a listing's entry for a parent's image a sync got for
// a service (SyncCheck): listed, so bookmarked and removed with the
// release, never a service of its own.
const syncPrefix = "sync:"

// SyncDone takes over the parent's data images once a sync's copy is
// complete: each pulled data service the parent has runs the parent's
// image, held by digest (rel's listing and ComposeFile), and its history
// is the parent's, which came with the data. from is the parent's
// address.
func (s *Set) SyncDone(base, rel, data, from string) error {
	return s.syncDone(base, rel, data, SyncedFrom+from, Forced, false)
}

// ImportDone is SyncDone for a complete import of the data an export
// recorded (ImportCheck): the service held on the export's image, its
// history the export's, first line Imported; an accepted service runs
// its own, its history starting over (Imported).
func (s *Set) ImportDone(base, rel, data string) error {
	return s.syncDone(base, rel, data, Imported, Imported, true)
}

// syncDone is SyncDone, the data's history's first line how first, an
// accepted service's line how accepted; importing, held in the service's
// own repository only (ownHold).
func (s *Set) syncDone(base, rel, data, first, accepted string, importing bool) error {
	ps := parseParentData(data)
	es := readList(rel)
	holds := holdsIn(rel)
	if holds == nil {
		holds = map[string]string{}
	}
	h := readHistory(base)
	changed := false
	var out []entry
	var cs map[string]Container
	for _, e := range es {
		if strings.HasPrefix(e.Service, syncPrefix) {
			continue
		}
		p := ps[e.Service]
		if e.Kind != KindData || p == nil || p.hold() == "" {
			out = append(out, e)
			continue
		}
		if s.Accept[e.Service] {
			// The image it runs (its container's, else its release's) on
			// the parent's data, accepted: its history starting over from it.
			hold := e.Digest
			if cs == nil {
				cs = s.containers()
			}
			if c, ok := cs[e.Service]; ok && c.started() {
				if img, err := s.Docker.Inspect(c.Image); err == nil {
					if d := digestIn(img, e.Ref); d != "" {
						hold = d
					} else if len(img.Digests) > 0 {
						hold = img.Digests[0]
					}
				}
			}
			img, err := s.Docker.Inspect(hold)
			if err != nil || hold == "" {
				out = append(out, e)
				continue
			}
			n := s.entryOf(Service{Name: e.Service, Ref: e.Ref, Data: true}, hold)
			n.Kind, n.Digest = KindData, hold
			out = append(out, n)
			holds[e.Service] = hold
			var kept history
			for _, r := range h {
				if r.Service != e.Service {
					kept = append(kept, r)
				}
			}
			h = append(kept, ranOn(e.Service, accepted, e.Ref, img, s.now()))
			changed = true
			continue
		}
		hold := s.localHold(e, p)
		if importing {
			hold = s.ownHold(e, p)
		}
		n := s.entryOf(Service{Name: e.Service, Ref: e.Ref, Data: true}, hold)
		n.Kind, n.Digest = KindData, hold
		if n.ID == "" {
			n.ID = p.Image.ID
		}
		out = append(out, n)
		holds[e.Service] = hold
		// The parent's history for it, in place of the child's own.
		var kept history
		for _, r := range h {
			if r.Service != e.Service {
				kept = append(kept, r)
			}
		}
		// The image that wrote the data is added when the parent's history
		// lacks it (a parent with none); one that drifted back to an
		// image its history has stays out, as it does on the parent, so
		// the newest stays the newest. The last line names the image as
		// the child does (its mirror, its pin), so the child's own name
		// moving forward needs nothing.
		w := append(history{}, p.History.window(e.Service)...)
		img, ref := p.Image, p.Ref
		if importing {
			// An export's record is whoever made the tar's (an imported
			// snapshot's too): the line it adds keeps only digests in the
			// service's own repository and names the hold when the
			// record's name is another repository's, and the window ends
			// on a line in that repository (below), so the data's newest
			// image a rollback later holds is never another repository's
			// (review). Every line stays: they are also what the data
			// moved past, by identity (a mirror's name of an image the
			// upstream's lines recorded: review round 5, dropped, a tag
			// gone back ran an older image on the data).
			img.Digests = inRepo(img.Digests, repoKey(e.Ref))
			if len(img.Digests) == 0 && hold != "" {
				img.Digests = []string{hold}
			}
			if repoKey(ref) != repoKey(e.Ref) {
				ref = hold
			}
		}
		known := false
		for _, r := range w {
			known = known || r.is(img)
		}
		if !known {
			w = append(w, ranOn(e.Service, Ran, ref, img, s.now()))
		}
		// Only when the child's own name holds the very image (a mirror):
		// another series --yes let through stays one its data never ran.
		// Its digests are the child's, so a hold of it is one the child's
		// server can resolve.
		mine := s.ownImage(e)
		if last := w[len(w)-1]; last.is(p.Image) && last.is(mine) && familiar(last.Ref) != familiar(e.Ref) {
			ds := mine.Digests
			if len(ds) == 0 {
				ds = last.Digests
			}
			w = append(w, ran{Time: s.now().Unix(), Service: e.Service, How: Ran, Ref: e.Ref, ID: last.ID, Digests: ds})
		}
		if importing && hold != "" {
			if last := w[len(w)-1]; repoKey(last.hold()) != repoKey(e.Ref) {
				w = append(w, ran{Time: s.now().Unix(), Service: e.Service, How: Ran, Ref: hold, ID: img.ID, Digests: []string{hold}})
			}
		}
		w[0].How = first
		if importing {
			// The time it was imported: --resolved keeps a history whose
			// first line came since the import's mark (Resolved), and the
			// export's own time is when its environment first ran that
			// image, before any mark here (review).
			w[0].Time = s.now().Unix()
		}
		h = append(kept, w...)
		changed = true
	}
	if !changed {
		return nil
	}
	if err := h.write(base); err != nil {
		s.say(report.ErrorLevel, "could not take over the parent's history of the images that ran on the data: %v", err)
		return err
	}
	if err := s.keep(rel, out); err != nil {
		return err
	}
	return writeCompose(rel, out, holds)
}

// inRepo are of digests (<repository>@sha256:...) those in repo.
func inRepo(digests []string, repo string) []string {
	var out []string
	for _, d := range digests {
		if repoKey(d) == repo {
			out = append(out, d)
		}
	}
	return out
}

// Recreate are the services of rel whose container its activation gave
// another image than rel's ComposeFile now gives it: a sync took over
// the parent's image. Started with compose start, they would run the old
// image on the parent's data, the finish hook first.
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

// Imported starts a service's history over from what runs it now, once a
// complete import of its data (or --resolved) is in: the data came from
// elsewhere, and the image now running it is the only one known to have
// run it.
func (s *Set) Imported(base, rel, service string) error {
	es := readList(rel)
	isData := false
	ref := ""
	for _, e := range es {
		if e.Service == service && e.Kind == KindData {
			isData, ref = true, e.Ref
		}
	}
	if !isData {
		return nil
	}
	c, ok := s.containers()[service]
	if !ok {
		return nil
	}
	img, err := s.Docker.Inspect(c.Image)
	if err != nil {
		return err
	}
	h := readHistory(base)
	h = append(h, ranOn(service, Imported, ref, img, s.now()))
	return h.write(base)
}

// Resolved is Imported for --resolved, since the time of the import's
// mark: nothing when the history's window starts with an import line
// written since (the import held the service on its export's image and
// took the export's history, and only what came after failed: the data
// is complete, and starting over would forget what wrote it), else
// Imported, as before. Neither the hold nor the newest line tells: every
// release holds its data service, and an export's window ends with its
// own lines.
func (s *Set) Resolved(base, rel, service string, since time.Time) error {
	if w := readHistory(base).window(service); len(w) > 0 && w[0].How == Imported && !since.IsZero() && w[0].Time >= since.Unix() {
		return nil
	}
	return s.Imported(base, rel, service)
}

// readHistoryText reads history lines from text.
func readHistoryText(text string) history {
	return parseHistory(text)
}

// baseOfRel is rel's environment directory, for callers outside.
func BaseOf(rel string) string { return filepath.Dir(filepath.Dir(rel)) }

// ownImage is the image the child's own name for e's service holds on
// this server (its pin, its mirror's tag), else the image it runs: what
// its next release would run, which the parent's data must take.
func (s *Set) ownImage(e entry) Image {
	if !strings.Contains(e.Ref, "@") {
		if img, err := s.Docker.Inspect(e.Ref); err == nil {
			return img
		}
	}
	if img, err := s.Docker.Inspect(e.ID); err == nil {
		return img
	}
	return Image{ID: e.ID}
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
