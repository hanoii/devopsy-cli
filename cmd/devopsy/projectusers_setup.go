package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/howto"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// runProjectUsersSetup implements devopsy @<server> --project-users setup [<directory>]
// [--yes]: what a server with project users needs of it that devopsy can set (the door's
// space, its mount and its name in the config, every user's processes hidden from the
// others), in two sessions as the deploy user as --server-setup has: the first reads the
// facts, which the server's devopsy judges; the operator shows what is missing and asks once;
// the second applies under the server's apply lock and checks again.
func runProjectUsersSetup(t *remote.Target, args []string) int {
	dir, yes, ok := projectUsersSetupArgs(args)
	if !ok {
		return fail("usage: devopsy @" + targetRef(t) + " --project-users setup [<directory>] [--yes]")
	}
	step := []string{"setup", "facts"}
	apply := []string{"setup", "apply"}
	command := "--project-users setup"
	if dir != "" {
		step, apply, command = append(step, dir), append(apply, dir), command+" "+remote.QuoteWord(dir)
	}
	// An older server refuses in this first session, before anything: it carries the apply's
	// needs too.
	ctx := remote.WithAlsoNeeds(context.Background(), remote.ProjectUsersScript(t, apply))
	var out bytes.Buffer
	code, err := remote.SSHContext(ctx, t, nil, remote.ProjectUsersScript(t, step), bytes.NewReader(nil), &out, false, nil)
	if err != nil {
		return failErr(err)
	}
	if code == remote.AskedAsProjectUser {
		refuseProjectUser(t, out.String(), "project users are the server's deploy user's", command)
		return 1
	}
	if code != 0 {
		return code
	}
	var f setup.DoorFacts
	if err := json.Unmarshal(out.Bytes(), &f); err != nil {
		return fail("the server's facts for project users: " + err.Error())
	}
	es := doorSetupEntries(f)
	newEntryTable(os.Stdout, styleFor(os.Stdout), es).print(es)
	said := sayRefusalsOf(f.Refusals, nil, "", report.ErrorLevel, nil, false)
	refused := len(f.Refused()) > 0
	if !f.Applies() {
		if refused {
			report.Error(cmp.Or(refusedWords(f.Stopped()), "nothing changed"))
			return 1
		}
		report.Info("nothing to change: " + t.Host + " is set up for project users")
		return 0
	}
	doorSetupNotes(f)
	if !yes {
		ask := terminalAsker{t: t, command: command, yes: "applies what is shown"}
		var not []string
		if line := refusedWords(f.Stopped()); line != "" {
			not = []string{line}
		}
		ok, err := ask.confirm("apply these?", not...)
		if err != nil {
			return failErr(err)
		}
		if !ok {
			report.Error("nothing changed")
			return 1
		}
	}
	// The refusals said here, by their identities: the apply says only one new since.
	apply = append(apply[:2], append([]string{"--said=" + strings.Join(said, ",")}, apply[2:]...)...)
	code, err = remote.SSH(t, remote.ProjectUsersScript(t, apply), bytes.NewReader(nil), os.Stdout, false)
	if err != nil {
		return failErr(err)
	}
	return code
}

// projectUsersSetupArgs reads --project-users setup's arguments: a directory, --yes, in any
// order, each once.
func projectUsersSetupArgs(args []string) (dir string, yes, ok bool) {
	for _, a := range args {
		switch {
		case a == "--yes" && !yes:
			yes = true
		case strings.HasPrefix(a, "/") && dir == "":
			dir = a
		default:
			return "", false, false
		}
	}
	return dir, yes, true
}

// doorSetupNotes says, before the question, what the apply does beyond the differences: the
// lines appended to the deploy user's config, and what hiding processes does to the server.
func doorSetupNotes(f setup.DoorFacts) {
	if f.Config != "" && f.Goes(setup.StepConfig) {
		report.Info("the apply appends to "+f.ConfigFile+":", strings.Split(strings.TrimSuffix(f.Config, "\n"), "\n")...)
	}
	if f.Changes(setup.StepProcesses) && f.Goes(setup.StepProcesses) {
		report.Info("hiding processes: each user sees only its own, the deploy user and "+setup.ProcGroup+" every one",
			"a tool running as another user that reads /proc (a monitoring agent) sees only its own processes until it is put in "+setup.ProcGroup)
	}
}

