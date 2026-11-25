package layout

import (
	"strings"
	"testing"
)

// fableProse is prose long enough to wrap in any room these tests give.
const fableProse = "one two three four five six seven eight nine ten eleven twelve thirteen fourteen"

func fableWrap(s string) Cell { return Cell{Text: s, Fit: WrapWords} }

// fableInRoom fails when a line of lines is wider than room.
func fableInRoom(t *testing.T, name string, lines []string, room int) {
	t.Helper()
	for _, l := range lines {
		if Width(l) > room {
			t.Errorf("%s: a line of %d columns in a room of %d:\n%s", name, Width(l), room, strings.Join(lines, "\n"))
			return
		}
	}
}

// BUG (fitBoxes, border.go): phase 2 lays every row out in the shape taken
// before it (s := g.shapeOf() once per box), so a row whose last cell sits
// in one column is fitted against the unfitted width of a column another
// row's last cell fits; the box is then drawn in another shape than it was
// measured in: a cell wider than its column pushes its side out, and prose
// goes one word a line.
//
// Rows whose last cells sit in different columns, both prose: the box's
// right side holds.
func TestBoxLastCellsInTwoColumns(t *testing.T) {
	t.Parallel()
	for name, rows := range map[string][]Row{
		"the short row first": {
			{Cells: []Cell{text("k"), fableWrap(fableProse)}},
			{Cells: []Cell{text("k"), text("v"), fableWrap(fableProse)}},
		},
		"the short row last": {
			{Cells: []Cell{text("k"), text("v"), fableWrap(fableProse)}},
			{Cells: []Cell{text("k"), fableWrap(fableProse)}},
		},
		"a row of one cell over a key and its prose": {
			{Indent: 4, Cells: []Cell{fableWrap(fableProse)}},
			{Indent: 2, Cells: []Cell{text("k"), fableWrap(fableProse)}},
		},
	} {
		square(t, name, boxOf(&Grid{Width: 40, Rows: rows}).Lines())
	}
}

// BUG (fitBoxes' first phase, border.go): it takes a row's last cell for
// its positional last (len(r.Cells)-1) where cells takes the last that
// holds anything, so prose before a trailing empty cell is measured whole,
// unwrapped, into its column, and the box runs past its room with the
// prose wrapped inside a column as wide as its whole text.
//
// A row's last shown cell before a trailing empty one is fitted in the
// first phase as it is drawn: the box stays in its room.
func TestBoxTrailingEmptyCellInItsRoom(t *testing.T) {
	t.Parallel()
	g := boxOf(&Grid{Width: 40, Rows: []Row{
		{Header: true, Cells: Cells("a", "b", "c")},
		{Cells: []Cell{text("x"), fableWrap(fableProse), text("")}},
	}})
	lines := g.Lines()
	if len(lines) < 6 {
		t.Fatalf("the control: the prose wraps:\n%s", strings.Join(lines, "\n"))
	}
	fableInRoom(t, "prose before a trailing empty cell", lines, 40)
}

// BUG (shapeOf and fitBoxes' third phase, border.go): a fixed column takes
// no fitted width, so a span row wider than the columns cannot widen a
// fixed last column, and cells pads a cell wider than its fixed column to
// nothing: either row's right side is pushed out of the box.
//
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

// BUG (boxRow, border.go): a row that is no span row in a box with no
// columns (its other rows all span rows) is drawn between sides two spaces
// apart, whatever the box's inner width (renderBox takes it from the span
// rows): the plan's "a row with no cells is an empty line (in a box, its
// cells empty and padded)".
//
// An empty row in a box whose other rows are span rows is as wide as the
// box.
func TestBoxEmptyRowUnderSpanRows(t *testing.T) {
	t.Parallel()
	square(t, "an empty row under a span row", boxOf(&Grid{Rows: []Row{
		{Cells: []Cell{{Text: "a title of the box", Span: true}}},
		{},
	}}).Lines())
}

