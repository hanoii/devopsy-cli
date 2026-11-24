package layout

import (
	"strings"
	"testing"
)

// Width counts what a terminal shows: a wide character two columns, an
// emoji two, a combining mark none, color and a hyperlink's escapes none.
func TestWidth(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		s    string
		want int
	}{
		{"abc", 3},
		{"日本", 4},
		{"🚀", 2},
		{"é", 1},
		{"\x1b[0;33mwarn\x1b[0m", 4},
		{"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\", 4},
		{"\x1b]8;;https://example.com\alink\x1b]8;;\a", 4},
	} {
		if got := Width(c.s); got != c.want {
			t.Errorf("Width(%q) = %d, want %d", c.s, got, c.want)
		}
	}
	if got := Pad("\x1b[1mab\x1b[0m", 4); got != "\x1b[1mab\x1b[0m  " {
		t.Errorf("Pad pads by what shows: %q", got)
	}
}

// Wrap cuts at single spaces into parts that fit the room: the first
// keeps the text's leading spaces, a run of spaces inside the text stays,
// a break drops the spaces it lands in, a word wider than the room stays
// whole.
func TestWrap(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		text string
		room int
		want []string
	}{
		{"one two three", 7, []string{"one two", "three"}},
		{"  one two three", 7, []string{"  one two", "three"}},
		{"/  xfs  free", 20, []string{"/  xfs  free"}},
		{"aa  bb", 3, []string{"aa", "bb"}},
		{"short averyveryverylongword end", 8, []string{"short", "averyveryverylongword", "end"}},
		{"日本 日本 日本", 9, []string{"日本 日本", "日本"}},
	} {
		if got := Wrap(c.text, c.room); strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("Wrap(%q, %d) = %q, want %q", c.text, c.room, got, c.want)
		}
	}
}

func text(s string) Cell { return Cell{Text: s} }

