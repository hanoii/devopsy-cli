package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
)

// gcLeftWarned is --gc's exit code when something is left to remove: what --dry-run lists,
// or what a removal skipped, could not remove or could not read. As --server-info's, for a
// timer or a monitor.
const gcLeftWarned = serverInfoWarned

// runGC implements devopsy @<server> --gc [--dry-run] [--yes]: what the server keeps that no
// release, --destroy or pruning removes, listed with its sizes in a session of its own
// (gcScope.find), asked about locally, then removed by a second session, given the listing,
// which removes only what it listed and still goes, deciding each removal again under its lock
// (gcScope.remove): what came to go between the listing and the answer was never confirmed.
// --yes runs the second alone, removing what it finds; --dry-run the first alone, asking
// nothing.
func runGC(t *remote.Target, args []string) int {
	dry, yes := false, false
	for _, a := range args {
		switch a {
		case "--dry-run":
			dry = true
		case "--yes", "-y":
			yes = true
		default:
			return fail("usage: devopsy @" + targetRef(t) + " --gc [--dry-run] [--yes]")
		}
	}
	remove := remote.GCScript(t, "remove-listed")
	if yes && !dry {
		return gcRemove(t, remote.GCScript(t, "remove"), nil)
	}
	// An older server refuses in this first session, before anything: it carries the
	// removal's needs too.
	ctx := remote.WithAlsoNeeds(context.Background(), remove)
	var out bytes.Buffer
	code, err := remote.SSHContext(ctx, t, nil, remote.GCScript(t, "list"), bytes.NewReader(nil), &out, false, nil)
	if err != nil {
		return failErr(err)
	}
	if code == remote.AskedAsProjectUser {
		refuseProjectUser(t, out.String(), "what --gc removes is the server's, its deploy user's", "--gc")
		return 1
	}
	if code != 0 {
		return code
	}
	var plan gcPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		return fail("the server's devopsy answered what this one cannot read: " + err.Error())
	}
	es := plan.entries()
	newEntryTable(os.Stdout, styleFor(os.Stdout), es).print(es)
	if plan.count() == 0 {
		report.Info("nothing to remove on " + t.Host)
		gcLeaves(t)
		if len(plan.Unread) > 0 {
			return gcLeftWarned
		}
		return 0
	}
	if dry {
		gcLeaves(t)
		return gcLeftWarned
	}
	if !atTerminal() {
		report.Fail(&report.Failure{Msg: "nothing removed: no terminal to confirm it on", Remedies: []report.Remedy{report.Fix("add --yes to go on without asking")}})
		return 1
	}
	if !report.Default().Confirm(stdinReader, "remove these?") {
		report.Error("nothing removed")
		return 1
	}
	return gcRemove(t, remove, out.Bytes())
}

// gcRemove runs --gc's removal (script), given the listing as the server's devopsy wrote it
// (none for --yes), then says what it leaves: its exit, gcLeftWarned when something is left to
// remove.
func gcRemove(t *remote.Target, script string, listing []byte) int {
	var out bytes.Buffer
	code, err := remote.SSH(t, script, bytes.NewReader(listing), &out, false)
	if err != nil {
		return failErr(err)
	}
	switch code {
	case remote.AskedAsProjectUser:
		refuseProjectUser(t, out.String(), "what --gc removes is the server's, its deploy user's", "--gc")
		return 1
	case sshLost:
		// Each removal stands alone: a cut run leaves only less to remove.
		report.Say(report.ErrorLevel, lostSays(t, "", "--gc may have stopped halfway there, each removal done or not begun"), nil, report.Fix("", "devopsy @"+targetRef(t)+" --gc"))
		return code
	}
	if code == 0 || code == gcLeftWarned {
		gcLeaves(t)
	}
	return code
}

// gcLeaves is --gc's last line: what it never removes is --server-info's to show.
func gcLeaves(t *remote.Target) {
	report.Say(report.InfoLevel, "--gc removes only what nothing on the server owns", nil,
		report.Hint("what it leaves, to look at:", "devopsy @"+targetRef(t)+" --server-info"))
}

// gcPlan is what --gc removes on a server, as its list finds it (gcScope.find): each kind in
// the order the removal goes, what it could not read in Unread, nothing of which is removed.
// Now is the server's time, which the ages count from.
type gcPlan struct {
	Now        time.Time     `json:"now"`
	Containers time.Duration `json:"containers_age"`
	BuildCache time.Duration `json:"build_cache_age"`
	OneOffs    []gcOneOff    `json:"one_offs,omitempty"`
	Builders   []gcBuilder   `json:"builders,omitempty"`
	Snapshots  []gcSnapshot  `json:"snapshots,omitempty"`
	Left       []gcLeft      `json:"left,omitempty"`
	Dangling   gcDangling    `json:"dangling,omitzero"`
	Unread     []string      `json:"unread,omitempty"`
}

