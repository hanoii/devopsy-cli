package remote

import (
	"strings"
	"testing"
)

// getenv answers from m, as os.Getenv does.
func getenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// A devopsy that runs in a session passes its command on unchanged, so the sessions it opens
// (a server's to another) carry the operator's; anywhere else its own arguments as typed. A
// command past its cap is none, rather than a cut one.
func TestOperatorCommandIsTheSessions(t *testing.T) {
	t.Parallel()
	args := []string{"@vm1:prod", "--sync-push"}
	if got := OperatorCommand(getenv(map[string]string{CommandEnv: "devopsy @dev --sync --from @prod"}), args); got != "devopsy @dev --sync --from @prod" {
		t.Errorf("in a session: %q", got)
	}
	if got := OperatorCommand(getenv(nil), args); got != "devopsy @vm1:prod --sync-push" {
		t.Errorf("outside one: %q", got)
	}
	if got := OperatorCommand(getenv(nil), []string{strings.Repeat("x", commandMax)}); got != "" {
		t.Errorf("past its cap: %d bytes", len(got))
	}
}

// What a session carries is taken only as devopsy writes it: a command within its cap, and
// both addresses only when each parses as an environment's (an alias, a server with a port
// or an IPv6 address, an instance), never a server alone, a word with a shell's characters
// or one past its cap. What is not is dropped, never an error.
func TestSessionFromTakesOnlyWhatParses(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		env  map[string]string
		want Session
	}{
		{map[string]string{CommandEnv: "devopsy @box restart web", AddressEnv: "box", HintAddressEnv: "devopsy@vm1:2222:eu/prod"},
			Session{"devopsy @box restart web", "box", "devopsy@vm1:2222:eu/prod"}},
		{map[string]string{AddressEnv: "devopsy@[2001:db8::1]:prod", HintAddressEnv: "devopsy@[2001:db8::1]:prod"},
			Session{"", "devopsy@[2001:db8::1]:prod", "devopsy@[2001:db8::1]:prod"}},
		{map[string]string{CommandEnv: "rm -rf /", AddressEnv: "vm1:", HintAddressEnv: "vm1:"}, Session{}},
		{map[string]string{AddressEnv: "prod", HintAddressEnv: "vm1;reboot:prod"}, Session{}},
		{map[string]string{AddressEnv: "prod$(id)", HintAddressEnv: "prod"}, Session{}},
		{map[string]string{CommandEnv: "devopsy " + strings.Repeat("x", commandMax), AddressEnv: strings.Repeat("a", addressMax+1), HintAddressEnv: "prod"}, Session{}},
	} {
		if got := SessionFrom(getenv(c.env)); got != c.want {
			t.Errorf("%v: %+v, want %+v", c.env, got, c.want)
		}
	}
}

// A hint on a server names the session's address, else the release's DEVOPSY_TARGET (read
// only then, and only when it parses), else none: no command, never a placeholder.
func TestSessionOrTheRelease(t *testing.T) {
	t.Parallel()
	read := 0
	release := func(a string) func() string { return func() string { read++; return a } }
	s := SessionFrom(getenv(map[string]string{AddressEnv: "box", HintAddressEnv: "vm1:prod"})).Or(release("vm2:prod"))
	if s.On("--status") != "devopsy @box --status" || s.Releasing("--sync") != "devopsy @vm1:prod --sync" || read != 0 {
		t.Errorf("the session's: %q, %q, the release read %d times", s.On("--status"), s.Releasing("--sync"), read)
	}
	s = Session{}.Or(release("vm2:prod"))
	if s.On("--status") != "devopsy @vm2:prod --status" || s.Releasing("--sync") != "devopsy @vm2:prod --sync" {
		t.Errorf("the release's: %q, %q", s.On("--status"), s.Releasing("--sync"))
	}
	for _, none := range []string{"", "<target>"} {
		if s := (Session{}).Or(release(none)); s.On("--status") != "" || s.Releasing("--sync") != "" {
			t.Errorf("none (%q): %q, %q", none, s.On("--status"), s.Releasing("--sync"))
		}
	}
}
