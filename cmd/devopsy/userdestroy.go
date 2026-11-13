package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/uids"
)

// destroyMark is what a --project-users remove --destroy's mark holds (remote.DoorDestroyingFile):
// its project, the user's home and uid as they were while it could be looked up, and whether
// its host step began.
type destroyMark struct {
	project, home, uid string
	hostBegun          bool
}

// hostStepBegun is the mark's line written before the host step deletes the user.
const hostStepBegun = "host step begun"

// readDestroyMark reads file, ok false when it is not there.
func readDestroyMark(file string) (m destroyMark, ok bool, err error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	m.project = lines[0]
	for _, l := range lines[1:] {
		switch k, v, _ := strings.Cut(l, " "); {
		case k == "home":
			m.home = v
		case k == "uid":
			m.uid = v
		case l == hostStepBegun:
			m.hostBegun = true
		}
	}
	if !remote.ValidName(m.project) {
		return m, false, fmt.Errorf("%s does not read: no project on its first line", file)
	}
	return m, true, nil
}

// write writes m into file, by rename.
func (m destroyMark) write(file string) error {
	s := m.project + "\n"
	if m.home != "" {
		s += "home " + m.home + "\n"
	}
	if m.uid != "" {
		s += "uid " + m.uid + "\n"
	}
	if m.hostBegun {
		s += hostStepBegun + "\n"
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte(s), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// userDestroy is one --project-users remove <project> --destroy, as the deploy user: a removed
// project user's environments, data, images, tree, home and Unix user, found by the deploy
// user's own records (the uid claims, the containers, volumes and networks under the project's
// names, the door's space), never read as the user, and no code of the user's run (no destroy:
// step). Docker is the deploy user's own.
type userDestroy struct {
	d                   *remote.Door
	project, who, entry string
	// root is the release root, store its uid claims.
	root  string
	store uids.Store
	// policy is the project's door policy, built from its entry and the server's other
	// projects alone (never doorPolicy, which looks the user up): its names, its space.
	policy door.Policy
	// tree is the entry's tree:, "" for none (the home's <home>/<project>); config whether the
	// user is under door: users:, with no entry file.
	tree   string
	config bool
	mark   string
	// end ends the user's processes and builder (endUser), lookup looks a user up
	// (user.Lookup), projectUser says whether a user is in the project users' group
	// (inDoorGroup), host runs a script as root on the host (hostRun.run).
	end         func(who string) error
	lookup      func(who string) (*user.User, error)
	projectUser func(who string) bool
	host        func(r hostRun, stdout io.Writer) error
	// stopAfter cuts the run after that step, for TestRemovalCuts; 0 never.
	stopAfter int
}

// runUserDestroy is --project-users remove <project> --destroy on the server (list: only what
// it would remove, for the confirmation; else it removes, deciding everything again).
func runUserDestroy(d *remote.Door, project, who, entry string, list bool) int {
	r, err := remote.ReleaseSettings()
	if err != nil {
		return failErr(err)
	}
	return newUserDestroy(d, project, who, entry, r.Root).run(list)
}

// newUserDestroy is project's destroy, its user who, its entry file entry, root the release
// root, with the host's own ways to end, look up and run as root.
func newUserDestroy(d *remote.Door, project, who, entry, root string) *userDestroy {
	return &userDestroy{d: d, project: project, who: who, entry: entry, root: root, store: claimsStore(root),
		mark: remote.DoorDestroyingFile(who), end: endUser, lookup: user.Lookup, projectUser: inDoorGroup,
		host: func(h hostRun, stdout io.Writer) error {
			return h.run(stdout, func(err error) error { return fmt.Errorf("the host step failed: %w", err) })
		}}
}

// userDestroySteps are the steps of a destroy after its checks, each standing alone: a cut
// one is done again from what is left.
var userDestroySteps = []string{"the removal", "the names", "the project's claims", "the images", "the door's directories", "the end's check", "the host step", "the entry and the mark"}

func (x *userDestroy) run(list bool) int {
	m, marked, err := readDestroyMark(x.mark)
	if err != nil {
		return failErr(err)
	}
	if marked && m.project != x.project {
		return fail(x.mark + " is " + m.project + "'s destroy, not " + x.project + "'s")
	}
	u, known := x.d.Users[x.who]
	if !known {
		if !marked {
			return fail("no project user for " + x.project + " here")
		}
		if list {
			report.Default().Say(report.InfoLevel, "what was "+x.who+"'s is gone but its mark", nil, runAgain(report.Hint, "run it again, which checks and removes it:", "its destroy run again checks and removes it"))
			return 0
		}
		return x.afterEntry()
	}
	x.tree, x.config = u.Tree, !fileExists(x.entry)
	if err := x.ready(m, marked); err != nil {
		return failErr(err)
	}
	f, err := x.find()
	if err == nil {
		err = x.checked(f)
	}
	if err != nil {
		return failErr(fmt.Errorf("%w: nothing removed", err))
	}
	if list {
		return x.print(f, m)
	}
	for i, step := range []func(*destroyMark) error{x.removal, x.names, x.projectClaims, x.images, x.doorDirs, x.endCheck, x.hostStep, x.last} {
		if err := step(&m); err != nil {
			report.Fail(err, runAgain(report.Fix, "the destroy stopped at "+userDestroySteps[i]+", "+x.who+" held by its mark meanwhile: once that is fixed, run it again, which goes on from what is left:",
				"the destroy stopped at "+userDestroySteps[i]+", "+x.who+" held by its mark meanwhile: once that is fixed, its destroy run again goes on from what is left"))
			return 1
		}
		if x.stopAfter == i+1 {
			return 1
		}
	}
	if x.config {
		report.Fail(errors.New(x.who+" is gone with all it held, and door: users: in "+remote.UserConfigFile()+" still names it"),
			runAgain(report.Fix, "remove it there, then run it again, which removes its mark:", "remove it there, then run its destroy again, which removes its mark"))
		return 1
	}
	report.Info("removed "+x.who+" with all it held: its environments, data, images, tree, home and Unix user", destroyLeaves...)
	return 0
}

// runAgain is kind offering the operator's command (remote.CommandEnv), which a session on the
// server carries, under text, or words when it carries none: a server named alone carries no
// address to build it from.
func runAgain(kind func(string, ...string) report.Remedy, text, words string) report.Remedy {
	return offer(kind, text, remote.SessionFrom(os.Getenv).Command, words)
}

// destroyLeaves is what a destroy leaves of a project user's, said with what it removes.
var destroyLeaves = []string{
	"left: snapshots under a snapshots: root of the user's own outside its tree, which the deploy user holds no record of and never reads, and the user's lines in /etc/cron.deny and /etc/at.deny",
}

// ready refuses, before anything, a release root that reads as not there: the entry's tree
// not a directory, until the host step began (never by the Unix user's absence: an admin may
// have deleted it by hand), and the server's projects not read (serverProjects), which the
// policy's names stop at. A root not mounted reads as no claims, and the destroy would end
// with the data and claims coming back at the next mount. And what the host step
// (remote.DoorUserDeleteScript) would refuse, which it would find only once the rest is gone:
// a Unix user of that name outside the project users' group, a tree or a home it does not
// remove as root (removablePaths), and, until the host step began, a home that is there as a
// link or no directory.
func (x *userDestroy) ready(m destroyMark, marked bool) error {
	if err := x.withPolicy(); err != nil {
		return err
	}
	if _, err := x.lookup(x.who); err == nil && !x.projectUser(x.who) {
		return errors.New(x.who + " is no project user, not in " + remote.DoorGroup + ": nothing removed")
	}
	if x.tree != "" && !(marked && m.hostBegun) {
		if fi, err := os.Lstat(x.tree); err != nil || !fi.IsDir() {
			return &report.Failure{Msg: x.tree + ", the tree " + x.who + "'s entry names, is not a directory (a disk not mounted, or removed by hand): nothing removed",
				Remedies: []report.Remedy{report.Fix("mount the disk it is on, or make the directory again, then run it again")}}
		}
	}
	home, _ := x.homeOf(m)
	if err := x.removablePaths(x.tree, home); err != nil {
		return fmt.Errorf("%w: nothing removed", err)
	}
	if home != "" && !(marked && m.hostBegun) {
		// Lstat: a link is no directory, and the host step removes nothing through one.
		if fi, err := os.Lstat(home); err == nil && !fi.IsDir() {
			return &report.Failure{Msg: home + ", " + x.who + "'s home, is a link or no directory, which the destroy never removes: nothing removed",
				Remedies: []report.Remedy{report.Fix("remove it by hand, with what it links to if that is the user's, then run it again")}}
		}
	}
	return nil
}

// removablePaths refuses a tree or a home ("" for none) the host step does not remove as root
// (destroyablePath): ready's look before anything, and the host step's own again.
func (x *userDestroy) removablePaths(tree, home string) error {
	if tree != "" && !destroyablePath(tree, x.project) {
		return fmt.Errorf("%s, the tree its entry names, is not a project's directory (%s under a parent)", tree, x.project)
	}
	if home != "" && !destroyablePath(home, x.who) {
		return fmt.Errorf("%s, its home, is not a home of %s's", home, x.who)
	}
	return nil
}

// withPolicy sets the project's door policy from its entry and the server's other projects
// (serverProjects): a root that does not read refuses, its deploy projects' names otherwise
// read as this one's.
func (x *userDestroy) withPolicy() error {
	projects, err := serverProjects(doorProjects(x.d), x.root)
	if err != nil {
		return fmt.Errorf("%w: nothing removed", err)
	}
	x.policy = door.Policy{Project: x.project, User: x.who, Space: x.d.Space, Others: projects}
	return nil
}

// destroyFound is what a destroy finds of a project's, by the deploy user's own records.
type destroyFound struct {
	claims []uids.Claim
	// names are the compose names under the project's names (door.Policy.OwnName) that the
	// claims, the containers', volumes' and networks' labels and <space>/mnt give, sorted;
	// claimNames those of the environment claims, which may be another's (checked).
	names, claimNames []string
	// containers, volumes and networks are those under its names, by id or name.
	containers, volumes, networks []string
	// refs are the image names only it writes (door.Policy.OwnRef), its claim holds included.
	refs []string
	// dirs are its directories in the door's space: sealed copies, clones, work directories.
	dirs []string
	// images is the size of the images refs name, each counted once.
	images int64
}

// find lists what of the project's is on the server. A read that fails is an error, never
// none.
func (x *userDestroy) find() (destroyFound, error) {
	var f destroyFound
	claims, err := projectClaims(x.store, x.project)
	if err != nil {
		return f, fmt.Errorf("the uid claims do not read: %w", err)
	}
	f.claims = claims
	names := map[string]bool{}
	for _, c := range claims {
		if c.Owner.Env != "" {
			f.claimNames = append(f.claimNames, c.Owner.ComposeName())
		}
	}
	all, err := containerLabels(exec.Command("docker", containerRows...))
	if err != nil {
		return f, err
	}
	for id, labels := range all {
		hit := false
		for _, n := range []string{labels["com.docker.compose.project"], labels["devopsy.env"]} {
			if x.policy.OwnName(n) {
				names[n], hit = true, true
			}
		}
		if hit {
			f.containers = append(f.containers, id)
		}
	}
	for kind, list := range map[string]*[]string{"volume": &f.volumes, "network": &f.networks} {
		rows, err := labelRows(kind)
		if err != nil {
			return f, err
		}
		for _, r := range rows {
			if x.policy.OwnName(r[1]) {
				names[r[1]] = true
			}
			// Compose names a volume or a network <compose name>_<key>.
			if x.policy.OwnName(r[1]) || x.policy.OwnVolume(r[0]) {
				*list = append(*list, r[0])
			}
		}
	}
	mnts, err := os.ReadDir(filepath.Join(x.policy.Space, "mnt"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return f, err
	}
	for _, e := range mnts {
		if x.policy.OwnName(e.Name()) {
			names[e.Name()] = true
		}
	}
	for _, n := range f.claimNames {
		names[n] = true
	}
	f.names = slices.Sorted(maps.Keys(names))
	if f.refs, f.images, err = x.ownRefs(); err != nil {
		return f, err
	}
	for _, d := range []string{x.policy.Sealed(), x.policy.Clones()} {
		if _, err := os.Lstat(d); err == nil {
			f.dirs = append(f.dirs, d)
		}
	}
	work, err := os.ReadDir(filepath.Join(x.policy.Space, "work"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return f, err
	}
	for _, e := range work {
		if ownWorkName(x.policy, e.Name()) {
			f.dirs = append(f.dirs, filepath.Join(x.policy.Space, "work", e.Name()))
		}
	}
	slices.Sort(f.containers)
	slices.Sort(f.volumes)
	slices.Sort(f.networks)
	return f, nil
}

// labelRows are docker's volumes or networks (kind), each its name and its compose project's
// label, one JSON array a line: a label's value is anyone's to write.
func labelRows(kind string) ([][2]string, error) {
	out, err := exec.Command("docker", kind, "ls", "--format", `[{{json .Name}},{{json (.Label "com.docker.compose.project")}}]`).Output()
	if err != nil {
		return nil, dockerFailed("docker "+kind+" ls", err)
	}
	var rows [][2]string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var row []string
		if err := json.Unmarshal([]byte(line), &row); err != nil || len(row) != 2 {
			return nil, fmt.Errorf("docker %s ls: %q does not read", kind, line)
		}
		rows = append(rows, [2]string{row[0], row[1]})
	}
	return rows, nil
}

// ownRefs are the image names only the project writes (door.Policy.OwnRef: its bookmarks, fetch
// tags, snapshot bookmarks, builds, and its claim holds), sorted, and the size of their images,
// each counted once. No compose name is read back from one.
func (x *userDestroy) ownRefs() ([]string, int64, error) {
	rows, err := imageRows("docker")
	if err != nil {
		return nil, 0, err
	}
	var refs []string
	var size int64
	counted := map[string]bool{}
	for _, r := range rows {
		if !x.policy.OwnRef(r.Ref) {
			continue
		}
		refs = append(refs, r.Ref)
		if !counted[r.ID] {
			counted[r.ID] = true
			size += r.Size
		}
	}
	slices.Sort(refs)
	return slices.Compact(refs), size, nil
}

// ownWorkName says whether a work directory's name is the project's (doorSide.ownWork).
func ownWorkName(p door.Policy, name string) bool {
	return strings.HasPrefix(name, p.Project+"-") && p.OwnName(name)
}

// checked refuses, before anything, a name found that is not the project's (a claim
// env:shop:eu-prod gives shop-eu-prod, which a deploy project shop-eu may hold now), naming
// both: removeLeft force-removes by label whoever made the container, and a down by name takes
// whatever compose finds. And the claims' directory missing while containers or volumes are
// under the names: a door start makes it (uids.Store.LockStart), so the release root's disk
// may not be mounted.
func (x *userDestroy) checked(f destroyFound) error {
	for _, n := range f.names {
		if err := x.ownName(n); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(x.store.Dir); errors.Is(err, os.ErrNotExist) && (len(f.containers) > 0 || len(f.volumes) > 0) {
		return fmt.Errorf("the uid claims' directory %s is not there, and containers or volumes are under %s's names: the release root's disk may not be mounted", x.store.Dir, x.project)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ownName refuses a compose name that is not the project's, naming the project it is.
func (x *userDestroy) ownName(name string) error {
	if x.policy.OwnName(name) {
		return nil
	}
	whose := ""
	for _, o := range x.policy.Others {
		if (name == o || door.Extends(name, o)) && len(o) > len(whose) {
			whose = o
		}
	}
	return fmt.Errorf("%s, a name of %s's claims, is under %s's names now: nothing of it is %s's to remove", name, x.project, whose, x.project)
}

// print lists what a destroy removes, sizes with it (the tree and the home by a root du, metadata
// only), and what it leaves.
func (x *userDestroy) print(f destroyFound, m destroyMark) int {
	home, uid := x.homeOf(m)
	tree := x.tree
	if tree == "" && home != "" {
		tree = filepath.Join(home, x.project)
	}
	sizes := x.sizes(tree, home)
	line := func(what string, items []string) {
		if len(items) > 0 {
			fmt.Printf("%s\t%s\n", what, strings.Join(items, ", "))
		}
	}
	var claims []string
	for _, c := range f.claims {
		claims = append(claims, fmt.Sprintf("%s (%d)", c.Owner, c.UID))
	}
	line("uid claims", claims)
	line("compose names", f.names)
	if n := len(f.containers) + len(f.volumes) + len(f.networks); n > 0 {
		line("under them", []string{plural(len(f.containers), "container"), plural(len(f.volumes), "volume"), plural(len(f.networks), "network")})
	}
	if len(f.refs) > 0 {
		line("images", []string{plural(len(f.refs), "name") + ", " + humanBytes(f.images)})
	}
	line("door's space", f.dirs)
	for _, p := range []string{tree, home} {
		if p == "" {
			continue
		}
		size := "its size unread"
		if n, ok := sizes[p]; ok {
			size = humanBytes(n)
		}
		what := "tree"
		if p == home {
			what = "home"
		}
		line(what, []string{p + " (" + size + ")"})
	}
	if uid != "" {
		line("Unix user", []string{x.who + " (uid " + uid + ")"})
	}
	report.Info("no destroy: step of "+x.project+"'s runs, nor anything of its user's: what it holds is removed as it is", destroyLeaves...)
	return 0
}

// homeOf is the user's home and uid: the mark's, else the password database's.
func (x *userDestroy) homeOf(m destroyMark) (home, uid string) {
	if m.home != "" || m.uid != "" {
		return m.home, m.uid
	}
	if u, err := x.lookup(x.who); err == nil {
		return u.HomeDir, u.Uid
	}
	return "", ""
}

// sizes are the bytes of each of paths there, by du as root on the host (the deploy user reads
// neither a tree nor a home), counting no link's target: metadata only, nothing of the user's
// run. One that does not read is left out.
func (x *userDestroy) sizes(paths ...string) map[string]int64 {
	var there []string
	for _, p := range paths {
		if p != "" {
			there = append(there, p)
		}
	}
	sizes := map[string]int64{}
	if len(there) == 0 {
		return sizes
	}
	var out strings.Builder
	_ = x.host(hostRun{Script: `for p in "$@"; do [ ! -d "$p" ] || [ -L "$p" ] || printf '%s\t%s\n' "$(du -skx -- "$p" | cut -f1)" "$p"; done`, Args: there, Stderr: io.Discard}, &out)
	for _, l := range strings.Split(out.String(), "\n") {
		k, p, ok := strings.Cut(l, "\t")
		if n, err := strconv.ParseInt(k, 10, 64); ok && err == nil {
			sizes[p] = n * 1024
		}
	}
	return sizes
}

// removal is the user's removal, as --project-users remove's (its keys, its end, what the door
// kept), the destroy's mark written with its marks in the same stretch under the keys' lock,
// the home and uid in it while the user can be looked up, a mark's kept: sshd gives it no key
// from that instant, sync keys included.
func (x *userDestroy) removal(m *destroyMark) error {
	m.project = x.project
	if m.home == "" && m.uid == "" {
		if u, err := x.lookup(x.who); err == nil {
			m.home, m.uid = u.HomeDir, u.Uid
		}
	}
	report.Info("removing " + x.who + "'s keys and ending its processes")
	if err := markRemoval(x.project, x.who, func() error { return m.write(x.mark) }); err != nil {
		return err
	}
	return endRemoval(x.d, x.project, x.who, x.end)
}

// names removes what is under the project's compose names: devopsy's own containers of every
// name first (removeLeft: the door's checks read all the project's names, so a cut destroy's
// container of one would refuse another's pass), then each environment claim, in their order,
// through freeUIDs under its start lock exclusive, which waits for a door start already past
// its checks, its down the destroy's (down); then every name with no claim left, by the same
// down under no start lock: the door refuses every start with no claim.
func (x *userDestroy) names(*destroyMark) error {
	f, err := x.find()
	if err != nil {
		return err
	}
	for _, n := range f.names {
		if err := x.ownName(n); err != nil {
			return err
		}
		if err := removeLeft(n, ""); err != nil {
			return err
		}
	}
	downed := map[string]bool{}
	down := func(name string) error {
		downed[name] = true
		return x.down(name)
	}
	slices.SortFunc(f.claims, func(a, b uids.Claim) int { return strings.Compare(a.Owner.String(), b.Owner.String()) })
	for _, c := range f.claims {
		if c.Owner.Env == "" {
			continue
		}
		// No release of the user's can start: it has no key, the mark holds it, its processes
		// are ended.
		if err := freeUIDs(x.store, c.Owner, func() error { return nil }, down); err != nil {
			return fmt.Errorf("freeing %s: %w", c.Owner, err)
		}
	}
	for _, n := range f.names {
		if !downed[n] {
			if err := down(n); err != nil {
				return err
			}
		}
	}
	return nil
}

// down takes the project's compose name down, its name checked first naming whose it is
// (ownName), then as the door's down is (checkedProjectDown), devopsy's own containers of it
// removed first (removeLeft): compose's down --volumes by name, and the name's mnt in the
// door's space emptied as root and removed.
func (x *userDestroy) down(name string) error {
	if err := x.ownName(name); err != nil {
		return err
	}
	return checkedProjectDown(x.policy, dockerCommand, name, func() error { return removeLeft(name, "") }, func() error {
		if err := downByName(realDocker())(name); err != nil {
			return fmt.Errorf("taking %s down: %w", name, err)
		}
		mnt := x.policy.Mnt(name)
		if _, err := os.Lstat(mnt); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err := emptyAsRoot(context.Background(), mnt, mnt, newContainer("destroy", name, strconv.Itoa(os.Getpid()), remote.TaskDestroy)); err != nil {
			return err
		}
		return os.Remove(mnt)
	})
}

// projectClaims frees the project's claims with no environment claim left (Env ""), each under
// its start lock: a run cut between an environment's free and its project's leaves one that no
// other record names.
func (x *userDestroy) projectClaims(*destroyMark) error {
	claims, err := projectClaims(x.store, x.project)
	if err != nil {
		return err
	}
	for _, c := range claims {
		if c.Owner.Env != "" {
			continue
		}
		lock, err := x.store.LockStart(c.Owner, syscall.LOCK_EX)
		if err != nil {
			return err
		}
		_, err = x.store.FreeIfUnused(c.Owner, lock)
		lock.Close()
		if err != nil {
			return fmt.Errorf("freeing %s: %w", c.Owner, err)
		}
	}
	return nil
}

// images untags every name only the project writes (door.Policy.OwnRef), under the server's
// image lock exclusive, taken once, no start lock held: never -f, so a name a container still
// uses fails, and the run stops there to go on later.
func (x *userDestroy) images(*destroyMark) error {
	unlock, err := imagesLock{root: x.root, how: syscall.LOCK_EX, who: "--project-users remove " + x.project + " --destroy", doing: "removing"}.take(context.Background(), report.Default())
	if err != nil {
		return err
	}
	defer unlock()
	refs, _, err := x.ownRefs()
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		report.Info("removing " + plural(len(refs), "image name") + " only " + x.project + " writes")
	}
	for _, ref := range refs {
		if out, err := exec.Command("docker", "image", "rm", ref).CombinedOutput(); err != nil {
			return fmt.Errorf("removing the image name %s: %w: %s", ref, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// doorDirs removes the project's directories in the door's space, as root where other uids'
// files are (removeSpaceDir): its sealed copies, its work directories, its clones.
func (x *userDestroy) doorDirs(*destroyMark) error {
	f, err := x.find()
	if err != nil {
		return err
	}
	docker := func(args ...string) *exec.Cmd { return exec.Command(realDocker(), args...) }
	for _, d := range f.dirs {
		if err := removeSpaceDir(docker, d); err != nil {
			return err
		}
	}
	return nil
}

// endCheck stops the destroy, its mark held, while anything of the project's is left: a claim,
// a container, volume or network under its names, a mnt of its names, an image name only it
// writes, a directory of its in the door's space.
func (x *userDestroy) endCheck(*destroyMark) error {
	left, err := x.left()
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return errors.New("left of " + x.project + "'s: " + strings.Join(left, "; "))
	}
	return nil
}

// left is what of the project's is still there, in words.
func (x *userDestroy) left() ([]string, error) {
	f, err := x.find()
	if err != nil {
		return nil, err
	}
	var left []string
	add := func(what string, items []string) {
		if len(items) > 0 {
			left = append(left, what+" "+strings.Join(items, ", "))
		}
	}
	var claims []string
	for _, c := range f.claims {
		claims = append(claims, c.Owner.String())
	}
	add("the uid claims", claims)
	add("the containers", f.containers)
	add("the volumes", f.volumes)
	add("the networks", f.networks)
	var mnts []string
	for _, n := range f.names {
		if _, err := os.Lstat(x.policy.Mnt(n)); err == nil {
			mnts = append(mnts, x.policy.Mnt(n))
		}
	}
	add("the mnts", mnts)
	add("the image names", f.refs)
	add("the door's directories", f.dirs)
	return left, nil
}

// hostStep ends the user again (a door build past its checks runs as the deploy user and may
// have made the builder anew), marks the host step begun, then deletes the user as root on the
// host (remote.DoorUserDeleteScript): its user manager's mask, the user with its home, the
// entry's tree, the home by the mark's path.
func (x *userDestroy) hostStep(m *destroyMark) error {
	if err := x.end(x.who); err != nil {
		return fmt.Errorf("its processes could not be ended: %w", err)
	}
	tree, home, uid := x.tree, m.home, m.uid
	if _, err := strconv.ParseUint(uid, 10, 32); err != nil {
		uid = ""
	}
	if err := x.removablePaths(tree, home); err != nil {
		return fmt.Errorf("%w: left", err)
	}
	if !m.hostBegun {
		m.hostBegun = true
		if err := m.write(x.mark); err != nil {
			return err
		}
	}
	report.Info("deleting the Unix user " + x.who + " with its home and tree, as root")
	return x.host(hostRun{Write: true, Script: remote.DoorUserDeleteScript, Args: []string{x.who, uid, tree, home}}, errOut)
}

// destroyablePath says whether p is a path a destroy removes as root: absolute, clean, below
// the root, and named base, as add made it.
func destroyablePath(p, base string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p && filepath.Base(p) == base && filepath.Dir(p) != "/"
}

// last removes the entry, then the mark: a cut between them leaves the mark alone, which a run
// again settles (afterEntry). A user under door: users: keeps its mark until its admin drops
// its line.
func (x *userDestroy) last(*destroyMark) error {
	if x.config {
		return nil
	}
	if err := os.Remove(x.entry); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(x.mark)
}

// afterEntry settles a mark whose user is no longer an entry or a door: users: line: it checks
// what the destroy's end does (left), and removes the mark only when nothing of the project's
// is left and the Unix user is gone. A cut between the entry's removal and the mark's leaves
// the names unguarded, so a deploy project may take them, and the check then fails for good:
// the mark is then the admin's to remove by hand.
func (x *userDestroy) afterEntry() int {
	if err := x.withPolicy(); err != nil {
		return failErr(err)
	}
	left, err := x.left()
	if err == nil && len(left) == 0 {
		if _, lerr := x.lookup(x.who); !errors.As(lerr, new(user.UnknownUserError)) {
			left, err = []string{"the Unix user " + x.who}, lerr
		}
	}
	if err != nil {
		return failErr(err)
	}
	if len(left) > 0 {
		report.Fail(errors.New(x.who+"'s destroy did not complete, and its entry or door: users: line is gone: left "+strings.Join(left, "; ")),
			report.Fix("once what is left is known to be no longer "+x.project+"'s, remove "+x.mark+" by hand"))
		return 1
	}
	if err := os.Remove(x.mark); err != nil {
		return failErr(err)
	}
	report.Info("removed " + x.who + " with all it held")
	return 0
}
