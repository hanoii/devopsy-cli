package remote

import (
	"cmp"
	"strings"
)

// What every session carries for the hints a devopsy on the server says (SSHStreams):
// devopsy talking to devopsy, internal, and only ever a hint's command, never a decision.
const (
	// CommandEnv is the operator's command as typed (Command), the same through every session
	// of one operation: what a hint to run it again offers.
	CommandEnv = "DEVOPSY_INTERNAL_COMMAND"
	// AddressEnv is the address of the target the session is on, as the operator would type
	// it (TypedAddress): what a hint of another command on that environment names.
	AddressEnv = "DEVOPSY_INTERNAL_ADDRESS"
	// HintAddressEnv is that address as a hint to release, roll back, sync or restore names
	// it (HintAddress).
	HintAddressEnv = "DEVOPSY_INTERNAL_HINT_ADDRESS"
)

// commandMax and addressMax cap what a session's values may hold, wherever they are read
// (SessionFrom) or sent: a value is an argument of a hint, and the door's caller sets them.
const (
	commandMax = 2048
	addressMax = 512
)

// Command is the operator's command, exported into every session as CommandEnv: the
// session's own when devopsy runs in one (a server's devopsy passes it on unchanged), else
// this devopsy's arguments as typed (OperatorCommand). Set by main.
var Command string

// OperatorCommand is the command a session carries: the one the session this devopsy runs
// in carries (getenv's CommandEnv, checked as SessionFrom does), else args, this devopsy's
// own after its name, as one command to copy (CommandOf); none when longer than its cap.
func OperatorCommand(getenv func(string) string, args []string) string {
	if c := SessionFrom(getenv).Command; c != "" {
		return c
	}
	if c := CommandOf(args); len(c) <= commandMax {
		return c
	}
	return ""
}

// Session is what a devopsy on a server knows of the session it runs in, for its hints
// alone: the operator's command, and the address of the session's environment as typed and
// as a release's hint names it, each "" when the session carries none.
type Session struct {
	Command, Address, Hint string
}

// SessionFrom reads the session's values from getenv: the command only as Command writes
// it (devopsy's, within its cap), the addresses only together and only when each parses as
// an environment's address within its cap. A value that does not is dropped, never an
// error: a hint then says its way out in words.
func SessionFrom(getenv func(string) string) Session {
	var s Session
	if c := getenv(CommandEnv); len(c) <= commandMax && strings.HasPrefix(c, "devopsy ") {
		s.Command = c
	}
	if a, h := getenv(AddressEnv), getenv(HintAddressEnv); IsAddress(a) && IsAddress(h) {
		s.Address, s.Hint = a, h
	}
	return s
}

// IsAddress says whether s is an environment's address, as typed after @, within its cap:
// an alias, or [<server>:][<instance>/]<environment> with a server ParseServer takes.
func IsAddress(s string) bool {
	if s == "" || len(s) > addressMax {
		return false
	}
	a, err := parseAddress(s)
	if err != nil {
		return false
	}
	if a.server != "" {
		if _, _, ok := ParseServer(a.server); !ok {
			return false
		}
	}
	return true
}

// CommandOf is devopsy's arguments as one command to copy: devopsy, then each word as a
// shell reads it back (QuoteWord), so the words come back as they were typed.
func CommandOf(args []string) string {
	words := []string{"devopsy"}
	for _, a := range args {
		words = append(words, QuoteWord(a))
	}
	return strings.Join(words, " ")
}

// TypedAddress names t as the operator would type it, for a hint: the alias as typed, a
// server named alone as <host>:, else the resolved address (an instance, a server from
// variables), so the hint works when copied.
func TypedAddress(t *Target) string {
	switch {
	case t.Alias != "":
		return t.Alias
	case t.Name == "":
		return ServerRef(t)
	}
	return cmp.Or(t.Address, t.Name)
}

// sessionExports is the head of a session's script on t: Command, and t's address as typed
// and as a release's hint names it, unless t is a server named alone, which has no
// environment to name. Each value is one quoted word of the script, whatever it holds.
func sessionExports(t *Target) string {
	s := ""
	if Command != "" && len(Command) <= commandMax {
		s += CommandEnv + "=" + Quote(Command) + "; export " + CommandEnv + "\n"
	}
	if t.Name != "" {
		if typed, hint := TypedAddress(t), HintAddress(t); len(typed) <= addressMax && len(hint) <= addressMax {
			s += AddressEnv + "=" + Quote(typed) + " " + HintAddressEnv + "=" + Quote(hint) + "; export " + AddressEnv + " " + HintAddressEnv + "\n"
		}
	}
	return s
}
