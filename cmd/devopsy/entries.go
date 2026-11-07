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
		e.Fixes = slices.Clone(e.Fixes)
		for j := range e.Fixes {
			f := &e.Fixes[j]
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
// entries' of that key, a warning's lines and fixes' texts yellow, each
// command bold after "$ ".
func (t entryTable) print(es []remote.StatusEntry) {
	prev := ""
	for i, e := range es {
		paint := func(s string) string { return s }
		if e.Level == remote.StatusWarning {
			paint = t.st.warn
		}
		for j, l := range e.Lines {
			key := ""
			if j == 0 && (i == 0 || e.Key != prev) {
				key = e.Key
			}
			fmt.Fprintln(t.w, paint(report.Copyable(t.row(key, l))))
		}
		for _, f := range e.Fixes {
			if f.Text != "" {
				fmt.Fprintln(t.w, paint(report.Copyable(t.row("", f.Text))))
			}
			for _, c := range f.Cmds {
				fmt.Fprintln(t.w, t.st.head(report.Copyable(t.row("", report.CmdMark+c))))
			}
		}
		prev = e.Key
	}
}
