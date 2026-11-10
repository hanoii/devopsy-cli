package images

import "strings"

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

// HubDomain says whether domain is Docker Hub's, under any of its names.
func HubDomain(domain string) bool {
	switch domain {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return true
	}
	return false
}

// Canonical is ref as Docker names it in full: Docker Hub's as docker.io/library/<name> or
// docker.io/<namespace>/<name> (index.docker.io folded to docker.io), any other host and port
// as written, the tag and digest kept. A loaded image's annotation needs it: a familiar name
// there loads with no registry digest.
func Canonical(ref string) string {
	domain, rest, _ := SplitDomain(ref)
	if !HubDomain(domain) {
		return domain + "/" + rest
	}
	if name := RepoOf(rest); !strings.Contains(name, "/") {
		rest = "library/" + rest
	}
	return "docker.io/" + rest
}
