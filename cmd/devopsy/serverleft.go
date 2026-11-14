package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

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

// keptUserLine is what says a project user with no key still holds what is under its names,
// each held kind on a line of its own after it (KeylessUser.Held).
func keptUserLine(k remote.KeylessUser) string { return k.User + " has no key and still holds:" }

// keptUserHints are the ways out of a project user with no key that still holds what is under
// its names: its destroy, offered by the server's address ref ("" for none, the way in words),
// and a key added, in words, since a key's command takes the deploy user's address and a file.
func keptUserHints(project, ref string) []report.Remedy {
	return []report.Remedy{
		remote.DestroyAgain(report.Hint, ref, project, "remove it with what it holds:", "a --project-users remove of "+project+" with --destroy removes it with what it holds"),
		report.Hint("or a key added for it (--ssh-keys with --for) takes it back"),
	}
}

// destroyPendingHint is the way out of a project user's destroy that did not complete, project
// its project's: its destroy run again, by the server's address ref ("" for none, in words).
func destroyPendingHint(ref, project string) report.Remedy {
	return remote.DestroyAgain(report.Hint, ref, project, "run it again, which completes it:", "its destroy run again (--project-users remove with --destroy) completes it")
}

// sessionServer is the address a server's command takes from the session this devopsy runs
// in (remote.Session.Server), "" when it carries none.
func sessionServer() string { return remote.SessionFrom(os.Getenv).Server }

// liveNames are the compose names of what is there, for every report that goes by an
// absence: the release root's environment directories (the deploy user's, never a read of a
// project user's tree, which may answer nothing) and every environment uid claim's (a project
// user's start always has one) but those staleUID judges left (a door project's, whose tree
// may answer nothing, it never judges so), stale by compose name. ok false when either does
// not read in full, or the root holds no environment: with its disk not mounted, every
// environment would read as gone.
func liveNames(info remote.ServerInfo) (live map[string]bool, stale map[string]remote.UIDClaim, ok bool) {
	live, stale = map[string]bool{}, map[string]remote.UIDClaim{}
	for _, e := range info.Environments {
		if e.User == "" {
			live[composeNameOf(e.Path)] = true
		}
	}
	if len(live) == 0 || info.EnvironmentsUnread != "" {
		return nil, nil, false
	}
	for _, c := range info.UIDs {
		if c.Bad != "" && c.Name == info.UIDsDir {
			return nil, nil, false // the claims did not read
		}
	}
	stale = staleClaims(info)
	for _, c := range info.UIDs {
		if o, err := uids.ParseOwner(c.Owner); err == nil && o.Env != "" {
			if _, left := stale[o.ComposeName()]; !left {
				live[o.ComposeName()] = true
			}
		}
	}
	return live, stale, true
}

// staleClaims are the environment uid claims staleUID judges left, by compose name.
func staleClaims(info remote.ServerInfo) map[string]remote.UIDClaim {
	stale := map[string]remote.UIDClaim{}
	for _, c := range info.UIDs {
		if o, err := uids.ParseOwner(c.Owner); err == nil && o.Env != "" &&
			staleUID(c, info.UIDs, info.Environments, info.EnvironmentsUnread != "", info.DoorProjects) != "" {
			stale[o.ComposeName()] = c
		}
	}
	return stale
}

// claimKey is where claim c's environment is shown: its project, and its key there.
func claimKey(c remote.UIDClaim) (project, key string) {
	o, _ := uids.ParseOwner(c.Owner)
	return o.Project, filepath.ToSlash(filepath.Join(o.Instance, o.Env))
}