// Rows naming one Widths share its columns wherever they sit: at other
// indents (the indent counted in the first cell), in another grid of
// the tree, across rows between them that name other columns.
func TestWidthsShared(t *testing.T) {
	t.Parallel()
	keys := NewWidths()
	cells := NewWidths()
	row := func(cs ...string) *Grid {
		r := Row{Widths: cells}
		for _, c := range cs {
			r.Cells = append(r.Cells, text(c))
		}
		r.Cells[len(r.Cells)-1].Span = true
		return &Grid{Rows: []Row{r}}
	}
	g := &Grid{Rows: []Row{
		{Widths: keys, Indent: 2, Cells: []Cell{text("system"), text("Linux")}},
		{Indent: 2, Cells: []Cell{{Text: "shop", Span: true}}},
		{Widths: keys, Indent: 4, Cells: []Cell{text("prod"), {Grid: row("released now", "1 of 1", "uid 2")}}},
		{Widths: keys, Indent: 4, Cells: []Cell{text(""), text("warning: app restarting")}},
		{Widths: keys, Indent: 4, Cells: []Cell{text("staging"), {Grid: row("released 2 days ago", "1 of 2", "uid 4")}}},
	}}
	want := `  system     Linux
  shop
    prod     released now         1 of 1  uid 2
             warning: app restarting
    staging  released 2 days ago  1 of 2  uid 4
`
	if got := g.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A column empty in every row naming it is left out with its gap, and
// trailing empty cells make the last one shown the row's last; a column
// has a minimum, a fixed width (a wider cell pushes its own row only)
// and an alignment.
func TestColumns(t *testing.T) {
	t.Parallel()
	w := NewWidths()
	g := &Grid{Widths: w, Rows: []Row{
		{Cells: []Cell{text("a"), text(""), text("x"), text("")}},
		{Cells: []Cell{text("bbb"), text(""), text("y"), text("z")}},
	}}
	if got, want := g.String(), "a    x\nbbb  y  z\n"; got != want {
		t.Errorf("empty columns:\n%q\nwant:\n%q", got, want)
	}
	w = NewWidths().Min(0, 6).Right(1)
	g = &Grid{Widths: w, Rows: []Row{
		{Cells: []Cell{text("id"), text("1.2 GiB"), text("ok")}},
		{Cells: []Cell{text("id2"), text("840 MiB"), text("")}},
		{Cells: []Cell{text("id3"), text("3 KiB"), text("ok")}},
	}}
	if got, want := g.String(), "id      1.2 GiB  ok\nid2     840 MiB\nid3       3 KiB  ok\n"; got != want {
		t.Errorf("a minimum and a right column:\n%q\nwant:\n%q", got, want)
	}
	w = NewWidths().Fixed(0, 4)
	g = &Grid{Widths: w, Rows: []Row{
		{Cells: []Cell{text("ab"), text("x")}},
		{Cells: []Cell{text("abcdef"), text("y")}},
	}}
	if got, want := g.String(), "ab    x\nabcdef  y\n"; got != want {
		t.Errorf("a fixed column:\n%q\nwant:\n%q", got, want)
	}
}

// Help's row: the scope right aligned with a gap of one, what you type
// in a fixed column whose overflow moves the description to the next
// line at its column, a description of several lines under itself, a
// row with no description.
func TestOverflow(t *testing.T) {
	t.Parallel()
	w := NewWidths().Fixed(0, 6).Right(0).Gap(0, 1).Fixed(1, 27).Overflow(1)
	row := func(scope, typed, desc string) Row {
		return Row{Cells: []Cell{text(scope), text(typed), text(desc)}}
	}
	g := &Grid{Widths: w, Rows: []Row{
		row("@t", "--status [--json]", "containers"),
		row("", "--a-flag-far-too-long-for-the-column", "what it does\nand more"),
		row("[@s]", "--x", ""),
	}}
	want := "    @t --status [--json]            containers\n" +
		"       --a-flag-far-too-long-for-the-column\n" +
		"                                    what it does\n" +
		"                                    and more\n" +
		"  [@s] --x\n"
	if got := g.String(); got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

// A row of cells fitted to the room (MoveLast): its last cell moves whole
// to a line of its own when the row's own cells do not fit, never by the
// padding a wider row gives it; a cell is padded only while the row
// still fits with it padded; a row with an empty last cell is laid out by
// the cells it shows.
func TestMoveLast(t *testing.T) {
	t.Parallel()
	render := func(room int, rows ...[]string) string {
		w := NewWidths()
		g := &Grid{Width: room, Widths: w}
		for _, cs := range rows {
			var r Row
			for _, c := range cs {
				r.Cells = append(r.Cells, text(c))
			}
			r.Cells[len(r.Cells)-1].Span = true
			r.Cells[len(r.Cells)-1].Fit = MoveLast
			g.Rows = append(g.Rows, r)
		}
		return g.String()
	}
	long := "released 10 min ago by somebody@a-long-runner-name.example.com"
	if got, want := render(86, []string{long, "3 of 3 running", "uid 2091094815", "a sync or an export from it"}),
		long+"  3 of 3 running  uid 2091094815\na sync or an export from it\n"; got != want {
		t.Errorf("a row past the room:\n%q\nwant:\n%q", got, want)
	}
	if got, want := render(86, []string{long, "3 of 3 running", "uid 2091094815", ""}),
		long+"  3 of 3 running\nuid 2091094815\n"; got != want {
		t.Errorf("an empty last cell:\n%q\nwant:\n%q", got, want)
	}
	short := strings.Repeat("r", 60)
	got := render(86, []string{short, "3 of 3 running", "uid 7", ""}, []string{"released", "1 of 1 running", "uid 2091094815 or so", "syncing"})
	if !strings.HasPrefix(got, short+"  3 of 3 running  uid 7\n") {
		t.Errorf("a row that fits is cut for the padding after its last shown cell:\n%s", got)
	}
	wide := "registry.example.com/team/app:2026-11-14-build-1234, registry.example.com/team/app:latest"
	if got := render(80, []string{"a:1", "4.3 MB"}, []string{wide, "8.0 MB"}); !strings.HasPrefix(got, "a:1") || !strings.Contains(got, "a:1  4.3 MB\n") && !strings.HasPrefix(got, "a:1"+strings.Repeat(" ", len(wide)-1)) {
		t.Errorf("a short row's last cell moved beside a wide one:\n%s", got)
	}
	if got := render(80, []string{"a:1", "4.3 MB"}, []string{wide, "8.0 MB"}); strings.HasPrefix(got, "a:1\n") {
		t.Errorf("a short row's last cell moved beside a wide one:\n%s", got)
	}
}

// Prose wraps at a word to its column, its later lines at its hang after
// its prefix, every line of it prefixed; with less room than MinRoom, or
// when it fits, it stays whole; a Keep cell never wraps.
func TestWrapCell(t *testing.T) {
	t.Parallel()
	keys := NewWidths()
	long := "a line long enough to wrap at a word once it runs past the report's width, which it does here"
	g := &Grid{Width: 100, MinRoom: 40, Rows: []Row{
		{Widths: keys, Indent: 2, Cells: []Cell{text("setup"), {Text: long, Fit: WrapWords}}},
		{Widths: keys, Cells: []Cell{text(""), {Text: "1. first " + long, Fit: WrapWords, Prefix: "| ", Hang: 3}}},
		{Widths: keys, Cells: []Cell{text(""), {Text: "$ devopsy " + long + " " + long, Prefix: "| "}}},
		{Widths: keys, Cells: []Cell{text(""), {Text: "", Prefix: "| "}}},
	}}
	want := "  setup  a line long enough to wrap at a word once it runs past the report's width, which it does\n" +
		"         here\n" +
		"         | 1. first a line long enough to wrap at a word once it runs past the report's width,\n" +
		"         |    which it does here\n" +
		"         | $ devopsy " + long + " " + long + "\n" +
		"         |\n"
	if got := g.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	g.MinRoom = 95
	if got := g.Lines()[0]; got != "  setup  "+long {
		t.Errorf("with less room than MinRoom: %q", got)
	}
}

// A row's paint colors each of its lines whole, its key and padding too,
// after its right is trimmed; a cell's paint gets each line without its
// prefix and returns it whole, measured unpainted.
func TestPaint(t *testing.T) {
	t.Parallel()
	yellow := func(s string) string { return "\x1b[33m" + s + "\x1b[0m" }
	bar := func(s string) string {
		if s == "" {
			return "\x1b[2m|\x1b[0m"
		}
		return "\x1b[2m| \x1b[0m" + s
	}
	keys := NewWidths()
	g := &Grid{Rows: []Row{
		{Widths: keys, Paint: yellow, Cells: []Cell{text("maintenance"), text("warning: on")}},
		{Widths: keys, Cells: []Cell{text(""), {Text: "since 10 min ago", Prefix: "| ", Paint: bar}}},
		{Widths: keys, Cells: []Cell{text(""), {Text: "", Prefix: "| ", Paint: bar}}},
		{Widths: keys, Cells: []Cell{{Text: "shop", Paint: yellow}, text("x")}},
	}}
	want := "\x1b[33mmaintenance  warning: on\x1b[0m\n" +
		"             \x1b[2m| \x1b[0msince 10 min ago\n" +
		"             \x1b[2m|\x1b[0m\n" +
		"\x1b[33mshop\x1b[0m         x\n"
	if got := g.String(); got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

// A break is a blank line before its row, never before a grid's first;
// an underline is a line of - as wide as its row's text, at its indent.
func TestBreakAndUnderline(t *testing.T) {
	t.Parallel()
	g := &Grid{Rows: []Row{
		{Break: true, Underline: true, Cells: []Cell{{Text: "server", Span: true}}},
		{Break: true, Indent: 2, Cells: []Cell{text("system"), text("Linux")}},
		{Break: true, Indent: 2, Underline: true, Cells: []Cell{{Text: "日本", Span: true}}},
	}}
	want := "server\n------\n\n  system  Linux\n\n  日本\n  ----\n"
	if got := g.String(); got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

// A grid in a cell (a sub-table) is laid out at its cell's column and
// measured by its widest line, so the columns after it hold.
func TestNested(t *testing.T) {
	t.Parallel()
	sub := &Grid{Rows: []Row{
		{Cells: []Cell{text("app"), text("running")}},
		{Cells: []Cell{text("worker"), text("no container")}},
	}}
	g := &Grid{Rows: []Row{
		{Cells: []Cell{text("containers"), {Grid: sub}, text("end")}},
		{Cells: []Cell{text("php"), text("8.4"), text("end")}},
	}}
	want := "containers  app     running       end\n" +
		"            worker  no container\n" +
		"php         8.4                   end\n"
	if got := g.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A box: a line between its columns, under its header and at a break,
// every cell padded so its right side holds, a span row across it with no
// column lines (the last column widened to hold it), a row with no cells
// an empty line, a grid in a cell without a box of its own, boxes sharing
// columns drawn at one width, and prose wrapped inside its cell.
func TestBox(t *testing.T) {
	t.Parallel()
	g := &Grid{Border: Box, Rows: []Row{
		{Header: true, Cells: []Cell{text("SNAPSHOT"), text("SIZE")}},
		{Cells: []Cell{text("s1"), text("1.2 GiB")}},
		{Cells: []Cell{{Text: "  images: app:1 db:2 cache:3", Span: true}}},
		{},
		{Break: true, Cells: []Cell{text("s2"), text("840 MiB")}},
	}}
	want := `╭──────────┬───────────────────╮
│ SNAPSHOT │ SIZE              │
├──────────┼───────────────────┤
│ s1       │ 1.2 GiB           │
│   images: app:1 db:2 cache:3 │
│          │                   │
├──────────┼───────────────────┤
│ s2       │ 840 MiB           │
╰──────────┴───────────────────╯
`
	if got := g.String(); got != want {
		t.Errorf("a list's box:\n%s\nwant:\n%s", got, want)
	}

	w := NewWidths()
	sub := &Grid{Rows: []Row{{Cells: []Cell{text("a"), text("b")}}}}
	one := &Grid{Border: Box, Widths: w, Rows: []Row{{Cells: []Cell{text("system"), text("Linux")}}}}
	two := &Grid{Border: Box, Widths: w, Rows: []Row{{Cells: []Cell{text("x"), {Grid: sub}}}}}
	root := &Grid{Rows: []Row{
		{Cells: []Cell{{Text: "server", Span: true}}},
		{Cells: []Cell{{Grid: one}}},
		{Cells: []Cell{{Grid: two}}},
	}}
	want = `server
╭────────┬───────╮
│ system │ Linux │
╰────────┴───────╯
╭────────┬───────╮
│ x      │ a  b  │
╰────────┴───────╯
`
	if got := root.String(); got != want {
		t.Errorf("boxes sharing columns:\n%s\nwant:\n%s", got, want)
	}

	long := "a line long enough to wrap at a word once it runs past the report's width, which it does here"
	b := &Grid{Border: Box, Width: 60, Rows: []Row{
		{Cells: []Cell{text("setup"), {Text: long, Fit: WrapWords}}},
		{Break: true, Cells: []Cell{text("x"), text("y")}},
	}}
	for _, l := range b.Lines() {
		if Width(l) > 60 {
			t.Errorf("a line past the box's room:\n%s", b.String())
			break
		}
	}
	if got := b.Lines()[2]; got != "│       │ runs past the report's width, which it does here │" {
		t.Errorf("prose wrapped in its cell: %q in:\n%s", got, b.String())
	}
}

// A right aligned column holds its row's last cell to the right too (a
// header's SIZE over its sizes).
func TestRightLastCell(t *testing.T) {
	t.Parallel()
	g := &Grid{Widths: NewWidths().Right(1), Rows: []Row{
		{Cells: Cells("ID", "SIZE")},
		{Cells: Cells("a", "1.2 GiB", "PARTIAL")},
	}}
	if got, want := g.String(), "ID     SIZE\na   1.2 GiB  PARTIAL\n"; got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}
