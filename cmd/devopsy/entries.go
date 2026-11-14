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
		e.Key, e.Group = report.Copyable(e.Key), report.Copyable(e.Group)
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
// sees. A command never wraps, since it is copied, nor a row of cells,
// whose last cell moves whole to a line of its own instead; a flat
// report's lines never do, each message whole on its line as the code
// writes it.
const reportWidth = 100

// entryTable prints entries on w in st, their keys in a column two
// spaces after the longest: one for every section of a report (each
// section's values start where the others' do), another for the entries
// with no section, sized on theirs and more.
type entryTable struct {
	w    io.Writer
	st   style
	col  int
	cols map[string]int
	// width is where a section's prose wraps, reportWidth.
	width int
}

// newEntryTable is a table whose key columns fit the keys of es, as
// they show (report.Copyable), more in the column of the entries with no
// section.
func newEntryTable(w io.Writer, st style, es []remote.StatusEntry, more ...string) entryTable {
	t := entryTable{w: w, st: st, cols: map[string]int{}, width: reportWidth}
	for _, k := range more {
		t.cols[""] = max(t.cols[""], utf8.RuneCountInString(k)+2)
	}
	sections := 0
	seen := map[string]bool{}
	for _, l := range layOut(es) {
		seen[l.section] = true
		if l.kind == lineEntry && l.key != "" {
			t.cols[l.section] = max(t.cols[l.section], l.indent+utf8.RuneCountInString(l.key)+2)
			if l.section != "" {
				sections = max(sections, t.cols[l.section])
			}
		}
	}
	// Every section, one with no key of its own too, starts its values at the one column.
	for sec := range seen {
		if sec != "" {
			t.cols[sec] = sections
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

// laidLine is one line of a report before its key column is known, its
// words as they show (report.Copyable).
type laidLine struct {
	kind    int
	section string
	indent  int
	key     string // shown on this line only
	mark    bool   // a warning's first line, "! " in its indent
	value   string
	cells   []string // a row of cells, as they show
	widths  []int    // a row of cells: its run's widths, of all but its last
	hang    int      // a remedy's text: where a part cut from it goes on
	level   string
	group   bool // the key is its project's name, the line its heading's
}

// layOut lays out es in the order print shows them: a section's heading
// on each change of section, a project's heading on each change of
// group when its first entry is not one of the heading's (Head), a key
// on the first line of each run of it, a warning's first line marked,
// a row of cells with its run's widths (cellWidths), each key, name,
// line and cell quoted as
// report.Copyable quotes it. Remedies a later entry of the same key
// shares are laid out once, after the last entry holding them
// (sharedLater): one command that fixes several lines is offered once.
func layOut(es []remote.StatusEntry) []laidLine {
	var out []laidLine
	widths := cellWidths(es)
	var prev remote.StatusEntry
	for i, e := range es {
		newSection := i == 0 || e.Section != prev.Section
		if newSection && e.Section != "" {
			out = append(out, laidLine{kind: lineSection, section: e.Section, value: e.Section})
		}
		newGroup := newSection || e.Group != prev.Group
		if e.Group != "" && newGroup && !e.Head {
			out = append(out, laidLine{kind: lineGroup, section: e.Section, indent: 2, key: report.Copyable(e.Group), group: true})
		}
		indent := 0
		switch {
		case e.Group != "" && !e.Head:
			indent = 4
		case e.Section != "":
			indent = 2
		}
		newKey := newGroup || e.Key != prev.Key || e.Name != prev.Name || e.Head != prev.Head
		key := report.Copyable(e.Key)
		if e.Head {
			key = report.Copyable(e.Group)
		}
		for j, l := range e.Lines {
			line := laidLine{kind: lineEntry, section: e.Section, indent: indent, value: report.Copyable(l), level: e.Level,
				mark: j == 0 && e.Level == remote.StatusWarning}
			if j == 0 && newKey {
				line.key, line.group = key, e.Head
			}
			if j == 0 && len(e.Cells) > 0 {
				for _, c := range e.Cells {
					line.cells = append(line.cells, report.Copyable(c))
				}
				line.widths = widths[i]
			}
			out = append(out, line)
		}
		if !sharedLater(es, i) {
			for _, r := range report.RemedyRows(e.Remedies) {
				out = append(out, laidLine{kind: lineRemedy, section: e.Section, indent: indent, value: report.Copyable(r.Text), hang: r.Hang, level: e.Level})
			}
		}
		prev = e
	}
	return out
}

// cellWidths are the widths of each entry's cells but its last, by its
// index, as they show: the widest of its run, every row of cells in a
// section's groups (so environments align across projects), else the
// rows under one key in a section, whatever lines without cells (a
// warning) sit between them.
func cellWidths(es []remote.StatusEntry) map[int][]int {
	runs := map[string][]int{}
	var order []string
	run := ""
	for i, e := range es {
		if i == 0 || e.Key != es[i-1].Key || e.Section != es[i-1].Section || e.Group != es[i-1].Group {
			run = fmt.Sprintf("run\x00%d", i)
		}
		if len(e.Cells) == 0 {
			continue
		}
		id := run
		if e.Group != "" {
			id = "section\x00" + e.Section
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
// each command bold; in a section, prose past the width wraps at a word
// to its column (wrapAt), and a row of cells past it moves its last
// cell whole to a line of its own at the column (cellRow).
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
		wrap := l.section != ""
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
			if l.group {
				// The project's name in its color, its facts as any line's.
				key = indent + t.st.name(l.key) + key[len(indent)+len(l.key):]
			}
			parts := wrapAt(l.value, col, t.width, 0, wrap)
			if len(l.cells) > 0 {
				room := -1
				if wrap {
					room = t.width - col
				}
				parts = cellRow(l.cells, l.widths, room)
			}
			for k, part := range parts {
				if k > 0 {
					key = strings.Repeat(" ", col)
				}
				if l.group && k == 0 {
					fmt.Fprintln(t.w, strings.TrimRight(key+paint(part), " "))
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
			for _, part := range wrapAt(l.value, col, t.width, l.hang, wrap) {
				fmt.Fprintln(t.w, paint(strings.TrimRight(strings.Repeat(" ", col)+part, " ")))
			}
		}
	}
}

// cellRow is a row of cells as its lines: each cell but the last that
// holds anything padded to its run's width (widths), a column empty in
// every row of its run left out, nothing after that last; with a room
// (-1 for none), a cell is padded only while the row still fits it, and
// a row whose own cells do not fit moves its last whole to a line of
// its own. A row is never cut at a word.
func cellRow(cells []string, widths []int, room int) []string {
	last := len(cells) - 1
	for last > 0 && cells[last] == "" {
		last--
	}
	shown := func(k int) bool { return k < len(widths) && widths[k] > 0 }
	// own is the width of cells from..to as they are, two spaces between.
	own := func(from, to int) int {
		n := 0
		for k := from; k <= to; k++ {
			if k == to || shown(k) {
				n += utf8.RuneCountInString(cells[k]) + 2
			}
		}
		return max(0, n-2)
	}
	move := room >= 0 && last > 0 && own(0, last) > room
	end := last
	if move {
		end = last - 1
	}
	var b strings.Builder
	pos := 0
	for k := 0; k < last; k++ {
		if !shown(k) {
			continue
		}
		cell := cells[k]
		rest := 0
		if k < end {
			rest = own(k+1, end) + 2
		}
		if room < 0 || pos+widths[k]+rest <= room {
			cell = pad(cell, widths[k])
		}
		b.WriteString(cell + "  ")
		pos += utf8.RuneCountInString(cell) + 2
	}
	if move {
		return []string{strings.TrimRight(b.String(), " "), cells[last]}
	}
	return []string{b.String() + cells[last]}
}

// wrapAt is line cut at words into parts that fit width from column col
// on when wrap, whole otherwise, when it fits or too little room is left
// to wrap into (under 40 columns): the first part keeps the line's
// leading spaces, the others start at hang or at them, whichever is
// further; a word longer than the room stays whole.
func wrapAt(line string, col, width, hang int, wrap bool) []string {
	body := strings.TrimLeft(line, " ")
	lead := len(line) - len(body)
	hang = max(hang, lead)
	room := width - col - hang
	if !wrap || room < 40 || col+utf8.RuneCountInString(line) <= width {
		return []string{line}
	}
	var parts []string
	cur := ""
	for _, w := range strings.Split(body, " ") {
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
	parts = append(parts, cur)
	for i := range parts {
		n := hang
		if i == 0 {
			n = lead
		}
		parts[i] = strings.Repeat(" ", n) + parts[i]
	}
	return parts
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
