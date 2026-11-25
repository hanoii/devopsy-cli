package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/layout"
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
	newEntryTable(&b, style{on: true}).print(es)
	want := "sync  none\n" +
		"      \033[2m| \033[0mfrom its parent:\n" +
		"      \033[2m| \033[0m\033[1m$ devopsy @dev --sync --from @'[2001:db8::1]:prod'\033[0m\n"
	if b.String() != want {
		t.Errorf("a command ending in a bracketed address:\n%q\nwant:\n%q", b.String(), want)
	}
}

// A report in sections: a section's title underlined between blank lines,
// a blank line around a key of several lines and a project, one key
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
	newEntryTable(&b, style{}).print(es)
	want := `
server
------

  system     Linux
  disks      /  xfs

             warning: / is 95% full
             | to fix it:
             | $ devopsy @vm1: --gc --dry-run --and-a-command-much-longer-than-the-width-left-for-it-to-show-whole

  setup      a line long enough to wrap at a word once it runs past the report's width, which it
             does here

projects
--------

  app        project uid 1
    prod     released just now by ci          1 of 1 running  uid 2

  shop       project uid 3
    staging  released 2 days ago by somebody  1 of 2 running  uid 4

             warning: app restarting

  web
    dev      gone                             x

leftovers
---------

  images     busybox:1.37  4.3 MB
             alpine:3      8.0 MB
             no release holds them
             | remove them:
             | $ docker rmi busybox:1.37 alpine:3
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
	newEntryTable(&b, style{}).print(es)
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
	newEntryTable(&b, style{}).print(flat)
	if strings.Contains(b.String(), "\x1b") {
		t.Errorf("a flat report's key: %q", b.String())
	}
}

// Rows of cells under one key align with each other whatever lines sit between them, a full
// disk's warning between two disks' rows, under its disk's row with a blank line after it.
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
	newEntryTable(&b, style{}).print(es)
	want := `
server
------

  disks  /            ext4  releases
         warning: / is 94% full

         /mnt/backup  ext4  snapshots
`
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

// A remedy's text that wraps goes on under its number, after its bar, its first line keeping
// its indent: a numbered fix's second line and its wrapped rest start where its text does.
func TestEntryWrappedRemedyKeepsItsIndent(t *testing.T) {
	t.Parallel()
	long := strings.TrimSpace(strings.Repeat("words ", 20))
	es := []remote.StatusEntry{{Level: remote.StatusWarning, Section: "server", Key: "disks", Lines: []string{"full"},
		Remedies: []report.Remedy{report.Fix("first\n" + long), report.Fix("second")}}}
	var b bytes.Buffer
	newEntryTable(&b, style{}).print(es)
	lines := strings.Split(b.String(), "\n")
	at := func(l string) int {
		if !strings.HasPrefix(strings.TrimLeft(l, " "), report.BarMark) {
			return -1
		}
		return len(l) - len(strings.TrimLeft(l, " |"))
	}
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

// A run's warnings bound to no row come after its facts, in their order,
// in what prints and in --json alike: a warning under a row of cells stays
// under it, a note under its line, nothing moves across runs, and moving
// them again changes nothing.
func TestOrderWarnings(t *testing.T) {
	t.Parallel()
	e := func(level, key string, line string) remote.StatusEntry {
		return remote.StatusEntry{Level: level, Section: "server", Key: key, Lines: []string{line}}
	}
	row := e(remote.StatusInfo, "disks", "/")
	row.Cells = []string{"/", "xfs"}
	note := e(remote.StatusInfo, "docker", "its noProxy leaves ::1")
	note.Remedies = []report.Remedy{report.Hint("add it")}
	es := []remote.StatusEntry{
		e(remote.StatusInfo, "system", "Linux"),
		e(remote.StatusWarning, "system", "reboot"),
		e(remote.StatusInfo, "system", "clock: synchronized"),
		e(remote.StatusWarning, "system", "kexec"),
		e(remote.StatusInfo, "docker", "29.9.1"),
		e(remote.StatusInfo, "docker", "containers' proxy: x"),
		note,
		e(remote.StatusInfo, "docker", "data: /var/lib/docker"),
		row,
		e(remote.StatusWarning, "disks", "/ is full"),
		e(remote.StatusInfo, "disks", "/mnt"),
	}
	lines := func(es []remote.StatusEntry) string {
		var out []string
		for _, e := range es {
			out = append(out, e.Lines[0])
		}
		return strings.Join(out, "|")
	}
	want := "Linux|clock: synchronized|reboot|kexec|29.9.1|containers' proxy: x|its noProxy leaves ::1|data: /var/lib/docker|/|/ is full|/mnt"
	got := orderWarnings(es)
	if lines(got) != want {
		t.Errorf("ordered:\n%s\nwant:\n%s", lines(got), want)
	}
	if again := orderWarnings(got); lines(again) != want {
		t.Errorf("ordered again:\n%s", lines(again))
	}
}

// Blank lines in a report, the same at any width: a section's title
// underlined between blank lines; around a key of several lines; around a
// warning bound to nothing, a run of them together; after a warning under
// a row of cells (none before it) and after a note under its line; before
// each project; in a project, after an entry's bar lines; none between a
// project's rows otherwise, nor between keys of one line.
func TestReportBlankLines(t *testing.T) {
	t.Parallel()
	info := func(section, group, key string, lines ...string) remote.StatusEntry {
		return remote.StatusEntry{Level: remote.StatusInfo, Section: section, Group: group, Key: key, Lines: lines}
	}
	warn := func(section, group, key, line string, fixes ...report.Remedy) remote.StatusEntry {
		e := info(section, group, key, line)
		e.Level, e.Remedies = remote.StatusWarning, fixes
		return e
	}
	row := func(section, group, key string, cells ...string) remote.StatusEntry {
		e := info(section, group, key, remote.CellsJoined(cells...))
		e.Cells = cells
		return e
	}
	note := info("server", "", "docker", "its noProxy leaves ::1")
	note.Remedies = []report.Remedy{report.Hint("add it:", "devopsy --server-setup-howto proxy")}
	gone := row("projects", "shop", "pr-1", "gone", "a uid claim")
	gone.Remedies = []report.Remedy{report.Hint("remove it:", "devopsy @vm1: --gc")}
	es := []remote.StatusEntry{
		info("server", "", "system", "Linux"),
		info("server", "", "arch", "amd64"),
		info("server", "", "docker", "29.9.1"),
		info("server", "", "docker", "containers' proxy: x"),
		note,
		warn("server", "", "docker", "no apparmor_parser"),
		warn("server", "", "docker", "logs grow"),
		row("server", "", "disks", "/", "xfs"),
		warn("server", "", "disks", "/ is full", report.Fix("", "devopsy @vm1: --gc")),
		row("server", "", "disks", "/mnt", "ext4"),
		row("projects", "shop", "prod", "released", "1 of 1 running"),
		gone,
		row("projects", "shop", "dev", "released", "1 of 1 running"),
		row("projects", "web", "prod", "released", "1 of 1 running"),
	}
	var b bytes.Buffer
	newEntryTable(&b, style{}).print(es)
	want := `
