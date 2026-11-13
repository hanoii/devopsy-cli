package cli

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/howto"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// Scope is where a flag of devopsy's runs: which address it takes.
type Scope int

const (
	// Local runs on this machine only, without a target.
	Local Scope = iota
	// LocalOrTarget runs locally, or with @<target> on it: [@t].
	LocalOrTarget
	// Target needs @<target>: @t.
	Target
	// Server needs a server: @<server> or any target's (remote.ServerTarget): @s.
	Server
	// LocalOrServer runs locally, or with @<server> on one: [@s].
	LocalOrServer
)

// prefix is how help shows a scope, before the flag.
func (s Scope) prefix() string {
	return [...]string{"", "[@t]", "@t", "@s", "[@s]"}[s]
}

// Remote reports whether the scope takes an address.
func (s Scope) Remote() bool { return s != Local }

// Unaddressed reports whether the scope runs without an address.
func (s Scope) Unaddressed() bool { return s == Local || s == LocalOrTarget || s == LocalOrServer }

// Flag is one of devopsy's own flags, as help and completion show it.
type Flag struct {
	Name  string // as typed, like --release
	Also  string // another spelling, like -h, shown after a comma
	Args  string // what follows it, for help
	Scope Scope
	Desc  string
}

// FlagGroup is a heading of devopsy's help and its flags, in order.
type FlagGroup struct {
	Title string
	Flags []Flag
}

