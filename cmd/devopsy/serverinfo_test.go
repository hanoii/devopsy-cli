package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/howto"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
	"github.com/hanoii/devopsy-cli/internal/testfile"
	"github.com/hanoii/devopsy-cli/internal/uids"
	"strconv"
	"syscall"
)

// The facts read on a server: os-release, /proc/meminfo, docker info,
// the containers' inspect lines.
func TestServerFactsParsers(t *testing.T) {
	t.Parallel()
	if got := osRelease("NAME=\"Debian GNU/Linux\"\nPRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\n"); got != "Debian GNU/Linux 13 (trixie)" {
		t.Errorf("os-release: %q", got)
	}
	if got := osRelease("NAME='Alpine Linux'\nVERSION=3.22\n"); got != "Alpine Linux 3.22" {
		t.Errorf("os-release without PRETTY_NAME: %q", got)
	}
	m := meminfo("MemTotal:        8000000 kB\nMemAvailable:    2000000 kB\nSwapTotal:             0 kB\nHugePages_Total:       0\n")
	if m["MemTotal"] != 8000000*1024 || m["MemAvailable"] != 2000000*1024 || m["SwapTotal"] != 0 || m["HugePages_Total"] != 0 {
		t.Errorf("meminfo: %v", m)
	}
	// Every zone's cma, never nr_free_cma's.
	if got := zoneCMA("Node 0, zone      DMA\n        managed  3840\n        cma      0\n      nr_free_cma  12\nNode 0, zone   Normal\n        managed  480000\n        cma      320512\n      nr_free_cma  300000\n"); got != 320512 {
		t.Errorf("zoneinfo: %d", got)
	}
	var d remote.DockerFacts
	if err := parseDockerInfo([]byte(`{"ServerVersion":"29.9.1","Driver":"overlayfs","DriverStatus":[["driver-type","io.containerd.snapshotter.v1"]],"DockerRootDir":"/var/lib/docker","SecurityOptions":["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"],"CgroupVersion":"2","LoggingDriver":"json-file","HttpsProxy":"http://proxy:3128"}`), &d); err != nil {
		t.Fatal(err)
	}
	if d.Version != "29.9.1" || !d.Containerd || d.RootDir != "/var/lib/docker" || !slices.Equal(d.Security, []string{"apparmor", "seccomp", "cgroupns"}) || d.Cgroup != "v2" || d.LogDriver != "json-file" || d.Proxy != "http://proxy:3128" {
		t.Errorf("docker        info: %+v", d)
	}
	d = remote.DockerFacts{}
	if err := parseDockerInfo([]byte(`{"Driver":"overlay2","DriverStatus":[["Backing Filesystem","xfs"]]}`), &d); err != nil || d.Containerd {
		t.Errorf("classic store: %+v %v", d, err)
	}
	if err := parseDockerInfo([]byte(`{"ServerErrors":["Cannot connect to the Docker daemon"]}`), &d); err == nil || !strings.Contains(err.Error(), "Cannot connect") {
		t.Errorf("daemon down: %v", err)
	}
	// /proc's hidepid, as /proc/mounts prints it: the kernel's names since 5.8 and its numbers
	// before, none printed for off; the last mount on /proc counts, never one below it (Docker
	// mounts /proc/sys and others), and no proc mount there is unknown.
	const mounts = "sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0\n%s\nproc /proc/sys proc ro,nosuid,nodev,noexec,relatime 0 0\n"
	for line, want := range map[string]string{
		"proc /proc proc rw,nosuid,nodev,noexec,relatime,gid=990,hidepid=invisible 0 0": "invisible",
		"proc /proc proc rw,nosuid,nodev,noexec,relatime,hidepid=2,gid=990 0 0":         "2",
		"proc /proc proc rw,relatime,gid=990,hidepid=ptraceable 0 0":                    "ptraceable",
		"proc /proc proc rw,relatime,gid=990,hidepid=noaccess 0 0":                      "noaccess",
		"proc /proc proc rw,relatime,hidepid=1 0 0":                                     "1",
		"proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0":                           "off",
		"proc /proc proc rw,hidepid=invisible 0 0\nproc /proc proc rw,relatime 0 0":     "off",
		"proc /proc proc rw,relatime 0 0\nproc /proc proc rw,hidepid=invisible 0 0":     "invisible",
		"tmpfs /proc tmpfs rw 0 0":                                                      "",
		"proc /var/lib/x/proc proc rw,hidepid=off 0 0":                                  "",
		"/dev/sda1 / ext4 rw,relatime,errors=remount-ro 0 0":                            "",
	} {
		if got := procHidePID(fmt.Sprintf(mounts, line)); got != want {
			t.Errorf("hidepid of %q: %q, want %q", line, got, want)
		}
	}
	for hidepid, hidden := range map[string]bool{"invisible": true, "2": true, "ptraceable": true, "4": true, "noaccess": true, "1": true, "off": false, "0": false} {
		if processesHidden(hidepid) != hidden || processesShown(hidepid) == hidden {
			t.Errorf("hidepid=%s: hidden %v, shown %v", hidepid, processesHidden(hidepid), processesShown(hidepid))
		}
	}
	// Another system, with no /proc (macOS): unknown, never said.
	if p, refused := procHidingAt(setupHost{root: t.TempDir()}, nil); p.HidePID != "" || refused != nil || processesShown(p.HidePID) {
		t.Errorf("no mounts file: %+v, %v", p, refused)
	}
	cs := parseHostContainers([]byte("running 0 healthy sha256:t1 \"0001-01-01T00:00:00Z\" {\"com.docker.compose.project\":\"traefik-prod\",\"com.docker.compose.service\":\"traefik\",\"devopsy.role\":\"proxy\",\"devopsy.export.BASE_DOMAIN\":\"vm1.example.com\",\"traefik.http.routers.x.rule\":\"Host(`a`, `b`)\"}\nexited 1 - sha256:t2 \"2026-01-01T00:00:00Z\" null\nexited 2 - sha256:t2 \"2026-01-01T00:00:00Z\" {\"com.docker.compose.project\":\"shop-prod\",\"com.docker.compose.service\":\"app\",\"com.docker.compose.oneoff\":\"True\"}\n"), nil)
	if len(cs) != 3 || cs[0].Project != "traefik-prod" || cs[0].Service != "traefik" || cs[0].Health != "healthy" || len(cs[0].Labels) != 2 || cs[0].Labels["devopsy.role"] != "proxy" || cs[0].OneOff ||
		cs[1].State != "exited" || cs[1].Health != "" || cs[1].ExitCode != 1 || !cs[2].OneOff ||
		cs[0].Image != "sha256:t1" || !cs[0].Finished.IsZero() || cs[2].Image != "sha256:t2" || !cs[2].Finished.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("containers: %+v", cs)
	}
}

// The containers are what docker lists, a container gone since its ps aside: a list docker
// failed to give is unknown, never none, so --server-info says it rather than show no role
// and no container.
func TestHostContainers(t *testing.T) {
	t.Parallel()
	line := `running 0 - sha256:t1 "0001-01-01T00:00:00Z" {"com.docker.compose.project":"shop-prod","com.docker.compose.service":"app"}`
	for name, c := range map[string]struct {
		docker string
		want   int
		err    string
	}{
		"listed":     {"case $1 in ps) echo c1 ;; inspect) echo '" + line + "' ;; esac", 1, ""},
		"none":       {"case $1 in ps) ;; inspect) exit 9 ;; esac", 0, ""},
		"gone since": {"case $1 in ps) echo c1; echo gone ;; inspect) echo '" + line + "'; echo 'Error response from daemon: No such container: gone' >&2; exit 1 ;; esac", 1, ""},
		"ps fails":   {"case $1 in ps) echo 'Cannot connect to the Docker daemon' >&2; exit 1 ;; esac", 0, "Cannot connect to the Docker daemon"},
		"inspect fails": {"case $1 in ps) echo c1; echo c2 ;; inspect) echo '" + line + "'; echo 'Error response from daemon: No such container: c2' >&2; echo 'error during connect: EOF' >&2; exit 1 ;; esac",
			1, "error during connect: EOF"},
	} {
		docker := filepath.Join(t.TempDir(), "docker")
		write(t, docker, "#!/bin/sh\n"+c.docker+"\n", 0o755)
		cs, err := hostContainers(docker, nil)
		if len(cs) != c.want || (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: %+v, %v", name, cs, err)
		}
	}
}

