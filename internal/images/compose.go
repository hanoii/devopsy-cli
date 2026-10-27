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
// activation fetched gets DigestLabel, the image it gave it; a pulled
// data service, and a pulled service a rollback keeps on an image its
// name no longer points to, are held by digest (image:
// <repository>@sha256:...), so nothing but the environment's own next
// activation moves them: not another environment's pull, not a plain up.
// devopsy runs compose with it between the project's compose.yaml and
// the operator's compose.override.yaml, on the server only.
const ComposeFile = "devopsy-compose.yaml"

// writeCompose writes rel's ComposeFile: each entry's label, and holds
// (service to digest name).
func writeCompose(rel string, es []entry, holds map[string]string) error {
	var b strings.Builder
	b.WriteString("# devopsy's own: the image each service was given by this release's\n" +
		"# last activation, and the ones it holds by digest. Rewritten by every\n" +
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
		if hold != "" {
			label = hold
		}
		if label == "" {
			continue
		}
		svcs.WriteString("  " + strconv.Quote(e.Service) + ":\n")
		if hold != "" {
			svcs.WriteString("    image: " + strconv.Quote(hold) + "\n")
		}
		svcs.WriteString("    labels:\n      " + strconv.Quote(DigestLabel) + ": " + strconv.Quote(label) + "\n")
	}
	if svcs.Len() == 0 {
		b.WriteString("services: {}\n")
	} else {
		b.WriteString("services:\n" + svcs.String())
	}
	return writeFile(filepath.Join(rel, ComposeFile), b.String())
}

// holdsIn reads the holds of rel's ComposeFile (service to digest name):
// what its last activation held.
func holdsIn(rel string) map[string]string {
	holds, _ := composeFacts(rel)
	return holds
}

// labelsIn reads the DigestLabel of each service in rel's ComposeFile.
func labelsIn(rel string) map[string]string {
	_, labels := composeFacts(rel)
	return labels
}

// composeFacts reads rel's ComposeFile as writeCompose writes it: nil
// without one.
func composeFacts(rel string) (holds, labels map[string]string) {
	data, err := os.ReadFile(filepath.Join(rel, ComposeFile))
	if err != nil {
		return nil, nil
	}
	holds, labels = map[string]string{}, map[string]string{}
	svc := ""
	label := "      " + strconv.Quote(DigestLabel) + ": "
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(line, ":"):
			svc, _ = strconv.Unquote(strings.TrimSuffix(strings.TrimSpace(line), ":"))
		case strings.HasPrefix(line, "    image: ") && svc != "":
			holds[svc], _ = strconv.Unquote(strings.TrimPrefix(line, "    image: "))
		case strings.HasPrefix(line, label) && svc != "":
			labels[svc], _ = strconv.Unquote(strings.TrimPrefix(line, label))
		}
	}
	return holds, labels
}

// StatusLines are --status's lines on rel's images, one per service
// worth a word: held by digest (its activation's), running another image
// than rel gives it (its container another activation's: one cut off
// past its switch, a rollback's whose newer containers run on; OrbStack
// cuts, round 8), or another image than its activation gave it
// (DigestLabel: a plain up followed a name another environment's pull
// moved, or something outside devopsy).
func StatusLines(d Docker, project, rel string) []string {
	holds, labels := composeFacts(rel)
	cs, err := d.Containers(project)
	if err != nil {
		return []string{"could not list the containers: " + err.Error()}
	}
	refs := map[string]string{}
	for _, e := range readList(rel) {
		refs[e.Service] = e.Ref
	}
	var lines []string
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
			lines = append(lines, fmt.Sprintf("%s runs %s, not %s the current release gives it: an activation was cut off, and the next release makes it again", c.Service, shownImage(refs[c.Service], img), shortName(labels[c.Service])))
			continue
		case err != nil || gave == "":
		case !sameImage(img, gave):
			l := fmt.Sprintf("%s runs %s, not %s its activation gave it", c.Service, shownImage(refs[c.Service], img), shortName(gave))
			if r := refs[c.Service]; r != "" {
				l += ": " + r + " moved since (a plain up follows names)"
			}
			lines = append(lines, l)
			continue
		}
		if h := holds[c.Service]; h != "" {
			lines = append(lines, fmt.Sprintf("%s held on %s", c.Service, shortName(h)))
		}
	}
	return lines
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
