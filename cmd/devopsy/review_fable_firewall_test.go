package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// A release's read of the chains runs the util image only as it is there, never pulled: the
// read checks the image with an inspect and then runs it, and an image gone in between (a
// prune, a rebuild) is the daemon's to pull by its name, a root container with the host's
// network on whatever answers it. The door runs this read for a project user, and every util
// container the door runs holds --pull never (internals/door.md).
//
// Bug: the read's docker run holds no --pull never.
func TestReleaseFirewallReadNeverPulls(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	firewallLoaded(t, host, false)
	s.logs()
	if out, code := s.run(nil, remote.ServerReadyFlag, "one:"); code != 0 {
		t.Fatalf("--server-ready (%d):\n%s", code, out)
	}
	docker, _ := s.logs()
	ran := false
	for line := range strings.SplitSeq(docker, "\n") {
		if !strings.Contains(line, "chroot /host") {
			continue
		}
		ran = true
		if !strings.Contains(line, "--pull never") {
			t.Errorf("the read's run may pull its image: %s", line)
		}
	}
	if !ran {
		t.Fatalf("no root read:\n%s", docker)
	}
}

// A release's read of the chains ends within its bound when Docker's client hangs (a daemon
// restarting, a socket that takes the connection and answers nothing), with an error and no
// answer: a release is never held up for it.
func TestReleaseFirewallReadIsBounded(t *testing.T) {
	t.Parallel()
	host := t.TempDir()
	write(t, filepath.Join(host, remote.FirewallMark), remote.FirewallContainers+"\n", 0o644)
	docker := filepath.Join(t.TempDir(), "docker")
	// The image is there, and its run never answers.
	write(t, docker, "#!/bin/sh\ncase $1 in image) echo sha256:1; exit 0 ;; esac\nexec sleep 60\n", 0o755)
	start := time.Now()
	right, err := releaseFirewall(setupHost{root: host}, docker)
	if took := time.Since(start); took > firewallReadWait+5*time.Second {
		t.Errorf("the read held a release for %s, its bound %s", took.Round(time.Second), firewallReadWait)
	}
	if err == nil || right {
		t.Errorf("a read that never answered: right %v, %v", right, err)
	}
}

// --server-info, on a server whose config gained door: while the containers' chain alone is
// loaded, before any project user was added (no group of the door's yet), says the door's
// chains are to load, and nothing of a read that failed: the root read ran and answered.
//
// Bug: the read is judged as the door's by the kind wanted, never the kind loaded, so the
// door's judgment fails on its missing groups and the facts say the chains "could not be read
// as root: no devopsy-projects group yet".
func TestServerInfoFirewallToLoadAsTheDoorsWasRead(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	firewallLoaded(t, host, false)
	setupConfig(t, s, "door:\n  space: $HOME/space\n")
	out, _ := s.run(nil, "@devopsy@one:", "--server-info")
	if !strings.Contains(out, "door: is in this server's config, and devopsy's firewall loads the containers' chain alone") {
		t.Fatalf("not the state this test is for:\n%s", out)
	}
	if strings.Contains(out, "could not be read as root") {
		t.Errorf("a read that answered, said unread:\n%s", out)
	}
}

// --server-setup's facts session is read-only and builds no util image (internals/
// server-setup.md, Sessions), on a server whose firewall is loaded too: its read of the chains
// as root runs only with the image there, as a release's does.
//
// Bug: the facts' read readies the util image as an apply does, so the read-only session
// builds it where it is missing, and builds it again where it is over a week old.
func TestServerSetupFactsBuildNoUtilImage(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	firewallLoaded(t, host, false)
	for name, env := range map[string]string{"missing": "FAKE_UTIL_MISSING=1", "over a week old": "FAKE_UTIL_CREATED=2020-01-01T00:00:00Z"} {
		s.logs()
		if out, code := s.run([]string{env}, remote.ServerSetupFlag, "facts"); code != 0 {
			t.Fatalf("%s: facts (%d):\n%s", name, code, out)
		}
		if docker, _ := s.logs(); strings.Contains(docker, "build ") {
			t.Errorf("the util image %s: the facts session built it", name)
		}
	}
}
