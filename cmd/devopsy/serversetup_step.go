package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/howto"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
	"github.com/hanoii/devopsy-cli/internal/update"
)

// restartWait bounds the wait for Docker's restart to be confirmed, unless
// remote.RestartWaitEnv says otherwise.
const restartWait = 2 * time.Minute

// setupHost is the host as the server's setup reads and writes it: the system's files (/etc,
// /proc, /run, /sys) under root (remote.HostRootEnv, "" for the host's own), for tests; the
// release root is a path of its own. restartWait bounds the wait for Docker's restart.
type setupHost struct {
	root        string
	restartWait time.Duration
}

func (h setupHost) path(p string) string { return h.root + p }

// setupRead is a server's setup as read: its facts and what an apply acts on.
type setupRead struct {
	facts setup.Facts
	// docker: the Docker devopsy reaches is this host's (dockerHere), so a root run reads
	// this host.
	docker bool
	daemon setup.Daemon
	record setup.Record
	// exists: the record is there.
	exists     bool
	states     []setup.PartState
	invocation string
	// flags are the arguments docker.service starts dockerd with, but --config-file, and
	// flagKeys the daemon.json keys of devopsy's parts they set.
	flags, flagKeys []string
	// door: the server has a door, whose firewall the loader loads too.
	door bool
	// fileVisible is what Docker shows once it takes the file; running what it shows now.
	fileVisible, running setup.Visible
	// floorMet: Docker answers, with both plugins, at its floor (dockerAnswers none), what
	// the record's note says for a release (floorNote).
	floorMet bool
	// dropin is the mounts drop-in's text devopsy wants (mountsDropInWant), "" where refused,
	// and space the door's space it names, "" for none (or none mounted).
	dropin, space string
}

// mountsDropInWant is the mounts drop-in's text, from the release root and the door's space
// (space "" for none), each in its one form (resolveRoot): the one function --server-setup and
// --project-users setup want it by, so the two never fight. The space is in it only where it
// is mounted with nosuid and nodev (spaceMount, on h's mounts) or the run mounts it (mounting,
// --project-users setup's), since a RequiresMountsFor= of a mount that fails keeps Docker from
// starting at the next boot, or where devopsy's drop-in names it already: a mount lost is the
// space's to say and put back, and a want without it would take it out of what Docker waits for: --server-setup passes the config's space, --project-users setup
// the one the config names or the run names in it. A path a unit file cannot hold is a refusal
// of the drop-in alone.
func mountsDropInWant(h setupHost, root, space string, mounting bool) (string, *setup.Refusal) {
	paths := []string{resolveRoot(root)}
	if space != "" {
		sp := resolveRoot(space)
		with, err := setup.MountsDropInText(paths[0], sp)
		have, _ := os.ReadFile(h.path(setup.MountsDropIn))
		if mounting || spaceMount(h, sp).guarded || err == nil && string(have) == with {
			paths = append(paths, sp)
		}
	}
	text, err := setup.MountsDropInText(paths...)
	if err == nil {
		return text, nil
	}
	if _, rootErr := setup.MountsDropInText(paths[0]); rootErr != nil {
		return "", &setup.Refusal{Item: setup.RefusalRoot, Message: "releases: root " + rootErr.Error(), Fix: "set releases: root in " + remote.UserConfigFile() + " to a path with none", Stops: setup.RefusalMountsPath}
	}
	return "", &setup.Refusal{Item: setup.ItemSpace, Message: "door: space: " + err.Error(), Fix: "set door: space: in " + remote.UserConfigFile() + " to a path with none", Stops: setup.RefusalMountsPath}
}

// mountsDropInFact is the mounts drop-in against text, devopsy's want.
func mountsDropInFact(h setupHost, text string) setup.FileFact {
	have, err := os.ReadFile(h.path(setup.MountsDropIn))
	return setup.FileFact{Item: setup.ItemMounts, Path: setup.MountsDropIn, InSync: string(have) == text, Missing: err != nil}
}

// dropInWaits says what a mounts drop-in put changes (space: it names the door's space too),
// and that it asks nothing.
func dropInWaits(space bool) {
	what := "the release root's filesystem"
	if space {
		what = "the release root's filesystem and the project users' directory"
	}
	report.Info("Docker now waits for "+what+" before it starts", "this applies from Docker's next start and only matters at a boot: no restart is needed")
}

// floorNote is the record's note of the devopsy that found Docker at its floor (setup.Record's
// Floor), as an apply of this devopsy writes it: its version where it found the floor met,
// none where it did not, and none from a development build, whose version names no one build
// ("dev", or a checkout's git describe). A release of that version then asks Docker nothing
// of the floor (serverReady).
func floorNote(met bool) string {
	if met && update.IsRelease(version) {
		return version
	}
	return ""
}

// runServerSetupStep is remote.ServerSetupFlag on a server, as the deploy user: facts prints
// the setup's facts as JSON, apply applies them.
func runServerSetupStep(args []string) int {
	h := setupHost{root: os.Getenv(remote.HostRootEnv), restartWait: restartWait}
	if n, err := strconv.Atoi(os.Getenv(remote.RestartWaitEnv)); err == nil && n > 0 {
		h.restartWait = time.Duration(n) * time.Second
	}
	if len(args) == 1 && args[0] == "facts" {
		rd, err := readSetup(h, readHostDocker(h))
		if err != nil {
			return failErr(err)
		}
		readChains(rd.facts.Firewall, rd.docker, false)
		out, err := json.Marshal(rd.facts)
		if err != nil {
			return failErr(err)
		}
		fmt.Println(string(out))
		return 0
	}
	if len(args) > 0 && args[0] == "apply" {
		a, err := parseApply(args[1:])
		if err != nil {
			return failErr(err)
		}
		return applySetup(h, a)
	}
	return fail("usage: devopsy " + remote.ServerSetupFlag + " facts | apply [--keep <part>=<sha256>]... [--replace <part>=<sha256>]... [--restart | --restart-idle] [--upgrade] [--said=<id>,...]")
}

// setupApply is what an apply was asked: the answers by part, a restart now (idle: only
// with no container running), upgrade mode, and said, the refusals said already (by
// --server-setup's first session, by their IDs: a refusal is said once per run). A restart asked is a
// condition the apply judges under its locks, never a decision taken from the first
// session's facts.
type setupApply struct {
	answers map[string]setup.Answer
	restart bool
	idle    bool
	upgrade bool
	// said are the IDs of the refusals the first session said (--said=<id>,...), nil when
	// none was (--upgrade, an apply run alone).
	said []string
}

