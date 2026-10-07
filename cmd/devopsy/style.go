package main

import (
	"os"
	"regexp"
	"strings"

	"golang.org/x/term"
)

// style colors output for a terminal, and does nothing otherwise or with
// NO_COLOR set (https://no-color.org).
type style struct{ on bool }

func styleFor(f *os.File) style {
	return style{on: os.Getenv("NO_COLOR") == "" && term.IsTerminal(int(f.Fd()))}
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

// helpEntry is an entry line of devopsy's help: two spaces, then what you
// type (a flag, a target, a command), then two spaces or the line's end.
var helpEntry = regexp.MustCompile(`^(  )(\S(?:\S| \S)*)(  |$)`)

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
				return p[1] + s.name(p[2]) + p[3]
			})
		}
	}
	return strings.Join(lines, "\n")
}
