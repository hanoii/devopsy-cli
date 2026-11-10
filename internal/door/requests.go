package door

import (
	"encoding/csv"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/images"
)

// Known says what devopsy's own util containers run, from its constants: they are root
// on the host's files, so the door's most careful request (forms.go).
type Known struct {
	// Image is devopsy's util image.
	Image string
	// Dockerfile is what the door builds it from.
	Dockerfile string
	// The scripts, each with its own mounts and arguments.
	Owners, Export, Import, ReflinkProbe, SnapshotTar, Space, Share, Daemon, Restore, Receive string
	// RsyncCopy is how every rsync starts.
	RsyncCopy []string
	// Clone says whether a script is a reflink clone's, which is computed.
	Clone func(script string) bool
}

// CloneScriptForm says whether a script holds nothing but what a reflink clone
// does (cmd/devopsy's cloneScript): into /c, from /m, of plain names.
func CloneScriptForm(script string) bool {
	lines := strings.Split(strings.TrimRight(script, "\n"), "\n")
	if len(lines) < 2 || lines[0] != "set -e" || lines[1] != "mkdir -p /c/mnt /c/volumes" {
		return false
	}
	for _, line := range lines[2:] {
		if !cloneLine.MatchString(line) && !cloneDir.MatchString(line) {
			return false
		}
	}
	return true
}

var (
	cloneDir  = regexp.MustCompile(`^mkdir -p '/c/volumes/` + word + `'$`)
	cloneCopy = `cp -a --reflink=always '/m/(mnt/` + word + `|volumes/` + word + `/\.)' '/c/(mnt/|volumes/` + word + `/)'`
	cloneLine = regexp.MustCompile(`^` + cloneCopy + ` \|\| ` + cloneCopy + `$`)
)

// Daemon is what a request asks of Docker to decide.
type Daemon interface {
	// Inspect gives a container's full id and labels; an error if there is none.
	Inspect(target string) (id string, labels map[string]string, err error)
	// Containers lists every container's full id, stopped ones included.
	Containers() ([]string, error)
	// Image gives an image's id and names; an error if there is none.
	Image(ref string) (id string, tags []string, err error)
}

// Host is what a request asks of the project's tree and the door's space, on
// the project user's rights where the tree is concerned.
type Host interface {
	Paths
	// IsDir says whether a path of the project's is a directory.
	IsDir(path string) bool
	// CopyIn copies a path of the project's into a new directory of the door's, out
	// of that user's reach once done, and returns the copy's path.
	CopyIn(path string, dir bool) (string, error)
	// WorkDir makes a directory of the door's, which the project user may read.
	WorkDir() (string, error)
}

// Stream sends a file of the project's through a container with no mounts into
// a path of the project's, each end on that user's rights.
type Stream struct {
	From, To string
	Docker   []string
}

// Collect is what a container wrote for the project's tree: Dir, the door's, is
// copied to Dest by the project user once it ended.
type Collect struct{ Dir, Dest string }

// Action is what a request comes to.
type Action struct {
	// Docker is what to run. Exec'd unless Collect has something to do after.
	Docker []string
	// Stdin replaces what the caller sent.
	Stdin string
	// Collect is done after Docker.
	Collect []Collect
	// Remove are paths the project user removes, by its own rights.
	Remove []string
	// Stream, if set, is all there is to do.
	Stream *Stream
	// Output is the answer when Docker is empty.
	Output string
	// Exit is the exit code when Docker is empty.
	Exit int
	// Claims are images the project must claim before Docker runs: each fetched by the door
	// with its registry logins and loaded, its id then recorded as the project's
	// (Policy.Images). One that does not load refuses the request.
	Claims []string
	// Regrant are entries a root container writes, given back to the project user
	// once it exits: a copy's modes narrow its ACL there (the door's regrant).
	Regrant []string
	// Removes are containers whose writable entries are given back once removed: a
	// detached one's copy ends with it.
	Removes []string
	// Cleanup are directories of the door's that go after Docker.
	Cleanup []string
	// Entries are what Docker's mounts need there, which the door makes first.
	Entries []EntryToMake
}

