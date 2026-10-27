package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// opusQuiet is a ServerInfo that warns about nothing: the review's tests
// change one thing in it.
func opusQuiet() remote.ServerInfo {
	return remote.ServerInfo{
		Version: "0.53.0", Protocol: 8, Root: "/home/devopsy", Keep: 5, MaxKeep: 5, SnapshotMaxKeep: 5,
		Arch: "amd64", CPUs: 2, NTP: "yes",
		Docker: remote.DockerFacts{Version: "29.9.1", Compose: "5.5.0", Containerd: true, Driver: "overlayfs",
			Security: []string{"apparmor", "seccomp"}, LogDriver: "local", LogOptsKnown: true},
		Disks: []remote.Disk{{Paths: []string{"releases=/home/devopsy", "/=/"}, Type: "xfs", Size: 80e9, Free: 40e9, Used: 40e9, Reflinks: "yes"}},
	}
}

// Review (opus): a container that is stopped reports its health as
// "unhealthy" in docker inspect (Docker sets it when it stops the
// health monitor, "for compatibility reasons"), so every service with a
// healthcheck that is simply down (stopped on purpose, kept down by a
// mark, a one-off that exited) was warned as unhealthy, exit 3. Health
// only means something for a running container.
func TestReviewOpusStoppedContainerNotUnhealthy(t *testing.T) {
	t.Parallel()
	info := opusQuiet()
	info.Containers = parseHostContainers([]byte(`running healthy {"com.docker.compose.project":"shop-prod","com.docker.compose.service":"app"}
exited unhealthy {"com.docker.compose.project":"shop-prod","com.docker.compose.service":"db"}
`))
	info.Environments = []remote.Environment{{Path: "shop/prod", Status: "release\t20261027115000\t{\"mode\":\"build\",\"time\":\"2026-10-27T11:50:00Z\"}\nkept\t1\nsynced\t\n"}}
	_, warnings := serverInfoReport(info, "vm1", "vm1:", "0.53.0", time.Now())
	if len(warnings) != 0 {
		t.Errorf("a stopped container with a healthcheck warned as unhealthy: %+v", warnings)
	}
}

// Review (opus): an environment's directory keeps its name's case
// (shop/Staging) while its compose name is lowercased
// (Target.ComposeName: shop-staging), so --server-info matched none of
// its containers: no count, and a restarting one never warned.
func TestReviewOpusUppercaseEnvironmentContainers(t *testing.T) {
	t.Parallel()
	info := opusQuiet()
	info.Containers = []remote.HostContainer{
		{Project: "shop-staging", Service: "app", State: "running"},
		{Project: "shop-staging", Service: "db", State: "restarting"},
	}
	info.Environments = []remote.Environment{{Path: "shop/Staging", Status: "release\t20261027115000\t{\"mode\":\"build\",\"time\":\"2026-10-27T11:50:00Z\"}\nkept\t1\nsynced\t\n"}}
	lines, warnings := serverInfoReport(info, "vm1", "vm1:", "0.53.0", time.Now())
	if !strings.Contains(strings.Join(lines, "\n"), "1 of 2 running") {
		t.Errorf("no container counted for shop/Staging:\n%s", strings.Join(lines, "\n"))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].msg, "db restarting") {
		t.Errorf("want the restarting db warned, got %+v", warnings)
	}
}

// Review (opus): the no-reflinks warning's fix says to set
// snapshots: {mirror: true} in the server's devopsy config, but there
// mirror only turns mirrors off (ServerSnapshots.MirrorsOff: false wins,
// true or unset lets each environment decide): a mirror is the
// environment's setting, in its project's .devopsy/config.yaml.
func TestReviewOpusNoReflinksFixNamesTheEnvironment(t *testing.T) {
	t.Parallel()
	info := opusQuiet()
	info.Disks[0].Type, info.Disks[0].Reflinks = "ext4", "no"
	_, warnings := serverInfoReport(info, "vm1", "vm1:", "0.53.0", time.Now())
	if len(warnings) != 1 {
		t.Fatalf("want one warning, got %+v", warnings)
	}
	fix := strings.Join(warnings[0].fixes, "\n")
	if strings.Contains(fix, "server's devopsy config") {
		t.Errorf("the fix sends mirror: true to the server's config, where it does nothing: %q", fix)
	}
}

