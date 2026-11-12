// Package howto is the steps for what devopsy checks on a server and cannot set up itself,
// one topic each (devopsy --server-setup-howto [<topic>]): the text the binary prints, which
// docs/server-setup-howto.md shows too, and the command every fix of a server's setup offers
// for its steps (Command).
//
// Rules: internals/server-setup.md.
package howto

import (
	"strconv"
	"strings"
)

// Flag is the flag that prints a topic, or the list of them.
const Flag = "--server-setup-howto"

// Name is a topic's name: one of this package's constants, so a fix names a topic that
// exists.
type Name string

// Step is one step of a topic: what it does, in a sentence or two, and the commands that do
// it, as root, copyable as they are. A value the admin chooses is a shell variable set on the
// step's first line, with its usual value; a value of the system is computed.
type Step struct {
	Text string
	Cmds []string
}

// Topic is one thing devopsy checks on a server and cannot set up: its name, a line for the list
// and completion, a title, why it matters, its steps, and how to check them.
type Topic struct {
	Name    Name
	Summary string
	Title   string
	Why     string
	Steps   []Step
	Check   Step
}

// Find is the topic named name.
func Find(name string) (Topic, bool) {
	for _, t := range Topics {
		if string(t.Name) == name {
			return t, true
		}
	}
	return Topic{}, false
}

// Names are the topics' names, in order.
func Names() []string {
	var out []string
	for _, t := range Topics {
		out = append(out, string(t.Name))
	}
	return out
}

// Command is the command that prints topic name's steps, as a fix offers it: it holds no
// address, so it is right copied anywhere, on a server too.
func Command(name Name) string { return "devopsy " + Flag + " " + string(name) }

// width is where the text wraps.
const width = 76

// asRoot introduces every topic's steps, so a topic printed alone tells what the docs page's
// reader is told: where and as whom they run, who the deploy user is, where a value is set.
const asRoot = "Run each step as root on the server, Debian or Ubuntu with systemd. The deploy user is the Unix user devopsy logs in as, which must exist. A step's first line sets the values to change, if it has any (for example user=devopsy)."

// cmdIndent is how far a step's commands sit, under its text: indented, never after "$ ",
// which is how devopsy offers a command in what it says (report.Remedy).
const cmdIndent = "      "

// List is what the flag prints without a topic: each topic's name and summary, aligned.
func List() string {
	var b strings.Builder
	b.WriteString(wrap("What devopsy checks on a server and cannot set up itself, step by step, as root. Each topic prints with devopsy "+Flag+" followed by its name:", "", ""))
	b.WriteString("\n")
	longest := 0
	for _, t := range Topics {
		longest = max(longest, len(t.Name))
	}
	for _, t := range Topics {
		b.WriteString("  " + string(t.Name) + strings.Repeat(" ", longest-len(t.Name)+2) + t.Summary + "\n")
	}
	return b.String()
}

// Text is the topic as the flag prints it: its title, why, the steps numbered with their
// commands under them, then the check.
func (t Topic) Text() string {
	var b strings.Builder
	b.WriteString(t.Title + "\n\n")
	b.WriteString(wrap(t.Why, "", "") + "\n")
	b.WriteString(wrap(asRoot, "", "") + "\n")
	for i, s := range t.Steps {
		num := strconv.Itoa(i+1) + ". "
		b.WriteString(wrap(s.Text, num, strings.Repeat(" ", len(num))))
		writeCmds(&b, s.Cmds)
		b.WriteString("\n")
	}
	b.WriteString(wrap("To check it: "+t.Check.Text, "", ""))
	writeCmds(&b, t.Check.Cmds)
	return b.String()
}

// writeCmds writes cmds under a step's text, a blank line before them.
func writeCmds(b *strings.Builder, cmds []string) {
	if len(cmds) == 0 {
		return
	}
	b.WriteString("\n")
	for _, c := range cmds {
		b.WriteString(cmdIndent + c + "\n")
	}
}

// wrap breaks s into lines of width at most, at spaces, the first starting with first and the
// others with rest; a word longer than a line stays whole. Every line ends with a newline.
func wrap(s, first, rest string) string {
	var b strings.Builder
	line := first
	empty := true
	for _, w := range strings.Fields(s) {
		switch {
		case empty:
			line += w
			empty = false
		case len(line)+1+len(w) > width:
			b.WriteString(line + "\n")
			line = rest + w
		default:
			line += " " + w
		}
	}
	b.WriteString(line + "\n")
	return b.String()
}
