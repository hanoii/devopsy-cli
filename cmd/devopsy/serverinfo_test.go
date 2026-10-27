package main

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
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
	var d remote.DockerFacts
	if err := parseDockerInfo([]byte(`{"ServerVersion":"29.9.1","Driver":"overlayfs","DriverStatus":[["driver-type","io.containerd.snapshotter.v1"]],"DockerRootDir":"/var/lib/docker","SecurityOptions":["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"],"CgroupVersion":"2","LoggingDriver":"json-file","HttpsProxy":"http://proxy:3128"}`), &d); err != nil {
		t.Fatal(err)
	}
	if d.Version != "29.9.1" || !d.Containerd || d.RootDir != "/var/lib/docker" || !slices.Equal(d.Security, []string{"apparmor", "seccomp", "cgroupns"}) || d.Cgroup != "v2" || d.LogDriver != "json-file" || d.Proxy != "http://proxy:3128" {
		t.Errorf("docker info: %+v", d)
	}
	d = remote.DockerFacts{}
	if err := parseDockerInfo([]byte(`{"Driver":"overlay2","DriverStatus":[["Backing Filesystem","xfs"]]}`), &d); err != nil || d.Containerd {
		t.Errorf("classic store: %+v %v", d, err)
	}
	if err := parseDockerInfo([]byte(`{"ServerErrors":["Cannot connect to the Docker daemon"]}`), &d); err == nil || !strings.Contains(err.Error(), "Cannot connect") {
		t.Errorf("daemon down: %v", err)
	}
	cs := parseHostContainers([]byte("running healthy {\"com.docker.compose.project\":\"traefik-prod\",\"com.docker.compose.service\":\"traefik\",\"devopsy.role\":\"proxy\",\"devopsy.export.BASE_DOMAIN\":\"vm1.example.com\",\"traefik.http.routers.x.rule\":\"Host(`a`, `b`)\"}\nexited - null\n"))
	if len(cs) != 2 || cs[0].Project != "traefik-prod" || cs[0].Service != "traefik" || cs[0].Health != "healthy" || len(cs[0].Labels) != 2 || cs[0].Labels["devopsy.role"] != "proxy" || cs[1].State != "exited" || cs[1].Health != "" {
		t.Errorf("containers: %+v", cs)
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
			Docker: remote.DockerFacts{Version: "29.9.1", Compose: "5.5.0", Containerd: true, Driver: "overlayfs", Cgroup: "v2",
				RootDir: "/var/lib/docker", Security: []string{"apparmor", "seccomp"}, LogDriver: "json-file",
				LogOpts: map[string]string{"max-size": "20m", "max-file": "5"}, LogOptsKnown: true},
			Disks: []remote.Disk{{Paths: []string{"releases=/home/devopsy", "docker=/var/lib/docker", "/=/"}, Type: "xfs", Size: 80e9, Free: 40e9, Used: 40e9, Reflinks: "yes"}},
			Containers: []remote.HostContainer{
				{Project: "traefik-prod", Service: "traefik", State: "running", Labels: map[string]string{"devopsy.role": "proxy", "devopsy.export.BASE_DOMAIN": "vm1.example.com"}},
				{Project: "shop-prod", Service: "app", State: "running", Health: "healthy"},
				{Project: "shop-prod", Service: "migrate", State: "exited"},
			},
			Environments: []remote.Environment{
				{Path: "traefik/prod", Status: "release\t20261026120000\t{\"mode\":\"image\",\"time\":\"2026-10-26T12:00:00Z\",\"by\":\"ci\"}\nkept\t1\nsynced\t\n"},
				{Path: "shop/prod", Status: "release\t20261027115000\t{\"mode\":\"build\",\"time\":\"2026-10-27T11:50:00Z\",\"by\":\"ana\"}\nkept\t2\nsynced\t\n"},
			},
		}
	}
	lines, warnings := serverInfoReport(healthy(), "vm1", "vm1:", "0.53.0", now)
	out := strings.Join(lines, "\n")
	for _, want := range []string{
		"devopsy      0.53.0, protocol 8\n",
		"releases     /home/devopsy, keep 5 (at most 5)",
		"snapshots    in each environment's directory, keep at most 5",
		"system       Debian GNU/Linux 13 (trixie), 6.12.48+deb13-amd64, amd64",
		"2 CPUs, 4.3 GB memory (2.1 GB available), no swap, load 0.10 0.20 0.30, up 3 days",
		"docker       29.9.1, compose 5.5.0, containerd image store (overlayfs), cgroup v2",
		"security apparmor seccomp",
		"logs json-file max-file=5 max-size=20m",
		"disks        releases /home/devopsy, docker /var/lib/docker, /: xfs, 40.0 GB free of 80.0 GB (50% used), reflinks",
		"roles        proxy: traefik-prod BASE_DOMAIN=vm1.example.com",
		"environments traefik/prod  20261026120000  24 h ago  by ci",
		"shop/prod  20261027115000  10 min ago  by ana  1 of 2 running",
	} {
		if !strings.Contains(out+"\n", want) {
			t.Errorf("healthy: no %q in:\n%s", want, out)
		}
	}
	if len(warnings) != 0 {
		t.Errorf("healthy: warnings %+v", warnings)
	}

	for _, c := range []struct {
		name   string
		change func(*remote.ServerInfo)
		local  string
		want   string // the warning, then its fix after |
	}{
		{"older devopsy", func(*remote.ServerInfo) {}, "0.54.0", "older than the local 0.54.0|devopsy @vm1: --upgrade"},
		{"reboot", func(i *remote.ServerInfo) { i.RebootRequired, i.RebootPackages = true, []string{"linux-image-amd64"} }, "", "needs a reboot (for linux-image-amd64)|reboot it"},
		{"clock", func(i *remote.ServerInfo) { i.NTP = "no" }, "", "clock is not synchronized|timedatectl set-ntp true"},
		{"docker down", func(i *remote.ServerInfo) { i.Docker = remote.DockerFacts{Error: "docker info: Cannot connect"} }, "", "Docker did not answer: docker info: Cannot connect|"},
		{"no compose", func(i *remote.ServerInfo) { i.Docker.Compose = "" }, "", "docker compose is missing|docker-compose-plugin"},
		{"no apparmor", func(i *remote.ServerInfo) { i.Docker.Security = []string{"seccomp"} }, "", "no AppArmor|install apparmor"},
		{"unbounded logs", func(i *remote.ServerInfo) { i.Docker.LogOpts = nil }, "", "grow without a limit|max-size"},
		{"full disk", func(i *remote.ServerInfo) { i.Disks[0].Free, i.Disks[0].Used = 4e9, 76e9 }, "", "/ is 95% full, 4.0 GB free|devopsy @vm1: --shell-host"},
		{"little free", func(i *remote.ServerInfo) { i.Disks[0].Size, i.Disks[0].Free, i.Disks[0].Used = 9e9, 4.5e9, 4.5e9 }, "", "is 50% full, 4.5 GB free|"},
		{"no reflinks", func(i *remote.ServerInfo) { i.Disks[0].Type, i.Disks[0].Reflinks = "ext4", "no" }, "", "(ext4) makes no reflinks|mirror: true"},
		{"snapshot root on the data", func(i *remote.ServerInfo) {
			i.SnapshotRoot = "/backups"
			i.Disks[0].Paths = append(i.Disks[0].Paths, "snapshots=/backups")
		}, "", "the snapshot root /backups is on the release root's filesystem|mount the backup volume on /backups"},
		{"missing tool", func(i *remote.ServerInfo) { i.Missing = []string{"script"} }, "", "has no script: builds and pulls show no live progress|"},
		{"role twice", func(i *remote.ServerInfo) {
			i.Containers = append(i.Containers, remote.HostContainer{Project: "other-prod", State: "running", Labels: map[string]string{"devopsy.role": "proxy"}})
		}, "", "role proxy is held by traefik-prod and other-prod|"},
		{"first release failed", func(i *remote.ServerInfo) {
			i.Environments = append(i.Environments, remote.Environment{Path: "shop/dev", Status: "none\t20261027110000\n"})
		}, "", "shop/dev: its first release, 20261027110000, failed|in shop's checkout: devopsy @vm1:dev --status"},
		{"cut", func(i *remote.ServerInfo) { i.Environments[1].Status += "cut\t20261027115000\trelease\t20261026000000\n" }, "", "shop/prod: an activation was cut off past its switch|devopsy @vm1:prod --status"},
		{"sync", func(i *remote.ServerInfo) {
			i.Environments[1].Status += "partial\t" + remote.IncompleteLine("2026-10-27 11:00:00", "vm1:prod", remote.IncompleteCopy, "the copy was cut")
		}, "", "shop/prod: a sync did not complete|"},
		{"import", func(i *remote.ServerInfo) {
			i.Environments[1].Status += "partial\t" + remote.IncompleteLine("2026-10-27 11:00:00", "db", remote.IncompleteImport, "the import was cut")
		}, "", "shop/prod: the import of db did not complete|"},
		{"left", func(i *remote.ServerInfo) { i.Environments[1].Status += "left\tdb app \n" }, "", "shop/prod: an operation cut off left db app stopped|"},
		{"restarting", func(i *remote.ServerInfo) {
			i.Containers = append(i.Containers, remote.HostContainer{Project: "shop-prod", Service: "db", State: "restarting"}, remote.HostContainer{Project: "shop-prod", Service: "worker", State: "running", Health: "unhealthy"})
		}, "", "shop/prod: db restarting, worker unhealthy|"},
	} {
		info := healthy()
		c.change(&info)
		_, warnings := serverInfoReport(info, "vm1", "vm1:", cmp.Or(c.local, "0.53.0"), now)
		msg, fix, _ := strings.Cut(c.want, "|")
		if len(warnings) != 1 || !strings.Contains(warnings[0].msg, msg) || (fix != "" && !strings.Contains(strings.Join(warnings[0].fixes, "\n"), fix)) {
			t.Errorf("%s: want one warning %q (fix %q), got %+v", c.name, msg, fix, warnings)
		}
	}

	// What is running is said, never warned: an import under way included.
	info := healthy()
	info.Environments[1].Status += "importing\npartial\t" + remote.IncompleteLine("2026-10-27 11:00:00", "db", remote.IncompleteImport, "the import was cut")
	lines, warnings = serverInfoReport(info, "vm1", "vm1:", "0.53.0", now)
	if len(warnings) != 0 || !strings.Contains(strings.Join(lines, "\n"), "1 of 2 running  importing") {
		t.Errorf("an import running: %+v\n%s", warnings, strings.Join(lines, "\n"))
	}
}

