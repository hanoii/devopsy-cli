package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/howto"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// runProjectUsers implements devopsy @<server> --project-users [list | add <project> |
// remove <project>]: the server's project users, which have no Docker socket and ask the
// deploy user's devopsy through the door. Its session runs the server's devopsy as the
// deploy user (ProjectUsersFlag), which does the work there.
func runProjectUsers(t *remote.Target, args []string) int {
	usage := "usage: devopsy @" + targetRef(t) + " --project-users [list | add <project> | remove <project> [--destroy [--yes]]]"
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--project-users"]))
		return 0
	}
	if code := projectUsersArgs(args, false); code != 0 {
		return fail(usage)
	}
	if len(args) > 2 {
		return runDestroyProjectUser(t, args[1], len(args) == 4)
	}
	code, err := remote.SSH(t, remote.ProjectUsersScript(t, args), bytes.NewReader(nil), os.Stdout, false)
	if err != nil {
		return fail(err.Error())
	}
	return code
}

// runDestroyProjectUser is --project-users remove <project> --destroy [--yes]: a first session
// lists what it removes, asked by the project's name typed (--yes: neither), and a second
// removes, deciding everything again.
func runDestroyProjectUser(t *remote.Target, project string, yes bool) int {
	act := remote.ProjectUsersScript(t, []string{"remove", project, "--destroy"})
	if !yes {
		// An older server refuses in this first session, before anything.
		ctx := remote.WithAlsoNeeds(context.Background(), act)
		code, err := remote.SSHContext(ctx, t, nil, remote.ProjectUsersScript(t, []string{"remove", project, "--destroy", "--list"}), bytes.NewReader(nil), os.Stdout, false, nil)
		if err != nil {
			return failErr(err)
		}
		if code != 0 {
			return code
		}
		if err := typeToGoOn("this removes "+door.UserOf(project)+" with all it holds, as listed: its keys, its environments' containers, volumes and data, its images, its tree, its home and its Unix user, and runs no destroy: step of "+project+"'s",
			project, "nothing removed", false); err != nil {
			return failErr(err)
		}
	}
	code, err := remote.SSH(t, act, bytes.NewReader(nil), os.Stdout, false)
	if err != nil {
		return failErr(err)
	}
	if code == sshLost {
		// Each step stands alone, the user held by the destroy's mark meanwhile.
		report.Say(report.ErrorLevel, lostSays(t, "", "the destroy may have stopped halfway there, "+door.UserOf(project)+" held by its mark"), nil,
			report.Fix("", "devopsy @"+targetRef(t)+" --project-users remove "+project+" --destroy"))
	}
	return code
}

// projectUsersArgs says whether args are --project-users's: 0 when they are. Its destroy takes
// --yes from the operator (step false), --list on the server (step), which only lists.
func projectUsersArgs(args []string, step bool) int {
	switch {
	case len(args) == 0, len(args) == 1 && args[0] == "list":
		return 0
	case len(args) == 2 && (args[0] == "add" || args[0] == "remove") && remote.ValidName(args[1]):
		return 0
	case len(args) == 3 || len(args) == 4:
		if args[0] != "remove" || !remote.ValidName(args[1]) || args[2] != "--destroy" {
			return 2
		}
		if len(args) == 3 || (step && args[3] == "--list") || (!step && args[3] == "--yes") {
			return 0
		}
	}
	return 2
}

// runProjectUsersStep is ProjectUsersFlag, on the server as the deploy user.
func runProjectUsersStep(args []string) int {
	if projectUsersArgs(args, true) != 0 {
		return fail("usage: devopsy " + remote.ProjectUsersFlag + " [list | add <project> | remove <project> [--destroy [--list]]]")
	}
	d, err := remote.ServerDoor()
	if err != nil {
		return failErr(err)
	}
	if len(args) == 0 || args[0] == "list" {
		return listProjectUsers(d)
	}
	if d == nil || d.Space == "" {
		report.Fail(errors.New("this server is not set up for project users: "+remote.UserConfigFile()+" has no \"door: space:\""),
			report.Fix("set it to a directory of the deploy user's on a mount of its own, with nosuid and nodev; this prints the steps:", howto.Command(howto.ProjectUsers)))
		return 1
	}
	project := args[1]
	who := door.UserOf(project)
	entry := filepath.Join(remote.DoorDir(), "users", who+".yaml")
	if args[0] == "remove" && len(args) > 2 {
		return runUserDestroy(d, project, who, entry, len(args) == 4)
	}
	if args[0] == "remove" {
		return removeProjectUser(d, project, who, entry, endUser)
	}
	return addProjectUser(d, project, who, entry)
}

