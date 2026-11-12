package uids

import (
	"fmt"
	"regexp"
	"strconv"
)

// Map is the owners a receiving side gives data copied into it, by range alone: every file
// owned by a uid (and a gid) of EnvDomain's range is given Env, its own environment's uid, every
// one of the two project ranges Project, its project's (or instance's), and every other owner
// (root, an image's own users, anything below the ranges) stays as it is. Nothing of the
// sending side's numbers is read or carried: the range says the kind, and the receiving side
// knows its own two.
type Map struct{ Env, Project uint32 }

// mapRanges are the ranges a Map takes, in its renderers' order: the environments', then
// both project domains', which are one span.
func mapRanges() (env, project [2]uint32) {
	es, en := Range(EnvDomain)
	ps, _ := Range(ProjectDomain)
	is, in := Range(InstanceDomain)
	return [2]uint32{es, es + en - 1}, [2]uint32{ps, is + in - 1}
}

// mapSpec is rsync's FROM:TO list of m, ranges as FROM.
func (m Map) mapSpec() string {
	env, project := mapRanges()
	return fmt.Sprintf("%d-%d:%d,%d-%d:%d", env[0], env[1], m.Env, project[0], project[1], m.Project)
}

// RsyncArgs are m for rsync, users and groups alike. rsync refuses them with --chown, so a
// copy that gives everything one owner takes --chown alone, never these.
func (m Map) RsyncArgs() []string {
	return []string{"--usermap=" + m.mapSpec(), "--groupmap=" + m.mapSpec()}
}

// rsyncMapRe is one of RsyncArgs' words, its two targets captured.
var rsyncMapRe = func() *regexp.Regexp {
	env, project := mapRanges()
	return regexp.MustCompile(fmt.Sprintf(`^--(?:usermap|groupmap)=%d-%d:(\d+),%d-%d:(\d+)$`, env[0], env[1], project[0], project[1]))
}()

// ParseRsyncMap reads one of RsyncArgs' words back: its environment's and its project's
// targets, as written, for a check of whose they are.
func ParseRsyncMap(arg string) (env, project string, ok bool) {
	m := rsyncMapRe.FindStringSubmatch(arg)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// Args are m as a MapScript's two arguments after its root.
func (m Map) Args() []string {
	return []string{strconv.FormatUint(uint64(m.Env), 10), strconv.FormatUint(uint64(m.Project), 10)}
}

// MapScript defines map_owners, a shell function giving the files under $1 owned by a uid of
// a Map's ranges their map's (users to $2 in the environment range, $3 in the project ranges,
// and groups the same, apart: a file's uid and gid may be of different kinds), on one
// filesystem (-xdev), never through a file's own link (chown -h, chgrp -h, find's default -P),
// and only what differs, in batches: chown clears a setuid or setgid bit, so a file already
// right is never touched. For after a copy that carried the sender's owners (a reflink clone,
// a tar's extraction: GNU tar maps owners only when it creates a tar). As chown_entry does, a
// failed pass is looked at again, mapping nothing: a file the service removed meanwhile has
// no owner to fix, so only something left on a mapped uid fails it, or a look that fails
// itself. Only files, directories and links: a device node keeps its owner. $2 of "-" maps
// nothing (a bind mount locally on macOS, whose owners Docker fakes).
var MapScript = func() string {
	env, project := mapRanges()
	return fmt.Sprintf(`map_ids() {
  mi_err=$(find "$1" -xdev -ignore_readdir_race \( -type f -o -type d -o -type l \) "$2" "+$4" "$2" "-$5" ! "$2" "$6" -exec "$3" -h "$6" {} + 2>&1) && return 0
  if ! mi_left=$(find "$1" -xdev -ignore_readdir_race \( -type f -o -type d -o -type l \) "$2" "+$4" "$2" "-$5" ! "$2" "$6" -print -quit); then
    printf '%%s\n' "$mi_err" >&2
    return 1
  fi
  [ -n "$mi_left" ] || return 0
  printf '%%s\n' "$mi_err" >&2
  return 1
}
map_owners() {
  [ "$2" != - ] || return 0
  map_ids "$1" -uid chown %d %d "$2" && map_ids "$1" -gid chgrp %d %d "$2" &&
    map_ids "$1" -uid chown %d %d "$3" && map_ids "$1" -gid chgrp %d %d "$3"
}
`, env[0]-1, env[1]+1, env[0]-1, env[1]+1, project[0]-1, project[1]+1, project[0]-1, project[1]+1)
}()
