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

// stepIndent is how far a step's text sits after its number, and its commands' block with
// it, which Markdown then reads as part of the step. The text is Markdown that reads well as
// it is (a heading, paragraphs, a numbered list, fenced commands), so a renderer shows it
// too; a command is in a fenced block, never after "$ ", which is how devopsy offers one in
// what it says (report.Remedy).
const stepIndent = "   "

// fence opens and closes a block of commands.
const fence = "```"

// AllFlag is the argument that prints every topic.
const AllFlag = "--all"

// listIntro opens the list of topics.
const listIntro = "What devopsy checks on a server and cannot set up itself, step by step, as root. Each topic prints with devopsy " + Flag + " followed by its name, and all of them with " + AllFlag + ":"

// rows are the topics' names and summaries, aligned, each name between before and after.
func rows(before, after string) string {
	var b strings.Builder
	longest := 0
	for _, t := range Topics {
		longest = max(longest, len(t.Name))
	}
	for _, t := range Topics {
		b.WriteString(before + string(t.Name) + after + strings.Repeat(" ", longest-len(t.Name)+1) + t.Summary + "\n")
	}
	return b.String()
}

// List is what the flag prints without a topic: each topic's name and summary, aligned, a
// Markdown list.
func List() string { return wrap(listIntro, "", "") + "\n" + rows("- `", "`") }

// Help is the list as the flag's help shows it, laid out as help is: an entry starts with
// two spaces.
func Help() string { return wrap(listIntro, "", "") + "\n" + rows("  ", " ") }

// allIntro opens every topic printed together, which is the docs page.
const allIntro = "What devopsy checks on a server and does not set up itself, step by step, for the server's admin. `devopsy " + Flag + "` lists the topics, `devopsy " + Flag + " <topic>` prints one, and `devopsy " + Flag + " " + AllFlag + "` all of them, which is this page. Every fix devopsy offers for one of them names its command. It runs anywhere, needing no server, project or Docker."

// All is every topic under one title, with a table of contents: what the flag prints with
// AllFlag, and docs/server-setup-howto.md whole. Each topic is a section named as the topic
// is, so a link to the page's #<name> holds, its title on the section's first line.
func All() string {
	var b strings.Builder
	b.WriteString("# Server setup how-to\n\n" + wrap(allIntro, "", "") + "\n")
	for _, t := range Topics {
		b.WriteString("- [" + string(t.Name) + "](#" + string(t.Name) + "): " + t.Summary + "\n")
	}
	for _, t := range Topics {
		b.WriteString("\n## " + string(t.Name) + "\n\n" + t.Title + "\n\n" + t.Body())
	}
	return b.String()
}

// Text is the topic as the flag prints it: its title as a heading, then its Body.
func (t Topic) Text() string { return "# " + t.Title + "\n\n" + t.Body() }

// Body is the topic under its title, as the docs page shows it under the topic's own
// heading: why, the steps numbered with their commands under them, then the check.
func (t Topic) Body() string {
	var b strings.Builder
	b.WriteString(wrap(t.Why, "", "") + "\n")
	b.WriteString(wrap(asRoot, "", "") + "\n")
	for i, s := range t.Steps {
		b.WriteString(wrap(s.Text, strconv.Itoa(i+1)+". ", stepIndent))
		writeCmds(&b, s.Cmds, stepIndent)
		b.WriteString("\n")
	}
	b.WriteString(wrap("To check it: "+t.Check.Text, "", ""))
	writeCmds(&b, t.Check.Cmds, "")
	return b.String()
}

// writeCmds writes cmds under a step's text, in a fenced block indented as the text is, a
// blank line before it.
func writeCmds(b *strings.Builder, cmds []string, indent string) {
	if len(cmds) == 0 {
		return
	}
	b.WriteString("\n" + indent + fence + "sh\n")
	for _, c := range cmds {
		b.WriteString(indent + c + "\n")
	}
	b.WriteString(indent + fence + "\n")
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
