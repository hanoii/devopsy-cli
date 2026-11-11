package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// pathWithout is a PATH holding every command of bin and the test's PATH but tool, as links.
func pathWithout(t *testing.T, bin, tool string) string {
	t.Helper()
	path := t.TempDir()
	for _, dir := range append([]string{bin}, filepath.SplitList(os.Getenv("PATH"))...) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() != tool && !fileExists(filepath.Join(path, e.Name())) {
				_ = os.Symlink(filepath.Join(dir, e.Name()), filepath.Join(path, e.Name()))
			}
		}
	}
	return path
}

// Bug: --said tells the apply that every refusal was said, so a refusal new in the second
// session (Docker gone between the sessions, a tool removed) is never said: the apply fails
// with "nothing changed", or with no word at all for one that stops nothing, its message and
// fix unsaid. The apply must say what the first session did not.
func TestServerSetupApplySaysARefusalNewInItsSession(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		env  func(t *testing.T, host string) []string
		want string
	}{
		"Docker down": {func(t *testing.T, host string) []string {
			write(t, filepath.Join(host, "docker-down"), "", 0o644)
			return nil
		}, "Docker's daemon does not answer"},
		"tar missing": {func(t *testing.T, host string) []string {
			return []string{"PATH=" + pathWithout(t, filepath.Join(filepath.Dir(host), "bin"), "tar")}
		}, "no tar in PATH"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, host := setupServer(t)
			// The first session read no refusal, and so said none: its apply, as it runs it.
			out, code := s.run(c.env(t, host), remote.ServerSetupFlag, "apply", "--said")
			if code != 1 {
				t.Fatalf("the control: a refusal and exit %d:\n%s", code, out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("exit 1 with the refusal new in the apply unsaid (want %q):\n%s", c.want, out)
			}
		})
	}
}

// Bug: a missing compose or buildx plugin stops nothing of the setup (Refusal.Stops), yet
// readSetup's Pending requires no refusal of setupDocker's at all, the plugins' included: a
// change Docker has yet to take reads as none, so --server-setup neither says it nor
// restarts Docker for it.
func TestServerSetupPendingBesideAMissingPlugin(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	// A container running: daemon.json put, Docker not restarted, its change pending.
	if out, code := s.run([]string{"FAKE_RUNNING=c1"}, "@devopsy@one:", "--server-setup", "--yes"); code != 0 {
		t.Fatalf("the first run (%d):\n%s", code, out)
	}
	pending := func(env []string) bool {
		t.Helper()
		out, code := s.run(env, remote.ServerSetupFlag, "facts")
		var f setup.Facts
		if i := strings.Index(out, "{"); code != 0 || i < 0 || json.Unmarshal([]byte(out[i:]), &f) != nil {
			t.Fatalf("the facts (%d):\n%s", code, out)
		}
		return f.Pending
	}
	if !pending(nil) {
		t.Fatal("the control: not pending after a change not restarted")
	}
	if !pending([]string{"FAKE_NO_COMPOSE=1"}) {
		t.Error("no compose plugin, which stops nothing, hides a change Docker has yet to take")
	}
	s.run([]string{"FAKE_NO_COMPOSE=1"}, "@devopsy@one:", "--server-setup", "--yes")
	if !strings.Contains(readString(filepath.Join(host, "systemctl.log")), "restart --no-block docker.service") {
		t.Error("no compose plugin: Docker not restarted for its pending change, a restart the plugin does not need")
	}
}

