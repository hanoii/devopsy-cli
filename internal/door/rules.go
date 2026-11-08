// Package door decides what a project user, who has no Docker socket, may ask
// the deploy user to do with Docker: rules on a compose file rendered by that
// user, and a closed list of each verb's flags. Deny by default: a key or a
// flag not listed here refuses the request, so one compose adds later fails
// closed. Nothing here touches Docker or the filesystem.
//
// Rules: internals/door.md.
package door

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Policy is what the server says of one project user.
type Policy struct {
	// Project is the project this user holds.
	Project string
	// User is its Unix user.
	User string
	// Space is the deploy user's directory for the door, on a mount of its own
	// with nosuid and nodev.
	Space string
	// Tree is the project's release root, which that user owns.
	Tree string
	// UID is the project user's, for what is given it as root (the door's regrant).
	UID string
	// Joinable are shared networks it may join.
	Joinable []string
	// Roles are the roles the admin gave it.
	Roles []string
	// ReadAll, the admin's grant, lets it read every container and network: their
	// environments and secrets included.
	ReadAll bool
	// ForbiddenIDs are host ids no container may run as: a container as a host
	// user's uid is that user's to reach through /proc.
	ForbiddenIDs []int
	// Others are the server's other projects. Names are flat, so one whose name extends
	// this one's would otherwise be this one's to reach: shop-eu's, for shop.
	Others []string
}

// OwnName says whether a compose name is this project's (<project>-...), and
// no other project's whose name extends this one's.
func (p Policy) OwnName(name string) bool {
	// One word: this names a directory (Mnt).
	if !strings.HasPrefix(name, p.Project+"-") || !wordOnly.MatchString(name) {
		return false
	}
	for _, other := range p.Others {
		if len(other) > len(p.Project) && (name == other || strings.HasPrefix(name, other+"-")) {
			return false
		}
	}
	return true
}

// Mnt is the directory of a compose name's entries: every part of it is the
// deploy user's, so a project user cannot replace an entry.
func (p Policy) Mnt(name string) string {
	return filepath.Join(p.Space, "mnt", name)
}

// NameOf is the compose name of an environment at a real path in this user's
// tree, from nothing else (Target.ComposeName): <tree>/<environment> is
// <project>-<environment>, <tree>/<instance>/<environment> has its instance too.
func (p Policy) NameOf(real string) (string, error) {
	rel, ok := strings.CutPrefix(real, strings.TrimRight(p.Tree, "/")+"/")
	parts := strings.Split(rel, "/")
	if !ok || len(parts) > 2 {
		return "", refuse("not an environment of this project's: %s", real)
	}
	for _, part := range parts {
		if !nameWord.MatchString(part) {
			return "", refuse("not an environment of this project's: %s", real)
		}
	}
	name := p.Project + "-" + strings.Join(parts, "-")
	if !p.OwnName(name) {
		return "", refuse("not an environment of this project's: %s", real)
	}
	return name, nil
}

var nameWord = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Clones is the directory of this project's reflink clones: on the data's
// filesystem, the deploy user's, as a clone holds other uids' files.
func (p Policy) Clones() string {
	return filepath.Join(p.Space, "clones", p.Project)
}

// Sealed is the directory of this project's read-only copies.
func (p Policy) Sealed() string {
	return filepath.Join(p.Space, "sealed", p.Project)
}

// InTree says whether a real path is in the tree this user owns.
func (p Policy) InTree(real string) bool {
	return strings.HasPrefix(real, strings.TrimRight(p.Tree, "/")+"/")
}

// Paths is what the rules ask of the filesystem, on the project user's rights:
// where a path ends, and a copy of it out of that user's reach.
type Paths interface {
	// Real resolves a path as the project user sees it. Its answer is believed
	// only for a path whose every part is the deploy user's.
	Real(path string) (string, error)
	// Seal copies a path of the project's, by content, into the deploy user's
	// space, and returns the copy's path.
	Seal(path string) (string, error)
}

// Refusal is a request the door turns down.
type Refusal struct{ Why string }

func (r *Refusal) Error() string { return "refused: " + r.Why }

func refuse(format string, args ...any) error {
	return &Refusal{fmt.Sprintf(format, args...)}
}

// topKeys and serviceKeys are what a rendered file may hold. What compose does
// as whoever runs it (env_file, extends, label_file, provider, include) is not
// here, and a rendered file has no need of it: the project user read those.
var topKeys = []string{"name", "services", "networks", "volumes"}