server
------

  system  Linux
  arch    amd64

  docker  29.9.1
          containers' proxy: x
          its noProxy leaves ::1
          | add it:
          | $ devopsy --server-setup-howto proxy

          warning: no apparmor_parser
          warning: logs grow

  disks   /     xfs
          warning: / is full
          | to fix it:
          | $ devopsy @vm1: --gc

          /mnt  ext4

projects
--------

  shop
    prod  released  1 of 1 running
    pr-1  gone      a uid claim
          | remove it:
          | $ devopsy @vm1: --gc

    dev   released  1 of 1 running

  web
    prod  released  1 of 1 running
`
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

// A report flag prints as text, as JSON or in boxes, by one of --json,
// --borders and --no-borders, never two of them; with none of them, away
// from a terminal, as text.
func TestReportArgs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		args []string
		mode reportMode
		code int
	}{
		{nil, textReport, -1},
		{[]string{"--json"}, jsonReport, -1},
		{[]string{"--borders"}, boxedReport, -1},
		{[]string{"--no-borders"}, textReport, -1},
		{[]string{"--json", "--json"}, jsonReport, -1},
		{[]string{"--json", "--borders"}, textReport, 1},
		{[]string{"--borders", "--json"}, textReport, 1},
		{[]string{"--json", "--no-borders"}, textReport, 1},
		{[]string{"--borders", "--no-borders"}, textReport, 1},
		{[]string{"--yaml"}, textReport, 1},
	} {
		if mode, code := reportArgs("--status", c.args, "usage: devopsy --status [--json | --borders | --no-borders]", &bytes.Buffer{}); mode != c.mode || code != c.code {
			t.Errorf("%q: %d, %d, want %d, %d", c.args, mode, code, c.mode, c.code)
		}
	}
}

// With borders, each section is a box under its title, every box's
// columns at one width: a rule between its top-level entries (its keys
// and projects) and none inside one, so a project's instances and
// environments stay in its block; an empty row where a blank line goes
// inside an entry; every heading a key row, one with no facts its value
// empty; its keys at the box's edge.
func TestReportBoxed(t *testing.T) {
	t.Parallel()
	info := func(section, group, key string, lines ...string) remote.StatusEntry {
		return remote.StatusEntry{Level: remote.StatusInfo, Section: section, Group: group, Key: key, Lines: lines}
	}
	row := func(section, group, key string, cells ...string) remote.StatusEntry {
		e := info(section, group, key, remote.CellsJoined(cells...))
		e.Cells = cells
		return e
	}
	warn := info("server", "", "system", "reboot")
	warn.Level = remote.StatusWarning
	es := []remote.StatusEntry{
		info("server", "", "system", "Linux", "clock: synchronized"),
		warn,
		info("server", "", "docker", "29.9.1"),
		row("projects", "shop", "prod", "released", "1 of 1 running"),
		row("projects", "shop", "staging", "released", "0 of 1 running"),
		row("projects", "tunnel", "prod", "released", "1 of 1 running"),
	}
	inEU := row("projects", "tunnel", "prod", "released", "2 of 2 running")
	inEU.Instance = "eu"
	es = append(es, inEU)
	var b bytes.Buffer
	tab := newEntryTable(&b, style{})
	tab.borders = true
	tab.print(es)
	want := `