// gcOneOff is an exited one-off container (a compose run's) of a listed environment.
type gcOneOff struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Project  string    `json:"project"`
	Finished time.Time `json:"finished"`
	Size     int64     `json:"size"`
}

func (c gcOneOff) words(now time.Time) string {
	return fmt.Sprintf("%s (%s), exited %s ago, %s", c.Name, c.Project, duration(now.Sub(c.Finished)), humanBytes(c.Size))
}

// gcBuilder is a builder's build cache unused for the age, by its Name: the daemon's own (no
// Project), named for the Docker context the listing read it through (daemonBuilder), or a
// project user's project's (door.BuilderOf), which a stopped one keeps (Stopped). Records is
// how many records go, Size what of them no image shares.
type gcBuilder struct {
	Name    string `json:"name,omitempty"`
	Project string `json:"project,omitempty"`
	Stopped bool   `json:"stopped,omitempty"`
	Records int    `json:"records"`
	Size    int64  `json:"size"`
}

func (b gcBuilder) who() string {
	if b.Project == "" {
		return "the server's Docker"
	}
	return b.Name + " (" + b.Project + "'s builder)"
}

func (b gcBuilder) words(age time.Duration) string {
	if b.Stopped {
		return b.who() + ": not running, its cache stays until it runs again"
	}
	return fmt.Sprintf("%s: %s unused for %s, %s of them no image shares", b.who(), plural(b.Records, "record"), duration(age), humanBytes(b.Size))
}

// gcSnapshot is a release snapshot whose release is gone, of the environment Env
// (<project>/[<instance>/]<environment>) at Base.
type gcSnapshot struct {
	Env     string `json:"env"`
	Base    string `json:"base"`
	ID      string `json:"id"`
	Release string `json:"release"`
	Size    int64  `json:"size"`
}

func (s gcSnapshot) words() string {
	return fmt.Sprintf("%s %s, its release %s gone, %s", s.Env, s.ID, s.Release, humanBytes(s.Size))
}

// gcLeft is a name a project's record of names left holds (images.Retried) that its retry
// removes now, the image's Size when it is the image's last name, which takes the image.
type gcLeft struct {
	images.Retried
	Last bool  `json:"last"`
	Size int64 `json:"size"`
}

func (l gcLeft) words() string {
	s := l.Ref + ", left by release " + l.Release + " of " + l.Name
	if l.Last {
		return s + ", " + humanBytes(l.Size)
	}
	return s + ", a name only (the image has others)"
}

// gcDangling is the dangling images no container uses, what docker image prune removes, by id.
type gcDangling struct {
	Images []gcImage `json:"images,omitempty"`
}

// gcImage is an image by its id, with Docker's size.
type gcImage struct {
	ID   string `json:"id"`
	Size int64  `json:"size"`
}

func (d gcDangling) size() int64 {
	n := int64(0)
	for _, i := range d.Images {
		n += i.Size
	}
	return n
}

// count is how many things the plan removes: a stopped builder's cache stays.
func (p gcPlan) count() int {
	n := len(p.OneOffs) + len(p.Snapshots) + len(p.Left) + len(p.Dangling.Images)
	for _, b := range p.Builders {
		if !b.Stopped {
			n++
		}
	}
	return n
}

// entries are the plan as --gc lists it: a key per kind, a line per thing with its size, what
// does not read warned, then the total.
func (p gcPlan) entries() []remote.StatusEntry {
	var es []remote.StatusEntry
	add := func(key string, lines ...string) {
		if len(lines) > 0 {
			es = append(es, remote.StatusEntry{Level: remote.StatusInfo, Key: key, Lines: lines})
		}
	}
	var lines []string
	total := int64(0)
	for _, c := range p.OneOffs {
		lines = append(lines, c.words(p.Now))
		total += c.Size
	}
	add("one-offs", lines...)
	lines = nil
	for _, b := range p.Builders {
		lines = append(lines, b.words(p.BuildCache))
		total += b.Size
	}
	add("build cache", lines...)
	lines = nil
	for _, s := range p.Snapshots {
		lines = append(lines, s.words())
		total += s.Size
	}
	add("snapshots", lines...)
	lines = nil
	for _, l := range p.Left {
		lines = append(lines, l.words())
		total += l.Size
	}
	if n := len(p.Dangling.Images); n > 0 {
		lines = append(lines, fmt.Sprintf("%s no container uses, %s", plural(n, "dangling image"), humanBytes(p.Dangling.size())))
		total += p.Dangling.size()
	}
	add("images", lines...)
	for _, u := range p.Unread {
		es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: "not read", Lines: []string{u + ": nothing of it removed"}})
	}
	if p.count() > 0 {
		add("total", "up to "+humanBytes(total))
	}
	return es
}

