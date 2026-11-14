package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// reportArgs reads the arguments of a report flag (--status,
// --server-info): whether --json, and code, the exit code when that is
// all (its help, a usage error), else -1.
func reportArgs(flag string, args []string, usage string) (asJSON bool, code int) {
	for _, a := range args {
		switch a {
		case "--help", "-h":
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp[flag]))
			return false, 0
		case "--json":
			asJSON = true
		default:
			return false, fail(usage)
		}
	}
	return asJSON, -1
}

// entriesWarn says whether any of es is a warning.
func entriesWarn(es []remote.StatusEntry) bool {
	return slices.ContainsFunc(es, func(e remote.StatusEntry) bool { return e.Level == remote.StatusWarning })
}

// copyableEntries are es with their lines, fixes and commands copyable
// as the text's are (report.Copyable: a bracketed address quoted), for
// JSON; never nil, so JSON says [].
func copyableEntries(es []remote.StatusEntry) []remote.StatusEntry {
	out := make([]remote.StatusEntry, 0, len(es))
	for _, e := range es {
		e.Lines = slices.Clone(e.Lines)
		for j := range e.Lines {
			e.Lines[j] = report.Copyable(e.Lines[j])
		}
		e.Cells = slices.Clone(e.Cells)
		for j := range e.Cells {
			e.Cells[j] = report.Copyable(e.Cells[j])
		}
		e.Remedies = slices.Clone(e.Remedies)
		for j := range e.Remedies {
			f := &e.Remedies[j]
			f.Text = report.Copyable(f.Text)
			f.Cmds = slices.Clone(f.Cmds)
			for k := range f.Cmds {
				f.Cmds[k] = report.Copyable(f.Cmds[k])
			}
		}
		out = append(out, e)
	}
	return out
}

// printJSON prints v indented on w.
func printJSON(w io.Writer, v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(out))
	return err
}

// reportWidth is how wide a report laid out in sections runs before its
// prose wraps, at a word, to its value column: the same for a pipe, CI
// and a terminal of any width, so what a test sees is what a person
// sees. A command never wraps, since it is copied; a flat report's
// lines never do, each message whole on its line as the code writes it.
const reportWidth = 100

// entryTable prints entries on w in st, their keys in a column: one
// column per section, two spaces after its longest key, the key "" of
// an entry with no section sized on es and more.
type entryTable struct {
	w    io.Writer
	st   style
	col  int
	cols map[string]int
}

// newEntryTable is a table whose key columns fit the keys of es, more
// in the column of the entries with no section.
func newEntryTable(w io.Writer, st style, es []remote.StatusEntry, more ...string) entryTable {
	t := entryTable{w: w, st: st, cols: map[string]int{}}
	for _, k := range more {
		t.cols[""] = max(t.cols[""], utf8.RuneCountInString(k)+2)
	}
	for _, l := range layOut(es) {
		if l.kind == lineEntry && l.key != "" {
			t.cols[l.section] = max(t.cols[l.section], l.indent+utf8.RuneCountInString(l.key)+2)
		}
	}
	t.col = t.cols[""]
	return t
}

// row is value under key, padded to the column of the entries with no
// section, before it is painted.
func (t entryTable) row(key, value string) string {
	return strings.TrimRight(pad(key, t.col)+value, " ")
}

// pad is s with spaces after it to n runes.
func pad(s string, n int) string {
	return s + strings.Repeat(" ", max(0, n-utf8.RuneCountInString(s)))
}

// The kinds of a laid out line: a section's heading, a project's
// heading of its own, an entry's line, a remedy's line.
const (
	lineSection = iota
	lineGroup
	lineEntry
	lineRemedy
)

// laidLine is one line of a report before its key column is known.
type laidLine struct {
	kind    int
	section string
	indent  int
	key     string // shown on this line only
	mark    bool   // a warning's first line, "! " in its indent
	value   string
	level   string
	group   bool // the key is its project's name
}

// layOut lays out es in the order print shows them: a section's heading
// on each change of section, a project's heading on each change of
// group when its first entry is not the heading itself, a key on the
// first line of each run of it, a warning's first line marked, the
// cells aligned with their run's (cellWidths), each line quoted as
// report.Copyable quotes it. Remedies a later entry of the same key
// shares are laid out once, after the last entry holding them
// (sharedLater): one command that fixes several lines is offered once.
func layOut(es []remote.StatusEntry) []laidLine {
	var out []laidLine
	widths := cellWidths(es)
	prev := remote.StatusEntry{}
	for i, e := range es {
		if i == 0 || e.Section != prev.Section {
			if e.Section != "" {
				out = append(out, laidLine{kind: lineSection, section: e.Section, value: e.Section})
			}
			prev = remote.StatusEntry{Section: e.Section}
		}
		if e.Group != "" && e.Group != prev.Group && e.Key != e.Group {
			out = append(out, laidLine{kind: lineGroup, section: e.Section, indent: 2, key: e.Group, group: true})
		}
		indent := 0
		switch {
		case e.Group != "" && e.Key != e.Group:
			indent = 4
		case e.Section != "":
			indent = 2
		}
		newKey := i == 0 || e.Key != prev.Key || e.Group != prev.Group || e.Section != prev.Section
		lines := slices.Clone(e.Lines)
		if len(e.Cells) > 0 && len(lines) > 0 {
			cells, last := e.Cells[:len(e.Cells)-1], e.Cells[len(e.Cells)-1]
			w := widths[i]
			var b strings.Builder
			for j, c := range cells {
				b.WriteString(pad(report.Copyable(c), w[j]) + "  ")
			}
			lines[0] = b.String() + report.Copyable(last)
		}
		for j, l := range lines {
			if j > 0 || len(e.Cells) == 0 {
				l = report.Copyable(l)
			}
			key := ""
			if j == 0 {
				if newKey {
					key = e.Key
				}
			}
			out = append(out, laidLine{kind: lineEntry, section: e.Section, indent: indent, key: key, value: l, level: e.Level,
				mark: j == 0 && e.Level == remote.StatusWarning, group: j == 0 && e.Group != "" && e.Key == e.Group && newKey})
		}
		if !sharedLater(es, i) {
			for _, r := range report.RemedyLines(e.Remedies) {
				for _, l := range strings.Split(r, "\n") {
					out = append(out, laidLine{kind: lineRemedy, section: e.Section, indent: indent, value: report.Copyable(l), level: e.Level})
				}
			}
		}
		prev = e
	}
	return out
}

