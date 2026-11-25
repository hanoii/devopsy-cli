package layout

import (
	"strings"
	"testing"
)

// Review 3 (Opus): the grid and its boxes against their contract.

// Bug: boxes sharing Widths are fitted to the tightest room among them
// (fitBoxes), but each draws its rows in its own room (renderBox passes
// the box's fit to cells): the box with more room wraps its last cells
// wider than the columns it was fitted to, and its right side breaks.
// TestBoxStaysInItsRoom's case checks only that the lines fit the room.
func TestBoxesSharingWidthsDrawSquare(t *testing.T) {
	t.Parallel()
	shared := NewWidths()
	sharing := func() *Grid {
		return boxOf(&Grid{Rows: []Row{{Widths: shared, Cells: []Cell{text("k"), wrapped(prose)}}}})
	}
	a, b := sharing(), sharing()
	g := &Grid{Width: 50, Rows: []Row{
		{Cells: []Cell{{Grid: a}}},
		{Indent: 10, Cells: []Cell{{Grid: b}}},
	}}
	lines := g.Lines()
	var first, second []string
	for _, l := range lines {
		if strings.HasPrefix(l, " ") {
			second = append(second, strings.TrimLeft(l, " "))
		} else {
			first = append(first, l)
		}
	}
	if len(first) < 3 || len(second) < 3 {
		t.Fatalf("the control: two boxes:\n%s", strings.Join(lines, "\n"))
	}
	square(t, "the box with more room", first)
	square(t, "the box with less room", second)
}

// Bug: fitBoxes' loop does not settle when two rows' last cells sit in
// different columns, each the other's room: the widths swing between the
// cells whole and wrapped on every pass, the loop stops after its six
// passes on the whole ones, and the box runs past a room it fits in (here
// 135 columns in a room of 100, when each prose wrapped to 45 fits). With
// three such rows, it stops where the next pass would move a column, and
// the cells drawn are wider than their columns: the box's sides break.
func TestBoxFitSettlesInItsRoom(t *testing.T) {
	t.Parallel()
	p := "one two three four five six seven eight nine ten eleven twelve"
	g := boxOf(&Grid{Width: 100, Rows: []Row{
		{Cells: []Cell{text("k"), wrapped(p)}},
		{Cells: []Cell{text("k"), text("z"), wrapped(p)}},
	}})
	lines := g.Lines()
	square(t, "two last cells in two columns", lines)
	inRoom(t, "two last cells in two columns", lines, 100)
	square(t, "three last cells in three columns", boxOf(&Grid{Width: 25, Rows: []Row{
		{Cells: []Cell{wrapped("a bbbbbb ccc d ee fffffff ggg hhhhhh iiiii")}},
		{Cells: []Cell{text(""), text(""), wrapped("a bb c")}},
		{Cells: []Cell{text(""), wrapped("aa bbbb")}},
	}}).Lines())
}

// Bug: a span cell after every column the box draws (a row's own cells,
// then a span, as no other row goes further) starts after the last
// column's line; fitBoxes' span phase widens that last column, which
// moves the span right by as much, so it never fits, and the row runs
// past the box's side.
func TestBoxSpanAfterTheLastColumn(t *testing.T) {
	t.Parallel()
	for name, g := range map[string]*Grid{
		"a key and a span": boxOf(&Grid{Rows: []Row{
			{Cells: []Cell{text("k"), {Text: "span", Span: true}}},
		}}),
		"an empty span between cells": boxOf(&Grid{Rows: []Row{
			{Cells: []Cell{text("k"), {Span: true}, text("after")}},
		}}),
	} {
		square(t, name, g.Lines())
	}
}

// Bug: fitBoxes' span phase widens the last column after the loop that
// fitted the rows has ended, so a row whose last cell is in an earlier
// column is drawn with less room than it was fitted in: under its
// MinRoom it stays whole, wider than its column, and the box's side
// breaks; with a shorter span, the prose keeps the width it was fitted to
// beside the narrow column, and the box runs past a room it fits in (the
// prose wrapped to 13 beside the span's 20).
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

// Bug: prepare takes each grid's room twice (rooms), but a grid's room
// hangs on the widths of every grid before it in its row: the third grid
// of a row is measured at the room the second had before it was
// measured, wider than it draws, and the cell after it lands past its
// gap. A box anywhere else in the tree takes the rooms a third time and
// lays the row out right: the same row lays out two ways.
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

// Bug: a row whose first cell spans, with cells after it, is drawn as a
// span row (cells: no column lines across it) but is no SpanRow, so the
// box draws no rule above and below it: the column lines of the rows
// around it stop and start again with no junction.
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

// Bug: roomsBelow gives a grid in a box's cell its room from where the
// box's column starts to the box's side, while cells lays a row's last
// cell out in that room less the columns after it: a grid that is its
// row's last, in a column before the box's last, is measured wider than
// it draws, and what its own rows put after a grid in it lands past its
// gap. roomsBelow's box branch works the rooms out again rather than
// taking them from cells, as its plain branch takes them from place.
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