// gcScope is the server --gc goes over, as its deploy user: Docker by its command docker, the
// release root root, the ages of the server's gc: settings, the door's project users (nil for
// none), now. find fills in the rest, which remove decides by again.
type gcScope struct {
	docker string
	root   string
	ages   remote.ServerGC
	door   *remote.Door
	now    time.Time
	// names are the deploy user's environments' compose names, policies the project users'
	// door policies, by which a name is one of theirs (door.Policy.OwnName).
	names    map[string]bool
	policies []door.Policy
	// projects are the deploy user's project directories, bases its environments'.
	projects, bases []string
	// counts makes find --server-info's, run on a timer: counts only, never a size or the build
	// cache (docker system df -v, inspect --size, buildx, a walk of a snapshot), the one-offs
	// and the images in use from containers, as hostContainers read them (containersErr).
	counts        bool
	containers    []remote.HostContainer
	containersErr error
	// listedAt is when what remove removes was listed, the server's time: the build cache goes
	// by the cutoff its listing had.
	listedAt time.Time
}

// runGCStep implements the hidden GCStepFlag list|remove|remove-listed, as the deploy user on
// the server: remove removes what it finds, remove-listed only what of it the listing on stdin
// (list's output) listed.
func runGCStep(args []string) int {
	if len(args) != 1 || !slices.Contains([]string{"list", "remove", "remove-listed"}, args[0]) {
		return fail("usage: devopsy " + remote.GCStepFlag + " list|remove|remove-listed")
	}
	var listing gcPlan
	if args[0] == "remove-listed" {
		// Read before anything: a listing cut short removes nothing.
		b, err := io.ReadAll(os.Stdin)
		if err == nil {
			err = json.Unmarshal(b, &listing)
		}
		if err != nil {
			return fail("nothing removed: the listing does not read: " + err.Error())
		}
	}
	r, err := remote.ReleaseSettings()
	if err != nil {
		return failErr(err)
	}
	d, err := remote.ServerDoor()
	if err != nil {
		return failErr(err)
	}
	g := &gcScope{docker: "docker", root: r.Root, ages: r.GC, door: d, now: time.Now()}
	plan := g.find()
	switch args[0] {
	case "list":
		if err := json.NewEncoder(os.Stdout).Encode(plan); err != nil {
			return failErr(err)
		}
		return 0
	case "remove-listed":
		plan, g.listedAt = plan.only(listing), listing.Now
	default:
		g.listedAt = plan.Now
	}
	return g.remove(plan)
}

// only is what of p the listing listed too, each by its id: a one-off by its container's,
// a builder by its project and name (the daemon's by its context's), unless the listing said its cache stays (Stopped), a release snapshot
// by its environment and id, a name left by its environment, name and image, a dangling image
// by its id. What p could not read stays in.
func (p gcPlan) only(listing gcPlan) gcPlan {
	p.OneOffs = keepListed(p.OneOffs, listing.OneOffs, func(c gcOneOff) string { return c.ID })
	p.Builders = keepListed(p.Builders, slices.DeleteFunc(slices.Clone(listing.Builders), func(b gcBuilder) bool { return b.Stopped }),
		func(b gcBuilder) string { return b.Project + "\x00" + b.Name })
	p.Snapshots = keepListed(p.Snapshots, listing.Snapshots, gcSnapshot.key)
	p.Left = keepListed(p.Left, listing.Left, func(l gcLeft) string { return leftKey(l.Retried) })
	p.Dangling.Images = keepListed(p.Dangling.Images, listing.Dangling.Images, func(i gcImage) string { return i.ID })
	return p
}

// keepListed are the things of found whose key one of listed has too.
func keepListed[T any](found, listed []T, key func(T) string) []T {
	keys := map[string]bool{}
	for _, x := range listed {
		keys[key(x)] = true
	}
	return slices.DeleteFunc(found, func(x T) bool { return !keys[key(x)] })
}

// key is a release snapshot by its environment and id.
func (s gcSnapshot) key() string { return s.Base + "\x00" + s.ID }

// leftKey is a name a project's record left, by its environment, name and image.
func leftKey(r images.Retried) string { return r.Name + "\x00" + r.Ref + "\x00" + r.Image }

