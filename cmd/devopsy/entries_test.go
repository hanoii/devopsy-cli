package main

import (
	"bytes"
	"strings"
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
// cells aligned across projects; a warning's first line starting with
// "warning: " at its text's column, which no key column widens; cells under one key outside a project
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
             warning: / is 95% full
             to fix it:
             $ devopsy @vm1: --gc --dry-run --and-a-command-much-longer-than-the-width-left-for-it-to-show-whole
  setup      a line long enough to wrap at a word once it runs past the report's width, which it
             does here

projects
  app        project uid 1
    prod     released just now by ci          1 of 1 running  uid 2
  shop       project uid 3
    staging  released 2 days ago by somebody  1 of 2 running  uid 4
             warning: app restarting
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

// A key and a project's name show as every other line does, through report.Copyable, in the
// text and in --json, a flat report's key too: an environment's or a project's name another
// user chose reaches an admin's terminal.
func TestEntryTableEscapesKeysAndNames(t *testing.T) {
	t.Parallel()
	es := []remote.StatusEntry{
		{Level: remote.StatusInfo, Section: "projects", Group: "sh\x1b[2Jop", Key: "pr\x1b[2Jod", Lines: []string{"released"}},
		{Level: remote.StatusInfo, Section: "projects", Group: "ap\x1b[2Jp", Key: "ap\x1b[2Jp", Lines: []string{"project uid 1"}},
	}
	var b bytes.Buffer
	newEntryTable(&b, style{}, es).print(es)
	if strings.Contains(b.String(), "\x1b") {
		t.Errorf("a key or a heading reaches the terminal with its escape:\n%q", b.String())
	}
	for _, e := range copyableEntries(es) {
		if strings.Contains(e.Key+e.Group, "\x1b") {
			t.Errorf("--json: key %q, group %q", e.Key, e.Group)
		}
	}
	// A flat report's key too, as before the layout (the whole row went through Copyable).
	flat := []remote.StatusEntry{{Level: remote.StatusInfo, Key: "k\x1b[2Jk", Lines: []string{"v"}}}
	b.Reset()
	newEntryTable(&b, style{}, flat).print(flat)
	if strings.Contains(b.String(), "\x1b") {
		t.Errorf("a flat report's key: %q", b.String())
	}
}

// Rows of cells under one key align with each other whatever lines sit between them, a full
// disk's warning between two disks' rows.
func TestEntryCellsAlignAcrossAWarning(t *testing.T) {
	t.Parallel()
	row := func(cells ...string) remote.StatusEntry {
		return remote.StatusEntry{Level: remote.StatusInfo, Section: "server", Key: "disks", Lines: []string{remote.CellsJoined(cells...)}, Cells: cells}
	}
	es := []remote.StatusEntry{
		row("/", "ext4", "releases"),
		{Level: remote.StatusWarning, Section: "server", Key: "disks", Lines: []string{"/ is 94% full"}},
		row("/mnt/backup", "ext4", "snapshots"),
	}
	var b bytes.Buffer
	newEntryTable(&b, style{}, es).print(es)
	want := `
server
  disks  /            ext4  releases
         warning: / is 94% full
         /mnt/backup  ext4  snapshots
`
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

// A remedy's text that wraps goes on under its number, its first line keeping its indent: a
// numbered fix's second line and its wrapped rest start where its text does.
func TestEntryWrappedRemedyKeepsItsIndent(t *testing.T) {
	t.Parallel()
	long := strings.TrimSpace(strings.Repeat("words ", 20))
	es := []remote.StatusEntry{{Level: remote.StatusWarning, Section: "server", Key: "disks", Lines: []string{"full"},
		Remedies: []report.Remedy{report.Fix("first\n" + long), report.Fix("second")}}}
	var b bytes.Buffer
	newEntryTable(&b, style{}, es).print(es)
	lines := strings.Split(b.String(), "\n")
	at := func(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }
	for i, l := range lines {
		if strings.Contains(l, "1. first") {
			if want := at(l) + 3; at(lines[i+1]) != want || at(lines[i+2]) != want {
				t.Errorf("the fix's second line and its wrapped rest start at %d and %d, want %d:\n%s", at(lines[i+1]), at(lines[i+2]), want, b.String())
			}
			return
		}
	}
	t.Fatalf("no numbered fix:\n%s", b.String())
}

// A row of cells past the width is never cut at a word: its last cell moves whole to a line
// of its own at the value column, every other cell whole on the row.
func TestEntryRowMovesItsLastCellWhole(t *testing.T) {
	t.Parallel()
	cells := []string{"released 10 min ago by somebody@a-long-runner-name.example.com", "3 of 3 running", "uid 2091094815", "a sync or an export from it"}
	es := []remote.StatusEntry{
		{Level: remote.StatusInfo, Section: "projects", Group: "shop", Head: true, Key: "shop", Lines: []string{"project uid 1"}},
		{Level: remote.StatusInfo, Section: "projects", Group: "shop", Key: "prod", Lines: []string{remote.CellsJoined(cells...)}, Cells: cells},
	}
	var b bytes.Buffer
	newEntryTable(&b, style{}, es).print(es)
	// Each cell whole on a line, the last on its own: never "uid" on one line and its number on the next.
	lines := strings.Split(b.String(), "\n")
	whole := func(cell string) bool {
		for _, l := range lines {
			if strings.Contains(l, cell) {
				return true
			}
		}
		return false
	}
	last := false
	for _, l := range lines {
		last = last || strings.TrimSpace(l) == cells[3]
	}
	if !whole(cells[0]) || !whole(cells[1]) || !whole(cells[2]) || !last {
		t.Errorf("a cell cut, or the last cell not moved whole to its own line:\n%s", b.String())
	}
}

// A row of cells is laid out by its cells whatever its last holds: one whose last cell is
// empty (an environment with nothing running on it) is never cut at a word past the width,
// and one that fits stays one line, the padding after its last shown cell counting for
// nothing.
func TestEntryRowWithAnEmptyLastCellIsNeverCut(t *testing.T) {
	t.Parallel()
	row := func(key string, cells ...string) remote.StatusEntry {
		return remote.StatusEntry{Level: remote.StatusInfo, Section: "projects", Group: "shop", Key: key, Lines: []string{remote.CellsJoined(cells...)}, Cells: cells}
	}
	whole := func(out, cell string) bool {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, cell) {
				return true
			}
		}
		return false
	}
	// Past the width: no cell is cut in two.
	cells := []string{"released 10 min ago by somebody@a-long-runner-name.example.com", "3 of 3 running", "uid 2091094815", ""}
	es := []remote.StatusEntry{row("prod", cells...)}
	var b bytes.Buffer
	newEntryTable(&b, style{}, es).print(es)
	for _, c := range cells[:3] {
		if !whole(b.String(), c) {
			t.Errorf("the cell %q is cut at a word:\n%s", c, b.String())
		}
	}
	// Within the width as it shows (10 of key column, 86 of cells), past it only by the
	// padding under another row's wider uid cell: one line, as it fits, the next row after
	// it with no empty line between.
	short := []string{strings.Repeat("r", 60), "3 of 3 running", "uid 7", ""}
	es = []remote.StatusEntry{row("prod", short...), row("dev", "released", "1 of 1 running", "uid 2091094815 or so", "syncing")}
	b.Reset()
	newEntryTable(&b, style{}, es).print(es)
	if !strings.Contains(b.String(), short[0]+"  "+short[1]+"  "+short[2]+"\n    dev ") {
		t.Errorf("a row that fits the width is cut for the padding after its last shown cell:\n%s", b.String())
	}
}

// Whether a row's last cell moves goes by the row's own cells, never by the padding a wider
// row of its run gives it: a short row beside a wide one keeps its last cell on its line.
func TestEntryShortRowKeepsItsLastCellBesideAWideOne(t *testing.T) {
	t.Parallel()
	row := func(cells ...string) remote.StatusEntry {
		return remote.StatusEntry{Level: remote.StatusInfo, Section: "leftovers", Key: "images", Lines: []string{remote.CellsJoined(cells...)}, Cells: cells}
	}
	wide := "registry.example.com/team/app:2026-11-14-build-1234, registry.example.com/team/app:latest"
	es := []remote.StatusEntry{row("a:1", "4.3 MB"), row(wide, "8.0 MB")}
	var b bytes.Buffer
	newEntryTable(&b, style{}, es).print(es)
	for _, l := range strings.Split(b.String(), "\n") {
		if strings.Contains(l, "a:1") && !strings.Contains(l, "4.3 MB") {
			t.Errorf("a:1's row fits the width, and its size moved to a line of its own:\n%s", b.String())
		}
	}
}
