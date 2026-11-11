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
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/update"
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
// (internals/releases.md), with what goes without each.
var serverTools = []struct{ name, without string }{
	{"tar", "no release can be uploaded"},
	{"flock", "nothing that changes an environment runs"},
	{"setsid", "the run step runs plainly, and a fetch's live progress shows only before it"},
	{"script", "builds and pulls show no live progress"},
	{"mkfifo", "no sync runs"},
	{"tail", "only the traps protect a release after its switch (a tail that answers --version)"},
}

// doorFacts are the server's door, nil with none: its space's mount, its users, whether sshd
// waits for a reboot to take it, and its firewall, read as root through the util image.
func doorFacts() *remote.DoorFacts {
	d, err := remote.ServerDoor()
	if err != nil || d == nil || d.Space == "" {
		return nil
	}
	f := &remote.DoorFacts{Space: d.Space, Users: len(d.Users)}
	if err := spaceGuarded(d.Space); err != nil {
		f.Unguarded = err.Error()
	}
	if err := doorHostReady("docker", os.Environ(), ""); err != nil {
		f.Docker = err.Error()
	}
	if _, err := os.Stat(remote.DoorRebootMarker); err == nil {
		f.Reboot = true
	}
	for u := range d.Users {
		if p := d.Users[u].Project; f.Project == "" || p < f.Project {
			f.Project = p
		}
	}
	read, err := doorHostRead(remote.DoorFirewallRead, false)
	if err != nil {
		f.FirewallError = err.Error()
		return f
	}
	f.Listeners = remote.DoorListeners(read)
	if f.FirewallWrong, err = doorFirewallWrong(read); err != nil {
		f.FirewallError = err.Error()
	}
	return f
}

// doorFirewall runs script as root on the host with its network, through the util image
// (writing the host's files when write), and says what of the project users' firewall the
// host lacks from what it prints (remote.DoorFirewallRead): the deploy user can neither
// read nor change it.
func doorFirewall(script string, write bool) ([]string, error) {
	read, err := doorHostRead(script, write)
	if err != nil {
		return nil, err
	}
	return doorFirewallWrong(read)
}

// doorFirewallWrong is what of the project users' firewall read lacks, by the ids of the
// project users' group and the door's.
func doorFirewallWrong(read string) ([]string, error) {
	g, err := user.LookupGroup(remote.DoorGroup)
	if err != nil {
		return nil, fmt.Errorf("no %s group yet", remote.DoorGroup)
	}
	d, err := user.LookupGroup(remote.DoorRunGroup)
	if err != nil {
		return nil, fmt.Errorf("no %s group yet", remote.DoorRunGroup)
	}
	return remote.DoorFirewallWrong(read, g.Gid, d.Gid), nil
}