// goneEnv is what is left of a compose name with no environment: its claim when one is left.
type goneEnv struct {
	tags, containers []string
	volumes          []remote.HostVolume
	snapshots        string
	claim            *remote.UIDClaim
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
// machine's: groups what belongs to a project (a gone environment, a cut --destroy's
// directory, a project user's), by project, leftovers the rest. Those that go by an absence
// are made only when liveNames reads.
func leftEntries(info remote.ServerInfo, host, ref string) (groups map[string][]remote.StatusEntry, leftovers []remote.StatusEntry) {
	const section = "leftovers"
	groups = map[string][]remote.StatusEntry{}
	add := func(key, line string, hints ...report.Remedy) {
		leftovers = append(leftovers, remote.StatusEntry{Level: remote.StatusInfo, Section: section, Key: key, Lines: []string{line}, Remedies: hints})
	}
	warn := func(key, line string) {
		leftovers = append(leftovers, remote.StatusEntry{Level: remote.StatusWarning, Section: section, Key: key, Lines: []string{line}})
	}
	// inProject adds what is a project's, under key there, or under its heading when head.
	inProject := func(project, key string, head bool, lines []string, hints ...report.Remedy) {
		groups[project] = append(groups[project], remote.StatusEntry{Level: remote.StatusInfo, Section: "projects", Group: project, Head: head, Key: key, Lines: lines, Remedies: hints})
	}
	onServer := func(text string, cmds ...string) report.Remedy {
		return onServerRemedy(report.Hint, ref, text, cmds...)
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
			hint := report.Hint("--gc with --dry-run, run on this server from a checkout or an alias, lists their sizes, and the build cache it would remove too")
			if ref != "" {
				hint = report.Hint("to see their sizes, and the build cache it would remove too:", "devopsy @"+ref+" --gc --dry-run")
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
		// One row each, then one line for all of them and one rmi of every tag.
		var tags []string
		n := 0
		for _, img := range info.Images {
			if slices.ContainsFunc(img.Tags, func(t string) bool {
				_, hold := images.HoldOf(t)
				_, _, build := images.IsDoorBuild(t)
				return hold || build || images.Familiar(t) == images.Familiar(remote.UtilImage)
			}) || img.Containers > 0 {
				continue
			}
			cells := []string{strings.Join(img.Tags, ", "), humanBytes(img.Size)}
			leftovers = append(leftovers, remote.StatusEntry{Level: remote.StatusInfo, Section: section, Key: "images", Lines: []string{remote.CellsJoined(cells...)}, Cells: cells})
			tags = append(tags, img.Tags...)
			n++
		}
		it := "them"
		if n == 1 {
			it = "it"
		}
		if n > 0 {
			add("images", "no release holds "+it+", nor any container uses "+it,
				onServer("remove "+it+" once nothing needs "+it+":", "docker rmi "+strings.Join(tags, " ")))
		}
	}

	live, stale, ok := liveNames(info)
	if !ok || !dockerUp {
		// What else is left cannot be looked for: each claim left is said alone, never as all
		// there is.
		left := staleClaims(info)
		why := notLookedFor(info, dockerUp)
		for _, name := range slices.Sorted(maps.Keys(left)) {
			c := left[name]
			e := goneEntry(info, name, &c, claimLeftWords(c, nil, why), claimRemoval(report.Hint, ref, info.UIDsDir, c))
			groups[e.Group] = append(groups[e.Group], e)
		}
	}
	if ok && dockerUp {
		gone := map[string]*goneEnv{}
		of := func(name string) *goneEnv {
			if gone[name] == nil {
				gone[name] = &goneEnv{}
			}
			return gone[name]
		}
		for name, c := range stale {
			of(name).claim = &c
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
		why := notLookedFor(info, dockerUp)
		for _, name := range slices.Sorted(maps.Keys(gone)) {
			g := gone[name]
			var left, cmds []string
			if len(g.containers) > 0 {
				left = append(left, "containers "+strings.Join(g.containers, ", "))
				cmds = append(cmds, "docker compose -p "+remote.QuoteWord(name)+" down")
			}
			if len(g.volumes) > 0 {
				var vs, names []string
				for _, v := range g.volumes {
					vs = append(vs, v.Name)
					names = append(names, remote.QuoteWord(v.Name))
				}
				left = append(left, "volumes "+strings.Join(vs, ", "))
				cmds = append(cmds, "docker volume rm "+strings.Join(names, " "))
			}
			if len(g.tags) > 0 {
				left = append(left, plural(len(g.tags), "image name")+" of its releases")
				cmds = append(cmds, "docker rmi "+strings.Join(g.tags, " "))
			}
			if g.snapshots != "" {
				left = append(left, "snapshots in "+g.snapshots)
				cmds = append(cmds, "rm -rf "+remote.QuoteWord(g.snapshots))
			}
			c := g.claim
			if c != nil && len(left) == 0 {
				e := goneEntry(info, name, c, claimLeftWords(*c, nil, why), claimRemoval(report.Hint, ref, info.UIDsDir, *c))
				groups[e.Group] = append(groups[e.Group], e)
				continue
			}
			what := "no environment and no uid claim on the server, and left: " + strings.Join(left, ", ")
			if c != nil {
				// Its claim last: freed while anything of it is left, its uid could go to
				// another environment with these files still owned by it.
				cmds = append(cmds, claimRm(info.UIDsDir, *c))
				what = claimLeftWords(*c, left, why)
			}
			e := goneEntry(info, name, c, what, onServer("an environment removed by hand: remove what is left once none of it is needed:", cmds...))
			groups[e.Group] = append(groups[e.Group], e)
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
		// Under the environment's own name: its <base>.rm.<pid> would widen every key.
		e := placed(project, rest, []string{"a --destroy cut off while it removed it left " + path.Base(r.Path) + ", " + humanBytes(r.Size)}, hint)
		groups[e.Group] = append(groups[e.Group], e)
	}

	if info.Door != nil {
		for _, k := range info.Door.Keyless {
			inProject(k.Project, k.Project, true, append([]string{keptUserLine(k)}, k.Held...), keptUserHints(k.Project, ref)...)
		}
		for _, u := range info.Door.Destroying {
			inProject(u.Project, u.Project, true, []string{u.User + ": its --project-users remove --destroy did not complete, and the door answers it nothing"}, destroyPendingHint(ref, u.Project))
		}
	}
	return groups, leftovers
}

// claimPath is where claim c is in the claims directory dir: its file, named for its owner,
// never dir itself.
func claimPath(dir string, c remote.UIDClaim) string {
	return filepath.Join(dir, cmp.Or(c.Name, c.Owner))
}

// goneKeyMax is the longest name a gone environment is keyed by; a longer one is said in its
// line, under the key gone, a row of its own (its Name): a key widens every other.
const goneKeyMax = 12

// placed is project's row of key with lines and hint, key past goneKeyMax said in its first
// line under gone, the row its own by its Name.
func placed(project, key string, lines []string, hint report.Remedy) remote.StatusEntry {
	e := remote.StatusEntry{Level: remote.StatusInfo, Section: "projects", Group: project, Key: key, Lines: lines, Remedies: []report.Remedy{hint}}
	if utf8.RuneCountInString(key) > goneKeyMax {
		e.Key, e.Name = "gone", key
		e.Lines = append([]string{key + ": " + strings.TrimPrefix(lines[0], "gone, ")}, lines[1:]...)
	}
	return e
}

// goneEntry is the row of what is left of the gone environment of compose name, its claim c
// when one is left, what its words after "gone, ": in its project under its name, as its
// claim's owner says them, else as projectOfName finds them; a name of no project on the
// server a project of its own, its name the line above the row, so a heading never widens
// the key column. A claim's row is never a heading.
func goneEntry(info remote.ServerInfo, name string, c *remote.UIDClaim, what string, hint report.Remedy) remote.StatusEntry {
	if c != nil {
		project, key := claimKey(*c)
		return placed(project, key, []string{"gone, " + what}, hint)
	}
	project, key, own := projectOfName(name, info)
	if own {
		return remote.StatusEntry{Level: remote.StatusInfo, Section: "projects", Group: name, Name: name, Lines: []string{"gone, " + what}, Remedies: []report.Remedy{hint}}
	}
	return placed(project, key, []string{"gone, " + what}, hint)
}

// claimLeftWords are what is left of a gone environment whose claim c is left, besides left:
// only its claim when nothing else is and why is empty (everything else read), else its
// claim and left, then why what else is left could not be looked for.
func claimLeftWords(c remote.UIDClaim, left []string, why string) string {
	uid := strconv.FormatUint(uint64(c.UID), 10)
	words := "its uid claim " + uid + " is left"
	switch {
	case len(left) > 0:
		words = "no environment on the server, and left: " + strings.Join(append([]string{"its uid claim " + uid}, left...), ", ")
	case why == "":
		return "only its uid claim " + uid + " is left"
	}
	if why != "" {
		words += "; " + why
	}
	return words
}

// notLookedFor is why what is left of a gone environment could not all be looked for, empty
// when everything read, dockerUp whether Docker answers as the deploy user's.
func notLookedFor(info remote.ServerInfo, dockerUp bool) string {
	switch {
	case info.Docker.Door:
		return "what else is left is the server's admin's to look for"
	case !dockerUp:
		return "what else is left was not looked for: Docker did not answer"
	case info.EnvironmentsUnread != "":
		return "what else is left was not looked for: the environments did not all read"
	}
	if _, _, ok := liveNames(info); !ok {
		// Its claims unread, there is no claim to say this of: the root lists no environment.
		return "what else is left was not looked for: the release root lists no environment"
	}
	var unread []string
	for _, r := range []struct{ what, err string }{{"containers", info.ContainersUnread}, {"images", info.ImagesUnread}, {"volumes", info.VolumesUnread}} {
		if r.err != "" {
			unread = append(unread, r.what)
		}
	}
	if len(unread) == 0 {
		return ""
	}
	return "its " + strings.Join(unread, ", ") + " could not be read"
}

// onServerRemedy is a remedy of kind (report.Fix or report.Hint) of commands run on the
// server: through --shell-host from the operator's machine, ref the server as typed, empty on
// the server itself.
func onServerRemedy(kind func(string, ...string) report.Remedy, ref, text string, cmds ...string) report.Remedy {
	if ref != "" {
		cmds = append([]string{"devopsy @" + ref + " --shell-host"}, cmds...)
		text = strings.TrimSuffix(text, ":") + ", on the server:"
	}
	return kind(text, cmds...)
}

// claimRm removes claim c from the claims directory dir.
func claimRm(dir string, c remote.UIDClaim) string {
	return "rm " + remote.QuoteWord(claimPath(dir, c))
}

// claimRemoval is the way out of claim c left, of kind, on the server ref names: every claim
// left is removed by hand, as the deploy user, once nothing of its owner is left.
func claimRemoval(kind func(string, ...string) report.Remedy, ref, dir string, c remote.UIDClaim) report.Remedy {
	return onServerRemedy(kind, ref, "remove it by hand once nothing of it is left:", claimRm(dir, c))
}

// projectOfName is the project a compose name with no environment is filed under, and its
// key there: of the projects on the server (its environments', its claims'), the one whose
// name and a dash start it the longest, its key the rest; else own, the name of no project on
// the server, its own project.
func projectOfName(name string, info remote.ServerInfo) (project, key string, own bool) {
	var projects []string
	for _, e := range info.Environments {
		p, _, _ := strings.Cut(e.Path, "/")
		projects = append(projects, p)
	}
	for _, c := range info.UIDs {
		if o, err := uids.ParseOwner(c.Owner); err == nil {
			projects = append(projects, o.Project)
		}
	}
	for _, p := range projects {
		if rest, ok := strings.CutPrefix(name, p+"-"); ok && rest != "" && len(p) > len(project) {
			project, key = p, rest
		}
	}
	if project == "" {
		return name, "", true
	}
	return project, key, false
}
