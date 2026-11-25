package layout

import (
	"strings"
	"testing"
)

// Review 3's tests: each fails on a bug still in the grid, named in its
// comment.

// r3Square fails unless every line of a box is as wide as its first.
func r3Square(t *testing.T, lines []string) {
	t.Helper()
	for _, l := range lines {
		if Width(l) != Width(lines[0]) {
			t.Fatalf("the box is not square:\n%s", strings.Join(lines, "\n"))
		}
	}
}

// r3InRoom fails when a line is wider than room.
func r3InRoom(t *testing.T, lines []string, room int) {
	t.Helper()
	if n := linesWidth(lines); n > room {
		t.Fatalf("%d columns in a room of %d:\n%s", n, room, strings.Join(lines, "\n"))
	}
}

// Bug: fitBoxes takes every column's width from the same pass, so two
// columns that each hold a row's last cell trade their room back and
// forth (one wraps as the other unwraps) until the passes run out, and
// the box is drawn with widths its cells no longer take: a cell runs
// through its column's line and the box's side.
func TestBoxFittingTwoColumnsSettles(t *testing.T) {
	t.Parallel()
	g := &Grid{Border: Box, Width: 70, Rows: []Row{
		{Cells: []Cell{{Text: "w7xxxx w8x w9xxx w10xxxxxx w11xxxxxx w12xxxx", Fit: WrapWords}}},
		{Cells: []Cell{{Text: "w14"}, {Text: "w15 w16xx", Fit: WrapWords}}},
		{Cells: []Cell{{Text: "w17"}, {}, {Text: "w19xxxxxxx w20 w21xxx w22xxxxx w23xxxxx w24xxxx w25xx", Fit: WrapWords}}},
	}}
	r3Square(t, g.Lines())
}

// Bug: the same trade, stopped on the wide pass: a box that fits a room
// of 40 is 50 columns in a room of 48, its first column 12 wide around
// text wrapped to 7.
func TestBoxFittingTwoColumnsStaysInItsRoom(t *testing.T) {
	t.Parallel()
	rows := func() []Row {
		return []Row{
			{Cells: []Cell{{Text: "w3xx w4xxxxx", Fit: WrapWords}}},
			{Cells: []Cell{{Text: "w8"}, {Text: "w9xxxxx", Prefix: "| "}, {Text: "w10xxxxxxx w11 w12x", Fit: WrapWords}}},
		}
	}
	r3InRoom(t, (&Grid{Border: Box, Width: 40, Rows: rows()}).Lines(), 40)
	r3InRoom(t, (&Grid{Border: Box, Width: 48, Rows: rows()}).Lines(), 48)
}

// Bug: boxes sharing columns are fitted to the tightest room among them
// (fitBoxes' room), but each is drawn in its own (renderBox(g.fit)): the
// box with more room leaves unwrapped the cell its column was measured
// wrapped, and it runs through the box's side.
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
	r3Square(t, lines[:3])
}

// Bug: a span cell whose column no row of the box holds anything in
// starts past the box's last column (boxShape.column is len(cols)), and
// widening that column to make it room moves its start as far: it is
// written outside the box's side.
func TestBoxSpanCellPastItsColumns(t *testing.T) {
	t.Parallel()
	g := &Grid{Border: Box, Rows: []Row{
		{Cells: []Cell{{Text: "w1"}, {Text: "w6", Span: true}}},
	}}
	r3Square(t, g.Lines())
	g = &Grid{Border: Box, Rows: []Row{
		{Cells: []Cell{{Text: "w1"}}},
		{Cells: []Cell{{}, {Text: "w8", Span: true}}},
	}}
	r3Square(t, g.Lines())
}

// Bug: fitBoxes lays a box's cell out with the widths prepare took of
// the grids under it before the box was fitted (their natural, their
// parents' nest), when its fixed column had not grown: the grid two
// levels down was measured wrapped, is drawn whole 3 columns further
// (under its MinRoom), and the cell after it is written over its end in
// the measure and after it in the box, 2 columns past the side.
func TestBoxMeasuresGridsUnderItAsDrawn(t *testing.T) {
	t.Parallel()
	g := &Grid{Widths: NewWidths().Fixed(0, 5), Border: Box, Width: 40, Rows: []Row{
		{Cells: []Cell{{Text: "w1xxxxxx"}, {Grid: &Grid{Rows: []Row{
			{Cells: []Cell{{Grid: &Grid{MinRoom: 12, Rows: []Row{
				{Indent: 1, Cells: []Cell{{Text: "w3xxx w4xxx"}, {Text: "w5xxxx w6xxxxxx", Fit: WrapWords}}},
			}}}, {Text: "w13"}}},
		}}}}},
	}}
	r3Square(t, g.Lines())
}

// Bug: prepare forgets the columns' widths of an earlier render but not
// the grids' (natural, fit), and its first pass places rows by them: a
// grid rendered again is laid out from what the render before left, so
// the same grid in the same room draws two ways.
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

// Bug: a right aligned cell is padded to its column wherever it sits,
// but a row that may move its last cell is tested against the room with
// its cells unpadded: the row neither moves its last cell nor drops the
// padding, and is 29 columns in a room of 28 that held it at 17.
func TestMoveLastCountsARightCellPadded(t *testing.T) {
	t.Parallel()
	g := &Grid{Widths: NewWidths().Right(0), Width: 28, Rows: []Row{
		{Cells: []Cell{{Text: "w1xxxxxx w2xxxx"}}},
		{Cells: []Cell{{Text: "w3xx"}, {Text: "  w5 w6 w7xx", Fit: MoveLast}}},
	}}
	r3InRoom(t, g.Lines(), 28)
}

// Bug: a row that may move its last cell is measured as one line, though
// a cell over its fixed column with Overflow already sends the last to
// the next line at its column, where it fits: in a room of 40 that the
// row's 37 columns fit, the last cell loses its column for the origin.
func TestMoveLastAfterAnOverflow(t *testing.T) {
	t.Parallel()
	rows := func() []Row {
		return []Row{{Cells: []Cell{{Text: "w1"}, {Text: "w2xxxxx"}, {Text: "w3xxxxxx w4xx w5xxxxxx w6xxx", Fit: MoveLast}}}}
	}
	widths := func() *Widths { return NewWidths().Fixed(1, 3).Overflow(1) }
	free := (&Grid{Widths: widths(), Rows: rows()}).String()
	r3InRoom(t, strings.Split(free, "\n"), 40)
	if got := (&Grid{Widths: widths(), Width: 40, Rows: rows()}).String(); got != free {
		t.Fatalf("in a room of 40:\n%swith no room:\n%s", got, free)
	}
}

// Bug: an empty row with an indent and Underline draws a line of the
// indent's spaces under it: the one line the grid does not trim.
func TestUnderlineOfAnEmptyIndentedRow(t *testing.T) {
	t.Parallel()
	g := &Grid{Rows: []Row{{Indent: 2, Underline: true}}}
	for _, l := range g.Lines() {
		if l != strings.TrimRight(l, " ") {
			t.Fatalf("a line ends in spaces: %q", g.Lines())
		}
	}
}