// doorHostRead runs script as root on the host with its network, its processes and its
// cgroups, through the util image (writing the host's files when write), and returns what it
// prints: the firewall finds Docker's daemon and names its cgroup as the host has them. With
// SYS_PTRACE, which a host hiding other users' processes (/proc's hidepid) asks of a root
// without every capability of the process it looks at, dockerd's.
func doorHostRead(script string, write bool) (string, error) {
	if err := ensureUtilImage(context.Background()); err != nil {
		return "", err
	}
	bind := "/:/host:ro"
	if write {
		bind = "/:/host"
	}
	c := exec.Command("docker", "run", "--rm", "--network", "host", "--pid", "host", "--cgroupns", "host", "--cap-add", "NET_ADMIN", "--cap-add", "SYS_PTRACE", "-v", bind, remote.UtilImage,
		"chroot", "/host", "/bin/sh", "-c", script)
	c.Stderr = os.Stderr
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("the project users' firewall: %v", err)
	}
	return string(out), nil
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
		Version: version, Protocol: remote.Protocol,
		Root: r.Root, Keep: r.Keep, MaxKeep: r.MaxKeep,
		SnapshotRoot: r.Snapshots.Root, SnapshotMaxKeep: r.Snapshots.MaxKeep, MirrorsOff: r.Snapshots.MirrorsOff(),
		Arch: runtime.GOARCH, CPUs: runtime.NumCPU(),
	}
	systemFacts(&info)
	info.Docker = dockerFacts()
	info.Door = doorFacts()
	info.Disks = diskFacts(info.Root, info.Docker.RootDir, containerdRoot(info.Docker), info.SnapshotRoot, true)
	for _, t := range serverTools {
		if _, err := exec.LookPath(t.name); err != nil {
			info.Missing = append(info.Missing, t.name)
		} else if t.name == "tail" && exec.Command("tail", "--version").Run() != nil {
			// As the scripts tell (followOutput): GNU's, or uutils'.
			info.Missing = append(info.Missing, "tail")
		}
	}
	info.Containers = hostContainers()
	info.Environments = environments(info.Root, doorTrees(info.Root), func(who, tree string) []remote.Environment {
		return treeEnvironments(who, tree, doorTreeWait)
	}, ownedByMe)
	return info, nil
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
	if _, err := os.Stat("/var/run/reboot-required"); err == nil {
		info.RebootRequired = true
		if data, err := os.ReadFile("/var/run/reboot-required.pkgs"); err == nil {
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

// dockerFacts asks docker info, docker compose version and reads the
// daemon's configuration file.
func dockerFacts() remote.DockerFacts {
	d := dockerInfo()
	if out, err := exec.Command("docker", "compose", "version", "--short").Output(); err == nil {
		d.Compose = strings.TrimSpace(string(out))
	}
	data, err := os.ReadFile("/etc/docker/daemon.json")
	switch {
	case errors.Is(err, os.ErrNotExist):
		d.LogOptsKnown = true
	case err == nil:
		var conf struct {
			LogDriver string            `json:"log-driver"`
			LogOpts   map[string]string `json:"log-opts"`
		}
		if json.Unmarshal(data, &conf) == nil {
			d.LogOptsKnown, d.LogOpts = true, conf.LogOpts
		}
	}
	return d
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

// dockerInfo is what docker info says, alone.
func dockerInfo() remote.DockerFacts {
	var d remote.DockerFacts
	out, err := exec.Command("docker", "info", "--format", "{{json .}}").Output()
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
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return err
	}
	if len(info.ServerErrors) > 0 {
		return errors.New(strings.Join(info.ServerErrors, "; "))
	}
	d.Version, d.Driver, d.RootDir, d.LogDriver = info.ServerVersion, info.Driver, info.DockerRootDir, info.LoggingDriver
	if info.CgroupVersion != "" {
		d.Cgroup = "v" + info.CgroupVersion
	}
	for _, s := range info.DriverStatus {
		if s[0] == "driver-type" && s[1] == "io.containerd.snapshotter.v1" {
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
		d := remote.Disk{Paths: []string{label}}
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
// project and service, state and health, and its role and exports.
func hostContainers() []remote.HostContainer {
	out, err := exec.Command("docker", "ps", "-aq", "--no-trunc").Output()
	if err != nil {
		return nil
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	// A container gone since ps (a run --rm, a util container) fails
	// inspect, which printed every other one all the same.
	out, _ = exec.Command("docker", append([]string{"inspect", "--format", "{{.State.Status}} {{.State.ExitCode}} {{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}} {{json .Config.Labels}}"}, ids...)...).Output()
	return parseHostContainers(out)
}

// parseHostContainers reads lines of "<state> <exit code> <health or ->
// <labels as JSON>".
func parseHostContainers(out []byte) []remote.HostContainer {
	var cs []remote.HostContainer
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), " ", 4)
		if len(f) < 4 {
			continue
		}
		var labels map[string]string
		if json.Unmarshal([]byte(f[3]), &labels) != nil {
			continue
		}
		c := remote.HostContainer{State: f[0], Project: labels["com.docker.compose.project"], Service: labels["com.docker.compose.service"],
			OneOff: labels["com.docker.compose.oneoff"] == "True"}
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
func environments(root string, trees map[string]string, readTree func(user, tree string) []remote.Environment, owned func(path string) bool) []remote.Environment {
	var envs []remote.Environment
	for _, p := range envDirs(root) {
		if _, ok := trees[p]; !ok && owned(p) {
			envs = append(envs, projectEnvironments(root, p)...)
		}
	}
	// A project user's tree, in the release root or elsewhere (by default its own home).
	for _, tree := range slices.Sorted(maps.Keys(trees)) {
		got := readTree(trees[tree], tree)
		for i := range got {
			got[i].User = trees[tree]
		}
		envs = append(envs, got...)
	}
	return envs
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

// envDirs are dir's directories, a link to one included: a project or an
// environment moved to another disk and linked back is one, as the
// scripts reach it.
func envDirs(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if !projectDir(e.Name()) {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && fi.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// projectEnvironments are the environments of the project at p under root.
func projectEnvironments(root, p string) []remote.Environment {
	var envs []remote.Environment
	isEnv := func(dir string) bool {
		fi, err := os.Stat(filepath.Join(dir, "releases"))
		return err == nil && fi.IsDir()
	}
	add := func(dir string) {
		rel, _ := filepath.Rel(root, dir)
		cmd := exec.Command("sh", "-c", remote.EnvStatus)
		cmd.Env = append(os.Environ(), "base="+dir)
		out, _ := cmd.Output()
		envs = append(envs, remote.Environment{Path: filepath.ToSlash(rel), Status: string(out)})
	}
	for _, e := range envDirs(p) {
		if isEnv(e) {
			add(e)
			continue
		}
		for _, i := range envDirs(e) {
			if isEnv(i) {
				add(i)
			}
		}
	}
	return envs
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
		head := "local"
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
// the server's clock (info.Time) is compared with.
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
	note := func(msg string, fixes ...report.Remedy) {
		es = append(es, remote.StatusEntry{Level: remote.StatusInfo, Key: key, Lines: []string{msg}, Remedies: fixes})
	}

	// devopsy and the release settings.
	v := info.Version + ", protocol " + strconv.Itoa(info.Protocol)
	if local != info.Version {
		v += " (local " + local + ")"
	}
	row("devopsy", v)
	if ref != "" && update.Newer(local, info.Version) {
		warn("the server's devopsy "+info.Version+" is older than the local "+local, report.Fix("", "devopsy @"+ref+" --upgrade"))
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
	// (kdump's) or counts (Kexec HandOver's): devopsy-server's setup.sh
	// frees both.
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
		warn("Kexec HandOver keeps "+humanBytes(info.KHOScratch)+" of memory as CMA, which only movable pages may use: the kernel's own allocations can run out with memory free (an initramfs out of memory, a deadlocked kernel)",
			report.Fix("kho=off on the kernel's command line, then a reboot: devopsy-server's setup.sh adds it, rerun as root"))
	}
	if info.CrashKernel > 0 {
		note("kdump keeps "+humanBytes(info.CrashKernel)+" of memory for its crash kernel, which nothing else may use",
			report.Hint("without crash dumps: DEVOPSY_KDUMP=off for devopsy-server's setup.sh, rerun as root, then a reboot"))
	}
	if info.RebootRequired {
		msg := "the server needs a reboot"
		if len(info.RebootPackages) > 0 {
			msg += " (for " + strings.Join(info.RebootPackages, ", ") + ")"
		}
		warn(msg, report.Fix("reboot it at a quiet time: its containers come back with Docker"))
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
		msg := "the server's clock is not synchronized: certificates and logs need it"
		if off != "" {
			msg = "the server's clock is not synchronized (" + off + "): certificates and logs need it"
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
		warn(msg, fixes...)
	}

	if dr := info.Door; dr != nil {
		row("door", fmt.Sprintf("%s, %d project users", dr.Space, dr.Users))
		if dr.Unguarded != "" {
			warn(dr.Unguarded+": the door refuses every request",
				spaceFix(dr.Space))
		}
		if dr.Docker != "" {
			warn(dr.Docker+": the door refuses every image a project user does not build",
				report.Fix("move Docker to its containerd image store and a current buildx (docs/project-users.md)"))
		}
		if dr.Reboot {
			warn("sshd takes the door at the next reboot: until then no project user logs in",
				report.Fix("reboot it at a quiet time: its containers come back with Docker"))
		}
		switch {
		case dr.FirewallError != "":
			note("the project users' firewall could not be read: " + dr.FirewallError)
		case len(dr.FirewallWrong) > 0:
			es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: key, Lines: append([]string{"project users reach what their firewall keeps them off"}, dr.FirewallWrong...),
				Remedies: []report.Remedy{report.Fix("load it again, for any of its projects:", "devopsy @"+ref+" --project-users add "+cmp.Or(dr.Project, "<project>"))}})
		}
		if len(dr.Listeners) > 0 {
			es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: key, Lines: append([]string{"every container reaches what the host listens on beyond loopback"}, dr.Listeners...),
				Remedies: []report.Remedy{report.Fix("make each listen on 127.0.0.1, which no container reaches")}})
		}
	}

	// Docker.
	d := info.Docker
	if d.Error != "" {
		key = "docker"
		warn("Docker did not answer: " + d.Error)
	} else {
		parts := []string{d.Version}
		if d.Compose != "" {
			parts = append(parts, "compose "+d.Compose)
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
		if d.Compose == "" {
			warn("docker compose is missing", report.Fix("install the docker-compose-plugin package, as root"))
		}
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
		if d.LogDriver == "json-file" && d.LogOptsKnown && d.LogOpts["max-size"] == "" {
			warn("containers' logs grow without a limit (json-file without max-size) until the disk is full",
				report.Fix(`"log-opts": {"max-size": "20m", "max-file": "5"} in /etc/docker/daemon.json, then restart Docker, as root: containers created after it get it`))
		}
		if d.Proxy != "" {
			more("proxy " + d.Proxy)
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
		reflinks := ""
		switch disk.Reflinks {
		case "yes":
			reflinks = ", reflinks"
		case "no":
			reflinks = ", no reflinks"
		}
		key = "disks"
		es = append(es, judgeDisk(disk, info.SnapshotRoot, ref).entries(reflinks)...)
		if disk.Reflinks == "no" {
			warn("the release root's filesystem ("+disk.Type+") makes no reflinks: a snapshot, and a release snapshot, keep its services stopped for the whole copy",
				reflinkFix(info.MirrorsOff))
		}
	}
	if info.SnapshotRoot != "" && snapshotDisk >= 0 && snapshotDisk == release {
		warn("the snapshot root "+info.SnapshotRoot+" is on the release root's filesystem: a volume meant for it is not mounted, or the snapshots go down with the data's disk",
			report.Fix("mount the backup volume on "+info.SnapshotRoot))
	}

	// Tools.
	key = "tools"
	for _, t := range serverTools {
		if slices.Contains(info.Missing, t.name) {
			warn("the server has no " + t.name + ": " + t.without)
		}
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
	if len(holders) == 0 {
		row("roles", "none")
	}
	for _, role := range sortedKeysOf(holders) {
		for _, p := range holders[role] {
			value := role + ": " + p
			for _, k := range sortedKeysOf(exports[p]) {
				value += " " + k + "=" + exports[p][k]
			}
			row("roles", value)
		}
		if len(holders[role]) > 1 {
			warn("role " + role + " is held by " + strings.Join(holders[role], " and ") + ": a role is one compose project's on a host, and imports read either")
		}
	}

	// Environments. Without flock every lock reads held: no environment
	// is busy then, nothing can run there (warned above).
	noFlock := slices.Contains(info.Missing, "flock")
	if len(info.Environments) == 0 {
		row("environments", "none")
	}
	for _, env := range info.Environments {
		h := parseStatus(env.Status)
		h.busy = h.busy && !noFlock
		value := env.Path + "  "
		switch {
		case h.none && h.failed != "":
			value += "none: " + h.failed + " failed"
		case h.none:
			value += "none: not released yet"
		default:
			value += h.id
			if h.record != nil {
				if when, err := time.Parse(time.RFC3339, h.record.Time); err == nil {
					value += "  " + agoAt(when, now)
				}
				if h.record.By != "" {
					value += "  by " + h.record.By
				}
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
		if all > 0 {
			value += fmt.Sprintf("  %d of %d running", running, all)
		}
		switch {
		case h.syncing:
			value += "  syncing"
		case h.importing:
			value += "  importing"
		case h.snapshotting:
			value += "  taking a snapshot"
		case h.source:
			value += "  a sync or an export from it"
		case h.busy:
			value += "  an operation running"
		}
		row("environments", value)
		// --status's warnings (envState.entries), whose fixes --status
		// gives: this names the environment only by its path.
		var problems []string
		for _, e := range h.entries(envAt{}) {
			if e.Level == remote.StatusWarning {
				problems = append(problems, e.Lines...)
			}
		}
		if len(bad) > 0 {
			problems = append(problems, strings.Join(bad, ", "))
		}
		// One warning of every problem, with the --status that shows them.
		if len(problems) > 0 {
			var lines []string
			for _, p := range problems {
				lines = append(lines, env.Path+": "+p)
			}
			es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: key, Lines: lines, Remedies: []report.Remedy{statusHint(host, info.Root, env)}})
		}
	}
	return es
}

// clockOffset says d, the server's clock less the local one, as
// --server-info shows it: about 0.8 s behind local.
func clockOffset(d time.Duration) string {
	way := "ahead of"
	if d < 0 {
		way, d = "behind", -d
	}
	switch {
	case d < 100*time.Millisecond:
		return "matching local"
	case d < 10*time.Second:
		return fmt.Sprintf("about %.1f s %s local", d.Seconds(), way)
	case d < 2*time.Minute:
		return fmt.Sprintf("about %d s %s local", int(d.Seconds()), way)
	}
	return "about " + duration(d) + " " + way + " local"
}

// reflinkFix is the way to keep services up through a snapshot's copy
// on a filesystem without reflinks: a mirror, unless the server's
// snapshots: {mirror: false} refuses every one, or XFS.
func reflinkFix(mirrorsOff bool) report.Remedy {
	mirror := "snapshots: {mirror: true} for the environments with large data, in their project's .devopsy/config.yaml (a kept mirror copies most while they run)"
	if mirrorsOff {
		mirror = "the server's snapshots: {mirror: false} (its devopsy config) lifted, then " + mirror
	}
	return report.Fix(mirror + ", or data on XFS (a new server from devopsy-server's cloud-init.yaml)")
}

// diskUse is a disk as --server-info and --status say it: value its
// line (what lives there, its filesystem, what is free), and past the
// thresholds warn and the ways to free it.
type diskUse struct {
	value, warn string
	fixes       []report.Remedy
}

// entries are the disk's line under disks, more after its value, then
// its warning, an entry of its own with its fixes.
func (u diskUse) entries(more string) []remote.StatusEntry {
	es := []remote.StatusEntry{{Level: remote.StatusInfo, Key: "disks", Lines: []string{u.value + more}}}
	if u.warn != "" {
		es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: "disks", Lines: []string{u.warn}, Remedies: u.fixes})
	}
	return es
}

// diskWarnEnv off judges no disk full, for tests whose disk may be.
const diskWarnEnv = "DEVOPSY_INTERNAL_DISK_WARN"

// judgeDisk is disk's line and warning, for a server with snapshotRoot,
// ref the server as typed for its fixes (remote.ServerRef), empty for
// this machine.
func judgeDisk(disk remote.Disk, snapshotRoot, ref string) diskUse {
	var paths []string
	for _, p := range disk.Paths {
		what, path, _ := strings.Cut(p, "=")
		if what == "/" {
			paths = append(paths, "/")
		} else {
			paths = append(paths, what+" "+path)
		}
	}
	where := strings.Join(paths, ", ")
	if disk.Error != "" {
		return diskUse{value: where + ": " + disk.Error}
	}
	// As df counts it: of what the deploy user could use, rounded up.
	pct := int64(0)
	if total := disk.Used + disk.Free; total > 0 {
		pct = (disk.Used*100 + total - 1) / total
	}
	use := diskUse{value: fmt.Sprintf("%s: %s, %s free of %s (%d%% used)", where, disk.Type, humanBytes(disk.Free), humanBytes(disk.Size), pct)}
	if (pct < diskUsedWarn && disk.Free >= diskFreeWarn) || os.Getenv(diskWarnEnv) == "off" {
		return use
	}
	use.warn = fmt.Sprintf("%s is %d%% full, %s free: builds, pulls, uploads, logs and databases fail once it is full", where, pct, humanBytes(disk.Free))
	has := func(what string) bool {
		return slices.ContainsFunc(disk.Paths, func(p string) bool { return strings.HasPrefix(p, what+"=") })
	}
	if has("docker") || has("containerd") {
		if ref == "" {
			use.fixes = append(use.fixes, report.Fix("see what Docker takes, then empty its build cache:", "docker system df", "docker builder prune"))
		} else {
			use.fixes = append(use.fixes, report.Fix("see what Docker takes, then empty its build cache, on the server:", "devopsy @"+ref+" --shell-host", "docker system df", "docker builder prune"))
		}
	}
	if has("snapshots") || (has("releases") && snapshotRoot == "") {
		use.fixes = append(use.fixes, report.Fix("remove snapshots an environment no longer needs, in its project's checkout:", "devopsy @<target> --snapshots", "devopsy @<target> --snapshots remove <id>"))
	}
	if len(use.fixes) == 0 {
		if ref == "" {
			use.fixes = append(use.fixes, report.Hint("see what takes it"))
		} else {
			use.fixes = append(use.fixes, report.Hint("see what takes it, on the server:", "devopsy @"+ref+" --shell-host"))
		}
	}
	return use
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