func parseApply(args []string) (setupApply, error) {
	a := setupApply{answers: map[string]setup.Answer{}}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--restart":
			a.restart = true
		case "--restart-idle":
			a.restart, a.idle = true, true
		case "--upgrade":
			a.upgrade = true
		case "--said":
			a.said = []string{}
		case "--keep", "--replace":
			if i+1 >= len(args) {
				return a, errors.New(args[i] + " needs <part>=<sha256>")
			}
			part, sum, ok := strings.Cut(args[i+1], "=")
			if _, known := setup.PartNamed(part); !ok || !known || sum == "" {
				return a, errors.New(args[i] + " takes a part of daemon.json devopsy wants and its value's sha256, not " + args[i+1])
			}
			a.answers[part] = setup.Answer{Replace: args[i] == "--replace", Digest: sum}
			i++
		default:
			if ids, ok := strings.CutPrefix(args[i], "--said="); ok {
				a.said = []string{}
				for id := range strings.SplitSeq(ids, ",") {
					if _, err := hex.DecodeString(id); err != nil || id == "" && ids != "" {
						return a, errors.New("--said takes refusals' IDs, not " + ids)
					}
					if id != "" {
						a.said = append(a.said, id)
					}
				}
				continue
			}
			return a, errors.New("apply takes no " + args[i])
		}
	}
	if a.upgrade && (a.restart || len(a.answers) > 0) {
		return a, errors.New("apply --upgrade takes no answer and no restart")
	}
	return a, nil
}

// readSetup reads the server's setup from what was read of its Docker (hd): the required
// items, each tunable's state, pending, and what the operator words. It changes nothing and
// takes no lock.
func readSetup(h setupHost, hd hostDocker) (setupRead, error) {
	var rd setupRead
	r, err := remote.ReleaseSettings()
	if err != nil {
		return rd, err
	}
	f := &rd.facts
	f.Root = resolveRoot(r.Root)
	if r := toolsRefusal(); r != nil {
		f.Refusals = append(f.Refusals, *r)
	}
	action, refusal := releaseRootCheck(f.Root, os.Getuid())
	f.RootAction = action
	if refusal != nil {
		f.Refusals = append(f.Refusals, *refusal)
	}
	info, refusals := setupDocker(h, hd.facts)
	f.Refusals = append(f.Refusals, refusals...)
	rd.docker = dockerHere(hd.facts)
	f.Running, f.LiveRestore, rd.running = info.running, hd.facts.Running.LiveRestore, hd.facts.Running
	rd.invocation, rd.flags, rd.flagKeys = info.invocation, info.flags, info.flagKeys

	recordPath := setup.RecordPath(remote.UserConfigFile())
	rd.record, rd.exists, err = setup.ReadRecord(recordPath)
	if err != nil {
		f.Refusals = append(f.Refusals, setup.Refusal{Item: setup.RefusalRecord, Message: err.Error(), Fix: "remove it: devopsy writes it again at the next --server-setup"})
	}
	if rd.exists {
		f.Record = recordPath
	}
	rd.floorMet = len(dockerAnswers(hd.facts)) == 0
	f.FloorNote = rd.exists && rd.record.Floor != floorNote(rd.floorMet)
	if d, visible, why := judgeDaemon(hd.daemon, hd.daemonErr); why != "" {
		// A file Docker would refuse is judged no further: none of its parts is put.
		f.DaemonError = why
	} else {
		rd.daemon, rd.fileVisible = d, visible
		rd.states = setup.Judge(rd.daemon, rd.record, rd.flagKeys)
		for _, s := range rd.states {
			f.Parts = append(f.Parts, setup.FactOf(s))
		}
	}

	have, _ := os.ReadFile(h.path(setup.SysctlFile))
	f.Files = append(f.Files, setup.FileFact{Item: setup.ItemBuffers, Path: setup.SysctlFile, InSync: string(have) == setup.SysctlText()})
	for _, k := range setup.BufferKeys {
		live, _ := os.ReadFile(h.path(setup.BufferPath(k)))
		n, _ := strconv.ParseInt(strings.TrimSpace(string(live)), 10, 64)
		ok, err := setup.BufferAtLeast(string(live))
		f.Buffers = append(f.Buffers, setup.BufferFact{Key: k, Live: n, Low: err != nil || !ok})
	}
	d, err := remote.ServerDoor()
	if err != nil {
		return rd, err
	}
	space := ""
	if d != nil && d.Space != "" && spaceMount(h, resolveRoot(d.Space)).guarded {
		space = d.Space
	}
	rd.space = space
	if text, refusal := mountsDropInWant(h, f.Root, space, false); refusal != nil {
		f.Refusals = append(f.Refusals, *refusal)
	} else {
		rd.dropin = text
		f.Files = append(f.Files, mountsDropInFact(h, text))
	}

	mountinfo, _ := os.ReadFile(h.path("/proc/self/mountinfo"))
	for _, c := range setup.Candidates(string(mountinfo), h.path("/sys"), hd.facts.RootDir) {
		if _, size, free, _, err := statDisk(c.MountPoint); err == nil {
			c.Size, c.Free = size, free
		}
		f.Candidates = append(f.Candidates, c)
	}
	// A root it cannot read in full is taken for one with projects: no hint to move it.
	bases, err := deployEnvBases(f.Root)
	f.Projects = len(bases) > 0 || err != nil
	rd.door = d != nil
	// The door's tools only where the server has project users: without one they matter only
	// to --project-users add, which refuses without them.
	projectUsers := d != nil && len(d.Users) > 0
	f.Missing = missingTools(func(t serverTool) bool { return !t.required && (!t.door || projectUsers) })
	allow := r.Firewall.Metadata == remote.MetadataAllow
	f.Firewall = firewallFact(h, allow, rd.door)
	if f.Firewall.State == setup.FirewallRefused {
		msg := "firewall: metadata: allow, on a server with door: in its config: its project users' containers never reach a metadata service"
		if !rd.door {
			msg = "firewall: metadata: allow, on a server where the door's firewall is loaded: devopsy never takes a door's firewall off a server"
		}
		f.Refusals = append(f.Refusals, setup.Refusal{Item: firewallItem, Message: msg, Fix: "take firewall: metadata: allow out of " + remote.UserConfigFile()})
	}
	// Pending, by what a restart needs: Docker this host's, its restart's step going (a
	// refusal that stops nothing, Docker's plugins, hides nothing).
	marker, _ := os.ReadFile(h.path(setup.RestartMarker))
	outside := setup.OutsideParts(rd.states)
	f.Pending = rd.docker && f.Goes(setup.StepRestart) && setup.Pending(string(marker), rd.invocation, rd.fileVisible.Except(outside), rd.running.Except(outside))
	if f.Pending {
		f.PendingParts = rd.fileVisible.Except(outside).Differ(rd.running.Except(outside))
	}
	_, err = os.Stat(h.path(remote.RebootRequired))
	f.Reboot = err == nil
	return rd, nil
}

// dockerHere says whether the Docker devopsy reaches answers and is this host's (not
// rootless, not another host's through DOCKER_HOST): a root container through any other reads
// another machine, so no chains' read goes through it (readsChains), whatever else of Docker
// the setup refuses (no systemd, its unit inactive), which stops its steps alone.
func dockerHere(d remote.DockerFacts) bool {
	host, _ := os.Hostname()
	return d.Error == "" && !slices.Contains(d.Security, "rootless") && d.Name == host
}

// emptyDir says whether dir holds nothing, a filesystem's own lost+found aside (a fresh
// volume's mount point).
func emptyDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && !slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() != "lost+found" })
}

