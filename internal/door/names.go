package door

import (
	"path/filepath"
	"strings"
)

// A project user is one project's, and every name of what is the user's comes from that
// project, here alone: the user (UserOf), its builder and the builder's network, its logins
// and its record of claimed images. A name computed twice, from the user in one place and the
// project in another, drifts apart (a key's removal then misses the builder whose caches it
// is to drop), so an entry whose user is not its project's is refused where it is read.

// UserOf is a project's user: devopsy-<project>, the name sudo's rules and sshd's Match know it
// by, through its group.
func UserOf(project string) string { return "devopsy-" + project }

// ProjectOfUser is the project a user's name is of ("" when it is no project user's name), the
// inverse of UserOf.
func ProjectOfUser(user string) string {
	project, ok := strings.CutPrefix(user, UserOf(""))
	if !ok {
		return ""
	}
	return project
}

// BuilderOf is a project's BuildKit builder, made at its first build.
func BuilderOf(project string) string { return UserOf(project) }

// BuilderNetworkOf is a project's builder's network, its own.
func BuilderNetworkOf(project string) string { return BuilderOf(project) + "-build" }

// LoginsDirOf is the directory of a project's registry logins in space, the door's copy of
// their auths.
func LoginsDirOf(space, project string) string { return filepath.Join(space, "logins", project) }

// ImagesFileOf is a project's record of claimed images in space, by its user's name.
func ImagesFileOf(space, project string) string {
	return filepath.Join(space, "images", UserOf(project))
}