// listProjectUsers prints each project user: its project, whether its Unix user is there,
// and how many keys it has; says each with no key that still holds environments (keylessOf);
// and whether sshd waits for a reboot to take the door.
func listProjectUsers(d *remote.Door) int {
	if d == nil || len(d.Users) == 0 {
		report.Info("no project users on this server")
	} else {
		for _, name := range slices.Sorted(maps.Keys(d.Users)) {
			exists := "no Unix user"
			if _, err := user.Lookup(name); err == nil {
				exists = "user there"
			}
			fmt.Printf("%s\tproject %s\t%s\t%d keys\n", name, d.Users[name].Project, exists, keysOf(name))
		}
		for _, k := range keylessOf(d) {
			// The server named alone carries no address to a session: the way out in words.
			report.Default().Say(report.InfoLevel, k.User+" has no key and still holds "+strings.Join(k.Names, ", "), nil,
				report.Hint("removing "+k.Project+"'s project user with --project-users remove and --destroy removes them with it; "+keylessWords))
		}
	}
	// An end a key's or a user's removal began and did not see through (remote.DoorEndingFile),
	// the user listed or not: the door answers it nothing until it completes.
	ending, _ := filepath.Glob(remote.DoorEndingFile("*"))
	removing, _ := filepath.Glob(remote.DoorRemovingFile("*"))
	destroying := destroysPending()
	pending := map[string]bool{}
	for _, f := range append(ending, removing...) {
		if who := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(f), ".ending"), ".removing"); destroying[who] == "" {
			pending[who] = true
		}
	}
	for _, who := range slices.Sorted(maps.Keys(pending)) {
		report.Warn(who+": a removal it had did not complete, and the door answers it nothing until it does",
			"a removal asked again completes it, as does a listing or an edit of its keys while its Unix user is there")
	}
	for _, who := range slices.Sorted(maps.Keys(destroying)) {
		report.Warn(who+": its --project-users remove "+destroying[who]+" --destroy did not complete, and the door answers it nothing",
			"the same command run again completes it")
	}
	if _, err := os.Stat(remote.DoorRebootMarker); err == nil {
		report.Warn("sshd takes the door at the next reboot: until then no project user logs in")
	}
	return 0
}

// destroysPending are the project users whose --project-users remove --destroy did not
// complete (remote.DoorDestroyingFile), each with its project ("?" when its mark does not read).
func destroysPending() map[string]string {
	marks, _ := filepath.Glob(remote.DoorDestroyingFile("*"))
	out := map[string]string{}
	for _, f := range marks {
		who := strings.TrimSuffix(filepath.Base(f), ".destroying")
		out[who] = "?"
		if m, ok, err := readDestroyMark(f); err == nil && ok {
			out[who] = m.project
		}
	}
	return out
}

// projectNameClash is why project may not be added beside the others: compose names are
// flat, so project a's environment b-c and project a-b's environment c are one name, and
// whichever came first would hold the other's networks, volumes and containers.
func projectNameClash(project string, others []string) string {
	// devopsy's own names are under it: the util image's old names (devopsy-util),
	// the project users, their group, the builders.
	if project == "devopsy" || door.Extends(project, "devopsy") {
		return project + " is under devopsy's own names"
	}
	// useradd gives a user a group of its own name, which would be one of devopsy's: the
	// project users' (sshd's Match, the firewall) or the door's (what the door runs under).
	if u := door.UserOf(project); u == remote.DoorGroup || u == remote.DoorRunGroup {
		return "its user, " + u + ", would bear the name of a group of devopsy's"
	}
	for _, o := range others {
		switch {
		case o == project:
		case door.Extends(project, o):
			return project + " extends " + o + "'s name"
		case door.Extends(o, project):
			return o + " extends " + project + "'s name"
		}
	}
	return ""
}

// serverProjects are the projects a name could clash with: the door's users' (door), and the
// release root's, which holds every project released here, running or not, a link there
// included, its disk mounted or not: its name is what holds its names. A root that does not
// read, a missing one included, is an error, never a root with no project: a deploy
// project's names would read as free.
func serverProjects(door []string, root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("the release root, which holds the server's projects, does not read: %w", err)
	}
	ps := slices.Clone(door)
	for _, e := range entries {
		if (e.IsDir() || e.Type()&os.ModeSymlink != 0) && projectDir(e.Name()) {
			ps = append(ps, e.Name())
		}
	}
	return ps, nil
}

