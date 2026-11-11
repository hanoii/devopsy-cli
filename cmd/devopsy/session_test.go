package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// saidCommand prints what a session carries for a server's hints, as the server's
// processes see it.
const saidCommand = "#!/bin/sh\nprintf 'command=%s|address=%s|hint=%s\\n' \"${DEVOPSY_INTERNAL_COMMAND-unset}\" \"${DEVOPSY_INTERNAL_ADDRESS-unset}\" \"${DEVOPSY_INTERNAL_HINT_ADDRESS-unset}\"\n"

// sessionScripts are the scripts of the sessions FAKE_SSH_SCRIPTS logged in file, unquoted
// from the sh -c word ssh got.
func sessionScripts(t *testing.T, file string) []string {
	t.Helper()
	var scripts []string
	for _, s := range strings.Split(readString(file), "----\n") {
		s = strings.TrimSuffix(s, "\n")
		if q, ok := strings.CutPrefix(s, "sh -c '"); ok && strings.HasSuffix(q, "'") {
			scripts = append(scripts, strings.ReplaceAll(strings.TrimSuffix(q, "'"), `'\''`, "'"))
		}
	}
	return scripts
}

// Every session carries the operator's command as typed, its words as a shell reads them
// back (spaces, quotes, a newline, a leading dash, an empty word, an IPv6 server in
// brackets), and the address of the environment that session is on, as typed and as a
// release's hint names it (an alias without source: its resolved address): in a sync, the
// holder's the child's and the push's the parent's, both the one command. A devopsy a
// project command runs locally opens its sessions with its own command, never its caller's.
func TestSessionsCarryTheCommandAndTheirAddress(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "commands", "said"), saidCommand, 0o755)
	write(t, filepath.Join(s.dot, "commands", "outer"), "#!/bin/sh\n## Run: local\nexec devopsy @prod said inner\n", 0o755)
	home := ""
	for _, kv := range s.env {
		if v, ok := strings.CutPrefix(kv, "DEVOPSY_HOME="); ok {
			home = v
		}
	}
	write(t, filepath.Join(home, "config.yaml"), "releases:\n  root: "+s.root+"\naliases:\n  box: {to: \"devopsy@one:prod\", project: shop}\n", 0o644)
	s.releaseProd(nil)

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"@devopsy@[::1]:prod", "said", "a b", "it's", "-n", ""},
			"command=devopsy '@devopsy@[::1]:prod' said 'a b' 'it'\\''s' -n ''|address=devopsy@[::1]:prod|hint=devopsy@[::1]:prod\n"},
		// An argument of several lines: no command carried, which a hint would offer cut.
		{[]string{"@box", "said", "x\ny"}, "command=unset|address=box|hint=devopsy@one:prod\n"},
		{[]string{"@box", "said"}, "command=devopsy @box said|address=box|hint=devopsy@one:prod\n"},
		{[]string{"outer"}, "command=devopsy @prod said inner|address=devopsy@one:prod|hint=devopsy@one:prod\n"},
	} {
		if out, code := s.run(nil, c.args...); code != 0 || !strings.Contains(out, c.want) {
			t.Errorf("%q (%d), want %q:\n%s", c.args, code, c.want, out)
		}
	}

	scripts := filepath.Join(s.root, "..", "scripts")
	if out, code := s.run([]string{"FAKE_SSH_SCRIPTS=" + scripts}, "@dev", "--release"); code != 0 {
		t.Fatalf("dev's first release, which syncs (%d):\n%s", code, out)
	}
	sessions := sessionScripts(t, scripts)
	holder, push := 0, 0
	for _, sc := range sessions {
		if !strings.Contains(sc, remote.CommandEnv+"='devopsy @dev --release'; export "+remote.CommandEnv+"\n") {
			t.Errorf("a session without the command:\n%s", sc)
		}
		want := "devopsy@one:dev"
		switch {
		case strings.Contains(sc, "--sync-push"):
			push++
			want = "devopsy@one:prod"
		case strings.Contains(sc, "ready for sync in"):
			holder++
		default:
			continue
		}
		if !strings.Contains(sc, remote.AddressEnv+"='"+want+"' "+remote.HintAddressEnv+"='"+want+"' "+remote.EnvironmentEnv+"='shop-"+want[strings.LastIndex(want, ":")+1:]+"'; export ") {
			t.Errorf("a sync's session not on %s's address:\n%s", want, sc)
		}
	}
	if holder != 1 || push != 1 {
		t.Errorf("%d holders and %d pushes among %d sessions", holder, push, len(sessions))
	}
}

