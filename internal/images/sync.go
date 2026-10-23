package images

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

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
// pulled data service, the image that wrote its data (its container's,
// once started: a parent that drifted runs what wrote it; else its
// history's last), then the history that counts for it (its window).
// Lines, tab separated: "image", service, reference, id, digests; then
// "ran" and a history line.
func (s *Set) ParentData(base, rel string) string {
	h := readHistory(base)
	var cs map[string]Container
	var b strings.Builder
	for _, e := range readList(rel) {
		if e.Kind != KindData {
			continue
		}
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
		for _, r := range h.window(e.Service) {
			b.WriteString("ran\t" + r.line())
		}
	}
	return b.String()
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
		case "image":
			f := strings.Split(rest, "\t")
			if len(f) < 3 {
				continue
			}
			p := &parentImage{Service: f[0], Ref: f[1], Image: Image{ID: f[2]}}
			if len(f) > 3 {
				p.Image.Digests = strings.Fields(f[3])
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
	ps := parseParentData(data)
	es := readList(rel)
	var refused []string
	var mine []entry
	for _, e := range es {
		p := ps[e.Service]
		if e.Kind != KindData || p == nil {
			continue
		}
		mine = append(mine, e)
		img := Image{ID: e.ID}
		if i, err := s.Docker.Inspect(e.ID); err == nil {
			img = i
		}
		last := ran{Ref: p.Ref, ID: p.Image.ID, Digests: p.Image.Digests}
		switch {
		case last.is(img), familiar(p.Ref) == familiar(e.Ref):
		case p.History.movedPast(e.Service, img):
			refused = append(refused, fmt.Sprintf("%s: %s (%s) is an image the parent's data moved past: it last ran %s", e.Service, e.Ref, shownEntry(e), last.shown()))
		default:
			known := false
			for _, r := range p.History.window(e.Service) {
				known = known || familiar(r.Ref) == familiar(e.Ref)
			}
			if !known {
				refused = append(refused, fmt.Sprintf("%s: %s (%s) never ran on the parent's data, which last ran %s as %s", e.Service, e.Ref, shownEntry(e), last.shown(), p.Ref))
			}
		}
	}
	if len(refused) > 0 && !s.Yes {
		yes := "--sync --yes"
		if s.Address != "" {
			yes = "devopsy @" + s.Address + " " + yes
		}
		s.sayAll(report.ErrorLevel, "the child runs images the parent's data never ran, or moved past: nothing changed", refused,
			report.Fix("release the child with the parent's image (the same name), then sync, or release it with the sync (--release-sync)"),
			report.Fix("or, knowing the data takes it, sync anyway: the child runs the parent's image after the copy:", yes))
		return ErrRefused
	}
	if len(refused) > 0 {
		s.sayAll(report.WarnLevel, "--yes: syncing anyway, the child held on the parent's images after the copy:", refused)
	}
	var extra []entry
	for _, e := range mine {
		hold := ps[e.Service].hold()
		if hold == "" {
			continue
		}
		if _, err := s.Docker.Inspect(hold); err != nil {
			if err := s.Docker.Pull(hold); err != nil {
				s.sayAll(report.ErrorLevel, fmt.Sprintf("%s: the parent's image %s could not be pulled here: nothing changed", e.Service, hold), []string{err.Error()},
					report.Fix("the child's server needs the registry's image the parent's data last ran"))
				return ErrRefused
			}
			s.say(report.InfoLevel, "%s: pulled the parent's image %s", e.Service, hold)
		}
		extra = append(extra, s.entryOf(Service{Name: syncPrefix + e.Service, Ref: hold}, hold))
	}
	if len(extra) == 0 {
		return nil
	}
	// Bookmarked as rel's at once: an image pulled by digest has no other
	// name a removal would take, nor anything that keeps it until the copy.
	kept := make([]entry, 0, len(es)+len(extra))
	for _, e := range es {
		if !strings.HasPrefix(e.Service, syncPrefix) {
			kept = append(kept, e)
		}
	}
	return s.keep(rel, append(kept, extra...))
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
	ps := parseParentData(data)
	es := readList(rel)
	holds := holdsIn(rel)
	if holds == nil {
		holds = map[string]string{}
	}
	h := readHistory(base)
	changed := false
	var out []entry
	for _, e := range es {
		if strings.HasPrefix(e.Service, syncPrefix) {
			continue
		}
		p := ps[e.Service]
		if e.Kind != KindData || p == nil || p.hold() == "" {
			out = append(out, e)
			continue
		}
		hold := p.hold()
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
		w := p.History.window(e.Service)
		if len(w) == 0 || !w[len(w)-1].is(p.Image) {
			w = append(w, ranOn(e.Service, Ran, p.Ref, p.Image, s.now()))
		}
		w = append(history{}, w...)
		w[0].How = SyncedFrom + from
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

// readHistoryText reads history lines from text.
func readHistoryText(text string) history {
	return parseHistory(text)
}

// baseOfRel is rel's environment directory, for callers outside.
func BaseOf(rel string) string { return filepath.Dir(filepath.Dir(rel)) }