var serviceKeys = []string{
	"attach", "build", "cap_add", "cap_drop", "command", "cpus", "depends_on", "deploy", "domainname",
	"entrypoint", "environment", "expose", "extra_hosts", "group_add", "healthcheck", "hostname", "image",
	"init", "labels", "logging", "mem_limit", "network_mode", "networks", "pids_limit", "ports", "post_start",
	"pre_stop", "profiles", "pull_policy", "read_only", "restart", "security_opt", "shm_size", "stdin_open",
	"stop_grace_period", "stop_signal", "sysctls", "tmpfs", "tty", "ulimits", "user", "volumes", "working_dir",
}

// buildKeys are what a build may hold: its client reads the context with the
// deploy user's rights. A secret or an ssh agent would be that user's.
// Labels go on the image and from it on every container, past what reads a service's.
var buildKeys = []string{"args", "context", "dockerfile", "no_cache", "pull", "target"}

var numericUser = regexp.MustCompile(`^(\d+):(\d+)$`)

// CheckFile checks a rendered compose file for verb and changes it to what
// compose then runs: a build's block gone but for the build verb, bind sources
// pinned. Every refusal is returned, in the order of the file.
func CheckFile(file map[string]any, p Policy, verb string, paths Paths) []error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, refuse(format, args...)) }
	// Compose interpolates what it reads, after these checks: a rendered file holds only
	// `$$`, and anything else is the caller's, to mean one thing here and another there.
	if bad := variableIn(file); bad != "" {
		fail("a variable in a rendered file: %s", bad)
		return errs
	}
	name, _ := file["name"].(string)
	if !p.OwnName(name) {
		fail("another project's name: %s", name)
	}
	for _, k := range sortedKeys(file) {
		if !slices.Contains(topKeys, k) && !strings.HasPrefix(k, "x-") {
			fail("top: %s", k)
		}
	}
	checkTopVolumes(file, name, fail)
	checkTopNetworks(file, name, p, fail)
	services, _ := file["services"].(map[string]any)
	for _, svc := range sortedKeys(services) {
		s, ok := services[svc].(map[string]any)
		if !ok {
			fail("%s: not a service", svc)
			continue
		}
		checkService(file, name, svc, s, p, verb, paths, fail)
	}
	return errs
}

// variableIn is a string of the file, keys included, holding a `$` not doubled.
func variableIn(v any) string {
	switch x := v.(type) {
	case string:
		if strings.Contains(strings.ReplaceAll(x, "$$", ""), "$") {
			return x
		}
	case []any:
		for _, e := range x {
			if bad := variableIn(e); bad != "" {
				return bad
			}
		}
	case map[string]any:
		for k, e := range x {
			if bad := variableIn(k); bad != "" {
				return bad
			}
			if bad := variableIn(e); bad != "" {
				return bad
			}
		}
	}
	return ""
}

// Each of these is deny by default too: a key the rules do not read is not there.
var (
	topVolumeKeys  = []string{"name", "labels"}
	topNetworkKeys = []string{"name", "external", "internal", "driver", "labels", "attachable", "enable_ipv6", "ipam"}
	netKeys        = []string{"aliases", "gw_priority", "priority", "ipv4_address", "ipv6_address"}
	mountKeys      = []string{"type", "source", "target", "read_only", "bind", "volume", "tmpfs", "consistency"}
	bindKeys       = []string{"create_host_path"}
	volumeKeys     = []string{"nocopy", "subpath"}
	tmpfsKeys      = []string{"size", "mode"}
)

// unknownKey is a key of m not in known.
func unknownKey(m map[string]any, known []string) string {
	for _, k := range sortedKeys(m) {
		if !slices.Contains(known, k) {
			return k
		}
	}
	return ""
}

func checkTopVolumes(file map[string]any, name string, fail func(string, ...any)) {
	volumes, _ := file["volumes"].(map[string]any)
	for _, key := range sortedKeys(volumes) {
		v, _ := volumes[key].(map[string]any)
		n, _ := v["name"].(string)
		if unknownKey(v, topVolumeKeys) != "" || !strings.HasPrefix(n, name+"_") {
			fail("volume %s: not the project's plain volume", key)
		}
	}
}