// doorProjects are the door's users' projects, none without a door.
func doorProjects(d *remote.Door) []string {
	var ps []string
	if d != nil {
		for _, u := range d.Users {
			ps = append(ps, u.Project)
		}
	}
	return ps
}

// doorDockerFix is the way out of a Docker the door refuses (doorHostReady), for --server-info and
// --project-users add: its containerd image store and a current buildx, by the project-users
// topic's steps.
var doorDockerFix = report.Fix("move Docker to its containerd image store and a current buildx, as root:", howto.Command(howto.ProjectUsers))

// spaceFix is how a door's space gets nosuid and nodev, the one way every refusal of it offers.
func spaceFix(space string) report.Remedy {
	return report.Fix("mount it so, for example with this line in /etc/fstab:", space+" "+space+" none bind,nosuid,nodev 0 0")
}

// doorNameOwner is the project user whose names compose project name sits under, by the
// door's rule (door.Policy.OwnName) among the server's projects (serverProjects, root the
// release root, and project, the target's own, whose directory a first release has yet to
// make), "" for none or no door: the door acts only on containers it made, and refuses
// that user's every compose command while another's are under its names. With no project
// user nothing is read: the root may not be there yet (a server before its first release),
// and nothing of it changes the answer. With one, the root is there (--project-users add
// makes it first), and one that does not read is an error.
func doorNameOwner(d *remote.Door, root, project, name string) (string, error) {
	if d == nil || len(d.Users) == 0 {
		return "", nil
	}
	projects, err := serverProjects(doorProjects(d), root)
	if err != nil {
		return "", err
	}
	projects = append(projects, project)
	for who, u := range d.Users {
		if (door.Policy{Project: u.Project, User: who, Others: projects}).OwnName(name) {
			return who, nil
		}
	}
	return "", nil
}

