package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
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
	// docker: Docker answers as the host's docker.service, so a root run can go.
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
	return fail("usage: devopsy " + remote.ServerSetupFlag + " facts | apply [--keep <part>=<sha256>]... [--replace <part>=<sha256>]... [--restart | --restart-idle] [--upgrade]")
}

// setupApply is what an apply was asked: the answers by part, a restart now (idle: only
// with no container running), upgrade mode. A restart asked is a condition the apply judges
// under its locks, never a decision taken from the first session's facts.
type setupApply struct {
	answers map[string]setup.Answer
	restart bool
	idle    bool
	upgrade bool
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
	rd.docker = len(refusals) == 0
	f.Running, f.LiveRestore, rd.running = info.running, hd.facts.Running.LiveRestore, hd.facts.Running
	rd.invocation, rd.flags, rd.flagKeys = info.invocation, info.flags, info.flagKeys

	recordPath := setup.RecordPath(remote.UserConfigFile())
	rd.record, rd.exists, err = setup.ReadRecord(recordPath)
	if err != nil {
		f.Refusals = append(f.Refusals, setup.Refusal{Item: "record", Message: err.Error(), Fix: "remove it: devopsy writes it again at the next --server-setup"})
	}
	if rd.exists {
		f.Record = recordPath
	}
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
	if text, err := setup.MountsDropInText(f.Root); err != nil {
		f.Refusals = append(f.Refusals, setup.Refusal{Item: "release root", Message: "releases: root " + err.Error(), Fix: "set releases: root in " + remote.UserConfigFile() + " to a path with none"})
	} else {
		have, _ := os.ReadFile(h.path(setup.MountsDropIn))
		f.Files = append(f.Files, setup.FileFact{Item: setup.ItemMounts, Path: setup.MountsDropIn, InSync: string(have) == text})
	}
	marker, _ := os.ReadFile(h.path(setup.RestartMarker))
	outside := setup.OutsideParts(rd.states)
	f.Pending = rd.docker && f.DaemonError == "" && setup.Pending(string(marker), rd.invocation, rd.fileVisible.Except(outside), rd.running.Except(outside))

	mountinfo, _ := os.ReadFile(h.path("/proc/self/mountinfo"))
	for _, c := range setup.Candidates(string(mountinfo), h.path("/sys"), hd.facts.RootDir) {
		if _, size, free, _, err := statDisk(c.MountPoint); err == nil {
			c.Size, c.Free = size, free
		}
		f.Candidates = append(f.Candidates, c)
	}
	f.Projects = len(deployEnvBases(f.Root)) > 0
	d, err := remote.ServerDoor()
	if err != nil {
		return rd, err
	}
	rd.door = d != nil
	allow := r.Firewall.Metadata == remote.MetadataAllow
	f.Firewall = firewallFact(h, allow, rd.door)
	if f.Firewall.State == setup.FirewallRefused {
		msg := "firewall: metadata: allow, on a server with door: in its config: its project users' containers never reach a metadata service"
		if !rd.door {
			msg = "firewall: metadata: allow, on a server where the door's firewall is loaded: devopsy never takes a door's firewall off a server"
		}
		f.Refusals = append(f.Refusals, setup.Refusal{Item: firewallItem, Message: msg, Fix: "take firewall: metadata: allow out of " + remote.UserConfigFile()})
	}
	return rd, nil
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

// firewallFact is the metadata firewall as the deploy user can judge it, who cannot read
// iptables: what the server has of it (any file of devopsy's by its header, or the mark; the
// door's where the mark or the loader says so), its files against devopsy's
// (remote.FirewallFiles, modes included), the boot's link to its unit, and the mark the last load that went in left (remote.FirewallMark), whoever ran it. The unit's
// result is words only, for a firewall to load again: a load the unit did not run leaves it
// as it was. The full read of the chains is root's, the apply's.
func firewallFact(h setupHost, allow, door bool) *setup.FirewallFact {
	mark, markErr := os.ReadFile(h.path(remote.FirewallMark))
	loader, _ := os.ReadFile(h.path(remote.FirewallScript))
	loaded := setup.NoFirewall
	if markErr == nil || slices.ContainsFunc(slices.Collect(maps.Keys(remote.FirewallFiles(true))), func(p string) bool {
		text, err := os.ReadFile(h.path(p))
		return err == nil && remote.FirewallPiece(string(text))
	}) {
		loaded = setup.ContainersFirewall
	}
	if strings.TrimSpace(string(mark)) == remote.FirewallDoor || strings.Contains(string(loader), remote.DoorChain) {
		loaded = setup.DoorFirewall
	}
	wantDoor := setup.FirewallWantsDoor(door, loaded)
	want := remote.FirewallContainers
	if wantDoor {
		want = remote.FirewallDoor
	}
	inSync := strings.TrimSpace(string(mark)) == want
	for p, f := range remote.FirewallFiles(wantDoor) {
		have, err := os.ReadFile(h.path(p))
		fi, statErr := os.Stat(h.path(p))
		inSync = inSync && err == nil && statErr == nil && string(have) == f.Text && fi.Mode().Perm() == f.Mode
	}
	// The boot's link to the unit, without which no boot loads it.
	to, err := os.Readlink(h.path(remote.FirewallUnitLink))
	inSync = inSync && err == nil && to == remote.FirewallUnit
	fact := &setup.FirewallFact{Door: wantDoor, State: setup.JudgeFirewall(allow, door, loaded, inSync)}
	if fact.State == setup.FirewallReload {
		if result := unitShow("devopsy-firewall.service", "Result"); result != "" && result != "success" {
			fact.Unit = result
		}
	}
	return fact
}

// toolsItem is the tools' required item's name.
const toolsItem = "tools"

// toolsRefusal is the tools' required item: the required ones of serverTools in PATH, nil
// when they are.
func toolsRefusal() *setup.Refusal {
	var missing []string
	for _, t := range serverTools {
		if _, err := exec.LookPath(t.name); err != nil && t.required {
			missing = append(missing, t.name)
		}
	}
	return toolsRefusalOf(missing)
}

// toolsRefusalOf is the tools' refusal for the required tools missing, nil for none.
func toolsRefusalOf(missing []string) *setup.Refusal {
	if len(missing) == 0 {
		return nil
	}
	return &setup.Refusal{Item: toolsItem, Message: "no " + strings.Join(missing, " or ") + " in PATH, which every release needs",
		Fix: "install them with the system's packages (tar, util-linux), or with devopsy-server's setup.sh"}
}

// releaseRootCheck is the release root's required item: there, a directory, uid's (/ is
// refused by remote.ReleaseSettings already). What --server-setup fixes is its action
// (setup.RootMake, setup.RootGive); anything else refuses: not a directory, or another user's.
func releaseRootCheck(root string, uid int) (string, *setup.Refusal) {
	refuse := func(msg string) (string, *setup.Refusal) {
		return "", &setup.Refusal{Item: "release root", Message: msg, Fix: "set releases: root in " + remote.UserConfigFile() + " to a directory of the deploy user's, or move this one aside"}
	}
	if resolveRoot(root) == "/" {
		return refuse("the release root " + root + " is /, through a link: releases would sit among the system's own directories")
	}
	fi, err := os.Stat(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return setup.RootMake, nil
	case err != nil:
		return refuse(err.Error())
	case !fi.IsDir():
		return refuse("the release root " + root + " is not a directory")
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !ok, int(st.Uid) == uid:
		return "", nil
	case st.Uid == 0 && emptyDir(root):
		return setup.RootGive, nil
	case st.Uid == 0:
		return refuse("the release root " + root + " is root's and holds files: only an empty one is given to the deploy user, never a system directory")
	}
	owner := strconv.Itoa(int(st.Uid))
	if u, err := user.LookupId(owner); err == nil {
		owner = u.Username
	}
	return refuse("the release root " + root + " is " + owner + "'s")
}

// dockerSetup is what the setup reads of Docker beyond docker info: its service's
// InvocationID and how many containers run.
type dockerSetup struct {
	running    int
	invocation string
	// flags and flagKeys are setupRead's.
	flags, flagKeys []string
}

// setupDocker reads Docker as the setup needs it, from docker info as read (d), and refuses
// what it cannot set up: no daemon answering, no compose or buildx, no systemd, or a daemon
// that is not the host's docker.service (rootless, another host's).
func setupDocker(h setupHost, d remote.DockerFacts) (dockerSetup, []setup.Refusal) {
	var ds dockerSetup
	fix := "install Docker, with its compose and buildx plugins, and start it: devopsy-server's setup.sh does"
	if d.Error != "" {
		return ds, []setup.Refusal{{Item: "Docker", Message: "Docker's daemon does not answer: " + d.Error, Fix: fix}}
	}
	var refusals []setup.Refusal
	if d.Compose == "" {
		refusals = append(refusals, setup.Refusal{Item: "Docker", Message: "Docker has no compose plugin", Fix: fix})
	}
	if !d.Buildx {
		refusals = append(refusals, setup.Refusal{Item: "Docker", Message: "Docker has no buildx plugin", Fix: fix})
	}
	if _, err := os.Stat(h.path("/run/systemd/system")); err != nil {
		refusals = append(refusals, setup.Refusal{Item: "Docker", Message: "systemd is not this host's init, which --server-setup restarts Docker with"})
		return ds, refusals
	}
	if state := systemctlShow("ActiveState"); state != "active" {
		refusals = append(refusals, setup.Refusal{Item: "Docker", Message: "docker.service is not active (" + state + "): the Docker devopsy reaches is not the host's service", Fix: fix})
	}
	host, _ := os.Hostname()
	if slices.Contains(d.Security, "rootless") || d.Name != host {
		refusals = append(refusals, setup.Refusal{Item: "Docker", Message: "the Docker devopsy reaches is not this host's docker.service (rootless, or another host's: " + d.Name + ")", Fix: "unset DOCKER_HOST and DOCKER_CONTEXT for the deploy user"})
	}
	// The daemon's own flags, as docker.service starts it next: a key one sets is never put
	// beside it, and a config file of its own is not daemon.json.
	argv := setup.DaemonCommand(systemctlShow("ExecStart"))
	keys, config := setup.DaemonFlags(argv)
	if config != "" && filepath.Clean(config) != setup.DaemonFile {
		refusals = append(refusals, setup.Refusal{Item: "Docker", Message: "docker.service starts dockerd with --config-file " + config + ": " + setup.DaemonFile + " is not what this Docker reads"})
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
	failed := func(msg string, details ...string) int {
		if a.upgrade {
			report.Say(report.WarnLevel, msg, details, report.Fix("run --server-setup for this server"))
			return 0
		}
		report.Error(msg, details...)
		return 1
	}
	record := setup.RecordPath(remote.UserConfigFile())
	if record == "" {
		return failed("no home to keep devopsy's record of this server in")
	}
	if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
		return failed(err.Error())
	}
	lock, err := os.OpenFile(setup.LockPath(record), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return failed(err.Error())
	}
	defer lock.Close()
	if err := (boundedWait{what: "another --server-setup"}).wait(context.Background(), report.Default(), func() (bool, error) { return tryFlock(lock, syscall.LOCK_EX) }); err != nil {
		return failed(err.Error())
	}

	rd, err := readSetup(h, readHostDocker(h))
	if err != nil {
		return failed(err.Error())
	}
	f := rd.facts
	// A required item refuses the apply; under --upgrade each is a warning, and only
	// Docker's (no root run without it) or a broken record stops it.
	for _, r := range f.Refusals {
		msg, fixes := refusalWords(r)
		report.Say(sayLevel(a.upgrade), msg, nil, fixes...)
	}
	stop := len(f.Refusals) > 0
	if a.upgrade {
		stop = !rd.docker || slices.ContainsFunc(f.Refusals, func(r setup.Refusal) bool { return r.Item == "record" })
	}
	if stop {
		return failed("nothing changed")
	}
	if a.upgrade && f.RootAction != "" {
		report.Warn(rootWords(f.Root, f.RootAction) + ": --server-setup makes it the deploy user's")
	}
	if f.DaemonError != "" {
		msg, fixes := daemonWords(f.DaemonError)
		report.Say(sayLevel(a.upgrade), msg, nil, fixes...)
		if !a.upgrade {
			return failed("nothing changed")
		}
	}

	// The parts' decisions, by the states read under the lock.
	var put []setup.Part
	var putNames []string
	next := rd.record
	recorded := false
	for _, s := range rd.states {
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
		if err != nil {
			return failed(err.Error())
		}
		if changed {
			daemonText = out
		}
	}
	file := func(item string) bool {
		i := slices.IndexFunc(f.Files, func(x setup.FileFact) bool { return x.Item == item })
		return i >= 0 && !f.Files[i].InSync
	}
	sysctlText, dropinText := "", ""
	if file(setup.ItemBuffers) {
		sysctlText = setup.SysctlText()
	}
	if file(setup.ItemMounts) {
		dropinText, _ = setup.MountsDropInText(f.Root)
	}
	raise := slices.ContainsFunc(f.Buffers, func(b setup.BufferFact) bool { return b.Low })
	give := !a.upgrade && f.RootAction != ""
	// A restart only for what Docker has to take, as read under the lock: daemon.json put by
	// this run, or a change it has not taken yet.
	restart := a.restart && (len(daemonText) > 0 || f.Pending)
	switch {
	case a.restart && !restart:
		report.Info("Docker is not restarted: it has nothing new to take from daemon.json")
	case restart && a.idle && f.Running > 0:
		restart = false
		report.Info("Docker is not restarted, with containers running: it takes daemon.json at its next restart")
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
		for _, base := range deployEnvBases(f.Root) {
			l, err := os.OpenFile(filepath.Join(base, ".lock"), os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return failed(err.Error())
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
	queued := false
	if len(daemonText) > 0 || sysctlText != "" || dropinText != "" || raise || give || restart {
		after := ""
		if restart {
			after = "restart"
		}
		err := rootSetupRun(h, f.Root, daemonText, sysctlText, dropinText, raise, give, after, before, rd.flags, &out)
		// A restart asked may take the run's container down before its words reach here: the
		// marker the run wrote for it says it was asked, and the bounded wait tells whether it
		// came.
		marker, _ := os.ReadFile(h.path(setup.RestartMarker))
		asked := restart && (hasLine(out.String(), remote.SetupAsked) || before != "" && strings.TrimSpace(string(marker)) == before)
		queued = asked && !hasLine(out.String(), remote.SetupNoSystem)
		sayRootLines(out.String(), putNames, f.Root)
		if err != nil && !asked {
			return failed(err.Error())
		}
	}
	// The record follows the file, whatever the restart does next.
	if recorded || (!rd.exists && !a.upgrade) {
		if err := setup.WriteRecord(record, next); err != nil {
			return failed(err.Error())
		}
		if !rd.exists {
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
			if rootSetupRun(h, f.Root, nil, "", "", false, false, "mark", before, rd.flags, &marked) == nil {
				report.Fail(said)
				sayRootLines(marked.String(), nil, f.Root)
			} else {
				said.Details = []string{"Docker does not answer, so nothing was flagged for a reboot: it takes daemon.json whenever it starts again"}
				report.Fail(said)
			}
			return 1
		}
	}

	// The metadata firewall, by the firewall's own root run, as its state says; --upgrade
	// only loads again the kind loaded (setup.FirewallState.Upgrade).
	fwRan := false
	if fw := f.Firewall; fw != nil && fw.State.Changes() {
		if a.upgrade && !fw.State.Upgrade() {
			report.Warn("--upgrade leaves the metadata firewall to --server-setup: " + firewallLine(fw))
		} else {
			if err := applyFirewall(h, fw); err != nil {
				return failed(err.Error(), report.Details(err)...)
			}
			fwRan = true
		}
	}

	// The checks again: what was applied must read in sync now.
	again, err := readSetup(h, readHostDocker(h))
	if err != nil {
		return failed(err.Error())
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
		return failed("what was applied did not hold", left...)
	}
	// An apply with no failure notes the devopsy that made it, which --upgrade applies once.
	if again.record.Version != version {
		r := again.record
		r.Version = version
		if err := setup.WriteRecord(record, r); err != nil {
			return failed(err.Error())
		}
	}
	if again.facts.Pending && !queued && !hasLine(out.String(), remote.SetupMarked) {
		report.Info("Docker has yet to take daemon.json: it does at its next restart")
	}
	if dropinText != "" && !restart {
		report.Info("Docker waits for the release root's mounts from its next start on")
	}
	return 0
}

// refusalWords is how every command says a refusal of the setup's: its item and message, its
// fix.
func refusalWords(r setup.Refusal) (string, []report.Remedy) {
	return r.Item + ": " + r.Message, fixWords(r.Fix)
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

// fixWords is a refusal's fix as a remedy, none when it has none.
func fixWords(fix string) []report.Remedy {
	if fix == "" {
		return nil
	}
	return []report.Remedy{report.Fix(fix)}
}

// rootSetupRun runs remote.SetupRootScript as root on the host (setupRun), its words into out,
// the docker client's own dropped once the restart is queued.
func rootSetupRun(h setupHost, root string, daemon []byte, sysctl, dropin string, raise, give bool, after, invocation string, flags []string, out *bytes.Buffer) error {
	q := &queuedLines{out: out}
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return ""
	}
	owner := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	run := setupRun(h.root, owner, flag(give), root, string(daemon), sysctl, dropin, flag(raise), after, invocation, strings.Join(flags, "\n"))
	run.Stderr = quietQueued{q}
	return run.run(q, func(err error) error { return fmt.Errorf("the setup's run as root: %v", err) })
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

// hasLine says whether out holds the line word, alone or first.
func hasLine(out, word string) bool {
	for _, l := range strings.Split(out, "\n") {
		if l == word || strings.HasPrefix(l, word+" ") {
			return true
		}
	}
	return false
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
			report.Info("Docker takes daemon.json at its next restart: " + remote.RebootRequired + " is raised")
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
// the address named it). A project user checks nothing: its tree is --project-users add's,
// and its Docker the door.
func runServerReady(args []string) int {
	if len(args) != 1 {
		return fail("usage: devopsy " + remote.ServerReadyFlag + " <server>")
	}
	u, err := user.Current()
	projectUser := err == nil && inDoorGroup(u.Username)
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
	return 0
}

// serverReady is what refuses a release on this server: the tools, the release root; nothing
// for a project user (projectUser), whose tree --project-users add made and whose Docker is
// the door.
func serverReady(root, ref string, projectUser bool) []error {
	if projectUser {
		return nil
	}
	var errs []error
	if t := toolsRefusal(); t != nil {
		errs = append(errs, &report.Failure{Msg: t.Message + ": nothing released", Remedies: fixWords(t.Fix)})
	}
	if err := releaseRootReady(root, "nothing released", ref); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// releaseRootReady refuses a release root that is not a directory of this user's
// (releaseRootCheck), what goes undone said in nothing, with the fix: --server-setup on ref
// (in words when "") when it makes or gives the root, else the check's own words.
func releaseRootReady(root, nothing, ref string) error {
	action, refusal := releaseRootCheck(root, os.Getuid())
	setUp := report.Fix("set the server up with --server-setup first, which makes it the deploy user's")
	if ref != "" {
		setUp = report.Fix("set the server up:", "devopsy @"+ref+" --server-setup")
	}
	switch {
	case refusal != nil:
		return &report.Failure{Msg: refusal.Message + ": " + nothing, Remedies: fixWords(refusal.Fix)}
	case action != "":
		return &report.Failure{Msg: rootWords(root, action) + ": " + nothing, Remedies: []report.Remedy{setUp}}
	}
	return nil
}

// applyFirewall loads the metadata firewall as devopsy loads it, or removes it (fw's state),
// as root through the firewall's own run, then reads the chains back, as root, for what they
// lack.
func applyFirewall(h setupHost, fw *setup.FirewallFact) error {
	script := remote.FirewallInstall(fw.Door)
	if fw.State == setup.FirewallRemove {
		script = remote.FirewallRemove()
	}
	run := firewallRun(script, true)
	run.Args = []string{h.root}
	var out bytes.Buffer
	if err := run.run(&out, func(err error) error { return fmt.Errorf("the firewall's run as root: %v", err) }); err != nil {
		return err
	}
	if fw.State == setup.FirewallRemove {
		report.Info("removed devopsy's firewall (firewall: metadata: allow): containers reach the cloud's metadata services")
		return nil
	}
	wrong := remote.ContainersFirewallWrong(out.String())
	if fw.Door {
		var err error
		if wrong, err = remote.FirewallWrong(out.String()); err != nil {
			return err
		}
	}
	if len(wrong) > 0 {
		return &report.Failure{Msg: "the firewall did not load as devopsy's", Details: wrong}
	}
	report.Info("loaded devopsy's firewall: containers kept off the cloud's metadata services, DNS there aside")
	return nil
}