func checkTopNetworks(file map[string]any, name string, p Policy, fail func(string, ...any)) {
	networks, _ := file["networks"].(map[string]any)
	for _, key := range sortedKeys(networks) {
		n, _ := networks[key].(map[string]any)
		nm, _ := n["name"].(string)
		if truthy(n["external"]) {
			if !slices.Contains(p.Joinable, nm) {
				fail("network %s: another's: %s", key, nm)
			}
			continue
		}
		driver, _ := n["driver"].(string)
		ipam, _ := n["ipam"].(map[string]any)
		if !strings.HasPrefix(nm, name+"_") || unknownKey(n, topNetworkKeys) != "" || len(ipam) > 0 || (driver != "" && driver != "bridge") {
			fail("network %s: not the project's plain network: %s", key, nm)
		}
	}
}

func checkService(file map[string]any, name, svc string, s map[string]any, p Policy, verb string, paths Paths, fail func(string, ...any)) {
	// Compose reads other forms of these too, which the rules would not read at all.
	for _, want := range []struct {
		key   string
		ok    func(any) bool
		shape string
	}{
		{"labels", isMap, "a map"}, {"networks", isMap, "a map"}, {"volumes", isList, "a list"}, {"security_opt", isList, "a list"},
		{"cap_add", isList, "a list"}, {"ports", isList, "a list"}, {"group_add", isList, "a list"},
		{"post_start", isList, "a list"}, {"pre_stop", isList, "a list"}, {"deploy", isMap, "a map"}, {"logging", isMap, "a map"},
	} {
		if v := s[want.key]; v != nil && !want.ok(v) {
			fail("%s: %s must be %s", svc, want.key, want.shape)
		}
	}
	// A list entry that interpolated to nothing renders as a key compose then refuses.
	if labels, ok := s["labels"].(map[string]any); ok {
		delete(labels, "")
	}
	switch b := s["build"]; {
	case b == nil:
	case verb == "build":
		block, _ := b.(map[string]any)
		checkBuild(svc, block, p, paths, fail)
		// A build writes the tag it names, in the server's one image store.
		if image, _ := s["image"].(string); image != "" {
			repo, tag, _ := strings.Cut(image, ":")
			if !p.OwnName(repo) || strings.HasPrefix(tag, "devopsy-") {
				fail("%s: build: an image not this project's: %s", svc, image)
			}
		}
	case verb == "pull":
		// A pull has only to skip what is built: nothing of the block is read.
		s["build"] = map[string]any{"context": "."}
	default:
		// A build is its own request. Its image keeps compose's name.
		delete(s, "build")
		if s["image"] == nil {
			s["image"] = name + "-" + svc
		}
	}
	for _, k := range sortedKeys(s) {
		if !slices.Contains(serviceKeys, k) && !strings.HasPrefix(k, "x-") {
			fail("%s: %s", svc, k)
		}
	}
	// The name goes to Docker's commands as a word, so it is none of their flags.
	if image, ok := s["image"].(string); ok && strings.HasPrefix(image, "-") {
		fail("%s: an image named like a flag: %s", svc, image)
	}
	checkIdentity(svc, s, p, fail)
	checkLimits(svc, s, fail)
	if nm, _ := s["network_mode"].(string); nm != "" && nm != "none" && nm != "bridge" {
		fail("%s: network_mode %s", svc, nm)
	}
	if len(list(s["ports"])) > 0 {
		fail("%s: ports: a project publishes none, Traefik reaches it", svc)
	}
	if d, ok := s["deploy"].(map[string]any); ok {
		for _, k := range sortedKeys(d) {
			if k != "resources" {
				fail("%s: deploy holds only resources", svc)
			}
		}
	}
	if len(mapOf(s["sysctls"])) > 0 || len(list(s["sysctls"])) > 0 || len(list(s["group_add"])) > 0 {
		fail("%s: sysctls or group_add", svc)
	}
	if l, ok := s["logging"].(map[string]any); ok {
		if d, _ := l["driver"].(string); d != "" && d != "json-file" && d != "local" {
			fail("%s: a log driver: %s", svc, d)
		}
	}
	checkLabels(svc, s, p, fail)
	// The image a start runs is the one whose labels the door read: compose pulling or
	// building one of its own at the start would run another (checkImageLabels).
	if pp, _ := s["pull_policy"].(string); pp != "" && pp != "missing" && pp != "never" && pp != "if_not_present" {
		fail("%s: pull_policy %s: pull or build first, then start", svc, pp)
	}
	for _, key := range []string{"post_start", "pre_stop"} {
		for _, h := range list(s[key]) {
			hook, _ := h.(map[string]any)
			for _, k := range sortedKeys(hook) {
				if k != "command" {
					fail("%s: a hook may hold only its command", svc)
				}
			}
		}
	}
	checkAliases(file, svc, s, p, fail)
	for _, m := range list(s["volumes"]) {
		v, ok := m.(map[string]any)
		if !ok {
			fail("%s: a mount must be a map", svc)
			continue
		}
		// A bind's propagation reaches the host's mounts, whatever its path.
		if k := unknownKey(v, mountKeys); k != "" {
			fail("%s: a mount: %s", svc, k)
		}
		for key, known := range map[string][]string{"bind": bindKeys, "volume": volumeKeys, "tmpfs": tmpfsKeys} {
			if opts, ok := v[key].(map[string]any); ok {
				if k := unknownKey(opts, known); k != "" {
					fail("%s: a mount's %s: %s", svc, key, k)
				}
			} else if v[key] != nil {
				fail("%s: a mount's %s must be a map", svc, key)
			}
		}
		switch t, _ := v["type"].(string); t {
		case "bind":
			checkBind(svc, name, v, p, paths, fail)
		case "volume", "tmpfs":
		default:
			fail("%s: a mount of type %s", svc, t)
		}
	}
}

