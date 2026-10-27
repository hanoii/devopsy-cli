package main

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// (AGENTS.md, Releases), with what goes without each.
var serverTools = []struct{ name, without string }{
	{"tar", "no release can be uploaded"},
	{"flock", "nothing that changes an environment runs"},
	{"setsid", "the run step runs plainly, and a fetch's live progress shows only before it"},
	{"script", "builds and pulls show no live progress"},
	{"mkfifo", "no sync runs"},
	{"tail", "only the traps protect a release after its switch (a tail that answers --version)"},
}

// runServerFacts implements the hidden --server-facts on a server: its
// facts as one JSON document on stdout (remote.ServerInfo). It reads
// and tries, never changes anything but a file of its own it removes,
// and takes no lock.
func runServerFacts() int {
	r, err := remote.ReleaseSettings()
	if err != nil {
		report.Fail(err)
		return 1
	}
	info := remote.ServerInfo{
		Version: version, Protocol: remote.Protocol,
		Root: r.Root, Keep: r.Keep, MaxKeep: r.MaxKeep,
		SnapshotRoot: r.Snapshots.Root, SnapshotMaxKeep: r.Snapshots.MaxKeep, MirrorsOff: r.Snapshots.MirrorsOff(),
		Arch: runtime.GOARCH, CPUs: runtime.NumCPU(),
	}
	systemFacts(&info)
	info.Docker = dockerFacts()
	info.Disks = diskFacts(info.Root, info.Docker.RootDir, info.SnapshotRoot)
	for _, t := range serverTools {
		if _, err := exec.LookPath(t.name); err != nil {
			info.Missing = append(info.Missing, t.name)
		} else if t.name == "tail" && exec.Command("tail", "--version").Run() != nil {
			// As the scripts tell (followOutput): GNU's, or uutils'.
			info.Missing = append(info.Missing, "tail")
		}
	}
	info.Containers = hostContainers()
	info.Environments = environments(info.Root)
	out, err := json.Marshal(info)
	if err != nil {
		report.Fail(err)
		return 1
	}
	fmt.Println(string(out))
	return 0
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
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		m := meminfo(string(data))
		info.MemTotal, info.MemAvailable, info.SwapTotal = m["MemTotal"], m["MemAvailable"], m["SwapTotal"]
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
	if out, err := exec.Command("timedatectl", "show", "-p", "NTPSynchronized", "--value").Output(); err == nil {
		info.NTP = strings.TrimSpace(string(out))
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

// dockerFacts asks docker info, docker compose version and reads the
// daemon's configuration file.
func dockerFacts() remote.DockerFacts {
	var d remote.DockerFacts
	out, err := exec.Command("docker", "info", "--format", "{{json .}}").Output()
	if err != nil {
		d.Error = "docker info: " + cmdError(err)
	} else if err := parseDockerInfo(out, &d); err != nil {
		d.Error = "docker info: " + err.Error()
	}
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

// diskFacts is the filesystems of the release root, Docker's data, the
// snapshot root and /, one per device, each path where it is: the
// nearest existing directory above it, for a root not made yet.
func diskFacts(root, docker, snapshots string) []remote.Disk {
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
	if len(disks) > 0 && disks[0].Error == "" {
		disks[0].Reflinks = tryReflink(root)
	}
	add("docker", docker)
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
	out, _ = exec.Command("docker", append([]string{"inspect", "--format", "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}} {{json .Config.Labels}}"}, ids...)...).Output()
	return parseHostContainers(out)
}

// parseHostContainers reads lines of "<state> <health or -> <labels as
// JSON>".
func parseHostContainers(out []byte) []remote.HostContainer {
	var cs []remote.HostContainer
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), " ", 3)
		if len(f) < 3 {
			continue
		}
		var labels map[string]string
		if json.Unmarshal([]byte(f[2]), &labels) != nil {
			continue
		}
		c := remote.HostContainer{State: f[0], Project: labels["com.docker.compose.project"], Service: labels["com.docker.compose.service"]}
		if f[1] != "-" {
			c.Health = f[1]
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
// with its state lines (remote.EnvStatus).
func environments(root string) []remote.Environment {
	var envs []remote.Environment
	isEnv := func(dir string) bool {
		fi, err := os.Stat(filepath.Join(dir, "releases"))
		return err == nil && fi.IsDir()
	}
	dirs := func(dir string) []string {
		entries, _ := os.ReadDir(dir)
		var out []string
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
		return out
	}
	add := func(dir string) {
		rel, _ := filepath.Rel(root, dir)
		cmd := exec.Command("sh", "-c", remote.EnvStatus)
		cmd.Env = append(os.Environ(), "base="+dir)
		out, _ := cmd.Output()
		envs = append(envs, remote.Environment{Path: filepath.ToSlash(rel), Status: string(out)})
	}
	for _, p := range dirs(root) {
		for _, e := range dirs(p) {
			if isEnv(e) {
				add(e)
				continue
			}
			for _, i := range dirs(e) {
				if isEnv(i) {
					add(i)
				}
			}
		}
	}
	return envs
}

// runServerInfo implements devopsy @<server> --server-info: the facts
// the server's devopsy reports, then what needs attention, warned with
// its fix; serverInfoWarned when anything was.
func runServerInfo(t *remote.Target, args []string) int {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--server-info"]))
			return 0
		}
		report.Error("usage: devopsy @<server> --server-info")
		return 1
	}
	var out bytes.Buffer
	code, err := remote.SSH(t, remote.ServerInfoScript(t), bytes.NewReader(nil), &out, false)
	if err != nil {
		report.Fail(err)
		return 1
	}
	if code != 0 {
		return code
	}
	var info remote.ServerInfo
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		report.Error("the server's devopsy answered what this one cannot read: " + err.Error())
		return 1
	}
	ref := remote.ServerRef(t)
	lines, warnings := serverInfoReport(info, t.Host, ref, version, time.Now())
	fmt.Println(styleFor(os.Stdout).name("@" + ref + " (" + t.Host + ")"))
	for _, l := range lines {
		fmt.Println(l)
	}
	for _, w := range warnings {
		report.Warn(w.msg, w.fixes...)
	}
	if len(warnings) > 0 {
		return serverInfoWarned
	}
	return 0
}