func addProjectUser(d *remote.Door, project, who, entry string) int {
	// A removal of the user's, or a key's removal's end, not seen through: what settles it
	// would take what add writes now (its entry), so it goes first, its own refusal before any
	// other's.
	if fileExists(remote.DoorDestroyingFile(who)) {
		report.Fail(errors.New("a --project-users remove "+project+" --destroy did not complete, and adding the user would undo it"),
			report.Fix("run it again, which completes it, then add it"))
		return 1
	}
	if fileExists(remote.DoorRemovingFile(who)) || fileExists(remote.DoorEndingFile(who)) {
		report.Fail(errors.New("a removal of "+who+"'s did not complete, and finishing it would undo this add"),
			report.Fix("ask for the removal again, which finishes it, or list the user's keys if only a key was being removed, then add it"))
		return 1
	}
	// The door's tools, before anything else: without them the host's pieces fail halfway
	// (visudo's check, a setfacl of the tree).
	if missing := missingTools(doorTool); len(missing) > 0 {
		report.Fail(errors.New("no "+strings.Join(missing, " or ")+" on this server, which project users need: no project user made"),
			toolFix(true))
		return 1
	}
	// The release root a project user's tree goes in, as every release needs it.
	r, err := remote.ReleaseSettings()
	if err != nil {
		return failErr(err)
	}
	if err := releaseRootReady(r.Root, "no project user made", ""); err != nil {
		report.Fail(err)
		return 1
	}
	if err := spaceGuarded(d.Space); err != nil {
		report.Fail(err, spaceFix(d.Space))
		return 1
	}
	if err := staticSelf(); err != nil {
		report.Fail(err, report.Fix("install a release of devopsy, which is static (install.sh)"))
		return 1
	}
	if err := doorHostReady("docker", os.Environ(), ""); err != nil {
		report.Fail(err, doorDockerFix)
		return 1
	}
	if u, ok := d.Users[who]; ok && u.Project != project {
		return fail(who + " is " + u.Project + "'s already")
	}
	projects, err := serverProjects(doorProjects(d), r.Root)
	if err != nil {
		report.Fail(fmt.Errorf("%w: no project user made", err))
		return 1
	}
	if why := projectNameClash(project, projects); why != "" {
		report.Fail(errors.New("a project "+project+" here would share compose names: "+why),
			report.Fix("a project gone from the release root ("+r.Root+") holds its name while its directory stays, its .env kept by --destroy: removed with its last environment's --destroy --purge"))
		return 1
	}
	// What runs under the project's names stays out of the door's reach (foreignUnder), so a
	// project the deploy user runs moves only once it is down.
	if err := foreignUnder(door.Policy{Project: project, User: who, Others: projects}, dockerCommand); err != nil {
		return failErr(err)
	}
	me, err := user.Current()
	if err != nil {
		return failErr(err)
	}
	// With releases: root elsewhere than the deploy user's home (a block volume, say), the
	// project's tree is there too, which only root may make the project user's.
	tree, root := "", ""
	if r.Root != me.HomeDir {
		tree, root = filepath.Join(r.Root, project), r.Root
	}
	if err := remote.DoorDevopsyPath(devopsyPath()); err != nil {
		return failErr(err)
	}
	// The project users' firewall, loaded and checked before any project user can log in.
	wrong, err := doorFirewall(remote.FirewallInstall(true), true)
	if err != nil {
		return failErr(err)
	}
	if len(wrong) > 0 {
		report.Error("the project users' firewall did not load as devopsy's: nothing else changed", wrong...)
		return 1
	}
	// The deploy user's own projects, closed to project users: none reads another's.
	closed, err := closeDeployProjects(r.Root, d.Space, os.Getuid())
	if err != nil {
		return failErr(err)
	}
	if len(closed) > 0 {
		report.Info("closed to project users the deploy user's projects in " + r.Root + ": " + strings.Join(closed, ", "))
	}
	// An id given once (takeProjectID), and only to a user the host step makes: one there is
	// taken as it is, and so is the group of its name an add cut after it left (leftGroupID),
	// so that a failed or cut add takes no id again.
	id := ""
	if _, err := user.Lookup(who); errors.As(err, new(user.UnknownUserError)) {
		h := setupHost{root: os.Getenv(remote.HostRootEnv)}
		n, left, err := leftGroupID(h.path("/etc/group"), who)
		if err == nil && !left {
			n, err = takeProjectID(projectIDsGiven(), h.path("/etc/passwd"), h.path("/etc/group"))
		}
		if err != nil {
			report.Fail(fmt.Errorf("%w: no project user made", err))
			return 1
		}
		id = strconv.Itoa(n)
	} else if err != nil {
		return failErr(err)
	}
	var out bytes.Buffer
	if err := addUserRun(me.Username, devopsyPath(), who, tree, root, id).run(&out, func(err error) error {
		return errors.New("making " + who + " failed: " + err.Error())
	}); err != nil {
		return fail(err.Error())
	}
	if _, ok := d.Users[who]; !ok {
		body := "project: " + project + "\n"
		if tree != "" {
			body += "tree: " + tree + "\n"
		}
		if err := os.MkdirAll(filepath.Dir(entry), 0o700); err != nil {
			return failErr(err)
		}
		if err := os.WriteFile(entry, []byte(body), 0o600); err != nil {
			return failErr(err)
		}
	}
	report.Info("project user "+who+" for "+project+", its entry in "+entry,
		"its keys: devopsy @<deploy user>@<server>: --ssh-keys --for "+project+" add <public key file>")
	if strings.Contains(out.String(), "reboot") {
		report.Warn("sshd takes the door at the next reboot: until then no project user logs in",
			"any reboot takes it, one the server does by itself for /run/reboot-required included")
	}
	warnProcessesShown(report.Default(), setupHost{root: os.Getenv(remote.HostRootEnv)}.path(procMounts))
	return 0
}

// projectIDsGiven is the deploy user's record of the highest id --project-users add gave a
// project user, in DoorDir.
func projectIDsGiven() string { return filepath.Join(remote.DoorDir(), "uids-given") }

// takeProjectID is the id the next project user and its group take, one number: past both the
// highest the record gives and the highest of the project users' range (door.ProjectIDMin to
// ProjectIDMax) that passwd and group hold, recorded before anything makes the user, under the
// record's lock, its wait said and bounded. An id is never given twice: a project added after
// another's removal would own what the removed user left on the host under its uid (a file
// outside its tree, a snapshot root of its own). It refuses once the range is spent.
func takeProjectID(record, passwd, group string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
		return 0, err
	}
	lock, err := os.OpenFile(record+".lock", os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	if err := (boundedWait{what: "another --project-users add", bound: time.Minute}).wait(context.Background(), report.Default(), func() (bool, error) {
		return tryFlock(lock, syscall.LOCK_EX)
	}); err != nil {
		return 0, err
	}
	highest := door.ProjectIDMin - 1
	data, err := os.ReadFile(record)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if s := strings.TrimSpace(string(data)); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("%s, the ids given to project users, does not read: %q", record, s)
		}
		highest = max(highest, n)
	}
	for _, f := range []string{passwd, group} {
		data, err := os.ReadFile(f)
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			if fields := strings.Split(line, ":"); len(fields) >= 3 {
				if n, err := strconv.Atoi(fields[2]); err == nil && n >= door.ProjectIDMin && n <= door.ProjectIDMax {
					highest = max(highest, n)
				}
			}
		}
	}
	next := highest + 1
	if next > door.ProjectIDMax {
		return 0, fmt.Errorf("every id of the project users' range, %d to %d, was given once already", door.ProjectIDMin, door.ProjectIDMax)
	}
	tmp := record + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(next)+"\n"), 0o600); err != nil {
		return 0, err
	}
	return next, os.Rename(tmp, record)
}