// resolveRoot is root with every link along it followed, as far as its parts are there, each
// .. walked from where the part before it led, the rest as written: one form whether the root
// is made yet or not, which the mounts drop-in names (systemd follows no link in
// RequiresMountsFor). A link is followed by what it says, so one to a volume not mounted, or
// gone, gives the same path as once it is there.
func resolveRoot(root string) string {
	parts := strings.Split(root, "/")
	cur := "/"
	for hops := 0; len(parts) > 0; {
		p := parts[0]
		parts = parts[1:]
		switch p {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, p)
		fi, err := os.Lstat(next)
		if err != nil {
			// Nothing there: no link further on either, the rest as written.
			return filepath.Join(append([]string{next}, parts...)...)
		}
		if fi.Mode()&os.ModeSymlink == 0 || hops >= 40 {
			cur = next
			continue
		}
		target, err := os.Readlink(next)
		if err != nil {
			return filepath.Join(append([]string{next}, parts...)...)
		}
		hops++
		if filepath.IsAbs(target) {
			cur = "/"
		}
		parts = append(strings.Split(target, "/"), parts...)
	}
	return cur
}

// firewallItem is the metadata firewall's item's name.
const firewallItem = setup.FirewallItemName

// firewallFact is the metadata firewall as the deploy user can judge it by stats, who cannot
// read iptables: what the server has of it (firewallHere), its files against devopsy's
// (remote.FirewallFiles, modes included), the boot's link to its unit, no retired unit left
// (remote.FirewallRetired), and the mark the last load that went in left (remote.FirewallMark),
// whoever ran it, each difference said (Differs). The unit's result is words only, for a
// firewall to load again: a load the unit did not run leaves it as it was. The chains are read
// as root apart (readChains).
func firewallFact(h setupHost, allow, door bool) *setup.FirewallFact {
	loaded, mark := firewallHere(h)
	wantDoor := setup.FirewallWantsDoor(door, loaded)
	want := remote.FirewallContainers
	if wantDoor {
		want = remote.FirewallDoor
	}
	// What differs from devopsy's, each said for a firewall to load again.
	var differs []string
	switch mark {
	case want:
	case "":
		differs = append(differs, "no load went in since the last that failed (no mark in "+path.Dir(remote.FirewallMark)+")")
	default:
		differs = append(differs, "its last load loaded the "+mark+"'s, devopsy's the "+want+"'s")
	}
	for _, p := range slices.Sorted(maps.Keys(remote.FirewallFiles(wantDoor))) {
		f := remote.FirewallFiles(wantDoor)[p]
		have, err := os.ReadFile(h.path(p))
		fi, statErr := os.Stat(h.path(p))
		switch {
		case err != nil || statErr != nil:
			differs = append(differs, p+" is missing")
		case string(have) != f.Text:
			differs = append(differs, p+" is not devopsy's")
		case fi.Mode().Perm() != f.Mode:
			differs = append(differs, fmt.Sprintf("%s is mode %o, devopsy's %o", p, fi.Mode().Perm(), f.Mode))
		}
	}
	// The boot's link to the unit, without which no boot loads it.
	if to, err := os.Readlink(h.path(remote.FirewallUnitLink)); err != nil || to != remote.FirewallUnit {
		differs = append(differs, remote.FirewallUnitLink+" does not enable "+remote.FirewallUnit+": no boot loads it")
	}
	// A retired unit an earlier install left, or the link left to it, which the next install
	// takes as its removal does (remote.FirewallRetired).
	for _, u := range slices.Sorted(maps.Keys(remote.FirewallRetired)) {
		l := remote.FirewallRetired[u]
		_, gone := os.Stat(h.path(u))
		_, unlinked := os.Lstat(h.path(l))
		if devopsysAt(h, u) || unlinked == nil && gone != nil {
			differs = append(differs, u+", an older install's, is still there")
		}
	}
	fact := &setup.FirewallFact{Door: wantDoor, State: setup.JudgeFirewall(allow, door, loaded, len(differs) == 0), Loaded: loaded}
	if fact.State == setup.FirewallReload {
		fact.Differs = differs
		if result := unitShow("devopsy-firewall.service", "Result"); result != "" && result != "success" {
			fact.Unit = result
		}
	}
	return fact
}

// firewallHere is what of devopsy's firewall the server has, by stats: its kind (none; the
// containers' where the mark or any file of the firewall's devopsy wrote is there; the door's
// where the mark or the loader says so) and the mark's word, "" with none.
func firewallHere(h setupHost) (setup.FirewallKind, string) {
	data, markErr := os.ReadFile(h.path(remote.FirewallMark))
	mark := strings.TrimSpace(string(data))
	loader, _ := os.ReadFile(h.path(remote.FirewallScript))
	if markErr != nil && !slices.ContainsFunc(append(slices.Collect(maps.Keys(remote.FirewallFiles(true))), slices.Collect(maps.Keys(remote.FirewallRetired))...), func(p string) bool {
		return devopsysAt(h, p)
	}) {
		return setup.NoFirewall, ""
	}
	if mark == remote.FirewallDoor || strings.Contains(string(loader), remote.DoorChain) {
		return setup.DoorFirewall, mark
	}
	return setup.ContainersFirewall, mark
}

// devopsysAt says whether the host's file at p is one of the firewall's devopsy wrote, by its
// header.
func devopsysAt(h setupHost, p string) bool {
	text, err := os.ReadFile(h.path(p))
	return err == nil && remote.FirewallPiece(string(text))
}

// toolsItem is the tools' required item's name.
const toolsItem = setup.RefusalTools

// toolsRefusal is the tools' required item: the required ones of serverTools there, nil when
// they are.
func toolsRefusal() *setup.Refusal {
	return toolsRefusalOf(missingTools(requiredTool))
}

// toolsRefusalOf is the tools' refusal for the required tools missing, nil for none.
func toolsRefusalOf(missing []string) *setup.Refusal {
	if len(missing) == 0 {
		return nil
	}
	stops := setup.RefusalTar
	if slices.Contains(missing, "flock") {
		stops = setup.RefusalFlock
	}
	return &setup.Refusal{Item: toolsItem, Message: "no " + strings.Join(missing, " or ") + " in PATH, which every release needs",
		Fix: toolsFixText, Howto: howto.Tools, Stops: stops}
}

// releaseRootCheck is the release root's required item: there, a directory, uid's (/ is
// refused by remote.ReleaseSettings already). What --server-setup fixes is its action
// (setup.RootMake, setup.RootGive); anything else refuses (dirOwner), and a root that is /
// through a link.
func releaseRootCheck(root string, uid int) (string, *setup.Refusal) {
	refuse := func(msg string) (string, *setup.Refusal) {
		return "", &setup.Refusal{Item: setup.RefusalRoot, Message: msg, Fix: "set releases: root in " + remote.UserConfigFile() + " to a directory of the deploy user's, or move this one aside"}
	}
	if resolveRoot(root) == "/" {
		return refuse("the release root " + root + " is /, through a link: releases would sit among the system's own directories")
	}
	action, refused, _ := dirOwner("the release root", root, uid)
	if refused != "" {
		return refuse(refused)
	}
	return action, nil
}