// Bug: an earlier run that marked a change (Docker not restarted) left the restart marker
// holding the current InvocationID, and the apply takes a marker equal to it as the restart
// asked: a root run that fails before its restart (a put's write failing, exit 5; dockerd
// refusing daemon.json, exit 4) reads as a restart asked, its failure unsaid by the apply,
// the whole restart bound waited, and a restart that was never asked said not confirmed.
func TestServerSetupRunFailedBeforeItsRestartBesidePending(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if out, code := s.run([]string{"FAKE_RUNNING=c1"}, "@devopsy@one:", "--server-setup", "--yes"); code != 0 || readString(filepath.Join(host, setup.RestartMarker)) != "inv1\n" {
		t.Fatalf("the first run, marked (%d):\n%s", code, out)
	}
	// The sysctl file changed since, and its move failing: the run ends at that put, before
	// the restart its pending change asks.
	write(t, filepath.Join(host, setup.SysctlFile), "# someone's\n", 0o644)
	write(t, filepath.Join(filepath.Dir(host), "bin", "mv"), "#!/bin/sh\nfor a; do case $a in *"+filepath.Base(setup.SysctlFile)+") exit 1 ;; esac; done\nexec /bin/mv \"$@\"\n", 0o755)
	out, code := s.run([]string{remote.RestartWaitEnv + "=1"}, "@devopsy@one:", "--server-setup", "--yes")
	if code != 1 || !strings.Contains(out, "could not be written") {
		t.Fatalf("the control: the put's failure (%d):\n%s", code, out)
	}
	if strings.Contains(readString(filepath.Join(host, "systemctl.log")), "restart --no-block docker.service") {
		t.Fatal("the control: a restart asked after the failed put")
	}
	if strings.Contains(out, "Docker's restart did not end") || !strings.Contains(out, "the setup's run as root") {
		t.Errorf("a run that failed before its restart taken for a restart asked:\n%s", out)
	}
}

// Suspected bug: a release's read of the chains (--server-ready) runs a root container
// through whatever Docker the deploy user reaches, with no check that it is this host's
// (dockerHere), which the setup, --server-info and the apply all make: through DOCKER_HOST
// or a context naming another machine, it reads that machine's chains as root and judges
// this host's firewall by them.
func TestServerReadyReadsNoChainsThroughAnotherHostsDocker(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	firewallLoaded(t, host, false)
	write(t, filepath.Join(host, "docker-info.json"), `{"Name":"elsewhere","LoggingDriver":"json-file","LiveRestoreEnabled":false,"DockerRootDir":"/var/lib/docker","SecurityOptions":["name=seccomp"]}`, 0o644)
	s.logs()
	if out, code := s.run(nil, remote.ServerReadyFlag, "one:"); code != 0 {
		t.Fatalf("--server-ready (%d):\n%s", code, out)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "chroot /host") {
		t.Errorf("a release's root read through another host's Docker:\n%s", docker)
	}
}

// A restart that leaves Docker down names the firewall among what was not applied, and loads
// nothing: TestServerSetupFirewallAfterARestartNotConfirmed takes either outcome from a
// Docker that answers, so this side had no test.
func TestServerSetupFirewallNotAppliedWithDockerDown(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	out, code := s.run([]string{"FAKE_RESTART_DOWN=1", remote.RestartWaitEnv + "=1"}, "@devopsy@one:", "--server-setup", "--yes")
	if code != 1 || !strings.Contains(out, "error: not applied: "+setup.FirewallItemName+", Docker not answering since its restart") || fileExists(filepath.Join(host, remote.FirewallScript)) {
		t.Errorf("(%d):\n%s", code, out)
	}
}

// --upgrade notes its version beside a refusal that stops nothing (no tar), and none beside
// one that stops a change it had to make (no flock, the firewall to load again): the version
// says the apply took everything, which only the second leaves undone.
func TestUpgradeNotesItsVersionByWhatARefusalStopped(t *testing.T) {
	t.Parallel()
	for tool, noted := range map[string]bool{"tar": true, "flock": false} {
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			s, host := setupServer(t)
			if err := os.MkdirAll(s.root, 0o755); err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(filepath.Dir(s.root), "home", setup.RecordFile)
			write(t, record, "daemon.json: {}\n", 0o600)
			firewallLoaded(t, host, false)
			// The loader's mode lost: the firewall to load again, which --upgrade does.
			if err := os.Chmod(filepath.Join(host, remote.FirewallScript), 0o644); err != nil {
				t.Fatal(err)
			}
			out, code := s.run([]string{"PATH=" + pathWithout(t, filepath.Join(filepath.Dir(host), "bin"), tool)}, remote.ServerSetupFlag, "apply", "--upgrade")
			if code != 0 || !strings.Contains(out, "no "+tool+" in PATH") {
				t.Fatalf("the control (%d):\n%s", code, out)
			}
			if r, _, _ := setup.ReadRecord(record); (r.Version == binaryVersion(t)) != noted {
				t.Errorf("no %s: version %q noted, want noted %v:\n%s", tool, r.Version, noted, out)
			}
		})
	}
}