// serverWarning is a warning and its fixes.
type serverWarning struct {
	msg   string
	fixes []string
}

// serverInfoReport is what --server-info prints of info, its lines and
// its warnings, for the server host (as an address's server part) and
// ref (as typed, for fixes: remote.ServerRef); local is this devopsy's
// version, now the time ages count from.
func serverInfoReport(info remote.ServerInfo, host, ref, local string, now time.Time) (lines []string, warnings []serverWarning) {
	warn := func(msg string, fixes ...string) { warnings = append(warnings, serverWarning{msg, fixes}) }
	row := func(key, value string) {
		lines = append(lines, fmt.Sprintf("%-12s %s", key, value))
	}
	more := func(value string) { row("", value) }

	// devopsy and the release settings.
	v := info.Version + ", protocol " + strconv.Itoa(info.Protocol)
	if local != info.Version {
		v += " (local " + local + ")"
	}
	row("devopsy", v)
	if update.Newer(local, info.Version) {
		warn("the server's devopsy "+info.Version+" is older than the local "+local, "devopsy @"+ref+" --upgrade")
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
	if info.RebootRequired {
		msg := "the server needs a reboot"
		if len(info.RebootPackages) > 0 {
			msg += " (for " + strings.Join(info.RebootPackages, ", ") + ")"
		}
		warn(msg, "reboot it at a quiet time: its containers come back with Docker")
	}
	if info.NTP == "no" {
		warn("the server's clock is not synchronized: certificates and logs need it", "timedatectl set-ntp true, as root")
	}

	// Docker.
	d := info.Docker
	if d.Error != "" {
		row("docker", d.Error)
		warn("Docker did not answer: " + d.Error)
	} else {
		parts := []string{d.Version}
		if d.Compose != "" {
			parts = append(parts, "compose "+d.Compose)
		} else {
			warn("docker compose is missing", "install the docker-compose-plugin package")
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
		more("security " + strings.Join(d.Security, " "))
		if !slices.Contains(d.Security, "apparmor") {
			warn("Docker applies no AppArmor profile to containers", "install apparmor and restart Docker")
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
				`"log-opts": {"max-size": "20m", "max-file": "5"} in /etc/docker/daemon.json, then restart Docker (it applies to containers created after)`)
		}
		if d.Proxy != "" {
			more("proxy " + d.Proxy)
		}
	}

	// The disks.
	release := -1
	snapshotDisk := -1
	for i, disk := range info.Disks {
		var paths []string
		for _, p := range disk.Paths {
			what, path, _ := strings.Cut(p, "=")
			if what == "releases" {
				release = i
			}
			if what == "snapshots" {
				snapshotDisk = i
			}
			if what == "/" {
				paths = append(paths, "/")
			} else {
				paths = append(paths, what+" "+path)
			}
		}
		key := ""
		if i == 0 {
			key = "disks"
		}
		if disk.Error != "" {
			row(key, strings.Join(paths, ", ")+": "+disk.Error)
			continue
		}
		// As df counts it: of what the deploy user could use, rounded up.
		pct := int64(0)
		if total := disk.Used + disk.Free; total > 0 {
			pct = (disk.Used*100 + total - 1) / total
		}
		value := fmt.Sprintf("%s: %s, %s free of %s (%d%% used)", strings.Join(paths, ", "), disk.Type, humanBytes(disk.Free), humanBytes(disk.Size), pct)
		switch disk.Reflinks {
		case "yes":
			value += ", reflinks"
		case "no":
			value += ", no reflinks"
		}
		row(key, value)
		if pct >= diskUsedWarn || disk.Free < diskFreeWarn {
			warn(fmt.Sprintf("%s is %d%% full, %s free: builds, pulls, uploads, logs and databases fail once it is full", strings.Join(paths, ", "), pct, humanBytes(disk.Free)),
				"devopsy @"+ref+" --shell-host, then docker system df shows what takes it, and docker builder prune empties the build cache")
		}
		if disk.Reflinks == "no" {
			warn("the release root's filesystem ("+disk.Type+") makes no reflinks: a snapshot, and a release snapshot, keep its services stopped for the whole copy",
				"snapshots: {mirror: true} in the server's devopsy config, or data on XFS (a new server from devopsy-server's cloud-init.yaml)")
		}
	}
	if info.SnapshotRoot != "" && snapshotDisk >= 0 && snapshotDisk == release {
		warn("the snapshot root "+info.SnapshotRoot+" is on the release root's filesystem: a volume meant for it is not mounted, or the snapshots go down with the data's disk",
			"mount the backup volume on "+info.SnapshotRoot)
	}

	// Tools.
	for _, t := range serverTools {
		if slices.Contains(info.Missing, t.name) {
			warn("the server has no " + t.name + ": " + t.without)
		}
	}

	// Roles: who holds each, what they export, from running containers.
	holders := map[string][]string{}
	exports := map[string]map[string]string{}
	for _, c := range info.Containers {
		if c.State != "running" || c.Project == "" {
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
	for i, role := range sortedKeysOf(holders) {
		key := ""
		if i == 0 {
			key = "roles"
		}
		for _, p := range holders[role] {
			value := role + ": " + p
			for _, k := range sortedKeysOf(exports[p]) {
				value += " " + k + "=" + exports[p][k]
			}
			row(key, value)
			key = ""
		}
		if len(holders[role]) > 1 {
			warn("role " + role + " is held by " + strings.Join(holders[role], " and ") + ": a role is one compose project's on a host, and imports read either")
		}
	}

	// Environments.
	if len(info.Environments) == 0 {
		row("environments", "none")
	}
	for i, env := range info.Environments {
		key := ""
		if i == 0 {
			key = "environments"
		}
		h := parseStatus(env.Status)
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
			if c.Project != compose {
				continue
			}
			all++
			if c.State == "running" {
				running++
			}
			// Docker says unhealthy of a stopped container's health too:
			// it means something only while it runs.
			switch {
			case c.State == "restarting":
				bad = append(bad, c.Service+" restarting")
			case c.State == "running" && c.Health == "unhealthy":
				bad = append(bad, c.Service+" unhealthy")
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
		}
		row(key, value)
		problem := ""
		switch {
		case h.none && h.failed != "":
			problem = "its first release, " + h.failed + ", failed"
		case len(h.cut) >= 2:
			problem = "an activation was cut off past its switch, its run step perhaps never run"
		case h.mark.IsImport() && !h.importing:
			problem = "the import of " + h.mark.From + " did not complete"
		case h.mark.IsRestore():
			problem = "the restore of snapshot " + h.mark.From + " did not complete"
		case h.partial != "" && !h.mark.IsImport():
			problem = "a sync did not complete: " + h.partial
		case h.left != "":
			problem = "an operation cut off left " + h.left + " stopped"
		}
		if problem != "" {
			warn(env.Path+": "+problem, statusFix(host, env.Path))
		}
		if len(bad) > 0 {
			warn(env.Path+": "+strings.Join(bad, ", "), statusFix(host, env.Path))
		}
	}
	return lines, warnings
}

// composeNameOf is the compose name of the environment at path,
// <project>/[<instance>/]<environment>, as Target.ComposeName makes it:
// the environment's name lowercased.
func composeNameOf(path string) string {
	parts := strings.Split(path, "/")
	parts[len(parts)-1] = strings.ToLower(parts[len(parts)-1])
	return strings.Join(parts, "-")
}

// statusFix is the --status of the environment at path, in its project.
func statusFix(host, path string) string {
	project, rest, _ := strings.Cut(path, "/")
	return "in " + project + "'s checkout: devopsy @" + host + ":" + rest + " --status"
}

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