// find is what --gc removes now. Only what reads is listed, and only what a record or Docker's
// own rule gives, never what looks absent: a release root not mounted lists no environment, so
// nothing of it.
func (g *gcScope) find() gcPlan {
	p := gcPlan{Now: g.now, Containers: g.ages.Containers, BuildCache: g.ages.BuildCache}
	unread := func(what string, err error) {
		if err != nil {
			p.Unread = append(p.Unread, what+": "+oneLine(err.Error()))
		}
	}
	if err := exec.Command(g.docker, "version").Run(); err != nil {
		unread("Docker", errors.New("not answering"))
		return p
	}
	g.names = map[string]bool{}
	ps, err := ownProjects(g.root, doorTrees(g.root), ownedByMe)
	unread("the projects under "+g.root, err)
	g.projects = ps
	for _, pd := range ps {
		bases, err := envBases(g.root, pd)
		unread("the environments of "+pd, err)
		for _, b := range bases {
			if rel, err := filepath.Rel(g.root, b); err == nil {
				g.bases = append(g.bases, b)
				g.names[composeNameOf(filepath.ToSlash(rel))] = true
			}
		}
	}
	if g.door != nil && len(g.door.Users) > 0 {
		projects, err := serverProjects(doorProjects(g.door), g.root)
		unread("the project users' names", err)
		if err == nil {
			for _, who := range slices.Sorted(maps.Keys(g.door.Users)) {
				g.policies = append(g.policies, door.Policy{Project: g.door.Users[who].Project, User: who, Others: projects})
			}
		}
	}
	if g.counts {
		p.OneOffs, err = g.countedOneOffs()
	} else {
		p.OneOffs, err = g.oneOffs()
		bs, errs := g.builders()
		p.Builders = bs
		for _, e := range errs {
			p.Unread = append(p.Unread, oneLine(e.Error()))
		}
	}
	unread("the one-off containers", err)
	for _, b := range g.bases {
		sp := envSnapshotPaths(b)
		gone, err := goneSnapshots(sp)
		rel, _ := filepath.Rel(g.root, b)
		for _, rs := range gone {
			s := gcSnapshot{Env: filepath.ToSlash(rel), Base: b, ID: rs.id, Release: rs.release}
			if !g.counts {
				s.Size = treeBytes(filepath.Join(sp.snaps, rs.id))
			}
			p.Snapshots = append(p.Snapshots, s)
		}
		var f *report.Failure
		if errors.As(err, &f) {
			p.Unread = append(p.Unread, "the release snapshots of "+filepath.ToSlash(rel)+": "+f.Msg)
		}
	}
	for _, pd := range g.projects {
		set := projectImages(pd, io.Discard)
		for _, r := range set.Retryable() {
			l := gcLeft{Retried: r}
			if !g.counts {
				if img, err := set.Docker.Inspect(r.Ref); err == nil && len(img.Tags) <= 1 {
					l.Last, l.Size = true, img.Size
				}
			}
			p.Left = append(p.Left, l)
		}
	}
	p.Dangling, err = g.dangling()
	unread("the dangling images", err)
	return p
}

// envSnapshotPaths are the snapshot paths of the environment at base, from the server's own
// settings (snapshotsOf): --gc's script exports no environment's.
func envSnapshotPaths(base string) snapshotPaths {
	snaps := snapshotsOf(base)
	return snapshotPaths{base: base, snaps: snaps, mirror: filepath.Join(snaps, remote.SnapshotMirror)}
}

// listed says whether a compose name is a listed environment's: a deploy user's environment
// directory's, or one under a project user's names (door.Policy.OwnName).
func (g *gcScope) listed(name string) bool {
	return g.names[name] || slices.ContainsFunc(g.policies, func(p door.Policy) bool { return p.OwnName(name) })
}

// countedOneOffs are the one-offs of containers that go (goes), as hostContainers read them,
// with no id and no size.
func (g *gcScope) countedOneOffs() ([]gcOneOff, error) {
	var cs []gcOneOff
	for _, c := range g.containers {
		o := gcOneOff{Project: c.Project, Finished: c.Finished}
		if g.goes(c.State, c.OneOff, o) {
			cs = append(cs, o)
		}
	}
	return cs, g.containersErr
}

// oneOffs are the one-off containers --gc removes: exited (a created one has no finish time),
// of a listed environment, finished more than the containers age ago (gcScope.goes).
func (g *gcScope) oneOffs() ([]gcOneOff, error) {
	out, err := exec.Command(g.docker, "ps", "-a", "-q", "--no-trunc", "--filter", "label=com.docker.compose.oneoff=True", "--filter", "status=exited").Output()
	if err != nil {
		return nil, dockerFailed("docker ps", err)
	}
	cs, err := g.inspectOneOffs(strings.Fields(string(out)))
	return slices.DeleteFunc(cs, func(c gcOneOff) bool { return c.ID == "" }), err
}

