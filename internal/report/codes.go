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

// Codes is the registry, every code once, which docs/codes.md lists.
var Codes = []Code{FirewallNotLoaded}

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

// WarnForAdmin warns of c to a reader who can only pass it on: Msg, then ForAdmin.
func (l *Logger) WarnForAdmin(c Code) { l.Say(WarnLevel, c.Msg(), []string{c.ForAdmin()}) }
