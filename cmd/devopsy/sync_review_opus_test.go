package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Bug (review, opus): PortError is checked for the target (runRemote),
// the --environments servers and a sync's parent (syncPreflight) only,
// so --inherit opens its session to a parent whose port
// DEVOPSY_SSH_COMMAND overrides (ssh -G says 22, the parent names 5369)
// without a word: it reads the variables of whatever answers on port 22.
// The same holds for the parent.env refresh (refreshParentEnv's src).
func TestInheritRefusesParentPortOverriddenReview(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	sessions := filepath.Join(t.TempDir(), "sessions")
	ssh := filepath.Join(t.TempDir(), "ssh")
	// As ssh -p 22 resolves any destination; every session logged, none
	// answered.
	write(t, ssh, "#!/bin/sh\ncase \" $* \" in *\" -G \"*) echo 'hostname one'; echo 'port 22'; exit 0 ;; esac\necho \"$*\" | head -n 1 >> '"+sessions+"'\nexit 1\n", 0o755)
	out, code := s.run([]string{"DEVOPSY_SSH_COMMAND=" + ssh, "DEVOPSY_SERVER_PROD=devopsy@one:5369"}, "@dev", "--inherit")
	got, _ := os.ReadFile(sessions)
	if code == 0 || !strings.Contains(out, `server "devopsy@one:5369" names port 5369, and ssh would connect to 22`) || strings.Contains(string(got), "-p 5369") {
		t.Errorf("--inherit from a parent whose port DEVOPSY_SSH_COMMAND overrides (%d):\n%s\nsessions:\n%s", code, out, got)
	}
}