// doorSetupEntries are what --project-users setup shows before it asks, as --status shows
// entries: each item's differences, a warning, or its state, a line.
func doorSetupEntries(f setup.DoorFacts) []remote.StatusEntry {
	var es []remote.StatusEntry
	add := func(key string, lines []string, same string) {
		if len(lines) > 0 {
			es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: key, Lines: lines})
		} else if same != "" {
			es = append(es, remote.StatusEntry{Level: remote.StatusInfo, Key: key, Lines: []string{same}})
		}
	}
	add(setup.ItemSpace, doorItemLines(f, setup.StepSpace), f.Space+", on a mount with nosuid and nodev")
	add(setup.ItemMounts, doorItemLines(f, setup.StepDoorDrop), "")
	same := "/proc hides every user's processes from the others, " + setup.ProcGroup + " sees them all"
	if p := f.Processes; p.Other {
		same = "/proc hides every user's processes from the others (hidepid=" + p.HidePID + ", gid=" + p.GID + "), set outside devopsy and left as it is"
	}
	add(setup.ItemProcesses, doorItemLines(f, setup.StepProcesses), same)
	same = "door: space: " + f.Space
	if slices.ContainsFunc(f.Refusals, func(r setup.Refusal) bool { return r.Item == setup.ItemConfig }) {
		same = ""
	}
	add(setup.ItemConfig, doorItemLines(f, setup.StepConfig), same)
	return es
}

// doorItemLines are what differs of step s's item in f, none when in sync: what the first
// session shows and what an apply that did not hold names.
func doorItemLines(f setup.DoorFacts, s setup.Step) []string {
	if !f.Changes(s) {
		return nil
	}
	var lines []string
	// A unit's link is said only beside its file in sync: a unit missing is enabled with it.
	files := func(item string) {
		for _, x := range f.Files {
			switch {
			case x.Item != item || x.InSync:
			case x.Link != "":
				if !slices.ContainsFunc(f.Files, func(u setup.FileFact) bool { return u.Path == x.Link && !u.InSync }) {
					lines = append(lines, x.Path+" does not enable "+filepath.Base(x.Link)+": no boot starts it")
				}
			case x.Missing:
				lines = append(lines, x.Path+" is missing")
			default:
				lines = append(lines, x.Path+" is not devopsy's")
			}
		}
	}
	switch s {
	case setup.StepSpace:
		switch f.SpaceAction {
		case setup.RootMake:
			lines = append(lines, f.Space+" is missing: made, the deploy user's, mode 755")
		case setup.RootGive:
			lines = append(lines, f.Space+" is root's: given to the deploy user")
		}
		if f.SpaceMode != "" {
			lines = append(lines, f.Space+" is mode "+f.SpaceMode+": set to 755")
		}
		if !f.Guarded {
			lines = append(lines, f.Space+" is on no mount of its own with nosuid and nodev: bound on itself so, now and at every boot")
		}
		files(setup.ItemSpace)
	case setup.StepDoorDrop:
		files(setup.ItemMounts)
	case setup.StepProcesses:
		p := f.Processes
		switch {
		case p.Hides():
		case !setup.Hiding(p.HidePID):
			lines = append(lines, "/proc shows every user's processes (no hidepid): hidden from the others, now and at every boot")
		default:
			lines = append(lines, "/proc hides processes with gid="+cmp.Or(p.GID, "none")+", not "+setup.ProcGroup+"'s")
		}
		if p.Group == "" {
			lines = append(lines, "no group "+setup.ProcGroup+": made, the group that sees every process, the deploy user in it")
		}
		if !p.Member && p.Group != "" {
			lines = append(lines, "the deploy user is not in "+setup.ProcGroup+": put in it")
		}
		files(setup.ItemProcesses)
	case setup.StepConfig:
		lines = append(lines, f.ConfigFile+" has no door: (the lines below are appended)")
	}
	return lines
}