// ulimitKeys are limits, deny by default: rtprio and nice are the opposite, a priority
// over every other project's container on the host.
var ulimitKeys = []string{"as", "core", "cpu", "data", "fsize", "locks", "memlock", "msgqueue", "nofile", "nproc", "rss", "sigpending", "stack"}

// resourceKeys are a limit's or a reservation's: a device is the host's, whatever key.
var resourceKeys = []string{"cpus", "memory", "pids"}

func checkLimits(svc string, s map[string]any, fail func(string, ...any)) {
	if v := s["ulimits"]; v != nil {
		if !isMap(v) {
			fail("%s: ulimits must be a map", svc)
		} else if k := unknownKey(mapOf(v), ulimitKeys); k != "" {
			fail("%s: ulimits: %s", svc, k)
		}
	}
	resources := mapOf(mapOf(s["deploy"])["resources"])
	for _, key := range sortedKeys(resources) {
		if key != "limits" && key != "reservations" {
			fail("%s: deploy resources: %s", svc, key)
			continue
		}
		if k := unknownKey(mapOf(resources[key]), resourceKeys); k != "" {
			fail("%s: deploy resources: %s", svc, k)
		}
	}
}

func checkIdentity(svc string, s map[string]any, p Policy, fail func(string, ...any)) {
	user, _ := s["user"].(string)
	if why := p.userRefused(user); why != "" {
		fail("%s: user %s", svc, why)
	}
	// Without no-new-privileges a setuid image writes a root binary onto the host.
	opts := list(s["security_opt"])
	if len(opts) != 1 || (opts[0] != "no-new-privileges" && opts[0] != "no-new-privileges:true") {
		fail("%s: security_opt must be no-new-privileges alone, as security_opt: [no-new-privileges]", svc)
	}
	for _, c := range list(s["cap_add"]) {
		if c != "NET_BIND_SERVICE" {
			fail("%s: cap_add %v", svc, c)
		}
	}
}

// userRefused says why a container may not run as user, or nothing: a user is numbers
// only, since a name is the image's to map (to 0, say), and never root or an id of the
// host's (ForbiddenIDs), whose processes it would reach through /proc. One check for a
// service's user: and for exec's -u.
func (p Policy) userRefused(user string) string {
	m := numericUser.FindStringSubmatch(user)
	if m == nil {
		return fmt.Sprintf(`%q: <uid>:<gid> in numbers, for example "10001:10001"`, user)
	}
	uid, _ := strconv.Atoi(m[1])
	gid, _ := strconv.Atoi(m[2])
	if uid == 0 || gid == 0 || slices.Contains(p.ForbiddenIDs, uid) || slices.Contains(p.ForbiddenIDs, gid) {
		return user + ": a host id"
	}
	return ""
}

