package layout

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// reportGen makes reports at random as --borders draws them: a plain root
// whose rows each hold a box (a section), the boxes sharing one Widths or
// not, each with its own number of columns, or one box alone. It makes what
// randomGen does not: rows that stop short of the last column, headers with
// more cells than the rows under them, boxes sharing a Widths one of which
// holds a column the other never does, span cells at the last column, a
// grid as the cell a row runs to the side from, and grids in the last column
// of boxes sharing their columns.
type reportGen struct {
	rnd   *rand.Rand
	n     int
	words []string
	desc  []string
	// flat makes text cells only; lastGrids a grid only as a row's last cell,
	// its own rows with every cell, so every cell but a row's last is as wide
	// as its text and a room can be said to fit them.
	flat, lastGrids bool
}

func (g *reportGen) p(x float64) bool { return g.rnd.Float64() < x }

func (g *reportGen) word() string {
	g.n++
	w := "w" + strconv.Itoa(g.n) + strings.Repeat("x", g.rnd.Intn(4))
	g.words = append(g.words, w)
	return w
}

func (g *reportGen) text(long bool) string {
	n := 1 + g.rnd.Intn(2)
	if long {
		n = 6 + g.rnd.Intn(10)
		if g.flat {
			n += 30
		}
	}
	ws := make([]string, n)
	for i := range ws {
		ws[i] = g.word()
	}
	return strings.Join(ws, " ")
}

func (g *reportGen) widths(cols int) (*Widths, string) {
	w := NewWidths()
	var d []string
	for i := 0; i < cols; i++ {
		if g.p(0.3) {
			w.Right(i)
			d = append(d, fmt.Sprintf("Right(%d)", i))
		}
		if g.p(0.1) {
			n := 2 + g.rnd.Intn(10)
			w.Min(i, n)
			d = append(d, fmt.Sprintf("Min(%d,%d)", i, n))
		} else if g.p(0.08) {
			n := 2 + g.rnd.Intn(10)
			w.Fixed(i, n)
			d = append(d, fmt.Sprintf("Fixed(%d,%d)", i, n))
			if g.p(0.5) {
				w.Overflow(i)
				d = append(d, fmt.Sprintf("Overflow(%d)", i))
			}
		}
		if g.p(0.1) {
			n := 1 + g.rnd.Intn(3)
			w.Gap(i, n)
			d = append(d, fmt.Sprintf("Gap(%d,%d)", i, n))
		}
	}
	return w, strings.Join(d, ".")
}

// sub is a small grid for a cell, a box at times (it draws none in a box's
// cell).
func (g *reportGen) sub(d int, pad string) *Grid {
	cols := 1 + g.rnd.Intn(3)
	w, ws := g.widths(cols)
	out := &Grid{Widths: w}
	h := "Grid{W=" + ws
	if g.p(0.25) {
		out.Border = Box
		h += " Box"
	}
	g.desc = append(g.desc, pad+h)
	g.rows(out, nil, cols, d, pad, g.lastGrids)
	return out
}

// rows fills out with rows of up to cols cells, every row with cols of them
// and none empty when uniform.
func (g *reportGen) rows(out *Grid, w *Widths, cols, d int, pad string, uniform bool) {
	n := 1 + g.rnd.Intn(4)
	for i := 0; i < n; i++ {
		r := Row{Widths: w}
		rd := ""
		nc := 1 + g.rnd.Intn(cols)
		if uniform {
			nc = cols
		}
		if i == 0 && g.p(0.4) {
			r.Header = true
			rd += "Header "
			// A header with more cells than the rows under it.
			if g.p(0.5) && !uniform {
				nc = cols + g.rnd.Intn(2)
			}
		}
		if g.p(0.1) {
			r.Break = true
			rd += "Break "
			if g.p(0.4) {
				r.Open = 1 + g.rnd.Intn(3)
				rd += fmt.Sprintf("Open=%d ", r.Open)
			}
		}
		if g.p(0.08) {
			r.Indent = 1 + g.rnd.Intn(3)
			rd += fmt.Sprintf("Indent=%d ", r.Indent)
		}
		g.desc = append(g.desc, pad+"  Row{"+rd)
		span := -1
		if g.p(0.25) {
			span = g.rnd.Intn(nc)
			if g.p(0.5) {
				span = nc - 1
			}
		}
		for k := 0; k < nc; k++ {
			c := Cell{}
			last := k == nc-1
			cd := ""
			switch {
			case g.p(0.1) && !uniform:
				cd = "empty"
			case !g.flat && d < 2 && (!g.lastGrids && g.p(0.12) || last && g.p(0.2)):
				g.desc = append(g.desc, pad+"    Cell{grid:")
				c.Grid = g.sub(d+1, pad+"      ")
				cd = "the grid"
			case last && g.p(0.45):
				c.Text, c.Fit = g.text(g.p(0.7)), WrapWords
				cd = strconv.Quote(c.Text) + " WrapWords"
			default:
				c.Text = g.text(false)
				cd = strconv.Quote(c.Text)
			}
			if k == span {
				c.Span = true
				cd += " Span"
			}
			if last && c.Fit == Keep && g.p(0.08) {
				c.Fit = MoveLast
				cd += " MoveLast"
			}
			g.desc = append(g.desc, pad+"    Cell{"+cd+"}")
			r.Cells = append(r.Cells, c)
		}
		out.Rows = append(out.Rows, r)
	}
}