// BUG (roomsBelow, grid.go): it says it lays a row out as renderRow does,
// and does not: it skips a span cell's width, takes a cell wider than its
// fixed column for the column's width, and gives a moved last cell
// (MoveLast) the room after the cells it left; a box under any of them is
// fitted (fitBoxes) to another room than it is drawn in, its columns as
// wide as the wrong room and its prose wrapped to the right one.
//
// A box in a cell is fitted to the room its row leaves it as renderRow
// lays the row out: after a span cell, after a cell wider than its fixed
// column, and moved to a line of its own.
func TestBoxFittedWhereItsRowPutsIt(t *testing.T) {
	t.Parallel()
	box := func() *Grid {
		return boxOf(&Grid{Rows: []Row{{Cells: []Cell{text("a"), fableWrap(fableProse)}}}})
	}
	for name, g := range map[string]*Grid{
		"after a span cell": {Width: 40, Rows: []Row{
			{Cells: []Cell{{Text: "a spanning title", Span: true}, {Grid: box()}}},
		}},
		"after a cell wider than its fixed column": {Width: 40, Widths: NewWidths().Fixed(0, 3), Rows: []Row{
			{Cells: []Cell{text("wider than three"), {Grid: box()}}},
		}},
	} {
		fableInRoom(t, name, g.Lines(), 40)
	}
	// Moved to a line of its own, the box has the whole room: 40 columns
	// hold it (the control: alone in a grid of 40 it is 40 wide or less).
	alone := (&Grid{Width: 40, Rows: []Row{{Cells: []Cell{{Grid: box()}}}}}).Lines()
	fableInRoom(t, "the control, a box alone", alone, 40)
	moved := (&Grid{Width: 40, Rows: []Row{
		{Cells: []Cell{text("a first cell that is wider than the room it has"), {Grid: box(), Fit: MoveLast}}},
	}}).Lines()
	if len(moved) < 2 || !strings.HasPrefix(moved[1], "╭") {
		t.Fatalf("the control: the box moves to a line of its own:\n%s", strings.Join(moved, "\n"))
	}
	fableInRoom(t, "a box moved to its own line", moved[1:], 40)
}

// BUG (prepare, grid.go): a nested grid is measured into its parent's
// column at its natural width (render(-1)), then drawn in its own Width
// (roomOf), so the column is as wide as the grid's unwrapped text and the
// row's next cell sits that far right of a grid drawn narrower.
//
// A grid with a Width of its own, in a cell, takes the columns it is
// drawn in: the row's next cell follows it at its gap.
func TestNestedGridMeasuredAtItsOwnWidth(t *testing.T) {
	t.Parallel()
	for name, sub := range map[string]*Grid{
		"plain": {Width: 20, Rows: []Row{{Cells: []Cell{fableWrap(fableProse)}}}},
		"a box": boxOf(&Grid{Width: 20, Rows: []Row{{Cells: []Cell{fableWrap(fableProse)}}}}),
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

// BUG (roomsBelow, grid.go, and fitBoxes): a box in a box's cell is
// fitted with no room (roomsBelow gives it its own Width only) and drawn in
// its cell's (cells passes the room on), so its prose wraps to the room
// while its columns, and the box around it, stay as wide as the prose
// whole; boxes sharing their Widths in cells with different rooms draw the
// shared column as wide as the roomier one fits it, the other past its
// room; and a span row is laid out with no room at all (renderRow(r, -1)),
// its prose never wrapped.
//
// A box stays in its room: in another box's cell, sharing its columns
// with a box that has more room, and under a span row of prose.
func TestBoxStaysInItsRoom(t *testing.T) {
	t.Parallel()
	shared := NewWidths()
	sharing := func() *Grid {
		return boxOf(&Grid{Rows: []Row{{Widths: shared, Cells: []Cell{text("k"), fableWrap(fableProse)}}}})
	}
	for _, c := range []struct {
		name string
		room int
		g    *Grid
	}{
		{"a box in a box's cell", 40, boxOf(&Grid{Width: 40, Rows: []Row{
			{Cells: []Cell{text("k"), {Grid: boxOf(&Grid{Rows: []Row{{Cells: []Cell{text("a"), fableWrap(fableProse)}}}})}}},
		}})},
		{"boxes sharing columns in different rooms", 50, &Grid{Width: 50, Rows: []Row{
			{Cells: []Cell{{Grid: sharing()}}},
			{Indent: 10, Cells: []Cell{{Grid: sharing()}}},
		}}},
		{"a span row of prose", 40, boxOf(&Grid{Width: 40, Rows: []Row{
			{Cells: []Cell{{Text: fableProse, Span: true, Fit: WrapWords}}},
			{Cells: Cells("k", "v")},
		}})},
	} {
		fableInRoom(t, c.name, c.g.Lines(), c.room)
	}
}

// BUG (cells and fitBoxes, border.go): a box takes a span cell of a row
// that is no span row for a cell of its column: it is measured into the
// column (fitted), which widens for every row, and the cells after it are
// drawn in columns, where Cell.Span says "the cells after it follow at
// their gap, in no column" and the design that a span cell "runs from its
// column to the box's right edge with no column lines across it".
//
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

// BUG (boxRow, border.go): a span row is laid out as without borders
// (renderRow), where an empty cell before the span is padded to its
// column's width and gap, then put one padding after the box's side; the
// box's own columns start at their widths as fitted and a line between
// them, so the span starts a column left of the cells it sits under.
//
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
