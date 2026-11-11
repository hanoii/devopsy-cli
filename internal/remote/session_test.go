package remote

import (
	"github.com/hanoii/devopsy-cli/internal/report"
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
			Session{Command: "devopsy @box restart web", Address: "box", Hint: "devopsy@vm1:2222:eu/prod"}},
		{map[string]string{AddressEnv: "devopsy@[2001:db8::1]:prod", HintAddressEnv: "devopsy@[2001:db8::1]:prod", EnvironmentEnv: "shop-prod"},
			Session{Address: "devopsy@[2001:db8::1]:prod", Hint: "devopsy@[2001:db8::1]:prod", Name: "shop-prod"}},
		// A name no compose name holds is dropped, the addresses kept: outside a release they answer.
		{map[string]string{AddressEnv: "box", HintAddressEnv: "vm1:prod", EnvironmentEnv: "Shop Prod"}, Session{Address: "box", Hint: "vm1:prod"}},
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

// A hint on a server names the session's address when the session is on the release's
// environment or outside any release, else the release's DEVOPSY_TARGET (only when it
// parses), else none: no command, never a placeholder.
func TestSessionInTheRelease(t *testing.T) {
	t.Parallel()
	on := SessionFrom(getenv(map[string]string{AddressEnv: "box", HintAddressEnv: "vm1:prod", EnvironmentEnv: "shop-prod"}))
	for _, c := range []struct {
		what          string
		s             Session
		name, address string
		status, sync  string
	}{
		{"the session's, in its environment's release", on, "shop-prod", "vm2:prod", "devopsy @box --status", "devopsy @vm1:prod --sync"},
		{"the session's, outside a release", on, "", "", "devopsy @box --status", "devopsy @vm1:prod --sync"},
		{"the release's, the session on another environment", on, "shop-dev", "vm2:dev", "devopsy @vm2:dev --status", "devopsy @vm2:dev --sync"},
		{"none, the session on another environment and no address in the release", on, "shop-dev", "", "", ""},
		{"the release's, the session naming no environment", Session{Address: "box", Hint: "vm1:prod"}, "shop-prod", "vm2:prod", "devopsy @vm2:prod --status", "devopsy @vm2:prod --sync"},
		{"the release's, without a session", Session{}, "shop-prod", "vm2:prod", "devopsy @vm2:prod --status", "devopsy @vm2:prod --sync"},
		{"none, a release's address that is none", Session{}, "shop-prod", "<target>", "", ""},
		{"none", Session{}, "", "", "", ""},
	} {
		if s := c.s.In(c.name, c.address); s.On("--status") != c.status || s.Releasing("--sync") != c.sync {
			t.Errorf("%s: %q, %q, want %q, %q", c.what, s.On("--status"), s.Releasing("--sync"), c.status, c.sync)
		}
	}
}

// An address a session carries holds no control character: the door's caller sets it, and
// it is one word of a command offered to copy, on one line.
func TestSessionAddressHoldsNoControlCharacter(t *testing.T) {
	t.Parallel()
	for _, a := range []string{"vm1\nsecond line:prod", "vm1\nx:prod", "vm1\rx:prod", "vm1\x1b[2Jx:prod", "devopsy@vm1\x00:prod", "devopsy@vm1\x7f:prod"} {
		if IsAddress(a) {
			t.Errorf("%q is taken as an address", a)
		}
		s := SessionFrom(getenv(map[string]string{AddressEnv: "prod", HintAddressEnv: a}))
		if cmd := s.Releasing("--release"); cmd != "" {
			lines := report.Lines(report.WarnLevel, "a warning", nil, []report.Remedy{report.Fix("release again:", cmd)})
			t.Errorf("%q is offered as a command:\n%s", a, strings.Join(lines, "\n"))
		}
	}
}