// EntryToMake is an entry of a mnt, a directory or a file.
type EntryToMake struct {
	Path string
	File bool
}

const (
	projectLabel = "com.docker.compose.project"
	envLabel     = "devopsy.env"
	taskLabel    = "devopsy.task"
	// DoorLabel is on every container the door makes, its value the project user it made it
	// for; a file or a caller never sets it, and an image under devopsy. refuses.
	DoorLabel = "devopsy.door"
)

// ownLabels says whether a container's labels make it this project's: one the door made for
// this user, under one of its names (compose's project, or the environment devopsy's own
// containers carry). Those two labels are anyone's to set outside the door: a container the
// deploy user made under this project's names runs as root with any mount.
func (p Policy) ownLabels(labels map[string]string) bool {
	return labels[DoorLabel] == p.User && p.underName(labels)
}

func (p Policy) underName(labels map[string]string) bool {
	return p.OwnName(labels[projectLabel]) || p.OwnName(labels[envLabel])
}

// Foreign are the containers, by id with their labels, under this project's names that the
// door did not make for its user, sorted: compose acts on whatever its project's label
// finds, so none of its commands runs while one is there.
func (p Policy) Foreign(containers map[string]map[string]string) []string {
	var ids []string
	for id, labels := range containers {
		if p.underName(labels) && labels[DoorLabel] != p.User {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// Label puts the door's label on every service of a checked file, so each container compose
// makes from it says it is the door's (DoorLabel).
func (p Policy) Label(file map[string]any) {
	services, _ := file["services"].(map[string]any)
	for _, svc := range services {
		s, ok := svc.(map[string]any)
		if !ok {
			continue
		}
		labels, _ := s["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
		}
		labels[DoorLabel] = p.User
		s["labels"] = labels
	}
}

func (p Policy) ownContainer(d Daemon, target string) bool {
	_, ok := p.ownContainerID(d, target)
	return ok
}

// ownContainerID is the full id of the container target names when it is this project's:
// what Docker is then given, never the caller's word, a name or an id's prefix Docker would
// resolve again, to another container by then.
func (p Policy) ownContainerID(d Daemon, target string) (string, bool) {
	id, labels, err := d.Inspect(target)
	return id, err == nil && id != "" && p.ownLabels(labels)
}

// ownContainerIDs are targets' full ids when every one is this project's container.
func (p Policy) ownContainerIDs(d Daemon, targets []string) ([]string, string, bool) {
	var ids []string
	for _, t := range targets {
		id, ok := p.ownContainerID(d, t)
		if !ok {
			return nil, t, false
		}
		ids = append(ids, id)
	}
	return ids, "", true
}

// flagValue pulls a flag taking a value out of argv, in both spellings.
func flagValues(argv []string, names ...string) (values, rest []string) {
	for i := 0; i < len(argv); i++ {
		name, value, has := strings.Cut(argv[i], "=")
		if !slices.Contains(names, name) {
			rest = append(rest, argv[i])
			continue
		}
		if !has {
			i++
			if i >= len(argv) {
				return values, rest
			}
			value = argv[i]
		}
		values = append(values, value)
	}
	return values, rest
}

// Decide says what a Docker command other than compose comes to, for this
// project: only what a release's own scripts ask, for the project's own things.
func Decide(argv []string, p Policy, d Daemon, h Host, known Known) (*Action, error) {
	if len(argv) == 0 {
		return nil, refuse("no docker command")
	}
	cmd, args := argv[0], argv[1:]
	switch cmd {
	case "version":
		return &Action{Docker: argv}, nil
	case "ps":
		return decidePS(args, p, d)
	case "inspect":
		return decideInspect(args, p, d)
	case "network":
		return decideNetwork(args, p)
	case "image":
		return decideImage(args, p, d, known)
	case "volume":
		return decideVolume(args, p)
	case "tag":
		// Onto a hold of its own, a release's bookmark, the one name devopsy tags: its build's
		// plain name is the door's build to write.
		if len(args) != 2 || !p.ownHold(args[1]) || strings.HasPrefix(args[0], "-") {
			return nil, refuse("docker tag %s: only onto a hold of this project's", strings.Join(args, " "))
		}
		// Its source is an image this project names too: its own, or claimed. An own one
		// by the id that made it so, a claimed one by the digest its claim got (Claims):
		// a name another writes may move before Docker tags.
		a := &Action{Docker: argv}
		id, owned := p.ownedImage(d, args[0], known)
		if owned && id != "" {
			a.Docker = []string{"tag", id, args[1]}
		}
		if !owned {
			if err := p.Claimable(args[0]); err != nil {
				return nil, err
			}
			a.Claims = []string{args[0]}
		}
		return a, nil
	case "rm":
		return decideRM(args, p, d)
	case "pull":
		// An image a rollback or restart needs again: it could name it in a compose file.
		// The door's claim fetches it, with the project's logins, and records it.
		if len(args) != 2 || args[0] != "-q" || strings.HasPrefix(args[1], "-") {
			return nil, noDocker(argv)
		}
		if err := p.Claimable(args[1]); err != nil {
			return nil, err
		}
		return &Action{Claims: []string{args[1]}}, nil
	case "exec":
		// A sync's own daemon serving one connection over the pipe, and nothing else: the
		// daemon has no network, and the push relays to it from a socket of its own.
		if len(args) != 2+len(DaemonServe) || args[0] != "-i" || !slices.Equal(args[2:], DaemonServe) {
			return nil, noDocker(argv)
		}
		id, ok := p.ownContainerID(d, args[1])
		if !ok {
			return nil, noDocker(argv)
		}
		return &Action{Docker: slices.Concat([]string{"exec", "-i", id}, DaemonServe)}, nil
	case "build":
		return decideUtilBuild(args, known)
	case "run":
		return decideRun(args, p, h, known)
	}
	return nil, noDocker(argv)
}

func noDocker(argv []string) error {
	return refuse("a project user has no docker: docker %s: an admin may grant more", strings.Join(argv, " "))
}

func decidePS(args []string, p Policy, d Daemon) (*Action, error) {
	// devopsy's look at what runs, for imports: ids tell nothing, and inspect
	// gives only what another project exports.
	if slices.Equal(args, []string{"-q", "--no-trunc"}) {
		return &Action{Docker: append([]string{"ps"}, args...)}, nil
	}
	filters, rest := flagValues(args, "--filter", "-f")
	formats, rest := flagValues(rest, "--format")
	// Who holds a role, and nothing else about it.
	if len(rest) == 0 && len(filters) == 1 && strings.HasPrefix(filters[0], "label=devopsy.role=") &&
		slices.Equal(formats, []string{`{{.Label "com.docker.compose.project"}}`}) {
		return &Action{Docker: append([]string{"ps"}, args...)}, nil
	}
	// Anything else is cut to this project's by id, the only key of the door's: filters
	// of one key widen each other, so a caller's id filters go.
	for _, a := range rest {
		if !slices.Contains([]string{"-a", "--all", "-q", "--quiet", "--no-trunc", "-aq", "-qa"}, a) {
			return nil, refuse("docker ps: %s", a)
		}
	}
	docker := append([]string{"ps"}, rest...)
	for _, f := range formats {
		docker = append(docker, "--format", f)
	}
	for _, f := range filters {
		// Docker's CLI trims a filter's key and lowercases it: `ID=` and ` id =` are `id=`.
		if key, _, _ := strings.Cut(f, "="); strings.ToLower(strings.TrimSpace(key)) != "id" {
			docker = append(docker, "--filter", f)
		}
	}
	ids, err := d.Containers()
	if err != nil {
		return nil, refuse("cannot read what runs: %v", err)
	}
	cut := false
	for _, id := range ids {
		if p.ownContainer(d, id) {
			docker = append(docker, "--filter", "id="+id)
			cut = true
		}
	}
	if !cut {
		// No container has this id: the caller's format, and no rows.
		docker = append(docker, "--filter", "id=0000000000000000")
	}
	return &Action{Docker: docker}, nil
}

// exportedLabel says what of another project's labels anyone may read.
func exportedLabel(key string) bool {
	return key == "devopsy.role" || key == projectLabel || strings.HasPrefix(key, "devopsy.export.")
}

func decideInspect(args []string, p Policy, d Daemon) (*Action, error) {
	formats, targets := flagValues(args, "--format", "-f")
	for _, t := range targets {
		if strings.HasPrefix(t, "-") {
			return nil, refuse("docker inspect: %s", t)
		}
	}
	// The labels' form, for imports: another's cut to what it exports.
	if slices.Equal(formats, []string{"{{.Id}} {{json .Config.Labels}}"}) {
		var out strings.Builder
		for _, t := range targets {
			id, labels, err := d.Inspect(t)
			if err != nil {
				continue
			}
			if !p.ownLabels(labels) {
				for k := range labels {
					if !exportedLabel(k) {
						delete(labels, k)
					}
				}
			}
			b, err := json.Marshal(labels)
			if err != nil {
				return nil, err
			}
			out.WriteString(id + " " + string(b) + "\n")
		}
		return &Action{Output: out.String()}, nil
	}
	ids, t, ok := p.ownContainerIDs(d, targets)
	if !ok {
		return nil, refuse("not this project's: docker inspect %s", t)
	}
	// A container's, by its type and the id read: untyped, Docker answers for an image or a
	// volume of the same name, and by the caller's word for whatever it names by then.
	docker := []string{"inspect", "--type", "container"}
	for _, f := range formats {
		docker = append(docker, "--format", f)
	}
	return &Action{Docker: append(docker, ids...)}, nil
}

func decideNetwork(args []string, p Policy) (*Action, error) {
	if len(args) == 0 || args[0] != "inspect" {
		return nil, refuse("docker network %s", strings.Join(args, " "))
	}
	_, targets := flagValues(args[1:], "--format", "-f")
	for _, t := range targets {
		if strings.HasPrefix(t, "-") {
			return nil, refuse("docker network inspect: %s", t)
		}
		if !p.OwnName(t) && !slices.Contains(p.Joinable, t) {
			return nil, refuse("not this project's network: %s", t)
		}
	}
	return &Action{Docker: append([]string{"network"}, args...)}, nil
}

func decideImage(args []string, p Policy, d Daemon, known Known) (*Action, error) {
	if len(args) == 0 {
		return nil, refuse("docker image")
	}
	switch args[0] {
	case "inspect":
		// An image holds its project's configuration (its Env, a build argument
		// included): only one this project's, or claimed with its own logins.
		// An image of its own by its id goes to Docker by the id the door read, as a tag's
		// source does: the name is anyone's on the server to move before Docker reads it.
		formats, targets := flagValues(args[1:], "--format", "-f")
		a := &Action{Docker: []string{"image", "inspect"}}
		for _, f := range formats {
			a.Docker = append(a.Docker, "--format", f)
		}
		for _, t := range targets {
			if strings.HasPrefix(t, "-") {
				return nil, refuse("docker image inspect %s", t)
			}
			id, owned := p.ownedImage(d, t, known)
			switch {
			case owned && id != "":
				a.Docker = append(a.Docker, id)
				continue
			case !owned:
				if err := p.Claimable(t); err != nil {
					return nil, err
				}
				a.Claims = append(a.Claims, t)
			}
			a.Docker = append(a.Docker, t)
		}
		return a, nil
	case "rm":
		// A hold, or a name it built. `image prune` is server-wide: the admin's.
		own := []string{"image", "rm"}
		for _, t := range args[1:] {
			if strings.HasPrefix(t, "-") {
				return nil, refuse("docker image rm %s", t)
			}
			// Any other name is the server's (a registry's, or another project's hold, its
			// reference count on the image it runs): its pruning asks, and the name may be
			// others'. Left as it is, and nothing a release should hear as a failure.
			if p.OwnRef(t) {
				own = append(own, t)
			}
		}
		if len(own) == 2 {
			return &Action{}, nil
		}
		return &Action{Docker: own}, nil
	case "prune":
		// devopsy's tidying up, server-wide, so the admin's: for this user nothing to do.
		return &Action{}, nil
	}
	return nil, refuse("docker image %s: the admin's", strings.Join(args, " "))
}

// reservedVolumeLabel says whether a volume's label is compose's or devopsy's to set, which
// they read as whose it is and what holds data: a file's or a request's would say otherwise.
func reservedVolumeLabel(k string) bool {
	return strings.HasPrefix(k, "com.docker.compose.") || strings.HasPrefix(k, "devopsy.")
}

func decideVolume(args []string, p Policy) (*Action, error) {
	// A volume of this project's, as compose would make it: its own two labels and any
	// others of the file's, never a driver or options, which CheckFile refuses too.
	if len(args) >= 6 && args[0] == "create" && len(args)%2 == 0 {
		project, _ := strings.CutPrefix(args[2], projectLabel+"=")
		key, keyed := strings.CutPrefix(args[4], "com.docker.compose.volume=")
		name := args[len(args)-1]
		ok := args[1] == "--label" && args[3] == "--label" && keyed && name == project+"_"+key && p.OwnVolume(name) && p.OwnName(project) && Word.MatchString(key)
		for i := 5; ok && i+1 < len(args)-1; i += 2 {
			k, _, _ := strings.Cut(args[i+1], "=")
			ok = args[i] == "--label" && !reservedVolumeLabel(k)
		}
		if ok {
			return &Action{Docker: append([]string{"volume"}, args...)}, nil
		}
		return nil, refuse("docker volume create: not this project's")
	}
	if len(args) == 0 || args[0] != "inspect" {
		return nil, refuse("docker volume %s", strings.Join(args, " "))
	}
	_, targets := flagValues(args[1:], "--format", "-f")
	// A volume by compose's shape for one of this project's (OwnVolume), as a run's mount is.
	for _, t := range targets {
		if strings.HasPrefix(t, "-") || !p.OwnVolume(t) {
			return nil, refuse("docker volume inspect %s", t)
		}
	}
	return &Action{Docker: append([]string{"volume"}, args...)}, nil
}

func decideRM(args []string, p Policy, d Daemon) (*Action, error) {
	docker := []string{"rm"}
	var removes []string
	for _, a := range args {
		switch {
		case a == "-f" || a == "--force":
			docker = append(docker, a)
		case strings.HasPrefix(a, "-"):
			return nil, refuse("docker rm: %s", a)
		default:
			// By the id read, never the caller's word, which Docker would resolve again.
			id, ok := p.ownContainerID(d, a)
			if !ok {
				return nil, refuse("not this project's: docker rm %s", a)
			}
			removes = append(removes, id)
		}
	}
	return &Action{Docker: append(docker, removes...), Removes: removes}, nil
}

// decideUtilBuild builds only devopsy's util image, from the door's own
// Dockerfile: what the caller sends on stdin is not read.
func decideUtilBuild(args []string, known Known) (*Action, error) {
	allowed := []string{"-q", "--pull", "--no-cache", "-t", known.Image, "-"}
	for _, a := range args {
		if !slices.Contains(allowed, a) {
			return nil, refuse("docker build %s: a build is a release's", strings.Join(args, " "))
		}
	}
	if !slices.Contains(args, known.Image) || len(args) == 0 || args[len(args)-1] != "-" {
		return nil, refuse("docker build %s: a build is a release's", strings.Join(args, " "))
	}
	return &Action{Docker: append([]string{"build"}, args...), Stdin: known.Dockerfile}, nil
}

// ownHold says whether a tag is a hold of this project's,
// <repo>:devopsy-<compose name>-<id>: nothing moves a tag anyone else reads.
// OwnsImage says whether ref is this project's image: devopsy's util image (known), a
// name only this project writes (its build's or a hold of its own, OwnRef), an image
// another of whose names is so, or one whose id the project claimed (Images). Any other is
// claimed first (Action.Claims): fetched with this project's own registry logins, so an
// image another project pulled with its own is never this one's by being on disk.
func (p Policy) OwnsImage(d Daemon, ref string, known Known) bool {
	_, owned := p.ownedImage(d, ref, known)
	return owned
}

// ownedImage is OwnsImage, with the id it read when it read one ("" for a name only this
// project writes, or devopsy's util image): what a request then acts on by id, read once.
func (p Policy) ownedImage(d Daemon, ref string, known Known) (string, bool) {
	if ref == known.Image || p.OwnRef(ref) {
		return "", true
	}
	id, tags, err := d.Image(ref)
	if err != nil {
		return "", false
	}
	return id, p.OwnsImageOf(id, tags)
}

// OwnsImageOf says whether the image of id and tags, as one inspect read them, is this
// project's: claimed by its id, or named by a name only this project writes.
func (p Policy) OwnsImageOf(id string, tags []string) bool {
	return slices.Contains(p.Images, id) || slices.ContainsFunc(tags, p.OwnRef)
}

// OwnContainerName says whether a container's name is this project's: a compose name of its
// own, or one of devopsy's containers of an environment of its own,
// devopsy-<word>-<compose name>-<part> (cmd/devopsy's newContainer). A name says whose it is
// by itself: any other could take the name another project's next container needs.
func (p Policy) OwnContainerName(name string) bool {
	if p.OwnName(name) {
		return true
	}
	rest, ok := strings.CutPrefix(name, "devopsy-")
	if !ok {
		return false
	}
	word, rest, ok := strings.Cut(rest, "-")
	return ok && word != "" && p.OwnName(rest)
}

// OwnRef says whether ref is a name only this project writes: its build's, which the door
// names for one of its environments (images.DoorBuild, under a host no registry serves), or
// a hold of its own (a release's bookmark). Its image stays this project's until it moves the
// name; any other name is a registry's, or another project's, and anyone's on the server to
// move by a pull.
func (p Policy) OwnRef(ref string) bool {
	if strings.Contains(ref, "@") {
		return false
	}
	if p.ownHold(ref) {
		return true
	}
	name, _, ok := images.IsDoorBuild(ref)
	_, hold := images.HoldOf(ref)
	return ok && p.OwnName(name) && !hold
}

// Claimable refuses a claim, or any pull of a project's, that would write a name the door
// takes for one a project alone writes (OwnRef): a hold, any project's, this one's included
// (a rollback and a restart run it by name), or a build's name under images.DoorHost. A claim
// lands where a registry's manifest points, and Docker loads no layer it holds: landing on
// such a name, it would make another's image a project's with no claim and no proof. Every
// other name is a registry's, and what a claim of it writes is checked first. A claim by digest
// writes no name, but its host is still one the door's clients and the project's builder
// contact for the project, so the server's own and the metadata addresses are refused first,
// by digest or not.
func (p Policy) Claimable(ref string) error {
	if ownHost(registryHost(ref)) {
		return refuse("%s: a registry on the server's own or a metadata address, which the door would contact for this project", ref)
	}
	if strings.Contains(ref, "@") {
		return nil
	}
	repo, _ := images.SplitRef(ref)
	if _, hold := images.HoldOf(ref); hold {
		return refuse("%s: a hold, which devopsy alone writes, never a pull", ref)
	}
	if strings.HasPrefix(repo, images.DoorHost+"/") {
		return refuse("%s: a build's name, which the door alone writes", ref)
	}
	return nil
}

func (p Policy) ownHold(ref string) bool {
	rest, ok := images.HoldOf(ref)
	return ok && p.OwnName(rest) && holdName.MatchString(rest)
}

var holdName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*-[A-Za-z0-9_.-]+$`)

var (
	chownFlag = regexp.MustCompile(`^--chown=\d+:\d+$`)
	// A util container's mounts sit where devopsy's own commands look, never over the
	// image's binaries, which would then run as root.
	mountTarget = regexp.MustCompile(`^/(mnt|m|v|src|dst|c|d|s|o|in|x|vol)(/` + word + `)*$`)
	// A source goes back into a --mount and a -v by these words, so none of their syntax.
	mountSource = regexp.MustCompile(`^/[A-Za-z0-9_./+@=-]*$`)
	daemonConf  = regexp.MustCompile(`^RSYNCD_CONF=uid = 0\ngid = 0\nuse chroot = yes\nread only = yes\n(\[` + word + `\]\npath = /(mnt|vol)(/` + word + `)?\n(include = /` + word + `\nexclude = \*\n)?)*$`)
)

type mount struct {
	source, target string
	readOnly       bool
	volumeFlag     bool
	kind           string
}

// decideRun allows a util container only: root on the host's files, so one of devopsy's
// own commands, with the mounts it has, and no path a project user can change. Docker
// resolves a bind when the container starts, so a swap there would hand it anything.
func decideRun(args []string, p Policy, h Host, known Known) (*Action, error) {
	var out []string
	var mounts []mount
	network, conf, image := "", "", -1
scan:
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, error) {
			i++
			if i >= len(args) {
				return "", refuse("docker run: %s takes a value", a)
			}
			return args[i], nil
		}
		switch a {
		case "--rm", "-i", "--interactive", "-d":
			out = append(out, a)
		case "--name":
			v, err := value()
			if err != nil {
				return nil, err
			}
			if !p.OwnContainerName(v) {
				return nil, refuse("docker run: another's name: %s", v)
			}
			out = append(out, a, v)
		case "--label":
			v, err := value()
			if err != nil {
				return nil, err
			}
			k, val, _ := strings.Cut(v, "=")
			if k != envLabel && k != taskLabel {
				return nil, refuse("docker run: a label: %s", v)
			}
			if k == envLabel && !p.OwnName(val) {
				return nil, refuse("docker run: another's label: %s", v)
			}
			out = append(out, a, v)
		case "--network":
			v, err := value()
			if err != nil {
				return nil, err
			}
			if v != "none" {
				return nil, refuse("docker run: network %s", v)
			}
			network = v
			out = append(out, a, v)
		case "-e":
			v, err := value()
			if err != nil {
				return nil, err
			}
			if !daemonConf.MatchString(v) {
				return nil, refuse("docker run: an environment")
			}
			conf = v
			out = append(out, a, v)
		case "--mount":
			v, err := value()
			if err != nil {
				return nil, err
			}
			m, err := parseMount(v)
			if err != nil {
				return nil, err
			}
			mounts = append(mounts, m)
		case "-v", "--volume":
			v, err := value()
			if err != nil {
				return nil, err
			}
			parts := strings.Split(v, ":")
			if len(parts) < 2 || len(parts) > 3 || (len(parts) == 3 && parts[2] != "ro") {
				return nil, refuse("docker run: a volume's options: %s", v)
			}
			m := mount{source: parts[0], target: parts[1], readOnly: len(parts) == 3, volumeFlag: true, kind: "bind"}
			if !strings.HasPrefix(m.source, "/") {
				m.kind = "volume"
			}
			mounts = append(mounts, m)
		case known.Image:
			image = i
			break scan
		default:
			return nil, refuse("docker run: %s", a)
		}
	}
	if image < 0 {
		return nil, refuse("docker run: only devopsy's own image, and only its own scripts")
	}
	out = append(out, "--label", DoorLabel+"="+p.User)
	for _, m := range mounts {
		if !mountTarget.MatchString(m.target) {
			return nil, refuse("docker run: a mount over %s", m.target)
		}
	}
	cmd := args[image+1:]
	if len(cmd) == 5 && cmd[0] == "sh" && cmd[1] == "-c" && cmd[2] == known.Restore {
		return decideRestore(cmd, mounts, out, p, h, known)
	}
	if len(cmd) >= 3 && cmd[0] == "rm" && cmd[1] == "-rf" {
		return decideRemoval(cmd[2:], mounts, p, h, known)
	}
	return decideForm(cmd, mounts, out, network, conf, p, h, known)
}

var removePath = regexp.MustCompile(`^/s/` + word + `(/` + word + `)*$`)

// decideRemoval removes in the project's tree, which is that user's to do with no root; a
// clone behind a link there is root's files, in the door's space. Nothing else.
func decideRemoval(paths []string, mounts []mount, p Policy, h Host, known Known) (*Action, error) {
	if len(mounts) != 1 || mounts[0].target != "/s" || !strings.HasPrefix(mounts[0].source, "/") {
		return nil, refuse("docker run: a removal with other mounts")
	}
	base, err := h.Real(mounts[0].source)
	if err != nil || !p.InTree(base) {
		return nil, refuse("docker run: a removal outside its tree: %s", mounts[0].source)
	}
	a := &Action{}
	var clones []string
	for _, path := range paths {
		if !removePath.MatchString(path) {
			return nil, refuse("docker run: a removal of %s", path)
		}
		target := filepath.Join(base, strings.TrimPrefix(path, "/s/"))
		real, err := h.Real(target)
		if err != nil {
			return nil, refuse("docker run: %s: %v", target, err)
		}
		if rest, ok := strings.CutPrefix(real, p.Clones()+"/"); ok {
			if !CloneDir(rest) {
				return nil, refuse("docker run: a removal inside a clone: %s", target)
			}
			clones = append(clones, "/s/"+rest)
			if parent, err := h.Real(filepath.Dir(target)); err == nil && p.InTree(parent) {
				a.Remove = append(a.Remove, target)
			}
			continue
		}
		if !p.InTree(real) {
			return nil, refuse("docker run: a removal outside its tree: %s", target)
		}
		a.Remove = append(a.Remove, target)
	}
	if len(clones) > 0 {
		a.Docker = slices.Concat([]string{"run", "--rm", "--network", "none"}, RootUtil(), []string{"-v", p.Clones() + ":/s", known.Image, "rm", "-rf"}, clones)
	}
	return a, nil
}

func parseMount(v string) (mount, error) {
	m := mount{}
	// Docker reads a --mount as CSV, so this does: a word it would see as two is refused.
	fields, err := csv.NewReader(strings.NewReader(v)).Read()
	if err != nil {
		return m, refuse("docker run: a mount: %s", v)
	}
	for _, part := range fields {
		key, value, _ := strings.Cut(part, "=")
		switch key {
		case "type":
			m.kind = value
		case "source", "src":
			m.source = value
		case "target", "dst", "destination":
			m.target = value
		case "readonly", "ro":
			m.readOnly = true
		default:
			return m, refuse("docker run: a mount: %s", v)
		}
	}
	if m.kind != "bind" && m.kind != "volume" {
		return m, refuse("docker run: a mount: %s", v)
	}
	return m, nil
}

// decideRestore turns a restore's decompression, a project's own file into its
// own FIFO, into a stream through a container with no mounts: no root and no bind.
func decideRestore(cmd []string, mounts []mount, out []string, p Policy, h Host, known Known) (*Action, error) {
	if len(cmd) != 5 || !Word.MatchString(cmd[4]) || cmd[3] != "sh" || len(mounts) != 2 {
		return nil, refuse("docker run: a restore's stream")
	}
	var from, to string
	for _, m := range mounts {
		real, err := h.Real(m.source)
		if err != nil || !p.InTree(real) {
			return nil, refuse("docker run: a stream of another's")
		}
		switch m.target {
		case "/s":
			from = filepath.Join(real, cmd[4])
		case "/o":
			to = filepath.Join(real, "tar")
		}
	}
	if from == "" || to == "" {
		return nil, refuse("docker run: a restore's stream")
	}
	// Its name and labels, and nothing else of the caller's: no network, and no mounts.
	keep := []string{"--rm"}
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "--name" || out[i] == "--label" {
			keep = append(keep, out[i], out[i+1])
			i++
		}
	}
	docker := slices.Concat([]string{"run", "-i"}, keep, UtilNoPull, []string{"--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", known.Image, "zstd", "-dcq"})
	return &Action{Stream: &Stream{From: from, To: to, Docker: docker}}, nil
}
