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
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/howto"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
	"github.com/hanoii/devopsy-cli/internal/uids"
	"github.com/hanoii/devopsy-cli/internal/update"
	"github.com/hanoii/devopsy-cli/internal/vocab"
)

// serverInfoWarned is --server-info's exit code when it warned: 0 says
// nothing needs attention, anything else that it could not tell.
const serverInfoWarned = 3

// The thresholds of a disk's warning: this much used, or this little
// free.
const (
	diskUsedWarn = 90
	diskFreeWarn = 5_000_000_000
)

// serverTools are the tools a server needs beyond Docker and devopsy
// (internals/releases.md), with what goes without each; a release refuses a server without
// a required one (toolsRefusal), and the others degrade. The door's (door) are project users',
// which --project-users add refuses without; an sbin one is looked for in /usr/sbin and /sbin
// too, outside a deploy user's PATH. Every lookup of them is missingTools.
var serverTools = []serverTool{
	{name: "tar", without: "no release can be uploaded", required: true},
	{name: "flock", without: "nothing that changes an environment runs", required: true},
	{name: "setsid", without: "the run step runs plainly, and a fetch's live progress shows only before it"},
	{name: "script", without: "builds and pulls show no live progress"},
	{name: "mkfifo", without: "no sync runs"},
	{name: "tail", without: "only the traps protect a release after its switch (a tail that answers --version)"},
	{name: "sudo", without: "no project user can be added, nor reach Docker through the door", door: true},
	{name: "visudo", without: "no project user can be added: the door's sudo rules are checked with it", door: true, sbin: true},
	{name: "setfacl", without: "no project user can be added, nor given its files", door: true},
}

// serverTool is one of serverTools: its name, what goes without it, whether a release refuses
// without it, whether project users need it, whether it lives in sbin.
type serverTool struct {
	name, without string
	required      bool
	door, sbin    bool
}

// Which tools a lookup takes (missingTools): all of them, the required ones, the door's.
func anyTool(serverTool) bool        { return true }
func requiredTool(t serverTool) bool { return t.required }
func doorTool(t serverTool) bool     { return t.door }

// missingTools are the tools of serverTools that which takes and that are not there, in
// order: one not in PATH, nor in /usr/sbin or /sbin for an sbin one, and a tail that does not
// answer --version, as the scripts tell (followOutput: GNU's, or uutils').
func missingTools(which func(serverTool) bool) []string {
	var out []string
	for _, t := range serverTools {
		if which(t) && !toolThere(t) {
			out = append(out, t.name)
		}
	}
	return out
}

// toolThere says whether t is there, as missingTools looks.
func toolThere(t serverTool) bool {
	if _, err := exec.LookPath(t.name); err == nil {
		return t.name != "tail" || exec.Command("tail", "--version").Run() == nil
	}
	if t.sbin {
		for _, dir := range []string{"/usr/sbin", "/sbin"} {
			if fi, err := os.Stat(filepath.Join(dir, t.name)); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
				return true
			}
		}
	}
	return false
}

// toolMissingWords is how --server-info and --server-setup say a tool of serverTools missing
// (name), "" for a name that is none of them, and whether it is the door's, which matters
// only on a server with project users.
func toolMissingWords(name string) (msg string, door bool) {
	for _, t := range serverTools {
		if t.name == name {
			return "the server has no " + t.name + ": " + t.without, t.door
		}
	}
	return "", false
}

// toolsFixText is the way out of a tool missing, its package, by the tools topic's steps
// (howto.Tools): the tools' refusal's fix (fixWords), a missing one's where it is warned of,
// a hint where the door's is only noted. The refusal's and the warnings' are one text, so a
// run says it once after the last of them (sharedLater).
const toolsFixText = "install what is missing with the system's packages"

// toolsFix is toolsFixText as a fix, as fixWords words the tools' refusal's.
var toolsFix = stepsFix(toolsFixText, howto.Tools)

// doorToolsFixText is the way out of a door's tool missing (sudo, visudo, setfacl), which
// only project users need, so by their topic's steps (howto.ProjectUsers), never the tools':
// a fix where it is warned of or refused, a hint where it is only noted.
const doorToolsFixText = "install sudo and acl with the system's packages"

// toolFix is the fix of a missing tool: the door's by doorToolsFixText, any other by toolsFix.
func toolFix(door bool) report.Remedy {
	if door {
		return stepsFix(doorToolsFixText, howto.ProjectUsers)
	}
	return toolsFix
}

// doorFacts are the server's door, nil with none: its space's mount, its users, whether sshd
// waits for a reboot to take it, and the host's listeners from read, the root read of the
// firewall ("" when none was made).
func doorFacts(read string) *remote.DoorFacts {
	d, err := remote.ServerDoor()
	if err != nil || d == nil || d.Space == "" {
		return nil
	}
	f := &remote.DoorFacts{Space: d.Space, Users: len(d.Users)}
	if err := spaceGuarded(d.Space); err != nil {
		f.Unguarded = err.Error()
	}
	if _, err := os.Stat(remote.DoorRebootMarker); err == nil {
		f.Reboot = true
	}
	if read != "" {
		f.Listeners = remote.DoorListeners(read)
	}
	return f
}

// procMounts is the file a host's mounts are read from, /proc's options among them, under the
// host root (setupHost) in tests.
const procMounts = "/proc/mounts"

// mountEntry is one line of a /proc/mounts text: where it is mounted, its type and options.
type mountEntry struct {
	point, fstype string
	opts          []string
}

// parseMounts is a /proc/mounts text's mounts, in its order, each mount point as the kernel
// escapes it (\040 a space, \011 a tab, \012 a newline, \134 a backslash) read back.
func parseMounts(text string) []mountEntry {
	var ms []mountEntry
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		ms = append(ms, mountEntry{point: unescapeMount(f[1]), fstype: f[2], opts: strings.Split(f[3], ",")})
	}
	return ms
}

// unescapeMount reads back a field of /proc/mounts, whose \ooo is the byte of that octal.
func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// mountOf is the mount path p is on: the last listed of the longest mount point holding it,
// which a path there reaches.
func mountOf(ms []mountEntry, p string) (mountEntry, bool) {
	var found mountEntry
	ok := false
	for _, m := range ms {
		in := m.point == "/" || p == m.point || strings.HasPrefix(p, strings.TrimSuffix(m.point, "/")+"/")
		if in && (!ok || len(m.point) >= len(found.point)) {
			found, ok = m, true
		}
	}
	return found, ok
}

