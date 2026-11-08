// Package report is how devopsy talks: every line it says, on stderr,
// starts with "devopsy: ", so it stands apart from what Compose, Docker
// and a project's steps print around it, and says its level after it:
//
//	devopsy: releasing 20261019214911 to vm1:prod
//	devopsy: warning: vm1 keeps traefik/.env
//	devopsy: error: dev's parent, prod, has no server
//	devopsy: | the parent resolves from DEVOPSY_SERVER_PROD or DEVOPSY_SERVER
//	devopsy: | to fix it:
//	devopsy: | $ devopsy @dev --sync --from @vm1:prod
//
// Info has no tag; details and fixes follow on "| " lines, commands to
// copy after "$ ". Each line says what it is by itself, so coloring
// (Paint, Colorize), logs and grep never need the line before. Callers
// give the words (lowercase, no final period); this package adds
// everything else: the prefix, the tag, the bars, the numbering, the
// color and the verbosity filter. Results (help, lists, --env, data) go
// to stdout and never through here.
//
// Rules: the Output conventions in AGENTS.md.
package report

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// Level is how much a line matters: what Paint colors it, and whether
// the verbosity shows it.
type Level int

const (
	// TraceLevel lines show with -vv: scripts devopsy runs.
	TraceLevel Level = iota
	// DebugLevel lines show with -v: what devopsy runs.
	DebugLevel
	// InfoLevel is progress, and what a command did.
	InfoLevel
	// WarnLevel: the command goes on, something needs attention.
	WarnLevel
	// ErrorLevel: the command fails. The last error is its exit code's
	// reason; whatever goes on is a warning.
	ErrorLevel
)

// Prefix starts every line devopsy says.
const Prefix = "devopsy: "

// Bar starts a line that continues the one before: details and fixes.
const Bar = Prefix + "| "

// cmdMark starts a command to copy: only this package writes it
// (TestNothingSaysAround), so every command is laid out one way.
const cmdMark = "$ "

// SplitCmd splits a line of RemedyLines that is a command into what
// leads it (a number, an indent) and the command shown after "$ ", for
// painting it apart; ok false for any other line.
func SplitCmd(line string) (lead, cmd string, ok bool) {
	cmd = strings.TrimLeft(line, " 0123456789.")
	if !strings.HasPrefix(cmd, cmdMark) {
		return "", "", false
	}
	return line[:len(line)-len(cmd)], cmd, true
}

var tags = map[Level]string{TraceLevel: "trace", DebugLevel: "debug", WarnLevel: "warning", ErrorLevel: "error"}

// Tag is the level's word after the prefix: "" for info.
func (l Level) Tag() string { return tags[l] }

// Head is what a line of level l starts with: "devopsy: " for info,
// "devopsy: <tag>: " otherwise.
func Head(l Level) string {
	if t := l.Tag(); t != "" {
		return Prefix + t + ": "
	}
	return Prefix
}

// Line is msg as one line of level l. A newline in msg is the caller's
// mistake; Lines splits them.
func Line(l Level, msg string) string { return Head(l) + msg }

// Remedy is what a line offers to do: a fix, a way out of what is wrong
// (Fix), or a hint, a command worth knowing (Hint); text says what it
// does, the commands do it.
type Remedy struct {
	Text string   `json:"text,omitempty"`
	Cmds []string `json:"commands,omitempty"`
	// Hint is a hint's: shown in order, never under "to fix it:".
	Hint bool `json:"hint,omitempty"`
}

// Fix is a way out of what a line says is wrong: text says what to do
// (it may be empty when the commands say it all), cmds are shown after
// "$ ", to copy. A line's fixes are alternatives (RemedyLines).
func Fix(text string, cmds ...string) Remedy { return Remedy{Text: text, Cmds: cmds} }

// Hint is a command worth knowing, said after a line's details: text
// (perhaps empty), then cmds after "$ ". A line's hints are steps, in
// order (RemedyLines).
func Hint(text string, cmds ...string) Remedy { return Remedy{Text: text, Cmds: cmds, Hint: true} }

