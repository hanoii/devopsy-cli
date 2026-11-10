package door

import (
	"errors"
	"regexp"
	"unicode"
)

// HubLogin is the key Docker keeps Docker Hub's login under, and looks it up by for a pull.
const HubLogin = "https://index.docker.io/v1/"

var loginRegistry = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)

// LoginRegistry refuses a project user's login that names no registry as Docker keeps its
// logins (HubLogin, else a host and an optional port): a flag, a path or a scheme of the
// caller's, which docker login would read as more than a registry, or a host of the server's
// own or a metadata service's (ownHost), which Docker would contact for it.
func LoginRegistry(r string) error {
	switch {
	case r == HubLogin:
		return nil
	case !loginRegistry.MatchString(r):
		return errors.New("not a registry's name")
	case ownHost(hostOf(r)):
		return errors.New("a registry on the server's own or a metadata address, which Docker would contact for this project")
	}
	return nil
}

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