// What --server-info prints of the facts, and what it warns about: a
// healthy server says nothing; each problem is one warning with its fix.
func TestServerInfoReport(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	healthy := func() remote.ServerInfo {
		return remote.ServerInfo{
			Version: "0.53.0", Protocol: 8, Root: "/home/devopsy", Keep: 5, MaxKeep: 5, SnapshotMaxKeep: 5,
			OS: "Debian GNU/Linux 13 (trixie)", Kernel: "6.12.48+deb13-amd64", Arch: "amd64", CPUs: 2,
			MemTotal: 4 << 30, MemAvailable: 2 << 30, Load: "0.10 0.20 0.30", Uptime: 3 * 86400, NTP: "yes",
			AppArmor: true, AppArmorParser: true,
			Docker: remote.DockerFacts{Version: "29.9.1", Compose: "5.5.0", Buildx: "0.38.2", Containerd: true, Driver: "overlayfs", Cgroup: "v2",
				RootDir: "/var/lib/docker", Security: []string{"apparmor", "seccomp"}, LogDriver: "json-file",
				LogOpts: map[string]string{"max-size": "20m", "max-file": "5"}, LogOptsKnown: true},
			Disks: []remote.Disk{{Paths: []string{"releases=/home/devopsy", "docker=/var/lib/docker", "/=/"}, Type: "xfs", Size: 80e9, Free: 40e9, Used: 40e9, Reflinks: "yes"}},
			Containers: []remote.HostContainer{
				{Project: "traefik-prod", Service: "traefik", State: "running", Labels: map[string]string{"devopsy.role": "proxy", "devopsy.export.BASE_DOMAIN": "vm1.example.com"}},
				{Project: "shop-prod", Service: "app", State: "running", Health: "healthy"},
				{Project: "shop-prod", Service: "migrate", State: "exited"},
				// Stopped with a healthcheck: Docker says unhealthy once its
				// monitor stopped (Docker 29.8), which is no warning (review).
				{Project: "shop-prod", Service: "db", State: "exited", Health: "unhealthy"},
				// An environment's name keeps its case in its directory, its
				// compose name is lowercased (review).
				{Project: "shop-staging", Service: "app", State: "running"},
			},
			Environments: []remote.Environment{
				{Path: "traefik/prod", Status: "release\t20261026120000\t{\"mode\":\"image\",\"time\":\"2026-10-26T12:00:00Z\",\"by\":\"ci\"}\nkept\t1\nsynced\t\n"},
				{Path: "shop/prod", Status: "release\t20261027115000\t{\"mode\":\"build\",\"time\":\"2026-10-27T11:50:00Z\",\"by\":\"ana\"}\nkept\t2\nsynced\t\n"},
				{Path: "shop/Staging", Status: "release\t20261027110000\t{\"mode\":\"build\",\"time\":\"2026-10-27T11:00:00Z\",\"by\":\"ana\"}\nkept\t1\nsynced\t\n"},
			},
		}
	}
	out, warnings := serverInfoSays(healthy(), "vm1", "vm1:", "0.53.0", now)
	for _, want := range []string{
		"devopsy       0.53.0, protocol 8\n",
		"releases      /home/devopsy, keep 5 (at most 5)",
		"snapshots     in each environment's directory, keep at most 5",
		"system        Debian GNU/Linux 13 (trixie), 6.12.48+deb13-amd64, amd64",
		"2 CPUs, 4.3 GB memory (2.1 GB available), no swap, load 0.10 0.20 0.30, up 3 days",
		"              clock synchronized\n",
		"docker        29.9.1, compose 5.5.0, buildx 0.38.2, containerd image store (overlayfs), cgroup v2",
		"security apparmor seccomp",
		"logs json-file max-file=5 max-size=20m",
		"disks         releases /home/devopsy, docker /var/lib/docker, /: xfs, 40.0 GB free of 80.0 GB (50% used), reflinks",
		"roles         proxy: traefik-prod BASE_DOMAIN=vm1.example.com",
		"environments  traefik/prod  20261026120000  24 h ago  by ci",
		"shop/prod  20261027115000  10 min ago  by ana  1 of 3 running",
		"shop/Staging  20261027110000  1 h ago  by ana  1 of 1 running",
	} {
		if !strings.Contains(out+"\n", want) {
			t.Errorf("healthy: no %q in:\n%s", want, out)
		}
	}
	if len(warnings) != 0 {
		t.Errorf("healthy: warnings %+v", warnings)
	}
	// A count says one in the singular.
	one := healthy()
	one.Door = &remote.DoorFacts{Space: "/space", Users: 1}
	if out, _ := serverInfoSays(one, "vm1", "vm1:", "0.53.0", now); !strings.Contains(out, "door          /space, 1 project user\n") {
		t.Errorf("one project user:\n%s", out)
	}

	for _, c := range []struct {
		name   string
		change func(*remote.ServerInfo)
		local  string
		want   string // the warning, then its fix after |
	}{
		{"older devopsy", func(*remote.ServerInfo) {}, "0.54.0", "older than the local 0.54.0|devopsy @vm1: --upgrade"},
		{"reboot", func(i *remote.ServerInfo) { i.RebootRequired, i.RebootPackages = true, []string{"linux-image-amd64"} }, "", "the server needs a reboot (/run/reboot-required, for linux-image-amd64)|reboot it"},
		// The door: a space that takes setuid, sshd waiting for a reboot.
		{"door unguarded", func(i *remote.ServerInfo) {
			i.Door = &remote.DoorFacts{Space: "/space", Unguarded: "the project users' directory /space is not on a mount with nosuid and nodev"}
		}, "", "W0002: the project users' directory is not on a mount with nosuid and nodev\nthe project users' directory /space is not on a mount with nosuid and nodev, so the door refuses every request|set the server up for project users:\n$ devopsy @vm1: --project-users setup"},
		{"door reboot", func(i *remote.ServerInfo) { i.Door = &remote.DoorFacts{Space: "/space", Reboot: true} }, "", "sshd takes the door at the next reboot|reboot it"},
		{"door listeners", func(i *remote.ServerInfo) {
			i.Door = &remote.DoorFacts{Space: "/space", Listeners: []string{"0.0.0.0:5432 (postgres)"}}
		}, "", "every container reaches what the host listens on beyond loopback\n0.0.0.0:5432 (postgres)|make each listen on 127.0.0.1"},
		// Project users on a host that shows them every user's processes: what it exposes, and
		// the setup that hides them, as the fix.
		{"door processes shown", func(i *remote.ServerInfo) {
			i.Door, i.HidePID = &remote.DoorFacts{Space: "/space", Users: 2}, "off"
		}, "", "/proc shows project users every other user's processes (no hidepid)\ntheir command lines can hold a secret|set the server up for project users:\n$ devopsy @vm1: --project-users setup"},
		// A /proc hidden another way, judged as --project-users setup judges it, in its words.
		{"door processes hidden with no gid=", func(i *remote.ServerInfo) {
			i.Door = &remote.DoorFacts{Space: "/space", Users: 2}
			var p setup.ProcFact
			p, i.ProcRefused = procHiding("proc /proc proc rw,relatime,hidepid=2 0 0\n", false, "devopsy", nil)
			i.HidePID = p.HidePID
		}, "", "/proc hides other users' processes (hidepid=2) with no gid=, so the deploy user sees only its own|add gid= with a group the deploy user is in to /proc's options where they are set (its line in /etc/fstab)"},
		{"clock", func(i *remote.ServerInfo) { i.NTP = "no" }, "", "clock is not synchronized: certificates|timedatectl set-ntp true"},
		// Its time service on and no answer: a network blocking NTP.
		{"clock unanswered", func(i *remote.ServerInfo) {
			i.NTP, i.NTPService, i.Time = "no", "yes", now.Add(-800*time.Millisecond).UnixNano()
		}, "", "clock is not synchronized (about 0.8 s behind local)|UDP 123 blocked out?"},
		// Ubuntu 26.04's 7.0 kernel turns it on by default.
		{"kexec handover", func(i *remote.ServerInfo) { i.KHOScratch = 1252 << 20 }, "", "Kexec HandOver keeps 1.3 GB of memory as CMA, which only movable pages may use|kho=off on the kernel's command line, then a reboot; this prints the steps:\n$ devopsy --server-setup-howto kexec-handover"},
		// Docker's own, said once under docker with the docker topic, the setup's judgment of
		// them (setupDocker's refusals) not said again under setup.
		{"docker down", func(i *remote.ServerInfo) {
			i.Docker = remote.DockerFacts{Error: "docker info: Cannot connect"}
			i.Setup = setUp("")
			i.Setup.Refusals = dockerAnswers(i.Docker)
		}, "", "Docker's daemon does not answer: docker info: Cannot connect|install Docker, with its compose and buildx plugins, and start it; this prints the steps:\n$ devopsy --server-setup-howto docker"},
		{"no compose", func(i *remote.ServerInfo) {
			i.Docker.Compose = ""
			i.Setup = setUp("")
			i.Setup.Refusals = dockerAnswers(i.Docker)
		}, "", "Docker has no compose plugin|$ devopsy --server-setup-howto docker"},
		{"no buildx", func(i *remote.ServerInfo) { i.Docker.Buildx = "" }, "", "Docker has no buildx plugin|$ devopsy --server-setup-howto docker"},
		// The floor, every server's, said under docker with the docker topic, on a server with
		// no door as on one with project users.
		{"classic store", func(i *remote.ServerInfo) { i.Docker.Containerd, i.Docker.Driver = false, "overlay2" }, "",
			"Docker here uses the classic image store, and devopsy needs the containerd image store|set up Docker with the containerd image store, on a new server if this one already runs projects; this prints the steps:\n$ devopsy --server-setup-howto docker"},
		{"classic store, project users", func(i *remote.ServerInfo) {
			i.Docker.Containerd, i.Door = false, &remote.DoorFacts{Space: "/space", Users: 1}
		}, "", "Docker here uses the classic image store|$ devopsy --server-setup-howto docker"},
		{"old buildx", func(i *remote.ServerInfo) { i.Docker.Buildx = "0.29.1" }, "", "buildx here is 0.29.1, and devopsy needs 0.33.0 or later|upgrade Docker's buildx plugin; this prints the steps:\n$ devopsy --server-setup-howto docker"},
		{"old compose", func(i *remote.ServerInfo) {
			i.Docker.Compose = "2.32.4"
			i.Setup = setUp("")
			i.Setup.Refusals = dockerAnswers(i.Docker)
		}, "", "compose here is 2.32.4, and devopsy needs 2.33.0 or later|upgrade Docker's compose plugin; this prints the steps:\n$ devopsy --server-setup-howto docker"},
		// Docker lists apparmor whenever the kernel has it, and needs
		// apparmor_parser to load its profile (review).
		{"no apparmor_parser", func(i *remote.ServerInfo) { i.AppArmorParser = false }, "", "no apparmor_parser: Docker cannot load its profile, so containers fail to start|install the apparmor package"},
		{"unbounded logs", func(i *remote.ServerInfo) { i.Docker.LogOpts = nil }, "", "grow without a limit|max-size"},
		{"full disk", func(i *remote.ServerInfo) { i.Disks[0].Free, i.Disks[0].Used = 4e9, 76e9 }, "", "/ is 95% full, 4.0 GB free|devopsy @vm1: --gc"},
		{"little free", func(i *remote.ServerInfo) { i.Disks[0].Size, i.Disks[0].Free, i.Disks[0].Used = 9e9, 4.5e9, 4.5e9 }, "", "is 50% full, 4.5 GB free|"},
		{"no reflinks", func(i *remote.ServerInfo) { i.Disks[0].Type, i.Disks[0].Reflinks = "ext4", "no" }, "", "(ext4) makes no reflinks|mirror: true} for the environments with large data, in their project's .devopsy/config.yaml"},
		// The server's mirror: false refuses every project's (review).
		{"no reflinks, no mirrors", func(i *remote.ServerInfo) { i.Disks[0].Type, i.Disks[0].Reflinks, i.MirrorsOff = "ext4", "no", true }, "", "(ext4) makes no reflinks|the server's snapshots: {mirror: false} (its devopsy config) lifted, then snapshots: {mirror: true}"},
		// A full backup volume: snapshots to remove, not Docker's cache
		// (review).
		{"full snapshot disk", func(i *remote.ServerInfo) {
			i.SnapshotRoot = "/backups"
			i.Disks = append(i.Disks, remote.Disk{Paths: []string{"snapshots=/backups"}, Type: "ext4", Size: 100e9, Free: 1e9, Used: 99e9})
		}, "", "snapshots /backups is 99% full|$ devopsy @<target> --snapshots remove <id>"},
		{"snapshot root on the data", func(i *remote.ServerInfo) {
			i.SnapshotRoot = "/backups"
			i.Disks[0].Paths = append(i.Disks[0].Paths, "snapshots=/backups")
		}, "", "the snapshot root /backups is on the release root's filesystem|mount the backup volume on /backups"},
		{"missing tool", func(i *remote.ServerInfo) { i.Missing = []string{"script"} }, "", "has no script: builds and pulls show no live progress|install what is missing with the system's packages; this prints the steps:\n$ devopsy --server-setup-howto tools"},
		// The door's tools, which only --project-users add refuses without, on a server with
		// project users.
		{"missing setfacl", func(i *remote.ServerInfo) {
			i.Missing, i.Door = []string{"setfacl"}, &remote.DoorFacts{Space: "/space", Users: 1}
		}, "", "has no setfacl: no project user can be added|$ devopsy --server-setup-howto project-users"},
		// Held by a restarting container too, as releases and imports see
		// it: docker ps lists it (review).
		{"role twice", func(i *remote.ServerInfo) {
			i.Containers = append(i.Containers, remote.HostContainer{Project: "other-prod", State: "restarting", Labels: map[string]string{"devopsy.role": "proxy"}})
		}, "", "role proxy is held by traefik-prod and other-prod|"},
		{"first release failed", func(i *remote.ServerInfo) {
			i.Environments = append(i.Environments, remote.Environment{Path: "shop/dev", Status: "none\t20261027110000\n"})
		}, "", "shop/dev: none: 20261027110000 failed|$ devopsy @vm1:dev --status"},
		{"cut", func(i *remote.ServerInfo) {
			i.Environments[1].Status += "cut\t20261027115000\trelease\t20261026000000\n"
		}, "", "shop/prod: the release of 20261027115000 was cut off past its switch: its run step may not have run|devopsy @vm1:prod --status"},
		// In --status' words: a failed run step's restart is no switch.
		{"cut restart", func(i *remote.ServerInfo) {
			i.Environments[1].Status += "cut\t20261026000000\trestart\t20261027115000\n"
		}, "", "shop/prod: the restart of 20261026000000 after 20261027115000 failed was cut off: its run step may not have run|devopsy @vm1:prod --status"},
		{"sync", func(i *remote.ServerInfo) {
			i.Environments[1].Status += "partial\t" + remote.IncompleteLine("2026-10-27 11:00:00", "vm1:prod", remote.IncompleteCopy, "the copy was cut")
		}, "", "shop/prod: from a sync that did not complete: the copy was cut|"},
		{"import", func(i *remote.ServerInfo) {
			i.Environments[1].Status += "partial\t" + remote.IncompleteLine("2026-10-27 11:00:00", "db", remote.IncompleteImport, "the import was cut")
		}, "", "shop/prod: the import of db did not complete: the import was cut|"},
		{"left", func(i *remote.ServerInfo) { i.Environments[1].Status += "left\tdb app \n" }, "", "shop/prod: an operation cut off recorded db app as stopped|"},
		{"restarting", func(i *remote.ServerInfo) {
			i.Containers = append(i.Containers, remote.HostContainer{Project: "shop-prod", Service: "db", State: "restarting"}, remote.HostContainer{Project: "shop-prod", Service: "worker", State: "running", Health: "unhealthy"})
		}, "", "shop/prod: db restarting, worker unhealthy|"},
		{"paused", func(i *remote.ServerInfo) {
			i.Containers = append(i.Containers, remote.HostContainer{Project: "shop-prod", Service: "web", State: "paused"})
		}, "", "shop/prod: web paused|"},
		// Judged as --status judges it (containerTrouble): an exit on an
		// error of its own, never a one-off's.
		// The server's setup: never set up, each item not devopsy's, what refuses releases
		// there, and Docker yet to take daemon.json, each fixed by --server-setup.
		{"setup never", func(i *remote.ServerInfo) { i.Setup = setUp(""); i.Setup.Record = "" }, "", "never set up by devopsy: --upgrade applies nothing here|devopsy @vm1: --server-setup"},
		{"setup part", func(i *remote.ServerInfo) {
			i.Setup = setUp("")
			i.Setup.Parts[1].State, i.Setup.Parts[1].Observed = setup.Changed, `{"live-restore":false}`
		}, "",
			`live-restore: {"live-restore":false}, someone else's; devopsy's {"live-restore":true}|devopsy @vm1: --server-setup`},
		{"setup file", func(i *remote.ServerInfo) { i.Setup = setUp(""); i.Setup.Files[1].InSync = false }, "", "/etc/systemd/system/docker.service.d/devopsy-mounts.conf is not devopsy's|devopsy @vm1: --server-setup"},
		{"setup buffer", func(i *remote.ServerInfo) {
			i.Setup = setUp("")
			i.Setup.Buffers[0].Live, i.Setup.Buffers[0].Low = 212992, true
		}, "", "net.core.rmem_max is 212992, below 7500000|devopsy @vm1: --server-setup"},
		{"setup pending", func(i *remote.ServerInfo) { i.Setup = setUp(""); i.Setup.Pending = true }, "", "Docker: needs a restart to take daemon.json\nit waits for Docker's next restart, whoever makes it|to restart Docker sooner:\n$ devopsy @vm1: --server-setup"},
		{"setup firewall", func(i *remote.ServerInfo) {
			i.Setup = setUp("")
			i.Setup.Firewall = &setup.FirewallFact{State: setup.FirewallLoad}
		}, "", "metadata firewall: containers reach the cloud's metadata services|devopsy @vm1: --server-setup"},
		// The firewall's chains, read as root, not as devopsy loads them (a wipe): one line, its
		// files and mark in sync or not, the door's or the containers' alike.
		{"setup firewall wiped", func(i *remote.ServerInfo) {
			i.Setup = setUp("")
			i.Setup.Firewall = &setup.FirewallFact{State: setup.FirewallInSync, Door: true}
			i.Setup.Firewall.Chains([]string{"iptables: DEVOPSY-PROJECTS is not devopsy's"}, nil)
		}, "", "metadata firewall: W0001: the server's firewall is not loaded as devopsy loads it\niptables: DEVOPSY-PROJECTS is not devopsy's|devopsy @vm1: --server-setup"},
		// A project user's: what it can pass on to the server's admin, by its code, no remedy.
		{"for the admin", func(i *remote.ServerInfo) {
			i.Setup = nil
			i.ForAdmin = []string{report.FirewallNotLoaded.ID}
		}, "", "W0001: the server's firewall is not loaded as devopsy loads it\nreport W0001 to the server's admin|"},
		{"setup firewall failed", func(i *remote.ServerInfo) {
			i.Setup = setUp("")
			i.Setup.Firewall = &setup.FirewallFact{State: setup.FirewallReload, Unit: "exit-code"}
		}, "", "W0001: the server's firewall is not loaded as devopsy loads it\ndevopsy-firewall.service: exit-code|devopsy @vm1: --server-setup"},
		{"setup root", func(i *remote.ServerInfo) { i.Setup = setUp(setup.RootMake) }, "", "the release root /home/devopsy is missing: releases refuse it|devopsy @vm1: --server-setup"},
		{"setup refusal", func(i *remote.ServerInfo) {
			i.Setup = setUp("")
			i.Setup.Refusals = []setup.Refusal{{Item: "Docker", Message: "systemd is not this host's init, which --server-setup restarts Docker with"}}
		}, "", "Docker: systemd is not this host's init|"},
		{"exited on an error", func(i *remote.ServerInfo) {
			i.Containers = append(i.Containers, remote.HostContainer{Project: "shop-prod", Service: "worker", State: "exited", ExitCode: 1},
				remote.HostContainer{Project: "shop-prod", Service: "app", State: "exited", ExitCode: 2, OneOff: true})
		}, "", "shop/prod: worker exited 1|"},
		// A list that failed is unknown, never none: said, with what was read shown.
		{"containers unread", func(i *remote.ServerInfo) {
			i.Containers, i.ContainersUnread = nil, "docker ps: Cannot connect to the Docker daemon"
		}, "", "the containers could not be listed: docker ps: Cannot connect to the Docker daemon|"},
		{"environments unread", func(i *remote.ServerInfo) {
			i.EnvironmentsUnread = "open /home/devopsy/blog: permission denied"
		}, "", "not every environment could be read: open /home/devopsy/blog: permission denied|"},
	} {
		info := healthy()
		c.change(&info)
		_, warnings := serverInfoSays(info, "vm1", "vm1:", cmp.Or(c.local, "0.53.0"), now)
		msg, fix, _ := strings.Cut(c.want, "|")
		if len(warnings) != 1 || !strings.Contains(warnings[0].msg, msg) || (fix != "" && !strings.Contains(fixText(warnings[0].fixes), fix)) {
			t.Errorf("%s: want one warning %q (fix %q), got %+v", c.name, msg, fix, warnings)
		}
	}

	// What is running is said, and warned as --status warns it: an
	// import under way, and a restore, whose mark is there while it runs,
	// keep what they stopped down (envState.entries).
	for status, want := range map[string]string{
		"importing\nbusy\npartial\t" + remote.IncompleteLine("2026-10-27 11:00:00", "db", remote.IncompleteImport, "the import was cut"): "1 of 3 running  importing|shop/prod: importing db: what it stopped stays down until it completes",
		"busy\npartial\t" + remote.IncompleteLine("2026-10-27 11:00:00", "20261020000000", remote.IncompleteRestore, "restoring"):        "1 of 3 running  an operation running|shop/prod: restoring snapshot 20261020000000: what it stopped stays down until it completes",
	} {
		said, warned, _ := strings.Cut(want, "|")
		info := healthy()
		info.Environments[1].Status += status
		out, warnings = serverInfoSays(info, "vm1", "vm1:", "0.53.0", now)
		if len(warnings) != 1 || warnings[0].msg != warned || !strings.Contains(out, said) {
			t.Errorf("running (%q): %+v\n%s", said, warnings, out)
		}
	}

	// How far the server's clock is from the local one, synchronized or
	// not.
	for d, want := range map[time.Duration]string{
		50 * time.Millisecond: "matching local", -800 * time.Millisecond: "about 0.8 s behind local",
		42 * time.Second: "about 42 s ahead of local", -3 * time.Hour: "about 3 hours behind local",
	} {
		if got := clockOffset(d); got != want {
			t.Errorf("clock %v: %q, want %q", d, got, want)
		}
	}
	info0 := healthy()
	info0.Time = now.Add(42 * time.Second).UnixNano()
	if out, warnings := serverInfoSays(info0, "vm1", "vm1:", "0.53.0", now); len(warnings) != 0 || !strings.Contains(out, "clock synchronized, about 42 s ahead of local") {
		t.Errorf("clock ahead: %+v\n%s", warnings, out)
	}

	// Other users' processes are never said with them hidden (a name, a number, noaccess's
	// closed /proc/<pid>) or unknown (an older server's facts, a project user's, which hold
	// none); shown with no project user, with or without a door, they are a line with the
	// way out and no warning.
	for name, c := range map[string]struct {
		hidepid string
		door    *remote.DoorFacts
		note    bool
	}{
		"hidden":            {"invisible", &remote.DoorFacts{Users: 2}, false},
		"hidden, by number": {"2", &remote.DoorFacts{Users: 2}, false},
		"noaccess":          {"noaccess", &remote.DoorFacts{Users: 2}, false},
		"unknown":           {"", &remote.DoorFacts{Users: 2}, false},
		"no project user":   {"off", &remote.DoorFacts{}, true},
		"no door":           {"off", nil, true},
	} {
		info := healthy()
		info.HidePID = c.hidepid
		if c.door != nil {
			c.door.Space = "/space"
			info.Door = c.door
		}
		out, warnings := serverInfoSays(info, "vm1", "vm1:", "0.53.0", now)
		noted := len(warnings) == 1 && warnings[0].note && len(warnings[0].fixes) == 2 && warnings[0].fixes[0].Hint && warnings[0].fixes[1].Hint &&
			warnings[0].msg == "/proc shows every user's processes (no hidepid), which matters once the server has project users" &&
			warnings[0].fixes[0].Text == "set the server up for project users:" && slices.Equal(warnings[0].fixes[0].Cmds, []string{"devopsy @vm1: --project-users setup"}) &&
			warnings[0].fixes[1].Text == "or set it up by hand, with the steps this prints:" && slices.Equal(warnings[0].fixes[1].Cmds, []string{howto.Command(howto.Processes)})
		if c.note != noted || (!c.note && (len(warnings) != 0 || strings.Contains(out, "processes"))) {
			t.Errorf("processes, %s: %+v\n%s", name, warnings, out)
		}
	}

	// The door's tools missing (sudo, visudo, setfacl) on a server with no project user are a
	// line with the tools topic as a hint, never a warning, as a /proc without hidepid is: they
	// matter once one is added, which refuses without them. Any other tool missing is a warning.
	for name, c := range map[string]struct {
		door *remote.DoorFacts
		warn bool
	}{
		"no door":         {nil, false},
		"no project user": {&remote.DoorFacts{Space: "/space"}, false},
		"a project user":  {&remote.DoorFacts{Space: "/space", Users: 1}, true},
	} {
		info := healthy()
		info.Door, info.Missing = c.door, []string{"sudo", "setfacl"}
		out, warnings := serverInfoSays(info, "vm1", "vm1:", "0.53.0", now)
		said := len(warnings) == 2 && warnings[0].msg == "the server has no sudo: no project user can be added, nor reach Docker through the door" &&
			strings.HasPrefix(warnings[1].msg, "the server has no setfacl: ") && strings.Count(out, howto.Command(howto.ProjectUsers)) == 1
		for _, w := range warnings {
			said = said && w.note != c.warn && len(w.fixes) == 1 && w.fixes[0].Hint != c.warn && slices.Equal(w.fixes[0].Cmds, []string{howto.Command(howto.ProjectUsers)})
		}
		if !said {
			t.Errorf("the door's tools, %s: %+v\n%s", name, warnings, out)
		}
	}
	info := healthy()
	info.Missing = []string{"mkfifo"}
	if _, warnings := serverInfoSays(info, "vm1", "vm1:", "0.53.0", now); len(warnings) != 1 || warnings[0].note {
		t.Errorf("mkfifo missing, no project user: %+v", warnings)
	}

	// A project user's: Docker is the door's to answer, read not at all and no warning.
	infoDoor := healthy()
	infoDoor.Docker, infoDoor.Setup = remote.DockerFacts{Door: true}, nil
	if out, warnings := serverInfoSays(infoDoor, "vm1", "vm1:", "0.53.0", now); len(warnings) != 0 || !strings.Contains(out, "docker        the server's admin's, through the door") {
		t.Errorf("a project user's docker: %+v\n%s", warnings, out)
	}

	// What kdump keeps is a note, never a warning: crash dumps are a
	// choice.
	info = healthy()
	info.CrashKernel = 512 << 20
	out, warnings = serverInfoSays(info, "vm1", "vm1:", "0.53.0", now)
	if len(warnings) != 1 || !warnings[0].note || !strings.Contains(warnings[0].msg, "kdump keeps 536.9 MB of memory for its crash kernel") ||
		!strings.Contains(fixText(warnings[0].fixes), "crashkernel= taken off the kernel's command line and kdump's service disabled, then a reboot; this prints the steps:\n$ devopsy --server-setup-howto kdump\n") || !strings.Contains(out, "kernel keeps 536.9 MB for kdump") {
		t.Errorf("kdump: %+v\n%s", warnings, out)
	}

	// This machine's: fixes run here, no session to the server.
	info = healthy()
	info.Version = "0.54.0"
	info.Disks[0].Free, info.Disks[0].Used = 4e9, 76e9
	info.Environments = append(info.Environments, remote.Environment{Path: "shop/dev", Status: "none\t20261027110000\n"})
	_, warnings = serverInfoSays(info, "", "", "0.54.0", now)
	if len(warnings) != 2 || !strings.HasPrefix(fixText(warnings[0].fixes), "see what Docker takes, then remove what nothing on this server owns with --gc, run on it from a checkout or an alias:\n$ docker system df\n") ||
		strings.Contains(fixText(warnings[0].fixes), "--shell-host") ||
		fixText(warnings[1].fixes) != "\n$ cd /home/devopsy/shop/dev/current && devopsy --status\n" {
		t.Errorf("local: %+v", warnings)
	}

	// Set up by devopsy: where its record is, a kept part said and never warned, the volume
	// hint its lines, one not offered with why, and a hint per volume offered on the address;
	// the logs part covering json-file without max-size, said once.
	info = healthy()
	info.Setup = setUp("")
	info.Setup.Parts[2].State, info.Setup.Parts[2].Observed = setup.Kept, `{"default-address-pools":[{"base":"10.200.0.0/16","size":24}]}`
	info.Setup.Candidates = []setup.Candidate{{MountPoint: "/srv", Source: "/dev/sdb", FSType: "xfs", Size: 100e9, Free: 99e9},
		{MountPoint: "/mnt/vol 2", Source: "/dev/sdc", FSType: "ext4", Size: 50e9, Free: 49e9},
		{MountPoint: "/mnt/vol3", Source: "/dev/sdd", FSType: "ext4", Size: 10e9, Free: 9e9, Why: "nothing mounts /mnt/vol3 at boot", Fix: "add it to /etc/fstab, as root, then reload systemd:", Command: "systemctl daemon-reload"}}
	out, warnings = serverInfoSays(info, "vm1", "vm1:", "0.53.0", now)
	for _, want := range []string{"setup         by devopsy, its record /home/devopsy/.config/devopsy/server.yaml\n", `              daemon.json: default-address-pools: {"default-address-pools":[{"base":"10.200.0.0/16","size":24}]}, kept by the admin`,
		"              volumes: releases live in the deploy user's home, /home/devopsy, while releases: root names no other; volumes that could hold them:\n              /srv (xfs on /dev/sdb, 100.0 GB, 99.0 GB free)\n              /mnt/vol 2 (ext4 on /dev/sdc, 50.0 GB, 49.0 GB free)\n              /mnt/vol3 (ext4 on /dev/sdd, 10.0 GB, 9.0 GB free), not offered: nothing mounts /mnt/vol3 at boot\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("set up: no %q in:\n%s", want, out)
		}
	}
	if len(warnings) != 1 || !warnings[0].note || fixText(warnings[0].fixes) != "to keep releases on /srv:\n$ devopsy @vm1: --server-setup --root /srv\nto keep releases on /mnt/vol 2:\n$ devopsy @vm1: --server-setup --root '/mnt/vol 2'\nfor /mnt/vol3, add it to /etc/fstab, as root, then reload systemd:\n$ systemctl daemon-reload\n" {
		t.Errorf("set up: %+v", warnings)
	}
	// Locally, with no address to name, the hint is words.
	if es := volumeEntries(*info.Setup, "", "setup"); len(es) != 1 || fixText(es[0].Remedies) != "for /mnt/vol3, add it to /etc/fstab, as root, then reload systemd:\n$ systemctl daemon-reload\nto keep releases on a volume, run --server-setup for this server, as its deploy user, with --root and its mount point\n" {
		t.Errorf("locally: %+v", es)
	}
	// Every volume refused: the hint still says each one and why, with its fix and no --root.
	only := *info.Setup
	only.Candidates = only.Candidates[2:]
	if es := volumeEntries(only, "vm1:", "setup"); len(es) != 1 || !strings.Contains(strings.Join(es[0].Lines, "\n"), "/mnt/vol3 (ext4 on /dev/sdd, 10.0 GB, 9.0 GB free), not offered: nothing mounts /mnt/vol3 at boot") || fixText(es[0].Remedies) != "for /mnt/vol3, add it to /etc/fstab, as root, then reload systemd:\n$ systemctl daemon-reload\n" {
		t.Errorf("none offered: %+v", es)
	}
	// A root the config names, one named by this run or a root holding releases is an answer:
	// no volume hint.
	for name, change := range map[string]func(f *setup.Facts){
		"named":    func(f *setup.Facts) { f.RootNamed = true },
		"naming":   func(f *setup.Facts) { f.NamesRoot = true },
		"releases": func(f *setup.Facts) { f.Projects = true },
	} {
		f := *info.Setup
		change(&f)
		if es := volumeEntries(f, "vm1:", "setup"); es != nil {
			t.Errorf("%s: a hint %+v", name, es)
		}
	}
	info.Setup.Candidates = nil
	info.Docker.LogOpts = nil
	info.Setup.Parts[0].State, info.Setup.Parts[0].Observed = setup.Changed, `{"log-driver":"json-file"}`
	if _, warnings = serverInfoSays(info, "vm1", "vm1:", "0.53.0", now); len(warnings) != 1 || !strings.Contains(warnings[0].msg, "logs: ") {
		t.Errorf("json-file without max-size, judged as the logs part: %+v", warnings)
	}
	info.Setup.Record = ""
	if _, warnings = serverInfoSays(info, "", "", "0.53.0", now); len(warnings) != 2 || fixText(warnings[0].fixes) != "run --server-setup for this server, as its deploy user\n" {
		t.Errorf("never set up, locally: %+v", warnings)
	}

	// Items one --server-setup fixes are each said, the command offered once after the last of
	// them, each still its own warning with its fix in --json; a line with a fix of its own (a
	// missing plugin, whose refusal stops nothing) keeps it under it.
	info = healthy()
	info.Setup = setUp("")
	info.Setup.Record = ""
	info.Setup.Files[0].InSync, info.Setup.Files[1].InSync = false, false
	info.Setup.Firewall = &setup.FirewallFact{State: setup.FirewallLoad}
	info.Setup.Refusals = []setup.Refusal{{Item: "Docker", Message: "Docker has no compose plugin", Fix: "install Docker's compose plugin", Stops: setup.RefusalPlugins}}
	out, warnings = serverInfoSays(info, "vm1", "vm1:", "0.53.0", now)
	want := "setup         never set up by devopsy: --upgrade applies nothing here\n" +
		"              Docker: Docker has no compose plugin\n              to fix it:\n              install Docker's compose plugin\n" +
		"              socket buffers: /etc/sysctl.d/90-devopsy-net.conf is not devopsy's\n" +
		"              mounts drop-in: /etc/systemd/system/docker.service.d/devopsy-mounts.conf is not devopsy's\n" +
		"              metadata firewall: containers reach the cloud's metadata services"
	if !strings.Contains(out, want) || strings.Count(out, "$ devopsy @vm1: --server-setup") != 1 ||
		!strings.Contains(out[strings.Index(out, "metadata firewall"):], "\n              to fix it:\n              $ devopsy @vm1: --server-setup\n") {
		t.Errorf("the setup's one fix, once:\n%s", out)
	}
	if len(warnings) != 5 || slices.ContainsFunc(warnings, func(w said) bool { return len(w.fixes) != 1 }) {
		t.Errorf("each warning with its own fix: %+v", warnings)
	}

	// Docker yet to take daemon.json: the restart it needs and the parts it takes, then what
	// does it. With no reboot asked for it waits for whoever restarts Docker, a warning; with
	// the reboot the setup asked for, the reboot does it and nothing is lost by waiting, a line
	// beside the reboot's own warning. --server-setup restarts it sooner either way, a hint.
	for _, reboot := range []bool{false, true} {
		info = healthy()
		info.Setup = setUp("")
		info.Setup.Pending, info.Setup.PendingParts, info.Setup.Reboot = true, []string{"live-restore"}, reboot
		info.RebootRequired, info.RebootPackages = reboot, []string{"devopsy"}
		out, warnings = serverInfoSays(info, "vm1", "vm1:", "0.53.0", now)
		detail, warned := "it waits for Docker's next restart, whoever makes it", 1
		if reboot {
			detail = "nothing is lost by waiting: the reboot the server needs (/run/reboot-required) does it"
		}
		want := "              Docker: needs a restart to take daemon.json (live-restore)\n              " + detail + "\n              to restart Docker sooner:\n              $ devopsy @vm1: --server-setup\n"
		n := 0
		for _, w := range warnings {
			if !w.note {
				n++
			}
		}
		if !strings.Contains(out, want) || n != warned || reboot != strings.Contains(out, "the server needs a reboot (/run/reboot-required, for devopsy)\n") {
			t.Errorf("pending, reboot %v: %d warnings %+v\n%s", reboot, n, warnings, out)
		}
	}

	// A kernel without AppArmor is said, not warned: nothing to install.
	info = healthy()
	info.Docker.Security, info.AppArmor, info.AppArmorParser = []string{"seccomp"}, false, false
	out, warnings = serverInfoSays(info, "vm1", "vm1:", "0.53.0", now)
	if len(warnings) != 0 || !strings.Contains(out, "security seccomp (no AppArmor in this kernel)") {
		t.Errorf("no AppArmor in the kernel: %+v\n%s", warnings, out)
	}

	// Without flock every lock reads held: no environment is busy, its
	// mark warned (review).
	info = healthy()
	info.Missing = []string{"flock"}
	info.Environments[1].Status += "busy\npartial\t" + remote.IncompleteLine("2026-10-27 11:00:00", "db", remote.IncompleteImport, "the import was cut")
	out, warnings = serverInfoSays(info, "vm1", "vm1:", "0.53.0", now)
	if len(warnings) != 2 || !strings.Contains(warnings[1].msg, "shop/prod: the import of db did not complete") || strings.Contains(out, "an operation running") {
		t.Errorf("no flock: %+v\n%s", warnings, out)
	}

	// Every problem of an environment is warned, as --status says each
	// (review: the first alone was), in one warning with the --status
	// that shows them.
	info = healthy()
	info.Environments = append(info.Environments, remote.Environment{Path: "shop/dev", Status: "none\t20261027110000\npartial\t" + remote.IncompleteLine("2026-10-27 11:00:00", "vm1:prod", remote.IncompleteCopy, "the copy was cut")})
	if _, warnings = serverInfoSays(info, "vm1", "vm1:", "0.53.0", now); len(warnings) != 1 || strings.Count(warnings[0].msg, "shop/dev: ") != 2 || len(warnings[0].fixes) != 1 {
		t.Errorf("two problems of shop/dev, one warning with its fix: %+v", warnings)
	}
}

