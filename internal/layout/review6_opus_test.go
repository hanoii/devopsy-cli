package layout

import (
	"math/rand"
	"strings"
	"testing"
)

// Bug (review 6): a grid that is a row's last cell short of a box's last
// column takes its room from what the columns after it leave (lastRoom,
// after), and those columns' widths are fitted from this grid's own width
// (the start of the last column, MaxWidth's cap, what runs to the side):
// each pass feeds the next, the passes swing between two layouts, and
// prepare stops at its bound on whichever it reached. With MaxWidth 70 the
// box below is drawn 78 wide, its grid's column 64 wide around a grid
// wrapped to 23, its last column one wide. The documented limit is a Widths
// shared across depths; here every grid has its own.
func TestOpus6BoxWithAGridShortOfTheLastColumnSettles(t *testing.T) {
	t.Parallel()
	prose := "one two three four five six seven eight nine ten eleven twelve"
	long := "$ docker rmi " + strings.Repeat("an-image ", 8)
	g := &Grid{Border: Box, Width: 40, MaxWidth: 70, Rows: []Row{
		{Cells: []Cell{{Text: "k"}, {Grid: &Grid{Rows: []Row{{Cells: []Cell{{Text: prose, Fit: WrapWords}}}}}}}},
		{Cells: Cells("k2", "x", "y", long)},
	}}
	lines := g.Lines()
	if msg := settledBreak(g); msg != "" {
		t.Errorf("MaxWidth 70: %s\n%s", msg, strings.Join(lines, "\n"))
	}
	if w := Width(lines[0]); w > 70 {
		t.Errorf("a box drawn %d wide, past its MaxWidth 70, though its columns but the last take 13:\n%s", w, strings.Join(lines, "\n"))
	}
	// The same without MaxWidth, through a run (a span) and a grid short of
	// the last column: seed 1192 of the reports at random, past the 250
	// seeds TestBoxesFitEveryRoomTheyFitOne runs.
	root, gen := reportReport(1192, false)
	words := &randomGen{rnd: rand.New(rand.NewSource(1192)), words: gen.words}
	if msg := contractBreak(root, words, 100); msg != "" {
		root.Width = 100
		t.Errorf("seed 1192, a room of 100: %s\n%s", msg, root.String())
	}
}