// inspectOneOffs are the containers ids as --gc judges them, each that does not go (goes)
// with no ID; one gone since its listing is none, and any other failure an error, with what
// it read.
func (g *gcScope) inspectOneOffs(ids []string) ([]gcOneOff, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out, err := inspectAll(g.docker, ids, "container", "inspect", "--size", "--format", "{{json .}}")
	var cs []gcOneOff
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var v struct {
			ID     string `json:"Id"`
			Name   string
			SizeRw int64
			State  struct {
				Status     string
				FinishedAt time.Time
			}
			Config struct{ Labels map[string]string }
		}
		if json.Unmarshal(sc.Bytes(), &v) != nil {
			continue
		}
		c := gcOneOff{ID: v.ID, Name: strings.TrimPrefix(v.Name, "/"), Project: v.Config.Labels["com.docker.compose.project"], Finished: v.State.FinishedAt, Size: v.SizeRw}
		if !g.goes(v.State.Status, v.Config.Labels["com.docker.compose.oneoff"] == "True", c) {
			c.ID = ""
		}
		cs = append(cs, c)
	}
	return cs, err
}

// goes says whether a container in state status, a one-off or not, goes: an exited one-off
// of a listed environment that finished more than the containers age ago.
func (g *gcScope) goes(status string, oneOff bool, c gcOneOff) bool {
	return status == "exited" && oneOff && !c.Finished.IsZero() && g.now.Sub(c.Finished) > g.ages.Containers && g.listed(c.Project)
}

// What docker buildx prune and du do, as Docker 29.9 and buildx 0.38 do it, tried on a
// throwaway daemon and a throwaway docker-container builder:
//   - prune's --filter until=<duration> goes by when a record was last used, never when it was
//     made: a record made before the cutoff and used since stays;
//   - without -a it removes only the records of local sources and cache mounts; the layers
//     (regular records) go only with -a;
//   - a record stays while one built on it (its child) stays, and goes in the same prune as
//     the last of them;
//   - docker builder prune is buildx prune on the current builder (docker buildx use), which
//     may be any; the daemon's own, of the docker driver, is named for the current Docker
//     context (docker context show, daemonBuilder), whose daemon docker system df -v reads;
//   - on a docker-container builder that is not running (stopped, or made and never started),
//     prune and du fail with "driver not running" and never start it;
//   - du ignores --filter until, and prints its last use only in words ("2 weeks ago"):
//     the times come from the daemon (docker system df -v) for its own builder, and from
//     buildctl in a docker-container builder's container;
//   - BuildKit's own GC keeps, by default, local sources, cache mounts and git checkouts 48
//     hours, anything 60 days, then caps the space.
//
// So --gc lists, per builder, the records prune -a --filter until removes (cacheGoing), and
// leaves a builder not running as it is.

// cacheRecord is one record of a builder's build cache.
type cacheRecord struct {
	ID       string
	Parents  []string
	InUse    bool
	Shared   bool
	Size     int64
	LastUsed time.Time
}

// cacheGoing is how many of records docker buildx prune -a --filter until=<age> removes at
// now, and what of them no image shares: each not in use and last used before the cutoff,
// whose every child goes too.
func cacheGoing(records []cacheRecord, now time.Time, age time.Duration) (n int, size int64) {
	going := map[string]bool{}
	for _, r := range records {
		if !r.InUse && !r.LastUsed.IsZero() && !r.LastUsed.After(now.Add(-age)) {
			going[r.ID] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, r := range records {
			if going[r.ID] {
				continue
			}
			for _, p := range r.Parents {
				if going[p] {
					delete(going, p)
					changed = true
				}
			}
		}
	}
	for _, r := range records {
		if going[r.ID] {
			n++
			if !r.Shared {
				size += r.Size
			}
		}
	}
	return n, size
}

// builders are the builders whose build cache --gc removes: the daemon's own, then each
// project user's project's (door.BuilderOf, under the door's BUILDX_CONFIG as the door makes
// them), one not running listed as kept; one with nothing to remove is left out, and one
// that does not read is an error.
func (g *gcScope) builders() ([]gcBuilder, []error) {
	var bs []gcBuilder
	var errs []error
	name, err := g.daemonBuilder()
	var rs []cacheRecord
	if err == nil {
		rs, err = g.daemonCache()
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("the build cache of the server's Docker: %w", err))
	} else if n, size := cacheGoing(rs, g.now, g.ages.BuildCache); n > 0 {
		bs = append(bs, gcBuilder{Name: name, Records: n, Size: size})
	}
	if g.door == nil || g.door.Space == "" || len(g.door.Users) == 0 {
		return bs, errs
	}
	nodes, err := g.doorBuilders()
	if err != nil {
		return bs, append(errs, fmt.Errorf("the project users' builders: %w", err))
	}
	for _, who := range slices.Sorted(maps.Keys(g.door.Users)) {
		project := g.door.Users[who].Project
		name := door.BuilderOf(project)
		node, ok := nodes[name]
		switch {
		case !ok:
			// No builder: no build yet, or its user ended.
		case node.Status != "running":
			bs = append(bs, gcBuilder{Name: name, Project: project, Stopped: true})
		default:
			rs, err := g.builderCache(node.Name)
			if err != nil {
				errs = append(errs, fmt.Errorf("the build cache of %s: %w", name, err))
				continue
			}
			if n, size := cacheGoing(rs, g.now, g.ages.BuildCache); n > 0 {
				bs = append(bs, gcBuilder{Name: name, Project: project, Records: n, Size: size})
			}
		}
	}
	return bs, errs
}