// runProjectUsersSetupStep is ProjectUsersFlag's setup words, on the server as the deploy
// user: facts prints the facts as JSON, apply applies them.
func runProjectUsersSetupStep(args []string) int {
	h := setupHost{root: os.Getenv(remote.HostRootEnv)}
	usage := "usage: devopsy " + remote.ProjectUsersFlag + " setup facts [<directory>] | setup apply [--said=<id>,...] [<directory>]"
	if len(args) == 0 {
		return fail(usage)
	}
	var said []string
	rest := args[1:]
	if args[0] == "apply" && len(rest) > 0 && strings.HasPrefix(rest[0], "--said") {
		a, err := parseApply(rest[:1])
		if err != nil {
			return failErr(err)
		}
		said, rest = a.said, rest[1:]
	}
	if len(rest) > 1 || len(rest) == 1 && !strings.HasPrefix(rest[0], "/") {
		return fail(usage)
	}
	dir := ""
	if len(rest) == 1 {
		dir = rest[0]
	}
	switch args[0] {
	case "facts":
		rd, err := readDoorSetup(h, dir)
		if err != nil {
			return failErr(err)
		}
		out, err := json.Marshal(rd.facts)
		if err != nil {
			return failErr(err)
		}
		fmt.Println(string(out))
		return 0
	case "apply":
		return applyDoorSetup(h, dir, said)
	}
	return fail(usage)
}

// doorSetupRead is what --project-users setup reads of a server: its facts and what an apply puts.
type doorSetupRead struct {
	facts setup.DoorFacts
	// dropin is the mounts drop-in's text devopsy wants, "" where refused, and dropinBare the
	// one without the space, as --server-setup wants it while the space is not mounted, which
	// the root run puts where its mount fails; unit the space's mount unit's path and text.
	dropin, dropinBare, unit, unitText string
	// space is the space as the config names it (cleaned), which the config gets.
	space string
}

// readDoorSetup reads what project users need of this server, dir the space the operator
// named ("" for none): each item's state, its refusals, what an apply puts. It changes nothing
// and takes no lock.
func readDoorSetup(h setupHost, dir string) (doorSetupRead, error) {
	var rd doorSetupRead
	f := &rd.facts
	r, err := remote.ReleaseSettings()
	if err != nil {
		return rd, err
	}
	d, err := remote.ServerDoor()
	if err != nil {
		return rd, err
	}
	f.ConfigFile = remote.UserConfigFile()
	refuse := func(item, stops, msg, fix string) {
		f.Refusals = append(f.Refusals, setup.Refusal{Item: item, Message: msg, Fix: fix, Stops: stops})
	}
	// The tools and Docker first: each refuses every step, with nothing applied.
	if missing := missingTools(doorTool); len(missing) > 0 {
		f.Refusals = append(f.Refusals, setup.Refusal{Item: toolsItem, Message: "no " + strings.Join(missing, " or ") + " in PATH, which project users need",
			Fix: doorToolsFixText, Howto: howto.ProjectUsers, Stops: setup.RefusalDoorTools})
	}
	docker := dockerFacts()
	f.Refusals = append(f.Refusals, dockerAnswers(docker)...)
	if docker.Error == "" {
		f.Refusals = append(f.Refusals, hostRefusals(h, docker, "--project-users setup mounts the door's space and /proc with")...)
	}

	// The space: the config's, else the one named, else devopsy's.
	space := filepath.Clean(cmp.Or(dir, setup.DefaultSpace))
	switch {
	case d != nil && d.Space != "":
		if dir != "" && filepath.Clean(dir) != filepath.Clean(d.Space) {
			refuse(setup.ItemSpace, setup.RefusalSpace, "the door's space is "+d.Space+" (door: space: in "+f.ConfigFile+"), not "+dir,
				"leave the directory out, which sets up "+d.Space+": a space is moved by hand")
		}
		space = filepath.Clean(d.Space)
	case d != nil:
		refuse(setup.ItemConfig, setup.RefusalSpace, f.ConfigFile+" has door: with no space: under it",
			"add \"space: "+space+"\" under its door:, indented, then run it again")
	default:
		f.Config = setup.DoorConfigLines(space)
		if have, err := os.ReadFile(f.ConfigFile); err == nil || errors.Is(err, os.ErrNotExist) {
			if _, err := setup.AppendDoorConfig(have, space); err != nil {
				refuse(setup.ItemConfig, setup.RefusalSpace, "devopsy cannot add the project users' directory to "+f.ConfigFile+": "+err.Error(),
					"add these two lines to its settings by hand, then run it again:\n"+strings.TrimSuffix(f.Config, "\n"))
			}
		}
	}
	rd.space = space
	f.Space = resolveRoot(space)
	for _, p := range slices.Compact([]string{space, f.Space}) {
		if err := setup.PlainPath(p); err != nil {
			refuse(setup.ItemSpace, setup.RefusalSpace, "the door's space "+err.Error(), "name a directory whose path has none")
		}
	}
	rd.unit, rd.unitText = setup.SpaceUnit(f.Space), setup.SpaceUnitText(f.Space)
	if why := swappable(space, os.Getuid()); why != "" {
		refuse(setup.ItemSpace, setup.RefusalSpace, why+": devopsy acts as root only on a path no other user can change",
			"name a directory whose every parent is root's or the deploy user's, writable by no one else, and none of them a link")
	} else {
		readSpace(h, &rd, refuse)
	}

	// Docker's wait at boot for the space, beside the release root's (mountsDropInWant): the
	// space where the config names it or this run will, mounted or mounted by this run.
	named := ""
	if d != nil && d.Space != "" || f.Config != "" && f.Goes(setup.StepConfig) {
		named = space
	}
	if text, refusal := mountsDropInWant(h, r.Root, named, f.Goes(setup.StepSpace)); refusal != nil {
		f.Refusals = append(f.Refusals, *refusal)
	} else {
		rd.dropin = text
		rd.dropinBare, _ = mountsDropInWant(h, r.Root, "", false)
		f.Files = append(f.Files, mountsDropInFact(h, text))
	}
	readProcesses(h, f, refuse)
	return rd, nil
}

