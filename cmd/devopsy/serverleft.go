package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/uids"
)

// What --server-info shows of what may be dead or gone on a server, none of it removed by
// devopsy: the facts the deploy user's devopsy reads on the server (leftFacts), and the lines
// the operator's devopsy makes of them (leftEntries), each with its hint.

// leftFacts reads, as the deploy user, what may be dead or gone into info: what --gc would
// remove (its own listing, gcScope.find, counted: --server-info runs on a timer, and sizes and
// the build cache are --gc's own listing's), the images with a tag, the volumes, what a cut
// --destroy left, the snapshot root's environments and the project users with no key that
// still hold environments. The containers are info's, as hostContainers read them, never read
// again. A read that fails is said in its *Unread, never taken for none.
func leftFacts(info *remote.ServerInfo, r remote.Releases, d *remote.Door, trees map[string]string) {
	var containersErr error
	if info.ContainersUnread != "" {
		containersErr = errors.New(info.ContainersUnread)
	}
	g := &gcScope{docker: "docker", root: r.Root, ages: r.GC, door: d, now: time.Now(), counts: true, containers: info.Containers, containersErr: containersErr}
	info.GC = g.find().facts()
	if info.Docker.Error == "" {
		var err error
		if info.Volumes, err = hostVolumes(g.docker); err != nil {
			info.VolumesUnread = err.Error()
		}
		if info.Images, err = hostImages(g.docker, info.Containers); err != nil {
			info.ImagesUnread = err.Error()
		}
	}
	info.Removing = cutDestroys(r.Root, trees)
	if r.Snapshots.Root != "" {
		info.SnapshotDirs = snapshotEnvDirs(r.Snapshots.Root)
	}
	if info.Door != nil && d != nil && len(d.Users) > 0 {
		info.Door.Keyless = keylessUsers(d)
	}
	if info.Door != nil {
		pending := destroysPending()
		for _, who := range slices.Sorted(maps.Keys(pending)) {
			info.Door.Destroying = append(info.Door.Destroying, remote.KeylessUser{User: who, Project: pending[who]})
		}
	}
}

// facts are the plan counted, for --server-info: what --gc would remove.
func (p gcPlan) facts() *remote.GCFacts {
	return &remote.GCFacts{OneOffs: len(p.OneOffs), Snapshots: len(p.Snapshots), Left: len(p.Left), Dangling: len(p.Dangling.Images), Unread: p.Unread}
}

// hostImages are the images with a tag, by docker image ls (imageRows), each with every tag it
// has, its size as it gives it, and how many of containers use it.
func hostImages(docker string, containers []remote.HostContainer) ([]remote.HostImage, error) {
	rows, err := imageRows(docker)
	if err != nil {
		return nil, err
	}
	byID := map[string]*remote.HostImage{}
	var order []string
	for _, r := range rows {
		if strings.Contains(r.Ref, "<none>") {
			continue
		}
		if byID[r.ID] == nil {
			byID[r.ID] = &remote.HostImage{ID: r.ID, Size: r.Size}
			order = append(order, r.ID)
		}
		byID[r.ID].Tags = append(byID[r.ID].Tags, r.Ref)
	}
	for _, c := range containers {
		if img := byID[c.Image]; img != nil {
			img.Containers++
		}
	}
	var imgs []remote.HostImage
	for _, id := range order {
		imgs = append(imgs, *byID[id])
	}
	return imgs, nil
}

// imageRow is a line of docker image ls: an image's id, one of its names (<none>:<none> for
// none) and its size as Docker gives it.
type imageRow struct {
	ID, Ref string
	Size    int64
}