server
╭───────────┬──────────────────────────╮
│ system    │ Linux                    │
│           │ clock: synchronized      │
│           │                          │
│           │ warning: reboot          │
├───────────┼──────────────────────────┤
│ docker    │ 29.9.1                   │
╰───────────┴──────────────────────────╯

projects
╭───────────┬──────────────────────────╮
│ shop      │                          │
│   prod    │ released  1 of 1 running │
│   staging │ released  0 of 1 running │
├───────────┼──────────────────────────┤
│ tunnel    │                          │
│   prod    │ released  1 of 1 running │
│   eu      │                          │
│     prod  │ released  2 of 2 running │
╰───────────┴──────────────────────────╯
`
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

// Whether a warning is bound to a row is decided on the order the report
// built its entries, and moves with it (ordered): one about a line with no
// cells (a disk that did not read) that moves after a row of cells in its
// key is still no row's, a blank line between them.
func TestMovedWarningStaysUnbound(t *testing.T) {
	t.Parallel()
	row := remote.StatusEntry{Level: remote.StatusInfo, Section: "server", Key: "disks", Cells: []string{"/mnt", "ext4"}, Lines: []string{remote.CellsJoined("/mnt", "ext4")}}
	es := []remote.StatusEntry{
		{Level: remote.StatusInfo, Section: "server", Key: "disks", Lines: []string{"releases /home/devopsy: it did not answer"}},
		{Level: remote.StatusWarning, Section: "server", Key: "disks", Lines: []string{"the release root's filesystem makes no reflinks"}},
		row,
	}
	_, before := blocks(es)
	moved, _, after := ordered(es)
	if moved[2].Level != remote.StatusWarning {
		t.Fatalf("the control: the warning moves after its key's facts: %v", moved)
	}
	if before[1] || after[2] != before[1] {
		t.Errorf("the warning bound to a row: %v before the move, %v after", before[1], after[2])
	}
	var b bytes.Buffer
	newEntryTable(&b, style{}).print(es)
	if !strings.Contains(b.String(), "/mnt  ext4\n\n") {
		t.Errorf("no blank line between a row and a warning that is not about it:\n%s", b.String())
	}
}

// A project's name with no facts on its heading sets no key column
// without borders: its heading runs across the report, and the keys'
// column is as wide as the keys need. In a box, every heading is a key
// row, so the column's line runs on by it, and its name sets the column.
func TestBoxedKeyColumnLeavesAProjectHeadingOut(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	// keyColumn is the report's system line up to its value.
	keyColumn := func(info remote.ServerInfo, borders bool) string {
		var b strings.Builder
		tab := newEntryTable(&b, style{})
		tab.borders = borders
		tab.print(serverInfoReport(info, "vm1", "vm1:", "0.53.0", now))
		for _, l := range strings.Split(b.String(), "\n") {
			if i := strings.Index(l, "Debian"); i >= 0 && strings.Contains(l, "system") {
				return l[:i]
			}
		}
		t.Fatalf("no system line in:\n%s", b.String())
		return ""
	}
	long := healthyServerInfo()
	long.Environments = append(long.Environments, remote.Environment{Path: "storefront-checkout-service/prod",
		Status: "release\t20261027115000\t{\"mode\":\"image\",\"time\":\"2026-10-27T11:50:00Z\",\"by\":\"ana\"}\nkept\t1\nsynced\t\n"})
	if short, wide := keyColumn(healthyServerInfo(), false), keyColumn(long, false); short != wide {
		t.Errorf("a project's name moved the value column: %q, with a long project name %q", short, wide)
	}
	if boxed := keyColumn(long, true); layout.Width(boxed) != layout.Width("│ storefront-checkout-service │ ") {
		t.Errorf("in a box, the key column is not the long name's: %q", boxed)
	}
	if !strings.Contains(reportText(long, true), "│ storefront-checkout-service │") {
		t.Errorf("in a box, a heading with no facts has no column line by it:\n%s", reportText(long, true))
	}
}

func reportText(info remote.ServerInfo, borders bool) string {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	var b strings.Builder
	tab := newEntryTable(&b, style{})
	tab.borders = borders
	tab.print(serverInfoReport(info, "vm1", "vm1:", "0.53.0", now))
	return b.String()
}

// With borders, a command wider than the room widens the boxes up to the
// terminal's width (reportWidth with none) and no further: past it, its
// own line runs out past the box's side, every other line of every box in
// its room.
func TestBoxedLongLineRunsPastTheSide(t *testing.T) {
	t.Parallel()
	info := healthyServerInfo()
	for i := range 8 {
		info.Images = append(info.Images, remote.HostImage{ID: "i" + string(rune('a'+i)), Tags: []string{"registry.example.com/team/app-" + string(rune('a'+i)) + ":2026-11-14-build"}, Size: 1e6})
	}
	out := reportText(info, true)
	if !strings.Contains(out, "docker rmi") {
		t.Fatalf("the control: no docker rmi in:\n%s", out)
	}
	// With a terminal as wide as the command, the boxes widen to hold it.
	var wide strings.Builder
	tab := newEntryTable(&wide, style{})
	tab.borders, tab.wide = true, 1000
	tab.print(serverInfoReport(info, "vm1", "vm1:", "0.53.0", time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)))
	for _, l := range strings.Split(wide.String(), "\n") {
		if strings.HasPrefix(l, "│") && !strings.HasSuffix(l, "│") {
			t.Errorf("a line runs past its box though the terminal holds it:\n%s", l)
		}
	}
	// The server's box: from its title to the next.
	server := out[strings.Index(out, "server\n"):strings.Index(out, "devopsy\n")]
	for _, l := range strings.Split(server, "\n") {
		if w := layout.Width(l); w > reportWidth {
			t.Fatalf("the server's box drawn %d wide for leftovers' command, past its %d:\n%s", w, reportWidth, l)
		}
	}
}

// A flat report's box, which wraps nothing, widens for a long line no
// further than the terminal, a narrow one too: a longer line runs past its
// side alone.
func TestFlatBoxWidensNoFurtherThanTheTerminal(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	tab := newEntryTable(&b, style{})
	tab.borders, tab.wide = true, 80
	url := "https://shop.example.com/" + strings.Repeat("a-long-path/", 6)
	tab.write([]layout.Row{tab.row("project", "shop-prod"), tab.row("url", url)})
	lines := strings.Split(b.String(), "\n")
	if !strings.Contains(b.String(), url) {
		t.Fatalf("the control: no url in:\n%s", b.String())
	}
	if w := layout.Width(lines[0]); w > 80 {
		t.Errorf("on an 80-column terminal the box is %d wide for one line it cannot wrap:\n%s", w, b.String())
	}
}

// With neither --borders nor --no-borders, the reports and lists are
// boxed as the user's config says, at a terminal only: a pipe gets the
// plain lines without the config being read, and a config that does not
// parse fails there.
func TestBordersDefault(t *testing.T) {
	t.Parallel()
	loads := 0
	load := func(c *remote.Config, err error) func(string) (*remote.Config, error) {
		return func(dir string) (*remote.Config, error) {
			if dir != "" {
				t.Errorf("read the config of %q, not the user's alone", dir)
			}
			loads++
			return c, err
		}
	}
	for _, c := range []struct {
		terminal, borders, want bool
		err                     error
	}{
		{terminal: true, borders: true, want: true},
		{terminal: true},
		{borders: true},
		{terminal: true, err: errors.New("config.yaml: borders: true or false")},
	} {
		loads = 0
		got, err := bordersDefault(c.terminal, load(&remote.Config{Borders: c.borders}, c.err))
		if got != c.want || (err != nil) != (c.err != nil) {
			t.Errorf("%+v: %v, %v", c, got, err)
		}
		if !c.terminal && loads > 0 {
			t.Errorf("%+v: the config read away from a terminal", c)
		}
	}
	// What is given wins over the default, and reads no config.
	for a, want := range map[string]bool{"--borders": true, "--no-borders": false} {
		if got, err := (bordersArg{a}).on(&bytes.Buffer{}); got != want || err != nil {
			t.Errorf("%s: %v, %v", a, got, err)
		}
	}
	if b := (bordersArg{}); !b.take("--borders") || !b.take("--borders") || b.take("--no-borders") {
		t.Error("take: the same flag twice is fine, the other after it refused")
	}
}