// readSpace judges the door's space: a directory of the deploy user's, mode 0755, on a mount
// with nosuid and nodev, read from the host's mounts (whatever made it: an fstab line by hand
// is in sync, and nothing of devopsy's goes beside it). One that exists with another owner, is
// no directory, or is someone's mount without the two, refuses. One that holds files on no
// mount of its own is bound on itself as any other: a bind of a directory on itself hides
// nothing, so a space whose mount went (a boot where it failed) is mounted again.
func readSpace(h setupHost, rd *doorSetupRead, refuse func(item, stops, msg, fix string)) {
	f := &rd.facts
	sp := f.Space
	anew := "name another directory, or move this one aside"
	fi, err := os.Stat(sp)
	exists := err == nil
	switch {
	case errors.Is(err, os.ErrNotExist):
		f.SpaceAction = setup.RootMake
	case err != nil:
		refuse(setup.ItemSpace, setup.RefusalSpace, err.Error(), "")
		return
	case !fi.IsDir():
		refuse(setup.ItemSpace, setup.RefusalSpace, "the door's space "+sp+" is not a directory", anew)
		return
	}
	if exists {
		st, _ := fi.Sys().(*syscall.Stat_t)
		switch {
		case st == nil, int(st.Uid) == os.Getuid():
		case st.Uid == 0 && emptyDir(sp):
			f.SpaceAction = setup.RootGive
		case st.Uid == 0:
			refuse(setup.ItemSpace, setup.RefusalSpace, "the door's space "+sp+" is root's and holds files: only an empty one is given to the deploy user", anew)
		default:
			owner := strconv.Itoa(int(st.Uid))
			if u, err := user.LookupId(owner); err == nil {
				owner = u.Username
			}
			refuse(setup.ItemSpace, setup.RefusalSpace, "the door's space "+sp+" is "+owner+"'s", anew)
		}
		if fi.Mode().Perm() != 0o755 {
			f.SpaceMode = strconv.FormatUint(uint64(fi.Mode().Perm()), 8)
		}
	}
	sm := spaceMount(h, sp)
	f.Guarded = sm.guarded
	point := sm.found && sm.m.point == sp
	unitText, _ := os.ReadFile(h.path(rd.unit))
	ours := unitText != nil
	if ours || !f.Guarded {
		link, _ := os.Readlink(h.path(setup.SpaceUnitLink(sp)))
		f.Files = append(f.Files,
			setup.FileFact{Item: setup.ItemSpace, Path: rd.unit, InSync: string(unitText) == rd.unitText, Missing: !ours},
			setup.FileFact{Item: setup.ItemSpace, Path: setup.SpaceUnitLink(sp), Link: rd.unit, InSync: link == rd.unit})
	}
	switch {
	case f.Guarded:
	case point && !ours:
		refuse(setup.ItemSpace, setup.RefusalSpace, "the door's space "+sp+" is a mount without nosuid and nodev, not devopsy's: devopsy puts no mount over it",
			"mount it with nosuid and nodev where it is set (its /etc/fstab line, its unit)")
	}
}

