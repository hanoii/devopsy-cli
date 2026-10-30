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
	"github.com/hanoii/devopsy-cli/internal/report"
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
			AppArmor: true, AppArmorParser: true,
			Docker: remote.DockerFacts{Version: "29.9.1", Compose: "5.5.0", Containerd: true, Driver: "overlayfs", Cgroup: "v2",
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

	for _, c := range []struct {
		name   string
		change func(*remote.ServerInfo)
		local  string
		want   string // the warning, then its fix after |
	}{
		{"older devopsy", func(*remote.ServerInfo) {}, "0.54.0", "older than the local 0.54.0|devopsy @vm1: --upgrade"},
		{"reboot", func(i *remote.ServerInfo) { i.RebootRequired, i.RebootPackages = true, []string{"linux-image-amd64"} }, "", "needs a reboot (for linux-image-amd64)|reboot it"},
		{"clock", func(i *remote.ServerInfo) { i.NTP = "no" }, "", "clock is not synchronized|timedatectl set-ntp true"},
		// Ubuntu 26.04's 7.0 kernel turns it on by default.
		{"kexec handover", func(i *remote.ServerInfo) { i.KHOScratch = 1252 << 20 }, "", "Kexec HandOver keeps 1.3 GB of memory as CMA, which only movable pages may use|kho=off on the kernel's command line, then a reboot: devopsy-server's setup.sh adds it"},
		{"docker down", func(i *remote.ServerInfo) { i.Docker = remote.DockerFacts{Error: "docker info: Cannot connect"} }, "", "Docker did not answer: docker info: Cannot connect|"},
		{"no compose", func(i *remote.ServerInfo) { i.Docker.Compose = "" }, "", "docker compose is missing|docker-compose-plugin"},
		// Docker lists apparmor whenever the kernel has it, and needs
		// apparmor_parser to load its profile (review).
		{"no apparmor_parser", func(i *remote.ServerInfo) { i.AppArmorParser = false }, "", "no apparmor_parser: Docker cannot load its profile, so containers fail to start|install the apparmor package"},
		{"unbounded logs", func(i *remote.ServerInfo) { i.Docker.LogOpts = nil }, "", "grow without a limit|max-size"},
		{"full disk", func(i *remote.ServerInfo) { i.Disks[0].Free, i.Disks[0].Used = 4e9, 76e9 }, "", "/ is 95% full, 4.0 GB free|devopsy @vm1: --shell-host"},
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
		{"missing tool", func(i *remote.ServerInfo) { i.Missing = []string{"script"} }, "", "has no script: builds and pulls show no live progress|"},
		// Held by a restarting container too, as releases and imports see
		// it: docker ps lists it (review).
		{"role twice", func(i *remote.ServerInfo) {
			i.Containers = append(i.Containers, remote.HostContainer{Project: "other-prod", State: "restarting", Labels: map[string]string{"devopsy.role": "proxy"}})
		}, "", "role proxy is held by traefik-prod and other-prod|"},
		{"first release failed", func(i *remote.ServerInfo) {
			i.Environments = append(i.Environments, remote.Environment{Path: "shop/dev", Status: "none\t20261027110000\n"})
		}, "", "shop/dev: its first release, 20261027110000, failed|$ devopsy @vm1:dev --status"},
		{"cut", func(i *remote.ServerInfo) {
			i.Environments[1].Status += "cut\t20261027115000\trelease\t20261026000000\n"
		}, "", "shop/prod: the release of 20261027115000 was cut off past its switch: its run step may not have run|devopsy @vm1:prod --status"},
		// In --status' words: a failed run step's restart is no switch.
		{"cut restart", func(i *remote.ServerInfo) {
			i.Environments[1].Status += "cut\t20261026000000\trestart\t20261027115000\n"
		}, "", "shop/prod: the restart of 20261026000000 after 20261027115000 failed was cut off: its run step may not have run|devopsy @vm1:prod --status"},
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
		{"paused", func(i *remote.ServerInfo) {
			i.Containers = append(i.Containers, remote.HostContainer{Project: "shop-prod", Service: "web", State: "paused"})
		}, "", "shop/prod: web paused|"},
	} {
		info := healthy()
		c.change(&info)
		_, warnings := serverInfoReport(info, "vm1", "vm1:", cmp.Or(c.local, "0.53.0"), now)
		msg, fix, _ := strings.Cut(c.want, "|")
		if len(warnings) != 1 || !strings.Contains(warnings[0].msg, msg) || (fix != "" && !strings.Contains(fixText(warnings[0].fixes), fix)) {
			t.Errorf("%s: want one warning %q (fix %q), got %+v", c.name, msg, fix, warnings)
		}
	}

	// What is running is said, never warned: an import under way, and a
	// restore, whose mark is there while it runs (review).
	for status, said := range map[string]string{
		"importing\nbusy\npartial\t" + remote.IncompleteLine("2026-10-27 11:00:00", "db", remote.IncompleteImport, "the import was cut"): "1 of 3 running  importing",
		"busy\npartial\t" + remote.IncompleteLine("2026-10-27 11:00:00", "20261020000000", remote.IncompleteRestore, "restoring"):        "1 of 3 running  an operation running",
	} {
		info := healthy()
		info.Environments[1].Status += status
		lines, warnings = serverInfoReport(info, "vm1", "vm1:", "0.53.0", now)
		if len(warnings) != 0 || !strings.Contains(strings.Join(lines, "\n"), said) {
			t.Errorf("running (%q): %+v\n%s", said, warnings, strings.Join(lines, "\n"))
		}
	}

	// What kdump keeps is a note, never a warning: crash dumps are a
	// choice.
	info := healthy()
	info.CrashKernel = 512 << 20
	lines, warnings = serverInfoReport(info, "vm1", "vm1:", "0.53.0", now)
	if len(warnings) != 1 || !warnings[0].note || !strings.Contains(warnings[0].msg, "kdump keeps 536.9 MB of memory for its crash kernel") ||
		!strings.Contains(fixText(warnings[0].fixes), "DEVOPSY_KDUMP=off") || !strings.Contains(strings.Join(lines, "\n"), "kernel keeps 536.9 MB for kdump") {
		t.Errorf("kdump: %+v\n%s", warnings, strings.Join(lines, "\n"))
	}

	// This machine's: fixes run here, no session to the server.
	info = healthy()
	info.Version = "0.54.0"
	info.Disks[0].Free, info.Disks[0].Used = 4e9, 76e9
	info.Environments = append(info.Environments, remote.Environment{Path: "shop/dev", Status: "none\t20261027110000\n"})
	_, warnings = serverInfoReport(info, "", "", "0.54.0", now)
	if len(warnings) != 2 || !strings.HasPrefix(fixText(warnings[0].fixes), "see what Docker takes, then empty its build cache:\n$ docker system df\n$ docker builder prune\n") ||
		strings.Contains(fixText(warnings[0].fixes), "--shell-host") ||
		fixText(warnings[1].fixes) != "\n$ cd /home/devopsy/shop/dev/current && devopsy --status\n" {
		t.Errorf("local: %+v", warnings)
	}

	// A kernel without AppArmor is said, not warned: nothing to install.
	info = healthy()
	info.Docker.Security, info.AppArmor, info.AppArmorParser = []string{"seccomp"}, false, false
	lines, warnings = serverInfoReport(info, "vm1", "vm1:", "0.53.0", now)
	if len(warnings) != 0 || !strings.Contains(strings.Join(lines, "\n"), "security seccomp (no AppArmor in this kernel)") {
		t.Errorf("no AppArmor in the kernel: %+v\n%s", warnings, strings.Join(lines, "\n"))
	}

	// Without flock every lock reads held: no environment is busy, its
	// mark warned (review).
	info = healthy()
	info.Missing = []string{"flock"}
	info.Environments[1].Status += "busy\npartial\t" + remote.IncompleteLine("2026-10-27 11:00:00", "db", remote.IncompleteImport, "the import was cut")
	lines, warnings = serverInfoReport(info, "vm1", "vm1:", "0.53.0", now)
	if len(warnings) != 2 || !strings.Contains(warnings[1].msg, "shop/prod: the import of db did not complete") || strings.Contains(strings.Join(lines, "\n"), "an operation running") {
		t.Errorf("no flock: %+v\n%s", warnings, strings.Join(lines, "\n"))
	}

	// Every problem of an environment is warned, as --status says each
	// (review: the first alone was).
	info = healthy()
	info.Environments = append(info.Environments, remote.Environment{Path: "shop/dev", Status: "none\t20261027110000\npartial\t" + remote.IncompleteLine("2026-10-27 11:00:00", "vm1:prod", remote.IncompleteCopy, "the copy was cut")})
	if _, warnings = serverInfoReport(info, "vm1", "vm1:", "0.53.0", now); len(warnings) != 2 {
		t.Errorf("two problems of shop/dev: %+v", warnings)
	}
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
	// What a --destroy cut off while removing an environment left (review).
	if err := os.MkdirAll(filepath.Join(s.root, "shop", "old.rm.4242", "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "bin")
	// Ubuntu 26.04's tail is uutils', which the scripts use as GNU's: no
	// tool missing (review).
	write(t, filepath.Join(bin, "tail"), "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'tail (uutils coreutils) 0.2.2'; exit 0; fi\nexec /usr/bin/tail \"$@\"\n", 0o755)
	write(t, filepath.Join(bin, "docker"), `#!/bin/sh
case "$1 $2" in
  "info --format") echo '{"ServerVersion":"29.9.1","Driver":"overlayfs","DriverStatus":[["driver-type","io.containerd.snapshotter.v1"]],"DockerRootDir":"/var/lib/docker","SecurityOptions":["name=apparmor"],"CgroupVersion":"2","LoggingDriver":"local"}'; exit 0 ;;
  "compose version") echo 5.5.0; exit 0 ;;
  "ps -aq") echo c1; echo c2; echo gone; exit 0 ;;
  # A container gone since ps: inspect prints the others, then fails
  # (review: every container was dropped then).
  "inspect --format") echo 'running - {"com.docker.compose.project":"traefik-prod","com.docker.compose.service":"traefik","devopsy.role":"proxy","devopsy.export.BASE_DOMAIN":"vm1.example.com"}'
    echo 'restarting - {"com.docker.compose.project":"shop-prod","com.docker.compose.service":"db"}'
    echo 'Error response from daemon: No such container: gone' >&2; exit 1 ;;
esac
exec "$(dirname "$DOCKER_LOG")/bin/docker" "$@"
`, 0o755)
	out, code := s.run([]string{"PATH=" + bin + ":" + filepath.Join(filepath.Dir(s.root), "bin") + ":/usr/bin:/bin"}, "@devopsy@one:", "--server-info")
	for _, want := range []string{
		"@devopsy@one:\n",
		"releases     " + s.root + ", keep 5",
		"containerd /var/lib/containerd",
		"docker       29.9.1, compose 5.5.0, containerd image store (overlayfs), cgroup v2",
		"disks        releases " + s.root,
		"roles        proxy: traefik-prod BASE_DOMAIN=vm1.example.com",
		"environments shop/dev  none: 19990101000000 failed",
		"shop/eu/prod  none: not released yet",
		"shop/prod  " + strings.TrimPrefix(s.current("prod"), "releases/") + "  just now",
		"shop/dev: its first release, 19990101000000, failed",
		"devopsy: | in shop's checkout:\ndevopsy: | $ devopsy @devopsy@one:dev --status",
		"shop/prod: db restarting",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in (%d):\n%s", want, code, out)
		}
	}
	if code != serverInfoWarned || strings.Contains(out, "shop/notes") || strings.Contains(out, "old.rm") || strings.Contains(out, "has no tail") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	// Without a target, this machine's, the same facts read in process.
	out, code = s.run([]string{"PATH=" + bin + ":" + filepath.Join(filepath.Dir(s.root), "bin") + ":/usr/bin:/bin"}, "--server-info")
	for _, want := range []string{
		"local (",
		"releases     " + s.root + ", keep 5",
		"roles        proxy: traefik-prod BASE_DOMAIN=vm1.example.com",
		"environments shop/dev  none: 19990101000000 failed",
		"devopsy: | $ cd " + filepath.Join(s.root, "shop", "dev", "current") + " && devopsy --status",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("local: no %q in (%d):\n%s", want, code, out)
		}
	}
	if code != serverInfoWarned {
		t.Errorf("local: exit %d:\n%s", code, out)
	}
	// It left nothing of its own in the release root.
	if m, _ := filepath.Glob(filepath.Join(s.root, ".devopsy-reflink.*")); len(m) > 0 {
		t.Errorf("left %v", m)
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
	var got []string
	for _, e := range environments(root) {
		got = append(got, e.Path)
	}
	if !slices.Equal(got, []string{"app/prod", "blog/prod", "shop/prod"}) {
		t.Errorf("environments: %v", got)
	}
}