func checkLabels(svc string, s map[string]any, p Policy, fail func(string, ...any)) {
	labels, _ := s["labels"].(map[string]any)
	for _, k := range sortedKeys(labels) {
		if strings.HasPrefix(k, "com.docker.compose.") || k == envLabel || k == taskLabel || k == DoorLabel {
			fail("%s: label %s", svc, k)
		}
		// A role is the admin's to give: others import from whoever claims it.
		if v, _ := labels[k].(string); k == "devopsy.role" && !slices.Contains(p.Roles, v) {
			fail("%s: the role %s is not this project's", svc, v)
		}
	}
}

// checkAliases refuses an alias on a network others share: it would answer for
// their names. So would the service's own name, which compose makes an alias on each
// of its networks: there it is one of this project's (OwnName).
func checkAliases(file map[string]any, svc string, s map[string]any, p Policy, fail func(string, ...any)) {
	networks, _ := file["networks"].(map[string]any)
	nets, _ := s["networks"].(map[string]any)
	if _, ok := s["networks"]; !ok && s["network_mode"] == nil {
		nets = map[string]any{"default": nil} // compose's own, unless the file makes it shared
	}
	for _, net := range sortedKeys(nets) {
		top, _ := networks[net].(map[string]any)
		opts, _ := nets[net].(map[string]any)
		if truthy(top["external"]) && !p.OwnName(svc) {
			fail("%s: on the shared network %s, a service is named %s-<name>: compose answers for its name there", svc, net, p.Project)
		}
		if k := unknownKey(opts, netKeys); k != "" {
			fail("%s: network %s: %s", svc, net, k)
		}
		if truthy(top["external"]) && len(list(opts["aliases"])) > 0 {
			fail("%s: an alias on the shared network %s", svc, net)
		}
		// So is another's address: its owner's next start then fails on it.
		if truthy(top["external"]) && (opts["ipv4_address"] != nil || opts["ipv6_address"] != nil) {
			fail("%s: a fixed address on the shared network %s", svc, net)
		}
	}
}

// checkBind allows an entry of this compose name, or a read-only part of the
// project's tree, and leaves in the file a path with no part the project user
// can change: the daemon resolves a bind at each start, so a link swapped in
// later would otherwise take a restart anywhere.
func checkBind(svc, name string, v map[string]any, p Policy, paths Paths, fail func(string, ...any)) {
	src, _ := v["source"].(string)
	real, err := paths.Real(src)
	if err != nil {
		fail("%s: %s: %v", svc, src, err)
		return
	}
	// Believed only clean: a `..` in it would mean something else to the daemon.
	if !cleanPath(real) {
		fail("%s: a host path: %s", svc, src)
		return
	}
	if filepath.Dir(real) == p.Mnt(name) {
		v["source"] = real
		return
	}
	if p.InTree(real) && truthy(v["read_only"]) {
		sealed, err := paths.Seal(real)
		if err != nil {
			fail("%s: sealing %s: %v", svc, src, err)
			return
		}
		v["source"] = sealed
		return
	}
	fail("%s: a host path: %s: a bind is an entry of mnt (./mnt/<name>), or read-only in the release", svc, src)
}

func checkBuild(svc string, b map[string]any, p Policy, paths Paths, fail func(string, ...any)) {
	for _, k := range sortedKeys(b) {
		if !slices.Contains(buildKeys, k) {
			fail("%s: build: %s", svc, k)
		}
	}
	ctx, _ := b["context"].(string)
	real, err := paths.Real(ctx)
	if err != nil {
		fail("%s: build: %s: %v", svc, ctx, err)
		return
	}
	if !p.InTree(real) {
		fail("%s: build: a context outside its release: %s", svc, ctx)
		return
	}
	dockerfile, _ := b["dockerfile"].(string)
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	df, err := paths.Real(filepath.Join(real, dockerfile))
	if err != nil {
		fail("%s: build: %s: %v", svc, dockerfile, err)
		return
	}
	if !strings.HasPrefix(df, real+"/") {
		fail("%s: build: a Dockerfile outside its context: %s", svc, dockerfile)
		return
	}
	sealed, err := paths.Seal(real)
	if err != nil {
		fail("%s: sealing %s: %v", svc, ctx, err)
		return
	}
	b["context"] = sealed
	b["dockerfile"] = strings.TrimPrefix(df, real+"/")
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func cleanPath(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p
}

func isMap(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

func isList(v any) bool {
	_, ok := v.([]any)
	return ok
}

func truthy(v any) bool {
	b, ok := v.(bool)
	return ok && b
}
