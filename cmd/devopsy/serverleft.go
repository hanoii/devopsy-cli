package main

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/uids"
)

// What --server-info shows of what may be dead or gone on a server, none of it removed by
// devopsy: the facts the deploy user's devopsy reads on the server (leftFacts), and the lines
// the operator's devopsy makes of them (leftEntries), each with its hint.

// leftFacts reads, as the deploy user, what may be dead or gone into info: what --gc would
// remove (its own listing, gcScope.find), the images with a tag, the volumes, what a cut
// --destroy left, the snapshot root's environments and the project users with no key that
// still hold environments. Docker's system df -v is read once, for --gc's listing and the
// sizes here. A read that fails is said in its *Unread, never taken for none.
func leftFacts(info *remote.ServerInfo, r remote.Releases, d *remote.Door, trees map[string]string) {
	g := &gcScope{docker: "docker", root: r.Root, ages: r.GC, door: d, now: time.Now()}
	plan := g.find()
	info.GC = plan.facts()
	if info.Docker.Error == "" {
		u, err := g.systemUsage()
		if err != nil {
			info.ImagesUnread, info.VolumesUnread = err.Error(), err.Error()
		} else {
			info.Volumes = hostVolumes(u)
		}
		if info.ImagesUnread == "" {
			if info.Images, err = hostImages(g.docker, u); err != nil {
				info.ImagesUnread = err.Error()
			}
		}
	}
	info.Removing = cutDestroys(r.Root, trees)
	if r.Snapshots.Root != "" {
		info.SnapshotDirs = snapshotEnvDirs(r.Snapshots.Root)
	}
	if info.Door != nil && d != nil && len(d.Users) > 0 {
		info.Door.Keyless = keylessUsers(d, r.Root, info.UIDs, info.Containers)
	}
	if info.Door != nil {
		pending := destroysPending()
		for _, who := range slices.Sorted(maps.Keys(pending)) {
			info.Door.Destroying = append(info.Door.Destroying, remote.KeylessUser{User: who, Project: pending[who]})
		}
	}
}

// facts are the plan counted, for --server-info: what --gc would remove, up to its size.
func (p gcPlan) facts() *remote.GCFacts {
	f := &remote.GCFacts{OneOffs: len(p.OneOffs), Snapshots: len(p.Snapshots), Images: len(p.Left) + len(p.Dangling.Images), Unread: p.Unread}
	for _, c := range p.OneOffs {
		f.Size += c.Size
	}
	for _, b := range p.Builders {
		if !b.Stopped {
			f.Builders++
			f.Size += b.Size
		}
	}
	for _, s := range p.Snapshots {
		f.Size += s.Size
	}
	for _, l := range p.Left {
		f.Size += l.Size
	}
	f.Size += p.Dangling.size()
	return f
}

// hostImages are the images with a tag, by docker image ls, each with every tag it has, and
// its size and the containers using it as system df -v counts them (u).
func hostImages(docker string, u dockerUsage) ([]remote.HostImage, error) {
	out, err := exec.Command(docker, "image", "ls", "--no-trunc", "--format", "{{.ID}} {{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		return nil, dockerFailed("docker image ls", err)
	}
	byID := map[string]*remote.HostImage{}
	var order []string
	for _, line := range strings.Split(string(out), "\n") {
		id, ref, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || strings.Contains(ref, "<none>") {
			continue
		}
		if byID[id] == nil {
			byID[id] = &remote.HostImage{ID: id}
			order = append(order, id)
		}
		byID[id].Tags = append(byID[id].Tags, ref)
	}
	for _, i := range u.Images {
		if img := byID[i.ID]; img != nil {
			img.Size = parseDockerSize(i.Size)
			img.Containers, _ = strconv.Atoi(i.Containers)
		}
	}
	var imgs []remote.HostImage
	for _, id := range order {
		imgs = append(imgs, *byID[id])
	}
	return imgs, nil
}