// said is a warning --server-info shows, or a note (said with its
// fixes, never warned).
type said struct {
	msg   string
	fixes []report.Remedy
	note  bool
}

// serverInfoSays is serverInfoReport's entries as --server-info shows
// them, uncolored, and its warnings and notes, each its lines.
func serverInfoSays(info remote.ServerInfo, host, ref, local string, now time.Time) (string, []said) {
	es := serverInfoReport(info, host, ref, local, now)
	var b strings.Builder
	newEntryTable(&b, style{}, es).print(es)
	var ws []said
	for _, e := range es {
		if e.Level == remote.StatusWarning || len(e.Remedies) > 0 {
			ws = append(ws, said{strings.Join(e.Lines, "\n"), e.Remedies, e.Level != remote.StatusWarning})
		}
	}
	return b.String(), ws
}

// fixText is fixes as one text, the commands after their words.
func fixText(fixes []report.Remedy) string {
	var b strings.Builder
	for _, f := range fixes {
		b.WriteString(f.Text + "\n")
		for _, c := range f.Cmds {
			b.WriteString("$ " + c + "\n")
		}
	}
	return b.String()
}

// firstWrite as a command's stdout, the way a session's facts reach it:
// exec.Cmd copies into it with io.Copy, which prefers a ReadFrom to Write.
func TestFirstWriteAsCommandStdout(t *testing.T) {
	t.Parallel()
	out := &firstWrite{}
	before := time.Now()
	cmd := exec.Command("echo", "hello")
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if out.at.Before(before) || out.at.After(time.Now()) {
		t.Errorf("first write at %v, want since %v", out.at, before)
	}
	if got := out.buf.Bytes(); string(got) != "hello\n" {
		t.Errorf("output %q", got)
	}
}

