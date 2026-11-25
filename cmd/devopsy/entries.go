package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/layout"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// reportMode is how a report prints: as text, as JSON, or as text in
// boxes.
type reportMode int

const (
	textReport reportMode = iota
	jsonReport
	boxedReport
)

// reportArgs reads the arguments of a report flag (--status,
// --server-info): how it prints (--json, --borders, never both), and
// code, the exit code when that is all (its help, a usage error), else -1.
func reportArgs(flag string, args []string, usage string) (mode reportMode, code int) {
	for _, a := range args {
		switch a {
		case "--help", "-h":
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp[flag]))
			return textReport, 0
		case "--json", "--borders":
			m := map[string]reportMode{"--json": jsonReport, "--borders": boxedReport}[a]
			if mode != textReport && mode != m {
				report.Say(report.ErrorLevel, flag+" takes --json or --borders, never both", []string{usage})
				return textReport, 1
			}
			mode = m
		default:
			return textReport, fail(usage)
		}
	}
	return mode, -1
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
		e.Key, e.Group, e.Instance = report.Copyable(e.Key), report.Copyable(e.Group), report.Copyable(e.Instance)
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
// with no section.
type entryTable struct {
	w  io.Writer
	st style
	// sections and flat are the key and value columns of the entries in
	// a section and of those in none.
	sections, flat *layout.Widths
	// width is where a section's prose wraps, reportWidth.
	width int
	// borders draws each section, or a flat report, in a box: a rule
	// between its keys and before each project, where blank lines go
	// without.
	borders bool
}

// newEntryTable is a table printing on w in st.
func newEntryTable(w io.Writer, st style) entryTable {
	return entryTable{w: w, st: st, sections: layout.NewWidths(), flat: layout.NewWidths(), width: reportWidth}
}

// row is value under key in the column of the entries with no section.
func (t entryTable) row(key, value string) layout.Row {
	return layout.Row{Widths: t.flat, Cells: layout.Cells(key, value)}
}

// print prints es as one grid (rows), a blank line first when it starts
// with a section's title.
func (t entryTable) print(es []remote.StatusEntry) {
	rows, _ := t.rows(es)
	t.write(rows)
}

// write prints rows as one grid at the table's width, a blank line first
// when the first is a section's title: every row of a report in one grid,
// so its columns are measured together.
func (t entryTable) write(rows []layout.Row) {
	if len(rows) == 0 {
		return
	}
	g := &layout.Grid{Rows: rows, Width: t.width, MinRoom: minWrapRoom}
	if t.borders {
		g = t.boxed(rows)
	}
	if rows[0].Break && rows[0].Underline {
		fmt.Fprintln(t.w)
	}
	fmt.Fprint(t.w, g.String())
}

// boxed is rows with each section's in a box under its title (bold, no
// underline, the box right under it), a flat report's after its heading in
// one, the boxes sharing their columns.
func (t entryTable) boxed(rows []layout.Row) *layout.Grid {
	root := &layout.Grid{Width: t.width, MinRoom: minWrapRoom}
	var box *layout.Grid
	for i, r := range rows {
		switch {
		case r.Underline:
			r.Underline = false
			root.Rows = append(root.Rows, r)
			box = nil
		case i == 0 && layout.SpanRow(r):
			// The report's own heading.
			root.Rows = append(root.Rows, r)
		default:
			if box == nil {
				box = &layout.Grid{Border: layout.Box, Width: t.width, MinRoom: minWrapRoom}
				root.Rows = append(root.Rows, layout.Row{Cells: []layout.Cell{{Grid: box}}})
			}
			// A project's heading runs across the box, its name widening no
			// column.
			box.Rows = append(box.Rows, r)
		}
	}
	return root
}

// sameRun says whether a and b are one run of a report: the entries
// under one key that layOut shows as one, in one section, group and
// instance.
func sameRun(a, b remote.StatusEntry) bool {
	return a.Section == b.Section && a.Group == b.Group && a.Instance == b.Instance && a.Key == b.Key && a.Name == b.Name && a.Head == b.Head
}

