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
package report

import (
	"errors"
	"fmt"
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

// CmdMark starts a command to copy, on a Bar line.
const CmdMark = "$ "

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

// Remedy is one way to fix a failure: what to do and, optionally, the
// commands that do it.
type Remedy struct {
	Text string
	Cmds []string
}

// Fix is a Remedy: text says what to do (it may be empty when the
// commands say it all), cmds are shown after CmdMark, to copy.
func Fix(text string, cmds ...string) Remedy { return Remedy{Text: text, Cmds: cmds} }

// Lines formats msg at level l: msg's first line after Head(l), then
// msg's other lines, the details and the fixes, each on a Bar line
// (every string split at its newlines). One fix reads "to fix it:" and
// the fix; more read "to fix it, do one of these:" and each numbered.
func Lines(l Level, msg string, details []string, fixes []Remedy) []string {
	head, rest, _ := strings.Cut(strings.TrimRight(msg, "\n"), "\n")
	out := []string{Head(l) + head}
	add := func(s string) {
		for _, x := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
			out = append(out, strings.TrimRight(Bar+x, " "))
		}
	}
	if rest != "" {
		add(rest)
	}
	for _, d := range details {
		add(d)
	}
	switch len(fixes) {
	case 0:
	case 1:
		add("to fix it:")
		if fixes[0].Text != "" {
			add(fixes[0].Text)
		}
		for _, c := range fixes[0].Cmds {
			add(CmdMark + c)
		}
	default:
		add("to fix it, do one of these:")
		for i, f := range fixes {
			num := fmt.Sprintf("%d. ", i+1)
			indent := strings.Repeat(" ", len(num))
			cmds := f.Cmds
			if f.Text != "" {
				// A text of several lines (a label, a config example)
				// goes under its number.
				add(num + strings.ReplaceAll(strings.TrimRight(f.Text, "\n"), "\n", "\n"+indent))
			} else if len(cmds) > 0 {
				add(num + CmdMark + cmds[0])
				cmds = cmds[1:]
			}
			for _, c := range cmds {
				add(indent + CmdMark + c)
			}
		}
	}
	return out
}

// Failure is an error with details (why it happens) and fixes (what to
// do about it). Error() is the message alone, so wrapping keeps the usual
// "context: message" shape; Fail prints the rest under it.
type Failure struct {
	Msg     string
	Details []string
	Fixes   []Remedy
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

// Fixes are err's, when a Failure is in its chain.
func Fixes(err error) []Remedy {
	var e *Failure
	if errors.As(err, &e) {
		return e.Fixes
	}
	return nil
}