// --server-info end to end against the fake ssh: the server's facts
// read there, its environments and their state, a role, the warnings
// and exit 3, as text and as JSON.
func TestServerInfo(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	// A first release that failed.
	write(t, filepath.Join(s.root, "shop", "dev", "releases", "19990101000000", remote.FailedFile), "", 0o644)
	// An instance's environment, and what is no environment.
	if err := os.MkdirAll(filepath.Join(s.root, "shop", "eu", "prod", "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.root, "shop", "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	// What a --destroy cut off while removing an environment left (review): no environment,
	// said as what a --destroy run again resumes. Its pid is none a system gives.
	if err := os.MkdirAll(filepath.Join(s.root, "shop", "old.rm.999999999", "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "bin")
	// Ubuntu 26.04's tail is uutils', which the scripts use as GNU's: no
	// tool missing (review).
	write(t, filepath.Join(bin, "tail"), "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'tail (uutils coreutils) 0.2.2'; exit 0; fi\nexec /usr/bin/tail \"$@\"\n", 0o755)
	write(t, filepath.Join(bin, "timedatectl"), "#!/bin/sh\nprintf 'NTP=yes\\nNTPSynchronized=no\\n'\n", 0o755)
	write(t, filepath.Join(bin, "docker"), `#!/bin/sh
case "$1 $2" in
  "info --format") echo '{"ServerVersion":"29.9.1","Driver":"overlayfs","DriverStatus":[["driver-type","io.containerd.snapshotter.v1"]],"DockerRootDir":"/var/lib/docker","SecurityOptions":["name=apparmor"],"CgroupVersion":"2","LoggingDriver":"local"}'; exit 0 ;;
  "compose version") echo 5.5.0; exit 0 ;;
  "ps -aq") echo c1; echo c2; echo gone; exit 0 ;;
  # A container gone since ps: inspect prints the others, then fails
  # (review: every container was dropped then).
  "inspect --format") echo 'running 0 - sha256:t2 "0001-01-01T00:00:00Z" {"com.docker.compose.project":"traefik-prod","com.docker.compose.service":"traefik","devopsy.role":"proxy","devopsy.export.BASE_DOMAIN":"vm1.example.com"}'
    echo 'restarting 0 - sha256:t2 "0001-01-01T00:00:00Z" {"com.docker.compose.project":"shop-prod","com.docker.compose.service":"db"}'
    # A release's container of an environment removed by hand: compose worked in the root.
    echo 'exited 0 - sha256:t2 "2026-01-01T00:00:00Z" {"com.docker.compose.project":"shop-gone","com.docker.compose.service":"web","com.docker.compose.project.working_dir":"`+s.root+`/shop/gone/releases/1/code"}'
    echo 'Error response from daemon: No such container: gone' >&2; exit 1 ;;
  # Counted, --server-info sizes nothing of Docker's but its images.
  "system df"|"buildx ls") echo "docker $*: not for --server-info" >&2; exit 9 ;;
  "image ls") echo 'sha256:t1 busybox:1.37 4.26MB'; exit 0 ;;
  "volume ls") exit 0 ;;
esac
exec "$(dirname "$DOCKER_LOG")/bin/docker" "$@"
`, 0o755)
	// The host's own files are a host root of the test's: no systemd, nothing set up.
	host := t.TempDir()
	out, code := s.run([]string{"PATH=" + bin + ":" + filepath.Join(filepath.Dir(s.root), "bin") + ":/usr/bin:/bin", remote.HostRootEnv + "=" + host}, "@devopsy@one:", "--server-info")
	for _, want := range []string{
		"@devopsy@one:\n",
		"releases      " + s.root + ", keep 5",
		"containerd /var/lib/containerd",
		"docker        29.9.1, compose 5.5.0, buildx 0.38.2, containerd image store (overlayfs), cgroup v2",
		"disks         releases " + s.root,
		"roles         proxy: traefik-prod BASE_DOMAIN=vm1.example.com",
		"environments  shop/dev  none: 19990101000000 failed",
		"shop/eu/prod  none: not released yet",
		"shop/prod  " + strings.TrimPrefix(s.current("prod"), "releases/") + "  just now",
		"shop/dev: none: 19990101000000 failed",
		"in shop's checkout:\n              $ devopsy @devopsy@one:dev --status",
		"shop/prod: db restarting",
		// The server's clock, taken last and compared on arrival.
		"clock not synchronized, ",
		"$ timedatectl timesync-status",
		// The setup as --server-setup judges it, with its fixes: no systemd here refuses every
		// step, so --server-setup is offered only past that refusal, once after the last line
		// it fixes.
		"setup         never set up by devopsy: --upgrade applies nothing here\n              Docker: ",
		"(its instance's credentials)\n              to fix it:\n              fix what refuses every step of the setup first, then run --server-setup\n",
		"              Docker: systemd is not this host's init",
		"              daemon.json: logs: none, devopsy's",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in (%d):\n%s", want, code, out)
		}
	}
	for _, want := range []string{
		"images        busybox:1.37, 4.3 MB: no release holds it, nor any container uses it\n              remove it once nothing needs it, on the server:\n              $ devopsy @devopsy@one: --shell-host\n              $ docker rmi busybox:1.37",
		"gone          shop-gone: no environment and no uid claim on the server, and left:\n              containers web (exited)",
		"environments  shop/old.rm.999999999: left by a --destroy cut off while it removed shop/old, 0 B\n              run it again, which resumes it, in shop's checkout:\n              $ devopsy @devopsy@one:old --destroy",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in (%d):\n%s", want, code, out)
		}
	}
	if code != serverInfoWarned || strings.Contains(out, "shop/notes") || strings.Contains(out, "old.rm.999999999  ") || strings.Contains(out, "has no tail") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	// The fake ssh shares this clock: no first byte seen reads as days off.
	if strings.Contains(out, "days ahead") {
		t.Errorf("clock offset:\n%s", out)
	}
	// It left nothing of its own in the release root.
	if m, _ := filepath.Glob(filepath.Join(s.root, ".devopsy-reflink.*")); len(m) > 0 {
		t.Errorf("left %v", m)
	}
	// --json: the facts and what it shows of them, each warning with its
	// fixes, alone on the output (nothing said around it) and exit 3.
	out, code = s.run([]string{"PATH=" + bin + ":" + filepath.Join(filepath.Dir(s.root), "bin") + ":/usr/bin:/bin"}, "@devopsy@one:", "--server-info", "--json")
	var r serverReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("--json (%d): %v:\n%s", code, err, out)
	}
	var fixes []string
	for _, e := range r.Entries {
		if e.Level == remote.StatusWarning && e.Key == "environments" {
			for _, f := range e.Remedies {
				fixes = append(fixes, f.Cmds...)
			}
		}
	}
	if code != serverInfoWarned || !r.Attention || r.Server != "devopsy@one:" || r.Facts.Root != s.root || len(r.Facts.Environments) == 0 ||
		r.Facts.GC == nil || len(r.Facts.Images) != 1 || len(r.Facts.Removing) != 1 ||
		!slices.Contains(fixes, "devopsy @devopsy@one:dev --status") {
		t.Errorf("--json (%d): %+v", code, r)
	}

	// Without a target, the Linux server it runs on, the same facts read
	// in process; refused elsewhere (macOS: CI runs both).
	out, code = s.run([]string{"PATH=" + bin + ":" + filepath.Join(filepath.Dir(s.root), "bin") + ":/usr/bin:/bin"}, "--server-info")
	if runtime.GOOS != "linux" {
		if code != 1 || !strings.Contains(out, "reads the Linux server it runs on, and this is "+runtime.GOOS) || !strings.Contains(out, "$ devopsy @<server> --server-info") || strings.Contains(out, "local (") {
			t.Errorf("not on Linux: exit %d:\n%s", code, out)
		}
		return
	}
	for _, want := range []string{
		"local (",
		"releases      " + s.root + ", keep 5",
		"roles         proxy: traefik-prod BASE_DOMAIN=vm1.example.com",
		"environments  shop/dev  none: 19990101000000 failed",
		"$ cd " + filepath.Join(s.root, "shop", "dev", "current") + " && devopsy --status",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("local: no %q in (%d):\n%s", want, code, out)
		}
	}
	if code != serverInfoWarned {
		t.Errorf("local: exit %d:\n%s", code, out)
	}
}