// opt is the mount's option key: its value after = ("" for a flag), and whether it is there.
func (m mountEntry) opt(key string) (string, bool) {
	for _, o := range m.opts {
		if o == key {
			return "", true
		}
		if v, ok := strings.CutPrefix(o, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// procMount is the proc mount on /proc in a /proc/mounts text, false with none.
func procMount(mounts string) (mountEntry, bool) {
	m, ok := mountOf(parseMounts(mounts), "/proc")
	return m, ok && m.point == "/proc" && m.fstype == "proc"
}

// procHidePID is /proc's hidepid in mounts, a /proc/mounts text, as the kernel prints it
// (invisible, noaccess or ptraceable since Linux 5.8, 2 or 1 before it), "off" for a /proc
// with none, which the kernel then leaves out, and "" with no proc mount on /proc. The last
// mount on /proc is the one a path there reaches.
func procHidePID(mounts string) string {
	m, ok := procMount(mounts)
	if !ok {
		return ""
	}
	if v, ok := m.opt("hidepid"); ok {
		return v
	}
	return "off"
}

// processesHidden says whether hidepid keeps a user from what other users' processes run, by
// the one judgment (setup.Hiding).
func processesHidden(hidepid string) bool { return setup.Hiding(hidepid) }

// processesShown says whether hidepid, as procHidePID reads it, shows a project user every other
// user's processes; "" is unknown (an older server's facts, no /proc mount), never said.
func processesShown(hidepid string) bool { return hidepid != "" && !processesHidden(hidepid) }

// projectUsersSetup is what --project-users setup sets offered as kind (report.Fix, else
// report.Hint): the setup on ref, the server as typed, in words where it is "", then topic's
// steps as a hint for doing it by hand.
func projectUsersSetup(kind func(string, ...string) report.Remedy, ref string, topic howto.Name) []report.Remedy {
	cmd := ""
	if ref != "" {
		cmd = "devopsy @" + ref + " --project-users setup"
	}
	return []report.Remedy{offer(kind, "set the server up for project users:", cmd, "run --project-users setup for this server, as its deploy user"),
		byHandHint("set it up", topic)}
}

// processesShownWarning is the one wording, in --server-info and --project-users add, of a host
// that shows project users every other user's processes (processesShown), its fixes on ref
// (projectUsersSetup).
func processesShownWarning(ref string) (msg, detail string, fixes []report.Remedy) {
	return "/proc shows project users every other user's processes (no hidepid)",
		"their command lines can hold a secret",
		projectUsersSetup(report.Fix, ref, howto.Processes)
}

// processesShownNote is the same on a server with no project user, where it only matters once
// one is added: the setup a hint.
func processesShownNote(ref string) (msg string, hints []report.Remedy) {
	return "/proc shows every user's processes (no hidepid), which matters once the server has project users",
		projectUsersSetup(report.Hint, ref, howto.Processes)
}

// warnProcesses says on l what --project-users setup would of /proc at h (procHidingAt, groups
// the group lookup): processesShownWarning where it shows project users every other user's
// processes, the setup's refusal where it hides them another way the setup refuses, in the
// setup's words, nothing where it hides them as the setup leaves them or cannot tell.
func warnProcesses(l *report.Logger, h setupHost, ref string, groups groupLookup) {
	p, refused := procHidingAt(h, groups)
	switch {
	case processesShown(p.HidePID):
		msg, detail, fixes := processesShownWarning(ref)
		l.Say(report.WarnLevel, msg, []string{detail}, fixes...)
	case refused != nil:
		l.Say(report.WarnLevel, refused.Message, nil, fixWords(*refused)...)
	}
}

// doorFirewall runs script as root on the host with its network, through the util image
// (writing the host's files when write), and says what of the project users' firewall the
// host lacks from what it prints (remote.FirewallRead): the deploy user can neither
// read nor change it.
func doorFirewall(script string, write bool) ([]string, error) {
	_, wrong, err := firewallJudged(context.Background(), firewallRun(script, write), true, true)
	return wrong, err
}

// runServerFacts implements the hidden --server-facts on a server: its
// facts as one JSON document on stdout (remote.ServerInfo).
func runServerFacts() int {
	info, err := serverFacts()
	if err != nil {
		return failErr(err)
	}
	// Last, so only the way to the operator is between it and the
	// operator's clock.
	info.Time = time.Now().UnixNano()
	out, err := json.Marshal(info)
	if err != nil {
		return failErr(err)
	}
	fmt.Println(string(out))
	return 0
}

// serverFacts is this machine's facts as a server. It reads and tries,
// never changes anything but a file of its own it removes, and takes no
// lock.
func serverFacts() (remote.ServerInfo, error) {
	r, err := remote.ReleaseSettings()
	if err != nil {
		return remote.ServerInfo{}, err
	}
	info := remote.ServerInfo{
		Version: version,
		Root:    r.Root, Keep: r.Keep, MaxKeep: r.MaxKeep,
		SnapshotRoot: r.Snapshots.Root, SnapshotMaxKeep: r.Snapshots.MaxKeep, MirrorsOff: r.Snapshots.MirrorsOff(),
		Arch: runtime.GOARCH, CPUs: runtime.NumCPU(),
	}
	systemFacts(&info)
	h := setupHost{root: os.Getenv(remote.HostRootEnv)}
	// The firewall's chains read as root, for the setup's judgment and the door's listeners,
	// an install's even where the setup cannot be read; a project user's, whose server's setup
	// is its admin's and whose docker is the door, none of it nor of Docker but what the door
	// tells (projectUserFacts): the door would refuse Docker's reads and log each.
	read := ""
	if asProjectUser() {
		info.Docker.Door = true
		projectUserFacts(&info, askDoorFirewall)
	} else {
		var p setup.ProcFact
		p, info.ProcRefused = procHidingAt(h, groupOf)
		info.HidePID = p.HidePID
		hd := readHostDocker(h)
		info.Docker = hd.facts
		info.Docker.ClientProxies = clientProxies(dockerConfigDir())
		// What the login cannot read of another deploy user, as root where the util image is
		// there already, else said unread.
		hr := h
		hr.rootRead = rootReadOther(h, false)
		if rd, err := readSetup(hr, hd); err == nil {
			info.Setup = &rd.facts
			read = readChains(rd.facts.Firewall, rd.docker, false)
		} else if kind, _ := firewallHere(h); kind != setup.NoFirewall {
			read = readChains(&setup.FirewallFact{Loaded: kind}, dockerHere(hd.facts), false)
		}
	}
	info.Door = doorFacts(read)
	info.Disks = diskFacts(info.Root, info.Docker.RootDir, containerdRoot(info.Docker), info.SnapshotRoot, true)
	info.Missing = missingTools(anyTool)
	trees := doorTrees(info.Root)
	if info.Containers, err = hostContainers("docker", releaseDirs(info.Root, trees)); err != nil {
		info.ContainersUnread = err.Error()
	}
	for _, who := range trees {
		info.DoorProjects = append(info.DoorProjects, strings.TrimPrefix(who, "devopsy-"))
	}
	slices.Sort(info.DoorProjects)
	if info.Environments, err = environments(info.Root, trees, func(who, tree string) []remote.Environment {
		return treeEnvironments(who, tree, doorTreeWait)
	}, ownedByMe); err != nil {
		info.EnvironmentsUnread = err.Error()
	}
	info.UIDsDir, info.UIDs = uidFacts(info.Root)
	if !asProjectUser() {
		d, _ := remote.ServerDoor()
		leftFacts(&info, r, d, trees)
	}
	return info, nil
}

// releaseDirs are where a release's compose project works: the release root, by its path and
// its links resolved, and each project user's tree.
func releaseDirs(root string, trees map[string]string) []string {
	dirs := []string{root}
	if real, err := filepath.EvalSymlinks(root); err == nil && real != root {
		dirs = append(dirs, real)
	}
	return append(dirs, slices.Sorted(maps.Keys(trees))...)
}

// systemFacts fills the machine's facts from /etc and /proc; what is
// missing stays empty.
func systemFacts(info *remote.ServerInfo) {
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		info.OS = osRelease(string(data))
	}
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		info.Kernel = strings.TrimSpace(string(data))
	}
	cmaAreas := int64(0)
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		m := meminfo(string(data))
		info.MemTotal, info.MemAvailable, info.SwapTotal = m["MemTotal"], m["MemAvailable"], m["SwapTotal"]
		cmaAreas = m["CmaTotal"]
	}
	// Kexec HandOver's scratch is CMA pageblocks no CMA area counts
	// (init_cma_reserved_pageblock): in each zone's cma, not in
	// CmaTotal. /proc/pagetypeinfo would say it too, but it is root's.
	if data, err := os.ReadFile("/proc/zoneinfo"); err == nil {
		info.KHOScratch = max(0, zoneCMA(string(data))*int64(os.Getpagesize())-cmaAreas)
	}
	if data, err := os.ReadFile("/sys/kernel/kexec_crash_size"); err == nil {
		info.CrashKernel, _ = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	}
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(data)); len(f) >= 3 {
			info.Load = strings.Join(f[:3], " ")
		}
	}
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(data)); len(f) > 0 {
			info.Uptime, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if _, err := os.Stat(remote.RebootRequired); err == nil {
		info.RebootRequired = true
		if data, err := os.ReadFile(remote.RebootRequired + ".pkgs"); err == nil {
			for _, p := range strings.Fields(string(data)) {
				if !slices.Contains(info.RebootPackages, p) {
					info.RebootPackages = append(info.RebootPackages, p)
				}
			}
		}
	}
	if out, err := exec.Command("timedatectl", "show", "-p", "NTP", "-p", "NTPSynchronized").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
			switch k {
			case "NTP":
				info.NTPService = v
			case "NTPSynchronized":
				info.NTP = v
			}
		}
	}
	if data, err := os.ReadFile("/sys/module/apparmor/parameters/enabled"); err == nil {
		info.AppArmor = strings.TrimSpace(string(data)) == "Y"
	}
	// In sbin, which a deploy user's PATH lacks.
	for _, p := range []string{"/usr/sbin/apparmor_parser", "/sbin/apparmor_parser"} {
		if _, err := os.Stat(p); err == nil {
			info.AppArmorParser = true
		}
	}
}

// osRelease is os-release's PRETTY_NAME, else its NAME and VERSION.
func osRelease(data string) string {
	values := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		if u, err := strconv.Unquote(v); err == nil {
			v = u
		} else {
			v = strings.Trim(v, `'"`)
		}
		values[k] = v
	}
	if values["PRETTY_NAME"] != "" {
		return values["PRETTY_NAME"]
	}
	return strings.TrimSpace(values["NAME"] + " " + values["VERSION"])
}

// meminfo is /proc/meminfo's values, in bytes.
func meminfo(data string) map[string]int64 {
	m := map[string]int64{}
	for _, line := range strings.Split(data, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 1 && f[1] == "kB" {
			n *= 1024
		}
		m[k] = n
	}
	return m
}

// zoneCMA is /proc/zoneinfo's cma pages, every zone's.
func zoneCMA(data string) int64 {
	var pages int64
	for _, line := range strings.Split(data, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "cma" {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			pages += n
		}
	}
	return pages
}

// hostDocker is the host's Docker as devopsy reads it once, for --server-info's facts and the
// setup's: docker info, its compose and buildx, and daemon.json as the file holds it
// (daemonErr why it could not be read; a missing file is none).
type hostDocker struct {
	facts     remote.DockerFacts
	daemon    []byte
	daemonErr error
}

// readHostDocker asks Docker of itself (dockerFacts), and reads the daemon's configuration
// file, its log-opts as --server-info shows them.
func readHostDocker(h setupHost) hostDocker {
	hd := hostDocker{facts: dockerFacts()}
	hd.daemon, hd.daemonErr = readDaemonFile(h)
	if hd.daemonErr == nil {
		var conf struct {
			LogOpts map[string]string `json:"log-opts"`
		}
		if len(bytes.TrimSpace(hd.daemon)) == 0 || json.Unmarshal(hd.daemon, &conf) == nil {
			hd.facts.LogOptsKnown, hd.facts.LogOpts = true, conf.LogOpts
		}
	}
	return hd
}

