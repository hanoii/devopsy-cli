package images

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ComposeFile is devopsy's own compose file, in a release's directory
// beside its code: the facts of its activation a project could not write
// in advance, and nothing the project chooses. Every service the
// activation fetched gets DigestLabel, the image it gave it, and a built
// one image: that image's id (environments building under one name, a
// branch's or a pull request's, move it for each other); a data
// service held on another image than its release's own (holdsFor: the
// rule refuses its own against its data's history) gets HeldLabel, and
// image: the reference that holds it (its digest, another release's own
// name, a bookmark) unless its own name was put on it. devopsy runs compose with
// it between the project's compose.yaml and the operator's
// compose.override.yaml, on the server only.
const ComposeFile = "devopsy-compose.yaml"

// HeldLabel marks a held service (holdsFor), its value where the image
// it runs came from (source): the release that ran it on the data, or
// "snapshot <id>", or historyHeld: --status says so (heldWhat).
const HeldLabel = "devopsy.held"

// composeHold is how ComposeFile holds a service: Image its image: (""
// when its own name holds it), From HeldLabel's value ("" for an own image
// by its bookmark, no hold). bookmark: by the release's own bookmark,
// named once its listing is known (resolveBookmarks).
type composeHold struct {
	Image, From string
	bookmark    bool
}

// writeCompose writes rel's ComposeFile: each entry's label, and holds.
func writeCompose(rel string, es []entry, holds map[string]composeHold) error {
	return writeFile(filepath.Join(rel, ComposeFile), composeText(es, holds))
}

// composeText is a ComposeFile's content.
func composeText(es []entry, holds map[string]composeHold) string {
	var b strings.Builder
	b.WriteString("# devopsy's own: the image each service was given by this release's\n" +
		"# last activation, and the ones it holds. Rewritten by every\n" +
		"# activation: never edit it (shared/compose.override.yaml is yours).\n")
	var svcs strings.Builder
	seen := map[string]bool{}
	for _, e := range es {
		if e.Service == "" || seen[e.Service] {
			continue
		}
		seen[e.Service] = true
		label := e.ID
		if e.Kind != KindOwn && e.Digest != "" {
			label = e.Digest
		}
		hold := holds[e.Service]
		if label == "" && hold.Image == "" {
			continue
		}
		if label == "" {
			label = hold.Image
		}
		image := hold.Image
		if image == "" && e.Kind == KindOwn && e.ID != "" {
			// Pinned: the run step, a plain up, a sync's start resolve the name again
			// otherwise, and every environment building under it moves it (a branch's,
			// a pull request's).
			image = e.ID
		}
		svcs.WriteString("  " + strconv.Quote(e.Service) + ":\n")
		if image != "" {
			svcs.WriteString("    image: " + strconv.Quote(image) + "\n")
		}
		svcs.WriteString("    labels:\n      " + strconv.Quote(DigestLabel) + ": " + strconv.Quote(label) + "\n")
		if hold.From != "" {
			svcs.WriteString("      " + strconv.Quote(HeldLabel) + ": " + strconv.Quote(hold.From) + "\n")
		}
	}
	if svcs.Len() == 0 {
		b.WriteString("services: {}\n")
	} else {
		b.WriteString("services:\n" + svcs.String())
	}
	return b.String()
}

// heldIn reads rel's ComposeFile's holds: service to HeldLabel's value.
func heldIn(rel string) map[string]string {
	_, _, held := composeFacts(rel)
	return held
}

// labelsIn reads the DigestLabel of each service in rel's ComposeFile.
func labelsIn(rel string) map[string]string {
	_, labels, _ := composeFacts(rel)
	return labels
}