// swappable is why another user could swap a part of path p (the door's space, absolute and
// clean) for a link between the facts and the root run, whose make, chown, chmod and mount
// would then act on wherever the link leads, "" when none can: a part that is a link, a parent
// owned by anyone but root or uid (the deploy user, root on the host through Docker already),
// or one others may write, unless it is sticky and the part in it is there, no link, and root's
// or uid's (a sticky directory lets no one else rename or remove it, /tmp's way). A part not
// there yet is made by the root run under the last one there, so the walk ends at it. The root
// run judges the same again right before it acts (remote.ProjectUsersSetupScript's safe_path).
func swappable(p string, uid int) string {
	trusted := func(fi os.FileInfo) string {
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid == 0 || int(st.Uid) == uid {
			return ""
		}
		owner := strconv.Itoa(int(st.Uid))
		if u, err := user.LookupId(owner); err == nil {
			owner = u.Username
		}
		return owner
	}
	cur := "/"
	for _, part := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		next := filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		switch {
		case err != nil:
			return p + " is under " + cur + ", which devopsy cannot read: " + err.Error()
		case fi.Mode()&os.ModeSymlink != 0:
			return p + " is under a symbolic link, " + cur
		case !fi.IsDir():
			return p + " is under " + cur + ", which is no directory"
		}
		if owner := trusted(fi); owner != "" {
			return p + " is under " + cur + ", which " + owner + " owns"
		}
		nfi, nerr := os.Lstat(next)
		if fi.Mode().Perm()&0o022 != 0 {
			if fi.Mode()&os.ModeSticky == 0 || nerr != nil || nfi.Mode()&os.ModeSymlink != 0 || trusted(nfi) != "" {
				return p + " is under " + cur + ", which other users can write"
			}
		}
		if nerr != nil {
			return ""
		}
		cur = next
	}
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return p + " is a symbolic link"
	}
	return ""
}

// spaceAt is the mount a space is on, as spaceMount reads it: whether there is one, and whether
// it has nosuid and nodev.
type spaceAt struct {
	m              mountEntry
	found, guarded bool
}

// spaceMount is the mount sp is on in the host's /proc/mounts (under h's root), read the same
// for --project-users setup's facts and --server-setup's drop-in (mountsDropInWant): whatever
// made it, an fstab line by hand included.
func spaceMount(h setupHost, sp string) spaceAt {
	mounts, _ := os.ReadFile(h.path(procMounts))
	m, found := mountOf(parseMounts(string(mounts)), sp)
	_, nosuid := m.opt("nosuid")
	_, nodev := m.opt("nodev")
	return spaceAt{m: m, found: found, guarded: found && nosuid && nodev}
}