// Environments are found as the scripts reach them, through links too:
// a project or an environment moved to another disk and linked back
// (review).
func TestServerInfoEnvironments(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root, elsewhere := filepath.Join(tmp, "root"), filepath.Join(tmp, "data")
	for _, d := range []string{
		filepath.Join(elsewhere, "shop", "prod", "releases"),
		filepath.Join(elsewhere, "blog-prod", "releases"),
		filepath.Join(root, "blog"),
		filepath.Join(root, "app", "prod", "releases"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(elsewhere, "shop"), filepath.Join(root, "shop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "blog-prod"), filepath.Join(root, "blog", "prod")); err != nil {
		t.Fatal(err)
	}
	paths := func(envs []remote.Environment, err error) []string {
		if err != nil {
			t.Error(err)
		}
		var got []string
		for _, e := range envs {
			got = append(got, e.Path)
		}
		return got
	}
	if got := paths(environments(root, nil, nil, ownedByMe)); !slices.Equal(got, []string{"app/prod", "blog/prod", "shop/prod"}) {
		t.Errorf("environments: %v", got)
	}
	// A root not made yet (a server before its first release) holds none.
	if got := paths(environments(filepath.Join(tmp, "none"), nil, nil, ownedByMe)); len(got) != 0 {
		t.Errorf("no root: %v", got)
	}
	// A directory it cannot read is unknown, never none: what it read comes with the error,
	// which --server-info says. Root reads it all the same.
	if os.Getuid() != 0 {
		blog := filepath.Join(root, "blog")
		if err := os.Chmod(blog, 0); err != nil {
			t.Fatal(err)
		}
		envs, err := environments(root, nil, nil, ownedByMe)
		if err := os.Chmod(blog, 0o755); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range envs {
			got = append(got, e.Path)
		}
		if err == nil || !strings.Contains(err.Error(), blog) || !slices.Equal(got, []string{"app/prod", "shop/prod"}) {
			t.Errorf("an unreadable project: %v, %v", got, err)
		}
	}
	// A link to no directory that a uid claim names an environment under (a project's or an
	// environment's disk not mounted) is unknown, never none: an environment there would read
	// as gone, and --server-info would offer to remove what is left of it, its uid claim's file
	// included. What it read comes with the error. One no claim names is a stray link, skipped:
	// the root's top level is by default the deploy user's home.
	for _, l := range []string{filepath.Join(root, "wiki"), filepath.Join(root, "app", "stage")} {
		if err := os.Symlink(filepath.Join(elsewhere, "gone"), l); err != nil {
			t.Fatal(err)
		}
	}
	if got := paths(environments(root, nil, nil, ownedByMe)); !slices.Equal(got, []string{"app/prod", "blog/prod", "shop/prod"}) {
		t.Errorf("stray links to no directory: %v", got)
	}
	for _, c := range []struct {
		claim uids.Owner
		link  string
	}{{uids.Owner{Project: "wiki", Env: "prod"}, filepath.Join(root, "wiki")}, {uids.Owner{Project: "app", Env: "stage"}, filepath.Join(root, "app", "stage")}} {
		claims := uids.StoreAt(root)
		if _, err := claims.Lookup(c.claim); err != nil {
			t.Fatal(err)
		}
		if envs, err := environments(root, nil, nil, ownedByMe); err == nil || !strings.Contains(err.Error(), c.link+" links to no directory") || len(envs) != 3 {
			t.Errorf("a link to no directory a claim names: %v, %v", envs, err)
		}
		if err := os.Remove(filepath.Join(claims.Dir, c.claim.String())); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(c.link); err != nil {
			t.Fatal(err)
		}
	}
	// A project user's tree is never the deploy user's to read, a link there least of all:
	// its environments come from what that user reads (--door-read), and nothing of the
	// deploy user's is walked through a link the project user put there.
	secret := filepath.Join(t.TempDir(), "deploy-only")
	if err := os.MkdirAll(filepath.Join(secret, "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "app", "stolen")); err != nil {
		t.Fatal(err)
	}
	asked := ""
	var got []string
	envs, _ := environments(root, map[string]string{filepath.Join(root, "app"): "devopsy-app"}, func(user, tree string) []remote.Environment {
		asked = user + " " + tree
		return []remote.Environment{{Path: "app/prod", Status: "as its user\n"}}
	}, ownedByMe)
	for _, e := range envs {
		got = append(got, e.Path+"="+e.Status)
	}
	if asked != "devopsy-app "+filepath.Join(root, "app") || !slices.Contains(got, "app/prod=as its user\n") || slices.ContainsFunc(got, func(g string) bool { return strings.HasPrefix(g, "app/stolen") }) {
		t.Errorf("a project user's tree (asked %q): %v", asked, got)
	}
	// A project user's tree outside the release root, in its own home by default, is read too.
	tree := filepath.Join(t.TempDir(), "devopsy-shop", "shop")
	got = paths(environments(root, map[string]string{tree: "devopsy-shop"}, func(user, tree string) []remote.Environment {
		return []remote.Environment{{Path: "shop/prod", Status: "as " + user + "\n"}}
	}, ownedByMe))
	if !slices.Contains(got, "app/prod") || !slices.Contains(got, "shop/prod") {
		t.Errorf("a project user's tree in its own home: %v", got)
	}
}

// What a project user's read answers for its tree is that user's to write: only an
// environment of its project, under names devopsy gives, is listed, since an admin's
// --server-info offers a command with it.
func TestEnvPathIn(t *testing.T) {
	t.Parallel()
	for path, ok := range map[string]bool{
		"shop/prod": true, "shop/eu/prod": true, "shop/stage.2": true, "shop/Stage.2": false,
		"blog/prod": false, "shop": false, "shop/a/b/c": false, "shop/prod$(touch x)": false,
		"shop/prod;id": false, "shop/it's": false, "shop/../blog": false, "shop/EU/prod": false, "shop/-x": false,
	} {
		if envPathIn("shop", path) != ok {
			t.Errorf("%q: %v", path, !ok)
		}
	}
}

// --server-info's help and docs/servers.md name every container trouble
// it warns about (containerTrouble), dead and exited on an error too.
func TestServerInfoDocsNameContainerTroubles(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "servers.md"))
	if err != nil {
		t.Fatal(err)
	}
	for where, text := range map[string]string{
		"--server-info's help": cli.RemoteCommandHelp["--server-info"],
		"docs/servers.md":      string(doc),
	} {
		text = strings.Join(strings.Fields(text), " ")
		for _, trouble := range []string{containerTrouble("dead", "", 0), "exited"} {
			if !strings.Contains(text, trouble) {
				t.Errorf("%s does not say a container %s warns", where, trouble)
			}
		}
	}
}

// The command a hint offers to copy for an environment on this machine quotes its path when
// a shell would read any of it: the path is a project user's to name, and a copy must run
// cd and --status, nothing it adds.
func TestStatusHintQuotesItsPath(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"shop/prod":          "cd /srv/shop/prod/current && devopsy --status",
		"shop/prod;rm -rf ~": "cd '/srv/shop/prod;rm -rf ~/current' && devopsy --status",
		"shop/it's":          `cd '/srv/shop/it'\''s/current' && devopsy --status`,
	} {
		if got := statusHint("", "/srv", remote.Environment{Path: path}).Cmds; len(got) != 1 || got[0] != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
	// On a server, the address it offers quotes the environment the same way.
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	for _, env := range []string{"prod$(touch ran)", "prod;touch ran", "it's"} {
		cmds := statusHint("vm1", "/srv", remote.Environment{Path: "shop/" + env}).Cmds
		if len(cmds) != 1 {
			t.Fatalf("%q: commands %q", env, cmds)
		}
		// The command as a shell reads it, devopsy printing its arguments.
		sh := exec.Command("sh", "-c", `devopsy() { printf '%s\n' "$@"; }`+"\n"+cmds[0])
		sh.Dir = dir
		out, err := sh.CombinedOutput()
		if want := "@vm1:" + env + "\n--status\n"; err != nil || string(out) != want || fileExists(marker) {
			t.Errorf("%q: the offered command %q gives a shell %q (%v, ran what the name holds: %v), want %q", env, cmds[0], out, err, fileExists(marker), want)
		}
		os.Remove(marker)
	}
}

// --server-info offers a project user's environment's --status as that user, from the
// project's checkout, never `cd <tree> && devopsy --status` for whoever reads it on the
// server, the deploy user: devopsy there would load that user's project and run its programs
// (the env capability, a status capability) with the deploy user's Docker socket, and whether
// the warning shows is that user's to choose, its state lines being its own read's answer.
// The environment's user is the deploy user's devopsy's to set, never the read's.
func TestServerInfoOffersNoStatusInAProjectUsersTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tree := filepath.Join(root, "shop")
	info := remote.ServerInfo{Version: "0.53.0", Root: root}
	// As serverFacts lists them: the tree read as its user, who answers a first release
	// that failed, which --server-info warns of.
	envs, err := environments(root, map[string]string{tree: "devopsy-shop"}, func(user, tree string) []remote.Environment {
		return []remote.Environment{{Path: "shop/prod", Status: "none\t20261027110000\n", User: ""}}
	}, ownedByMe)
	if err != nil {
		t.Fatal(err)
	}
	info.Environments = envs
	_, warnings := serverInfoSays(info, "", "", "0.53.0", time.Now())
	warned := false
	for _, w := range warnings {
		if !strings.Contains(w.msg, "shop/prod") {
			continue
		}
		warned = true
		for _, f := range w.fixes {
			for _, c := range f.Cmds {
				if strings.HasPrefix(c, "cd "+filepath.Join(tree, "prod", "current")+" ") && strings.Contains(c, "devopsy") {
					t.Errorf("offered to whoever reads --server-info on the server, the deploy user: %s", c)
				}
				if c != "devopsy @devopsy-shop@<server>:prod --status" {
					t.Errorf("offered for a project user's environment: %s", c)
				}
			}
		}
	}
	if !warned {
		t.Fatalf("the control: no warning of shop/prod: %+v", warnings)
	}
}

