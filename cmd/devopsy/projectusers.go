package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/door"
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
			report.Fix("give it one: door: space: <dir> in that file, a directory of the deploy user's on a mount of its own with nosuid and nodev"))
		return 1
	}
	project := args[1]
	who := remote.DoorUserOf(project)
	entry := filepath.Join(remote.DoorDir(), "users", who+".yaml")
	if args[0] == "remove" {
		return removeProjectUser(d, project, who, entry)
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
	if _, err := os.Stat(remote.DoorRebootMarker); err == nil {
		report.Warn("sshd takes the door at the next reboot: until then no project user logs in")
	}
	return 0
}

// projectNameClash is why project may not be added beside the others: compose names are
// flat, so project a's environment b-c and project a-b's environment c are one name, and
// whichever came first would hold the other's networks, volumes and containers.
func projectNameClash(project string, others []string) string {
	for _, o := range others {
		switch {
		case o == project:
		case strings.HasPrefix(project, o+"-"):
			return project + " extends " + o + "'s name"
		case strings.HasPrefix(o, project+"-"):
			return o + " extends " + project + "'s name"
		}
	}
	return ""
}

// serverProjects are the projects a name could clash with: the door's users' and the release
// root's, which holds every project released here, running or not.
func serverProjects(d *remote.Door, root string) []string {
	var ps []string
	for _, u := range d.Users {
		ps = append(ps, u.Project)
	}
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				ps = append(ps, e.Name())
			}
		}
	}
	return ps
}

// doorNameOwner is the project user whose names compose project name sits under, by the
// door's rule (door.Policy.OwnName) among the server's projects (serverProjects, root the
// release root), "" for none or no door: the door acts only on containers it made, and
// refuses that user's every compose command while another's are under its names.
func doorNameOwner(d *remote.Door, root, name string) string {
	if d == nil {
		return ""
	}
	projects := serverProjects(d, root)
	for who, u := range d.Users {
		if (door.Policy{Project: u.Project, User: who, Others: projects}).OwnName(name) {
			return who
		}
	}
	return ""
}

func addProjectUser(d *remote.Door, project, who, entry string) int {
	if err := spaceGuarded(d.Space); err != nil {
		report.Fail(err, report.Fix("mount it so, for example with this line in /etc/fstab:", d.Space+" "+d.Space+" none bind,nosuid,nodev 0 0"))
		return 1
	}
	if u, ok := d.Users[who]; ok && u.Project != project {
		return fail(who + " is " + u.Project + "'s already")
	}
	r, err := remote.ReleaseSettings()
	if err != nil {
		return failErr(err)
	}
	projects := serverProjects(d, r.Root)
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
	if err := ensureUtilImage(context.Background()); err != nil {
		return failErr(err)
	}
	var out bytes.Buffer
	c := exec.Command("docker", "run", "--rm", "--network", "none", "-v", "/:/host", remote.UtilImage,
		"chroot", "/host", "/bin/sh", "-c", remote.DoorHostScript(me.Username, devopsyPath()), "sh", who, tree, root)
	c.Stdout, c.Stderr = &out, os.Stderr
	if err := c.Run(); err != nil {
		return fail("making " + who + " failed: " + err.Error())
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
	if missing, err := doorFirewallMissing(); err == nil && len(missing) > 0 {
		report.Warn("project users reach what the door's firewall rule keeps them off: "+strings.Join(missing, ", "),
			"devopsy @<server> --server-info says how to add it")
	}
	if strings.Contains(out.String(), "reboot") {
		report.Warn("sshd takes the door at the next reboot: until then no project user logs in",
			"devopsy-server's DEVOPSY_AUTO_REBOOT_TIME takes it, as a kernel update (/run/reboot-required)")
	}
	return 0
}

func removeProjectUser(d *remote.Door, project, who, entry string) int {
	if _, err := os.Stat(entry); err != nil {
		if d != nil {
			if _, ok := d.Users[who]; ok {
				return fail(who + " is under door: users: in " + remote.UserConfigFile() + ": remove it there")
			}
		}
		return fail("no project user for " + project + " here")
	}
	if err := os.Remove(entry); err != nil {
		return failErr(err)
	}
	if err := os.Remove(remote.DoorSyncKeysFile(who)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return failErr(err)
	}
	if err := os.Remove(remote.DoorKeysFile(who)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return failErr(err)
	}
	report.Info("removed "+who+"'s entry and keys: it logs in no more, and the door answers it no more",
		"its Unix user, home and tree stay, with what runs: a running session ends on its own")
	return 0
}