// readDaemonFile is the host's daemon.json, nil with no error when there is none.
func readDaemonFile(h setupHost) ([]byte, error) {
	data, err := os.ReadFile(h.path(setup.DaemonFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// containerdRoot is where d's images and layers are when it uses the
// containerd image store, the system containerd's root, not Docker's;
// else empty.
func containerdRoot(d remote.DockerFacts) string {
	if d.Containerd {
		return "/var/lib/containerd"
	}
	return ""
}

// dockerFacts is what Docker answers of itself, as every check of it reads it (dockerAnswers):
// docker info, and its compose's and buildx's versions, "" for a plugin that does not answer,
// the three asked at once.
func dockerFacts() remote.DockerFacts {
	var d remote.DockerFacts
	var compose, buildx string
	var wg sync.WaitGroup
	wg.Go(func() { d = dockerInfo() })
	wg.Go(func() {
		if out, err := exec.Command("docker", "compose", "version", "--short").Output(); err == nil {
			compose = strings.TrimSpace(string(out))
		}
	})
	wg.Go(func() {
		if out, err := exec.Command("docker", "buildx", "version").Output(); err == nil {
			buildx = versionOf(string(out))
		}
	})
	wg.Wait()
	d.Compose, d.Buildx = compose, buildx
	return d
}

// dockerInfo is what docker info says, alone.
func dockerInfo() remote.DockerFacts { return dockerInfoWith(context.Background(), "docker") }

// dockerInfoWith is dockerInfo through docker, within ctx.
func dockerInfoWith(ctx context.Context, docker string) remote.DockerFacts {
	var d remote.DockerFacts
	out, err := exec.CommandContext(ctx, docker, "info", "--format", "{{json .}}").Output()
	if err != nil {
		d.Error = "docker info: " + cmdError(err)
	} else if err := parseDockerInfo(out, &d); err != nil {
		d.Error = "docker info: " + err.Error()
	}
	return d
}

// parseDockerInfo reads docker info's JSON into d.
func parseDockerInfo(out []byte, d *remote.DockerFacts) error {
	var info struct {
		ServerVersion   string
		Driver          string
		DriverStatus    [][2]string
		DockerRootDir   string
		SecurityOptions []string
		CgroupVersion   string
		LoggingDriver   string
		HTTPProxy       string `json:"HttpProxy"`
		HTTPSProxy      string `json:"HttpsProxy"`
		ServerErrors    []string
		Name            string
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return err
	}
	if len(info.ServerErrors) > 0 {
		return errors.New(strings.Join(info.ServerErrors, "; "))
	}
	running, err := setup.InfoVisible(out)
	if err != nil {
		return err
	}
	d.Version, d.Driver, d.RootDir, d.LogDriver, d.Name, d.Running = info.ServerVersion, info.Driver, info.DockerRootDir, info.LoggingDriver, info.Name, running
	if info.CgroupVersion != "" {
		d.Cgroup = "v" + info.CgroupVersion
	}
	for _, s := range info.DriverStatus {
		if s[0] == "driver-type" && s[1] == containerdStore {
			d.Containerd = true
		}
	}
	// name=seccomp,profile=builtin: the name alone.
	for _, o := range info.SecurityOptions {
		name, _, _ := strings.Cut(o, ",")
		d.Security = append(d.Security, strings.TrimPrefix(name, "name="))
	}
	d.Proxy = cmp.Or(info.HTTPSProxy, info.HTTPProxy)
	return nil
}

// dockerConfigDir is where the Docker client of this user reads its config.json: DOCKER_CONFIG,
// else ~/.docker.
func dockerConfigDir() string {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".docker")
}

// clientProxies are the proxies config.json in dir gives the containers its client creates,
// those naming one, by the daemon they are for; none when the file is missing or unreadable,
// which gives containers none either.
func clientProxies(dir string) []remote.ClientProxy {
	file := filepath.Join(dir, "config.json")
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var c struct {
		Proxies map[string]struct {
			HTTPProxy  string `json:"httpProxy"`
			HTTPSProxy string `json:"httpsProxy"`
			AllProxy   string `json:"allProxy"`
			NoProxy    string `json:"noProxy"`
		} `json:"proxies"`
	}
	if json.Unmarshal(data, &c) != nil {
		return nil
	}
	var ps []remote.ClientProxy
	for _, k := range sortedKeysOf(c.Proxies) {
		p := c.Proxies[k]
		if proxy := cmp.Or(p.HTTPSProxy, p.HTTPProxy, p.AllProxy); proxy != "" {
			ps = append(ps, remote.ClientProxy{For: k, File: file, Proxy: proxy, NoProxy: p.NoProxy})
		}
	}
	return ps
}

// loopbackProxied is what of localhost, 127.0.0.1 and ::1 a noProxy list leaves to the proxy, as
// curl and libcurl match it: each entry trimmed, its case and a leading dot ignored, * naming
// every host, a range naming its addresses. Go never proxies them; curl does unless named.
func loopbackProxied(noProxy string) []string {
	var left []string
	for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
		named := false
		for _, e := range strings.Split(noProxy, ",") {
			e = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(e)), ".")
			if _, n, err := net.ParseCIDR(e); err == nil {
				named = named || n.Contains(net.ParseIP(h))
				continue
			}
			named = named || e == "*" || e == h || strings.Trim(e, "[]") == h
		}
		if !named {
			left = append(left, h)
		}
	}
	return left
}

// cmdError is a command's error with what it said on stderr.
func cmdError(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return err.Error()
}

// diskFacts is the filesystems of the release root, Docker's data,
// containerd's (empty: not its image store), the snapshot root and /,
// one per device, each path where it is: the
// nearest existing directory above it, for a root not made yet. reflink
// tries a clone in the release root (a file made and removed there).
func diskFacts(root, docker, containerd, snapshots string, reflink bool) []remote.Disk {
	var disks []remote.Disk
	var devs []uint64
	add := func(what, path string) {
		if path == "" {
			return
		}
		at := path
		var fi os.FileInfo
		var err error
		for {
			if fi, err = os.Stat(at); err == nil || at == "/" || at == "." {
				break
			}
			at = filepath.Dir(at)
		}
		label := what + "=" + path
		if err != nil {
			disks = append(disks, remote.Disk{Paths: []string{label}, Error: err.Error()})
			devs = append(devs, ^uint64(0))
			return
		}
		dev := uint64(fi.Sys().(*syscall.Stat_t).Dev)
		if i := slices.Index(devs, dev); i >= 0 {
			disks[i].Paths = append(disks[i].Paths, label)
			return
		}
		d := remote.Disk{Paths: []string{label}, Mount: mountPoint(at, dev)}
		if d.Type, d.Size, d.Free, d.Used, err = statDisk(at); err != nil {
			d.Error = err.Error()
		}
		disks = append(disks, d)
		devs = append(devs, dev)
	}
	add("releases", root)
	if reflink && len(disks) > 0 && disks[0].Error == "" {
		disks[0].Reflinks = tryReflink(root)
	}
	add("docker", docker)
	add("containerd", containerd)
	add("snapshots", snapshots)
	add("/", "/")
	return disks
}

// mountPoint is where the filesystem of dev, which path is on, is
// mounted: the highest directory above path, its links resolved, still
// on dev, "" when one on the way does not read.
func mountPoint(path string, dev uint64) string {
	at, err := filepath.EvalSymlinks(path)
	if err == nil {
		at, err = filepath.Abs(at)
	}
	if err != nil {
		return ""
	}
	for at != "/" {
		up := filepath.Dir(at)
		fi, err := os.Stat(up)
		if err != nil {
			return ""
		}
		if uint64(fi.Sys().(*syscall.Stat_t).Dev) != dev {
			return at
		}
		at = up
	}
	return at
}

// tryReflink tries a clone in dir: yes, no, or empty when it could not
// tell (no file could be made there, a system without clones).
func tryReflink(dir string) string {
	src, err := os.CreateTemp(dir, ".devopsy-reflink.*")
	if err != nil {
		return ""
	}
	defer os.Remove(src.Name())
	defer src.Close()
	if _, err := src.WriteString("devopsy\n"); err != nil {
		return ""
	}
	dst, err := os.CreateTemp(dir, ".devopsy-reflink.*")
	if err != nil {
		return ""
	}
	defer os.Remove(dst.Name())
	defer dst.Close()
	err = cloneFile(int(dst.Fd()), int(src.Fd()))
	switch {
	case err == nil:
		return "yes"
	case errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.EXDEV), errors.Is(err, syscall.EINVAL), errors.Is(err, syscall.ENOTTY):
		return "no"
	}
	return ""
}

// hostContainers is every container, any state, with its compose
// project and service, state and health, its role and exports, its image
// and when it exited, as docker (its command) lists them, each released
// when compose worked under one of dirs: the one inspect of every
// container --server-info makes. A list docker failed to give is an
// error, with what it gave: unknown, never none.
func hostContainers(docker string, dirs []string) ([]remote.HostContainer, error) {
	out, err := exec.Command(docker, "ps", "-aq", "--no-trunc").Output()
	if err != nil {
		return nil, dockerFailed("docker ps", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil, nil
	}
	out, err = inspectAll(docker, ids, "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}} {{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}} {{.Image}} {{json .State.FinishedAt}} {{json .Config.Labels}}")
	return parseHostContainers(out, dirs), err
}

// inspectAll is docker (its command) run with args, an inspect, and every one of ids after
// them, its output: one gone since its listing (a run --rm, a util container) fails it, which
// printed every other all the same, and is none (onlyGone); any other failure is an error,
// with what it printed.
func inspectAll(docker string, ids []string, args ...string) ([]byte, error) {
	cmd := exec.Command(docker, slices.Concat(args, ids)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && !onlyGone(stderr.String()) {
		what := "docker " + strings.Join(args[:slices.Index(args, "inspect")+1], " ")
		return out, errors.New(what + ": " + cmp.Or(strings.TrimSpace(stderr.String()), err.Error()))
	}
	return out, nil
}

// onlyGone says whether docker's stderr says only of objects not there: "No such container"
// (the daemon), "No such object" (the CLI).
func onlyGone(stderr string) bool {
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		if !strings.Contains(strings.ToLower(line), "no such ") {
			return false
		}
	}
	return true
}

// dockerFailed is the error of what, a docker command run for its output, with its stderr.
func dockerFailed(what string, err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
		return errors.New(what + ": " + string(bytes.TrimSpace(ee.Stderr)))
	}
	return errors.New(what + ": " + err.Error())
}

// parseHostContainers reads lines of "<state> <exit code> <health or ->
// <image id> <finish time as JSON> <labels as JSON>": a container whose
// compose working directory is under one of dirs is a release's
// (Released).
func parseHostContainers(out []byte, dirs []string) []remote.HostContainer {
	var cs []remote.HostContainer
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), " ", 6)
		if len(f) < 6 {
			continue
		}
		var labels map[string]string
		if json.Unmarshal([]byte(f[5]), &labels) != nil {
			continue
		}
		c := remote.HostContainer{State: f[0], Project: labels["com.docker.compose.project"], Service: labels["com.docker.compose.service"],
			OneOff: labels["com.docker.compose.oneoff"] == "True", Image: f[3]}
		_ = json.Unmarshal([]byte(f[4]), &c.Finished)
		if wd := labels["com.docker.compose.project.working_dir"]; wd != "" {
			c.Released = slices.ContainsFunc(dirs, func(d string) bool { return within(wd, d) })
		}
		c.ExitCode, _ = strconv.Atoi(f[1])
		if f[2] != "-" {
			c.Health = f[2]
		}
		for k, v := range labels {
			if k == cli.RoleLabel || strings.HasPrefix(k, cli.ExportPrefix) {
				if c.Labels == nil {
					c.Labels = map[string]string{}
				}
				c.Labels[k] = v
			}
		}
		cs = append(cs, c)
	}
	return cs
}

