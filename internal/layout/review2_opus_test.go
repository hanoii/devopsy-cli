package layout

import "testing"

// BUG (renderRow's shownCell, grid.go): a cell after a span cell is in no
// column (measured), yet renderRow shows it only when its column is shown
// (w.shown) or it is the row's last; a column only cells after a span hold
// is never measured, so such a cell is dropped from its line without a
// word, where Cell.Span says "the cells after it follow at their gap". The
// round 1 fix stopped measuring them and left the visibility test asking
// the column.
//
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
			t.Errorf("%s: a cell after a span was dropped:\n%q\nwant:\n%q", c.name, got, c.want)
		}
	}
}