// readProcesses judges /proc's hiding of every user's processes from the others: hidden
// another way (no unit of devopsy's: an fstab line) it is in sync when its gid= names a group
// the deploy user is in, else refused; devopsy's way wants the group devopsy-proc with the deploy
// user in it, its unit with the group's number, enabled, /proc remounted so, and polkit's
// drop-in where polkit is. Membership comes from the group file, never this process's groups,
// which count from its login.
func readProcesses(h setupHost, f *setup.DoorFacts, refuse func(item, stops, msg, fix string)) {
	p := &f.Processes
	mounts, _ := os.ReadFile(h.path(procMounts))
	p.HidePID = procHidePID(string(mounts))
	if m, ok := procMount(string(mounts)); ok {
		p.GID, _ = m.opt("gid")
	}
	unitText, _ := os.ReadFile(h.path(setup.ProcUnit))
	ours := unitText != nil
	me := "the deploy user"
	if u, err := user.Current(); err == nil {
		me = u.Username
	}
	if !ours && setup.Hiding(p.HidePID) {
		p.Other = true
		if p.GID == "" {
			refuse(setup.ItemProcesses, setup.RefusalProcesses, "/proc hides other users' processes (hidepid="+p.HidePID+") with no gid=, so the deploy user sees only its own",
				"add gid= a group the deploy user is in to /proc's options where they are set (its /etc/fstab line)")
			return
		}
		name, gid, member := groupOf(p.GID, me)
		p.Member = member
		if !member {
			fixes := "put the deploy user in the group whose number /proc's gid= is, as root"
			if name != "" && gid == p.GID {
				fixes = "put " + me + " in the group " + name + ", as root"
			}
			refuse(setup.ItemProcesses, setup.RefusalProcesses, "/proc hides other users' processes (hidepid="+p.HidePID+", gid="+p.GID+"), and the deploy user is not in that group", fixes)
		}
		return
	}
	if _, gid, member := groupOf(setup.ProcGroup, me); gid != "" {
		p.Group, p.Member = gid, member
	}
	p.Polkit = unitShow("polkit.service", "LoadState") == "loaded"
	link, _ := os.Readlink(h.path(setup.ProcUnitLink))
	f.Files = append(f.Files,
		setup.FileFact{Item: setup.ItemProcesses, Path: setup.ProcUnit, InSync: p.Group != "" && string(unitText) == setup.ProcUnitText(p.Group), Missing: !ours},
		setup.FileFact{Item: setup.ItemProcesses, Path: setup.ProcUnitLink, Link: setup.ProcUnit, InSync: link == setup.ProcUnit})
	if p.Polkit {
		have, err := os.ReadFile(h.path(setup.PolkitDropIn))
		f.Files = append(f.Files, setup.FileFact{Item: setup.ItemProcesses, Path: setup.PolkitDropIn, InSync: string(have) == setup.PolkitDropInText(), Missing: err != nil})
	}
}

// groupOf is the group key names (a name or a number) as the group file has it, through
// getent: its name, its number ("" when there is none), and whether who is in it, by its
// members or as its primary group.
func groupOf(key, who string) (name, gid string, member bool) {
	out, err := exec.Command("getent", "group", key).Output()
	if err != nil {
		return "", "", false
	}
	f := strings.Split(strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), ":")
	if len(f) < 4 {
		return "", "", false
	}
	return f[0], f[2], slices.Contains(strings.Split(f[3], ","), who) || f[2] == strconv.Itoa(os.Getgid())
}