// environments is every environment under the release root,
// <project>/[<instance>/]<environment> (a directory with releases/),
// with its state lines (remote.EnvStatus). Not one a --destroy cut off
// while removing it left (<base>.rm.<pid>, removeBase). A project user's
// tree there (trees, by its path) is read as that user (readTree): the
// deploy user never reads a project's tree, where a link would show it
// its own files. Nor any project directory there it does not own (owned):
// a project user's whose entry is gone (--project-users remove keeps its
// Unix user and tree), or one the door's list names by another spelling.
// A directory it cannot read is an error, with every environment it read.
func environments(root string, trees map[string]string, readTree func(user, tree string) []remote.Environment, owned func(path string) bool) ([]remote.Environment, error) {
	ps, err := ownProjects(root, trees, owned)
	errs := []error{err}
	var envs []remote.Environment
	for _, p := range ps {
		got, err := projectEnvironments(root, p)
		envs = append(envs, got...)
		errs = append(errs, err)
	}
	// A project user's tree, in the release root or elsewhere (by default its own home).
	for _, tree := range slices.Sorted(maps.Keys(trees)) {
		got := readTree(trees[tree], tree)
		for i := range got {
			got[i].User = trees[tree]
		}
		envs = append(envs, got...)
	}
	return envs, errors.Join(errs...)
}

// ownProjects are the project directories in root the deploy user walks: none of a project
// user's tree (trees, by its path) and none it does not own (owned), whose files another user
// may change.
func ownProjects(root string, trees map[string]string, owned func(path string) bool) ([]string, error) {
	dirs, err := envDirs(root, root)
	var out []string
	for _, p := range dirs {
		if _, ok := trees[p]; !ok && owned(p) {
			out = append(out, p)
		}
	}
	return out, err
}

// deployEnvBases are the deploy user's environments' directories in root (ownProjects), with
// an error for a directory it could not read.
func deployEnvBases(root string) ([]string, error) {
	ps, err := ownProjects(root, doorTrees(root), ownedByMe)
	errs := []error{err}
	var out []string
	for _, p := range ps {
		bases, err := envBases(root, p)
		out = append(out, bases...)
		errs = append(errs, err)
	}
	return out, errors.Join(errs...)
}

// ownedByMe says whether path, links resolved, is this user's own.
func ownedByMe(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// envDirs are dir's directories, under the release root root, a link to
// one included: a project or an environment moved to another disk and
// linked back is one, as the scripts reach it. A link resolving to
// nothing is an error when a uid claim names an environment under it
// (danglingClaimed): an environment's disk not mounted, what is there
// would read as gone. Any other is a stray link, skipped: the root's top
// level is by default the deploy user's home. A dir not there, or no
// directory, holds none (a release root before its first release, a
// project removed since its root was read); one it cannot read, or an
// entry it cannot stat, is an error, with what it read.
func envDirs(root, dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil, nil
	}
	var out []string
	errs := []error{err}
	for _, e := range entries {
		if !projectDir(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		fi, err := os.Stat(path)
		switch {
		case err == nil && fi.IsDir():
			out = append(out, path)
		case errors.Is(err, os.ErrNotExist):
			if li, lerr := os.Lstat(path); lerr == nil && li.Mode()&os.ModeSymlink != 0 {
				errs = append(errs, danglingClaimed(root, path))
			}
		case err != nil:
			errs = append(errs, err)
		}
	}
	return out, errors.Join(errs...)
}

// danglingClaimed is the error of path, a link to no directory under the release root root,
// when a uid claim there names an environment under it (its project, and instance if any,
// path's names), an environment's disk not mounted, else nil. Claims that do not read leave
// it unknown, an error too.
func danglingClaimed(root, path string) error {
	unread := errors.New(path + " links to no directory, a disk not mounted perhaps")
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return unread
	}
	claims, err := uids.StoreAt(root).List()
	if err != nil {
		return fmt.Errorf("%w, and the uid claims do not read: %w", unread, err)
	}
	rel = filepath.ToSlash(rel)
	for _, c := range claims {
		if c.Bad != "" {
			continue
		}
		at := c.Owner.Project
		if c.Owner.Instance != "" {
			at += "/" + c.Owner.Instance
		}
		if c.Owner.Env != "" {
			at += "/" + c.Owner.Env
		}
		if at == rel || strings.HasPrefix(at, rel+"/") {
			return unread
		}
	}
	return nil
}

// projectEnvironments are the environments of the project at p under root, with an error for
// a directory of it it could not read.
func projectEnvironments(root, p string) ([]remote.Environment, error) {
	bases, err := envBases(root, p)
	var envs []remote.Environment
	for _, dir := range bases {
		rel, _ := filepath.Rel(root, dir)
		cmd := exec.Command("sh", "-c", remote.EnvStatus)
		cmd.Env = append(os.Environ(), "base="+dir)
		out, _ := cmd.Output()
		envs = append(envs, remote.Environment{Path: filepath.ToSlash(rel), Status: string(out)})
	}
	return envs, err
}

// envBases are the directories of the project at p's environments, [<instance>/]<environment>
// (a directory with releases/), under the release root root, with an error for a directory it
// could not read.
func envBases(root, p string) ([]string, error) {
	return envsUnder(root, p, func(dir string) bool {
		fi, err := os.Stat(filepath.Join(dir, "releases"))
		return err == nil && fi.IsDir()
	})
}

// envsUnder are the directories under the project at p in root, [<instance>/]<environment> as
// devopsy lays them out, that isEnv takes for an environment's (a release root's, a snapshot
// root's), an instance's looked into only when it is not one itself, with an error for a
// directory it could not read.
func envsUnder(root, p string, isEnv func(dir string) bool) ([]string, error) {
	var out []string
	dirs, err := envDirs(root, p)
	errs := []error{err}
	for _, e := range dirs {
		if isEnv(e) {
			out = append(out, e)
			continue
		}
		inner, err := envDirs(root, e)
		errs = append(errs, err)
		for _, i := range inner {
			if isEnv(i) {
				out = append(out, i)
			}
		}
	}
	return out, errors.Join(errs...)
}

// doorTrees are the project users' trees under root, each to its user.
func doorTrees(root string) map[string]string {
	d, err := remote.ServerDoor()
	if err != nil || d == nil {
		return nil
	}
	trees := map[string]string{}
	for who, u := range d.Users {
		tree := u.Tree
		if tree == "" {
			pu, err := user.Lookup(who)
			if err != nil {
				continue
			}
			tree = filepath.Join(pu.HomeDir, u.Project)
		}
		trees[filepath.Clean(tree)] = who
	}
	return trees
}

// doorTreeWait is how long --server-info waits for a project user's read of its tree.
const doorTreeWait = 30 * time.Second

// treeEnvironments are a project user's environments as that user reads them (--door-read
// environments), none when it cannot, or does not answer within (doorAnswer): only those
// whose path is the tree's project and names devopsy gives (envPathIn), since that user
// writes what the read answers, and an admin's --server-info offers commands with them.
func treeEnvironments(who, tree string, within time.Duration) []remote.Environment {
	out, err := doorAnswer(doorRead(who, "environments", tree), maxEnvironments, within)
	if err != nil {
		return nil
	}
	var envs []remote.Environment
	if json.Unmarshal(out, &envs) != nil {
		return nil
	}
	return slices.DeleteFunc(envs, func(e remote.Environment) bool { return !envPathIn(filepath.Base(tree), e.Path) })
}