// buildxEnv is the environment of a docker command on the project users' builders: the door's
// BUILDX_CONFIG.
func (g *gcScope) buildxEnv() []string {
	return append(os.Environ(), "BUILDX_CONFIG="+filepath.Join(g.door.Space, "buildx"))
}

// builderNode is a builder's one node, as docker buildx ls names it: its name, which its
// container's name carries, and its status.
type builderNode struct{ Name, Status string }

// doorBuilders are the builders under the door's BUILDX_CONFIG, each to its node.
func (g *gcScope) doorBuilders() (map[string]builderNode, error) {
	cmd := exec.Command(g.docker, "buildx", "ls", "--format", "json")
	cmd.Env = g.buildxEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, dockerFailed("docker buildx ls", err)
	}
	nodes := map[string]builderNode{}
	for _, line := range strings.Split(string(out), "\n") {
		var b struct {
			Name  string
			Nodes []builderNode
		}
		if json.Unmarshal([]byte(line), &b) == nil && len(b.Nodes) == 1 {
			nodes[b.Name] = b.Nodes[0]
		}
	}
	return nodes, nil
}

// daemonBuilder is the name of the daemon's own builder, the docker driver's, which buildx
// names for the current Docker context (docker context show): the daemon docker system df -v
// reads, never the deploy user's current builder (docker buildx use), which may be another's.
func (g *gcScope) daemonBuilder() (string, error) {
	out, err := exec.Command(g.docker, "context", "show").Output()
	if err != nil {
		return "", dockerFailed("docker context show", err)
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", errors.New("docker context show named no context")
	}
	return name, nil
}

// daemonCache is the daemon's own builder's records, as the daemon gives them with their
// times (docker system df -v, the costliest of Docker's reads: it sizes every volume).
func (g *gcScope) daemonCache() ([]cacheRecord, error) {
	u, err := readDockerUsage(g.docker)
	if err != nil {
		return nil, err
	}
	var rs []cacheRecord
	for _, v := range u.BuildCache {
		r := cacheRecord{ID: v.ID, InUse: v.InUse == "true", Shared: v.Shared == "true", Size: parseDockerSize(v.Size)}
		if v.Parent != "" {
			r.Parents = []string{v.Parent}
		}
		r.LastUsed, _ = time.Parse(dockerTime, cmp.Or(v.LastUsedAt, v.CreatedAt))
		rs = append(rs, r)
	}
	return rs, nil
}

// dockerUsage is docker system df -v as its {{json .}} prints it, its build cache: every value
// a string, sizes in Docker's units (parseDockerSize).
type dockerUsage struct {
	BuildCache []struct {
		ID, Parent, Size, LastUsedAt, CreatedAt string
		InUse, Shared                           string
	}
}

func readDockerUsage(docker string) (dockerUsage, error) {
	var u dockerUsage
	out, err := exec.Command(docker, "system", "df", "-v", "--format", "{{json .}}").Output()
	if err != nil {
		return u, dockerFailed("docker system df", err)
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &u); err != nil {
		return u, fmt.Errorf("docker system df: %w", err)
	}
	return u, nil
}

// dockerTime is how docker's formats print a time (Go's time.Time.String).
const dockerTime = "2006-01-02 15:04:05.999999999 -0700 MST"