// applyDoorSetup applies what project users need of this server, dir the space named: under
// the apply lock, the facts read again and their refusals said (but those said: their IDs),
// one root run for the space, the drop-in and the processes, then the config, then the checks
// again and what the operator should know.
func applyDoorSetup(h setupHost, dir string, said []string) int {
	failures := 0
	fail := func(msg string, details ...string) {
		failures++
		report.Error(msg, details...)
	}
	lock, err := applyLock(setup.RecordPath(remote.UserConfigFile()))
	if err != nil {
		return failErr(err)
	}
	defer lock.Close()
	rd, err := readDoorSetup(h, dir)
	if err != nil {
		return failErr(err)
	}
	f := rd.facts
	refused := len(f.Refused()) > 0
	sayRefusalsOf(f.Refusals, nil, "", report.ErrorLevel, said, false)
	end := func() int {
		if refused || failures > 0 {
			return 1
		}
		return 0
	}
	if f.NoneGoes() {
		report.Error(cmp.Or(refusedWords(f.Stopped()), "nothing changed"))
		return 1
	}
	does := func(s setup.Step) bool { return f.Goes(s) && f.Changes(s) }
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return ""
	}
	space, procs := does(setup.StepSpace), does(setup.StepProcesses)
	var unitText, dropin, procText, polkit string
	mount := false
	if space {
		for _, x := range f.Files {
			if x.Item == setup.ItemSpace && !x.InSync {
				mount = true
			}
		}
		mount = mount || !f.Guarded
		if mount {
			unitText = rd.unitText
		}
	}
	if does(setup.StepDoorDrop) {
		dropin = rd.dropin
	}
	if procs {
		procText = setup.ProcUnitText(setup.GIDMark)
		if f.Processes.Polkit {
			polkit = setup.PolkitDropInText()
		}
	}
	me, err := user.Current()
	if err != nil {
		return failErr(err)
	}
	var out bytes.Buffer
	if space || dropin != "" || procs {
		owner := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
		run := doorSetupRun(h.root, owner, f.Space, flag(space && f.SpaceAction != ""), flag(space && f.SpaceMode != ""), rd.unit, unitText, flag(mount),
			dropin, flag(procs), me.Username, procText, polkit, remote.QuoteWord(filepath.Base(rd.unit)), rd.dropinBare)
		err := run.run(&out, func(err error) error { return fmt.Errorf("the setup's run as root: %w", err) })
		sayDoorSetupLines(out.String(), me.Username)
		if err != nil {
			fail(err.Error())
		}
	}
	// Each step judged by its own result, read again: the config, the deploy user's own file,
	// last, once the space it names is made and mounted, whatever else of the run failed (the
	// processes).
	again, err := readDoorSetup(h, dir)
	if err != nil {
		fail(err.Error())
		return end()
	}
	if does(setup.StepConfig) {
		switch {
		case again.facts.Changes(setup.StepSpace):
			fail("door: space: not added to " + f.ConfigFile + ": the door's space is not set up")
		default:
			if err := appendDoorConfig(f.ConfigFile, rd.space); err != nil {
				fail("door: space: not added to " + f.ConfigFile + ": " + err.Error())
			} else {
				report.Info("named the door's space in "+f.ConfigFile, strings.Split(strings.TrimSuffix(f.Config, "\n"), "\n")...)
				if again, err = readDoorSetup(h, dir); err != nil {
					fail(err.Error())
					return end()
				}
			}
		}
	}

	// The checks again: what was applied must read in sync now, a refused step's left aside,
	// and the config's when it was not added (said).
	var left []string
	for _, s := range setup.DoorSteps {
		if does(s) && (s != setup.StepConfig || !again.facts.Changes(setup.StepSpace)) {
			left = append(left, doorItemLines(again.facts, s)...)
		}
	}
	if len(left) > 0 {
		fail("what was applied did not hold", left...)
	}
	if hasLine(out.String(), remote.SetupPut+" "+setup.MountsDropIn) {
		have, _ := os.ReadFile(h.path(setup.MountsDropIn))
		dropInWaits(string(have) != rd.dropinBare)
	}
	if hasLine(out.String(), remote.DoorSetupMember) {
		report.Info(me.Username+" is in "+setup.ProcGroup+" from its next login",
			"a session open now, a shared SSH connection's included, sees only its own processes until it is opened again")
	}
	if line := refusedWords(f.Stopped()); line != "" {
		report.Error(line)
	}
	if !refused && failures == 0 {
		report.Info("set up for project users: the first comes with --project-users add")
	}
	return end()
}

// appendDoorConfig appends door: space: to the deploy user's config at file as text
// (setup.AppendDoorConfig), by rename, the file's mode kept (0600 for one it makes).
func appendDoorConfig(file, space string) error {
	have, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	text, err := setup.AppendDoorConfig(have, space)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(file); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp := file + ".new"
	if err := os.WriteFile(tmp, text, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// sayDoorSetupLines says what the root run changed, line by line
// (remote.ProjectUsersSetupScript's), who the deploy user.
func sayDoorSetupLines(out, who string) {
	for _, l := range strings.Split(out, "\n") {
		word, rest, _ := strings.Cut(l, " ")
		switch word {
		case remote.SetupPut:
			report.Info("put " + rest)
		case remote.SetupMade:
			report.Info("made the door's space " + rest)
		case remote.SetupGave:
			report.Info("gave the door's space " + rest + " to " + who)
		case remote.DoorSetupMode:
			report.Info("set " + rest + " to mode 755")
		case remote.DoorSetupGroup:
			report.Info("made the group " + rest)
		case remote.DoorSetupMember:
			report.Info("put " + rest + " in " + setup.ProcGroup)
		case remote.DoorSetupMounted:
			report.Info("mounted " + rest + " on itself with nosuid and nodev")
		case remote.DoorSetupHidden:
			report.Info("hid every user's processes from the others, " + setup.ProcGroup + " seeing them all")
		case remote.DoorSetupPolkit:
			report.Info("restarted polkit, in " + setup.ProcGroup)
		}
	}
}