// leftGroupID is the gid of the group of who's name in group (a group file) that a project
// user's add left with no user (one cut between its groupadd and its useradd, or a removal on
// a host that keeps a user's group), left false when there is none. One with a member, or a gid
// out of the project users' range, is no project user's, and refused.
func leftGroupID(group, who string) (gid int, left bool, err error) {
	data, err := os.ReadFile(group)
	if err != nil {
		return 0, false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 4 || f[0] != who {
			continue
		}
		n, err := strconv.Atoi(f[2])
		if err != nil || n < door.ProjectIDMin || n > door.ProjectIDMax || f[3] != "" {
			return 0, false, fmt.Errorf("a group %s is there already (gid %s), and no project user's left: a project user's group is one --project-users add made", who, f[2])
		}
		return n, true, nil
	}
	return 0, false, nil
}

// projectDir says whether a directory named name in a release root is a project's: none
// starting with a dot (a home's .cache, .config) or a removal cut off (removing).
func projectDir(name string) bool {
	return !strings.HasPrefix(name, ".") && !removing.MatchString(name)
}

// closeDeployProjects closes to everyone else each project directory in root (projectDir)
// that the deploy user (uid) owns and others may enter, the door's space aside, and says
// which: its projects, made before the server had project users with the session's umask (a
// release closes its own since, remote's prelude), while a project user enters root to reach
// its own tree when root is not the deploy user's home.
func closeDeployProjects(root, space string, uid int) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var closed []string
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o077 == 0 || !projectDir(e.Name()) || filepath.Join(root, e.Name()) == filepath.Clean(space) {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != uid {
			continue
		}
		if err := os.Chmod(filepath.Join(root, e.Name()), 0o700); err != nil {
			return closed, err
		}
		closed = append(closed, e.Name())
	}
	return closed, nil
}