func reportReport(seed int64, flat bool) (*Grid, *reportGen) {
	g := &reportGen{rnd: rand.New(rand.NewSource(seed)), lastGrids: seed%2 == 0, flat: flat}
	root := &Grid{}
	shared, ss := g.widths(4)
	g.desc = append(g.desc, "shared="+ss)
	if g.p(0.25) {
		root.Border, root.Widths = Box, shared
		g.desc = append(g.desc, "Box W=shared")
		g.rows(root, nil, 1+g.rnd.Intn(4), 0, "", false)
		return root, g
	}
	n := 1 + g.rnd.Intn(3)
	for i := 0; i < n; i++ {
		box := &Grid{Border: Box}
		cols := 1 + g.rnd.Intn(4)
		var w *Widths
		h := fmt.Sprintf("Section Box of %d columns ", cols)
		if g.p(0.7) {
			w = shared
			h += "W=shared"
		} else {
			var s string
			box.Widths, s = g.widths(cols)
			h += "W=" + s
		}
		g.desc = append(g.desc, h)
		g.rows(box, w, cols, 1, "", false)
		if g.p(0.3) {
			root.Rows = append(root.Rows, Row{Cells: []Cell{{Text: g.word(), Span: true}}})
		}
		root.Rows = append(root.Rows, Row{Cells: []Cell{{Grid: box}}})
	}
	return root, g
}

// reportBoxes are the boxes of a report: its root, or those in its cells.
func reportBoxes(root *Grid) []*Grid {
	if root.Border == Box {
		return []*Grid{root}
	}
	var boxes []*Grid
	for _, r := range root.Rows {
		for _, c := range r.Cells {
			if c.Grid != nil {
				boxes = append(boxes, c.Grid)
			}
		}
	}
	return boxes
}

// reportNeed is a room every cell of a flat report fits: each column at its
// widest cell that is no prose, the lines between, a row's own cells that are
// no prose side by side after them, and the longest word of prose.
func reportNeed(root *Grid) int {
	cols := map[*column]int{}
	row, word := 0, 0
	for _, b := range reportBoxes(root) {
		for _, r := range b.Rows {
			w := b.widths(r)
			own := r.Indent
			for k, c := range r.Cells {
				col := w.col(k)
				cols[col] = max(cols[col], col.min, col.fixed, 1)
				if c.Fit == WrapWords {
					for _, f := range strings.Fields(c.Text) {
						word = max(word, Width(f))
					}
					continue
				}
				n := Width(c.Text) + indentOf(r, k)
				cols[col] = max(cols[col], n)
				own += n + max(w.gap(k), 3)
			}
			row = max(row, own)
		}
	}
	need := 4 + row + word
	for _, n := range cols {
		need += n + 3
	}
	return need
}

