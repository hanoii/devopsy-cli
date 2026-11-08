package door

import (
	"regexp"
	"unicode"
)

// HubLogin is the key Docker keeps Docker Hub's login under, and looks it up by for a pull.
const HubLogin = "https://index.docker.io/v1/"

var loginRegistry = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)

// LoginRegistry says whether a project user's login names a registry as Docker keeps its
// logins (HubLogin, else a host and an optional port): never a flag, a path or a scheme of
// the caller's, which docker login would read as more than a registry.
func LoginRegistry(r string) bool { return r == HubLogin || loginRegistry.MatchString(r) }

// LoginUser says whether a login's user name is one word of text, which docker login takes
// as a value and nothing else.
func LoginUser(u string) bool {
	if u == "" || u[0] == '-' || len(u) > 256 {
		return false
	}
	for _, r := range u {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
