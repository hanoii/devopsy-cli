package layout

import (
	"strings"
	"testing"
)

// fableBox is g drawn in a box.
func fableBox(g *Grid) *Grid { g.Border = Box; return g }

// fableSquare fails unless every line of a box is as wide as its first:
// a box whose lines differ has a side that does not join.
func fableSquare(t *testing.T, name string, lines []string) {
	t.Helper()
	for _, l := range lines {
		if Width(l) != Width(lines[0]) || strings.Contains(l, "\n") {
			t.Errorf("%s: a line of the box is not as wide as its first:\n%s", name, strings.Join(lines, "\n"))
			return
		}
	}
}

// Bug: a box draws a column that is empty in every row (boxColumns
// counts cells, never Widths.shown), where a grid without borders leaves
// it out with its gap: --releases --borders with no release in a state,
// and --snapshots --borders with no snapshot in one (its header has five
// cells, its rows six), end in an empty column.
func TestFableBoxLeavesOutAnEmptyColumn(t *testing.T) {
	t.Parallel()
	for name, g := range map[string]*Grid{
		"in the middle": {Rows: []Row{{Cells: Cells("a", "", "c")}, {Cells: Cells("bbb", "", "d")}}},
		"at the end":    {Rows: []Row{{Header: true, Cells: Cells("ID", "SIZE")}, {Cells: Cells("a", "1", "")}, {Cells: Cells("bbb", "2", "")}}},
	} {
		plain := strings.Join(g.Lines(), "\n")
		if strings.Contains(plain, "a      c") || strings.HasSuffix(strings.Split(plain, "\n")[0], " ") {
			t.Fatalf("%s: the control: without borders the empty column is left out:\n%s", name, plain)
		}
		got := strings.Join(fableBox(g).Lines(), "\n")
		if strings.Contains(got, "│  │") || strings.Contains(got, "┬──┬") || strings.Contains(got, "┬──╮") {
			t.Errorf("%s: the box draws a column empty in every row:\n%s", name, got)
		}
	}
}

// Bug: a cell after a Span cell is measured into its column (prepare
// skips only the Span cell itself), though Cell.Span says the cells
// after it "follow at their gap, in no column": a long one widens that
// column for every other row.
func TestFableCellsAfterASpanWidenNoColumn(t *testing.T) {
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

// Bug: MoveLast's sum of the row's shown cells leaves out a Span cell
// before the last (own counts a cell only when it is the last or no
// Span), so a row wider than the room keeps its last cell beside the
// others.
func TestFableMoveLastCountsASpanBeforeTheLast(t *testing.T) {
	t.Parallel()
	g := &Grid{Width: 20, Rows: []Row{
		{Cells: []Cell{{Text: "key"}, {Text: "a-middle-span-cell", Span: true}, {Text: "last", Fit: MoveLast}}},
	}}
	lines := g.Lines()
	if len(lines) != 2 || lines[1] != "last" {
		t.Errorf("a row of 29 columns in a room of 20 kept its last cell: %q", lines)
	}
}

// Bug: a span row of a box is written as one line whatever its text
// holds: a text of two lines breaks the box's side.
func TestFableBoxSpanRowOfSeveralLines(t *testing.T) {
	t.Parallel()
	g := fableBox(&Grid{Rows: []Row{{Cells: Cells("a", "b")}, {Cells: []Cell{{Text: "one\ntwo", Span: true}}}}})
	fableSquare(t, "a span row of two lines", g.Lines())
}

// Bug: a span row of a box with an indent, or with more than one cell,
// is written from its plain text (spanText): its cell's Paint never runs.
func TestFableBoxSpanRowKeepsItsCellPaint(t *testing.T) {
	t.Parallel()
	paint := func(s string) string { return "\x1b[1m" + s + "\x1b[0m" }
	g := fableBox(&Grid{Rows: []Row{{Cells: Cells("a", "b")}, {Indent: 2, Cells: []Cell{{Text: "title", Span: true, Paint: paint}}}}})
	if got := strings.Join(g.Lines(), "\n"); !strings.Contains(got, paint("title")) {
		t.Errorf("an indented span row lost its paint:\n%q", got)
	}
}

// Bug: fitBorders measures a row's last cell fitted only when it sits in
// the box's last column; boxRow fits the last cell of every row. A row
// with fewer cells than the box has columns is so measured whole and
// drawn wrapped: its column is as wide as the text unwrapped, and the
// box runs past its room around a cell that was wrapped to fit it.
func TestFableBoxMeasuresAShortRowAsItDrawsIt(t *testing.T) {
	t.Parallel()
	g := fableBox(&Grid{Width: 30, Rows: []Row{
		{Cells: []Cell{{Text: "a long prose line that wraps in the box", Fit: WrapWords}}},
		{Cells: Cells("k2", "v")},
	}})
	lines := g.Lines()
	fableSquare(t, "a short row", lines)
	if len(lines) < 5 {
		t.Fatalf("the control: the prose wraps:\n%s", strings.Join(lines, "\n"))
	}
	if w := Width(lines[0]); w > 30 {
		t.Errorf("the box is %d wide in a room of 30, its first column wider than the wrapped text in it:\n%s", w, strings.Join(lines, "\n"))
	}
}

// Bug: a box in a cell is fitted to its own Width (fitBorders) and drawn
// in what its parent's row leaves it (render): at an indent the two
// differ, the text wraps to the smaller room and the column keeps the
// larger one's width, so the box runs past the room.
func TestFableBoxInAnIndentedCellStaysInTheRoom(t *testing.T) {
	t.Parallel()
	box := fableBox(&Grid{Width: 30, Rows: []Row{{Cells: []Cell{{Text: "key"}, {Text: "some prose that wraps here and there again", Fit: WrapWords}}}}})
	g := &Grid{Width: 30, Rows: []Row{{Indent: 4, Cells: []Cell{{Grid: box}}}}}
	for _, l := range g.Lines() {
		if Width(l) > 30 {
			t.Errorf("a line of %d columns in a room of 30:\n%s", Width(l), strings.Join(g.Lines(), "\n"))
			break
		}
	}
}

// Bug: boxes sharing their Widths are to draw their columns at one width
// (fitBorders), but a span row wider than the columns widens its own
// box's last column only (renderBox): the boxes of one report end at
// different columns.
func TestFableBoxesSharingWidthsAreOneWidth(t *testing.T) {
	t.Parallel()
	w := NewWidths()
	one := fableBox(&Grid{Widths: w, Rows: []Row{{Cells: Cells("k", "v")}, {Cells: []Cell{{Text: "a span row wider than the columns", Span: true}}}}})
	two := fableBox(&Grid{Widths: w, Rows: []Row{{Cells: Cells("key2", "value two")}}})
	g := &Grid{Rows: []Row{{Cells: []Cell{{Grid: one}}}, {Cells: []Cell{{Grid: two}}}}}
	fableSquare(t, "two boxes sharing their columns", g.Lines())
}