// envPathIn says whether path is an environment's of project: <project>/[<instance>/]<env>,
// each name one devopsy gives.
func envPathIn(project, path string) bool {
	parts := strings.Split(path, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != project {
		return false
	}
	if len(parts) == 3 && !remote.ValidName(parts[1]) {
		return false
	}
	return remote.ValidEnvName(parts[len(parts)-1])
}

// runServerInfo implements devopsy [@<server>] --server-info [--json]:
// the facts the server's devopsy reports, or this machine's without a
// target, each with what needs attention in it, warned with its fix;
// serverInfoWarned when anything was.
func runServerInfo(t *remote.Target, args []string) int {
	asJSON, code := reportArgs("--server-info", args, "usage: devopsy [@<server>] --server-info [--json]")
	if code >= 0 {
		return code
	}
	if t == nil {
		// Servers are Linux: elsewhere (the operator's Mac) the facts are
		// half missing or another machine's (Docker's VM).
		if runtime.GOOS != "linux" {
			report.Default().Say(report.ErrorLevel, "--server-info without a server reads the Linux server it runs on, and this is "+runtime.GOOS, nil,
				report.Fix("", "devopsy @<server> --server-info"))
			return 1
		}
		info, err := serverFacts()
		if err != nil {
			return failErr(err)
		}
		r := serverReport{Facts: info, Entries: serverInfoReport(info, "", "", version, time.Now())}
		head := "this machine"
		if name, err := os.Hostname(); err == nil {
			head += " (" + name + ")"
			r.Host = name
		}
		return r.print(os.Stdout, styleFor(os.Stdout), head, asJSON)
	}
	// The facts' first byte is when the server's clock is compared: the
	// session's end comes after.
	out := &firstWrite{}
	code, err := remote.SSH(t, remote.ServerInfoScript(t), bytes.NewReader(nil), out, false)
	if err != nil {
		return failErr(err)
	}
	if code != 0 {
		return code
	}
	var info remote.ServerInfo
	if err := json.Unmarshal(out.buf.Bytes(), &info); err != nil {
		report.Error("the server's devopsy answered what this one cannot read: " + err.Error())
		return 1
	}
	ref := remote.ServerRef(t)
	head := "@" + ref
	if ref != t.Host+":" {
		head += " (" + t.Host + ")"
	}
	r := serverReport{Server: ref, Host: t.Host, Facts: info, Entries: serverInfoReport(info, t.Host, ref, version, out.at)}
	return r.print(os.Stdout, styleFor(os.Stdout), head, asJSON)
}

// firstWrite is a buffer that keeps when its first write came. The buffer
// is a named field: embedded, its ReadFrom would take exec.Cmd's io.Copy
// around Write.
type firstWrite struct {
	buf bytes.Buffer
	at  time.Time
}

func (w *firstWrite) Write(p []byte) (int, error) {
	if w.at.IsZero() {
		w.at = time.Now()
	}
	return w.buf.Write(p)
}

// serverReport is what --server-info shows and --server-info --json
// prints.
type serverReport struct {
	// Server is the server as typed (remote.ServerRef), empty for this
	// machine; Host where it is, this machine's name.
	Server string            `json:"server,omitempty"`
	Host   string            `json:"host,omitempty"`
	Facts  remote.ServerInfo `json:"facts"`
	// Entries are what --server-info shows of the facts, in order, each
	// warning after the line it is about, under its key.
	Entries []remote.StatusEntry `json:"entries"`
	// Attention says something is at StatusWarning: it exits 3.
	Attention bool `json:"attention"`
}

// print shows r on w in st under heading, or prints it as JSON:
// serverInfoWarned when something needs attention.
func (r *serverReport) print(w io.Writer, st style, heading string, asJSON bool) int {
	r.Attention = entriesWarn(r.Entries)
	code := 0
	if r.Attention {
		code = serverInfoWarned
	}
	if asJSON {
		r.Entries = copyableEntries(r.Entries)
		if err := printJSON(w, r); err != nil {
			return failErr(err)
		}
		return code
	}
	fmt.Fprintln(w, st.name(report.Copyable(heading)))
	newEntryTable(w, st, r.Entries).print(r.Entries)
	return code
}

// serverInfoReport is what --server-info shows of info, for the server
// host (as an address's server part) and ref (as typed, for fixes:
// remote.ServerRef), both empty for this machine's; local is this
// devopsy's version, now when the facts came: what ages count from and
// the server's clock (info.Time) is compared with. Its entries are in
// three sections: the server's own (serverKeys' order), its projects
// (projectEntries, with what leftEntries finds of each), and the
// leftovers that are the server's.
func serverInfoReport(info remote.ServerInfo, host, ref, local string, now time.Time) []remote.StatusEntry {
	var es []remote.StatusEntry
	key := ""
	// row adds a line under k, more one under the last row's key.
	row := func(k, value string) {
		key = k
		es = append(es, remote.StatusEntry{Level: remote.StatusInfo, Key: k, Lines: []string{value}})
	}
	more := func(value string) { row(key, value) }
	// warn adds a warning under the last row's key, with its fixes; note
	// a line said with its fixes, never warned.
	warn := func(msg string, fixes ...report.Remedy) {
		es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: key, Lines: []string{msg}, Remedies: fixes})
	}
	// A project user's (its docker the door's) is read by who can only pass on what the
	// server's admin fixes.
	projectUser := info.Docker.Door
	note := func(msg string, fixes ...report.Remedy) {
		if projectUser {
			fixes = nil
		}
		es = append(es, remote.StatusEntry{Level: remote.StatusInfo, Key: key, Lines: []string{msg}, Remedies: fixes})
	}
	// forAdmin warns of what only the server's admin fixes, by its code: with its fixes to
	// the admin, to a project user with the code to pass on and no remedy.
	forAdmin := func(c report.Code, details []string, fixes ...report.Remedy) {
		lines := append([]string{c.Msg()}, details...)
		if projectUser {
			lines, fixes = append(lines, c.ForAdmin()), nil
		}
		es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: key, Lines: lines, Remedies: fixes})
	}

	// devopsy and the release settings.
	v := info.Version
	if local != info.Version {
		v += " (" + vocab.Operator.Poss + " " + local + ")"
	}
	row("devopsy", v)
	if ref != "" && update.Newer(local, info.Version) {
		forAdmin(report.OlderDevopsy, []string{"the server's " + info.Version + ", " + vocab.Operator.Poss + " " + local},
			report.Fix("", "devopsy @"+ref+" --upgrade"), report.Fix("", "devopsy @"+ref+" --match-server-version"))
	}
	// The facts are judged here: an older local devopsy reads a field it does not know (a
	// renamed one) as missing, and warns of what the server has. The fix is the operator's own,
	// a project user's too.
	if ref != "" && update.Newer(info.Version, local) {
		es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: key,
			Lines:    []string{"the devopsy " + vocab.Operator.On + " is older than the server's", "the server's " + info.Version + ", " + vocab.Operator.Poss + " " + local + ": what it does not know of the server's facts reads as missing"},
			Remedies: []report.Remedy{report.Fix("", "devopsy @"+ref+" --match-server-version")}})
	}
	row("releases", fmt.Sprintf("%s, keep %d (at most %d)", info.Root, info.Keep, info.MaxKeep))
	snaps := "in each environment's directory"
	if info.SnapshotRoot != "" {
		snaps = info.SnapshotRoot
	}
	snaps += fmt.Sprintf(", keep at most %d", info.SnapshotMaxKeep)
	if info.MirrorsOff {
		snaps += ", no mirrors"
	}
	row("snapshots", snaps)

	// The machine.
	var sys []string
	for _, s := range []string{info.OS, info.Kernel, info.Arch} {
		if s != "" {
			sys = append(sys, s)
		}
	}
	row("system", strings.Join(sys, ", "))
	var hw []string
	hw = append(hw, plural(info.CPUs, "CPU"))
	if info.MemTotal > 0 {
		hw = append(hw, fmt.Sprintf("%s memory (%s available)", humanBytes(info.MemTotal), humanBytes(info.MemAvailable)))
	}
	if info.SwapTotal > 0 {
		hw = append(hw, humanBytes(info.SwapTotal)+" swap")
	} else {
		hw = append(hw, "no swap")
	}
	if info.Load != "" {
		hw = append(hw, "load "+info.Load)
	}
	if info.Uptime > 0 {
		hw = append(hw, "up "+duration(time.Duration(info.Uptime*float64(time.Second))))
	}
	more(strings.Join(hw, ", "))
	// Memory the kernel keeps from the rest, which MemTotal leaves out
	// (kdump's) or counts (Kexec HandOver's).
	var kept []string
	if info.KHOScratch > 0 {
		kept = append(kept, humanBytes(info.KHOScratch)+" for Kexec HandOver")
	}
	if info.CrashKernel > 0 {
		kept = append(kept, humanBytes(info.CrashKernel)+" for kdump")
	}
	if len(kept) > 0 {
		more("kernel keeps " + strings.Join(kept, ", "))
	}
	if info.KHOScratch > 0 {
		forAdmin(report.KexecHandOver, []string{"it keeps " + humanBytes(info.KHOScratch) + ": the kernel's own allocations can run out with memory free (an initramfs out of memory, a deadlocked kernel)"},
			stepsFix("kho=off on the kernel's command line, then a reboot", howto.KexecHandOver))
	}
	if info.CrashKernel > 0 {
		note("kdump keeps "+humanBytes(info.CrashKernel)+" of memory for its crash kernel, which nothing else may use",
			stepsHint("without crash dumps: crashkernel= taken off the kernel's command line and kdump's service disabled, then a reboot", howto.Kdump))
	}
	if info.RebootRequired {
		// Named by its flag, as what waits for it says it (pendingWords): devopsy among the
		// packages when the setup or the door asked for it.
		why := remote.RebootRequired
		if len(info.RebootPackages) > 0 {
			why += ", for " + strings.Join(info.RebootPackages, ", ")
		}
		forAdmin(report.RebootNeeded, []string{why}, report.Fix("reboot it at a quiet time: its containers come back with Docker"))
	}
	// Against the operator's clock, which a laptop or CI keeps
	// synchronized: off by the way's one-way time at most.
	clock := map[string]string{"yes": "synchronized", "no": "not synchronized"}[info.NTP]
	off := ""
	if info.Time != 0 {
		off = clockOffset(time.Unix(0, info.Time).Sub(now))
		clock = strings.TrimPrefix(clock+", "+off, ", ")
	}
	if clock != "" {
		more("clock " + clock)
	}
	if info.NTP == "no" {
		why := "certificates and logs need it synchronized"
		if off != "" {
			why = off + ": " + why
		}
		fixes := []report.Remedy{report.Fix("as root:", "timedatectl set-ntp true")}
		if info.NTPService == "yes" {
			// Its time service runs and no server answers it: a network
			// that blocks NTP (UDP 123) out.
			fixes = []report.Remedy{
				report.Hint("its time service runs and no time server answers (UDP 123 blocked out?); what it tried:", "timedatectl timesync-status"),
				report.Fix("name one the network has, NTP=<host> in /etc/systemd/timesyncd.conf.d/ (or chrony's sources), as root"),
			}
		}
		forAdmin(report.ClockUnsynced, []string{why}, fixes...)
	}

	if dr := info.Door; dr != nil {
		row("door", dr.Space+", "+plural(dr.Users, "project user"))
		if dr.Unguarded != "" {
			es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: key, Lines: []string{report.SpaceUnguarded.Msg(), dr.Unguarded + ", so the door refuses every request"},
				Remedies: projectUsersSetup(report.Fix, ref, howto.ProjectUsers)})
		}
		if dr.Reboot {
			warn("sshd takes the door at the next reboot: until then no project user logs in",
				report.Fix("reboot it at a quiet time: its containers come back with Docker"))
		}
		if len(dr.Listeners) > 0 {
			es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: key, Lines: append([]string{"every container reaches what the host listens on beyond loopback"}, dr.Listeners...),
				Remedies: []report.Remedy{report.Fix("make each listen on 127.0.0.1, which no container reaches")}})
		}
	}

	// Docker, and what the setup refuses of what it answers (dockerAnswers), said here only.
	d := info.Docker
	sayDocker := func() {
		for _, r := range dockerAnswers(d) {
			warn(r.Message, fixWords(r)...)
		}
	}
	switch {
	case d.Door:
		key = "docker"
		more("the server's admin's, through the door")
	case d.Error != "":
		key = "docker"
		sayDocker()
	default:
		parts := []string{d.Version}
		if d.Compose != "" {
			parts = append(parts, "compose "+d.Compose)
		}
		if d.Buildx != "" {
			parts = append(parts, "buildx "+d.Buildx)
		}
		store := "classic image store"
		if d.Containerd {
			store = "containerd image store"
		}
		parts = append(parts, store+" ("+d.Driver+")")
		if d.Cgroup != "" {
			parts = append(parts, "cgroup "+d.Cgroup)
		}
		row("docker", strings.Join(parts, ", "))
		sayDocker()
		// Docker lists apparmor whenever the kernel has it, and then loads
		// its profile with apparmor_parser: without it no container
		// starts ("the docker-default profile could not be loaded").
		security := "security " + strings.Join(d.Security, " ")
		if !info.AppArmor {
			security += " (no AppArmor in this kernel)"
		}
		more(security)
		if info.AppArmor && !info.AppArmorParser {
			warn("the kernel has AppArmor and the server has no apparmor_parser: Docker cannot load its profile, so containers fail to start",
				report.Fix("install the apparmor package, as root"))
		}
		logs := "logs " + d.LogDriver
		if d.LogOptsKnown {
			for _, k := range sortedKeysOf(d.LogOpts) {
				logs += " " + k + "=" + d.LogOpts[k]
			}
		} else {
			logs += " (daemon.json unreadable)"
		}
		more(logs)
		// The setup's logs part covers it where it was judged, and stays silent on a value kept.
		judged := info.Setup != nil && info.Setup.DaemonError == ""
		if !judged && d.LogDriver == "json-file" && d.LogOptsKnown && d.LogOpts["max-size"] == "" {
			warn("containers' logs grow without a limit (json-file without max-size) until the disk is full",
				report.Fix(`"log-opts": {"max-size": "20m", "max-file": "5"} in /etc/docker/daemon.json, then restart Docker, as root: containers created after it get it`))
		}
		if d.Proxy != "" {
			more("proxy " + d.Proxy)
		}
		// The proxy containers get, and the loopback a request to the container itself would send
		// there: curl and libcurl do unless noProxy names it (an app calling itself on localhost).
		// A note, not a warning: Go and most checks never do, and nothing devopsy runs needs it.
		for _, p := range d.ClientProxies {
			line := "containers' proxy " + p.Proxy
			if p.For != "default" {
				line += " (for " + p.For + ")"
			}
			more(line)
			if left := loopbackProxied(p.NoProxy); len(left) > 0 {
				note("its noProxy leaves "+strings.Join(left, ", ")+" to the proxy: an app's request to itself there goes through it (curl and libcurl send localhost unless noProxy names it)",
					stepsHint("add "+strings.Join(left, ",")+" to noProxy in "+p.File+", as the deploy user: containers created after it get it", howto.Proxy))
			}
		}
		// One proxy without the other: the containers' requests out going direct while Docker's
		// pulls go through it, or the reverse. On a network whose only way out is the proxy,
		// what goes direct fails.
		switch {
		case d.Proxy != "" && len(d.ClientProxies) == 0:
			warn("Docker's daemon pulls through a proxy and the deploy user's containers get none: their requests to the Internet go direct",
				stepsFix("give the deploy user's Docker client the proxy (config.json's proxies)", howto.Proxy))
		case d.Proxy == "" && len(d.ClientProxies) > 0:
			warn("the deploy user's containers get a proxy and Docker's daemon none: its pulls go direct",
				stepsFix("give Docker's daemon the proxy, as root", howto.Proxy))
		}
		// Where Docker keeps its data, which disks then names alone.
		var data []string
		for _, disk := range info.Disks {
			for _, p := range disk.Paths {
				switch what, path, _ := strings.Cut(p, "="); what {
				case "docker":
					data = append(data, "data "+path)
				case "containerd":
					data = append(data, what+" "+path)
				}
			}
		}
		if len(data) > 0 {
			more(strings.Join(data, ", "))
		}
	}

	// The server's setup, as --server-setup judges it.
	if info.Setup != nil {
		es = append(es, setupInfoEntries(*info.Setup, d, ref)...)
	}

	// /proc's processes, under setup: --project-users setup sets them. A warning only where a
	// project user is there to read them: without one every login is the deploy user's or an
	// admin's, and it is a line to know before adding one.
	key = "setup"
	users := info.Door != nil && info.Door.Users > 0
	switch r := info.ProcRefused; {
	case processesShown(info.HidePID) && users:
		msg, detail, fixes := processesShownWarning(ref)
		es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: key, Lines: []string{msg, detail}, Remedies: fixes})
	case processesShown(info.HidePID):
		msg, hints := processesShownNote(ref)
		note(msg, hints...)
	// /proc hidden another way, judged as --project-users setup judges it, in its words.
	case r != nil && users:
		warn(r.Message, fixWords(*r)...)
	case r != nil:
		note(r.Message, report.Hint(r.Fix))
	}

	// What a project user can only pass on: by its code, no remedy it could take.
	for _, id := range info.ForAdmin {
		if c, ok := report.CodeOf(id); ok {
			es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: "admin", Lines: []string{c.Msg(), c.ForAdmin()}})
		}
	}

	// The disks.
	release := -1
	snapshotDisk := -1
	for i, disk := range info.Disks {
		for _, p := range disk.Paths {
			what, _, _ := strings.Cut(p, "=")
			if what == "releases" {
				release = i
			}
			if what == "snapshots" {
				snapshotDisk = i
			}
		}
		reflinks := map[string]string{"yes": "reflinks", "no": "no reflinks"}[disk.Reflinks]
		key = "disks"
		es = append(es, judgeDisk(disk, info.SnapshotRoot, ref).entries("", reflinks, false)...)
		if disk.Reflinks == "no" {
			// The mirror is the project's to take, data on XFS the admin's.
			fixes := []report.Remedy{reflinkFix(info.MirrorsOff)}
			if !projectUser {
				fixes = append(fixes, stepsHint("what data on XFS takes", howto.XFS))
			}
			warn("the release root's filesystem ("+disk.Type+") makes no reflinks: a snapshot, and a release snapshot, keep its services stopped for the whole copy", fixes...)
		}
	}
	if info.SnapshotRoot != "" && snapshotDisk >= 0 && snapshotDisk == release {
		forAdmin(report.SnapshotOnData, []string{info.SnapshotRoot + ": a volume meant for it is not mounted, or the snapshots go down with the data's disk"},
			report.Fix("mount the backup volume on "+info.SnapshotRoot))
	}

	// Tools. The door's, with no project user, are a line to know before adding one, as a
	// /proc without hidepid is: --project-users add refuses without them.
	key = "tools"
	projectUsers := info.Door != nil && info.Door.Users > 0
	for _, name := range info.Missing {
		switch msg, door := toolMissingWords(name); {
		case msg == "":
		case door && !projectUsers && !projectUser:
			note(msg, stepsHint(doorToolsFixText, howto.ProjectUsers))
		default:
			forAdmin(report.ToolMissing, []string{msg}, toolFix(door))
		}
	}

	// The server's own, in its sections' order, each key's entries kept in theirs.
	for i := range es {
		es[i].Section = "server"
	}
	slices.SortStableFunc(es, func(a, b remote.StatusEntry) int { return cmp.Compare(serverKeyRank(a.Key), serverKeyRank(b.Key)) })
	groups, top := projectEntries(info, host, ref, now)
	leftGroups, leftovers := leftEntries(info, host, ref)
	if len(top) == 0 && len(groups) == 0 && len(leftGroups) == 0 {
		top = append(top, remote.StatusEntry{Level: remote.StatusInfo, Section: "projects", Lines: []string{"none"}})
	}
	es = append(es, top...)
	for _, p := range slices.SortedFunc(maps.Keys(mergeKeys(groups, leftGroups)), byName) {
		es = append(es, projectRows(append(groups[p], leftGroups[p]...))...)
	}
	return append(es, leftovers...)
}

