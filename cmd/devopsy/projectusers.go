package main

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

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
	usage := "usage: devopsy @" + targetRef(t) + " --project-users [list | add <project> | remove <project>]"
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--project-users"]))
		return 0
	}
	if code := projectUsersArgs(args); code != 0 {
		return fail(usage)
	}
	code, err := remote.SSH(t, remote.ProjectUsersScript(t, args), bytes.NewReader(nil), os.Stdout, false)
	if err != nil {
		return fail(err.Error())
	}
	return code
}

// projectUsersArgs says whether args are --project-users's: 0 when they are.
func projectUsersArgs(args []string) int {
	switch {
	case len(args) == 0, len(args) == 1 && args[0] == "list":
		return 0
	case len(args) == 2 && (args[0] == "add" || args[0] == "remove") && remote.ValidName(args[1]):
		return 0
	}
	return 2
}

// runProjectUsersStep is ProjectUsersFlag, on the server as the deploy user.
func runProjectUsersStep(args []string) int {
	if projectUsersArgs(args) != 0 {
		return fail("usage: devopsy " + remote.ProjectUsersFlag + " [list | add <project> | remove <project>]")
	}
	d, err := remote.ServerDoor()
	if err != nil {
		return failErr(err)
	}
	if len(args) == 0 || args[0] == "list" {
		return listProjectUsers(d)
	}
	if d == nil || d.Space == "" {
		report.Fail(errors.New("this server has no door: no door: space: in "+remote.UserConfigFile()),
			report.Fix("give it one in that file, a directory of the deploy user's on a mount of its own with nosuid and nodev, as root:", howto.Command(howto.ProjectUsers)))
		return 1
	}
	project := args[1]
	who := door.UserOf(project)
	entry := filepath.Join(remote.DoorDir(), "users", who+".yaml")
	if args[0] == "remove" {
		return removeProjectUser(d, project, who, entry, endUser)
	}
	return addProjectUser(d, project, who, entry)
}

// listProjectUsers prints each project user: its project, whether its Unix user is there,
// and how many keys it has; and whether sshd waits for a reboot to take the door.
func listProjectUsers(d *remote.Door) int {
	if d == nil || len(d.Users) == 0 {
		report.Info("no project users on this server")
	} else {
		for _, name := range slices.Sorted(maps.Keys(d.Users)) {
			exists := "no Unix user"
			if _, err := user.Lookup(name); err == nil {
				exists = "user there"
			}
			data, _ := os.ReadFile(remote.DoorKeysFile(name))
			fmt.Printf("%s\tproject %s\t%s\t%d keys\n", name, d.Users[name].Project, exists, len(remote.ParseAuthorizedKeys(string(data))))
		}
	}
	// An end a key's or a user's removal began and did not see through (remote.DoorEndingFile),
	// the user listed or not: the door answers it nothing until it completes.
	ending, _ := filepath.Glob(remote.DoorEndingFile("*"))
	removing, _ := filepath.Glob(remote.DoorRemovingFile("*"))
	pending := map[string]bool{}
	for _, f := range append(ending, removing...) {
		pending[strings.TrimSuffix(strings.TrimSuffix(filepath.Base(f), ".ending"), ".removing")] = true
	}
	for _, who := range slices.Sorted(maps.Keys(pending)) {
		report.Warn(who+": a removal it had did not complete, and the door answers it nothing until it does",
			"a removal asked again completes it, as does a listing or an edit of its keys while its Unix user is there")
	}
	if _, err := os.Stat(remote.DoorRebootMarker); err == nil {
		report.Warn("sshd takes the door at the next reboot: until then no project user logs in")
	}
	return 0
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
// release root's, which holds every project released here, running or not.
func serverProjects(door []string, root string) []string {
	ps := slices.Clone(door)
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if e.IsDir() && projectDir(e.Name()) {
				ps = append(ps, e.Name())
			}
		}
	}
	return ps
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
// that user's every compose command while another's are under its names.
func doorNameOwner(d *remote.Door, root, project, name string) string {
	if d == nil {
		return ""
	}
	projects := append(serverProjects(doorProjects(d), root), project)
	for who, u := range d.Users {
		if (door.Policy{Project: u.Project, User: who, Others: projects}).OwnName(name) {
			return who
		}
	}
	return ""
}

