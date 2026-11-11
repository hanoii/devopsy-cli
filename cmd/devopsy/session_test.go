package main

import (
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
		{[]string{"@devopsy@[::1]:prod", "said", "a b", "it's", "x\ny", "-n", ""},
			"command=devopsy '@devopsy@[::1]:prod' said 'a b' 'it'\\''s' 'x\ny' -n ''|address=devopsy@[::1]:prod|hint=devopsy@[::1]:prod\n"},
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
		if !strings.Contains(sc, remote.AddressEnv+"='"+want+"' "+remote.HintAddressEnv+"='"+want+"'; export ") {
			t.Errorf("a sync's session not on %s's address:\n%s", want, sc)
		}
	}
	if holder != 1 || push != 1 {
		t.Errorf("%d holders and %d pushes among %d sessions", holder, push, len(sessions))
	}
}