// reportDrawn says what in a flat report's lines is not as its boxes'
// columns say: the cells of one column start at one place, or end at one in
// a right-aligned column (a cell its row runs to the side from, and those
// after it, are in no column), and a rule stands between a row across the
// box and a row of columns.
func reportDrawn(root *Grid, lines []string) string {
	type place struct {
		x    int
		text string
	}
	at := map[*column]place{}
	lineOf := func(text string) (int, int) {
		for i, l := range lines {
			if j := strings.Index(l, " "+text+" "); j >= 0 {
				return i, Width(l[:j]) + 1
			}
		}
		return -1, -1
	}
	for _, b := range reportBoxes(root) {
		lastCol := b.shapeOf().lastCol
		prev, prevLine := -1, -1
		for i, r := range b.Rows {
			w := b.widths(r)
			rf := runsFrom(r, lastCol)
			first := -1
			for k, c := range r.Cells {
				if c.Text == "" {
					continue
				}
				text := c.Text
				if c.Fit == WrapWords {
					text = strings.Fields(c.Text)[0]
				}
				y, x := lineOf(text)
				if y < 0 {
					return fmt.Sprintf("the cell %q is on no line", text)
				}
				if first < 0 {
					first = y
				}
				col := w.col(k)
				if k >= rf || k == 0 && r.Indent > 0 || col.align == Right && c.Fit == WrapWords {
					continue
				}
				if col.align == Right {
					x += Width(text)
				}
				if old, ok := at[col]; ok && old.x != x {
					return fmt.Sprintf("column %d: %q at %d, %q at %d", k, old.text, old.x, text, x)
				}
				at[col] = place{x, text}
			}
			if first < 0 {
				continue
			}
			if prev >= 0 && across(b.Rows[prev], lastCol) != across(r, lastCol) {
				rule := false
				for _, l := range lines[prevLine:first] {
					rule = rule || strings.HasPrefix(l, "├")
				}
				if !rule {
					return fmt.Sprintf("no rule between the rows of lines %d and %d, one across the box and one of columns", prevLine, first)
				}
			}
			prev, prevLine = i, first
		}
	}
	return ""
}

// Flat reports in boxes, at random: in a room every cell fits no line is
// past it, wider or not; each column's cells start at one place, or end at
// one in a right-aligned column; a rule stands where the box goes from a
// row across it to columns or back.
func TestFlatBoxesInARoomTheirCellsFit(t *testing.T) {
	t.Parallel()
	for seed := range 2500 {
		root, gen := reportReport(int64(seed), true)
		need := reportNeed(root)
		for _, room := range []int{0, need, need + 7, need + 40} {
			root.Width = room
			lines := root.Lines()
			msg := reportDrawn(root, lines)
			if got := linesWidth(lines); msg == "" && room > 0 && got > room {
				msg = fmt.Sprintf("%d columns", got)
			}
			if msg != "" {
				t.Fatalf("seed %d, a room of %d: %s\n%s\n%s", seed, room, msg, strings.Join(gen.desc, "\n"), strings.Join(lines, "\n"))
			}
		}
	}
}

// Reports in boxes with grids in their cells, at random, hold the contract
// random grids do (contractBreak), and where a grid is only a row's last
// cell, what fits one room fits every room as wide as it draws there.
func TestBoxesFitEveryRoomTheyFitOne(t *testing.T) {
	t.Parallel()
	rooms := []int{0, 150, 100, 80, 60, 40, 20}
	for seed := range 250 {
		root, gen := reportReport(int64(seed), false)
		fail := func(room int, msg string) {
			t.Helper()
			root.Width = room
			out := root.String()
			if len(out) > 6000 {
				out = "(long)"
			}
			t.Fatalf("seed %d, a room of %d: %s\n%s\n%s", seed, room, msg, strings.Join(gen.desc, "\n"), out)
		}
		words := &randomGen{rnd: rand.New(rand.NewSource(int64(seed))), words: gen.words}
		widths := map[int]int{}
		for _, room := range rooms {
			if msg := contractBreak(root, words, room); msg != "" {
				fail(room, msg)
			}
			root.Width = room
			widths[room] = linesWidth(root.Lines())
		}
		if !gen.lastGrids {
			continue
		}
		best, at := widths[150], 150
		for room := 195; room >= 5; room -= 10 {
			root.Width = room
			if n := linesWidth(root.Lines()); n < best {
				best, at = n, room
			}
		}
		for _, room := range rooms[1:] {
			if best <= room && widths[room] > room {
				fail(room, fmt.Sprintf("%d columns, yet %d in a room of %d", widths[room], best, at))
			}
		}
	}
}

// A span cell wider than its column's fixed width does not end its line
// with Overflow: it is in no column, and the cells after it follow at their
// gap.
func TestSpanCellEndsNoLine(t *testing.T) {
	t.Parallel()
	g := &Grid{Widths: NewWidths().Fixed(0, 4).Overflow(0), Rows: []Row{
		{Cells: []Cell{{Text: "a span cell", Span: true}, {Text: "after"}}},
		{Cells: Cells("a cell in it", "under")},
	}}
	want := "a span cell  after\na cell in it\n      under\n"
	if got := g.String(); got != want {
		t.Errorf("got:\n%swant:\n%s", got, want)
	}
}

