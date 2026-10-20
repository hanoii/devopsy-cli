package main

import (
	"os"
	"regexp"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"

	"golang.org/x/term"
)

// style colors output for a terminal, or with FORCE_COLOR, and does
// nothing otherwise or with NO_COLOR set (https://no-color.org).
type style struct{ on bool }

func styleFor(f *os.File) style {
	return style{on: os.Getenv("NO_COLOR") == "" && (term.IsTerminal(int(f.Fd())) || cli.ColorForced(os.LookupEnv))}
}

func (s style) wrap(code, text string) string {
	if !s.on || text == "" {
		return text
	}
	return code + text + "\033[0m"
}

func (s style) head(t string) string { return s.wrap("\033[1m", t) }
func (s style) name(t string) string { return s.wrap("\033[0;36m", t) }
func (s style) dim(t string) string  { return s.wrap("\033[2m", t) }
func (s style) ok(t string) string   { return s.wrap("\033[0;32m", t) }
func (s style) warn(t string) string { return s.wrap("\033[0;33m", t) }
func (s style) bad(t string) string  { return s.wrap("\033[0;31m", t) }

// scope colors an entry's scope: targets and servers apart.
func (s style) scope(t string) string {
	if strings.Contains(t, "@s") {
		return s.wrap("\033[0;33m", t)
	}
	return s.wrap("\033[0;35m", t)
}

// entry colors what you type, and a command's "(runs locally)" apart.
func (s style) entry(t string) string {
	if name, ok := strings.CutSuffix(t, " "+cli.RunsLocallyNote); ok {
		return s.name(name) + " " + s.ok(cli.RunsLocallyNote)
	}
	return s.name(t)
}

// scopePrefix is an entry's scope, before what you type.
var scopePrefix = regexp.MustCompile(`^(\[?@[ts]\]?)(?: (.*))?$`)

// helpEntry is an entry line of devopsy's help: two to seven spaces (an
// entry's scope is right aligned), then what you type (a flag, a target, a
// command), then two spaces or the line's end. Descriptions continued on
// the next line start further in.
var helpEntry = regexp.MustCompile(`^( {2,7})(\S(?:\S| \S)*)(  |$)`)

// help colors devopsy's help text: headings in bold, what you type in cyan.
func (s style) help(text string) string {
	if !s.on {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		switch {
		case l != "" && !strings.HasPrefix(l, " ") && (strings.HasSuffix(l, ":") || strings.HasPrefix(l, "Usage")):
			lines[i] = s.head(l)
		case helpEntry.MatchString(l):
			lines[i] = helpEntry.ReplaceAllStringFunc(l, func(m string) string {
				p := helpEntry.FindStringSubmatch(m)
				if q := scopePrefix.FindStringSubmatch(p[2]); q != nil {
					if q[2] == "" {
						// The legend.
						return p[1] + s.scope(q[1]) + p[3]
					}
					return p[1] + s.scope(q[1]) + " " + s.entry(q[2]) + p[3]
				}
				return p[1] + s.entry(p[2]) + p[3]
			})
		}
	}
	return strings.Join(lines, "\n")
}