// dirOwner judges a directory devopsy makes or gives to the deploy user (uid) at p, the release
// root's and the door's space's alike, named so (what, "" for p alone) in a refusal: missing, it is made
// (setup.RootMake); uid's, it is in place (""); root's and empty, a filesystem's own lost+found
// aside, it is given (setup.RootGive); anything else refuses, its words refused: root's with
// files (a system's: /var, /opt), another user's, no directory, or what stat said. fi is p as
// stat read it, nil when it is not there.
func dirOwner(what, p string, uid int) (action, refused string, fi os.FileInfo) {
	name := strings.TrimSpace(what + " " + p)
	fi, err := os.Stat(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return setup.RootMake, "", nil
	case err != nil:
		return "", err.Error(), nil
	case !fi.IsDir():
		return "", name + " is not a directory", fi
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !ok, int(st.Uid) == uid:
		return "", "", fi
	case st.Uid == 0 && emptyDir(p):
		return setup.RootGive, "", fi
	case st.Uid == 0:
		return "", name + " is root's and holds files, and devopsy gives the deploy user only an empty one, never a system directory", fi
	}
	return "", name + " is " + ownerName(st.Uid) + "'s", fi
}

// ownerName is the user uid as a message names an owner: its name, else its number.
func ownerName(uid uint32) string {
	owner := strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(owner); err == nil {
		return u.Username
	}
	return owner
}

// dockerSetup is what the setup reads of Docker beyond docker info: its service's
// InvocationID and how many containers run.
type dockerSetup struct {
	running    int
	invocation string
	// flags and flagKeys are setupRead's.
	flags, flagKeys []string
}

// dockerFix is the way out of a Docker the setup refuses, by the docker topic's steps.
const dockerFix = "install Docker, with its compose and buildx plugins, and start it"

// dockerAnswers are the setup's refusals of what Docker answers itself (d, dockerFacts): no
// daemon answering, else no compose or no buildx plugin, and each floor missed (floorMissed),
// a plugin's only where it answers. --server-info says them under docker, and not again under
// setup (setupInfoEntries); a release (serverReady) and --project-users add refuse on them.
// The plugins and the floor stop no step of the setup: releases need them, and no step does.
func dockerAnswers(d remote.DockerFacts) []setup.Refusal {
	if d.Error != "" {
		return []setup.Refusal{{Item: setup.RefusalDocker, Message: "Docker's daemon does not answer: " + d.Error, Fix: dockerFix, Howto: howto.Docker}}
	}
	var refusals []setup.Refusal
	parts := []floorPart{floorStore}
	if d.Compose == "" {
		refusals = append(refusals, setup.Refusal{Item: setup.RefusalDocker, Message: "Docker has no compose plugin", Fix: dockerFix, Howto: howto.Docker, Stops: setup.RefusalPlugins})
	} else {
		parts = append(parts, floorCompose)
	}
	if d.Buildx == "" {
		refusals = append(refusals, setup.Refusal{Item: setup.RefusalDocker, Message: "Docker has no buildx plugin", Fix: dockerFix, Howto: howto.Docker, Stops: setup.RefusalPlugins})
	} else {
		parts = append(parts, floorBuildx)
	}
	for _, m := range floorMissed(floorRead{containerd: d.Containerd, buildx: d.Buildx, compose: d.Compose}, parts...) {
		refusals = append(refusals, setup.Refusal{Item: setup.RefusalDocker, Message: m.msg, Fix: m.floor.fix, Howto: howto.Docker, Stops: setup.RefusalPlugins})
	}
	return refusals
}

// setupDocker reads Docker as the setup needs it, from docker info as read (d), and refuses
// what it cannot set up: no daemon answering, no compose or buildx, no systemd, or a daemon
// that is not the host's docker.service (rootless, another host's), what Docker answers
// itself first (dockerAnswers).
func setupDocker(h setupHost, d remote.DockerFacts) (dockerSetup, []setup.Refusal) {
	var ds dockerSetup
	refusals := dockerAnswers(d)
	if d.Error != "" {
		return ds, refusals
	}
	// Whether Docker is this host's, judged with or without systemd, so its refusal and fix
	// are said beside one of systemd's.
	refusals = append(refusals, hostRefusals(h, d, "--server-setup restarts Docker with")...)
	if !systemdHere(h) {
		return ds, refusals
	}
	if state := systemctlShow("ActiveState"); state != "active" {
		refusals = append(refusals, setup.Refusal{Item: setup.RefusalDocker, Message: "docker.service is not active (" + state + "): the Docker devopsy reaches is not the host's service", Fix: dockerFix, Howto: howto.Docker})
	}
	// The daemon's own flags, as docker.service starts it next: a key one sets is never put
	// beside it, and a config file of its own is not daemon.json.
	argv := setup.DaemonCommand(systemctlShow("ExecStart"))
	keys, config := setup.DaemonFlags(argv)
	if config != "" && filepath.Clean(config) != setup.DaemonFile {
		refusals = append(refusals, setup.Refusal{Item: setup.RefusalDocker, Message: "docker.service starts dockerd with --config-file " + config + ": " + setup.DaemonFile + " is not what this Docker reads", Stops: setup.RefusalDaemon})
	}
	ds.flagKeys = keys
	for i := 1; i < len(argv); i++ {
		switch {
		case argv[i] == "--config-file":
			i++
		case strings.HasPrefix(argv[i], "--config-file="):
		default:
			ds.flags = append(ds.flags, argv[i])
		}
	}
	ds.invocation = systemctlShow("InvocationID")
	if ids, err := exec.Command("docker", "ps", "-q").Output(); err == nil {
		ds.running = len(strings.Fields(string(ids)))
	}
	return ds, refusals
}

// hostRefusals refuse what a root run of the setup's needs of the host, Docker answering (d):
// a Docker that is this host's, or the run's root container would act on another machine, and
// systemd, which what says the setup needs it for.
func hostRefusals(h setupHost, d remote.DockerFacts, what string) []setup.Refusal {
	var refusals []setup.Refusal
	if !dockerHere(d) {
		refusals = append(refusals, setup.Refusal{Item: setup.RefusalDocker, Message: "the Docker devopsy reaches is not this host's docker.service (rootless, or another host's: " + d.Name + ")", Fix: "unset DOCKER_HOST and DOCKER_CONTEXT for the deploy user"})
	}
	if !systemdHere(h) {
		refusals = append(refusals, setup.Refusal{Item: setup.RefusalDocker, Message: "systemd is not this host's init, which " + what})
	}
	return refusals
}

// systemdHere says whether systemd is the host's init.
func systemdHere(h setupHost) bool {
	_, err := os.Stat(h.path("/run/systemd/system"))
	return err == nil
}

// judgeDaemon is daemon.json as read (data, err from readDaemonFile) parsed, with what Docker
// shows once it takes it, or why devopsy cannot judge it ("" when it can).
func judgeDaemon(data []byte, err error) (setup.Daemon, setup.Visible, string) {
	if err != nil {
		return setup.Daemon{}, setup.Visible{}, setup.DaemonFile + ": " + err.Error()
	}
	d, err := setup.ParseDaemon(data)
	if err != nil {
		return d, setup.Visible{}, setup.DaemonFile + ": " + err.Error()
	}
	v, err := d.Visible()
	if err != nil {
		return d, v, setup.DaemonFile + ": " + err.Error()
	}
	return d, v, ""
}

// systemctlShow is docker.service's property, "" when systemctl cannot say.
func systemctlShow(property string) string { return unitShow("docker.service", property) }

