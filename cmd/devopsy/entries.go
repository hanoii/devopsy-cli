package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

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

// entryTable prints entries on w in st, their keys in a column.
type entryTable struct {
	w   io.Writer
	st  style
	col int
}

// newEntryTable is a table whose key column fits the keys of es and
// more, two spaces after the longest.
func newEntryTable(w io.Writer, st style, es []remote.StatusEntry, more ...string) entryTable {
	width := 0
	for _, k := range more {
		width = max(width, len(k))
	}
	for _, e := range es {
		width = max(width, len(e.Key))
	}
	return entryTable{w: w, st: st, col: width + 2}
}

// row is value under key, padded to the column before it is painted.
func (t entryTable) row(key, value string) string {
	return strings.TrimRight(fmt.Sprintf("%-*s%s", t.col, key, value), " ")
}

// print prints es: a key once over its entry's lines and the next
// entries' of that key, then its remedies as report lays them out
// (RemedyLines), a warning's lines and texts yellow, a highlight's bold,
// each command bold. Remedies a later entry of the same key shares are
// printed once, after the last entry holding them (sharedLater): one
// command that fixes several lines is offered once.
func (t entryTable) print(es []remote.StatusEntry) {
	prev := ""
	for i, e := range es {
		if sharedLater(es, i) {
			e.Remedies = nil
		}
		paint := func(s string) string { return s }
		switch e.Level {
		case remote.StatusWarning:
			paint = t.st.warn
		case remote.StatusHighlight:
			paint = t.st.head
		}
		for j, l := range e.Lines {
			key := ""
			if j == 0 && (i == 0 || e.Key != prev) {
				key = e.Key
			}
			fmt.Fprintln(t.w, paint(report.Copyable(t.row(key, l))))
		}
		for _, r := range report.RemedyLines(e.Remedies) {
			for _, l := range strings.Split(r, "\n") {
				// Quoted before it is painted: the quotes are the
				// command's, inside its bold.
				l = report.Copyable(l)
				if lead, cmd, ok := report.SplitCmd(l); ok {
					fmt.Fprintln(t.w, t.row("", lead+t.st.head(cmd)))
				} else {
					fmt.Fprintln(t.w, paint(t.row("", l)))
				}
			}
		}
		prev = e.Key
	}
}

// sharedLater says whether an entry after es[i], under the same key with
// no other key between, holds the same remedies as es[i].
func sharedLater(es []remote.StatusEntry, i int) bool {
	if len(es[i].Remedies) == 0 {
		return false
	}
	lines := report.RemedyLines(es[i].Remedies)
	for _, e := range es[i+1:] {
		if e.Key != es[i].Key {
			return false
		}
		if slices.Equal(report.RemedyLines(e.Remedies), lines) {
			return true
		}
	}
	return false
}