// cellWidths are the widths of each entry's cells but its last, by its
// index: the widest of its run, every row of cells in a section's
// groups (so environments align across projects), else the entries in
// a row under one key.
func cellWidths(es []remote.StatusEntry) map[int][]int {
	runs := map[string][]int{}
	var order []string
	run := ""
	for i, e := range es {
		if len(e.Cells) == 0 {
			continue
		}
		id := "section\x00" + e.Section
		if e.Group == "" {
			if i == 0 || es[i-1].Key != e.Key || es[i-1].Section != e.Section || len(es[i-1].Cells) == 0 {
				run = fmt.Sprintf("run\x00%d", i)
			}
			id = run
		}
		if runs[id] == nil {
			order = append(order, id)
		}
		runs[id] = append(runs[id], i)
	}
	widths := map[int][]int{}
	for _, id := range order {
		var w []int
		for _, i := range runs[id] {
			for j, c := range es[i].Cells[:len(es[i].Cells)-1] {
				if j == len(w) {
					w = append(w, 0)
				}
				w[j] = max(w[j], utf8.RuneCountInString(report.Copyable(c)))
			}
		}
		for _, i := range runs[id] {
			widths[i] = w
		}
	}
	return widths
}

// print prints es as layOut lays them out: a section's heading bold
// after a blank line, a project's name in its color, a warning's lines
// and texts yellow, its first line with "! " in the last two columns of
// its indent (a warning shows in a pipe and greps; a report with no
// sections has no indent to hold it, and a key column widened for it
// would move every line whenever a warning shows), a highlight's bold,
// each command bold; in a section, prose past reportWidth wraps at a
// word to its column (wrapAt).
func (t entryTable) print(es []remote.StatusEntry) {
	for _, l := range layOut(es) {
		col := t.cols[l.section]
		paint := func(s string) string { return s }
		switch l.level {
		case remote.StatusWarning:
			paint = t.st.warn
		case remote.StatusHighlight:
			paint = t.st.head
		}
		indent := strings.Repeat(" ", l.indent)
		switch l.kind {
		case lineSection:
			fmt.Fprintln(t.w)
			fmt.Fprintln(t.w, t.st.head(l.value))
		case lineGroup:
			fmt.Fprintln(t.w, indent+t.st.name(l.key))
		case lineEntry:
			if l.mark && l.indent >= 2 {
				indent = strings.Repeat(" ", l.indent-2) + "! "
			}
			key := pad(indent+l.key, col)
			for k, part := range wrapAt(l.value, col, l.section != "") {
				if k > 0 {
					key = strings.Repeat(" ", col)
				}
				if l.group && k == 0 {
					// The project's name in its color, its facts as any line's.
					fmt.Fprintln(t.w, strings.TrimRight(indent+t.st.name(l.key)+strings.Repeat(" ", col-l.indent-utf8.RuneCountInString(l.key))+paint(part), " "))
					continue
				}
				fmt.Fprintln(t.w, paint(strings.TrimRight(key+part, " ")))
			}
		case lineRemedy:
			// Quoted before it is painted (layOut): the quotes are the
			// command's, inside its bold.
			if lead, cmd, ok := report.SplitCmd(l.value); ok {
				fmt.Fprintln(t.w, strings.Repeat(" ", col)+lead+t.st.head(cmd))
				continue
			}
			lead := len(l.value) - len(strings.TrimLeft(l.value, " 0123456789."))
			for k, part := range wrapAt(l.value, col+lead, l.section != "") {
				if k > 0 {
					part = strings.Repeat(" ", lead) + part
				}
				fmt.Fprintln(t.w, paint(strings.TrimRight(strings.Repeat(" ", col)+part, " ")))
			}
		}
	}
}

// wrapAt is line cut at words into parts that fit reportWidth from
// column col on when wrap, whole otherwise, when it fits or too little
// room is left to wrap into (under 40 columns): a word longer than the
// room stays whole.
func wrapAt(line string, col int, wrap bool) []string {
	room := reportWidth - col
	if !wrap || room < 40 || utf8.RuneCountInString(line) <= room {
		return []string{line}
	}
	var parts []string
	cur := ""
	for _, w := range strings.Split(line, " ") {
		switch {
		case cur == "":
			cur = w
		case utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(w) <= room:
			cur += " " + w
		default:
			parts = append(parts, cur)
			cur = w
		}
	}
	return append(parts, cur)
}

// sharedLater says whether an entry after es[i], under the same key in
// the same section and group with no other key between, holds the same
// remedies as es[i].
func sharedLater(es []remote.StatusEntry, i int) bool {
	if len(es[i].Remedies) == 0 {
		return false
	}
	lines := report.RemedyLines(es[i].Remedies)
	for _, e := range es[i+1:] {
		if e.Key != es[i].Key || e.Group != es[i].Group || e.Section != es[i].Section {
			return false
		}
		if slices.Equal(report.RemedyLines(e.Remedies), lines) {
			return true
		}
	}
	return false
}
