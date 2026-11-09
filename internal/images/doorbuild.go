package images

import "strings"

// DoorHost is the registry host of a door project's builds: devopsy's own, under .internal,
// which is reserved for private networks and never exists on the public internet, so no
// public registry serves it; the door pulls no name under it but a project's own, which
// moves only its own image (door.Policy.Claimable).
const DoorHost = "devopsy.internal"

// DoorBuild is the name of the image a door project builds for service in the environment
// of composeName, whatever image: its file says: devopsy.internal/<compose name>/<service>.
// Only the door writes it (the render, internal/door), and the listing names the build so
// too (ParseComposeConfig), so every name a door project writes has a host no registry
// serves, and a registry's names only ever move by a pull of them.
func DoorBuild(composeName, service string) string {
	return DoorHost + "/" + composeName + "/" + service
}

// IsDoorBuild reads ref, a tag allowed and never a digest, as a DoorBuild name: the compose
// name and service it is of.
func IsDoorBuild(ref string) (composeName, service string, ok bool) {
	rest, found := strings.CutPrefix(ref, DoorHost+"/")
	if !found || strings.Contains(rest, "@") {
		return "", "", false
	}
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		rest = rest[:i]
	}
	name, svc, found := strings.Cut(rest, "/")
	if !found || name == "" || svc == "" || strings.Contains(svc, "/") {
		return "", "", false
	}
	return name, svc, true
}

// doorBuildsIn says whether a listing of rel names its builds by DoorBuild: behind the door
// (a project user's devopsy), in a release that builds them. In image mode a registry serves
// them, under the image: the file says.
func doorBuildsIn(rel string, door bool) bool {
	return door && ModeOf(rel) != ModeImage
}