// blocks says of each of es whether it is a block, a message with lines
// of its own (a warning, or a note: an entry with remedies that does not
// start its run), and whether a block is bound to what is above it: a
// note always, a warning when the nearest entry before it in its run that
// is no block has cells (a disk's, an environment's row). Decided on the
// order a report builds its entries, never after ordered moves them: a
// moved warning sits under a row it is not about.
func blocks(es []remote.StatusEntry) (block, bound []bool) {
	block, bound = make([]bool, len(es)), make([]bool, len(es))
	for i, e := range es {
		starts := i == 0 || !sameRun(es[i-1], e)
		switch {
		case e.Level == remote.StatusWarning:
			block[i] = true
			j := i - 1
			for j >= 0 && sameRun(es[j], e) && block[j] {
				j--
			}
			bound[i] = j >= 0 && sameRun(es[j], e) && len(es[j].Cells) > 0
		case len(e.Remedies) > 0 && !starts:
			block[i], bound[i] = true, true
		}
	}
	return block, bound
}

// ordered is es with each run's warnings that are bound to no row after
// its facts, in their order, with each entry's block and bound (blocks)
// moved along: a warning reads after what it is about, the facts of its
// key together above it. Notes stay under their line, and a warning under
// a row of cells under its row.
func ordered(es []remote.StatusEntry) (out []remote.StatusEntry, block, bound []bool) {
	isBlock, isBound := blocks(es)
	for i := 0; i < len(es); {
		j := i
		for j < len(es) && sameRun(es[i], es[j]) {
			j++
		}
		var moved []int
		for k := i; k < j; k++ {
			if es[k].Level == remote.StatusWarning && !isBound[k] {
				moved = append(moved, k)
				continue
			}
			out, block, bound = append(out, es[k]), append(block, isBlock[k]), append(bound, isBound[k])
		}
		for _, k := range moved {
			out, block, bound = append(out, es[k]), append(block, isBlock[k]), append(bound, isBound[k])
		}
		i = j
	}
	return out, block, bound
}

// minWrapRoom is the least room prose wraps into: with less left after
// its column, it stays whole.
const minWrapRoom = 40

// orderWarnings is es as a report prints them (ordered), for --json.
func orderWarnings(es []remote.StatusEntry) []remote.StatusEntry {
	out, _, _ := ordered(es)
	return out
}

// The kinds of a laid out line: a section's heading, a project's
// heading of its own, an entry's line, a line after a bar (a warning's
// detail, a remedy's line).
const (
	lineSection = iota
	lineGroup
	lineEntry
	lineBar
)

// laidLine is one line of a report before its key column is known, its
// words as they show (report.Copyable).
type laidLine struct {
	kind    int
	entry   int // the entry it is of, -1 for a section's or a group's heading
	section string
	indent  int
	key     string // shown on this line only
	first   bool   // its entry's first line: a warning's message, the rest its details
	value   string
	cells   []string // a row of cells, as they show
	run     string   // a row of cells: its run, whose rows share their widths
	hang    int      // a line after a bar: where a part cut from its text goes on
	level   string
	group   bool // the key is its project's name, the line its heading's
}

// layOut lays out es in the order print shows them: a section's heading
// on each change of section, a project's heading on each change of
// group when its first entry is not one of the heading's (Head), an
// instance's the same way under it, a key
// on the first line of each run of it, a warning's first line in a
// section starting with its level's mark (report.WarnLevel.Mark),
// a row of cells with its run (cellRuns), each key, name,
// line and cell quoted as
// report.Copyable quotes it. Remedies a later entry of the same key
// shares are laid out once, after the last entry holding them
// (sharedLater): one command that fixes several lines is offered once.
func layOut(es []remote.StatusEntry) []laidLine {
	var out []laidLine
	runs := cellRuns(es)
	var prev remote.StatusEntry
	for i, e := range es {
		newSection := i == 0 || e.Section != prev.Section
		if newSection && e.Section != "" {
			out = append(out, laidLine{kind: lineSection, entry: -1, section: e.Section, value: e.Section})
		}
		newGroup := newSection || e.Group != prev.Group
		if e.Group != "" && newGroup && (!e.Head || e.Instance != "") {
			out = append(out, laidLine{kind: lineGroup, entry: -1, section: e.Section, indent: 2, key: report.Copyable(e.Group), group: true})
		}
		newInstance := newGroup || e.Instance != prev.Instance
		if e.Instance != "" && newInstance && !e.Head {
			out = append(out, laidLine{kind: lineGroup, entry: -1, section: e.Section, indent: 4, key: report.Copyable(e.Instance), group: true})
		}
		indent := 0
		switch {
		case e.Instance != "" && e.Head:
			indent = 4
		case e.Instance != "":
			indent = 6
		case e.Group != "" && !e.Head:
			indent = 4
		case e.Section != "":
			indent = 2
		}
		newKey := newInstance || !sameRun(prev, e)
		key := report.Copyable(e.Key)
		if e.Head {
			key = report.Copyable(cmp.Or(e.Instance, e.Group))
		}
		// The entry as report says a message (report.Rows): a warning's first line its
		// message, tagged, and its other lines its details; any entry's lines its values,
		// and its remedies, unless a later entry shares them, after a bar.
		level, values, details := report.InfoLevel, e.Lines, []string(nil)
		if e.Level == remote.StatusWarning && len(e.Lines) > 0 {
			level, values, details = report.WarnLevel, e.Lines[:1], e.Lines[1:]
		}
		var remedies []report.Remedy
		if !sharedLater(es, i) {
			remedies = e.Remedies
		}
		rows := report.Rows(level, strings.Join(values[:min(len(values), 1)], ""), details, remedies)
		for j, l := range values {
			line := laidLine{kind: lineEntry, entry: i, section: e.Section, indent: indent, value: report.Copyable(l), level: e.Level, first: j == 0}
			if j == 0 {
				line.value = rows[0].Text
			}
			if j == 0 && newKey {
				line.key, line.group = key, e.Head
			}
			if j == 0 && len(e.Cells) > 0 {
				for _, c := range e.Cells {
					line.cells = append(line.cells, report.Copyable(c))
				}
				line.run = runs[i]
			}
			out = append(out, line)
		}
		for _, r := range rows[1:] {
			out = append(out, laidLine{kind: lineBar, entry: i, section: e.Section, indent: indent, value: r.Text, hang: r.Hang, level: e.Level})
		}
		prev = e
	}
	return out
}