// byName orders names as people count them, projects' and environments'
// alike: ignoring case, a run of digits by its number (pr-7 before
// pr-24), then as they are written.
func byName(a, b string) int {
	x, y := strings.ToLower(a), strings.ToLower(b)
	for x != "" && y != "" {
		cx, restX := nameChunk(x)
		cy, restY := nameChunk(y)
		digits := cx[0] >= '0' && cx[0] <= '9' && cy[0] >= '0' && cy[0] <= '9'
		if digits {
			nx, ny := strings.TrimLeft(cx, "0"), strings.TrimLeft(cy, "0")
			if c := cmp.Or(cmp.Compare(len(nx), len(ny)), cmp.Compare(nx, ny)); c != 0 {
				return c
			}
		} else if c := cmp.Compare(cx, cy); c != 0 {
			return c
		}
		x, y = restX, restY
	}
	return cmp.Or(cmp.Compare(len(x), len(y)), cmp.Compare(a, b))
}

// nameChunk is s's first run of digits, or of anything else, and the rest.
func nameChunk(s string) (chunk, rest string) {
	digit := func(r byte) bool { return r >= '0' && r <= '9' }
	i := 1
	for i < len(s) && digit(s[i]) == digit(s[0]) {
		i++
	}
	return s[:i], s[i:]
}

// projectRows are a project's entries in the order it shows them: its
// heading's first, then each row by its key's name, what is left of a
// gone environment among the live ones, each row's entries (its warning,
// what is left beside it) kept together and in their order.
func projectRows(es []remote.StatusEntry) []remote.StatusEntry {
	// A row is its name's, else its key's.
	rowOf := func(e remote.StatusEntry) string { return cmp.Or(e.Name, e.Key) }
	var head []remote.StatusEntry
	var rows [][]remote.StatusEntry
	for _, e := range es {
		if e.Head {
			head = append(head, e)
			continue
		}
		if i := slices.IndexFunc(rows, func(r []remote.StatusEntry) bool { return rowOf(r[0]) == rowOf(e) }); i >= 0 {
			rows[i] = append(rows[i], e)
			continue
		}
		rows = append(rows, []remote.StatusEntry{e})
	}
	slices.SortStableFunc(rows, func(a, b []remote.StatusEntry) int { return byName(rowOf(a[0]), rowOf(b[0])) })
	for _, r := range rows {
		head = append(head, r...)
	}
	return head
}

// serverKeys are the server section's keys in their order: the
// machine, Docker, the disks and the tools, then devopsy's own install.
var serverKeys = strings.Fields("system docker disks tools devopsy releases snapshots setup door admin")

// serverKeyRank is key's place in serverKeys, any other's after them.
func serverKeyRank(key string) int {
	if i := slices.Index(serverKeys, key); i >= 0 {
		return i
	}
	return len(serverKeys)
}

// mergeKeys is a set of the keys of maps.
func mergeKeys[V any](ms ...map[string]V) map[string]bool {
	keys := map[string]bool{}
	for _, m := range ms {
		for k := range m {
			keys[k] = true
		}
	}
	return keys
}

