package report

// Code names a condition a message is said with, so whoever can only pass the message on (a
// project user) and whoever acts on it (the server's admin) name it the same: W and four
// digits, each with one meaning, never reused or renumbered once released. Desc is what the
// message's first line says after the code (Msg). Only what is said for an admin has a code.
type Code struct {
	ID, Desc string
}

// FirewallNotLoaded: devopsy's firewall on the server is not as devopsy loads it (a reload of
// the server's own firewall, an iptables -F, a failed load).
var FirewallNotLoaded = Code{"W0001", "the server's firewall is not loaded as devopsy loads it"}

// SpaceUnguarded: the door's space, the directory where devopsy keeps project users' data, is
// not on a mount with nosuid and nodev (none, or one without them), so the door refuses every
// request.
var SpaceUnguarded = Code{"W0002", "the project users' directory is not on a mount with nosuid and nodev"}

// Codes is the registry, every code once, which docs/codes.md lists.
var Codes = []Code{FirewallNotLoaded, SpaceUnguarded}

// CodeOf is the registry's code by its ID.
func CodeOf(id string) (Code, bool) {
	for _, c := range Codes {
		if c.ID == id {
			return c, true
		}
	}
	return Code{}, false
}

// Msg is a coded message's first line, the same for every reader: the code, a colon, its
// description.
func (c Code) Msg() string { return c.ID + ": " + c.Desc }

// ForAdmin is a coded message's detail for a reader who can only pass it on, the code
// repeated in it, and no way out: what the admin does is the admin's (docs/codes.md).
func (c Code) ForAdmin() string { return "report " + c.ID + " to the server's admin" }

// SayCode says c at lv, its first line Msg, with the details and remedies of a reader who acts
// on it.
func (l *Logger) SayCode(lv Level, c Code, details []string, remedies ...Remedy) {
	l.Say(lv, c.Msg(), details, remedies...)
}

// SayForAdmin says c at lv to a reader who can only pass it on: Msg, then ForAdmin, and no
// remedy (a warning of what goes on, an error of what the reader's command could not do).
func (l *Logger) SayForAdmin(lv Level, c Code) { l.Say(lv, c.Msg(), []string{c.ForAdmin()}) }