// imageRows are the images docker (its command) lists, a row per name.
func imageRows(docker string) ([]imageRow, error) {
	out, err := exec.Command(docker, "image", "ls", "--no-trunc", "--format", "{{.ID}} {{.Repository}}:{{.Tag}} {{.Size}}").Output()
	if err != nil {
		return nil, dockerFailed("docker image ls", err)
	}
	var rows []imageRow
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		r := imageRow{ID: f[0], Ref: f[1]}
		if len(f) > 2 {
			r.Size = parseDockerSize(f[2])
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// hostVolumes are the volumes docker volume ls gives, with the compose project of their labels,
// whether Docker made them anonymous and whether no container uses them (its dangling filter),
// with no size: Docker sizes a volume only by walking it. Its labels come as one line, k=v
// joined by commas.
func hostVolumes(docker string) ([]remote.HostVolume, error) {
	out, err := exec.Command(docker, "volume", "ls", "--format", "[{{json .Name}},{{json .Labels}}]").Output()
	if err != nil {
		return nil, dockerFailed("docker volume ls", err)
	}
	unused, err := exec.Command(docker, "volume", "ls", "-q", "--filter", "dangling=true").Output()
	if err != nil {
		return nil, dockerFailed("docker volume ls", err)
	}
	dangling := map[string]bool{}
	for _, n := range strings.Fields(string(unused)) {
		dangling[n] = true
	}
	var vs []remote.HostVolume
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var row []string
		if err := json.Unmarshal([]byte(line), &row); err != nil || len(row) != 2 {
			return nil, fmt.Errorf("docker volume ls: %q does not read", line)
		}
		hv := remote.HostVolume{Name: row[0], Unused: dangling[row[0]]}
		for _, kv := range strings.Split(row[1], ",") {
			k, val, _ := strings.Cut(kv, "=")
			switch k {
			case "com.docker.compose.project":
				hv.Project = val
			case "com.docker.volume.anonymous":
				hv.Anonymous = true
			}
		}
		vs = append(vs, hv)
	}
	return vs, nil
}

// cutDestroys are the directories a --destroy cut off while it removed them left under root
// (<base>.rm.<pid>, removeBase), its devopsy gone (as removeMoved, which the next --destroy of
// that environment runs, tells one), in the deploy user's projects (ownProjects), each with
// its size, by path.
func cutDestroys(root string, trees map[string]string) []remote.RemovingDir {
	ps, _ := ownProjects(root, trees, ownedByMe)
	var out []remote.RemovingDir
	look := func(dir string) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			if !removing.MatchString(e.Name()) || !e.IsDir() || destroyRunning(e.Name()) {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			out = append(out, remote.RemovingDir{Path: filepath.ToSlash(rel), Size: treeBytes(path)})
		}
	}
	for _, p := range ps {
		look(p)
		dirs, _ := envDirs(root, p)
		for _, d := range dirs {
			look(d)
		}
	}
	slices.SortFunc(out, func(a, b remote.RemovingDir) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// within says whether path is dir or under it.
func within(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}

// destroyRunning says whether the devopsy of a <base>.rm.<pid> runs: a signal 0 reaches it.
func destroyRunning(name string) bool {
	pid, err := strconv.Atoi(name[strings.LastIndex(name, ".")+1:])
	return err == nil && pid > 0 && syscall.Kill(pid, 0) == nil
}

// snapshotEnvDirs are the paths under the snapshot root that hold an environment's snapshots,
// <project>/[<instance>/]<environment>: a directory with a snapshot's id or the mirror in it.
func snapshotEnvDirs(root string) []string {
	holds := func(dir string) bool {
		entries, _ := os.ReadDir(dir)
		return slices.ContainsFunc(entries, func(e os.DirEntry) bool {
			return e.IsDir() && (remote.ReleaseIDFormat.MatchString(e.Name()) || e.Name() == remote.SnapshotMirror)
		})
	}
	var out []string
	ps, _ := envDirs(root, root)
	for _, p := range ps {
		es, _ := envsUnder(root, p, holds)
		for _, e := range es {
			if rel, err := filepath.Rel(root, e); err == nil {
				out = append(out, filepath.ToSlash(rel))
			}
		}
	}
	return out
}

// keylessUsers are the project users with no key whose project still holds something on the
// server, for --server-info and --project-users list: what a plain remove refuses for and the
// destroy's end checks (heldBy), a read that fails said among it, never taken for nothing. Not
// one whose destroy did not complete (its mark, remote.DoorDestroyingFile): its own line says
// so, and no key takes it back while the mark holds it.
func keylessUsers(d *remote.Door) []remote.KeylessUser {
	var out []remote.KeylessUser
	for _, who := range slices.Sorted(maps.Keys(d.Users)) {
		if keysOf(who) > 0 || fileExists(remote.DoorDestroyingFile(who)) {
			continue
		}
		project := d.Users[who].Project
		held, err := heldBy(d, project, who)
		if err != nil {
			held = append(held, "what it holds, which did not read: "+oneLine(err.Error()))
		}
		if len(held) > 0 {
			out = append(out, remote.KeylessUser{User: who, Project: project, Held: held})
		}
	}
	return out
}

// keysOf is how many keys project user who has.
func keysOf(who string) int {
	data, _ := os.ReadFile(remote.DoorKeysFile(who))
	return len(remote.ParseAuthorizedKeys(string(data)))
}

// keptUserHints are the ways out of a project user with no key that still holds what is under
// its names: its destroy, offered by the server's address ref ("" for none, the way in words),
// and a key added, in words, since a key's command takes the deploy user's address and a file.
func keptUserHints(project, ref string) []report.Remedy {
	key := report.Hint("or a key added for it (--ssh-keys with --for) takes it back")
	if ref == "" {
		return []report.Remedy{report.Hint("a --project-users remove of " + project + " with --destroy removes it with what it holds"), key}
	}
	return []report.Remedy{report.Hint("remove it with what it holds:", "devopsy @"+ref+" --project-users remove "+project+" --destroy"), key}
}

// liveNames are the compose names of what is there, for every report that goes by an
// absence: the release root's environment directories (the deploy user's, never a read of a
// project user's tree, which may answer nothing) and every environment uid claim's (a project
// user's start always has one). ok false when either does not read in full, or the root holds
// no environment: with its disk not mounted, every environment would read as gone.
func liveNames(info remote.ServerInfo) (live map[string]bool, ok bool) {
	live = map[string]bool{}
	for _, e := range info.Environments {
		if e.User == "" {
			live[composeNameOf(e.Path)] = true
		}
	}
	if len(live) == 0 || info.EnvironmentsUnread != "" {
		return nil, false
	}
	for _, c := range info.UIDs {
		if c.Bad != "" && c.Name == info.UIDsDir {
			return nil, false // the claims did not read
		}
		if o, err := uids.ParseOwner(c.Owner); err == nil && o.Env != "" {
			live[o.ComposeName()] = true
		}
	}
	return live, true
}

// goneEnv is what is left of a compose name with no environment and no uid claim.
type goneEnv struct {
	tags, containers []string
	volumes          []remote.HostVolume
	snapshots        string
}

// goneTags are the tags of a release (bookmarks, snapshots' bookmarks, fetch tags, a project
// user's builds) of no live name, by the name they read back as: every tag that NamesTag
// gives a live name is dropped first, and only the rest is read back (NameOfTag), which may
// give a name cut and hashed, or split wrongly. A claim's hold is none: the project's next
// pruning drops it.
func goneTags(imgs []remote.HostImage, live map[string]bool) map[string][]string {
	gone := map[string][]string{}
	names := slices.Collect(maps.Keys(live))
	for _, img := range imgs {
		for _, tag := range img.Tags {
			if name, _, ok := images.IsDoorBuild(tag); ok {
				if !live[name] {
					gone[name] = append(gone[name], tag)
				}
				continue
			}
			if slices.ContainsFunc(names, func(n string) bool { return images.NamesTag(tag, n) }) {
				continue
			}
			if name, ok := images.NameOfTag(tag); ok {
				gone[name] = append(gone[name], tag)
			}
		}
	}
	return gone
}

// leftEntries are --server-info's lines of what may be dead or gone, each with its hint, for
// the server host (an address's server part) and ref (as typed), both empty for this
// machine's. Those that go by an absence are made only when liveNames reads.
func leftEntries(info remote.ServerInfo, host, ref string) []remote.StatusEntry {
	var es []remote.StatusEntry
	add := func(key, line string, hints ...report.Remedy) {
		es = append(es, remote.StatusEntry{Level: remote.StatusInfo, Key: key, Lines: []string{line}, Remedies: hints})
	}
	warn := func(key, line string) {
		es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: key, Lines: []string{line}})
	}
	// onServer is a hint of commands run on the server: through --shell-host from here.
	onServer := func(text string, cmds ...string) report.Remedy {
		if ref != "" {
			cmds = append([]string{"devopsy @" + ref + " --shell-host"}, cmds...)
			text = strings.TrimSuffix(text, ":") + ", on the server:"
		}
		return report.Hint(text, cmds...)
	}
	dockerUp := info.Docker.Error == "" && !info.Docker.Door

	if f := info.GC; f != nil && dockerUp {
		var what []string
		for _, c := range []struct {
			n    int
			what string
		}{{f.OneOffs, "one-off container"}, {f.Snapshots, "release snapshot"}, {f.Left, "image name a project's record left"}, {f.Dangling, "dangling image"}} {
			if c.n > 0 {
				what = append(what, plural(c.n, c.what))
			}
		}
		if len(what) > 0 {
			hint := report.Hint("--gc with --dry-run, run on this server from a checkout or an alias, lists it with its sizes, and the build cache it removes too")
			if ref != "" {
				hint = report.Hint("its sizes, and the build cache it removes too:", "devopsy @"+ref+" --gc --dry-run")
			}
			add("gc", "--gc would remove "+strings.Join(what, ", "), hint)
		}
		// What the count could not read is unknown, never none.
		for _, u := range f.Unread {
			warn("gc", "--gc's count could not read "+u)
		}
	}

	switch {
	case !dockerUp:
	case info.ImagesUnread != "":
		warn("images", "the images could not be listed: "+oneLine(info.ImagesUnread))
	case info.ContainersUnread != "":
		// Each image's containers are counted from that list: none of it reads as none.
	default:
		for _, img := range info.Images {
			if slices.ContainsFunc(img.Tags, func(t string) bool {
				_, hold := images.HoldOf(t)
				_, _, build := images.IsDoorBuild(t)
				return hold || build || images.Familiar(t) == images.Familiar(remote.UtilImage)
			}) || img.Containers > 0 {
				continue
			}
			add("images", strings.Join(img.Tags, ", ")+", "+humanBytes(img.Size)+": no release holds it, nor any container uses it",
				onServer("remove it once nothing needs it:", "docker rmi "+strings.Join(img.Tags, " ")))
		}
	}

	if live, ok := liveNames(info); ok && dockerUp {
		gone := map[string]*goneEnv{}
		of := func(name string) *goneEnv {
			if gone[name] == nil {
				gone[name] = &goneEnv{}
			}
			return gone[name]
		}
		if info.ImagesUnread == "" {
			for name, tags := range goneTags(info.Images, live) {
				of(name).tags = tags
			}
		}
		if info.ContainersUnread == "" {
			for _, c := range info.Containers {
				if c.Released && !c.OneOff && c.Project != "" && !live[c.Project] {
					of(c.Project).containers = append(of(c.Project).containers, c.Service+" ("+c.State+")")
				}
			}
		}
		// A volume only of a name a release's tags or containers give: compose labels the
		// admin's own projects' volumes too.
		if info.VolumesUnread == "" {
			for _, v := range info.Volumes {
				if g := gone[v.Project]; g != nil && !v.Anonymous && !live[v.Project] {
					g.volumes = append(g.volumes, v)
				}
			}
		}
		for _, p := range info.SnapshotDirs {
			if n := composeNameOf(p); !live[n] {
				of(n).snapshots = filepath.Join(info.SnapshotRoot, filepath.FromSlash(p))
			}
		}
		for _, name := range slices.Sorted(maps.Keys(gone)) {
			g := gone[name]
			lines := []string{name + ": no environment and no uid claim on the server, and left:"}
			var cmds []string
			if len(g.containers) > 0 {
				lines = append(lines, "containers "+strings.Join(g.containers, ", "))
				cmds = append(cmds, "docker compose -p "+remote.QuoteWord(name)+" down")
			}
			if len(g.volumes) > 0 {
				var vs, names []string
				for _, v := range g.volumes {
					vs = append(vs, v.Name)
					names = append(names, remote.QuoteWord(v.Name))
				}
				lines = append(lines, "volumes "+strings.Join(vs, ", "))
				cmds = append(cmds, "docker volume rm "+strings.Join(names, " "))
			}
			if len(g.tags) > 0 {
				lines = append(lines, plural(len(g.tags), "image name")+" of its releases")
				cmds = append(cmds, "docker rmi "+strings.Join(g.tags, " "))
			}
			if g.snapshots != "" {
				lines = append(lines, "snapshots in "+g.snapshots)
				cmds = append(cmds, "rm -rf "+remote.QuoteWord(g.snapshots))
			}
			es = append(es, remote.StatusEntry{Level: remote.StatusInfo, Key: "gone", Lines: lines,
				Remedies: []report.Remedy{onServer("an environment removed by hand: remove what is left once none of it is needed:", cmds...)}})
		}
	}

	if dockerUp && info.VolumesUnread == "" {
		n := 0
		for _, v := range info.Volumes {
			if v.Anonymous && v.Unused {
				n++
			}
		}
		if n > 0 {
			add("volumes", plural(n, "anonymous volume")+" no container uses (an image's VOLUME after a down without -v: a database's data, maybe)",
				onServer("once checked that none holds what is needed, remove them:", "docker volume prune"))
		}
	} else if dockerUp {
		warn("volumes", "the volumes could not be listed: "+oneLine(info.VolumesUnread))
	}

	for _, r := range info.Removing {
		env := r.Path[:strings.LastIndex(r.Path, ".rm.")]
		project, rest, _ := strings.Cut(env, "/")
		hint := report.Hint("a --destroy of " + env + " run again from " + project + "'s checkout resumes it")
		if host != "" {
			hint = report.Hint("run it again, which resumes it, in "+project+"'s checkout:", "devopsy @"+host+":"+remote.QuoteWord(rest)+" --destroy")
		}
		add("environments", r.Path+": left by a --destroy cut off while it removed "+env+", "+humanBytes(r.Size), hint)
	}

	if info.Door != nil {
		for _, k := range info.Door.Keyless {
			add("door", k.User+" has no key and still holds "+strings.Join(k.Held, "; "), keptUserHints(k.Project, ref)...)
		}
		for _, u := range info.Door.Destroying {
			hint := report.Hint("its destroy run again (--project-users remove with --destroy) completes it")
			if ref != "" && remote.ValidName(u.Project) {
				hint = report.Hint("run it again, which completes it:", "devopsy @"+ref+" --project-users remove "+u.Project+" --destroy")
			}
			add("door", u.User+": its --project-users remove --destroy did not complete, and the door answers it nothing", hint)
		}
	}
	return es
}
