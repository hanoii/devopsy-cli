package layout

import "testing"

// Bug (review, opus): a box draws a column that is empty in every row, a
// trailing one as an empty "│  │" and a middle one as a blank column,
// where the same grid without borders leaves it out with its gap (the
// plan's "a column empty in every row of its Widths group ... is left out
// with its gap"). --snapshots --borders and --releases --borders hit it
// whenever no row has a state (the last cell is "" for a taken snapshot
// and a live or kept release), and --releases --borders when no release
// says who made it: boxColumns counts len(r.Cells) and never asks
// Widths.shown.
func TestReviewOpusBoxLeavesOutEmptyColumns(t *testing.T) {
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
		t.Errorf("a box draws a column empty in every row:\n%s\nwant:\n%s", got, want)
	}
	// A middle column empty in every row (--releases' who, none recorded):
	// left out without borders, drawn blank in a box.
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
		t.Errorf("a box draws a middle column empty in every row:\n%s\nwant:\n%s", got, want)
	}
}
