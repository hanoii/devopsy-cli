package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// BUG: a sync key's receive connects wherever the parent says on the child's loopback. The
// request's port is the parent's to choose (--sync-child checks only 1..65535), and the
// child's receive runs rsync as root on the host's network to rsync://127.0.0.1:<port>: a
// hostile parent's server, with the sync's key, points it at any local service of the child's
// server (its sshd, a database published on 127.0.0.1, another sync's forward) and reads the
// first line that service answers in rsync's error, which the session's stderr carries back
// to the parent. Only the port this session's own reverse forward listens on is the sync's.
func TestOpusReviewSyncChildReceivePortIsTheParents(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	base := filepath.Join(s.root, "shop", "dev")
	id := "1762600000.0a1b2c"
	write(t, filepath.Join(base, remote.SyncOpenFile(id)), "vm1:prod "+id+"\n", 0o644)
	s.logs()
	child := func(req string) (string, int) {
		t.Helper()
		cmd := exec.Command(binary, remote.SyncChildFlag, id, base, "vm2:dev")
		cmd.Dir = t.TempDir()
		cmd.Env = append(append([]string{}, s.env...), "SSH_ORIGINAL_COMMAND="+req)
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}
	// Control, which holds now: a port out of range is refused.
	if out, code := child(`{"receive":{"port":70000,"dirs":["storage"]}}`); code == 0 || !strings.Contains(out, "not a port") {
		t.Fatalf("control: port 70000 (%d):\n%s", code, out)
	}
	// Port 22, the child's sshd: no reverse forward of this session's is there (there is
	// none at all here).
	out, code := child(`{"receive":{"port":22,"dirs":["storage"]}}`)
	docker, _ := s.logs()
	if strings.Contains(docker, "--network host") && strings.Contains(docker, "rsync://127.0.0.1:22/storage/") {
		t.Errorf("BUG: the child's root rsync, on the host's network, connected to the port the parent named, 22, no forward of the session's (%d):\n%s\n%s", code, out, docker)
	}
}