func addProjectUser(d *remote.Door, project, who, entry string) int {
	// The door's tools, before anything: without them the host's pieces fail halfway (visudo's
	// check, a setfacl of the tree).
	if missing := doorToolsMissing(missingTools()); len(missing) > 0 {
		report.Fail(errors.New("no "+strings.Join(missing, " or ")+" on this server, which project users need: no project user made"),
			report.Fix("install sudo and acl with the system's packages, as root:", howto.Command(howto.Tools)))
		return 1
	}
	// A removal of the user's, or a key's removal's end, not seen through: what settles it
	// would take what add writes now (its entry), so it goes first.
	if fileExists(remote.DoorRemovingFile(who)) || fileExists(remote.DoorEndingFile(who)) {
		report.Fail(errors.New("a removal of "+who+"'s did not complete, and finishing it would undo this add"),
			report.Fix("ask for the removal again, which finishes it, or list the user's keys if only a key was being removed, then add it"))
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
	projects := serverProjects(doorProjects(d), r.Root)
	if why := projectNameClash(project, projects); why != "" {
		report.Fail(errors.New("a project "+project+" here would share compose names: "+why),
			report.Fix("a project gone from the release root ("+r.Root+") holds its name while its directory stays, its .env kept by --destroy: removed with its last environment's --destroy --purge"))
		return 1
	}
	// What runs under the project's names stays out of the door's reach (door.Policy.Foreign),
	// so a project the deploy user runs moves only once it is down.
	all, err := containerLabels(exec.Command("docker", containerRows...))
	if err != nil {
		return failErr(err)
	}
	if ids := (door.Policy{Project: project, User: who, Others: projects}).Foreign(all); len(ids) > 0 {
		report.Fail(foreignError(project, ids, all))
		return 1
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
	var out bytes.Buffer
	if err := addUserRun(me.Username, devopsyPath(), who, tree, root).run(&out, func(err error) error {
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

// removeProjectUser takes who's entry and keys, then ends its processes (end): the door
// answers it no more first, so nothing it runs meanwhile gets a new call through.
func removeProjectUser(d *remote.Door, project, who, entry string, end func(who string) error) int {
	ending, removing := remote.DoorEndingFile(who), remote.DoorRemovingFile(who)
	keys := remote.DoorKeysFile(who)
	if _, err := os.Stat(entry); err != nil {
		// A removal before that did not see itself through (its own mark, never a key's
		// removal's): asked again, it does what is left, its keys first, the entry being gone.
		if _, err := os.Lstat(removing); err == nil {
			if err := lockedKeys(keys, func([]string) ([]string, error) { return nil, nil }); err != nil {
				return failErr(err)
			}
			return finishRemoval(d, project, who, end)
		}
		// A user the config names: its entry is the admin's to drop, and everything else of a
		// removal (its keys, which sshd takes from the keys file alone, its end, what the door
		// kept) is the door's, done here the same, so following the words below leaves nothing.
		if d != nil {
			if _, ok := d.Users[who]; ok {
				if err := lockedKeys(keys, func([]string) ([]string, error) {
					if err := os.WriteFile(removing, []byte(project+"\n"), 0o600); err != nil {
						return nil, err
					}
					return nil, os.WriteFile(ending, nil, 0o600)
				}); err != nil {
					return failErr(err)
				}
				if code := finishRemoval(d, project, who, end); code != 0 {
					return code
				}
				report.Fail(errors.New(who+"'s keys and processes are gone, and door: users: in "+remote.UserConfigFile()+" still names it"),
					report.Fix("remove it there"))
				return 1
			}
		}
		return fail("no project user for " + project + " here")
	}
	// Under the keys' lock, in one stretch: the removal's mark (remote.DoorRemovingFile, with
	// its project) and its end's, the entry and the keys, so a key's removal meanwhile, which
	// holds that lock for its end, finds them all or none, and clears only its own mark. A
	// failure or a cut session past here leaves the marks, which a removal asked again, or the
	// next edit or listing of the user's keys, finish, the door answering the user nothing
	// meanwhile.
	if err := lockedKeys(keys, func([]string) ([]string, error) {
		if err := os.WriteFile(removing, []byte(project+"\n"), 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(ending, nil, 0o600); err != nil {
			return nil, err
		}
		return nil, os.Remove(entry)
	}); err != nil {
		return failErr(err)
	}
	return finishRemoval(d, project, who, end)
}

// finishRemoval ends who's processes (end), clears the end's mark, and removes what the door
// kept for it: the part of a removal that a failure lets it ask again.
func finishRemoval(d *remote.Door, project, who string, end func(who string) error) int {
	// It ends before anything else may fail: what it left running must not outlive a removal
	// that stopped halfway.
	if err := endProjectUser(who, end); err != nil {
		report.Fail(errors.New("removed "+who+"'s entry and keys, but "+err.Error()),
			report.Fix("ask for the removal again once that is fixed: the end is marked, and the door answers "+who+" nothing meanwhile"))
		return 1
	}
	// What the door kept for it: its registry logins and the record of the images it claimed,
	// under the record's lock (doorSide.whileAUser), so a door request past its sudo before
	// the removal, which writes them only while the user has a key, brings neither back.
	if d != nil && d.Space != "" {
		if err := removeKept(d.Space, project); err != nil {
			return failErr(err)
		}
	}
	// Its marks last, the removal's after the end's: what is left until then is done again.
	for _, f := range []string{remote.DoorEndingFile(who), remote.DoorRemovingFile(who)} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return failErr(err)
		}
	}
	stays := "its Unix user, home and tree stay"
	if _, err := user.Lookup(who); err != nil {
		stays = "its Unix user was gone already"
	}
	report.Info("removed "+who+"'s entry and keys, and ended its processes: it logs in no more, and the door answers it no more", stays)
	return 0
}
