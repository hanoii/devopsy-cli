package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// Bug: the first session's confirmation names an empty list ("not applied, refused: ") when a
// refusal stops no step with a change, as readSetup gives a daemon.json devopsy cannot judge:
// no parts at all (judgeDaemon), so stepWords has nothing to name, yet planSetup adds the line
// whenever anything is refused. TestPlanSetupNamesWhatIsRefused gives parts beside the
// DaemonError, which readSetup never does.
func TestReviewOpusPlanSetupNamesNoEmptyRefusal(t *testing.T) {
	t.Parallel()
	f := setup.Facts{Root: "/srv", Record: "/r", DaemonError: setup.DaemonFile + ": not a JSON object",
		Files: []setup.FileFact{{Item: setup.ItemBuffers, InSync: false}}}
	ask := &scripted{confirms: []bool{true}}
	if _, run, err := planSetup(f, false, ask); err != nil || !run || len(ask.asked) != 1 {
		t.Fatalf("run %v (%v), asked %q", run, err, ask.asked)
	}
	if strings.HasSuffix(ask.asked[0], "refused: ") {
		t.Errorf("the confirmation names an empty list: %q", ask.asked[0])
	}
}

// Bug: a restart not confirmed in time returns at once (applySetup, "Docker down, no root run
// goes"), so the metadata firewall, which had a change to make, is neither loaded nor said
// as not applied. Docker is up here (the restart queued never came, and the second root run
// that raises the flag went), so its root run could go; the apply's "what went in and what
// did not" leaves the firewall out of both.
func TestReviewOpusRestartNotConfirmedLeavesTheFirewallUnsaid(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	out, code := s.run([]string{"FAKE_RESTART_STUCK=1", remote.RestartWaitEnv + "=1"}, "@devopsy@one:", "--server-setup", "--yes")
	if code != 1 || !strings.Contains(out, "Docker's restart did not end in a second") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	loaded := fileExists(filepath.Join(host, remote.FirewallScript))
	said := strings.Contains(out, "not applied") && strings.Contains(out, setup.FirewallItemName)
	if !loaded && !said {
		t.Errorf("the firewall neither loaded nor said as not applied:\n%s", out)
	}
}

// Bug: a refusal filed under the Docker item that says nothing of root runs (docker.service
// reading another config file, no compose plugin) stops every step, the firewall and the
// buffers among them, though only daemon.json's parts and a restart depend on what it
// refuses: on such a server W0001's one fix never loads, the case 0df19ef fixed for a
// daemon.json devopsy cannot judge.
func TestReviewOpusDockerRefusalsNotOfRootRunsStopOnlyTheirParts(t *testing.T) {
	t.Parallel()
	for name, env := range map[string][]string{"another config file": nil, "no compose plugin": {"FAKE_NO_COMPOSE=1"}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, host := setupServer(t)
			if err := os.MkdirAll(s.root, 0o755); err != nil {
				t.Fatal(err)
			}
			if env == nil {
				write(t, filepath.Join(host, "execstart"), "{ path=/usr/bin/dockerd ; argv[]=/usr/bin/dockerd --config-file /srv/docker.json ; ignore_errors=no }\n", 0o644)
			}
			out, code := s.run(env, "@devopsy@one:", "--server-setup", "--yes")
			if code != 1 {
				t.Fatalf("(%d):\n%s", code, out)
			}
			if !fileExists(filepath.Join(host, remote.FirewallScript)) || readString(filepath.Join(host, setup.SysctlFile)) != setup.SysctlText() {
				t.Errorf("the firewall or the buffers stopped by a refusal they do not need:\n%s", out)
			}
		})
	}
}

// Bug: --upgrade notes its version in the record over a daemon.json devopsy cannot judge,
// which stopped the parts' step: with no parts judged (judgeDaemon), stepWords names nothing,
// so the version goes in, and once the admin fixes the file no --upgrade of this version puts
// the parts. The rule: the version only after an apply with no failure and nothing a refusal
// stopped.
func TestReviewOpusUpgradeNotesNoVersionOverADaemonItCannotJudge(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(filepath.Dir(s.root), "home", setup.RecordFile)
	write(t, record, "daemon.json: {}\n", 0o600)
	write(t, filepath.Join(host, setup.DaemonFile), "[]\n", 0o644)
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade")
	if code != 0 || !strings.Contains(out, "devopsy leaves the file as it is") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if r, _, _ := setup.ReadRecord(record); r.Version != "" {
		t.Errorf("version %q noted while daemon.json's parts were refused:\n%s", r.Version, out)
	}
}