// Flags are devopsy's own flags, by task, common first: what help lists,
// completion offers, and which scope each resolves its address with. A
// flag that works locally and on servers differently (--upgrade) is listed
// once for each. A new flag goes here.
var Flags = []FlagGroup{
	{"Releases", []Flag{
		{Name: "--release", Args: "[--from @<target>] [--accept-image <svc>] [--yes]", Scope: Target, Desc: "upload a release, switch to it, run it\n(a child's first one syncs from its parent)"},
		{Name: "--release-sync", Args: "[--from @<target>] [--accept-image <svc>] [--yes]", Scope: Target, Desc: "the same, and sync the parent's data in"},
		{Name: "--release-no-sync", Args: "[--accept-image <svc>] [--yes]", Scope: Target, Desc: "the same, never syncing"},
		{Name: "--release", Scope: Local, Desc: "locally, in place: prepare, images, run\n(defaults: release steps)"},
		{Name: "--rollback", Args: "[--accept-image <svc>] [--yes]", Scope: Target, Desc: "back to the previous release"},
		{Name: "--reload", Scope: LocalOrTarget, Desc: "the release again with its variables and\nimports as they are now: its reload: step"},
		{Name: "--releases", Args: "[remove <id|failed>... [--yes]]", Scope: Target, Desc: "the releases there; or remove some,\nwith the images only they hold"},
		{Name: "--log", Args: "[<id>]", Scope: Target, Desc: "what the last release (or <id>) printed"},
		{Name: "--destroy", Args: "[--yes] [--purge]", Scope: Target, Desc: "remove the environment, data included"},
		{Name: "--destroy", Args: "[--yes]", Scope: Local, Desc: "locally, in place: the destroy step\n(defaults: destroy)"},
	}},
	{"Data", []Flag{
		{Name: "--sync", Args: "[--from @<target>] [--accept-image <svc>] [--yes]", Scope: Target, Desc: "a child's data from its parent, then run"},
		{Name: "--reset", Args: "[--from @<target>] [--accept-image <svc>] [--yes]", Scope: Target, Desc: "a child wiped and synced anew, then run"},
		{Name: "--inherit", Scope: Target, Desc: "the parent's inherit: variables again"},
		{Name: "--export", Args: "<service>", Scope: LocalOrTarget, Desc: "a service's data, to stdout"},
		{Name: "--import", Args: "<service> [--accept-image <svc>] [--yes] [--resolved]", Scope: LocalOrTarget, Desc: "a service's data, from stdin"},
		{Name: "--snapshot", Scope: Target, Desc: "a snapshot: the data, code and images,\nits services stopped for the copy"},
		{Name: "--snapshots", Args: "[restore | remove | export | import ...]", Scope: Target, Desc: "the snapshots there; or restore one,\nremove, export or import some"},
	}},
	{"Variables", []Flag{
		{Name: "--env", Scope: LocalOrTarget, Desc: "what devopsy loads and computes (.env)"},
		{Name: "--vars", Args: "[get | set | unset KEY...]", Scope: Target, Desc: "the environment's (shared/.env)"},
		{Name: "--vars-instance", Args: "[...]", Scope: Target, Desc: "the instance's .env"},
		{Name: "--vars-project", Args: "[...]", Scope: Server, Desc: "the project's .env on the server"},
	}},
	{"Running", []Flag{
		{Name: "--status", Args: "[--json]", Scope: LocalOrTarget, Desc: "containers, and the project's own status"},
		{Name: "--shell", Args: "[service] [-- command...]", Scope: LocalOrTarget, Desc: "a shell, or the command, in a container"},
		{Name: "--shell-host", Args: "[--root]", Scope: Server, Desc: "a shell on the server itself, or root's"},
		{Name: "--environments", Scope: LocalOrServer, Desc: "the project's environments on its servers"},
		{Name: "--debug", Args: "[topic]", Scope: LocalOrTarget, Desc: "what devopsy sees (--debug --help)"},
	}},
	{"Servers and SSH", []Flag{
		{Name: "--ssh-keys-generate", Args: "[<name>]", Scope: Local, Desc: "a local SSH key pair, for CI"},
		{Name: "--project-users", Args: "[list | add <project> | remove <project>]", Scope: Server, Desc: "project users, with no Docker socket"},
		{Name: "--ssh-keys", Args: "[--for <project>] [list | add <file> | remove <key>]", Scope: Server, Desc: "keys of the deploy user, or a project user"},
		{Name: "--ssh-config", Scope: Server, Desc: "a ~/.ssh/config block for the server"},
		{Name: "--server-info", Args: "[--json]", Scope: LocalOrServer, Desc: "the server, its disks, Docker, roles and\nenvironments, and what needs attention"},
		{Name: "--server-setup", Args: "[--yes]", Scope: Server, Desc: "the server's setup for devopsy: Docker's\ndaemon.json, buffers, the release root"},
		{Name: "--gc", Args: "[--dry-run] [--yes]", Scope: Server, Desc: "remove what nothing on the server owns:\nold one-offs and build cache, snapshots\nof releases gone, images left"},
		{Name: howto.Flag, Args: "[<topic>|--all]", Scope: Local, Desc: "the steps, as root, for what devopsy\nchecks on a server and cannot set up"},
		{Name: "--upgrade", Args: "[v]", Scope: Server, Desc: "the server's devopsy, to latest or v"},
	}},
	{"Setup and tools", []Flag{
		{Name: "--init", Args: "[project]", Scope: Local, Desc: "a new .devopsy/config.yaml"},
		{Name: "--context-hash", Args: "[service]", Scope: Local, Desc: "a hash of an image's build inputs"},
		{Name: "--probe", Args: "<host>...", Scope: Local, Desc: "DNS, certificates, HTTPS, checked locally"},
		{Name: "--upgrade", Args: "[v]", Scope: Local, Desc: "this devopsy, to latest or v"},
		{Name: "--completion", Args: "<shell>", Scope: Local, Desc: "shell completion: bash, zsh or fish"},
		{Name: "--ai-skill", Args: "[--stub] [install [--global]]", Scope: Local, Desc: "the skill for agents: print it,\nor install it"},
		{Name: "--version", Scope: Local, Desc: "devopsy's, docker's and compose's versions"},
		{Name: "-v", Also: "-vv", Scope: Local, Desc: "first: what devopsy runs; -vv: scripts too"},
		{Name: SSHOptionFlag, Args: "<key=value>", Scope: Local, Desc: "first, repeatable: an ssh -o for every\nsession this command opens"},
		{Name: "--help", Also: "-h", Scope: LocalOrTarget, Desc: "this help; after a flag, its own"},
	}},
}

// flagsWhere lists the flags whose scope ok accepts, once each, in order.
func flagsWhere(ok func(Scope) bool) []Flag {
	var out []Flag
	for _, g := range Flags {
		for _, f := range g.Flags {
			if ok(f.Scope) && !slices.ContainsFunc(out, func(o Flag) bool { return o.Name == f.Name }) {
				out = append(out, f)
			}
		}
	}
	return out
}

// LocalFlags are the flags that run without a target.
func LocalFlags() []Flag { return flagsWhere(Scope.Unaddressed) }

