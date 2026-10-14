package cli

import (
	"fmt"
	"slices"
	"strings"
)

// Scope is where a flag of devopsy's runs: which address it takes.
type Scope int

const (
	// Here runs on this machine only, without a target.
	Here Scope = iota
	// HereOrTarget runs here, or with @<target> on it: [@t].
	HereOrTarget
	// Target needs @<target>: @t.
	Target
	// Server needs a server: @<server> or any target's (remote.ServerTarget): @s.
	Server
	// HereOrServer runs here, or with @<server> on one: [@s].
	HereOrServer
)

// prefix is how help shows a scope, before the flag.
func (s Scope) prefix() string {
	return [...]string{"", "[@t]", "@t", "@s", "[@s]"}[s]
}

// Remote reports whether the scope takes an address.
func (s Scope) Remote() bool { return s != Here }

// Local reports whether the scope runs without an address.
func (s Scope) Local() bool { return s == Here || s == HereOrTarget || s == HereOrServer }

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
// flag that works here and on servers differently (--upgrade) is listed
// once for each. A new flag goes here.
var Flags = []FlagGroup{
	{"Releases", []Flag{
		{Name: "--release", Args: "[--yes]", Scope: Target, Desc: "upload a release, switch to it, run it\n(a child's first one syncs from its parent)"},
		{Name: "--release-sync", Args: "[--yes]", Scope: Target, Desc: "the same, and sync the parent's data in"},
		{Name: "--release-no-sync", Args: "[--yes]", Scope: Target, Desc: "the same, never syncing"},
		{Name: "--rollback", Scope: Target, Desc: "back to the previous release"},
		{Name: "--releases", Scope: Target, Desc: "the releases there"},
		{Name: "--log", Args: "[<id>]", Scope: Target, Desc: "what the last release (or <id>) printed"},
		{Name: "--destroy", Args: "[--yes]", Scope: Target, Desc: "remove the environment, data included"},
	}},
	{"Data", []Flag{
		{Name: "--sync", Args: "[--from <target>] [--yes]", Scope: Target, Desc: "a child's data from its parent, then run"},
		{Name: "--inherit", Scope: Target, Desc: "the parent's inherit: variables again"},
	}},
	{"Variables", []Flag{
		{Name: "--env", Scope: HereOrTarget, Desc: "what devopsy loads and computes (.env)"},
		{Name: "--vars", Args: "[get | set | unset KEY...]", Scope: Target, Desc: "the environment's (shared/.env)"},
		{Name: "--vars-instance", Args: "[...]", Scope: Target, Desc: "the instance's .env"},
		{Name: "--vars-project", Args: "[...]", Scope: Server, Desc: "the project's .env on the server"},
	}},
	{"Running", []Flag{
		{Name: "--status", Scope: HereOrTarget, Desc: "containers, and the project's own status"},
		{Name: "--shell", Args: "[service] [-- command...]", Scope: HereOrTarget, Desc: "a shell, or the command, in a container"},
		{Name: "--shell-host", Scope: Server, Desc: "a shell on the server itself"},
		{Name: "--environments", Scope: HereOrServer, Desc: "the project's environments on its servers"},
		{Name: "--debug", Args: "[topic]", Scope: HereOrTarget, Desc: "what devopsy sees (--debug --help)"},
	}},
	{"Servers and SSH", []Flag{
		{Name: "--ssh-keys-generate", Args: "[<name>]", Scope: Here, Desc: "an SSH key pair here, for CI"},
		{Name: "--ssh-keys", Args: "[list | add <file> | remove <key>]", Scope: Server, Desc: "who may log in as the deploy user"},
		{Name: "--ssh-config", Scope: Server, Desc: "a ~/.ssh/config block for the server"},
		{Name: "--upgrade", Args: "[v]", Scope: Server, Desc: "the server's devopsy, to latest or v"},
	}},
	{"Setup and tools", []Flag{
		{Name: "--init", Args: "[project]", Scope: Here, Desc: "a new .devopsy/config.yaml"},
		{Name: "--context-hash", Args: "[service]", Scope: Here, Desc: "a hash of an image's build inputs"},
		{Name: "--probe", Args: "<host>...", Scope: Here, Desc: "DNS, certificates, HTTPS, from here"},
		{Name: "--upgrade", Args: "[v]", Scope: Here, Desc: "this devopsy, to latest or v"},
		{Name: "--completion", Args: "<shell>", Scope: Here, Desc: "shell completion: bash, zsh or fish"},
		{Name: "--version", Scope: Here, Desc: "devopsy's, docker's and compose's versions"},
		{Name: "-v", Also: "-vv", Scope: Here, Desc: "first: what devopsy runs; -vv: scripts too"},
		{Name: "--help", Also: "-h", Scope: HereOrTarget, Desc: "this help; after a flag, its own"},
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
func LocalFlags() []Flag { return flagsWhere(Scope.Local) }

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

// ServerScope are the flags that need only a server: they take @<server>
// or @<server>: as well as any target (remote.ServerTarget).
var ServerScope = names(flagsWhere(func(s Scope) bool { return s == Server || s == HereOrServer }))

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
