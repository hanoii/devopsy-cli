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

// BuilderContainerOf is the container buildx runs a project's builder in, its one node's
// (buildx_buildkit_<node>, the node <builder>0).
func BuilderContainerOf(project string) string { return "buildx_buildkit_" + BuilderOf(project) + "0" }

// BuilderStateOf is the volume a project's builder keeps its cache in.
func BuilderStateOf(project string) string { return BuilderContainerOf(project) + "_state" }

// BuildxConfigOf is the project users' builders' BUILDX_CONFIG in space: their settings and
// the builders' records, apart from the deploy user's own.
func BuildxConfigOf(space string) string { return filepath.Join(space, "buildx") }

// BuildersLocksOf is the directory of the builders' locks in space, the deploy user's alone,
// apart from BuildxConfigOf, whose files are buildx's.
func BuildersLocksOf(space string) string { return filepath.Join(space, "builders") }

// BuilderLockOf is a project's builder's lock in space: shared by each build and --gc's use,
// exclusive for a stop or a remake.
func BuilderLockOf(space, project string) string {
	return filepath.Join(BuildersLocksOf(space), UserOf(project)+".lock")
}

// BuilderIdleLockOf is the lock a project's one stopper holds in space: with a dot, which no
// project's name has, so it is never another project's builder's lock.
func BuilderIdleLockOf(space, project string) string {
	return filepath.Join(BuildersLocksOf(space), UserOf(project)+".idle.lock")
}

// LoginsDirOf is the directory of a project's registry logins in space, the door's copy of
// their auths.
func LoginsDirOf(space, project string) string { return filepath.Join(space, "logins", project) }

// ImagesFileOf is a project's record of claimed images in space, by its user's name.
func ImagesFileOf(space, project string) string {
	return filepath.Join(space, "images", UserOf(project))
}