// unitShow is a unit's property, "" when systemctl cannot say.
func unitShow(unit, property string) string {
	out, err := exec.Command("systemctl", "show", "-p", property, "--value", unit).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// systemctlProps is docker.service's properties in one systemctl show, by name, none when
// systemctl cannot say.
func systemctlProps(names ...string) map[string]string {
	props := map[string]string{}
	out, err := exec.Command("systemctl", "show", "-p", strings.Join(names, ","), "docker.service").Output()
	if err != nil {
		return props
	}
	for _, l := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			props[k] = strings.TrimSpace(v)
		}
	}
	return props
}

// startFailed is the error of a restart whose start systemd refused (result, the unit's
// Result), with the commands that start Docker again when its start limit was hit.
func startFailed(result string) error {
	f := &report.Failure{Msg: "Docker did not start again: " + cmp.Or(result, "failed")}
	if result == "start-limit-hit" {
		f.Remedies = append(f.Remedies, report.Fix("start it again, as root:", "systemctl reset-failed docker.service", "systemctl start docker"))
	}
	return f
}

// applySetup applies the server's setup, in order: under the apply lock, the required checks
// and the states read again, the release locks for a restart, one root run, the record, the
// restart's wait, the checks again, then what it did and what waits. In upgrade mode a
// failure is a warning, a changed part is left unasked, and nothing restarts.
func applySetup(h setupHost, a setupApply) int {
	// fail says what failed, a warning under --upgrade, whose every word is one; end is the
	// apply's exit once it went as far as it could, 1 for a refusal or a failure, 0 under
	// --upgrade; failed both, for what ends the apply.
	refused, failures := false, 0
	fail := func(msg string, details ...string) {
		failures++
		if a.upgrade {
			report.Say(report.WarnLevel, msg, details, report.Fix("run --server-setup for this server"))
			return
		}
		report.Error(msg, details...)
	}
	end := func() int {
		if (refused || failures > 0) && !a.upgrade {
			return 1
		}
		return 0
	}
	failed := func(msg string, details ...string) int {
		fail(msg, details...)
		return end()
	}
	record := setup.RecordPath(remote.UserConfigFile())
	lock, err := applyLock(record)
	if err != nil {
		return failed(err.Error())
	}
	defer lock.Close()

	rd, err := readSetup(h, readHostDocker(h))
	if err != nil {
		return failed(err.Error())
	}
	f := rd.facts
	// A refusal stops only the steps that need what it refuses (setup.Facts.Goes), each said
	// once per run with its fix (here, unless the first session said it), the others applied;
	// the apply then fails, a warning under --upgrade.
	refused = len(f.Refused()) > 0
	sayRefusals(f, sayLevel(a.upgrade), a.said, false)
	if a.upgrade && f.RootAction != "" {
		report.Warn(rootWords(f.Root, f.RootAction) + ": --server-setup makes it the deploy user's")
	}
	// Every step is a root run: with none going (Docker refused), nothing goes at all, no
	// image built and no chains read for it, said as the first session says it; but the
	// record's floor note, which a release trusts, never outlives the Docker it noted.
	if f.NoneGoes() {
		if f.FloorNote && !slices.Contains(f.Refused(), setup.RefusalRecord) {
			r := rd.record
			r.Floor = floorNote(rd.floorMet)
			if err := setup.WriteRecord(record, r); err != nil {
				fail(err.Error())
			}
		}
		return failed(cmp.Or(refusedLine(f), "nothing changed"))
	}
	// The chains read as root (an apply may build the util image), so a wipe reads to load
	// again.
	readChains(f.Firewall, rd.docker, true)

	// The parts' decisions, by the states read under the lock, only where they go.
	var put []setup.Part
	var putNames []string
	next := rd.record
	recorded := false
	states := rd.states
	if !f.Goes(setup.StepParts) {
		states = nil
	}
	for _, s := range states {
		var d setup.Decision
		if a.upgrade {
			d = s.Upgrade()
		} else {
			d = s.Setup(a.answers[s.Part.Name])
		}
		if d.Put {
			put = append(put, s.Part)
			putNames = append(putNames, s.Part.Name)
		}
		if d.Record != nil {
			next = next.Set(s.Part.Name, *d.Record)
			recorded = true
		}
		if d.Left && s.State == setup.Changed {
			_, asked := a.answers[s.Part.Name]
			switch {
			case asked:
				report.Warn("daemon.json's " + s.Part.Name + " changed since it was asked about: left as it is, and asked again at the next --server-setup")
			case !a.upgrade:
				// --server-setup asks about every changed part it read: one with no answer
				// changed between its two sessions.
				report.Warn("daemon.json's " + s.Part.Name + " changed since it was read: left as it is, and asked at the next --server-setup")
			case s.Observed == "{}":
				report.Warn("daemon.json's "+s.Part.Name+" was removed by someone: left out", "--server-setup asks whether to put it back")
			default:
				report.Warn("daemon.json's "+s.Part.Name+" is someone else's value: left as it is", "--server-setup asks whether to keep it")
			}
		}
	}
	var daemonText []byte
	if len(put) > 0 {
		out, changed, err := rd.daemon.Merge(put)
		switch {
		case err != nil:
			fail(err.Error())
			put, putNames, next, recorded = nil, nil, rd.record, false
		case changed:
			daemonText = out
		}
	}
	file := func(item string) bool {
		i := slices.IndexFunc(f.Files, func(x setup.FileFact) bool { return x.Item == item })
		return i >= 0 && !f.Files[i].InSync
	}
	sysctlText, dropinText := "", ""
	if file(setup.ItemBuffers) && f.Goes(setup.StepBuffers) {
		sysctlText = setup.SysctlText()
	}
	if file(setup.ItemMounts) && f.Goes(setup.StepMounts) {
		dropinText = rd.dropin
	}
	raise := f.Goes(setup.StepBuffers) && slices.ContainsFunc(f.Buffers, func(b setup.BufferFact) bool { return b.Low })
	give := !a.upgrade && f.RootAction != "" && f.Goes(setup.StepRoot)
	// A restart only for what Docker has to take, as read under the lock: daemon.json put by
	// this run, or a change it has not taken yet.
	restart := a.restart && f.Goes(setup.StepRestart) && (len(daemonText) > 0 || f.Pending)
	switch {
	case a.restart && !f.Goes(setup.StepRestart):
		report.Info("Docker is not restarted: its restart is refused (" + strings.Join(f.Blocks(setup.StepRestart), ", ") + ")")
	case a.restart && !restart:
		report.Info("Docker is not restarted: it has nothing new to take from daemon.json")
	case restart && a.idle && f.Running > 0:
		restart = false
		// What it then waits for is said by the run's end (pendingWords).
		report.Info("Docker is not restarted, with containers running")
	}

	// The release locks of the deploy user's environments, held until the restart is
	// confirmed or given up: Docker never restarts under a release.
	if restart {
		var held []*os.File
		defer func() {
			for _, l := range held {
				l.Close()
			}
		}()
		bases, err := deployEnvBases(f.Root)
		if err != nil {
			return failed(err.Error() + ": nothing changed")
		}
		for _, base := range bases {
			l, err := os.OpenFile(filepath.Join(base, ".lock"), os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return failed(err.Error() + ": nothing changed")
			}
			held = append(held, l)
			if ok, _ := tryFlock(l, syscall.LOCK_EX); !ok {
				report.Fail(errors.New("a release or another change runs in "+base+": Docker is not restarted under it, and nothing changed"),
					report.Fix("run --server-setup again once it ends"))
				return 1
			}
		}
		if d, err := remote.ServerDoor(); err == nil && d != nil && len(d.Users) > 0 {
			report.Info("project users' releases are not waited for: one under way is cut by the restart")
		}
	}

	// docker.service's InvocationID just before the run: the marker's, and what a restart
	// is known by.
	before := systemctlShow("InvocationID")
	var out bytes.Buffer
	queued, dockerDown := false, false
	if len(daemonText) > 0 || sysctlText != "" || dropinText != "" || raise || give || restart {
		after := ""
		if restart {
			after = "restart"
		}
		token := runToken()
		err := rootSetupRun(h, f.Root, daemonText, sysctlText, dropinText, raise, give, after, before, token, rd.flags, &out)
		// A restart asked may take the run's container down before its words reach here: the
		// marker this run wrote for it, by its token (an older run's, pending, never), says it
		// was asked, and the bounded wait tells whether it came. A run that ended on its own
		// exit (a put refused, 4, or not written, 5) was never cut by a restart.
		marker, _ := os.ReadFile(h.path(setup.RestartMarker))
		asked := restart && !scriptExit(err) && (hasLine(out.String(), remote.SetupAsked) || before != "" && strings.TrimSpace(string(marker)) == before+" "+token)
		queued = asked && !hasLine(out.String(), remote.SetupNoSystem)
		sayRootLines(out.String(), putNames, f.Root)
		// The run puts in order and stops at a failed put, daemon.json's last: what it said it did
		// went in, and only that is judged again; what failed is said, and the next steps go on. A
		// restart asked cut the run's words, never what it did.
		runErr := err
		if asked {
			runErr = nil
		}
		did := func(went ...string) bool { return rootRunDid(true, runErr, out.String(), "", went...) }
		if runErr != nil {
			fail(err.Error())
		}
		if !did(remote.SetupPut + " " + setup.DaemonFile) {
			if len(a.answers) > 0 {
				report.Warn("daemon.json was not put, so its answers are asked again at the next --server-setup")
			}
			put, putNames, daemonText, next, recorded = nil, nil, nil, rd.record, false
		}
		if !did(remote.SetupPut + " " + setup.SysctlFile) {
			sysctlText = ""
		}
		if !did(remote.SetupPut + " " + setup.MountsDropIn) {
			dropinText = ""
		}
		raise, give = raise && did(remote.SetupRaised), give && did(remote.SetupMade+" "+f.Root, remote.SetupGave+" "+f.Root)
	}
	// The record follows the file, whatever the restart does next; none over a record
	// devopsy could not read, which it says to remove.
	if (recorded || (!rd.exists && !a.upgrade)) && !slices.Contains(f.Refused(), setup.RefusalRecord) {
		if err := setup.WriteRecord(record, next); err != nil {
			fail(err.Error())
		} else if !rd.exists {
			report.Info("this server is set up by devopsy now: its record is " + record)
		}
	}
	if queued {
		err := waitRestart(before, h, setup.OutsideParts(rd.states))
		if err == nil {
			report.Info("Docker restarted, with daemon.json's values")
		} else {
			// The marker is there already; the system's restart flag goes now, by a root run
			// that needs Docker up, and only now: raised before the restart, a restart that
			// came would leave a reboot asked for nothing.
			said := &report.Failure{Msg: err.Error()}
			var cause *report.Failure
			if errors.As(err, &cause) {
				said.Remedies = cause.Remedies
			}
			said.Remedies = append(said.Remedies, report.Hint("what Docker said, as root:", "journalctl -u docker.service"))
			var marked bytes.Buffer
			failures++
			switch err := rootSetupRun(h, f.Root, nil, "", "", false, false, "mark", before, "", rd.flags, &marked); {
			case err == nil:
				report.Fail(said)
				sayRootLines(marked.String(), putNames, f.Root)
			case dockerInfo().Error != "":
				said.Details = []string{"Docker does not answer, so nothing was flagged for a reboot: it takes daemon.json whenever it starts again"}
				report.Fail(said)
				// Docker down, no root run goes: the firewall waits for the next run, said.
				dockerDown = true
			default:
				said.Details = []string{"nothing was flagged for a reboot: " + err.Error()}
				report.Fail(said)
			}
		}
	}

	// The metadata firewall, by the firewall's own root run, as its state says; --upgrade
	// only loads again the kind loaded (setup.FirewallState.Upgrade).
	fwRan := false
	if fw := f.Firewall; fw != nil && fw.State.Changes() && f.Goes(setup.StepFirewall) {
		if dockerDown {
			fail("not applied: " + setup.FirewallItemName + ", Docker not answering since its restart")
		} else if a.upgrade && !fw.State.Upgrade() {
			report.Warn("--upgrade leaves the metadata firewall to --server-setup: " + firewallLine(fw))
		} else if err := applyFirewall(h, fw); err != nil {
			fail(err.Error(), report.Details(err)...)
		} else {
			fwRan = true
		}
	}

	// The checks again: what was applied must read in sync now, a refused step's left aside.
	again, err := readSetup(h, readHostDocker(h))
	if err != nil {
		fail(err.Error())
		return end()
	}
	var left []string
	for _, p := range again.facts.Parts {
		if slices.Contains(putNames, p.Name) && p.State != setup.InSync {
			left = append(left, "daemon.json's "+p.Name+" is still not devopsy's")
		}
	}
	for _, x := range again.facts.Files {
		if !x.InSync && (x.Item == setup.ItemBuffers && sysctlText != "" || x.Item == setup.ItemMounts && dropinText != "") {
			left = append(left, x.Path+" is still not devopsy's")
		}
	}
	for _, b := range again.facts.Buffers {
		if b.Low && raise {
			left = append(left, fmt.Sprintf("%s is %d, below %d, until a reboot loads %s", b.Key, b.Live, setup.BufferSize, setup.SysctlFile))
		}
	}
	if give && again.facts.RootAction != "" {
		left = append(left, "the release root "+f.Root+" is still not the deploy user's")
	}
	if fw := again.facts.Firewall; fwRan && fw != nil && fw.State != setup.FirewallInSync {
		why := "the metadata firewall is still " + fw.State.String()
		if fw.Unit != "" {
			why += ", its unit " + fw.Unit
		}
		left = append(left, why)
	}
	if len(left) > 0 {
		fail("what was applied did not hold", left...)
	}
	// An apply with no failure notes the devopsy that made it, which --upgrade applies once:
	// one a refusal stopped a change of is applied again. Every apply notes whether this
	// devopsy found Docker at its floor, as its last read finds it (floorNote).
	if !slices.Contains(f.Refused(), setup.RefusalRecord) {
		r := again.record
		if failures == 0 && len(f.Stopped()) == 0 {
			r.Version = version
		}
		r.Floor = floorNote(again.floorMet)
		if r.Version != again.record.Version || r.Floor != again.record.Floor {
			if err := setup.WriteRecord(record, r); err != nil {
				fail(err.Error())
			}
		}
	}
	if again.facts.Pending && !queued && !hasLine(out.String(), remote.SetupMarked) {
		state, detail := pendingWords(again.facts.PendingParts, again.facts.Reboot)
		report.Say(report.InfoLevel, "Docker "+state, []string{detail}, soonerHint(""))
	}
	if dropinText != "" && !queued {
		dropInWaits(rd.space != "")
	}
	if line := refusedLine(f); line != "" {
		report.Say(sayLevel(a.upgrade), line, nil)
	}
	return end()
}