// In a box, a row of several cells that stops short of the box's last
// column keeps them in their columns, a right-aligned one at its right and
// the column's line after it, as without borders.
func TestBoxShortRowKeepsItsColumn(t *testing.T) {
	t.Parallel()
	rows := func() []Row {
		return []Row{
			{Header: true, Cells: Cells("NAME", "SIZE", "NOTE")},
			{Cells: Cells("app", "12")},
			{Cells: Cells("database", "1200", "big")},
			{Cells: Cells("cache", "7", "")},
		}
	}
	end := func(lines []string, word string) int {
		for _, l := range lines {
			if i := strings.Index(l, word+" "); i >= 0 {
				return Width(l[:i+len(word)])
			}
		}
		for _, l := range lines {
			if strings.HasSuffix(l, word) {
				return Width(l)
			}
		}
		t.Fatalf("no %q in:\n%s", word, strings.Join(lines, "\n"))
		return 0
	}
	plain := (&Grid{Widths: NewWidths().Right(1), Rows: rows()}).Lines()
	if end(plain, "12") != end(plain, "1200") {
		t.Fatalf("without borders 12 is not right-aligned over 1200:\n%s", strings.Join(plain, "\n"))
	}
	boxed := (&Grid{Border: Box, Widths: NewWidths().Right(1), Rows: rows()}).Lines()
	if end(boxed, "12") != end(boxed, "1200") || end(boxed, "7") != end(boxed, "1200") {
		t.Errorf("in a box 12, its row two cells of the header's three, is not right-aligned over 1200:\n%s", strings.Join(boxed, "\n"))
	}
	if strings.Count(boxed[3], boxSide) != strings.Count(boxed[4], boxSide) {
		t.Errorf("in a box the row of two cells has no column line after its second:\n%s", strings.Join(boxed, "\n"))
	}
}

// A box aligns a grid in a right-aligned column as one block, its own
// columns kept, as without borders.
func TestBoxGridInARightAlignedColumnKeepsItsColumns(t *testing.T) {
	t.Parallel()
	g := &Grid{Border: Box, Widths: NewWidths().Right(0), Rows: []Row{
		{Cells: []Cell{{Grid: &Grid{Rows: []Row{{Cells: Cells("ab", "cd")}, {Cells: Cells("x")}}}}, {Text: "after"}}},
		{Cells: Cells("a cell wider than the grid", "after")},
	}}
	lines := g.Lines()
	if len(lines) != 5 || strings.Index(lines[1], "ab") != strings.Index(lines[2], "x") {
		t.Errorf("the grid's first column starts at two columns:\n%s", strings.Join(lines, "\n"))
	}
}

// An empty span cell at a row's end draws no column in a box, as an empty
// cell does.
func TestBoxEmptySpanCellDrawsNoColumn(t *testing.T) {
	t.Parallel()
	box := func(last Cell) string {
		return (&Grid{Border: Box, Rows: []Row{
			{Cells: Cells("a", "b")},
			{Cells: []Cell{{Text: "c"}, {Text: "d"}, last}},
		}}).String()
	}
	if got, want := box(Cell{Span: true}), box(Cell{}); got != want {
		t.Errorf("with an empty span cell after d:\n%swith an empty cell:\n%s", got, want)
	}
}

// A row's fit is its last cell's, an empty one too: prose before an empty
// last cell that says WrapWords wraps, in a box and without.
func TestEmptyLastCellSaysTheRowWraps(t *testing.T) {
	t.Parallel()
	prose := "some prose of several words, wider than the room it has"
	moved := (&Grid{Width: 30, Rows: []Row{{Cells: []Cell{{Text: "key"}, {Text: prose}, {Fit: MoveLast}}}}}).Lines()
	if len(moved) != 2 {
		t.Fatalf("an empty last cell's MoveLast moves the cell before it no more:\n%s", strings.Join(moved, "\n"))
	}
	for _, border := range []Border{NoBorder, Box} {
		lines := (&Grid{Border: border, Width: 30, Rows: []Row{{Cells: []Cell{{Text: "key"}, {Text: prose}, {Fit: WrapWords}}}}}).Lines()
		if n := linesWidth(lines); n > 30 {
			t.Errorf("border %d: %d columns in a room of 30, the prose not wrapped:\n%s", border, n, strings.Join(lines, "\n"))
		}
	}
}
