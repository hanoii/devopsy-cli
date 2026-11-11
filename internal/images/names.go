package images

import (
	"regexp"
	"strings"
)

// SplitDomain is ref's registry host (domain) and the rest, as Docker reads a name: its first
// part is a host when it holds a dot or a colon, is localhost, or has a capital (a repository's
// parts are lowercase); any other name is Docker Hub's, docker.io, not written (explicit
// false). One rule for every reader of a host (Canonical, repoKey, the door's registryHost and
// login key), so none reads a name otherwise than Docker does.
func SplitDomain(ref string) (domain, rest string, explicit bool) {
	first, after, ok := strings.Cut(ref, "/")
	if !ok || !strings.ContainsAny(first, ".:") && first != "localhost" && strings.ToLower(first) == first {
		return "docker.io", ref, false
	}
	return first, after, true
}

// HubDomain says whether domain is Docker Hub's, under any of its names, as a login is kept
// (docker login folds registry-1.docker.io too). A name's is hubName's.
func HubDomain(domain string) bool {
	return hubName(domain) || domain == "registry-1.docker.io"
}

// hubName says whether domain names Docker Hub in an image's name, as Docker reads it: docker.io
// and index.docker.io, which it folds into docker.io, never registry-1.docker.io, a host as
// written.
func hubName(domain string) bool {
	return domain == "docker.io" || domain == "index.docker.io"
}

// Canonical is ref as Docker names it in full: Docker Hub's as docker.io/library/<name> or
// docker.io/<namespace>/<name> (index.docker.io folded to docker.io), any other host and port
// as written, the tag and digest kept. A loaded image's annotation needs it: a familiar name
// there loads with no registry digest.
func Canonical(ref string) string {
	domain, rest, _ := SplitDomain(ref)
	if !hubName(domain) {
		return domain + "/" + rest
	}
	if name := RepoOf(rest); !strings.Contains(name, "/") {
		rest = "library/" + rest
	}
	return "docker.io/" + rest
}

// reference is Docker's grammar of an image's name (distribution/reference): an optional
// registry host and port, a lowercase path of components, an optional tag, an optional digest.
var reference = func() *regexp.Regexp {
	const (
		alnum     = `[a-z0-9]+`
		separator = `(?:[._]|__|[-]+)`
		component = alnum + `(?:` + separator + alnum + `)*`
		domainC   = `(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])`
		host      = `(?:` + domainC + `(?:\.` + domainC + `)*|\[[a-fA-F0-9:]+\])`
		domain    = host + `(?::[0-9]+)?`
		name      = `(?:` + domain + `/)?` + component + `(?:/` + component + `)*`
		tag       = `[\w][\w.-]{0,127}`
		digest    = `[A-Za-z][A-Za-z0-9]*(?:[-_+.][A-Za-z][A-Za-z0-9]*)*:[0-9a-fA-F]{32,}`
	)
	return regexp.MustCompile(`^` + name + `(?::` + tag + `)?(?:@` + digest + `)?$`)
}()

// IsReference says whether ref is an image's name as Docker reads one, and nothing else a tool
// takes for a source: buildx's imagetools also reads `oci-layout://<path>:<tag>` (a local
// layout, which it makes when it is not there) and `docker-image://`, so a name a project
// gives reaches buildx only as one of Docker's.
func IsReference(ref string) bool {
	return len(ref) <= 4096 && reference.MatchString(ref)
}