// The push's sessions to the child (the same server's --sync-exec, another's sync key) run
// the child's devopsy with the child's address for its hints, the one the push has for it,
// never the push's own session's, which is the parent's; the operator's command goes on to
// both, to another server's in the request, sshd's environment bringing none.
func TestSyncHopsNameTheChild(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	said := filepath.Join(t.TempDir(), "said")
	caps := filepath.Join(s.dot, "capabilities", "data")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho \"dump of $1\"\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\ncat >/dev/null\nprintf '%s|%s|%s\\n' \"${DEVOPSY_INTERNAL_ADDRESS-unset}\" \"${DEVOPSY_INTERNAL_HINT_ADDRESS-unset}\" \"${DEVOPSY_INTERNAL_COMMAND-unset}\" >>"+said+"\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.labels("FAKE_DB=stream")
	for _, c := range []struct {
		env  []string
		args []string
		want string
	}{
		{nil, []string{"@dev", "--sync", "--yes"}, "devopsy@one:dev|devopsy@one:dev|devopsy @dev --sync --yes\n"},
		{[]string{"DEVOPSY_SERVER_PROD=devopsy@one"}, []string{"@devopsy@two:dev", "--sync", "--yes"}, "devopsy@two:dev|devopsy@two:dev|devopsy @devopsy@two:dev --sync --yes\n"},
	} {
		write(t, said, "", 0o644)
		if out, code := s.run(c.env, c.args...); code != 0 || readString(said) != c.want {
			t.Errorf("%v (%d): the child's import saw %q, want %q:\n%s", c.args, code, readString(said), c.want, out)
		}
	}
}

// What a devopsy on a server says names what the operator typed, from the session, whether a
// flag runs it there or a project command does: a hint of another command on the environment
// names the address as typed (an alias without source:, never the release's resolved
// DEVOPSY_TARGET), one to sync its hint's form (an alias with source:), and one to run it
// again the operator's command itself, the project command's included. A shell's session
// carries the address and no command: what runs in it is the operator's typing.
func TestServerHintsNameWhatTheOperatorTyped(t *testing.T) {
	t.Parallel()
	s := newImportSetup(t)
	home := ""
	for _, kv := range s.syncSetup.env {
		if v, ok := strings.CutPrefix(kv, "DEVOPSY_HOME="); ok {
			home = v
		}
	}
	write(t, filepath.Join(home, "config.yaml"), "releases:\n  root: "+s.root+"\naliases:\n  box: {to: \"devopsy@one:prod\", project: shop}\n  live: {to: \"devopsy@one:prod\", source: "+s.project+"}\n", 0o644)
	// A project command in the release, which runs devopsy there.
	write(t, filepath.Join(s.base, "current", ".devopsy", "commands", "fix"), "#!/bin/sh\nexec devopsy --import db --resolved --yes\n", 0o755)
	write(t, filepath.Join(s.base, "current", ".devopsy", "commands", "said"), saidCommand, 0o755)

	if _, out, code := s.runIO(append(s.env, "FAKE_FAIL_IMPORT=1"), []byte("rows\n"), "@box", "--import", "db", "--yes"); code == 0 || !s.mark().IsImport() ||
		!strings.Contains(out, "see the ways out:\ndevopsy: | $ devopsy @box --status\n") {
		t.Fatalf("a failing import's way out by the address typed (%d):\n%s", code, out)
	}
	for _, args := range [][]string{{"@box", "--import", "db", "--resolved", "--yes"}, {"@box", "fix"}} {
		want := "run it again:\ndevopsy: | $ " + remote.CommandOf(args) + "\n"
		if _, out, code := s.runIO([]string{"FAIL_START=shop-prod"}, nil, args...); code == 0 || !strings.Contains(out, want) {
			t.Errorf("%v (%d), want %q:\n%s", args, code, want, out)
		}
	}
	write(t, filepath.Join(s.base, remote.SyncIncompleteFile), remote.IncompleteLine("2026-10-22 10:00:00", "vm1:live", remote.IncompleteCopy, "the copy was cut"), 0o644)
	if _, out, code := s.runIO(nil, nil, "@live", "fix"); code == 0 || !strings.Contains(out, "sync it:\ndevopsy: | $ devopsy @live --sync\n") {
		t.Errorf("a sync's mark, by the hint's form of the address typed (%d):\n%s", code, out)
	}
	typed := strings.TrimPrefix(saidCommand, "#!/bin/sh\n")
	if out, errs, code := s.runIO([]string{"SHELL=/bin/sh"}, []byte(typed), "@box", "--shell-host"); code != 0 || !strings.Contains(string(out), "command=unset|address=box|hint=devopsy@one:prod\n") {
		t.Errorf("a shell (%d):\n%s%s", code, out, errs)
	}
}

// A devopsy typed in a host shell names, in its hints, the environment of the release it
// runs in, never the one the shell was opened on: the operator moves in a shell, and a
// command copied from a hint must act on the environment that said it.
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
	cmd.Stdin = strings.NewReader("cd " + remote.Quote(filepath.Join(dev, "current")) + " && " + remote.Quote(binary) + " --import db --resolved --yes\n")
	out, _ := cmd.CombinedOutput()
	if want := "sync it:\ndevopsy: | $ devopsy @devopsy@one:dev --sync\n"; !strings.Contains(string(out), want) {
		t.Errorf("in dev's release, from a shell opened on prod, want %q:\n%s", want, out)
	}
}

// A command offered to run again copies whole: every line of it after a "$ ", or the way
// out in words.
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