// composeFacts reads rel's ComposeFile as writeCompose writes it: each
// service's image:, DigestLabel and HeldLabel; nil without one.
func composeFacts(rel string) (images, labels, held map[string]string) {
	data, err := os.ReadFile(filepath.Join(rel, ComposeFile))
	if err != nil {
		return nil, nil, nil
	}
	images, labels, held = map[string]string{}, map[string]string{}, map[string]string{}
	svc := ""
	label := "      " + strconv.Quote(DigestLabel) + ": "
	heldLabel := "      " + strconv.Quote(HeldLabel) + ": "
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(line, ":"):
			svc, _ = strconv.Unquote(strings.TrimSuffix(strings.TrimSpace(line), ":"))
		case strings.HasPrefix(line, "    image: ") && svc != "":
			images[svc], _ = strconv.Unquote(strings.TrimPrefix(line, "    image: "))
		case strings.HasPrefix(line, label) && svc != "":
			labels[svc], _ = strconv.Unquote(strings.TrimPrefix(line, label))
		case strings.HasPrefix(line, heldLabel) && svc != "":
			held[svc], _ = strconv.Unquote(strings.TrimPrefix(line, heldLabel))
		}
	}
	return images, labels, held
}

// StatusLines are --status's lines on rel's images, one per service
// worth a word: running another image than rel gives it (its container
// another activation's: one cut off past its switch, a rollback's whose
// newer containers run on; OrbStack cuts, round 8), another image than
// its activation gave it (DigestLabel: a plain up followed a name another
// environment's pull moved, or something outside devopsy), or held on
// another image than the release's own (HeldLabel), own names included.
// A hold is said, the rest warned.
func StatusLines(d Docker, project, rel string) []StatusLine {
	pins, labels, held := composeFacts(rel)
	cs, err := d.Containers(project)
	if err != nil {
		return []StatusLine{{Text: "could not list the containers: " + err.Error(), Warn: true}}
	}
	refs := map[string]string{}
	for _, e := range readList(rel) {
		refs[e.Service] = e.Ref
	}
	var lines []StatusLine
	seen := map[string]bool{}
	for _, c := range cs {
		if c.OneOff || c.Service == "" || seen[c.Service] {
			continue
		}
		seen[c.Service] = true
		img, err := d.Inspect(c.Image)
		gave := c.Digest
		switch {
		case err == nil && labels[c.Service] != "" && gave != labels[c.Service] && !sameImage(img, labels[c.Service]):
			lines = append(lines, StatusLine{Text: fmt.Sprintf("%s runs %s, not %s the current release gives it: an activation was cut off, and the next release makes it again", c.Service, shownImage(refs[c.Service], img), shortName(labels[c.Service])), Warn: true})
			continue
		case err != nil || gave == "":
		case !sameImage(img, gave):
			l := fmt.Sprintf("%s runs %s, not %s its activation gave it", c.Service, shownImage(refs[c.Service], img), shortName(gave))
			if pins[c.Service] != "" {
				// The file names its image (a build's id, a hold): no name moved under it.
				l += ": something outside devopsy's compose file made it"
			} else if r := refs[c.Service]; r != "" {
				l += ": " + r + " moved since (a plain up follows names)"
			}
			lines = append(lines, StatusLine{Text: l, Warn: true})
			continue
		}
		if from := held[c.Service]; from != "" {
			on := shortName(labels[c.Service])
			if r := refs[c.Service]; r != "" && digestOf(labels[c.Service]) == "" {
				on = r + " (" + on + ")"
			}
			lines = append(lines, StatusLine{Text: fmt.Sprintf("%s held on %s, %s, which the current release's would not allow: a release that fixes forward is the way on", c.Service, on, heldWhat(from))})
		}
	}
	return lines
}

// StatusLine is one of StatusLines: Warn when it needs attention.
type StatusLine struct {
	Text string
	Warn bool
}

// shownImage is an image for messages: its digest in ref's repository,
// short, else its short id.
func shownImage(ref string, img Image) string {
	if d := digestIn(img, ref); d != "" {
		return repoOf(d) + "@" + short(digestOf(d))
	}
	return short(img.ID)
}

// sameImage says whether img is the one a DigestLabel names: its id, or
// one of its digests.
func sameImage(img Image, label string) bool {
	if img.ID == label {
		return true
	}
	for _, d := range img.Digests {
		if digestOf(d) != "" && digestOf(d) == digestOf(label) {
			return true
		}
	}
	return false
}

// shortName is a digest name or an id, short.
func shortName(n string) string {
	if d := digestOf(n); d != "" {
		return repoOf(n) + "@" + short(d)
	}
	return short(n)
}
