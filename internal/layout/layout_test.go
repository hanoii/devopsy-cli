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
// column lines (the last column widened to hold it) and a rule where it
// meets columns, so the lines join, a row with no cells
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
├──────────┴───────────────────┤
│   images: app:1 db:2 cache:3 │
├──────────┬───────────────────┤
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

// A grid laid out again measures afresh: a row changed or gone since
// leaves no width behind.
func TestRenderAgain(t *testing.T) {
	t.Parallel()
	g := &Grid{Rows: []Row{{Cells: Cells("a-long-key", "x")}, {Cells: Cells("b", "y")}}}
	_ = g.Lines()
	g.Rows = g.Rows[1:]
	if got := g.String(); got != "b  y\n" {
		t.Errorf("laid out again: %q", got)
	}
}

// A tab in a cell shows as one space, measured as one: a terminal's tab
// stops are not the grid's columns, and a comment someone chose may hold
// one.
func TestTabInACell(t *testing.T) {
	t.Parallel()
	g := &Grid{Rows: []Row{{Cells: Cells("a\tb", "x")}, {Cells: Cells("abcd", "y")}}}
	if got, want := g.String(), "a b   x\nabcd  y\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A grid in a cell its parent leaves no room keeps fitting to none:
// its row of cells moves its last cell rather than run on.
func TestNoRoomLeft(t *testing.T) {
	t.Parallel()
	sub := &Grid{Rows: []Row{{Cells: []Cell{text("aaa"), {Text: "bbb", Span: true, Fit: MoveLast}}}}}
	g := &Grid{Width: 8, Rows: []Row{{Cells: []Cell{text("a-key-wider"), {Grid: sub}}}}}
	if got, want := g.String(), "a-key-wider  aaa\n             bbb\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A row's underline goes under all its lines, as wide as the widest; a
// row's paint leaves its empty lines empty; an empty span cell costs no
// gap.
func TestUnderlinePaintAndEmptySpan(t *testing.T) {
	t.Parallel()
	g := &Grid{Width: 12, Rows: []Row{{Underline: true, Cells: []Cell{text(""), {Text: "a title that wraps", Fit: WrapWords}}}}}
	if got, want := g.String(), "a title that\nwraps\n------------\n"; got != want {
		t.Errorf("underline: got %q, want %q", got, want)
	}
	bold := func(s string) string { return "\x1b[1m" + s + "\x1b[0m" }
	g = &Grid{Rows: []Row{{Paint: bold, Cells: []Cell{text("k"), text("one\n\nthree")}}}}
	if got, want := g.String(), bold("k  one")+"\n\n"+bold("   three")+"\n"; got != want {
		t.Errorf("paint: got %q, want %q", got, want)
	}
	g = &Grid{Rows: []Row{{Cells: []Cell{text("a"), {Span: true}, text("c")}}}}
	if got, want := g.String(), "a  c\n"; got != want {
		t.Errorf("empty span: got %q, want %q", got, want)
	}
}

// boxOf is g drawn in a box.
func boxOf(g *Grid) *Grid { g.Border = Box; return g }

// square fails unless every line of a box is as wide as its first:
// a box whose lines differ has a side that does not join.
func square(t *testing.T, name string, lines []string) {
	t.Helper()
	for _, l := range lines {
		if Width(l) != Width(lines[0]) || strings.Contains(l, "\n") {
			t.Errorf("%s: a line of the box is not as wide as its first:\n%s", name, strings.Join(lines, "\n"))
			return
		}
	}
}

// The cells after a span cell follow it at their gap, in no column: a
// long one widens no column for the other rows.
func TestCellsAfterASpanInNoColumn(t *testing.T) {
	t.Parallel()
	g := &Grid{Rows: []Row{
		{Cells: []Cell{{Text: "a"}, {Text: "x", Span: true}, {Text: "long-long-long"}, {Text: "z"}}},
		{Cells: Cells("b", "c", "d", "e")},
	}}
	want := "a  x  long-long-long  z\nb  c  d  e\n"
	if got := g.String(); got != want {
		t.Errorf("a cell after a span widened its column:\n%swant:\n%s", got, want)
	}
}

// MoveLast counts every cell the row shows, a span cell before its last
// too: a row wider than the room moves its last cell.
func TestMoveLastCountsASpan(t *testing.T) {
	t.Parallel()
	g := &Grid{Width: 20, Rows: []Row{
		{Cells: []Cell{{Text: "key"}, {Text: "a-middle-span-cell", Span: true}, {Text: "last", Fit: MoveLast}}},
	}}
	lines := g.Lines()
	if len(lines) != 2 || lines[1] != "last" {
		t.Errorf("a row of 29 columns in a room of 20 kept its last cell: %q", lines)
	}
}

// A box's span row of several lines keeps the box's sides on each.
func TestBoxSpanRowOfSeveralLines(t *testing.T) {
	t.Parallel()
	g := boxOf(&Grid{Rows: []Row{{Cells: Cells("a", "b")}, {Cells: []Cell{{Text: "one\ntwo", Span: true}}}}})
	square(t, "a span row of two lines", g.Lines())
}

// A box's span row is laid out as without borders: its indent, and its
// cell's paint.
func TestBoxSpanRowKeepsItsPaint(t *testing.T) {
	t.Parallel()
	paint := func(s string) string { return "\x1b[1m" + s + "\x1b[0m" }
	g := boxOf(&Grid{Rows: []Row{{Cells: Cells("a", "b")}, {Indent: 2, Cells: []Cell{{Text: "title", Span: true, Paint: paint}}}}})
	if got := strings.Join(g.Lines(), "\n"); !strings.Contains(got, paint("title")) {
		t.Errorf("an indented span row lost its paint:\n%q", got)
	}
}

// A box measures a row as it draws it (cells): a row with fewer cells
// than the box has columns wraps its last cell in what the columns after
// it leave, and the box stays in its room.
func TestBoxShortRowInItsRoom(t *testing.T) {
	t.Parallel()
	g := boxOf(&Grid{Width: 30, Rows: []Row{
		{Cells: []Cell{{Text: "a long prose line that wraps in the box", Fit: WrapWords}}},
		{Cells: Cells("k2", "v")},
	}})
	lines := g.Lines()
	square(t, "a short row", lines)
	if len(lines) < 5 {
		t.Fatalf("the control: the prose wraps:\n%s", strings.Join(lines, "\n"))
	}
	if w := Width(lines[0]); w > 30 {
		t.Errorf("the box is %d wide in a room of 30, its first column wider than the wrapped text in it:\n%s", w, strings.Join(lines, "\n"))
	}
}

// A box in a cell is fitted to the room its parent's row leaves it,
// at most its own Width (roomOf): at an indent it stays in the room.
func TestBoxInAnIndentedCell(t *testing.T) {
	t.Parallel()
	box := boxOf(&Grid{Width: 30, Rows: []Row{{Cells: []Cell{{Text: "key"}, {Text: "some prose that wraps here and there again", Fit: WrapWords}}}}})
	g := &Grid{Width: 30, Rows: []Row{{Indent: 4, Cells: []Cell{{Grid: box}}}}}
	for _, l := range g.Lines() {
		if Width(l) > 30 {
			t.Errorf("a line of %d columns in a room of 30:\n%s", Width(l), strings.Join(g.Lines(), "\n"))
			break
		}
	}
}

// Boxes sharing their Widths draw their columns at one width, a span row
// wider than the columns in one widening the last column of all.
func TestBoxesSharingWidthsUnderASpan(t *testing.T) {
	t.Parallel()
	w := NewWidths()
	one := boxOf(&Grid{Widths: w, Rows: []Row{{Cells: Cells("k", "v")}, {Cells: []Cell{{Text: "a span row wider than the columns", Span: true}}}}})
	two := boxOf(&Grid{Widths: w, Rows: []Row{{Cells: Cells("key2", "value two")}}})
	g := &Grid{Rows: []Row{{Cells: []Cell{{Grid: one}}}, {Cells: []Cell{{Grid: two}}}}}
	square(t, "two boxes sharing their columns", g.Lines())
}

// A box leaves out a column empty in every row, with its line, as a grid
// without borders leaves it out with its gap: a list's state when no row
// has one, a release's author when none says who made it.
func TestBoxLeavesOutEmptyColumns(t *testing.T) {
	t.Parallel()
	g := &Grid{Border: Box, Widths: NewWidths().Right(2)}
	g.Rows = []Row{
		{Header: true, Cells: Cells("ID", "WHO", "SIZE")},
		{Cells: Cells("1", "", "4 MB", "")},
		{Cells: Cells("2", "", "40 MB", "")},
	}
	// Without borders the empty fourth column is left out (the header
	// fills WHO).
	plain := "ID  WHO   SIZE\n1         4 MB\n2        40 MB\n"
	g.Border = NoBorder
	if got := g.String(); got != plain {
		t.Fatalf("the control, without borders:\n%s\nwant:\n%s", got, plain)
	}
	g.Border = Box
	want := "╭────┬─────┬───────╮\n" +
		"│ ID │ WHO │  SIZE │\n" +
		"├────┼─────┼───────┤\n" +
		"│ 1  │     │  4 MB │\n" +
		"│ 2  │     │ 40 MB │\n" +
		"╰────┴─────┴───────╯\n"
	if got := g.String(); got != want {
		t.Errorf("a box drew a column empty in every row:\n%s\nwant:\n%s", got, want)
	}
	// A middle column empty in every row (--releases' who, none recorded),
	// left out with borders as without.
	m := &Grid{Rows: []Row{{Cells: Cells("* 1", "", "build")}, {Cells: Cells("  2", "", "image")}}}
	if got := m.String(); got != "* 1  build\n  2  image\n" {
		t.Fatalf("the control, without borders:\n%q", got)
	}
	m.Border = Box
	want = "╭─────┬───────╮\n" +
		"│ * 1 │ build │\n" +
		"│   2 │ image │\n" +
		"╰─────┴───────╯\n"
	if got := m.String(); got != want {
		t.Errorf("a box drew a middle column empty in every row:\n%s\nwant:\n%s", got, want)
	}
}

// prose is prose long enough to wrap in any room these tests give.
const prose = "one two three four five six seven eight nine ten eleven twelve thirteen fourteen"

func wrapped(s string) Cell { return Cell{Text: s, Fit: WrapWords} }

// inRoom fails when a line of lines is wider than room.
func inRoom(t *testing.T, name string, lines []string, room int) {
	t.Helper()
	for _, l := range lines {
		if Width(l) > room {
			t.Errorf("%s: a line of %d columns in a room of %d:\n%s", name, Width(l), room, strings.Join(lines, "\n"))
			return
		}
	}
}

// Rows whose last cells sit in different columns, both prose: the box's
// right side holds.
func TestBoxLastCellsInTwoColumns(t *testing.T) {
	t.Parallel()
	for name, rows := range map[string][]Row{
		"the short row first": {
			{Cells: []Cell{text("k"), wrapped(prose)}},
			{Cells: []Cell{text("k"), text("v"), wrapped(prose)}},
		},
		"the short row last": {
			{Cells: []Cell{text("k"), text("v"), wrapped(prose)}},
			{Cells: []Cell{text("k"), wrapped(prose)}},
		},
		"a row of one cell over a key and its prose": {
			{Indent: 4, Cells: []Cell{wrapped(prose)}},
			{Indent: 2, Cells: []Cell{text("k"), wrapped(prose)}},
		},
	} {
		square(t, name, boxOf(&Grid{Width: 40, Rows: rows}).Lines())
	}
}

// A row's last shown cell before a trailing empty one is fitted in the
// first phase as it is drawn: the box stays in its room.
func TestBoxTrailingEmptyCellInItsRoom(t *testing.T) {
	t.Parallel()
	g := boxOf(&Grid{Width: 40, Rows: []Row{
		{Header: true, Cells: Cells("a", "b", "c")},
		{Cells: []Cell{text("x"), wrapped(prose), text("")}},
	}})
	lines := g.Lines()
	if len(lines) < 6 {
		t.Fatalf("the control: the prose wraps:\n%s", strings.Join(lines, "\n"))
	}
	inRoom(t, "prose before a trailing empty cell", lines, 40)
}

// A box with a fixed column keeps its right side, under a span row wider
// than its columns and beside a cell wider than the fixed width.
func TestBoxFixedColumnKeepsItsSide(t *testing.T) {
	t.Parallel()
	square(t, "a span row over a fixed last column", boxOf(&Grid{Widths: NewWidths().Fixed(1, 3), Rows: []Row{
		{Cells: []Cell{{Text: "a long title of the box", Span: true}}},
		{Cells: Cells("k", "v")},
	}}).Lines())
	square(t, "a cell wider than its fixed column", boxOf(&Grid{Widths: NewWidths().Fixed(0, 3), Rows: []Row{
		{Cells: Cells("abcdefgh", "v")},
		{Cells: Cells("a", "v")},
	}}).Lines())
}

// An empty row in a box whose other rows are span rows is as wide as the
// box.
func TestBoxEmptyRowUnderSpanRows(t *testing.T) {
	t.Parallel()
	square(t, "an empty row under a span row", boxOf(&Grid{Rows: []Row{
		{Cells: []Cell{{Text: "a title of the box", Span: true}}},
		{},
	}}).Lines())
}

// A box in a cell is fitted to the room its row leaves it as renderRow
// lays the row out: after a span cell, after a cell wider than its fixed
// column, and moved to a line of its own.
func TestBoxFittedWhereItsRowPutsIt(t *testing.T) {
	t.Parallel()
	box := func() *Grid {
		return boxOf(&Grid{Rows: []Row{{Cells: []Cell{text("a"), wrapped(prose)}}}})
	}
	for name, g := range map[string]*Grid{
		"after a span cell": {Width: 40, Rows: []Row{
			{Cells: []Cell{{Text: "a spanning title", Span: true}, {Grid: box()}}},
		}},
		"after a cell wider than its fixed column": {Width: 40, Widths: NewWidths().Fixed(0, 3), Rows: []Row{
			{Cells: []Cell{text("wider than three"), {Grid: box()}}},
		}},
	} {
		inRoom(t, name, g.Lines(), 40)
	}
	// Moved to a line of its own, the box has the whole room: 40 columns
	// hold it (the control: alone in a grid of 40 it is 40 wide or less).
	alone := (&Grid{Width: 40, Rows: []Row{{Cells: []Cell{{Grid: box()}}}}}).Lines()
	inRoom(t, "the control, a box alone", alone, 40)
	moved := (&Grid{Width: 40, Rows: []Row{
		{Cells: []Cell{text("a first cell that is wider than the room it has"), {Grid: box(), Fit: MoveLast}}},
	}}).Lines()
	if len(moved) < 2 || !strings.HasPrefix(moved[1], "╭") {
		t.Fatalf("the control: the box moves to a line of its own:\n%s", strings.Join(moved, "\n"))
	}
	inRoom(t, "a box moved to its own line", moved[1:], 40)
}

// A grid with a Width of its own, in a cell, takes the columns it is
// drawn in: the row's next cell follows it at its gap.
func TestNestedGridMeasuredAtItsOwnWidth(t *testing.T) {
	t.Parallel()
	for name, sub := range map[string]*Grid{
		"plain": {Width: 20, Rows: []Row{{Cells: []Cell{wrapped(prose)}}}},
		"a box": boxOf(&Grid{Width: 20, Rows: []Row{{Cells: []Cell{wrapped(prose)}}}}),
	} {
		g := &Grid{Rows: []Row{{Cells: []Cell{{Grid: sub}, text("next")}}}}
		lines := g.Lines()
		if len(lines) < 3 {
			t.Fatalf("%s: the control: the nested grid wraps in its width:\n%s", name, strings.Join(lines, "\n"))
		}
		at := Width(lines[0][:strings.Index(lines[0], "next")])
		if at > 20+DefaultGap {
			t.Errorf("%s: the next cell starts at column %d, after a grid 20 wide at most:\n%s", name, at, strings.Join(lines, "\n"))
		}
	}
}

// A box stays in its room: in another box's cell, sharing its columns
// with a box that has more room, and under a span row of prose.
func TestBoxStaysInItsRoom(t *testing.T) {
	t.Parallel()
	shared := NewWidths()
	sharing := func() *Grid {
		return boxOf(&Grid{Rows: []Row{{Widths: shared, Cells: []Cell{text("k"), wrapped(prose)}}}})
	}
	for _, c := range []struct {
		name string
		room int
		g    *Grid
	}{
		{"a box in a box's cell", 40, boxOf(&Grid{Width: 40, Rows: []Row{
			{Cells: []Cell{text("k"), {Grid: boxOf(&Grid{Rows: []Row{{Cells: []Cell{text("a"), wrapped(prose)}}}})}}},
		}})},
		{"boxes sharing columns in different rooms", 50, &Grid{Width: 50, Rows: []Row{
			{Cells: []Cell{{Grid: sharing()}}},
			{Indent: 10, Cells: []Cell{{Grid: sharing()}}},
		}}},
		{"a span row of prose", 40, boxOf(&Grid{Width: 40, Rows: []Row{
			{Cells: []Cell{{Text: prose, Span: true, Fit: WrapWords}}},
			{Cells: Cells("k", "v")},
		}})},
	} {
		inRoom(t, c.name, c.g.Lines(), c.room)
	}
}

// A span cell after a key in a box runs free of the columns: it widens
// none for the other rows, and no column line crosses it.
func TestBoxSpanCellAfterAKey(t *testing.T) {
	t.Parallel()
	g := boxOf(&Grid{Rows: []Row{
		{Cells: Cells("k", "v", "z")},
		{Cells: []Cell{text("k"), {Text: "a spanning cell here", Span: true}, text("after")}},
	}})
	lines := g.Lines()
	square(t, "a span cell after a key", lines)
	if len(lines) != 4 {
		t.Fatalf("the box's lines:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(lines[1], "│ k │ v │ z") {
		t.Errorf("a span cell widened its column for another row:\n%s", strings.Join(lines, "\n"))
	}
	if n := strings.Count(lines[2], boxSide); n != 3 {
		t.Errorf("%d lines in a row with a span cell after its key, want its sides and the key's line:\n%s", n, strings.Join(lines, "\n"))
	}
}

// A span cell after an empty key starts where the box's value column
// does, as it does without borders.
func TestBoxSpanStartsAtItsColumn(t *testing.T) {
	t.Parallel()
	g := &Grid{Rows: []Row{
		{Cells: Cells("key", "value")},
		{Cells: []Cell{text(""), {Text: "span", Span: true}}},
	}}
	plain := g.Lines()
	at := func(line, word string) int { return Width(line[:strings.Index(line, word)]) }
	if at(plain[0], "value") != at(plain[1], "span") {
		t.Fatalf("the control, without borders:\n%s", strings.Join(plain, "\n"))
	}
	g.Border = Box
	lines := g.Lines()
	if len(lines) != 5 {
		t.Fatalf("the box's lines:\n%s", strings.Join(lines, "\n"))
	}
	if v, s := at(lines[1], "value"), at(lines[3], "span"); v != s {
		t.Errorf("the span starts at column %d, the value column at %d:\n%s", s, v, strings.Join(lines, "\n"))
	}
}

// A cell after a span cell shows at its gap whatever the other rows hold,
// between the span and the row's last, as without the span it would.
func TestCellAfterASpanAlwaysShows(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		g    *Grid
		want string
	}{
		{"before the row's last", &Grid{Rows: []Row{
			{Cells: []Cell{{Text: "a"}, {Text: "x", Span: true}, {Text: "mid"}, {Text: "z"}}},
		}}, "a  x  mid  z\n"},
		{"before a trailing empty cell", &Grid{Rows: []Row{
			{Cells: []Cell{{Text: "a"}, {Text: "x", Span: true}, {Text: "mid"}, {Text: "end"}, {}}},
		}}, "a  x  mid  end\n"},
	} {
		if got := c.g.String(); got != c.want {
			t.Errorf("%s: a cell after a span is missing:\n%q\nwant:\n%q", c.name, got, c.want)
		}
	}
}

// What runs to a box's side widens its last column before its rows are
// fitted to the room left: a row whose last cell sits in an earlier
// column keeps the room it fits in.
func TestBoxSpanPhaseKeepsTheRowsFitted(t *testing.T) {
	t.Parallel()
	box := func(span string) *Grid {
		return boxOf(&Grid{Width: 40, MinRoom: 10, Rows: []Row{
			{Cells: []Cell{wrapped(prose)}},
			{Cells: Cells("k", "v")},
			{Cells: []Cell{text(""), {Text: span, Span: true}}},
		}})
	}
	square(t, "a span of 30 after a row of prose", box("a span of thirty columns here.").Lines())
	inRoom(t, "a span of 20 after a row of prose", box("a span of twenty col").Lines(), 40)
}

// A grid's room is taken until it moves no more: the third grid in a row
// is measured in the room it draws in, the cell after it at its gap.
func TestThirdGridInARowMeasuredWhereItDraws(t *testing.T) {
	t.Parallel()
	row := func() Row {
		nine := "aaaaaaaaa bbbbbbbbb ccccccccc ddddddddd eeeeeeeee fffffffff ggggggggg"
		a := &Grid{Width: 20, Rows: []Row{{Cells: []Cell{wrapped(prose)}}}}
		b := &Grid{Rows: []Row{{Cells: []Cell{wrapped(nine)}}}}
		c := &Grid{Rows: []Row{{Cells: []Cell{wrapped("aa bb cc dd")}}}}
		return Row{Cells: []Cell{{Grid: a}, {Grid: b}, {Grid: c}, text("end")}}
	}
	alone := (&Grid{Width: 70, Rows: []Row{row()}}).Lines()
	withBox := (&Grid{Width: 70, Rows: []Row{row(), {Cells: []Cell{{Grid: boxOf(&Grid{Rows: []Row{{Cells: Cells("x")}}})}}}}}).Lines()
	if !strings.HasSuffix(withBox[0], "aa bb  end") {
		t.Fatalf("the control: with a box in the tree, end follows the third grid at its gap:\n%s", strings.Join(withBox, "\n"))
	}
	if alone[0] != withBox[0] {
		t.Errorf("the row lays out otherwise without a box in the tree:\n%s\nwith one:\n%s", alone[0], withBox[0])
	}
}

// A row whose first cell spans runs across the box, a rule around it so
// the column lines above and below join.
func TestBoxRuleAroundARowStartingWithASpan(t *testing.T) {
	t.Parallel()
	g := boxOf(&Grid{Rows: []Row{
		{Cells: Cells("key", "value")},
		{Cells: []Cell{{Text: "a title", Span: true}, text("x")}},
		{Cells: Cells("k", "v")},
	}})
	lines := g.Lines()
	if len(lines) != 7 || !strings.HasPrefix(lines[2], "├") || !strings.HasPrefix(lines[4], "├") {
		t.Errorf("no rule around a row starting with a span, as around a span row:\n%s", strings.Join(lines, "\n"))
	}
}

// A grid in a box's cell is measured in the room the box lays it in: the
// cell after it in it sits at its gap.
func TestBoxLastCellGridMeasuredWhereItDraws(t *testing.T) {
	t.Parallel()
	inner := &Grid{Rows: []Row{{Cells: []Cell{wrapped(prose)}}}}
	cell := &Grid{Rows: []Row{{Cells: []Cell{{Grid: inner}, text("x")}}}}
	g := boxOf(&Grid{Width: 60, Rows: []Row{
		{Cells: []Cell{text("k"), {Grid: cell}}},
		{Cells: []Cell{text("k"), text("v"), text("zzzzzzzzzzzzzzzzzzzz")}},
	}})
	lines := g.Lines()
	// The prose's lines as drawn in the box's second column (the first row's
	// lines, between the top and the second row), and where the x after it
	// starts.
	widest, at := 0, -1
	for _, l := range lines[1 : len(lines)-2] {
		parts := strings.Split(l, boxSide)
		if len(parts) < 4 {
			continue
		}
		col := strings.TrimPrefix(parts[2], " ")
		if strings.HasSuffix(strings.TrimRight(col, " "), " x") {
			at = strings.Index(col, " x") + 1
			col = strings.TrimRight(col, " x")
		}
		widest = max(widest, Width(strings.TrimRight(col, " ")))
	}
	if at < 0 || widest == 0 {
		t.Fatalf("the control: the prose and its x in the box:\n%s", strings.Join(lines, "\n"))
	}
	if at != widest+DefaultGap {
		t.Errorf("x starts at %d in its cell, the prose before it %d wide:\n%s", at, widest, strings.Join(lines, "\n"))
	}
}

// squareLines fails unless every line of a box is as wide as its first.
func squareLines(t *testing.T, lines []string) {
	t.Helper()
	for _, l := range lines {
		if Width(l) != Width(lines[0]) {
			t.Fatalf("the box is not square:\n%s", strings.Join(lines, "\n"))
		}
	}
}

// linesInRoom fails when a line is wider than room.
func linesInRoom(t *testing.T, lines []string, room int) {
	t.Helper()
	if n := linesWidth(lines); n > room {
		t.Fatalf("%d columns in a room of %d:\n%s", n, room, strings.Join(lines, "\n"))
	}
}

// Prose as the last cell of rows in two columns of a box: the earlier
// runs to the side, the box is measured in one pass, and its lines join.
func TestBoxFittingTwoColumnsSettles(t *testing.T) {
	t.Parallel()
	g := &Grid{Border: Box, Width: 70, Rows: []Row{
		{Cells: []Cell{{Text: "w7xxxx w8x w9xxx w10xxxxxx w11xxxxxx w12xxxx", Fit: WrapWords}}},
		{Cells: []Cell{{Text: "w14"}, {Text: "w15 w16xx", Fit: WrapWords}}},
		{Cells: []Cell{{Text: "w17"}, {}, {Text: "w19xxxxxxx w20 w21xxx w22xxxxx w23xxxxx w24xxxx w25xx", Fit: WrapWords}}},
	}}
	squareLines(t, g.Lines())
}

// Prose as the last cell of rows in two columns of a box: the box stays in
// a room it fits in.
func TestBoxFittingTwoColumnsStaysInItsRoom(t *testing.T) {
	t.Parallel()
	rows := func() []Row {
		return []Row{
			{Cells: []Cell{{Text: "w3xx w4xxxxx", Fit: WrapWords}}},
			{Cells: []Cell{{Text: "w8"}, {Text: "w9xxxxx", Prefix: "| "}, {Text: "w10xxxxxxx w11 w12x", Fit: WrapWords}}},
		}
	}
	linesInRoom(t, (&Grid{Border: Box, Width: 40, Rows: rows()}).Lines(), 40)
	linesInRoom(t, (&Grid{Border: Box, Width: 48, Rows: rows()}).Lines(), 48)
}

// Boxes sharing columns, in rooms of their own, are fitted and drawn in
// the tightest: each is square and in its room.
func TestBoxesSharingWidthsInTwoRooms(t *testing.T) {
	t.Parallel()
	shared := NewWidths()
	prose := "w16 w17 w18 w19 w20 w21 w22 w23 w24 w25 w26 w27 w28"
	box := func() *Grid {
		return &Grid{Border: Box, Widths: shared, Rows: []Row{{Cells: []Cell{{Text: prose, Fit: WrapWords}}}}}
	}
	wide, tight := box(), box()
	g := &Grid{Width: 60, Rows: []Row{
		{Widths: NewWidths(), Cells: []Cell{{Grid: wide}}},
		{Widths: NewWidths(), Cells: []Cell{{Text: "a key some twenty wide"}, {Grid: tight}}},
	}}
	lines := g.Lines()
	squareLines(t, lines[:3])
}

// A span cell where no row of the box holds anything starts a column of
// its own, which widens to hold it inside the box.
func TestBoxSpanCellPastItsColumns(t *testing.T) {
	t.Parallel()
	g := &Grid{Border: Box, Rows: []Row{
		{Cells: []Cell{{Text: "w1"}, {Text: "w6", Span: true}}},
	}}
	squareLines(t, g.Lines())
	g = &Grid{Border: Box, Rows: []Row{
		{Cells: []Cell{{Text: "w1"}}},
		{Cells: []Cell{{}, {Text: "w8", Span: true}}},
	}}
	squareLines(t, g.Lines())
}

// A box measures the grids two levels under it as they draw once it is
// fitted: its columns hold them.
func TestBoxMeasuresGridsUnderItAsDrawn(t *testing.T) {
	t.Parallel()
	g := &Grid{Widths: NewWidths().Fixed(0, 5), Border: Box, Width: 40, Rows: []Row{
		{Cells: []Cell{{Text: "w1xxxxxx"}, {Grid: &Grid{Rows: []Row{
			{Cells: []Cell{{Grid: &Grid{MinRoom: 12, Rows: []Row{
				{Indent: 1, Cells: []Cell{{Text: "w3xxx w4xxx"}, {Text: "w5xxxx w6xxxxxx", Fit: WrapWords}}},
			}}}, {Text: "w13"}}},
		}}}}},
	}}
	squareLines(t, g.Lines())
}

// A grid rendered again in the same room gives the same lines: a render
// forgets the rooms and widths an earlier one took.
func TestRenderAgainWithGridsThreeDeep(t *testing.T) {
	t.Parallel()
	for room := 0; room <= 120; room++ {
		w1, w2 := NewWidths(), NewWidths()
		g := &Grid{Width: room, Rows: []Row{
			{Cells: []Cell{{Grid: &Grid{Widths: w1, Rows: []Row{
				{Cells: []Cell{{Grid: &Grid{Widths: w2, Rows: []Row{
					{Cells: []Cell{{Text: "   w4xxxxxxx w5xxx"}}},
				}}}, {Text: "w7xx w8xxx w9xxxx w10xxxxxx w11xxx"}}},
			}}}}},
			{Cells: []Cell{{Grid: &Grid{Widths: w1, Rows: []Row{
				{Cells: []Cell{{Text: "w78x"}, {Text: "w79xxxx w80x"}, {Grid: &Grid{Widths: w2, Rows: []Row{
					{Cells: []Cell{{Text: "w81"}, {Text: "w83x w84x w85xxxxxx w86xxxx", Fit: WrapWords}}},
				}}}, {Fit: MoveLast}}},
			}}}}},
		}}
		first := g.String()
		if again := g.String(); again != first {
			t.Fatalf("in a room of %d, first:\n%sagain:\n%s", room, first, again)
		}
	}
}

// MoveLast counts a right aligned cell at its padded width: a row that
// does not fit so moves its last cell.
func TestMoveLastCountsARightCellPadded(t *testing.T) {
	t.Parallel()
	g := &Grid{Widths: NewWidths().Right(0), Width: 28, Rows: []Row{
		{Cells: []Cell{{Text: "w1xxxxxx w2xxxx"}}},
		{Cells: []Cell{{Text: "w3xx"}, {Text: "  w5 w6 w7xx", Fit: MoveLast}}},
	}}
	linesInRoom(t, g.Lines(), 28)
}

// MoveLast judges the line its last cell is on after an Overflow: a last
// cell that fits there stays.
func TestMoveLastAfterAnOverflow(t *testing.T) {
	t.Parallel()
	rows := func() []Row {
		return []Row{{Cells: []Cell{{Text: "w1"}, {Text: "w2xxxxx"}, {Text: "w3xxxxxx w4xx w5xxxxxx w6xxx", Fit: MoveLast}}}}
	}
	widths := func() *Widths { return NewWidths().Fixed(1, 3).Overflow(1) }
	free := (&Grid{Widths: widths(), Rows: rows()}).String()
	linesInRoom(t, strings.Split(free, "\n"), 40)
	if got := (&Grid{Widths: widths(), Width: 40, Rows: rows()}).String(); got != free {
		t.Fatalf("in a room of 40:\n%swith no room:\n%s", got, free)
	}
}

// An empty row's underline is no line of spaces.
func TestUnderlineOfAnEmptyIndentedRow(t *testing.T) {
	t.Parallel()
	g := &Grid{Rows: []Row{{Indent: 2, Underline: true}}}
	for _, l := range g.Lines() {
		if l != strings.TrimRight(l, " ") {
			t.Fatalf("a line ends in spaces: %q", g.Lines())
		}
	}
}

// A box widens for a line it cannot wrap up to its MaxWidth, and no
// further: a longer line runs out past its side, alone, every other line
// of the box at that width.
func TestBoxMaxWidth(t *testing.T) {
	t.Parallel()
	long := "$ docker rmi " + strings.Repeat("an-image-name ", 4)
	box := func(most int) []string {
		return boxOf(&Grid{Width: 30, MaxWidth: most, Rows: []Row{
			{Cells: Cells("k", "v")},
			{Cells: Cells("key", long)},
		}}).Lines()
	}
	wide := box(100)
	squareLines(t, wide)
	if !strings.HasSuffix(wide[2], strings.TrimSpace(long)+" "+boxSide) {
		t.Errorf("a box with room to widen did not hold the line:\n%s", strings.Join(wide, "\n"))
	}
	narrow := box(30)
	for i, l := range narrow {
		if strings.Contains(l, "docker rmi") {
			if Width(l) <= 30 || strings.HasSuffix(l, boxSide) {
				t.Errorf("the line past MaxWidth does not run past the side: %q", l)
			}
		} else if Width(l) != 30 {
			t.Errorf("line %d is %d wide, not the box's 30:\n%s", i, Width(l), strings.Join(narrow, "\n"))
		}
	}
}

// A rule at a Break leaves its Open columns blank between the lines that
// go on through them, so a key reads as one cell across the rows of its
// values; a rule the box needs anyway (under a header) stays whole, and
// one with every column open is none.
func TestBoxOpenRule(t *testing.T) {
	t.Parallel()
	g := &Grid{Border: Box, Rows: []Row{
		{Cells: Cells("KEY", "VALUE", "N"), Header: true},
		{Cells: Cells("shop", "uid 1", "1"), Break: true, Open: 1},
		{Cells: Cells("  prod", "released", "2"), Break: true, Open: 1},
		{Cells: Cells("", "more", "3"), Break: true, Open: 2},
		{Cells: Cells("", "all open", "4"), Break: true, Open: 3},
		{Cells: Cells("blog", "uid 2", "5"), Break: true},
	}}
	want := `╭────────┬──────────┬───╮
│ KEY    │ VALUE    │ N │
├────────┼──────────┼───┤
│ shop   │ uid 1    │ 1 │
│        ├──────────┼───┤
│   prod │ released │ 2 │
│        │          ├───┤
│        │ more     │ 3 │
│        │ all open │ 4 │
├────────┼──────────┼───┤
│ blog   │ uid 2    │ 5 │
╰────────┴──────────┴───╯
`
	if got := g.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
