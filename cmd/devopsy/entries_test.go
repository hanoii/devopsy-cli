package main

import (
	"bytes"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// A remedy's command in the status table copies as shown with an
// address in brackets quoted (report.Copyable), the whole command bold:
// the quote belongs inside the bold, so the command reads as one, never
// a closing quote after the reset escape.
func TestEntryTableQuotesBeforePainting(t *testing.T) {
	t.Parallel()
	es := []remote.StatusEntry{{Level: remote.StatusInfo, Key: "sync", Lines: []string{"none"},
		Remedies: []report.Remedy{report.Hint("from its parent:", "devopsy @dev --sync --from @[2001:db8::1]:prod")}}}
	var b bytes.Buffer
	newEntryTable(&b, style{on: true}, es).print(es)
	want := "sync  none\n" +
		"      from its parent:\n" +
		"      \033[1m$ devopsy @dev --sync --from @'[2001:db8::1]:prod'\033[0m\n"
	if b.String() != want {
		t.Errorf("a command ending in a bracketed address:\n%q\nwant:\n%q", b.String(), want)
	}
}

// A report in sections: a section's heading after a blank line, one key
// column for every section, so each section's values start where the
// others' do; a project's heading, its rows indented under it, their
// cells aligned across projects; a warning marked "! " in its indent,
// which no key column widens; cells under one key outside a project
// aligned with each other; prose past reportWidth wrapped at a word to
// its column, a command never.
func TestEntryTableSections(t *testing.T) {
	t.Parallel()
	info := func(section, group, key string, lines ...string) remote.StatusEntry {
		return remote.StatusEntry{Level: remote.StatusInfo, Section: section, Group: group, Key: key, Lines: lines}
	}
	head := func(group string, lines ...string) remote.StatusEntry {
		e := info("projects", group, group, lines...)
		e.Head = true
		return e
	}
	row := func(section, group, key string, cells ...string) remote.StatusEntry {
		e := info(section, group, key, remote.CellsJoined(cells...))
		e.Cells = cells
		return e
	}
	long := "a line long enough to wrap at a word once it runs past the report's width, which it does here"
	warn := info("server", "", "", "/ is 95% full")
	warn.Level, warn.Remedies = remote.StatusWarning, []report.Remedy{report.Fix("", "devopsy @vm1: --gc --dry-run --and-a-command-much-longer-than-the-width-left-for-it-to-show-whole")}
	envWarn := info("projects", "shop", "", "app restarting")
	envWarn.Level = remote.StatusWarning
	img1, img2 := row("leftovers", "", "images", "busybox:1.37", "4.3 MB"), row("leftovers", "", "images", "alpine:3", "8.0 MB")
	img2.Lines = append(img2.Lines, "no release holds them")
	img2.Remedies = []report.Remedy{report.Hint("remove them:", "docker rmi busybox:1.37 alpine:3")}
	es := []remote.StatusEntry{
		info("server", "", "system", "Linux"),
		info("server", "", "disks", "/  xfs"),
		warn,
		info("server", "", "setup", long),
		head("app", "project uid 1"),
		row("projects", "app", "prod", "released just now by ci", "1 of 1 running", "uid 2"),
		head("shop", "project uid 3"),
		row("projects", "shop", "staging", "released 2 days ago by somebody", "1 of 2 running", "uid 4"),
		envWarn,
		row("projects", "web", "dev", "gone", "x"),
		img1, img2,
	}
	var b bytes.Buffer
	newEntryTable(&b, style{}, es).print(es)
	want := `
server
  system     Linux
  disks      /  xfs
!            / is 95% full
             to fix it:
             $ devopsy @vm1: --gc --dry-run --and-a-command-much-longer-than-the-width-left-for-it-to-show-whole
  setup      a line long enough to wrap at a word once it runs past the report's width, which it
             does here

projects
  app        project uid 1
    prod     released just now by ci          1 of 1 running  uid 2
  shop       project uid 3
    staging  released 2 days ago by somebody  1 of 2 running  uid 4
  !          app restarting
  web
    dev      gone                             x

leftovers
  images     busybox:1.37  4.3 MB
             alpine:3      8.0 MB
             no release holds them
             remove them:
             $ docker rmi busybox:1.37 alpine:3
`
	if b.String() != want {
		t.Errorf("sections:\n%s\nwant:\n%s", b.String(), want)
	}
	// In JSON the cells are quoted as the lines are.
	q := copyableEntries([]remote.StatusEntry{row("", "", "k", "@[2001:db8::1]:prod", "x")})
	if q[0].Cells[0] != "@'[2001:db8::1]:prod'" {
		t.Errorf("cells in JSON: %q", q[0].Cells)
	}
}
