package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Review (fable): a stopped container with a healthcheck is "unhealthy"
// to Docker (its monitor stopped: docker inspect's .State.Health.Status
// stays unhealthy on an exited container, checked on Docker 29.8), so
// --server-info warns "<service> unhealthy" for every service down on
// purpose: a child's app kept down, a maintenance stop, a database
// stopped for an export. Only a running container's health says
// anything.
func TestServerInfoReportStoppedHealthcheck(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	info := remote.ServerInfo{
		Version: "0.53.0", Protocol: 8, Root: "/home/devopsy", Keep: 5, MaxKeep: 5, SnapshotMaxKeep: 5, Arch: "amd64", CPUs: 2, NTP: "yes",
		Docker: remote.DockerFacts{Version: "29.9.1", Compose: "5.5.0", Containerd: true, Driver: "overlayfs", Cgroup: "v2",
			RootDir: "/var/lib/docker", Security: []string{"apparmor", "seccomp"}, LogDriver: "local", LogOptsKnown: true},
		Disks: []remote.Disk{{Paths: []string{"releases=/home/devopsy", "docker=/var/lib/docker", "/=/"}, Type: "xfs", Size: 80e9, Free: 40e9, Used: 40e9, Reflinks: "yes"}},
		Containers: []remote.HostContainer{
			{Project: "shop-prod", Service: "app", State: "running", Health: "healthy"},
			// Stopped by hand (compose stop db): Docker reports its health
			// as unhealthy once the monitor stopped.
			{Project: "shop-prod", Service: "db", State: "exited", Health: "unhealthy"},
		},
		Environments: []remote.Environment{
			{Path: "shop/prod", Status: "release\t20261027115000\t{\"mode\":\"build\",\"time\":\"2026-10-27T11:50:00Z\",\"by\":\"ana\"}\nkept\t2\nsynced\t\n"},
		},
	}
	lines, warnings := serverInfoReport(info, "vm1", "vm1:", "0.53.0", now)
	if !strings.Contains(strings.Join(lines, "\n"), "shop/prod  20261027115000  10 min ago  by ana  1 of 2 running") {
		t.Errorf("lines:\n%s", strings.Join(lines, "\n"))
	}
	if len(warnings) != 0 {
		t.Errorf("a stopped service with a healthcheck warned: %+v", warnings)
	}
}

// Review (fable): hostContainers lists the ids with docker ps, then
// inspects them all in one call; a container gone in between (a one-off
// `compose run --rm` ending, an owners' or a util container) makes
// docker inspect exit 1 after printing the others ("no such object"),
// and Output()'s error drops them all: --server-info then reports no
// roles and no containers, nothing restarting, as if the host were
// empty. What inspect printed is every container still there.
func TestServerInfoInspectVanished(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	bin := filepath.Join(t.TempDir(), "bin")
	write(t, filepath.Join(bin, "docker"), `#!/bin/sh
case "$1 $2" in
  "info --format") echo '{"ServerVersion":"29.9.1","Driver":"overlayfs","DriverStatus":[["driver-type","io.containerd.snapshotter.v1"]],"DockerRootDir":"/var/lib/docker","SecurityOptions":["name=apparmor"],"CgroupVersion":"2","LoggingDriver":"local"}'; exit 0 ;;
  "compose version") echo 5.5.0; exit 0 ;;
  "ps -aq") echo c1; echo c2; echo gone; exit 0 ;;
  "inspect --format") echo 'running - {"com.docker.compose.project":"traefik-prod","com.docker.compose.service":"traefik","devopsy.role":"proxy","devopsy.export.BASE_DOMAIN":"vm1.example.com"}'
    echo 'restarting - {"com.docker.compose.project":"shop-prod","com.docker.compose.service":"db"}'
    echo 'Error response from daemon: No such container: gone' >&2; exit 1 ;;
esac
exec "$(dirname "$DOCKER_LOG")/bin/docker" "$@"
`, 0o755)
	out, code := s.run([]string{"PATH=" + bin + ":" + filepath.Join(filepath.Dir(s.root), "bin") + ":/usr/bin:/bin"}, "@devopsy@one:", "--server-info")
	for _, want := range []string{
		"roles        proxy: traefik-prod BASE_DOMAIN=vm1.example.com",
		"shop/prod  " + strings.TrimPrefix(s.current("prod"), "releases/") + "  just now",
		"  0 of 1 running",
		"shop/prod: db restarting",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in (%d):\n%s", want, code, out)
		}
	}
	if code != serverInfoWarned {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if m, _ := filepath.Glob(filepath.Join(s.root, ".devopsy-reflink.*")); len(m) > 0 {
		t.Errorf("left %v", m)
	}
}

// Review (fable): the roles --server-info reports come from containers
// in state "running" alone, but what a release checks (RoleHoldersScript:
// docker ps --filter label=devopsy.role=<role>) and what imports read
// (cli.Running: docker ps -q) are docker ps's containers, which include
// restarting and paused ones. A Traefik crash-looping in traefik-prod
// still holds the proxy role for every release on the host (a second
// proxy's release is refused, imports read its exports), while
// --server-info says "roles none", or names traefik-staging as the one
// holder, and never warns that the role is held twice.
func TestServerInfoRolesAsReleasesSeeThem(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	info := remote.ServerInfo{
		Version: "0.53.0", Protocol: 8, Root: "/home/devopsy", Keep: 5, MaxKeep: 5, SnapshotMaxKeep: 5, Arch: "amd64", CPUs: 2, NTP: "yes",
		Docker: remote.DockerFacts{Version: "29.9.1", Compose: "5.5.0", Containerd: true, Driver: "overlayfs", Cgroup: "v2",
			RootDir: "/var/lib/docker", Security: []string{"apparmor", "seccomp"}, LogDriver: "local", LogOptsKnown: true},
		Disks: []remote.Disk{{Paths: []string{"releases=/home/devopsy", "docker=/var/lib/docker", "/=/"}, Type: "xfs", Size: 80e9, Free: 40e9, Used: 40e9, Reflinks: "yes"}},
		Containers: []remote.HostContainer{
			{Project: "traefik-prod", Service: "traefik", State: "restarting", Labels: map[string]string{"devopsy.role": "proxy", "devopsy.export.BASE_DOMAIN": "vm1.example.com"}},
			{Project: "traefik-staging", Service: "traefik", State: "running", Labels: map[string]string{"devopsy.role": "proxy", "devopsy.export.BASE_DOMAIN": "staging.example.com"}},
		},
	}
	lines, warnings := serverInfoReport(info, "vm1", "vm1:", "0.53.0", now)
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "proxy: traefik-prod BASE_DOMAIN=vm1.example.com") {
		t.Errorf("a restarting holder is not listed:\n%s", out)
	}
	var held []string
	for _, w := range warnings {
		if strings.Contains(w.msg, "role proxy is held by") {
			held = append(held, w.msg)
		}
	}
	if len(held) != 1 || !strings.Contains(held[0], "traefik-prod") || !strings.Contains(held[0], "traefik-staging") {
		t.Errorf("the role held twice, as a release sees it, not warned: %+v", warnings)
	}
}