// RemedyLines lays out rs, without a bar (a string of several lines
// stays one), an empty one skipped: each hint's text then its commands,
// in order; then the fixes, one under "to fix it:", more under "to fix
// it, do one of these:", each numbered, a text of several lines under
// its number.
func RemedyLines(rs []Remedy) []string {
	var out []string
	var fixes []Remedy
	for _, r := range rs {
		if r.Text == "" && len(r.Cmds) == 0 {
			continue // nothing to say (a bare --fix)
		}
		if !r.Hint {
			fixes = append(fixes, r)
			continue
		}
		if r.Text != "" {
			out = append(out, r.Text)
		}
		for _, c := range r.Cmds {
			out = append(out, cmdMark+c)
		}
	}
	switch len(fixes) {
	case 0:
	case 1:
		out = append(out, "to fix it:")
		if fixes[0].Text != "" {
			out = append(out, fixes[0].Text)
		}
		for _, c := range fixes[0].Cmds {
			out = append(out, cmdMark+c)
		}
	default:
		out = append(out, "to fix it, do one of these:")
		for i, f := range fixes {
			num := fmt.Sprintf("%d. ", i+1)
			indent := strings.Repeat(" ", len(num))
			cmds := f.Cmds
			if f.Text != "" {
				out = append(out, num+strings.ReplaceAll(strings.TrimRight(f.Text, "\n"), "\n", "\n"+indent))
			} else if len(cmds) > 0 {
				out = append(out, num+cmdMark+cmds[0])
				cmds = cmds[1:]
			}
			for _, c := range cmds {
				out = append(out, indent+cmdMark+c)
			}
		}
	}
	return out
}

// Lines formats msg at level l: msg's first line after Head(l), then
// msg's other lines, the details and the remedies (RemedyLines), each on
// a Bar line (every string split at its newlines).
func Lines(l Level, msg string, details []string, remedies []Remedy) []string {
	head, rest, _ := strings.Cut(strings.TrimRight(msg, "\n"), "\n")
	out := []string{Head(l) + Copyable(head)}
	add := func(s string) {
		for _, x := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
			out = append(out, strings.TrimRight(Bar+Copyable(x), " "))
		}
	}
	if rest != "" {
		add(rest)
	}
	for _, d := range details {
		add(d)
	}
	for _, r := range RemedyLines(remedies) {
		add(r)
	}
	return out
}

// Copyable quotes each address in line holding an IPv6 address in
// brackets (@'[2001:db8::1]:22:prod', a server with a port), which zsh
// would take for a glob and refuse, so a command devopsy names copies as
// shown, wherever it is in the line. An address already in quotes (a
// word holding one, or between words opening and closing them) stays as
// it is, and so do the punctuation after it and brackets of another kind
// (a placeholder, @[<server>:]<env>).
func Copyable(line string) string {
	words := strings.Split(line, " ")
	quoted := false
	for i, w := range words {
		if !quoted && strings.HasPrefix(w, "@") && bracketsIPv6(w) && !strings.ContainsAny(w, `'"`) {
			addr := strings.TrimRight(w, ",;.)")
			words[i] = "@'" + addr[1:] + "'" + w[len(addr):]
		}
		if strings.HasPrefix(strings.TrimLeft(w, "("), "'") {
			quoted = true
		}
		if strings.HasSuffix(strings.TrimRight(w, ",;.)"), "'") {
			quoted = false
		}
	}
	return strings.Join(words, " ")
}

func bracketsIPv6(w string) bool {
	_, after, ok := strings.Cut(w, "[")
	inner, _, closed := strings.Cut(after, "]")
	a, err := netip.ParseAddr(inner)
	return ok && closed && err == nil && a.Is6()
}

// Failure is an error with details (why it happens) and remedies (what
// to do about it: fixes and hints). Error() is the message alone, so
// wrapping keeps the usual "context: message" shape; Fail prints the
// rest under it. Also are failures of the same kind found with it (each
// data service without a label), which Fail says after it, each with its
// own message, in order.
type Failure struct {
	Msg      string
	Details  []string
	Remedies []Remedy
	Also     []*Failure
}

func (e *Failure) Error() string { return e.Msg }

// Errorf is a Failure with a formatted message and details.
func Errorf(details []string, format string, args ...any) *Failure {
	return &Failure{Msg: fmt.Sprintf(format, args...), Details: details}
}

// Details are err's, when a Failure is in its chain.
func Details(err error) []string {
	var e *Failure
	if errors.As(err, &e) {
		return e.Details
	}
	return nil
}

// Also are the failures err carries after its own (Failure.Also), when a
// Failure is in its chain.
func Also(err error) []*Failure {
	var e *Failure
	if errors.As(err, &e) {
		return e.Also
	}
	return nil
}

// Messages are err's message and each of its Also's (Failure.Also), for
// a warning that names them all on its detail lines.
func Messages(err error) []string {
	ms := []string{err.Error()}
	for _, f := range Also(err) {
		ms = append(ms, f.Msg)
	}
	return ms
}

// Remedies are err's, when a Failure is in its chain.
func Remedies(err error) []Remedy {
	var e *Failure
	if errors.As(err, &e) {
		return e.Remedies
	}
	return nil
}