// applyLock takes the server's apply lock beside its record (setup.LockPath), bounded and
// said: --server-setup's and --upgrade's applies and --project-users setup's hold it, so no two
// change the server at once.
func applyLock(record string) (*os.File, error) {
	if record == "" {
		return nil, errors.New("no home to keep devopsy's record of this server in")
	}
	if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(setup.LockPath(record), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := (boundedWait{what: "another setup of this server"}).wait(context.Background(), report.Default(), func() (bool, error) { return tryFlock(lock, syscall.LOCK_EX) }); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

// refusalWords is how every command says a refusal of the setup's: its item and message, its
// fix.
func refusalWords(r setup.Refusal) (string, []report.Remedy) {
	return r.Item + ": " + r.Message, fixWords(r)
}

// daemonWords is how every command says a daemon.json devopsy cannot judge (why, the facts'
// DaemonError), which none rewrites.
func daemonWords(why string) (string, []report.Remedy) {
	return why + ": devopsy leaves the file as it is", []report.Remedy{report.Fix("make it a JSON object Docker reads, each value of the type Docker takes")}
}

// rootWords is how every command says the release root that --server-setup makes or gives
// (action, setup.RootMake or RootGive), "" for neither.
func rootWords(root, action string) string {
	switch action {
	case setup.RootMake:
		return "the release root " + root + " is missing"
	case setup.RootGive:
		return "the release root " + root + " is root's"
	}
	return ""
}

// sayLevel is a refusal's level: an error, a warning under --upgrade.
func sayLevel(upgrade bool) report.Level {
	if upgrade {
		return report.WarnLevel
	}
	return report.ErrorLevel
}

// fixWords is a refusal's fix as a remedy, none when it has none: its text, and its command
// (setup.Refusal.Command) or the command that prints its topic's steps (setup.Refusal.Howto)
// when it has one.
func fixWords(r setup.Refusal) []report.Remedy {
	switch {
	case r.Fix == "":
		return nil
	case r.Command != "":
		return []report.Remedy{report.Fix(r.Fix, r.Command)}
	case r.Howto == "":
		return []report.Remedy{report.Fix(r.Fix)}
	}
	return []report.Remedy{stepsFix(r.Fix, r.Howto)}
}

// rootSetupRun runs remote.SetupRootScript as root on the host (setupRun), its words into out,
// the docker client's own dropped once the restart is queued.
func rootSetupRun(h setupHost, root string, daemon []byte, sysctl, dropin string, raise, give bool, after, invocation, token string, flags []string, out *bytes.Buffer) error {
	q := &queuedLines{out: out}
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return ""
	}
	owner := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	run := setupRun(h.root, owner, flag(give), root, string(daemon), sysctl, dropin, flag(raise), after, invocation, strings.Join(flags, "\n"), token)
	run.Stderr = quietQueued{q}
	return run.run(q, func(err error) error { return fmt.Errorf("the setup's run as root: %w", err) })
}

// runToken is a root run's own token, which a restart's marker carries after the id.
func runToken() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// scriptExit says whether err is the setup's root script's own exit (a put refused, 4; one
// not written, 5), which no restart cuts.
func scriptExit(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && (ee.ExitCode() == 4 || ee.ExitCode() == 5)
}

// setupRun is remote.SetupRootScript's root run with its arguments: the host's network and
// NET_ADMIN for the live buffers (sysctl), its pid namespace for systemctl (systemd refuses a
// peer it cannot see), writing the host's files.
func setupRun(args ...string) hostRun {
	return hostRun{Write: true, Network: true, PID: true, NetAdmin: true, Script: remote.SetupRootScript, Args: args}
}

// queuedLines is the root run's stdout, which says when Docker's restart is queued.
type queuedLines struct {
	mu     sync.Mutex
	out    *bytes.Buffer
	queued bool
}

func (q *queuedLines) Write(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n, err := q.out.Write(p)
	q.queued = q.queued || hasLine(q.out.String(), remote.SetupQueued)
	return n, err
}

// quietQueued is the root run's stderr, devopsy's until the restart is queued, then dropped:
// the restart cuts the docker client's connection to its container, which it says, and that
// is the restart, not a failure.
type quietQueued struct{ q *queuedLines }

func (w quietQueued) Write(p []byte) (int, error) {
	w.q.mu.Lock()
	defer w.q.mu.Unlock()
	if w.q.queued {
		return len(p), nil
	}
	return os.Stderr.Write(p)
}

// rootRunDid says whether a root run did a step it was asked (asked): every one asked when the
// run went (err nil), else only one whose lines (out) say it went, one of went holding (hasLine),
// and do not say it failed (failed, "" for a run that stops at its first failure): what a run did
// not say it did is no "what was applied did not hold", and a failure it said is said once. Every
// apply's check after its root run (--server-setup's, --project-users setup's) goes by it.
func rootRunDid(asked bool, err error, out, failed string, went ...string) bool {
	switch {
	case !asked || err == nil:
		return asked
	case failed != "" && hasLine(out, failed):
		return false
	}
	return slices.ContainsFunc(went, func(w string) bool { return hasLine(out, w) })
}

// hasLine says whether out holds the line word, alone or first.
func hasLine(out, word string) bool {
	for _, l := range strings.Split(out, "\n") {
		if l == word || strings.HasPrefix(l, word+" ") {
			return true
		}
	}
	return false
}

// pendingWords is how every command says Docker has yet to take daemon.json: the restart it
// needs, for parts when known, and what does it, the reboot the server needs when the
// system's restart flag is raised (reboot), else whoever restarts Docker.
func pendingWords(parts []string, reboot bool) (state, detail string) {
	state = "needs a restart to take daemon.json"
	if len(parts) > 0 {
		state += " (" + strings.Join(parts, ", ") + ")"
	}
	if reboot {
		return state, "nothing is lost by waiting: the reboot the server needs (" + remote.RebootRequired + ") does it"
	}
	return state, "it waits for Docker's next restart, whoever makes it"
}

// soonerHint is --server-setup offered to restart Docker before what pendingWords says does
// it: a choice, never a fix. ref is the server as typed, "" where no address is known.
func soonerHint(ref string) report.Remedy {
	if ref == "" {
		return report.Hint("to restart Docker sooner, run --server-setup for this server")
	}
	return report.Hint("to restart Docker sooner:", "devopsy @"+ref+" --server-setup")
}

// sayRootLines says what the root run changed, line by line (remote.SetupRootScript's).
func sayRootLines(out string, parts []string, root string) {
	who := "the deploy user"
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	for _, l := range strings.Split(out, "\n") {
		word, rest, _ := strings.Cut(l, " ")
		switch word {
		case remote.SetupPut:
			if rest == setup.DaemonFile && len(parts) > 0 {
				report.Info("put " + rest + ": " + strings.Join(parts, ", "))
			} else {
				report.Info("put " + rest)
			}
		case remote.SetupMade:
			report.Info("made the release root " + rest)
		case remote.SetupGave:
			report.Info("gave the release root " + rest + " to " + who)
		case remote.SetupRaised:
			report.Info(fmt.Sprintf("raised %s to %d", rest, setup.BufferSize))
		case remote.SetupQueued:
			report.Info("restarting Docker")
		case remote.SetupMarked:
			// The run raised the system's restart flag with the marker.
			state, detail := pendingWords(parts, true)
			report.Say(report.InfoLevel, "Docker "+state, []string{detail}, soonerHint(""))
		case remote.SetupNoSystem:
			report.Say(report.WarnLevel, "Docker was not restarted", nil, report.Fix("restart it, as root:", "systemctl restart docker"))
		}
	}
}

// waitRestart waits, bounded and said, until Docker's restart is confirmed: docker.service's
// InvocationID no longer before, and docker info showing daemon.json's values, but for the
// parts set outside it.
func waitRestart(before string, h setupHost, outside []string) error {
	return (boundedWait{what: "Docker's restart", bound: h.restartWait, every: time.Second}).wait(context.Background(), report.Default(), func() (bool, error) {
		unit := systemctlProps("ActiveState", "Result", "InvocationID")
		// A start systemd refused ends the wait: Docker will not come back by itself.
		if unit["ActiveState"] == "failed" {
			return false, startFailed(unit["Result"])
		}
		if id := unit["InvocationID"]; id == "" || id == before {
			return false, nil
		}
		info := dockerInfo()
		if info.Error != "" {
			return false, nil
		}
		_, want, why := judgeDaemon(readDaemonFile(h))
		if why != "" {
			return false, errors.New(why)
		}
		return want.Except(outside).Equal(info.Running.Except(outside)), nil
	})
}

// runServerReady is remote.ServerReadyFlag, the upload's first step on the server: what every
// release needs of it, by stats and path lookups alone, said with its fix (ref the server as
// the address named it), then the firewall, a warning only (warnReleaseFirewall), the one
// word on the setup a release says, since nothing else shows a wipe. A project user checks
// nothing (its tree is --project-users add's, and its Docker the door) but the firewall,
// which it asks the door (warnDoorFirewall).
func runServerReady(args []string) int {
	if len(args) != 1 {
		return fail("usage: devopsy " + remote.ServerReadyFlag + " <server>")
	}
	projectUser := asProjectUser()
	r, err := remote.ReleaseSettings()
	if err != nil && !projectUser {
		report.Fail(fmt.Errorf("%w: nothing released", err))
		return 1
	}
	errs := serverReady(r.Root, args[0], projectUser)
	for _, err := range errs {
		report.Fail(err)
	}
	if len(errs) > 0 {
		return 1
	}
	if projectUser {
		warnDoorFirewall(askDoorFirewall)
	} else {
		warnReleaseFirewall(setupHost{root: os.Getenv(remote.HostRootEnv)}, args[0])
	}
	return 0
}

// serverReady is what refuses a release on this server: the tools, the release root, then
// what Docker answers of itself (dockerReady), asked only where the record's note does not
// say this devopsy found Docker at its floor (floorNoted); nothing for a project user
// (projectUser), whose tree --project-users add made and whose Docker is the door. A release
// never writes the record.
func serverReady(root, ref string, projectUser bool) []error {
	if projectUser {
		return nil
	}
	var errs []error
	if t := toolsRefusal(); t != nil {
		errs = append(errs, &report.Failure{Msg: t.Message + ": nothing released", Remedies: fixWords(*t)})
	}
	if err := releaseRootReady(root, "nothing released", ref); err != nil {
		errs = append(errs, err)
	}
	if floorNoted() {
		return errs
	}
	return append(errs, dockerReady("nothing released")...)
}

// floorNoted says whether the setup's record notes that this devopsy's apply found Docker at
// its floor (floorNote): a record missing, unreadable or of another devopsy notes nothing, and
// a development build trusts no note, another build's of the same name included.
func floorNoted() bool {
	if !update.IsRelease(version) {
		return false
	}
	r, exists, err := setup.ReadRecord(setup.RecordPath(remote.UserConfigFile()))
	return err == nil && exists && r.Floor == version
}

// dockerReady is the setup's required check of Docker's own answers (dockerAnswers of
// dockerFacts: its daemon, its plugins, the floor), each refusal an error with its fix, what
// goes undone said in nothing: a release and --project-users add refuse on it before anything
// changes.
func dockerReady(nothing string) []error {
	var errs []error
	for _, r := range dockerAnswers(dockerFacts()) {
		errs = append(errs, &report.Failure{Msg: r.Message + ": " + nothing, Remedies: fixWords(r)})
	}
	return errs
}

// releaseRootRefusal is the release root's required item for what needs it as it is (a
// release, --project-users add and setup): a directory of this user's (releaseRootCheck), nil
// when it is; one --server-setup makes or gives has --server-setup on ref as its fix (in words
// when ""), anything else the check's own.
func releaseRootRefusal(root, ref string) *setup.Refusal {
	action, refusal := releaseRootCheck(root, os.Getuid())
	if refusal != nil || action == "" {
		return refusal
	}
	r := &setup.Refusal{Item: setup.RefusalRoot, Message: rootWords(root, action), Fix: "set the server up with --server-setup first, which makes it the deploy user's"}
	if ref != "" {
		r.Fix, r.Command = "set the server up:", "devopsy @"+ref+" --server-setup"
	}
	return r
}

// releaseRootReady refuses a release root that is not a directory of this user's
// (releaseRootRefusal), what goes undone said in nothing.
func releaseRootReady(root, nothing, ref string) error {
	if r := releaseRootRefusal(root, ref); r != nil {
		return &report.Failure{Msg: r.Message + ": " + nothing, Remedies: fixWords(*r)}
	}
	return nil
}

// applyFirewall loads the metadata firewall as devopsy loads it, or removes it (fw's state),
// as root through the firewall's own run, then reads the chains back, as root, for what they
// lack.
func applyFirewall(h setupHost, fw *setup.FirewallFact) error {
	if fw.State == setup.FirewallRemove {
		run := firewallRun(remote.FirewallRemove(), true)
		run.Args = []string{h.root}
		if err := run.run(io.Discard, func(err error) error { return fmt.Errorf("the firewall's run as root: %v", err) }); err != nil {
			return err
		}
		report.Info("removed devopsy's firewall (firewall: metadata: allow): containers reach the cloud's metadata services")
		return nil
	}
	run := firewallRun(remote.FirewallInstall(fw.Door), true)
	run.Args = []string{h.root}
	_, wrong, err := firewallJudged(context.Background(), run, fw.Door, true)
	if err != nil {
		return err
	}
	if len(wrong) > 0 {
		return &report.Failure{Msg: "the firewall did not load as devopsy's", Details: wrong}
	}
	report.Info("loaded devopsy's firewall: containers kept off the cloud's metadata services, DNS there aside")
	return nil
}
