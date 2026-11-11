package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// Bug: a refusal that stops no step (Docker without its compose plugin) still hides a
// daemon.json Docker has yet to take: Pending is false beside any refusal of Docker's, so the
// restart is never offered and --server-info says nothing of it, while the restart's step goes.
func TestReviewPendingShowsBesideARefusalThatStopsNothing(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(host, setup.RestartMarker), "inv1\n", 0o644)
	if out, code := s.run(nil, remote.ServerSetupFlag, "facts"); code != 0 || !strings.Contains(out, `"pending":true`) {
		t.Fatalf("the control: a marker of this InvocationID is not pending (%d):\n%s", code, out)
	}
	out, code := s.run([]string{"FAKE_NO_COMPOSE=1"}, remote.ServerSetupFlag, "facts")
	if code != 0 || !strings.Contains(out, "no compose plugin") {
		t.Fatalf("the control: no refusal of the plugin (%d):\n%s", code, out)
	}
	if !strings.Contains(out, `"pending":true`) {
		t.Errorf("pending hidden by a refusal that stops nothing:\n%s", out)
	}
}

// Bug: a root run that fails before it asks for the restart (dockerd refusing daemon.json, a
// put's exit 4 or 5) reads as a restart asked whenever an earlier run's marker holds the
// current InvocationID, which is what pending is: the apply then waits its whole bound for a
// restart nobody asked and says it did not end, naming Docker's journal.
func TestReviewAStaleMarkerIsNoRestartAsked(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if out, code := s.run(nil, remote.ServerSetupFlag, "apply"); code != 0 || readString(filepath.Join(host, setup.RestartMarker)) != "inv1\n" {
		t.Fatalf("the control: a change marked for the next restart (%d):\n%s", code, out)
	}
	write(t, filepath.Join(filepath.Dir(host), "bin", "dockerd"), "#!/bin/sh\necho 'invalid' >&2\nexit 1\n", 0o755)
	out, code := s.run([]string{remote.RestartWaitEnv + "=1"}, remote.ServerSetupFlag, "apply", "--restart")
	if code != 1 || !strings.Contains(out, "not restarted") {
		t.Fatalf("the control: dockerd refusing the file fails the restart (%d):\n%s", code, out)
	}
	if strings.Contains(out, "Docker's restart did not end") || strings.Contains(readString(filepath.Join(host, "systemctl.log")), "restart --no-block") {
		t.Errorf("a restart never asked was waited for:\n%s", out)
	}
}

// Bug: the first session runs no apply when no step goes, so an apply told the refusals were
// said (--said) that finds Docker refused has one nobody said (Docker went down between the
// sessions): it ends with "nothing changed" and no reason.
func TestReviewAnApplySaysARefusalTheFirstSessionCouldNot(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	write(t, filepath.Join(host, "docker-down"), "", 0o644)
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--said")
	if code != 1 || !strings.Contains(out, "nothing changed") {
		t.Fatalf("the control (%d):\n%s", code, out)
	}
	if !strings.Contains(out, "does not answer") {
		t.Errorf("an apply that changed nothing, its reason said by no session:\n%s", out)
	}
}

// Bug: a release's read of the chains goes through whatever Docker the deploy user reaches,
// another host's included (dockerHere is asked only by the setup and --server-info): a root
// container there reads another machine's chains, and W0001 is said of this one.
func TestReviewAReleaseReadsNoChainsThroughAnotherHostsDocker(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	firewallLoaded(t, host, false)
	write(t, filepath.Join(host, "docker-info.json"), `{"Name":"elsewhere","LoggingDriver":"json-file","LiveRestoreEnabled":false,"DockerRootDir":"/var/lib/docker","SecurityOptions":["name=seccomp"]}`, 0o644)
	s.logs()
	out, code := s.run(nil, remote.ServerReadyFlag, "one:")
	if docker, _ := s.logs(); code != 0 || strings.Contains(docker, "chroot /host") {
		t.Errorf("a release's root run through a Docker that is not this host's (%d):\n%s\n%s", code, out, docker)
	}
}

// Bug: a release root a unit file cannot hold stops the mounts drop-in, yet the drop-in is no
// change the refusal stopped (the facts carry no file for it): the apply's end names nothing
// as not applied, and --upgrade notes its version, never applying again once the root is one
// a unit holds. A daemon.json devopsy cannot judge counts its parts for the same reason.
func TestReviewARootNoUnitHoldsStopsTheDropInSaid(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	root := filepath.Join(filepath.Dir(s.root), "re\tleases")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(filepath.Dir(s.root), "home")
	write(t, filepath.Join(home, "config.yaml"), "releases:\n  root: \""+strings.ReplaceAll(root, "\t", `\t`)+"\"\n", 0o644)
	record := filepath.Join(home, setup.RecordFile)
	write(t, record, "daemon.json: {}\n", 0o600)
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade")
	if code != 0 || !strings.Contains(out, "a unit file cannot hold") || fileExists(filepath.Join(host, setup.MountsDropIn)) {
		t.Fatalf("the control: the root refused for the drop-in alone (%d):\n%s", code, out)
	}
	if r, _, _ := setup.ReadRecord(record); r.Version != "" {
		t.Errorf("a version noted (%q) by an apply whose mounts drop-in a refusal stopped:\n%s", r.Version, out)
	}
	out, code = s.run(nil, remote.ServerSetupFlag, "apply")
	if code != 1 || !strings.Contains(out, "not applied, refused: "+string(setup.StepMounts)) {
		t.Errorf("the drop-in a refusal stopped, not named (%d):\n%s", code, out)
	}
}