// Review (opus): docker ps then docker inspect of every id: a container
// gone in between (a run --rm, a util container, a one-off) makes
// inspect exit 1 while it printed every other one, and hostContainers
// then dropped them all: roles none, no counts, no warnings, silently.
func TestReviewOpusInspectRace(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	bin := filepath.Join(t.TempDir(), "bin")
	write(t, filepath.Join(bin, "docker"), `#!/bin/sh
case "$1 $2" in
  "info --format") echo '{"ServerVersion":"29.9.1","Driver":"overlayfs","SecurityOptions":["name=apparmor"],"LoggingDriver":"local"}'; exit 0 ;;
  "compose version") echo 5.5.0; exit 0 ;;
  "ps -aq") echo c1; echo gone; exit 0 ;;
  "inspect --format") echo 'running - {"com.docker.compose.project":"traefik-prod","com.docker.compose.service":"traefik","devopsy.role":"proxy"}'
    echo 'Error: No such object: gone' >&2; exit 1 ;;
esac
exec "$(dirname "$DOCKER_LOG")/bin/docker" "$@"
`, 0o755)
	out, _ := s.run([]string{"PATH=" + bin + ":" + filepath.Join(filepath.Dir(s.root), "bin") + ":/usr/bin:/bin"}, "@devopsy@one:", "--server-info")
	if !strings.Contains(out, "roles        proxy: traefik-prod") {
		t.Errorf("a container gone between ps and inspect hid every other one:\n%s", out)
	}
}

// Review (opus): the tools check takes a tail whose --version lacks
// "GNU" for no tail at all, but the scripts use any tail that answers
// --version (followOutput), and Ubuntu 26.04's coreutils are uutils
// ("tail (uutils coreutils) ..."): every such server was warned, exit
// 3, for protection its releases have.
func TestReviewOpusTailAsTheScriptsSeeIt(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	bin := filepath.Join(t.TempDir(), "bin")
	write(t, filepath.Join(bin, "tail"), "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'tail (uutils coreutils) 0.2.2'; exit 0; fi\nexec /usr/bin/tail \"$@\"\n", 0o755)
	write(t, filepath.Join(bin, "docker"), `#!/bin/sh
case "$1 $2" in
  "info --format") echo '{"ServerVersion":"29.9.1","Driver":"overlayfs","SecurityOptions":["name=apparmor"],"LoggingDriver":"local"}'; exit 0 ;;
  "compose version") echo 5.5.0; exit 0 ;;
  "ps -aq") exit 0 ;;
esac
exec "$(dirname "$DOCKER_LOG")/bin/docker" "$@"
`, 0o755)
	out, _ := s.run([]string{"PATH=" + bin + ":" + filepath.Join(filepath.Dir(s.root), "bin") + ":/usr/bin:/bin"}, "@devopsy@one:", "--server-info")
	if strings.Contains(out, "has no tail") {
		t.Errorf("a tail the scripts use was reported missing:\n%s", out)
	}
}

// Review (opus): the --status refactor. Before it, an environment whose
// current is a directory (a failed release's containers restarting,
// Docker making their bind mounts under it as root: current/.devopsy/mnt/
// ...) read as "none" and the script ended there; now its exit test is
// [ -d "$base/current/.devopsy" ], which such a directory passes, so
// --status runs devopsy --status in that directory too.
func TestReviewOpusStatusCurrentDirectory(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	base := filepath.Join(s.root, "shop", "prod")
	if err := os.MkdirAll(filepath.Join(base, "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "current", ".devopsy", "mnt", "storage"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := s.run(nil, "@prod", "--status")
	_, after, _ := strings.Cut(out, "release     none: not released yet\n")
	if code != 0 || !strings.Contains(out, "release     none: not released yet") || strings.TrimSpace(after) != "" {
		t.Errorf("--status changed for a current that is a directory (exit %d):\n%s", code, out)
	}
}