// cellRuns are the runs of each entry with cells, by its index: every
// row of cells in a section's groups (so environments align across
// projects), else the rows under one key in a section, whatever lines
// without cells (a warning) sit between them. A run's rows share their
// columns (each but its last cell measured), and a column empty in all
// of them is left out. Wider than sameRun's: rows a Name tells apart (a
// gone environment's, keyed gone) align with the rest of their key.
func cellRuns(es []remote.StatusEntry) map[int]string {
	runs := map[int]string{}
	run := ""
	for i, e := range es {
		if i == 0 || e.Key != es[i-1].Key || e.Section != es[i-1].Section || e.Group != es[i-1].Group || e.Instance != es[i-1].Instance {
			run = fmt.Sprintf("run\x00%d", i)
		}
		if len(e.Cells) == 0 {
			continue
		}
		runs[i] = run
		if e.Group != "" {
			runs[i] = "section\x00" + e.Section
		}
	}
	return runs
}

// rows are es as layOut lays them out, their warnings ordered
// (orderWarnings): a section's title bold and underlined, a blank line
// before and after it, a project's name in its color, a warning's message and the
// lines after a bar colored as devopsy's own lines are (report.Colored,
// report.PaintBar: the message yellow, the bar dim, its text plain, a
// command bold), a highlight's lines bold; in a section, prose past the
// width wraps at a word to its column, a line after a bar each part after
// one, and a row of cells past it moves its last cell whole to a line of
// its own at the column. A command never wraps, nor anything in a flat
// report.
//
// Blank lines, the same at any width: before and after a block that is
// bound to nothing (blocks), after one that is, a run of blocks that
// stack together as one; before and after a run of
// more than one line outside a group; before a project; in a group, after
// an entry's bar lines. pending says the row after the last one breaks.
func (t entryTable) rows(es []remote.StatusEntry) (out []layout.Row, pending bool) {
	es, block, bound := ordered(es)
	laid := layOut(es)
	// Each entry's run, by its first entry, and the lines each run shows.
	start := make([]int, len(es))
	for i := range es {
		if i > 0 && sameRun(es[i-1], es[i]) {
			start[i] = start[i-1]
		} else {
			start[i] = i
		}
	}
	shown := map[int]int{}
	for _, l := range laid {
		if l.entry >= 0 {
			shown[start[l.entry]]++
		}
	}
	long := func(i int) bool { return shown[start[i]] > 1 && es[start[i]].Group == "" }
	// stacks says es[i] goes on under the block before it with no blank
	// line: a bound block under its row's, or a warning under a warning
	// bound to nothing (a run of them together, as --server-setup's).
	stacks := func(i int) bool {
		return i > 0 && sameRun(es[i-1], es[i]) && block[i-1] && block[i] && (bound[i] || !bound[i-1])
	}
	runs := map[string]*layout.Widths{}
	for j, l := range laid {
		brk := pending
		pending = false
		first := l.entry >= 0 && (j == 0 || laid[j-1].entry != l.entry)
		last := l.entry >= 0 && (j+1 == len(laid) || laid[j+1].entry != l.entry)
		// A key's first line outside a group, or a project's heading: where
		// a box draws its rule.
		keyStart := false
		switch {
		case l.kind == lineSection:
			brk, pending = true, !t.borders
		case l.group && l.indent == 2 && (l.kind == lineGroup || l.key != ""):
			// A project's heading, with facts of its own or none.
			brk, keyStart = true, true
		case first:
			i := l.entry
			brk = brk || (block[i] && !bound[i] && !stacks(i)) || (start[i] == i && long(i))
			keyStart = start[i] == i && es[i].Group == ""
		}
		if last {
			i := l.entry
			next := i + 1
			pending = (block[i] && !(next < len(es) && stacks(next))) ||
				(long(i) && (next == len(es) || start[next] == next)) ||
				(es[i].Group != "" && l.kind == lineBar)
		}
		keys := t.flat
		if l.section != "" {
			keys = t.sections
		}
		var paint func(string) string
		switch {
		case !t.st.on:
		case l.level == remote.StatusWarning && l.kind == lineEntry && l.first:
			paint = func(s string) string { return report.Colored(report.WarnLevel, s) }
		case l.level == remote.StatusHighlight && l.kind == lineEntry:
			paint = t.st.head
		}
		wrap := l.section != ""
		prose := layout.Keep
		if wrap {
			prose = layout.WrapWords
		}
		switch l.kind {
		case lineSection:
			out = append(out, layout.Row{Widths: keys, Break: true, Underline: true, Cells: []layout.Cell{{Text: l.value, Span: true, Paint: t.st.head}}})
		case lineGroup:
			out = append(out, layout.Row{Widths: keys, Break: brk, Indent: l.indent, Cells: []layout.Cell{{Text: l.key, Span: true, Paint: t.st.name}}})
		case lineEntry:
			key := layout.Cell{Text: l.key}
			value := layout.Cell{Text: l.value, Fit: prose}
			row := layout.Row{Widths: keys, Break: brk, Indent: l.indent, Paint: paint}
			if l.group {
				// The project's name in its color, its facts as any line's.
				key.Paint, value.Paint, row.Paint = t.st.name, paint, nil
			}
			if len(l.cells) > 0 {
				w := runs[l.run]
				if w == nil {
					w = layout.NewWidths()
					runs[l.run] = w
				}
				fit := layout.Keep
				if wrap {
					fit = layout.MoveLast
				}
				cells := layout.Cells(l.cells...)
				cells[len(cells)-1].Span, cells[len(cells)-1].Fit = true, fit
				value = layout.Cell{Grid: &layout.Grid{Rows: []layout.Row{{Widths: w, Cells: cells}}}, Paint: value.Paint}
			}
			row.Cells = []layout.Cell{key, value}
			out = append(out, row)
		case lineBar:
			// Quoted before it is painted (layOut): the quotes are the
			// command's, inside its bold. A command never wraps.
			fit := prose
			if _, _, ok := report.SplitCmd(l.value); ok {
				fit = layout.Keep
			}
			bar := layout.Cell{Text: l.value, Prefix: report.BarMark, Hang: l.hang, Fit: fit}
			if t.st.on {
				bar.Paint = report.PaintBar
			}
			out = append(out, layout.Row{Widths: keys, Break: brk, Cells: []layout.Cell{{}, bar}})
		}
		if t.borders && l.kind != lineSection {
			// In a box: a rule at each key and project, an empty row where a
			// blank line goes inside a key, a section's keys at its edge.
			r := &out[len(out)-1]
			blank := r.Break && !keyStart
			r.Break = keyStart
			if l.section != "" {
				r.Indent = max(0, r.Indent-2)
			}
			if blank {
				out = append(out[:len(out)-1], layout.Row{Widths: keys}, *r)
			}
		}
	}
	return out, pending
}

// sharedLater says whether an entry after es[i] in its run (sameRun),
// holds the same remedies as es[i].
func sharedLater(es []remote.StatusEntry, i int) bool {
	if len(es[i].Remedies) == 0 {
		return false
	}
	lines := report.RemedyLines(es[i].Remedies)
	for _, e := range es[i+1:] {
		if !sameRun(e, es[i]) {
			return false
		}
		if slices.Equal(report.RemedyLines(e.Remedies), lines) {
			return true
		}
	}
	return false
}
