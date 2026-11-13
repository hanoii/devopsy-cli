package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		usage := "usage: devopsy @" + targetRef(t) + " --project-users setup [<directory>] [--yes]"
		// A directory named as a relative path: said so, since it is a directory the usage takes.
		for _, a := range args {
			if a != "" && !strings.HasPrefix(a, "/") && !strings.HasPrefix(a, "-") {
				report.Error(usage, "the directory must be an absolute path, not "+a)
				return 1
			}
		}
		return fail(usage)
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
		report.Info("nothing to change: " + t.Host + " is ready for project users")
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
		report.Info("this adds to "+f.ConfigFile+":", strings.Split(strings.TrimSuffix(f.Config, "\n"), "\n")...)
	}
	if f.Changes(setup.StepProcesses) && f.Goes(setup.StepProcesses) {
		report.Info("once hidden, each user sees only its own processes; the deploy user and members of "+setup.ProcGroup+" see all",
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
	add(setup.ItemSpace, doorItemLines(f, setup.StepSpace, true), f.Space+", where devopsy keeps project users' data, is on a mount with nosuid and nodev")
	add(setup.ItemMounts, doorItemLines(f, setup.StepDoorDrop, true), "")
	same := "/proc hides each user's processes from the others; members of " + setup.ProcGroup + " see all"
	if p := f.Processes; p.Other {
		opts := "hidepid=" + p.HidePID + ", gid=" + p.GID
		if p.GID == "" {
			opts = "hidepid=" + p.HidePID + ", no gid="
		}
		same = "/proc hides each user's processes from the others (" + opts + "), set up outside devopsy and left as it is"
	}
	// An item refused is said by its refusal alone, never also as in sync.
	refused := func(item string) bool {
		return slices.ContainsFunc(f.Refusals, func(r setup.Refusal) bool { return r.Item == item })
	}
	if refused(setup.ItemProcesses) {
		same = ""
	}
	add(setup.ItemProcesses, doorItemLines(f, setup.StepProcesses, true), same)
	same = "door: space: " + f.Space
	if refused(setup.ItemConfig) {
		same = ""
	}
	add(setup.ItemConfig, doorItemLines(f, setup.StepConfig, true), same)
	return es
}

// doorItemLines are what differs of step s's item in f, none when in sync: what the first
// session shows (plan, each with what devopsy does about it while the step goes) and what an
// apply that did not hold names (only what differs).
func doorItemLines(f setup.DoorFacts, s setup.Step, plan bool) []string {
	if !f.Changes(s) {
		return nil
	}
	does := plan && f.Goes(s)
	var lines []string
	say := func(what, act string) {
		if does && act != "" {
			what += ": " + act
		}
		lines = append(lines, what)
	}
	// A unit's link is said only beside its file in sync: a unit missing is enabled with it.
	files := func(item string) {
		for _, x := range f.Files {
			switch {
			case x.Item != item || x.InSync:
			case x.Link != "":
				if !slices.ContainsFunc(f.Files, func(u setup.FileFact) bool { return u.Path == x.Link && !u.InSync }) {
					say(filepath.Base(x.Link)+" is not enabled, so no boot starts it", "devopsy enables it")
				}
			case x.Item == setup.ItemMounts && f.DropInWithout:
				say("Docker does not wait for the project users' directory at boot yet", "devopsy adds it to "+x.Path)
			case x.Missing:
				say(x.Path+" does not exist yet", "")
			default:
				say(x.Path+" differs from what devopsy writes", "")
			}
		}
	}
	switch s {
	case setup.StepSpace:
		// The first line names what the directory is for.
		name := f.Space + ", where devopsy keeps project users' data,"
		named := func() string {
			n := name
			name = f.Space
			return n
		}
		switch f.SpaceAction {
		case setup.RootMake:
			say(named()+" does not exist", "devopsy makes it, owned by the deploy user, mode 755")
		case setup.RootGive:
			say(named()+" is root's", "devopsy gives it to the deploy user")
		}
		if f.SpaceMode != "" {
			say(named()+" is mode "+f.SpaceMode, "devopsy sets it to 755")
		}
		if !f.Guarded {
			say(named()+" has no mount with nosuid and nodev", "devopsy bind-mounts it on itself with both, now and at every boot")
		}
		files(setup.ItemSpace)
	case setup.StepDoorDrop:
		files(setup.ItemMounts)
	case setup.StepProcesses:
		p := f.Processes
		switch {
		case p.Hides():
		case !setup.Hiding(p.HidePID):
			say("/proc shows each user's processes to every other user", "devopsy hides them, now and at every boot")
		case p.GID == "":
			say("/proc hides processes, and no group sees them all", "devopsy remounts it so members of "+setup.ProcGroup+" do")
		default:
			say("/proc hides processes, and group "+p.GID+" sees them all, not "+setup.ProcGroup, "devopsy remounts it so members of "+setup.ProcGroup+" do")
		}
		if p.Group == "" {
			say("the group "+setup.ProcGroup+" does not exist", "devopsy makes it, the group that sees every process, with the deploy user in it")
		}
		if !p.Member && p.Group != "" {
			say("the deploy user is not in "+setup.ProcGroup, "devopsy adds it")
		}
		files(setup.ItemProcesses)
	case setup.StepConfig:
		say(f.ConfigFile+" does not name the project users' directory", "devopsy adds the two lines below")
	}
	return lines
}

// runProjectUsersSetupStep is ProjectUsersFlag's setup words, on the server as the deploy
// user: facts prints the facts as JSON, apply applies them, ref the server as its address named
// it, "" without, for the command its last line offers.
func runProjectUsersSetupStep(args []string, ref string) int {
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
		rd, err := readDoorSetup(h, dir, ref)
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
		return applyDoorSetup(h, dir, said, ref)
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
// named ("" for none), ref the server as its address named it (for a fix's command): each
// item's state, its refusals, what an apply puts. It changes nothing and takes no lock.
func readDoorSetup(h setupHost, dir, ref string) (doorSetupRead, error) {
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
	// The tools, Docker and the release root first: each refuses every step, with nothing
	// applied. The release root is judged as --project-users add and a release judge it.
	if missing := missingTools(doorTool); len(missing) > 0 {
		f.Refusals = append(f.Refusals, setup.Refusal{Item: toolsItem, Message: "no " + strings.Join(missing, " or ") + " in PATH, which project users need",
			Fix: doorToolsFixText, Howto: howto.ProjectUsers, Stops: setup.RefusalDoorTools})
	}
	docker := dockerFacts()
	f.Refusals = append(f.Refusals, dockerAnswers(docker)...)
	if docker.Error == "" {
		f.Refusals = append(f.Refusals, hostRefusals(h, docker, "--project-users setup mounts the project users' directory and /proc with")...)
	}
	if refusal := releaseRootRefusal(r.Root, ref); refusal != nil {
		f.Refusals = append(f.Refusals, *refusal)
	}

	// The space: the config's, else the one named, else devopsy's.
	space := filepath.Clean(cmp.Or(dir, setup.DefaultSpace))
	switch {
	case d != nil && d.Space != "":
		if dir != "" && filepath.Clean(dir) != filepath.Clean(d.Space) {
			refuse(setup.ItemSpace, setup.RefusalSpace, f.ConfigFile+" names "+d.Space+" as the project users' directory (door: space:), not "+dir,
				"leave the directory out to set up "+d.Space+", since devopsy never moves it")
		}
		space = filepath.Clean(d.Space)
	case d != nil:
		refuse(setup.ItemConfig, setup.RefusalSpace, f.ConfigFile+" has door: but no space: under it",
			"add \"space: "+space+"\" under its door:, indented, then run it again")
	default:
		f.Config = setup.DoorConfigLines(space)
		if have, err := os.ReadFile(f.ConfigFile); err == nil || errors.Is(err, os.ErrNotExist) {
			if _, err := setup.AppendDoorConfig(have, space); err != nil {
				refuse(setup.ItemConfig, setup.RefusalSpace, "devopsy cannot add the project users' directory to "+f.ConfigFile+", since "+err.Error(),
					"add these two lines to its settings by hand, then run it again:\n"+strings.TrimSuffix(f.Config, "\n"))
			}
		}
	}
	// The space as named: a link in its path refuses (swappable), so its one form is itself.
	rd.space, f.Space = space, space
	// A refusal's fix by where the directory came from: one named is fixed by naming another,
	// one the config names by changing it, or moving it by hand and naming the new path there,
	// since another directory named is refused while the config names this one.
	fixFor := func(name, change, to string) string {
		if d == nil || d.Space == "" {
			return name
		}
		moved := "move it by hand to " + to + " and set door: space: in " + f.ConfigFile + " to that path"
		if change == "" {
			return moved
		}
		return change + ", or " + moved
	}
	if err := setup.PlainPath(space); err != nil {
		refuse(setup.ItemSpace, setup.RefusalSpace, err.Error(), fixFor("name a directory with only letters, digits and . _ - /", "", "a path with only letters, digits and . _ - /,"))
	}
	rd.unit, rd.unitText = setup.SpaceUnit(f.Space), setup.SpaceUnitText(f.Space)
	// The mount the space is on, read before any refusal of it: a refused space is said as it is.
	sm := spaceMount(h, space)
	f.Guarded = sm.guarded
	if why := swappable(space, os.Getuid()); why != "" {
		refuse(setup.ItemSpace, setup.RefusalSpace, why+", and devopsy acts as root only on a path no other user can change",
			fixFor("name a directory whose every parent is root's or the deploy user's, writable by no one else, and none of them a link",
				"make every part of its path a directory, none of them a link, each above it root's or the deploy user's and writable by no one else", "such a path"))
	} else {
		readSpace(h, &rd, sm, refuse, fixFor("name another directory, or move this one aside", "make it a directory of the deploy user's", "a directory of the deploy user's"))
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
		have, _ := os.ReadFile(h.path(setup.MountsDropIn))
		f.DropInWithout = text != rd.dropinBare && string(have) == rd.dropinBare
	}
	readProcesses(h, f)
	return rd, nil
}

// readSpace judges the door's space, sm the mount it is on (spaceMount), anew the fix of a
// directory refused: a directory of the deploy user's, mode 0755, on a mount
// with nosuid and nodev, read from the host's mounts (whatever made it: an fstab line by hand
// is in sync, and nothing of devopsy's goes beside it). One that exists with another owner, is
// no directory, or is someone's mount without the two, refuses. One that holds files on no
// mount of its own is bound on itself as any other: a bind of a directory on itself hides
// nothing, so a space whose mount went (a boot where it failed) is mounted again.
func readSpace(h setupHost, rd *doorSetupRead, sm spaceAt, refuse func(item, stops, msg, fix string), anew string) {
	f := &rd.facts
	sp := f.Space
	action, refused, fi := dirOwner("", sp, os.Getuid())
	if refused != "" {
		refuse(setup.ItemSpace, setup.RefusalSpace, refused, anew)
		return
	}
	f.SpaceAction = action
	if fi != nil && fi.Mode().Perm() != 0o755 {
		f.SpaceMode = strconv.FormatUint(uint64(fi.Mode().Perm()), 8)
	}
	point := sm.found && sm.m.point == sp
	f.SpaceMounted = point
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
		refuse(setup.ItemSpace, setup.RefusalSpace, sp+" is a mount of the server's own without nosuid and nodev, and devopsy puts no mount over another's",
			"add nosuid and nodev to its mount where it is set (its line in /etc/fstab, or its unit)")
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
// made it, an fstab line by hand included, judged by guards as the door's statfs is.
func spaceMount(h setupHost, sp string) spaceAt {
	mounts, _ := os.ReadFile(h.path(procMounts))
	m, found := mountOf(parseMounts(string(mounts)), sp)
	_, nosuid := m.opt("nosuid")
	_, nodev := m.opt("nodev")
	return spaceAt{m: m, found: found, guarded: found && guards(nosuid, nodev)}
}

// readProcesses judges /proc's hiding of every user's processes from the others: hidden
// another way (no unit of devopsy's: an fstab line) it is judged by procHiding alone, as
// --server-info and --project-users add judge it; devopsy's way wants the group devopsy-proc
// with the deploy user in it, its unit with the group's number, enabled, /proc remounted so,
// and polkit's drop-in where polkit is. Membership comes from the group file, never this
// process's groups, which count from its login.
func readProcesses(h setupHost, f *setup.DoorFacts) {
	var refused *setup.Refusal
	f.Processes, refused = procHidingAt(h, groupOf)
	p := &f.Processes
	if p.Other {
		if refused != nil {
			f.Refusals = append(f.Refusals, *refused)
		}
		return
	}
	unitText, _ := os.ReadFile(h.path(setup.ProcUnit))
	if _, gid, member := groupOf(setup.ProcGroup, deployUserName()); gid != "" {
		p.Group, p.Member = gid, member
	}
	p.Polkit = unitShow("polkit.service", "LoadState") == "loaded"
	link, _ := os.Readlink(h.path(setup.ProcUnitLink))
	f.Files = append(f.Files,
		setup.FileFact{Item: setup.ItemProcesses, Path: setup.ProcUnit, InSync: p.Group != "" && string(unitText) == setup.ProcUnitText(p.Group), Missing: unitText == nil},
		setup.FileFact{Item: setup.ItemProcesses, Path: setup.ProcUnitLink, Link: setup.ProcUnit, InSync: link == setup.ProcUnit})
	if p.Polkit {
		have, err := os.ReadFile(h.path(setup.PolkitDropIn))
		f.Files = append(f.Files, setup.FileFact{Item: setup.ItemProcesses, Path: setup.PolkitDropIn, InSync: string(have) == setup.PolkitDropInText(), Missing: err != nil})
	}
}

// groupLookup is groupOf's shape, a parameter where a test gives its own groups.
type groupLookup func(key, who string) (name, gid string, member bool)

// procHidingAt is procHiding of the host at h: its mounts (procMounts), whether devopsy's unit
// is there, the deploy user.
func procHidingAt(h setupHost, groups groupLookup) (setup.ProcFact, *setup.Refusal) {
	mounts, _ := os.ReadFile(h.path(procMounts))
	_, err := os.Stat(h.path(setup.ProcUnit))
	return procHiding(string(mounts), err == nil, deployUserName(), groups)
}

// procHiding is the one judgment of /proc's hiding of other users' processes, for
// --project-users setup, --server-info and --project-users add: p as the mounts (a /proc/mounts
// text) say it, and, where it hides them another way (ours false: no unit of devopsy's, which
// the setup judges by its files), the refusal of one the deploy user (me, its groups looked up
// by groups) does not see every process through: no gid=, or a gid= of a group it is not in.
func procHiding(mounts string, ours bool, me string, groups groupLookup) (p setup.ProcFact, refused *setup.Refusal) {
	p.HidePID = procHidePID(mounts)
	if m, ok := procMount(mounts); ok {
		p.GID, _ = m.opt("gid")
	}
	if ours || !setup.Hiding(p.HidePID) {
		return p, nil
	}
	p.Other = true
	refuse := func(msg, fix string) *setup.Refusal {
		return &setup.Refusal{Item: setup.ItemProcesses, Stops: setup.RefusalProcesses, Message: msg, Fix: fix}
	}
	if p.GID == "" {
		return p, refuse("/proc hides other users' processes (hidepid="+p.HidePID+") with no gid=, so the deploy user sees only its own",
			"add gid= with a group the deploy user is in to /proc's options where they are set (its line in /etc/fstab)")
	}
	name, gid, member := groups(p.GID, me)
	if p.Member = member; member {
		return p, nil
	}
	fix := "put the deploy user in the group whose number /proc's gid= is, as root"
	if name != "" && gid == p.GID {
		fix = "put " + me + " in the group " + name + ", as root"
	}
	return p, refuse("/proc hides other users' processes (hidepid="+p.HidePID+", gid="+p.GID+"), and the deploy user is not in that group", fix)
}

// deployUserName is this user's name, the deploy user's on a server, "the deploy user" where it
// cannot be read.
func deployUserName() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "the deploy user"
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
func applyDoorSetup(h setupHost, dir string, said []string, ref string) int {
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
	rd, err := readDoorSetup(h, dir, ref)
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
	mount := space && f.Mounts()
	if mount {
		unitText = rd.unitText
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
	var runErr error
	if space || dropin != "" || procs {
		owner := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
		run := doorSetupRun(h.root, owner, f.Space, flag(space && f.SpaceAction != ""), flag(space && f.SpaceMode != ""), rd.unit, unitText, flag(mount),
			dropin, flag(procs), me.Username, procText, polkit, remote.QuoteWord(filepath.Base(rd.unit)), rd.dropinBare)
		// Each line said as the run prints it, in order with what the run says itself.
		runErr = run.run(io.MultiWriter(&out, &lineWriter{line: func(l string) { sayDoorSetupLines(l, me.Username, f) }}), func(err error) error { return err })
		var ee *exec.ExitError
		switch {
		case errors.As(runErr, &ee) && ee.ExitCode() == remote.DoorSetupSaid:
			// The run's own failure, each said by the run.
			failures++
		case runErr != nil:
			fail("devopsy could not run its setup as root: " + runErr.Error())
		}
	}
	// What the run did of each step, by its lines: only that is judged again, and a failure the
	// run said is said once.
	did := map[setup.Step]bool{
		setup.StepSpace: rootRunDid(space, runErr, out.String(), remote.DoorSetupSpaceFailed, remote.SetupMade+" "+f.Space, remote.SetupGave+" "+f.Space,
			remote.DoorSetupMode+" "+f.Space, remote.SetupPut+" "+rd.unit, remote.DoorSetupMounted+" "+f.Space),
		setup.StepDoorDrop: rootRunDid(dropin != "", runErr, out.String(), "", remote.SetupPut+" "+setup.MountsDropIn),
		setup.StepProcesses: rootRunDid(procs, runErr, out.String(), remote.DoorSetupProcsFailed, remote.DoorSetupGroup, remote.DoorSetupMember,
			remote.SetupPut+" "+setup.ProcUnit, remote.SetupPut+" "+setup.PolkitDropIn, remote.DoorSetupHidden),
	}
	// Each step judged by its own result, read again: the config, the deploy user's own file,
	// last, once the space it names is made and mounted, whatever else of the run failed (the
	// processes). One the space keeps waiting is said in the run's last words.
	again, err := readDoorSetup(h, dir, ref)
	if err != nil {
		fail(err.Error())
		return end()
	}
	var waits []string
	if does(setup.StepConfig) {
		switch {
		case again.facts.Changes(setup.StepSpace):
			failures++
			waits = append(waits, "door: space: is not added to "+f.ConfigFile+" while the project users' directory is not set up")
		default:
			if err := appendDoorConfig(f.ConfigFile, rd.space); err != nil {
				fail("devopsy could not add the project users' directory to " + f.ConfigFile + ": " + err.Error())
			} else {
				did[setup.StepConfig] = true
				report.Info("added the project users' directory to "+f.ConfigFile, strings.Split(strings.TrimSuffix(f.Config, "\n"), "\n")...)
				if again, err = readDoorSetup(h, dir, ref); err != nil {
					fail(err.Error())
					return end()
				}
			}
		}
	}

	// The checks again: what was applied must read in sync now, what the run did not do or said
	// failed, a refused step's and a config not added (said) left aside.
	var left []string
	for _, s := range setup.DoorSteps {
		switch {
		case !did[s]:
		// A drop-in that lacks only the space is the space's own failure, said by its lines.
		case s == setup.StepDoorDrop && again.facts.DropInWithout:
		default:
			left = append(left, doorItemLines(again.facts, s, false)...)
		}
	}
	if len(left) > 0 {
		fail("what was applied did not hold", left...)
	}
	if hasLine(out.String(), remote.SetupPut+" "+setup.MountsDropIn) {
		have, _ := os.ReadFile(h.path(setup.MountsDropIn))
		dropInWaits(string(have) != rd.dropinBare)
	}
	// Its next login matters only once /proc hides processes: until then every user sees them.
	if hasLine(out.String(), remote.DoorSetupMember) && again.facts.Processes.Hides() {
		report.Info(me.Username+" sees every process from its next login",
			"a session open now, a shared SSH connection's included, sees only its own processes until it is opened again")
	}
	// An apply that fails ends on an error, as its exit goes (end), what it left waiting said
	// with it.
	switch line := refusedWords(f.Stopped()); {
	case line != "":
		report.Error(line, waits...)
	case end() != 0:
		report.Error("this server is not ready for project users", waits...)
	}
	if !refused && failures == 0 {
		// The first project user's add, offered only while there is none.
		var hints []report.Remedy
		if d, err := remote.ServerDoor(); err == nil && (d == nil || len(d.Users) == 0) {
			hints = append(hints, offer(report.Hint, "add a project's user:", addCommand(ref), "add a project's user with --project-users add for this server"))
		}
		report.Say(report.InfoLevel, "this server is ready for project users", nil, hints...)
	}
	return end()
}

// appendDoorConfig appends door: space: to the deploy user's config at file as text
// (setup.AppendDoorConfig), whole (setup.WriteFile: by rename, a link written at the file it
// names), the file's mode kept (0600 for one it makes).
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
	return setup.WriteFile(file, text, mode)
}

// sayDoorSetupLines says what the root run changed, line by line
// (remote.ProjectUsersSetupScript's), who the deploy user, f the facts the run went by.
func sayDoorSetupLines(out, who string, f setup.DoorFacts) {
	for _, l := range strings.Split(out, "\n") {
		word, rest, _ := strings.Cut(l, " ")
		switch word {
		case remote.SetupPut:
			report.Info("put " + rest)
		case remote.SetupMade:
			report.Info("made the project users' directory " + rest)
		case remote.SetupGave:
			report.Info("gave the project users' directory " + rest + " to " + who)
		case remote.DoorSetupMode:
			report.Info("set " + rest + " to mode 755")
		case remote.DoorSetupGroup:
			report.Info("made the group " + rest)
		case remote.DoorSetupMember:
			report.Info("added " + rest + " to " + setup.ProcGroup)
		case remote.DoorSetupMounted:
			// A unit enabled and started over its mount there already (its text or its link
			// put) mounts nothing now.
			if f.SpaceMounted {
				report.Info("every boot now bind-mounts " + rest + " on itself with nosuid and nodev; its mount there now is left as it is")
			} else {
				report.Info("bind-mounted " + rest + " on itself with nosuid and nodev")
			}
		case remote.DoorSetupHidden:
			report.Info("hid each user's processes from the others; members of " + setup.ProcGroup + " see all")
		case remote.DoorSetupPolkit:
			report.Info("restarted polkit so it sees every process")
		}
	}
}

// addCommand is --project-users add for the server as ref names it, "" without one: offered as
// is, the project's name to type in its place.
func addCommand(ref string) string {
	if ref == "" {
		return ""
	}
	return "devopsy @" + ref + " --project-users add <project>"
}

// lineWriter hands line each whole line written to it, without its newline, as it comes.
type lineWriter struct {
	line func(string)
	buf  []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.line(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
}