// RemoteFlags are the flags that take an address.
func RemoteFlags() []Flag { return flagsWhere(Scope.Remote) }

func names(flags []Flag) []string {
	var out []string
	for _, f := range flags {
		out = append(out, f.Name)
	}
	return out
}

// RemoteOnly are devopsy's flags that need an address: used alone, they
// say so.
var RemoteOnly = func() []string {
	local := names(LocalFlags())
	var out []string
	for _, n := range names(RemoteFlags()) {
		if !slices.Contains(local, n) {
			out = append(out, n)
		}
	}
	return out
}()

// LoneUnknownFlag says why args, one argument starting with - that is no
// flag of devopsy's, cannot run: compose's flags never stand alone (a
// flag followed by anything passes through to compose), so a lone flag
// devopsy does not know is a typo. Empty otherwise. The nearest flag of
// devopsy's is suggested (Nearest).
func LoneUnknownFlag(args []string) error {
	if len(args) != 1 || !strings.HasPrefix(args[0], "-") || args[0] == "--" {
		return nil
	}
	word := args[0]
	all := names(flagsWhere(func(Scope) bool { return true }))
	if slices.Contains(all, word) {
		return nil
	}
	nearest := Nearest(word, all)
	why := fmt.Sprintf("%s is not a devopsy flag", word)
	if nearest != "" {
		why += fmt.Sprintf(" (did you mean %s?)", nearest)
	}
	return &report.Failure{Msg: why, Remedies: []report.Remedy{report.Hint("devopsy's flags:", "devopsy --help")}}
}

// Nearest is the name of names nearest word, for a "did you mean": one a couple of letters
// off, or up to four when the two share a hyphen-separated part (--ssh-host and
// --shell-host), "" for none.
func Nearest(word string, names []string) string {
	nearest, best := "", 5
	for _, n := range names {
		d := editDistance(n, word)
		if d > 2 && !sharePart(n, word) {
			continue
		}
		if d < best {
			best, nearest = d, n
		}
	}
	return nearest
}

// sharePart reports whether two flags have a hyphen-separated part in
// common (--ssh-host and --shell-host: host).
func sharePart(a, b string) bool {
	parts := strings.Split(strings.TrimLeft(b, "-"), "-")
	for _, p := range strings.Split(strings.TrimLeft(a, "-"), "-") {
		if p != "" && slices.Contains(parts, p) {
			return true
		}
	}
	return false
}

// editDistance is the Levenshtein distance between two words.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// ServerScope are the flags that need only a server: they take @<server>
// or @<server>: as well as any target (remote.ServerTarget).
var ServerScope = names(flagsWhere(func(s Scope) bool { return s == Server || s == LocalOrServer }))

// helpColumn is where descriptions start in devopsy's help.
const helpColumn = 36

// helpLine is one entry of devopsy's help: the scope's prefix, right
// aligned, what you type, then the description, on the next line when
// what you type is too long.
func helpLine(b *strings.Builder, prefix, typed, desc string) {
	left := fmt.Sprintf("  %4s %s", prefix, typed)
	lines := strings.Split(desc, "\n")
	if len(left)+2 > helpColumn {
		b.WriteString(left + "\n")
	} else if desc != "" {
		fmt.Fprintf(b, "%-*s%s\n", helpColumn, left, lines[0])
		lines = lines[1:]
	} else {
		b.WriteString(left + "\n")
		return
	}
	// A description of several lines continues under itself.
	for _, l := range lines {
		fmt.Fprintf(b, "%*s%s\n", helpColumn, "", l)
	}
}

// flagsHelp is the help's list of flags, by group.
func flagsHelp() string {
	var b strings.Builder
	for _, g := range Flags {
		b.WriteString(g.Title + ":\n")
		for _, f := range g.Flags {
			typed := f.Name
			if f.Also != "" {
				typed += ", " + f.Also
			}
			if f.Args != "" {
				typed += " " + f.Args
			}
			helpLine(&b, f.Scope.prefix(), typed, f.Desc)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// SSHOptionFlag is --ssh-option <key=value>, before everything else like
// -v: an ssh -o for every session of this command (remote.SSHOptionsEnv).
const SSHOptionFlag = "--ssh-option"

// SSHOptionValue is what --ssh-option takes: ssh's -o key=value, never
// another of ssh's arguments, and one line.
var SSHOptionValue = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*=[^\n]+$`)