// removeKept removes what the door kept for project's user in space (door.LoginsDirOf,
// door.ImagesFileOf): its logins, its record of claimed images, and the record's lock last,
// under that lock.
func removeKept(space, project string) error {
	record := door.ImagesFileOf(space, project)
	lock, err := os.OpenFile(record+".lock", os.O_CREATE|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		// No record was ever written, so no lock: only its logins, if any.
		return os.RemoveAll(door.LoginsDirOf(space, project))
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	if err := os.RemoveAll(door.LoginsDirOf(space, project)); err != nil {
		return err
	}
	for _, f := range []string{record, record + ".lock"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// removeProjectUser takes who's keys, then ends its processes (end): the door answers it no
// more first, so nothing it runs meanwhile gets a new call through. Its entry stays: a removed
// project user is one with no key, as a fresh add leaves it, its names its own
// (doorNameOwner), so no deploy project takes them while what it left is there, and its tree
// known; a key added takes it back. Asked again, it does it all again.
func removeProjectUser(d *remote.Door, project, who, entry string, end func(who string) error) int {
	removing, keys := remote.DoorRemovingFile(who), remote.DoorKeysFile(who)
	// A destroy holds its marks until its last step: only it completes them.
	if fileExists(remote.DoorDestroyingFile(who)) {
		report.Fail(errors.New("a --project-users remove "+project+" --destroy did not complete"),
			report.Fix("run it again, which completes it"))
		return 1
	}
	// A user the config names, under door: users:, has no entry file (remote.ServerDoor).
	inConfig := false
	if d != nil && !fileExists(entry) {
		_, inConfig = d.Users[who]
	}
	if !fileExists(entry) && !inConfig {
		// A removal that did not see itself through, whose user's door: users: line went since:
		// asked again, it does what is left, its keys first.
		if _, err := os.Lstat(removing); err == nil {
			if err := lockedKeys(keys, func([]string) ([]string, error) { return nil, nil }); err != nil {
				return failErr(err)
			}
			return finishRemoval(d, project, who, end)
		}
		return fail("no project user for " + project + " here")
	}
	if err := markRemoval(project, who, nil); err != nil {
		return failErr(err)
	}
	if code := finishRemoval(d, project, who, end); code != 0 || !inConfig {
		return code
	}
	// A user the config names: its entry is the admin's to drop, everything else of a removal
	// (its keys, which sshd takes from the keys file alone, its end, what the door kept) done
	// here the same. Its line is the one record of its names: dropped while something is under
	// them, nothing would destroy it any more.
	if held, err := heldBy(d, project, who, entry); err != nil || len(held) > 0 {
		f := &report.Failure{Msg: who + "'s keys and processes are gone, and it still holds what is under its names, which its door: users: line in " + remote.UserConfigFile() + " is the one record of",
			Details: held, Remedies: []report.Remedy{report.Fix("remove them with it first, with --project-users remove " + project + " --destroy, then drop the line")}}
		if err != nil {
			f.Details = append(f.Details, "what it holds did not read: "+err.Error())
		}
		return failErr(f)
	}
	report.Fail(errors.New(who+"'s keys and processes are gone, nothing is under its names, and door: users: in "+remote.UserConfigFile()+" still names it"),
		report.Fix("remove it there"))
	return 1
}

// heldBy is what is under project's names on the server, as its destroy would find it.
func heldBy(d *remote.Door, project, who, entry string) ([]string, error) {
	r, err := remote.ReleaseSettings()
	if err != nil {
		return nil, err
	}
	x := newUserDestroy(d, project, who, entry, r.Root)
	if err := x.withPolicy(); err != nil {
		return nil, err
	}
	return x.left()
}

// markRemoval is a removal's first stretch, under the keys' lock: also first (a destroy's own
// mark, nil for none), the removal's mark (remote.DoorRemovingFile, with its project) and its
// end's, and the keys emptied, so a key's removal meanwhile, which holds that lock for its end,
// finds them all or none, and clears only its own mark. A failure or a cut session past here
// leaves the marks, which a removal asked again, or the next edit or listing of the user's
// keys, finish, the door answering the user nothing meanwhile.
func markRemoval(project, who string, also func() error) error {
	return lockedKeys(remote.DoorKeysFile(who), func([]string) ([]string, error) {
		if also != nil {
			if err := also(); err != nil {
				return nil, err
			}
		}
		if err := os.WriteFile(remote.DoorRemovingFile(who), []byte(project+"\n"), 0o600); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(remote.DoorEndingFile(who), nil, 0o600)
	})
}

// finishRemoval is endRemoval, then says what stays.
func finishRemoval(d *remote.Door, project, who string, end func(who string) error) int {
	if err := endRemoval(d, project, who, end); err != nil {
		return failErr(err)
	}
	stays := "its entry, Unix user, home and tree stay, holding its names: a key added for it (--ssh-keys --for " + project + ") takes it back, and --project-users remove " + project + " --destroy removes them all"
	if _, err := user.Lookup(who); err != nil {
		stays = "its entry stays, holding its names; its Unix user was gone already"
	}
	report.Info("removed "+who+"'s keys and ended its processes: it logs in no more, and the door answers it no more", stays)
	return 0
}

// endRemoval ends who's processes (end), clears the end's mark, and removes what the door
// kept for it: the part of a removal that a failure lets it ask again.
func endRemoval(d *remote.Door, project, who string, end func(who string) error) error {
	// It ends before anything else may fail: what it left running must not outlive a removal
	// that stopped halfway.
	if err := endProjectUser(who, end); err != nil {
		return &report.Failure{Msg: "removed " + who + "'s keys, but " + err.Error(),
			Remedies: []report.Remedy{report.Fix("ask for the removal again once that is fixed: the end is marked, and the door answers " + who + " nothing meanwhile")}}
	}
	// What the door kept for it: its registry logins and the record of the images it claimed,
	// under the record's lock (doorSide.whileAUser), so a door request past its sudo before
	// the removal, which writes them only while the user has a key, brings neither back.
	if d != nil && d.Space != "" {
		if err := removeKept(d.Space, project); err != nil {
			return err
		}
	}
	// Its marks last, the removal's after the end's: what is left until then is done again.
	for _, f := range []string{remote.DoorEndingFile(who), remote.DoorRemovingFile(who)} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