// projectEntries are --server-info's projects, by project: its heading
// (its project uid claims), then each environment's row (its release,
// its containers, its uid claim, what runs there) with the role it
// holds and its one warning under it, then the environments only a
// claim is left of; top the section's own (the roles no environment
// holds, what did not read), for the server host (an address's server
// part, empty for this machine's) and ref (as typed), now when the
// facts came.
func projectEntries(info remote.ServerInfo, host, ref string, now time.Time) (groups map[string][]remote.StatusEntry, top []remote.StatusEntry) {
	const section = "projects"
	groups = map[string][]remote.StatusEntry{}
	entry := func(level, group, key string, lines []string, rs ...report.Remedy) remote.StatusEntry {
		return remote.StatusEntry{Level: level, Section: section, Group: group, Key: key, Lines: lines, Remedies: rs}
	}
	topAdd := func(level, key, line string, rs ...report.Remedy) {
		top = append(top, entry(level, "", key, []string{line}, rs...))
	}

	// Roles: who holds each, what they export, from the containers docker
	// ps lists, as a release's check and imports see them (cli.Running,
	// RoleHoldersScript): a restarting one holds its role too.
	holders := map[string][]string{}
	exports := map[string]map[string]string{}
	for _, c := range info.Containers {
		if !slices.Contains([]string{"running", "restarting", "paused"}, c.State) || c.Project == "" {
			continue
		}
		if role := c.Labels[cli.RoleLabel]; role != "" && !slices.Contains(holders[role], c.Project) {
			holders[role] = append(holders[role], c.Project)
		}
		for k, v := range c.Labels {
			if key, ok := strings.CutPrefix(k, cli.ExportPrefix); ok {
				if exports[c.Project] == nil {
					exports[c.Project] = map[string]string{}
				}
				exports[c.Project][key] = v
			}
		}
	}
	envNames := map[string]bool{}
	for _, env := range info.Environments {
		envNames[composeNameOf(env.Path)] = true
	}
	// roleLine is what compose name holds: its roles and what it exports.
	roleLine := func(name string, roles []string) string {
		line := "role " + strings.Join(roles, ", ")
		var ex []string
		for _, k := range sortedKeysOf(exports[name]) {
			ex = append(ex, k+"="+exports[name][k])
		}
		if len(ex) > 0 {
			line += ", exports " + strings.Join(ex, ", ")
		}
		return line
	}
	held := map[string][]string{}
	if info.ContainersUnread != "" {
		topAdd(remote.StatusWarning, "roles", "the roles are unknown: the containers could not be listed: "+oneLine(info.ContainersUnread))
	}
	for _, role := range sortedKeysOf(holders) {
		for _, p := range holders[role] {
			held[p] = append(held[p], role)
		}
		if len(holders[role]) > 1 {
			topAdd(remote.StatusWarning, "roles", "role "+role+" is held by "+strings.Join(holders[role], " and ")+": a role is one compose project's on a host, and imports read either")
		}
	}
	// A role held by no environment (the admin's own compose project) is the section's.
	for _, p := range sortedKeysOf(held) {
		if !envNames[p] {
			topAdd(remote.StatusInfo, "roles", p+": "+roleLine(p, held[p]))
		}
	}

	// The uid claims: a project's on its heading, an environment's on its row, one whose
	// environment is gone in its project, one that does not read the section's.
	envUID := map[string]remote.UIDClaim{}
	projectUIDs := map[string][]string{}
	staleProject := map[string][]remote.StatusEntry{}
	for _, c := range info.UIDs {
		rm := func(kind func(string, ...string) report.Remedy) report.Remedy {
			return claimRemoval(kind, ref, info.UIDsDir, c)
		}
		switch {
		case c.Bad != "" && c.Name == info.UIDsDir:
			// The directory itself: nothing in it to remove.
			topAdd(remote.StatusWarning, "uids", "the uid claims could not be read: "+oneLine(c.Bad))
			continue
		case c.Bad != "":
			topAdd(remote.StatusWarning, "uids", c.Name+" in "+info.UIDsDir+" is no claim: "+c.Bad, rm(report.Fix))
			continue
		}
		o, err := uids.ParseOwner(c.Owner)
		if err != nil {
			topAdd(remote.StatusWarning, "uids", c.Owner+"'s uid claim is left: "+err.Error(), rm(report.Fix))
			continue
		}
		uid := strconv.FormatUint(uint64(c.UID), 10)
		if o.Env != "" {
			envUID[o.ComposeName()] = c
		} else {
			words := "project uid " + uid
			if o.Instance != "" {
				words += " for " + o.Instance
			}
			projectUIDs[o.Project] = append(projectUIDs[o.Project], words)
		}
		// A claim left harms nothing: its uid stays reserved, and only freeing it could hand
		// its files to another environment. An environment's is what is left of it
		// (leftEntries); a project's is said under its heading.
		if why := staleUID(c, info.UIDs, info.Environments, info.EnvironmentsUnread != "", info.DoorProjects); why != "" && o.Env == "" {
			e := entry(remote.StatusInfo, o.Project, o.Project, []string{c.Owner + "'s uid claim " + uid + " is left: " + why}, rm(report.Hint))
			e.Head = true
			staleProject[o.Project] = append(staleProject[o.Project], e)
		}
	}

	// Environments. Without flock every lock reads held: no environment
	// is busy then, nothing can run there (warned above).
	noFlock := slices.Contains(info.Missing, "flock")
	if info.EnvironmentsUnread != "" {
		topAdd(remote.StatusWarning, "environments", "not every environment could be read: "+oneLine(info.EnvironmentsUnread))
	}
	for _, env := range info.Environments {
		project, rest, _ := strings.Cut(env.Path, "/")
		h := parseStatus(env.Status)
		h.busy = h.busy && !noFlock
		// A first release that failed is its warning's to say.
		release := ""
		switch {
		case h.none:
			release = "not released yet"
		case h.record == nil:
			release = "release " + h.id
		default:
			release = "released"
			if when, err := time.Parse(time.RFC3339, h.record.Time); err == nil {
				release += " " + agoAt(when, now)
			}
			if h.record.By != "" {
				release += " by " + h.record.By
			}
		}
		compose := composeNameOf(env.Path)
		running, all := 0, 0
		var bad []string
		for _, c := range info.Containers {
			// A one-off (a run, an import's run --rm) is no service's
			// container.
			if c.Project != compose || c.OneOff {
				continue
			}
			all++
			if c.State == "running" {
				running++
			}
			if trouble := containerTrouble(c.State, c.Health, c.ExitCode); trouble != "" {
				bad = append(bad, c.Service+" "+trouble)
			}
		}
		cells := []string{release, "", "", ""}
		if all > 0 {
			cells[1] = fmt.Sprintf("%d of %d running", running, all)
		}
		if c, ok := envUID[compose]; ok {
			cells[2] = "uid " + strconv.FormatUint(uint64(c.UID), 10)
		}
		switch {
		case h.syncing:
			cells[3] = "syncing"
		case h.importing:
			cells[3] = "importing"
		case h.snapshotting:
			cells[3] = "taking a snapshot"
		case h.source:
			cells[3] = "a sync or an export from it"
		case h.busy:
			cells[3] = "an operation running"
		}
		e := entry(remote.StatusInfo, project, rest, []string{remote.CellsJoined(cells...)})
		e.Cells = cells
		if roles := held[compose]; len(roles) > 0 {
			e.Lines = append(e.Lines, roleLine(compose, roles))
		}
		groups[project] = append(groups[project], e)
		// --status's warnings (envState.entries), whose fixes --status
		// gives: this names the environment by its row.
		var problems []string
		for _, w := range h.entries(envAt{}) {
			if w.Level == remote.StatusWarning {
				problems = append(problems, w.Lines...)
			}
		}
		if len(bad) > 0 {
			problems = append(problems, strings.Join(bad, ", "))
		}
		// One warning of every problem, with the --status that shows them.
		if len(problems) > 0 {
			groups[project] = append(groups[project], entry(remote.StatusWarning, project, rest, problems, statusHint(host, info.Root, env)))
		}
	}
	// An environment claim with no row of its own and none of what is left (a project user's
	// tree that answered nothing, an environment that did not read): said under its project,
	// every claim somewhere.
	rows := map[string]bool{}
	for _, env := range info.Environments {
		rows[composeNameOf(env.Path)] = true
	}
	left := staleClaims(info)
	for _, c := range info.UIDs {
		o, err := uids.ParseOwner(c.Owner)
		if _, stale := left[o.ComposeName()]; err != nil || o.Env == "" || rows[o.ComposeName()] || stale {
			continue
		}
		project, key := claimKey(c)
		e := placed(project, key, []string{"uid " + strconv.FormatUint(uint64(c.UID), 10) + ", its environment not read"}, report.Remedy{})
		e.Remedies = nil
		groups[project] = append(groups[project], e)
	}
	// Each project's heading first, its project uid claims, and under it those left.
	for p, words := range projectUIDs {
		head := entry(remote.StatusInfo, p, p, []string{strings.Join(words, ", ")})
		head.Head = true
		groups[p] = append(append([]remote.StatusEntry{head}, staleProject[p]...), groups[p]...)
	}
	return groups, top
}

// oneLine is text, an error's lines joined (errors.Join), on one line.
func oneLine(text string) string { return strings.ReplaceAll(strings.TrimSpace(text), "\n", "; ") }

// clockOffset says d, the server's clock less the local one, as
// --server-info shows it: about 0.8 s behind local.
func clockOffset(d time.Duration) string {
	way := "ahead of"
	if d < 0 {
		way, d = "behind", -d
	}
	switch {
	case d < 100*time.Millisecond:
		return "matching " + vocab.Operator.Poss
	case d < 10*time.Second:
		return fmt.Sprintf("about %.1f s %s %s", d.Seconds(), way, vocab.Operator.Poss)
	case d < 2*time.Minute:
		return fmt.Sprintf("about %d s %s %s", int(d.Seconds()), way, vocab.Operator.Poss)
	}
	return "about " + duration(d) + " " + way + " " + vocab.Operator.Poss
}

// reflinkFix is the way to keep services up through a snapshot's copy
// on a filesystem without reflinks: a mirror, unless the server's
// snapshots: {mirror: false} refuses every one, or XFS.
func reflinkFix(mirrorsOff bool) report.Remedy {
	mirror := "snapshots: {mirror: true} for the environments with large data, in their project's .devopsy/config.yaml (a kept mirror copies most while they run)"
	if mirrorsOff {
		mirror = "the server's snapshots: {mirror: false} (its devopsy config) lifted, then " + mirror
	}
	return report.Fix(mirror + ", or data on XFS")
}

