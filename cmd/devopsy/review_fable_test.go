package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// A devopsy typed in a host shell names, in its hints, the environment of the release it
// runs in, never the one the shell was opened on: the operator moves in a shell, and a
// command copied from a hint must act on the environment that said it.
//
// Bug (review): a shell's session exports its address (sessionExports, --shell-host on an
// environment), and sessionHint takes the session's before the release's DEVOPSY_TARGET, so a
// devopsy run in dev's release from a shell opened on prod offers its fixes on prod (here a
// sync; the same path gives --destroy and --snapshots restore).
func TestShellHintsNameTheReleaseTheyRunIn(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	dev := filepath.Join(s.root, "shop", "dev")
	write(t, filepath.Join(dev, remote.SyncIncompleteFile), remote.IncompleteLine("2026-10-22 10:00:00", "devopsy@one:prod", remote.IncompleteCopy, "the copy was cut"), 0o644)

	cmd := exec.Command(binary, "@prod", "--shell-host")
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), "SHELL=/bin/sh")
	cmd.Stdin = strings.NewReader("cd " + remote.Quote(filepath.Join(dev, "current")) + " && devopsy --import db --resolved --yes\n")
	out, _ := cmd.CombinedOutput()
	if want := "sync it:\ndevopsy: | $ devopsy @devopsy@one:dev --sync\n"; !strings.Contains(string(out), want) {
		t.Errorf("in dev's release, from a shell opened on prod, want %q:\n%s", want, out)
	}
}

// A command offered to run again copies whole: every line of it after a "$ ", or the way
// out in words.
//
// Bug (review): the operator's command is offered as typed, and report.Lines cuts a remedy
// at its newlines, so a command with an argument of several lines is offered with its first
// line after "$ " and the rest as text after the bar, which copied runs something else.
func TestRunItAgainIsNeverACutCommand(t *testing.T) {
	t.Parallel()
	s := newImportSetup(t)
	write(t, filepath.Join(s.base, "current", ".devopsy", "commands", "fix"), "#!/bin/sh\nexec devopsy --import db --resolved --yes\n", 0o755)
	if _, out, code := s.runIO(append(s.env, "FAKE_FAIL_IMPORT=1"), []byte("rows\n"), "@prod", "--import", "db", "--yes"); code == 0 || !s.mark().IsImport() {
		t.Fatalf("a failing import (%d):\n%s", code, out)
	}
	_, out, code := s.runIO([]string{"FAIL_START=shop-prod"}, nil, "@prod", "fix", "one\ntwo")
	if code == 0 || !strings.Contains(out, "run it again") {
		t.Fatalf("a resolve that fails to start (%d):\n%s", code, out)
	}
	if cut := "devopsy: | $ devopsy @prod fix 'one\ndevopsy: | two'\n"; strings.Contains(out, cut) {
		t.Errorf("the command to run again is cut over two lines:\n%s", out)
	}
}