// hostVolumes are the volumes system df -v gives (u), with the compose project of their labels
// and whether Docker made them anonymous. Its labels come as one line, k=v joined by commas.
func hostVolumes(u dockerUsage) []remote.HostVolume {
	var vs []remote.HostVolume
	for _, v := range u.Volumes {
		hv := remote.HostVolume{Name: v.Name, Size: parseDockerSize(v.Size)}
		hv.Links, _ = strconv.Atoi(v.Links)
		for _, kv := range strings.Split(v.Labels, ",") {
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
	return vs
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
		dirs, _ := envDirs(p)
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
	add := func(dir string) {
		if rel, err := filepath.Rel(root, dir); err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
	}
	ps, _ := envDirs(root)
	for _, p := range ps {
		es, _ := envDirs(p)
		for _, e := range es {
			if holds(e) {
				add(e)
				continue
			}
			inner, _ := envDirs(e)
			for _, i := range inner {
				if holds(i) {
					add(i)
				}
			}
		}
	}
	return out
}

// keylessUsers are the project users with no key whose project still holds environments
// (projectHolds), by the claims and containers read.
func keylessUsers(d *remote.Door, root string, claims []remote.UIDClaim, containers []remote.HostContainer) []remote.KeylessUser {
	var owners, projects []string
	for _, c := range claims {
		owners = append(owners, c.Owner)
	}
	for _, c := range containers {
		projects = append(projects, c.Project)
	}
	others, err := serverProjects(doorProjects(d), root)
	var out []remote.KeylessUser
	for _, who := range slices.Sorted(maps.Keys(d.Users)) {
		if keysOf(who) > 0 {
			continue
		}
		var p *door.Policy
		if err == nil {
			p = &door.Policy{Project: d.Users[who].Project, User: who, Others: others}
		}
		if names := projectHolds(d.Users[who].Project, p, owners, projects); len(names) > 0 {
			out = append(out, remote.KeylessUser{User: who, Project: d.Users[who].Project, Names: names})
		}
	}
	return out
}

// keylessOf is keylessUsers as --project-users list reads them, on its own: the claims under
// the release root and every container's compose project. A read that fails gives what the
// rest says, a line of the list's alone.
func keylessOf(d *remote.Door) []remote.KeylessUser {
	r, err := remote.ReleaseSettings()
	if err != nil {
		return nil
	}
	_, claims := uidFacts(r.Root)
	var cs []remote.HostContainer
	if out, err := exec.Command("docker", "ps", "-a", "--format", `{{.Label "com.docker.compose.project"}}`).Output(); err == nil {
		for _, p := range strings.Fields(string(out)) {
			cs = append(cs, remote.HostContainer{Project: p})
		}
	}
	return keylessUsers(d, r.Root, claims, cs)
}

// keysOf is how many keys project user who has.
func keysOf(who string) int {
	data, _ := os.ReadFile(remote.DoorKeysFile(who))
	return len(remote.ParseAuthorizedKeys(string(data)))
}

// projectHolds are the compose names project still holds, sorted: its environment claims'
// (owners, every claim's), and those of the containers (projects, each container's compose
// project) under its names by p (door.Policy.OwnName), none of those when p is nil (the
// server's projects did not read, so whose a name is cannot be told).
func projectHolds(project string, p *door.Policy, owners, projects []string) []string {
	var names []string
	for _, o := range owners {
		if c, err := uids.ParseOwner(o); err == nil && c.Env != "" && c.Project == project {
			names = append(names, c.ComposeName())
		}
	}
	for _, n := range projects {
		if p != nil && n != "" && p.OwnName(n) {
			names = append(names, n)
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(names)))
}

// keylessWords are the ways out of a project user with no key that holds environments, in
// words: its project's removal with them, a key to take it back.
const keylessWords = "a key added for it (--ssh-keys) takes it back"

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
		}{{f.OneOffs, "one-off container"}, {f.Builders, "builder's build cache"}, {f.Snapshots, "release snapshot"}, {f.Images, "image"}} {
			if c.n > 0 {
				what = append(what, plural(c.n, c.what))
			}
		}
		if len(what) > 0 {
			hint := report.Hint("--gc, run on this server from a checkout or an alias, lists and removes it")
			if ref != "" {
				hint = report.Hint("list it, then remove it:", "devopsy @"+ref+" --gc")
			}
			add("gc", "--gc would remove up to "+humanBytes(f.Size)+": "+strings.Join(what, ", "), hint)
		}
	}

	switch {
	case !dockerUp:
	case info.ImagesUnread != "":
		warn("images", "the images could not be listed: "+oneLine(info.ImagesUnread))
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
					vs = append(vs, v.Name+" "+humanBytes(v.Size))
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
		n, size := 0, int64(0)
		for _, v := range info.Volumes {
			if v.Anonymous && v.Links == 0 {
				n++
				size += v.Size
			}
		}
		if n > 0 {
			add("volumes", plural(n, "anonymous volume")+" no container uses, "+humanBytes(size)+" (an image's VOLUME after a down without -v: a database's data, maybe)",
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
			hints := []report.Remedy{report.Hint("removing " + k.Project + "'s project user with --project-users remove and --destroy removes them with it; " + keylessWords)}
			if ref != "" {
				hints = []report.Remedy{report.Hint("remove it with what it holds:", "devopsy @"+ref+" --project-users remove "+k.Project+" --destroy"), report.Hint("or " + keylessWords)}
			}
			add("door", k.User+" has no key and still holds "+strings.Join(k.Names, ", "), hints...)
		}
		for _, u := range info.Door.Destroying {
			hint := report.Hint("the same --project-users remove " + u.Project + " --destroy run again completes it")
			if ref != "" && remote.ValidName(u.Project) {
				hint = report.Hint("run it again, which completes it:", "devopsy @"+ref+" --project-users remove "+u.Project+" --destroy")
			}
			add("door", u.User+": its --project-users remove --destroy did not complete, and the door answers it nothing", hint)
		}
	}
	return es
}