// builderCache is a docker-container builder's records, by its node's name, as buildctl in its
// container gives them with their times (buildx's container is buildx_buildkit_<node>).
func (g *gcScope) builderCache(node string) ([]cacheRecord, error) {
	out, err := exec.Command(g.docker, "exec", "buildx_buildkit_"+node, "buildctl", "du", "--format", "{{json .}}").Output()
	if err != nil {
		return nil, dockerFailed("buildctl du", err)
	}
	var vs []struct {
		ID         string    `json:"id"`
		Parents    []string  `json:"parents"`
		InUse      bool      `json:"inUse"`
		Shared     bool      `json:"shared"`
		Size       int64     `json:"size"`
		CreatedAt  time.Time `json:"createdAt"`
		LastUsedAt time.Time `json:"lastUsedAt"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &vs); err != nil {
		return nil, fmt.Errorf("buildctl du: %w", err)
	}
	var rs []cacheRecord
	for _, v := range vs {
		last := v.LastUsedAt
		if last.IsZero() {
			last = v.CreatedAt
		}
		rs = append(rs, cacheRecord{ID: v.ID, Parents: v.Parents, InUse: v.InUse, Shared: v.Shared, Size: v.Size, LastUsed: last})
	}
	return rs, nil
}

// parseDockerSize reads a size as docker prints one (6.06MB, 4.096kB, 0B: units of 1000), 0
// when it cannot.
func parseDockerSize(s string) int64 {
	s = strings.TrimSpace(s)
	for i, unit := range []string{"PB", "TB", "GB", "MB", "kB", "B"} {
		if n, ok := strings.CutSuffix(s, unit); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
			if err != nil {
				return 0
			}
			for range 5 - i {
				f *= 1000
			}
			return int64(f)
		}
	}
	return 0
}

// dangling are the dangling images no container uses, with their size as Docker counts it
// (none counted): what docker image prune removes.
func (g *gcScope) dangling() (gcDangling, error) {
	out, err := exec.Command(g.docker, "image", "ls", "-q", "--no-trunc", "--filter", "dangling=true").Output()
	if err != nil {
		return gcDangling{}, dockerFailed("docker image ls", err)
	}
	ids := slices.Compact(slices.Sorted(slices.Values(strings.Fields(string(out)))))
	if len(ids) == 0 {
		return gcDangling{}, nil
	}
	used, err := g.imagesUsed()
	if err != nil {
		return gcDangling{}, err
	}
	if g.counts {
		var d gcDangling
		for _, id := range ids {
			if !used[id] {
				d.Images = append(d.Images, gcImage{ID: id})
			}
		}
		return d, nil
	}
	out, err = inspectAll(g.docker, ids, "image", "inspect", "--format", "{{.Id}} {{.Size}}")
	if err != nil {
		return gcDangling{}, err
	}
	var d gcDangling
	for _, line := range strings.Split(string(out), "\n") {
		id, size, ok := strings.Cut(line, " ")
		if !ok || used[id] {
			continue
		}
		n, _ := strconv.ParseInt(size, 10, 64)
		d.Images = append(d.Images, gcImage{ID: id, Size: n})
	}
	return d, nil
}

// imagesUsed are the ids of the images a container uses, any state: counted, those of the
// containers hostContainers read.
func (g *gcScope) imagesUsed() (map[string]bool, error) {
	used := map[string]bool{}
	if g.counts {
		for _, c := range g.containers {
			used[c.Image] = true
		}
		return used, g.containersErr
	}
	out, err := exec.Command(g.docker, "ps", "-aq", "--no-trunc").Output()
	if err != nil {
		return nil, dockerFailed("docker ps", err)
	}
	if cs := strings.Fields(string(out)); len(cs) > 0 {
		out, err := inspectAll(g.docker, cs, "container", "inspect", "--format", "{{.Image}}")
		if err != nil {
			return nil, err
		}
		for _, id := range strings.Fields(string(out)) {
			used[id] = true
		}
	}
	return used, nil
}

// remove removes what the plan lists, in its order, each removal standing alone and decided
// again under its lock, never more than the plan: a cut run leaves only less to remove. Its
// exit is gcLeftWarned when something stays: not read, skipped, or not removed.
func (g *gcScope) remove(p gcPlan) int {
	left := len(p.Unread) > 0
	log := report.Default()
	for _, u := range p.Unread {
		log.Warn(u + ": nothing of it removed")
	}
	// 1. One-offs, decided again on a look of their own: docker rm without -f refuses one
	// started since, and never with -v, since a database's compose run keeps its data in an
	// anonymous volume.
	for _, c := range p.OneOffs {
		now, err := g.inspectOneOffs([]string{c.ID})
		if err != nil || len(now) != 1 || now[0].ID == "" {
			continue
		}
		log.Info("removing one-off " + now[0].words(g.now))
		if out, err := exec.Command(g.docker, "rm", c.ID).CombinedOutput(); err != nil {
			log.Warn(now[0].Name+" stays", oneLine(cmp.Or(string(out), err.Error())))
			left = true
		}
	}
	// 2. Build cache, decided by BuildKit itself, by the listing's cutoff: prune takes only a
	// duration, so the time since the listing is added, a second over, so that no record
	// used after the cutoff goes.
	until := "until=" + (g.ages.BuildCache + time.Since(g.listedAt).Truncate(time.Second) + time.Second).String()
	for _, b := range p.Builders {
		if b.Stopped {
			continue
		}
		// Each builder by the name its listing gave, the daemon's its context's (daemonBuilder).
		cmd := exec.Command(g.docker, "buildx", "prune", "--builder", b.Name, "-a", "-f", "--filter", until)
		if b.Project != "" {
			cmd.Env = g.buildxEnv()
		}
		log.Info("removing the build cache of " + b.who() + " unused for " + duration(g.ages.BuildCache))
		out, err := cmd.Output()
		if err != nil {
			log.Warn("the build cache of "+b.who()+" stays", dockerFailed("docker buildx prune", err).Error())
			left = true
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			if v, ok := strings.CutPrefix(line, "Total:"); ok {
				log.Info(b.who() + ": " + humanBytes(parseDockerSize(v)) + " removed, as Docker counts it")
			}
		}
	}
	// 3. Release snapshots whose release is gone, each environment under its snapshot lock,
	// a busy one skipped (its holder prunes), the image lock taken by each removal itself.
	ended := false
	for _, base := range uniqueBases(p.Snapshots) {
		rel, _ := filepath.Rel(g.root, base)
		rel = filepath.ToSlash(rel)
		unlock, ok, err := trySnapshotLock(base)
		if err != nil || !ok {
			log.Warn("the release snapshots of "+rel+" stay: a snapshot, a restore or a removal there holds them, and prunes them itself", errText(err)...)
			left = true
			continue
		}
		r := &snapshotRemoval{cwd: filepath.Join(base, "current"), name: composeNameOf(rel), of: rel, p: envSnapshotPaths(base), wait: waitBound}
		listed := func(id, why string) bool {
			if !slices.ContainsFunc(p.Snapshots, func(s gcSnapshot) bool { return s.Base == base && s.ID == id }) {
				return true // not listed: it stays
			}
			return r.remove(id, why)
		}
		if !pruneGone(r.p, listed) {
			left = true
		}
		unlock()
		if r.ended {
			ended = true
			break
		}
	}
	// 4. Images, under the exclusive image lock taken once: each project's record retried,
	// then the dangling images. Never after a wait for that lock ended: it would wait again.
	switch {
	case ended:
		log.Warn("the server's images stayed busy: the other release snapshots and the images stay, for the next --gc")
		return gcLeftWarned
	case len(p.Left) == 0 && len(p.Dangling.Images) == 0:
	default:
		unlock, err := imagesLock{root: g.root, how: syscall.LOCK_EX, docker: g.docker, who: "--gc", doing: "removing"}.take(context.Background(), log)
		if err != nil {
			log.Warn(err.Error() + ": the images stay, for the next --gc")
			return gcLeftWarned
		}
		listed := func(r images.Retried) bool {
			return slices.ContainsFunc(p.Left, func(l gcLeft) bool { return leftKey(l.Retried) == leftKey(r) })
		}
		for _, pd := range g.projects {
			set := projectImages(pd, errOut)
			set.RetryOnly(listed)
			left = left || slices.ContainsFunc(set.Retryable(), listed)
		}
		if !g.removeDangling(p.Dangling) {
			left = true
		}
		unlock()
	}
	if left {
		return gcLeftWarned
	}
	return 0
}

// removeDangling removes the dangling images of d that are still dangling and used by no
// container, decided again under the image lock (dangling), each by its id: docker rm without
// -f refuses one a container started on since. False when one stays.
func (g *gcScope) removeDangling(d gcDangling) bool {
	if len(d.Images) == 0 {
		return true
	}
	now, err := g.dangling()
	if err != nil {
		report.Warn("the dangling images stay: " + err.Error())
		return false
	}
	ok, n, freed := true, 0, int64(0)
	for _, i := range d.Images {
		if !slices.ContainsFunc(now.Images, func(x gcImage) bool { return x.ID == i.ID }) {
			continue
		}
		if out, err := exec.Command(g.docker, "image", "rm", i.ID).CombinedOutput(); err != nil {
			report.Warn("dangling image "+i.ID+" stays", oneLine(cmp.Or(string(out), err.Error())))
			ok = false
			continue
		}
		n++
		freed += i.Size
	}
	if n > 0 {
		report.Info(fmt.Sprintf("removed %s, %s", plural(n, "dangling image"), humanBytes(freed)))
	}
	return ok
}

// errText is err's text as a detail, none for nil.
func errText(err error) []string {
	if err == nil {
		return nil
	}
	return []string{err.Error()}
}

// uniqueBases are the environments' directories of snapshots, once each, in order.
func uniqueBases(snaps []gcSnapshot) []string {
	var out []string
	for _, s := range snaps {
		if !slices.Contains(out, s.Base) {
			out = append(out, s.Base)
		}
	}
	return out
}

// trySnapshotLock takes the snapshot lock of the environment at base, as the scripts'
// snapshotLock does, if it is free now: ok false when another holds it. Taken, it removes the
// holding file a take killed outright left, as every holder does.
func trySnapshotLock(base string) (unlock func(), ok bool, err error) {
	f, err := os.OpenFile(filepath.Join(base, remote.SnapshotLockFile), os.O_WRONLY|os.O_CREATE, 0o666)
	if err != nil {
		return nil, false, err
	}
	if ok, err := tryFlock(f, syscall.LOCK_EX); !ok {
		f.Close()
		return nil, false, err
	}
	_ = os.Remove(filepath.Join(base, remote.SnapshotHoldingFile))
	return func() { f.Close() }, true, nil
}