// Asked of a server, the address offered names the project user on that server.
func TestStatusHintNamesTheProjectUser(t *testing.T) {
	t.Parallel()
	for host, want := range map[string]string{"devopsy@vm1": "devopsy @devopsy-shop@vm1:prod --status", "vm1": "devopsy @devopsy-shop@vm1:prod --status"} {
		if got := statusHint(host, "/srv", remote.Environment{Path: "shop/prod", User: "devopsy-shop"}).Cmds; len(got) != 1 || got[0] != want {
			t.Errorf("%s: %q, want %q", host, got, want)
		}
	}
}

// --server-info gives up on a project user's read of its tree that does not answer in time
// (treeEnvironments, doorAnswer), its environments left out and the rest of the server's facts
// still coming: that read runs as the project user, on files it writes, and a FIFO where an
// environment's state file goes blocks it on its open. The read's own bound is a second
// here; the test's, the behavior's, ten.
//
// Not parallel: it sets PATH, for a sudo that never answers.
func TestServerInfoWaitsForNoProjectUsersRead(t *testing.T) {
	bin := t.TempDir()
	pid := filepath.Join(bin, "pid")
	if err := testfile.Write(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\necho $$ >"+pid+"\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	tree := filepath.Join(t.TempDir(), "shop")
	done, ended := make(chan []remote.Environment, 1), make(chan struct{})
	go func() { done <- treeEnvironments("devopsy-shop", tree, time.Second); close(ended) }()
	// The read that never answers goes with the test, whatever came of it.
	t.Cleanup(func() {
		data, _ := os.ReadFile(pid)
		if p, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
		select {
		case <-ended:
		case <-time.After(startWait):
		}
	})
	select {
	case envs := <-done:
		if len(envs) != 0 {
			t.Errorf("a read that never answered gave environments: %v", envs)
		}
	case <-time.After(10 * time.Second):
		t.Error("--server-info still waits for a project user's read that never answers")
	}
}

// Once --project-users remove took a project user's entry, the deploy user still never walks
// that user's tree in the release root, which remove keeps: whose a directory is comes from
// its owner (owned), never from the door's list alone. Else remote.EnvStatus would read its
// files with no bound (a FIFO where a release's record goes), follow its links, and statusHint
// offer `cd <tree>/<environment>/current && devopsy --status`, devopsy running what that user
// wrote. The tree here is the test's own user's, so owned names it another's.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the door's entries are.
func TestServerInfoReadsNoTreeOfARemovedProjectUser(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	root := t.TempDir()
	tree := filepath.Join(root, "shop")
	if err := os.MkdirAll(filepath.Join(tree, "prod", "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "config.yaml"), "door:\n  space: "+t.TempDir()+"\n", 0o600)
	entry := filepath.Join(remote.DoorDir(), "users", "devopsy-shop.yaml")
	write(t, entry, "project: shop\ntree: "+tree+"\n", 0o600)
	// walk is --server-info's list: how many trees it asked their user for, and the
	// environments the deploy user read itself.
	walk := func() (asked int, own []string) {
		owned := func(p string) bool { return p != tree }
		envs, err := environments(root, doorTrees(root), func(string, string) []remote.Environment { asked++; return nil }, owned)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range envs {
			if e.User == "" {
				own = append(own, e.Path)
			}
		}
		return asked, own
	}
	// The control: with its entry, the tree is read as its user alone.
	if asked, own := walk(); asked != 1 || len(own) != 0 {
		t.Fatalf("with its entry: %d trees asked of their user, the deploy user read %v", asked, own)
	}
	d, err := remote.ServerDoor()
	if err != nil {
		t.Fatal(err)
	}
	if code := removeProjectUser(d, "shop", "devopsy-shop", entry, func(string) error { return nil }); code != 0 {
		t.Fatalf("remove: exit %d", code)
	}
	if _, own := walk(); len(own) != 0 {
		t.Errorf("after --project-users remove, the deploy user reads the tree its Unix user still owns: %v", own)
	}
}

// setUp is a server's setup as devopsy left it, every item devopsy's, the release root's
// action rootAction.
func setUp(rootAction string) *setup.Facts {
	f := &setup.Facts{Root: "/home/devopsy", RootAction: rootAction, Record: "/home/devopsy/.config/devopsy/server.yaml",
		Files: []setup.FileFact{{Item: setup.ItemBuffers, Path: setup.SysctlFile, InSync: true}, {Item: setup.ItemMounts, Path: setup.MountsDropIn, InSync: true}}}
	for _, p := range setup.Parts {
		f.Parts = append(f.Parts, setup.PartFact{Name: p.Name, State: setup.InSync, Observed: p.Want, Want: p.Want})
	}
	for _, k := range setup.BufferKeys {
		f.Buffers = append(f.Buffers, setup.BufferFact{Key: k, Live: setup.BufferSize})
	}
	return f
}

// A tool the server lacks is said once, as --server-info says every tool it needs: the
// setup's facts carry the required ones' refusal too, which says nothing more.
func TestServerInfoSaysAMissingToolOnce(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	info := remote.ServerInfo{Version: "0.53.0", Protocol: remote.Protocol, Root: "/home/devopsy", NTP: "yes", Missing: []string{"tar"}}
	info.Setup = setUp("")
	if r := toolsRefusalOf([]string{"tar"}); r != nil {
		info.Setup.Refusals = []setup.Refusal{*r}
	}
	_, warnings := serverInfoSays(info, "vm1", "vm1:", "0.53.0", now)
	var tar []string
	for _, w := range warnings {
		if !w.note && strings.Contains(w.msg, "tar") {
			tar = append(tar, w.msg)
		}
	}
	if len(tar) != 1 {
		t.Errorf("a missing tar warned %d times: %q", len(tar), tar)
	}
}
