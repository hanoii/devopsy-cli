package layout

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Review 4: random grids with boxes against the contract (every box a
// rectangle whose lines join, every word shown, a second render alike,
// prepare ended on a pass that changed nothing, no room and a wide room
// alike), and the bugs it and a reading found, each a failing test that
// names it.

// r4opts are what a random grid may hold, each a probability.
type r4opts struct {
	box, nestBox, nest, span, prose, move, fixed, min, right, gap, overflow float64
	indent, prefix, brk, header, underline, multiline, shared, emptyCell    float64
	ownWidth, minRoom, long, rowWidths                                      float64
	maxDepth, maxCols, maxRows                                              int
	// What the random test leaves out, each for a bug a test below
	// names: a grid in a right-aligned column, MoveLast in a row holding a
	// grid, boxes sharing their Widths.
	rightGrid, moveGrid, boxShared bool
}

var r4all = r4opts{
	box: 0.6, nestBox: 0.3, nest: 0.15, span: 0.12, prose: 0.4, move: 0.1, fixed: 0.12, min: 0.12, right: 0.15, gap: 0.15, overflow: 0.5,
	indent: 0.12, prefix: 0.08, brk: 0.15, header: 0.1, underline: 0.05, multiline: 0.08, shared: 0.4, emptyCell: 0.15,
	ownWidth: 0.08, minRoom: 0.2, long: 0.2, rowWidths: 0,
	maxDepth: 2, maxCols: 4, maxRows: 4,
}

type r4gen struct {
	rnd   *rand.Rand
	o     r4opts
	n     int
	words []string
	pool  map[int]*Widths
	desc  []string
}

func (g *r4gen) p(x float64) bool { return g.rnd.Float64() < x }

func (g *r4gen) word() string {
	g.n++
	w := "w" + strconv.Itoa(g.n) + strings.Repeat("x", g.rnd.Intn(5))
	g.words = append(g.words, w)
	return w
}

func (g *r4gen) text() string {
	n := 1 + g.rnd.Intn(3)
	if g.p(g.o.long) {
		n = 5 + g.rnd.Intn(12)
	}
	ws := make([]string, n)
	for i := range ws {
		ws[i] = g.word()
	}
	s := strings.Join(ws, " ")
	if g.p(g.o.multiline) {
		s += "\n" + g.word()
	}
	return s
}

func (g *r4gen) newWidths(d int) (*Widths, string) {
	w := NewWidths()
	var desc []string
	for i := 0; i < g.o.maxCols; i++ {
		if g.p(g.o.fixed) {
			n := 2 + g.rnd.Intn(12)
			w.Fixed(i, n)
			desc = append(desc, fmt.Sprintf("Fixed(%d,%d)", i, n))
			if g.p(g.o.overflow) {
				w.Overflow(i)
				desc = append(desc, fmt.Sprintf("Overflow(%d)", i))
			}
		} else if g.p(g.o.min) {
			n := 2 + g.rnd.Intn(14)
			w.Min(i, n)
			desc = append(desc, fmt.Sprintf("Min(%d,%d)", i, n))
		}
		if g.p(g.o.right) {
			w.Right(i)
			desc = append(desc, fmt.Sprintf("Right(%d)", i))
		}
		if g.p(g.o.gap) {
			n := g.rnd.Intn(5)
			w.Gap(i, n)
			desc = append(desc, fmt.Sprintf("Gap(%d,%d)", i, n))
		}
	}
	return w, strings.Join(desc, ".")
}

func (g *r4gen) grid(d int, pad string) *Grid {
	out := &Grid{}
	head := "Grid{"
	if d == 0 && g.p(g.o.box) || d > 0 && g.p(g.o.nestBox) {
		out.Border = Box
		head += "Box "
	}
	if g.p(g.o.shared) && (g.o.boxShared || out.Border != Box) {
		if g.pool[d] == nil {
			w, s := g.newWidths(d)
			g.pool[d] = w
			g.desc = append(g.desc, fmt.Sprintf("pool[%d]=%s", d, s))
		}
		out.Widths = g.pool[d]
		head += fmt.Sprintf("W=pool[%d] ", d)
	} else {
		w, s := g.newWidths(d)
		out.Widths = w
		head += "W=" + s + " "
	}
	if d > 0 && g.p(g.o.ownWidth) {
		out.Width = 10 + g.rnd.Intn(40)
		head += fmt.Sprintf("Width=%d ", out.Width)
	}
	if g.p(g.o.minRoom) {
		out.MinRoom = 5 + g.rnd.Intn(20)
		head += fmt.Sprintf("MinRoom=%d ", out.MinRoom)
	}
	g.desc = append(g.desc, pad+head)
	rows := g.rnd.Intn(g.o.maxRows + 1)
	for i := 0; i < rows; i++ {
		r := Row{}
		rd := ""
		if g.p(g.o.indent) {
			r.Indent = 1 + g.rnd.Intn(4)
			rd += fmt.Sprintf("Indent=%d ", r.Indent)
		}
		if g.p(g.o.brk) {
			r.Break = true
			rd += "Break "
		}
		if g.p(g.o.header) {
			r.Header = true
			rd += "Header "
		}
		if g.p(g.o.underline) {
			r.Underline = true
			rd += "Underline "
		}
		if g.p(g.o.rowWidths) {
			w, s := g.newWidths(d)
			r.Widths = w
			rd += "RowW=" + s + " "
		}
		g.desc = append(g.desc, pad+"  Row{"+rd)
		n := g.rnd.Intn(g.o.maxCols + 1)
		rowGrid := false
		for k := 0; k < n; k++ {
			c := Cell{}
			cd := ""
			switch {
			case g.p(g.o.emptyCell):
				cd = "empty"
			case d < g.o.maxDepth && g.p(g.o.nest) && (g.o.rightGrid || out.Widths.col(k).align != Right):
				g.desc = append(g.desc, pad+"    Cell{grid:")
				c.Grid = g.grid(d+1, pad+"      ")
				rowGrid = true
			default:
				c.Text = g.text()
				cd = strconv.Quote(c.Text)
			}
			if g.p(g.o.span) {
				c.Span = true
				cd += " Span"
			}
			if c.Grid == nil && c.Text != "" && g.p(g.o.prefix) {
				c.Prefix = "| "
				cd += " Prefix"
			}
			if k == n-1 {
				switch {
				case g.p(g.o.prose):
					c.Fit = WrapWords
					cd += " WrapWords"
					if g.p(0.2) {
						c.Hang = 1 + g.rnd.Intn(4)
						cd += fmt.Sprintf(" Hang=%d", c.Hang)
					}
				case g.p(g.o.move) && (g.o.moveGrid || !rowGrid && c.Grid == nil):
					c.Fit = MoveLast
					cd += " MoveLast"
				}
			}
			r.Cells = append(r.Cells, c)
			g.desc = append(g.desc, pad+"    Cell{"+cd+"}")
		}
		out.Rows = append(out.Rows, r)
	}
	return out
}

func r4build(seed int64, o r4opts) (*Grid, *r4gen) {
	g := &r4gen{rnd: rand.New(rand.NewSource(seed)), o: o, pool: map[int]*Widths{}}
	root := g.grid(0, "")
	return root, g
}

// r4conn is the sides a box character joins: up, down, left, right.
var r4conn = map[rune][4]bool{
	'│': {true, true, false, false},
	'─': {false, false, true, true},
	'╭': {false, true, false, true},
	'╮': {false, true, true, false},
	'╰': {true, false, false, true},
	'╯': {true, false, true, false},
	'├': {true, true, false, true},
	'┤': {true, true, true, false},
	'┬': {false, true, true, true},
	'┴': {true, false, true, true},
	'┼': {true, true, true, true},
}

// r4joined says what in lines does not join: every box character meets
// one joining it back on each side it joins, and none on a side it does
// not.
func r4joined(lines []string) string {
	grid := make([][]rune, len(lines))
	for i, l := range lines {
		grid[i] = []rune(l)
	}
	at := func(y, x int) [4]bool {
		if y < 0 || y >= len(grid) || x < 0 || x >= len(grid[y]) {
			return [4]bool{}
		}
		return r4conn[grid[y][x]]
	}
	for y := range grid {
		for x, r := range grid[y] {
			c, ok := r4conn[r]
			if !ok {
				continue
			}
			type side struct {
				name   string
				dy, dx int
				me, it int
			}
			for _, s := range []side{{"up", -1, 0, 0, 1}, {"down", 1, 0, 1, 0}, {"left", 0, -1, 2, 3}, {"right", 0, 1, 3, 2}} {
				other := at(y+s.dy, x+s.dx)
				if r == '│' && s.dx == 0 {
					// A column line may end at a row running to the side.
					yy, xx := y+s.dy, x+s.dx
					if yy >= 0 && yy < len(grid) && xx < len(grid[yy]) {
						if _, box := r4conn[grid[yy][xx]]; !box {
							continue
						}
					}
				}
				if c[s.me] != other[s.it] {
					return fmt.Sprintf("line %d col %d: %c joins %s %v, what is there joins back %v", y, x, r, s.name, c[s.me], other[s.it])
				}
			}
		}
	}
	return ""
}

func r4check(root *Grid, gen *r4gen, room int) string {
	root.Width = room
	lines := root.Lines()
	if msg := r4joined(lines); msg != "" {
		return msg
	}
	all := strings.Join(lines, "\n")
	if root.Border == Box {
		for i, l := range lines {
			if Width(l) != Width(lines[0]) {
				return fmt.Sprintf("line %d not as wide as the first", i)
			}
		}
	}
	for _, w := range gen.words {
		if !strings.Contains(all, w) {
			return "word " + w + " missing"
		}
	}
	again := root.Lines()
	if strings.Join(again, "\n") != all {
		return "second render differs:\n" + strings.Join(again, "\n")
	}
	if msg := r4rects(lines); msg != "" {
		return msg
	}
	if msg := r4settled(root); msg != "" {
		return msg
	}
	if strings.Join(root.render(root.fit), "\n") != all {
		return "render after one more pass differs"
	}
	if room == 0 {
		root.Width = linesWidth(lines) + 10 + gen.rnd.Intn(3)
		wide := strings.Join(root.Lines(), "\n")
		if wide != all {
			a, b := strings.Split(all, "\n"), strings.Split(wide, "\n")
			for i := range a {
				if i >= len(b) || a[i] != b[i] {
					return fmt.Sprintf("in a room of %d, its width or more, it differs from no room at line %d", root.Width, i)
				}
			}
			return "in a wide room, more lines"
		}
	}
	return ""
}

// r4rects says which box in lines is no rectangle.
func r4rects(lines []string) string {
	grid := make([][]rune, len(lines))
	for i, l := range lines {
		grid[i] = []rune(l)
	}
	for y := range grid {
		for x, r := range grid[y] {
			if r != '╭' {
				continue
			}
			x2 := -1
			for j := x + 1; j < len(grid[y]); j++ {
				if grid[y][j] == '╮' {
					x2 = j
					break
				}
			}
			if x2 < 0 {
				return fmt.Sprintf("line %d col %d: a box with no right corner", y, x)
			}
			for yy := y + 1; ; yy++ {
				if yy >= len(grid) || x2 >= len(grid[yy]) {
					return fmt.Sprintf("line %d: the box from line %d col %d..%d has no side", yy, y, x, x2)
				}
				l, r := grid[yy][x], grid[yy][x2]
				if l == '╰' && r == '╯' {
					break
				}
				if !(l == '│' && r == '│' || l == '├' && r == '┤') {
					return fmt.Sprintf("line %d: the box from line %d col %d..%d has %c and %c for sides", yy, y, x, x2, l, r)
				}
			}
		}
	}
	return ""
}

// r4settled runs one more pass of prepare's loop over root as prepared
// and says what it changed.
func r4settled(root *Grid) string {
	byDepth := [][]*Grid{}
	var walk func(g *Grid, d int)
	walk = func(g *Grid, d int) {
		for len(byDepth) <= d {
			byDepth = append(byDepth, nil)
		}
		byDepth[d] = append(byDepth[d], g)
		for _, r := range g.Rows {
			for _, c := range r.Cells {
				if c.Grid != nil {
					walk(c.Grid, d+1)
				}
			}
		}
	}
	walk(root, 0)
	all := func(f func(g *Grid)) {
		for _, gs := range byDepth {
			for _, g := range gs {
				f(g)
			}
		}
	}
	state := func() []int {
		var out []int
		all(func(g *Grid) {
			out = append(out, g.fit, g.natural, g.boxRoom)
			for _, r := range g.Rows {
				for _, c := range g.widths(r).cols {
					out = append(out, c.fitted, c.nest)
				}
			}
		})
		return out
	}
	nested := func() {
		all(func(g *Grid) {
			for _, r := range g.Rows {
				for _, c := range g.widths(r).cols {
					c.nest = 0
				}
			}
		})
		for d := len(byDepth) - 1; d >= 0; d-- {
			for _, g := range byDepth[d] {
				for _, r := range g.Rows {
					w := g.widths(r)
					for k, c := range measured(r) {
						if c.Grid != nil {
							col := w.col(k)
							col.nest = max(col.nest, indentOf(r, k)+c.Grid.natural)
						}
					}
				}
			}
		}
	}
	var boxes []*Grid
	all(func(g *Grid) {
		if g.boxed() {
			boxes = append(boxes, g)
		}
	})
	before := state()
	for _, gs := range byDepth {
		for _, g := range gs {
			g.roomsBelow()
		}
	}
	for d := len(byDepth) - 1; d >= 1; d-- {
		for _, g := range byDepth[d] {
			g.natural = linesWidth(g.render(g.fit))
		}
		nested()
	}
	if len(boxes) > 0 {
		fitBoxes(boxes)
	}
	if after := state(); !equal(before, after) {
		return fmt.Sprintf("one more pass of prepare changes the layout: %v to %v", before, after)
	}
	return ""
}

// Random grids hold the contract, with what the bugs below break left
// out (r4opts): R4_OPT brings each back (rightGrid, moveGrid, boxShared),
// R4_N and R4_FROM say which seeds.
func TestRandomBoxesAgainstTheContract(t *testing.T) {
	t.Parallel()
	n, from := 3000, 0
	if s := os.Getenv("R4_N"); s != "" {
		n, _ = strconv.Atoi(s)
	}
	if s := os.Getenv("R4_FROM"); s != "" {
		from, _ = strconv.Atoi(s)
	}
	o := r4all
	for _, f := range strings.Fields(os.Getenv("R4_OPT")) {
		switch f {
		case "boxShared":
			o.boxShared = true
		case "moveGrid":
			o.moveGrid = true
		case "rightGrid":
			o.rightGrid = true
		case "deep":
			o.maxDepth, o.nest = 4, 0.3
		}
	}
	for seed := from; seed < from+n; seed++ {
		root, gen := r4build(int64(seed), o)
		for _, room := range []int{0, 120, gen.rnd.Intn(121), gen.rnd.Intn(60), gen.rnd.Intn(30)} {
			msg := func() (msg string) {
				defer func() {
					if r := recover(); r != nil {
						msg = fmt.Sprint("panic: ", r)
					}
				}()
				return r4check(root, gen, room)
			}()
			if msg != "" {
				fresh, _ := r4build(int64(seed), o)
				fresh.Width = room
				out := "(panic)"
				func() {
					defer func() { _ = recover() }()
					out = fresh.String()
				}()
				if len(out) > 6000 {
					out = "(long)"
				}
				t.Fatalf("seed %d room %d: %s\n%s\n%s", seed, room, msg, strings.Join(gen.desc, "\n"), out)
			}
		}
	}
}

const r4prose = "w16 w17 w18 w19 w20 w21 w22 w23 w24 w25 w26 w27 w28 w29 w30 w31 w32 w33 w34 w35 w36"

// Bug: a box in a row that may move its last cell (MoveLast) never
// settles. prepare measures a box's width before it fits it in the same
// pass, so the width a row is placed by is the one of the room before:
// the row moves the box, which gets the whole room and grows, so the row
// keeps it, which gets less and shrinks, and so on until the bound ends
// the loop, on a pass that depends on the tree's depth. The box is drawn
// past its room at one depth, and moved to a line of its own yet fitted
// to the room beside its key at another.
func TestBoxInARowThatMovesItsLastSettles(t *testing.T) {
	t.Parallel()
	var drawn []string
	for deep := range 3 {
		box := &Grid{Border: Box, Rows: []Row{{Cells: []Cell{{Text: "w28"}, {Text: "w29xx w30xx w31xxx w32xx w33xxxx w34xxx w35xxxx w36xx w37xx", Fit: WrapWords}}}}}
		g := &Grid{Rows: []Row{{Cells: []Cell{{Text: "w27xx"}, {Grid: box, Fit: MoveLast}}}}}
		for range deep {
			g = &Grid{Rows: []Row{{Cells: []Cell{{Grid: g}}}}}
		}
		g.Width = 60
		lines := g.Lines()
		if msg := r4settled(g); msg != "" {
			t.Errorf("%d grids around it: %s", deep, msg)
		}
		if n := linesWidth(lines); n > 60 {
			t.Errorf("%d grids around it: %d columns in a room of 60:\n%s", deep, n, strings.Join(lines, "\n"))
		}
		drawn = append(drawn, strings.Join(lines, "\n"))
	}
	for deep, d := range drawn {
		if d != drawn[0] {
			t.Errorf("with %d grids around it the row draws otherwise:\n%s\nthan with none:\n%s", deep, d, drawn[0])
		}
	}
}

// Bug: boxes sharing their Widths whose last columns differ (a section
// of keys and values, another of a heading or of prose alone) are fitted
// wrong. fitBoxes takes each box's own last column as the one that fits
// the room, but the column is shared: what the shorter box's last column
// needs (its prose fitted to the whole room) lands in a column the other
// box draws before its last, after that box's last column was fitted
// beside the narrower one. The longer box is drawn past the room, and a
// row of it past its own side.
func TestBoxesSharingWidthsWithDifferentLastColumns(t *testing.T) {
	t.Parallel()
	for name, second := range map[string][]Cell{
		"a span row alone":        {{Text: r4prose, Span: true, Fit: WrapWords}},
		"prose alone":             {{Text: r4prose, Fit: WrapWords}},
		"a heading in its column": {{Text: "a heading some thirty columns wide"}},
	} {
		w := NewWidths()
		one := &Grid{Border: Box, Rows: []Row{
			{Widths: w, Cells: []Cell{{Text: "key"}, {Text: "a value"}}},
			{Widths: w, Cells: []Cell{{Text: "other"}, {Text: r4prose, Fit: WrapWords}}},
		}}
		two := &Grid{Border: Box, Rows: []Row{{Widths: w, Cells: second}}}
		g := &Grid{Width: 60, Rows: []Row{{Cells: []Cell{{Grid: one}}}, {Cells: []Cell{{Grid: two}}}}}
		lines := g.Lines()
		if msg := r4rects(lines); msg != "" {
			t.Errorf("%s: %s:\n%s", name, msg, strings.Join(lines, "\n"))
		}
		if n := linesWidth(lines); n > 60 {
			t.Errorf("%s: %d columns in a room of 60:\n%s", name, n, strings.Join(lines, "\n"))
		}
		if at := strings.Index(lines[1], "a value"); at > len("│ other │ ") {
			t.Errorf("%s: the keys' column is wider than its keys:\n%s", name, strings.Join(lines, "\n"))
		}
	}
}

// Bug: prose in the column before one a span cell starts never fits the
// room. runsFrom takes the box's last column for the last one a row holds
// before its span (lastColumn), shapeOf draws the span's own column after
// it, and fitBoxes fits only the last column drawn: the prose is in
// neither, so its column is as wide as its text unwrapped, the box past
// its room, while the prose itself is wrapped to the room inside it.
func TestBoxProseBeforeASpansColumnFitsTheRoom(t *testing.T) {
	t.Parallel()
	g := &Grid{Border: Box, Width: 40, Rows: []Row{
		{Cells: []Cell{{Text: "key"}, {Text: r4prose, Fit: WrapWords}}},
		{Cells: []Cell{{Text: "key"}, {Text: "v"}, {Text: "x", Span: true}}},
	}}
	lines := g.Lines()
	squareLines(t, lines)
	linesInRoom(t, lines, 40)
}

// Bug: in a box, the cells from a span cell on take the gaps (and the
// fixed widths and Overflow) of the columns from 0 on, not of their own.
// cells draws them as a row of their own (Row{Cells: r.Cells[rf:]}), so
// renderRow reads column 0's settings for the cell at rf. Without
// borders the same row takes each cell's own.
func TestBoxCellsAfterASpanKeepTheirGaps(t *testing.T) {
	t.Parallel()
	rows := func() []Row {
		return []Row{{Cells: []Cell{{Text: "a"}, {Text: "b", Span: true}, {Text: "c"}}}}
	}
	plain := (&Grid{Widths: NewWidths().Gap(0, 5), Rows: rows()}).Lines()
	if len(plain) != 1 || plain[0] != "a     b  c" {
		t.Fatalf("without borders: %q", plain)
	}
	boxed := (&Grid{Border: Box, Widths: NewWidths().Gap(0, 5), Rows: rows()}).Lines()
	if len(boxed) != 3 || !strings.Contains(boxed[1], "b  c") {
		t.Errorf("in a box the cells after the span are not at their gap, two spaces:\n%s", strings.Join(boxed, "\n"))
	}
}

// Bug: a grid in a right-aligned column is torn apart. compose pads
// each line of a cell on its left by itself, a grid's too, so its lines
// of different widths start at different columns: its own columns no
// longer align, and a box in it loses its shape.
func TestGridInARightAlignedColumnKeepsItsColumns(t *testing.T) {
	t.Parallel()
	sub := &Grid{Rows: []Row{{Cells: Cells("ab", "cd")}, {Cells: Cells("x")}}}
	g := &Grid{Widths: NewWidths().Right(0), Rows: []Row{
		{Cells: []Cell{{Grid: sub}, {Text: "after"}}},
		{Cells: Cells("a cell wider than the grid", "after")},
	}}
	lines := g.Lines()
	if len(lines) != 3 || strings.Index(lines[0], "ab") != strings.Index(lines[1], "x") {
		t.Errorf("the grid's first column starts at two columns:\n%s", strings.Join(lines, "\n"))
	}
	box := &Grid{Border: Box, Rows: []Row{{Cells: Cells("k", "v")}}}
	g = &Grid{Widths: NewWidths().Right(0), Rows: []Row{
		{Cells: []Cell{{Grid: &Grid{Rows: []Row{{Cells: []Cell{{Grid: box}, {Text: "beside"}}}}}}}},
	}}
	lines = g.Lines()
	if msg := r4joined(lines); msg != "" {
		t.Errorf("a box in it: %s:\n%s", msg, strings.Join(lines, "\n"))
	}
}
