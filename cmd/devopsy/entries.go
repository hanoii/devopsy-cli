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
// with no section, at least as wide as more.
type entryTable struct {
	w  io.Writer
	st style
	// sections and flat are the key and value columns of the entries in
	// a section and of those in none.
	sections, flat *layout.Widths
	// width is where a section's prose wraps, reportWidth.
	width int
}

// newEntryTable is a table whose flat key column is at least as wide as
// each of more.
func newEntryTable(w io.Writer, st style, es []remote.StatusEntry, more ...string) entryTable {
	t := entryTable{w: w, st: st, sections: layout.NewWidths(), flat: layout.NewWidths(), width: reportWidth}
	for _, k := range more {
		t.flat.Min(0, layout.Width(k))
	}
	return t
}

// row is value under key in the column of the entries with no section.
func (t entryTable) row(key, value string) layout.Row {
	return layout.Row{Widths: t.flat, Cells: layout.Cells(key, value)}
}

// print prints es as one grid (rows), a blank line first when it starts
// with a section's heading.
func (t entryTable) print(es []remote.StatusEntry) { t.write(t.rows(es)) }

// write prints rows as one grid at the table's width, a blank line first
// when the first row breaks (a section's heading): every row of a report
// in one grid, so its columns are measured together.
func (t entryTable) write(rows []layout.Row) {
	if len(rows) == 0 {
		return
	}
	g := &layout.Grid{Rows: rows, Width: t.width, MinRoom: minWrapRoom}
	if rows[0].Break {
		fmt.Fprintln(t.w)
	}
	fmt.Fprint(t.w, g.String())
}

// minWrapRoom is the least room prose wraps into: with less left after
// its column, it stays whole.
const minWrapRoom = 40

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
// a row of cells with its run's widths (cellWidths), each key, name,
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
			out = append(out, laidLine{kind: lineSection, section: e.Section, value: e.Section})
		}
		newGroup := newSection || e.Group != prev.Group
		if e.Group != "" && newGroup && (!e.Head || e.Instance != "") {
			out = append(out, laidLine{kind: lineGroup, section: e.Section, indent: 2, key: report.Copyable(e.Group), group: true})
		}
		newInstance := newGroup || e.Instance != prev.Instance
		if e.Instance != "" && newInstance && !e.Head {
			out = append(out, laidLine{kind: lineGroup, section: e.Section, indent: 4, key: report.Copyable(e.Instance), group: true})
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
		newKey := newInstance || e.Key != prev.Key || e.Name != prev.Name || e.Head != prev.Head
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
			line := laidLine{kind: lineEntry, section: e.Section, indent: indent, value: report.Copyable(l), level: e.Level, first: j == 0}
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
			out = append(out, laidLine{kind: lineBar, section: e.Section, indent: indent, value: r.Text, hang: r.Hang, level: e.Level})
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
// of them is left out.
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

// rows are es as layOut lays them out: a section's heading bold after a
// blank line, a project's name in its color, a warning's message and the
// lines after a bar colored as devopsy's own lines are (report.Colored,
// report.PaintBar: the message yellow, the bar dim, its text plain, a
// command bold), a highlight's lines bold; in a section, prose past the
// width wraps at a word to its column, a line after a bar each part after
// one, and a row of cells past it moves its last cell whole to a line of
// its own at the column. A command never wraps, nor anything in a flat
// report.
func (t entryTable) rows(es []remote.StatusEntry) []layout.Row {
	var out []layout.Row
	runs := map[string]*layout.Widths{}
	for _, l := range layOut(es) {
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
			out = append(out, layout.Row{Widths: keys, Break: true, Cells: []layout.Cell{{Text: l.value, Span: true, Paint: t.st.head}}})
		case lineGroup:
			out = append(out, layout.Row{Widths: keys, Indent: l.indent, Cells: []layout.Cell{{Text: l.key, Span: true, Paint: t.st.name}}})
		case lineEntry:
			key := layout.Cell{Text: l.key}
			value := layout.Cell{Text: l.value, Fit: prose}
			row := layout.Row{Widths: keys, Indent: l.indent, Paint: paint}
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
			out = append(out, layout.Row{Widths: keys, Cells: []layout.Cell{{}, bar}})
		}
	}
	return out
}

// sharedLater says whether an entry after es[i], under the same key in
// the same section, group and instance with no other key between, holds the same
// remedies as es[i].
func sharedLater(es []remote.StatusEntry, i int) bool {
	if len(es[i].Remedies) == 0 {
		return false
	}
	lines := report.RemedyLines(es[i].Remedies)
	for _, e := range es[i+1:] {
		if e.Key != es[i].Key || e.Group != es[i].Group || e.Instance != es[i].Instance || e.Section != es[i].Section {
			return false
		}
		if slices.Equal(report.RemedyLines(e.Remedies), lines) {
			return true
		}
	}
	return false
}
