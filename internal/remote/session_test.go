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

// What a session carries is taken only as devopsy writes it: a command within its cap, both
// addresses only when each parses as an environment's (an alias, a server with a port or an
// IPv6 address, an instance), never a server alone, and the server's command's address as an
// environment's or a server alone; never a word with a shell's characters or one past its cap.
// What is not is dropped, never an error.
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
		// The address a server's command takes: a server named alone too, or an environment's.
		{map[string]string{ServerEnv: "devopsy@vm1:2222:"}, Session{Server: "devopsy@vm1:2222:"}},
		{map[string]string{ServerEnv: "devopsy@[2001:db8::1]:"}, Session{Server: "devopsy@[2001:db8::1]:"}},
		{map[string]string{ServerEnv: "box"}, Session{Server: "box"}},
		{map[string]string{ServerEnv: "vm1;reboot:"}, Session{}},
		{map[string]string{ServerEnv: "vm1\nx:"}, Session{}},
		{map[string]string{ServerEnv: strings.Repeat("a", addressMax) + ":"}, Session{}},
		// The server's SSH destination, which a hint to log in as another user names.
		{map[string]string{HostEnv: "admin@vm1:2222"}, Session{Host: "admin@vm1:2222"}},
		{map[string]string{HostEnv: "vm1;reboot"}, Session{}},
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

// A session on a server named alone carries its address for a server's command, and no
// environment's; one on an environment carries both, as typed; each carries its SSH destination.
func TestSessionExportsTheServer(t *testing.T) {
	t.Parallel()
	alone := sessionExports(&Target{Host: "devopsy@vm1"})
	if !strings.Contains(alone, ServerEnv+"='devopsy@vm1:'; export "+ServerEnv+"\n") || strings.Contains(alone, AddressEnv) || !strings.Contains(alone, HostEnv+"='devopsy@vm1'; export "+HostEnv+"\n") {
		t.Errorf("a server named alone:\n%s", alone)
	}
	env := sessionExports(&Target{Host: "devopsy@vm1", Name: "prod", Alias: "box", Path: "shop/prod"})
	if !strings.Contains(env, ServerEnv+"='box'; export "+ServerEnv+"\n") || !strings.Contains(env, AddressEnv+"='box'") {
		t.Errorf("an environment:\n%s", env)
	}
}

// AsUser names the server at a destination logged in as another user, its login replaced, its
// port kept, an IPv6 address without one bracketed for the colon after it.
// CommandAs is the session's command as typed, its address logged in as another user on the
// session's host: what W0009 offers to run again as the deploy user. A command whose address
// names no server (an alias, a server from variables), or a session without its command or its
// host, has none, and the hint says it in words.
func TestCommandAs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ command, host, want string }{
		{"devopsy @admin@vm1: --upgrade", "admin@vm1", "devopsy @deploy@vm1: --upgrade"},
		{"devopsy @admin@vm1:2222:prod --release", "admin@vm1:2222", "devopsy @deploy@vm1:2222:prod --release"},
		{"devopsy @vm1:eu/prod --vars set 'A=b c'", "vm1", "devopsy @deploy@vm1:eu/prod --vars set 'A=b c'"},
		{"devopsy '@admin@[2001:db8::1]:22:' --server-info", "admin@[2001:db8::1]:22", "devopsy '@deploy@[2001:db8::1]:22:' --server-info"},
		{"devopsy -v @admin@vm1: --gc", "admin@vm1", "devopsy -v @deploy@vm1: --gc"},
		{"devopsy @prod --release", "vm1", ""},
		{"devopsy @admin@vm1: --upgrade", "", ""},
		{"", "vm1", ""},
	} {
		if got := (Session{Command: c.command, Host: c.host}).CommandAs("deploy"); got != c.want {
			t.Errorf("CommandAs of %q on %q = %q, want %q", c.command, c.host, got, c.want)
		}
	}
}

func TestAsUser(t *testing.T) {
	t.Parallel()
	for host, want := range map[string]string{
		"vm1":                    "deploy@vm1:",
		"admin@vm1":              "deploy@vm1:",
		"admin@vm1:2222":         "deploy@vm1:2222:",
		"2001:db8::1":            "deploy@[2001:db8::1]:",
		"admin@[2001:db8::1]:22": "deploy@[2001:db8::1]:22:",
	} {
		if got := AsUser(host, "deploy"); got != want {
			t.Errorf("AsUser(%q) = %q, want %q", host, got, want)
		}
		if _, _, ok := ParseServer(strings.TrimSuffix(AsUser(host, "deploy"), ":")); !ok {
			t.Errorf("AsUser(%q) does not parse back", host)
		}
	}
}