// diskUse is a disk as --server-info and --status say it: its row's
// cells (where it is mounted, its filesystem, what is free, how full it
// is), what lives there by name and with its path, a line instead when
// it did not read, and past the thresholds warn and the ways to free it.
type diskUse struct {
	cells, names, paths []string
	line, warn          string
	fixes               []report.Remedy
}

// entries are the disk's row under disks in section, reflinks its cell
// after its use (empty when not tried, kept for the next cells to
// align), what lives there last, with its paths when withPaths (a
// report with no other line naming them), then its warning, an entry
// of its own with its fixes.
func (u diskUse) entries(section, reflinks string, withPaths bool) []remote.StatusEntry {
	e := remote.StatusEntry{Level: remote.StatusInfo, Section: section, Key: "disks", Lines: []string{u.line}}
	if u.line == "" {
		holds := u.names
		if withPaths {
			holds = u.paths
		}
		e.Cells = append(slices.Clone(u.cells), reflinks, strings.Join(holds, ", "))
		e.Lines = []string{remote.CellsJoined(e.Cells...)}
	}
	es := []remote.StatusEntry{e}
	if u.warn != "" {
		es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Section: section, Key: "disks", Lines: []string{u.warn}, Remedies: u.fixes})
	}
	return es
}

// diskWarnEnv off judges no disk full, for tests whose disk may be.
const diskWarnEnv = "DEVOPSY_INTERNAL_DISK_WARN"

// judgeDisk is disk's row and warning, for a server with snapshotRoot,
// ref the server as typed for its fixes (remote.ServerRef), empty for
// this machine.
func judgeDisk(disk remote.Disk, snapshotRoot, ref string) diskUse {
	var u diskUse
	mount, root := disk.Mount, false
	var named []string
	for _, p := range disk.Paths {
		what, path, _ := strings.Cut(p, "=")
		named = append(named, path)
		if what == "/" {
			// / is named for the disk it is on: its mount says it.
			root = true
			continue
		}
		u.names = append(u.names, what)
		u.paths = append(u.paths, what+" "+path)
	}
	if disk.Error != "" {
		// What it is by its paths, its mount only when it is one they do not name.
		where := u.paths
		if root {
			where = append(where, "/")
		}
		if mount != "" && !slices.Contains(named, mount) {
			where = append(where, mount)
		}
		u.line = strings.Join(where, ", ") + ": " + disk.Error
		return u
	}
	// Facts with no mount (an older server's): / when it is there, else what lives there first.
	switch {
	case mount != "":
	case root:
		mount = "/"
	case len(named) > 0:
		mount = named[0]
	}
	// As df counts it: of what the deploy user could use, rounded up.
	pct := int64(0)
	if total := disk.Used + disk.Free; total > 0 {
		pct = (disk.Used*100 + total - 1) / total
	}
	u.cells = []string{mount, disk.Type, humanBytes(disk.Free) + " free of " + humanBytes(disk.Size), fmt.Sprintf("%d%% used", pct)}
	if (pct < diskUsedWarn && disk.Free >= diskFreeWarn) || os.Getenv(diskWarnEnv) == "off" {
		return u
	}
	u.warn = fmt.Sprintf("%s is %d%% full, %s free: builds, pulls, uploads, logs and databases fail once it is full", mount, pct, humanBytes(disk.Free))
	has := func(what string) bool {
		return slices.ContainsFunc(disk.Paths, func(p string) bool { return strings.HasPrefix(p, what+"=") })
	}
	if has("docker") || has("containerd") {
		// --gc, the one removal of what nothing on the server owns (old build cache, one-offs,
		// dangling images), needs an address: on the server itself, it is words.
		if ref == "" {
			u.fixes = append(u.fixes, report.Fix("see what Docker takes, then remove what nothing on this server owns with --gc, run on it from a checkout or an alias:", "docker system df"))
		} else {
			u.fixes = append(u.fixes, report.Fix("remove what nothing on the server owns, listed first with its sizes:", "devopsy @"+ref+" --gc"))
		}
	}
	if has("snapshots") || (has("releases") && snapshotRoot == "") {
		u.fixes = append(u.fixes, report.Fix("remove snapshots an environment no longer needs, in its project's checkout:", "devopsy @<target> --snapshots", "devopsy @<target> --snapshots remove <id>"))
	}
	if len(u.fixes) == 0 {
		if ref == "" {
			u.fixes = append(u.fixes, report.Hint("see what takes it"))
		} else {
			u.fixes = append(u.fixes, report.Hint("see what takes it, on the server:", "devopsy @"+ref+" --shell-host"))
		}
	}
	return u
}

// composeNameOf is the compose name of the environment at path,
// <project>/[<instance>/]<environment>, as Target.ComposeName makes it:
// the environment's name lowercased.
func composeNameOf(path string) string {
	parts := strings.Split(path, "/")
	parts[len(parts)-1] = strings.ToLower(parts[len(parts)-1])
	return strings.Join(parts, "-")
}

// statusHint is the --status of the environment at path, which says
// how to fix what it warns of, in its project, or in its current release
// under root on this machine (no host).
func statusHint(host, root string, env remote.Environment) report.Remedy {
	project, rest, _ := strings.Cut(env.Path, "/")
	if env.User != "" {
		// A project user's: its --status runs as that user, from the project's checkout, never
		// as whoever reads this in that user's tree, where devopsy would run its programs.
		server := "<server>"
		if host != "" {
			server = host[strings.LastIndex(host, "@")+1:]
		}
		return report.Hint("as "+env.User+", in "+project+"'s checkout:", "devopsy @"+env.User+"@"+server+":"+remote.QuoteWord(rest)+" --status")
	}
	if host == "" {
		return report.Hint("", "cd "+remote.QuoteWord(filepath.Join(root, env.Path, "current"))+" && devopsy --status")
	}
	return report.Hint("in "+project+"'s checkout:", "devopsy @"+host+":"+remote.QuoteWord(rest)+" --status")
}

// removing is a directory a --destroy cut off while removing it left
// (removeBase: <base>.rm.<pid>).
var removing = regexp.MustCompile(`\.rm\.[0-9]+$`)

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

// duration is d in days, else hours, else minutes.
func duration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return plural(int(d.Hours()/24), "day")
	case d >= time.Hour:
		return plural(int(d.Hours()), "hour")
	default:
		return plural(int(d.Minutes()), "minute")
	}
}

func sortedKeysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// setupInfoEntries are --server-info's lines on the server's setup (f): under setup, whether
// devopsy set it up and what refuses releases there, then its items (setupItems), each not
// devopsy's fixed by --server-setup on ref (in words without a target, ref ""). What Docker
// (d) answers itself is said under docker (dockerAnswers), and the tools under tools, never
// again here.
func setupInfoEntries(f setup.Facts, d remote.DockerFacts, ref string) []remote.StatusEntry {
	fix := setupFix(ref, "", "run --server-setup for this server, as its deploy user")
	// With every step refused, --server-setup applies nothing: the way out is the refusal's,
	// the deploy user's by name, whose own fixes say what to run.
	if f.NoneGoes() {
		fix = report.Fix("fix what refuses every step of the setup first, then run --server-setup")
		if slices.ContainsFunc(f.Refusals, func(r setup.Refusal) bool { return r.Item == setup.RefusalDeployUser }) {
			fix = report.Fix("fix the deploy user first, then run --server-setup")
		}
	}
	var es []remote.StatusEntry
	line := func(level, msg string, fixes ...report.Remedy) {
		es = append(es, remote.StatusEntry{Level: level, Key: "setup", Lines: []string{msg}, Remedies: fixes})
	}
	if f.Record == "" {
		line(remote.StatusWarning, "never set up by devopsy: --upgrade applies nothing here", fix)
	} else {
		line(remote.StatusInfo, "by devopsy, its record "+f.Record)
	}
	// The deploy user, by name once recorded. Not recorded yet, on a server set up before the
	// record or never set up, the next setup records whoever runs it, which the reader learns
	// before it does: said, and no warning, as nothing it refuses differs from before.
	if i := slices.IndexFunc(f.Files, func(x setup.FileFact) bool { return x.Item == setup.ItemDeployUser }); i >= 0 {
		x := f.Files[i]
		// The login named where the read knew it.
		here, record := "", report.Hint("to record the deploy user, run --server-setup as that user")
		if x.User != "" {
			here = " (" + x.User + " here)"
			record = report.Hint("to record " + x.User + " as the deploy user, run --server-setup as " + x.User)
			if len(fix.Cmds) > 0 {
				record = report.Hint(record.Text+":", fix.Cmds...)
			}
		}
		switch {
		case x.Missing && f.Record != "":
			f.Files = slices.Delete(slices.Clone(f.Files), i, i+1)
			line(remote.StatusInfo, "no deploy user recorded yet ("+setup.DeployUserFile+"), so any user in the docker group can act as one; the next --server-setup records the login it runs as"+here, record)
		case x.Missing:
			f.Files = slices.Delete(slices.Clone(f.Files), i, i+1)
			line(remote.StatusInfo, "no deploy user recorded yet: the first --server-setup records the login it runs as"+here+", and from then on refuses every other login")
		case x.InSync && x.User != "":
			line(remote.StatusInfo, "deploy user "+x.User+" ("+setup.DeployUserFile+")")
		}
	}
	var docker []string
	for _, r := range dockerAnswers(d) {
		docker = append(docker, r.ID())
	}
	for _, r := range f.Refusals {
		// The tools are said once, with the others the server needs (serverTools).
		if r.Item != toolsItem && !slices.Contains(docker, r.ID()) {
			// Said to a reader who runs no setup, where the refusal has words for one.
			r.Message = cmp.Or(r.Info, r.Message)
			msg, fixes := refusalWords(r)
			line(remote.StatusWarning, msg, fixes...)
		}
	}
	if f.RootAction != "" {
		line(remote.StatusWarning, rootWords(f.Root, f.RootAction)+": releases refuse it", fix)
	}
	if f.DaemonError != "" {
		msg, fixes := daemonWords(f.DaemonError)
		line(remote.StatusWarning, msg, fixes...)
	}
	sooner := soonerHint(ref)
	es = append(es, setupItems(f, setup.ShowInfo, &fix, &sooner, "setup")...)
	return append(es, volumeEntries(f, ref, "setup")...)
}