// --server-info end to end against the fake ssh: the server's facts
// read there, its environments and their state, a role, the warnings
// and exit 3.
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
	bin := filepath.Join(t.TempDir(), "bin")
	write(t, filepath.Join(bin, "docker"), `#!/bin/sh
case "$1 $2" in
  "info --format") echo '{"ServerVersion":"29.9.1","Driver":"overlayfs","DriverStatus":[["driver-type","io.containerd.snapshotter.v1"]],"DockerRootDir":"/var/lib/docker","SecurityOptions":["name=apparmor"],"CgroupVersion":"2","LoggingDriver":"local"}'; exit 0 ;;
  "compose version") echo 5.5.0; exit 0 ;;
  "ps -aq") echo c1; echo c2; exit 0 ;;
  "inspect --format") echo 'running - {"com.docker.compose.project":"traefik-prod","com.docker.compose.service":"traefik","devopsy.role":"proxy","devopsy.export.BASE_DOMAIN":"vm1.example.com"}'
    echo 'restarting - {"com.docker.compose.project":"shop-prod","com.docker.compose.service":"db"}'; exit 0 ;;
esac
exec "$(dirname "$DOCKER_LOG")/bin/docker" "$@"
`, 0o755)
	out, code := s.run([]string{"PATH=" + bin + ":" + filepath.Join(filepath.Dir(s.root), "bin") + ":/usr/bin:/bin"}, "@devopsy@one:", "--server-info")
	for _, want := range []string{
		"@devopsy@one: (devopsy@one)",
		"releases     " + s.root + ", keep 5",
		"docker       29.9.1, compose 5.5.0, containerd image store (overlayfs), cgroup v2",
		"disks        releases " + s.root,
		"roles        proxy: traefik-prod BASE_DOMAIN=vm1.example.com",
		"environments shop/dev  none: 19990101000000 failed",
		"shop/eu/prod  none: not released yet",
		"shop/prod  " + strings.TrimPrefix(s.current("prod"), "releases/") + "  just now",
		"shop/dev: its first release, 19990101000000, failed",
		"in shop's checkout: devopsy @devopsy@one:dev --status",
		"shop/prod: db restarting",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in (%d):\n%s", want, code, out)
		}
	}
	if code != serverInfoWarned || strings.Contains(out, "shop/notes") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	// It left nothing of its own in the release root.
	if m, _ := filepath.Glob(filepath.Join(s.root, ".devopsy-reflink.*")); len(m) > 0 {
		t.Errorf("left %v", m)
	}
}
