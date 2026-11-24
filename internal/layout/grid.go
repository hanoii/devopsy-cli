package layout

import "strings"

// Fit is how a row's last cell fits the room a grid has (Grid.Width).
type Fit int

const (
	// Keep never cuts the cell: an id, an address, a command.
	Keep Fit = iota
	// WrapWords wraps prose at a word within its cell, its later lines
	// at its hang (Cell.Hang) or its text's leading spaces, whichever is
	// further, when the room left is at least the grid's MinRoom.
	WrapWords
	// MoveLast is a row of cells': when the row's shown cells, unpadded,
	// do not fit the room, the last moves whole to a line of its own at
	// the grid's origin; a cell is padded only while the row still fits
	// with it padded. A row is never cut at a word.
	MoveLast
)

// Align is where a cell sits in its column.
type Align int

const (
	Left Align = iota
	Right
)

// DefaultGap is the spaces after a cell, between it and the next.
const DefaultGap = 2

// Widths are the columns a set of rows share, wherever they sit: every
// row naming them, in any grid of one rendered tree, is measured into
// them before any is rendered, so sections share one value column and a
// run of rows aligns across the lines between them.
type Widths struct {
	cols []*column
}

type column struct {
	align    Align
	fixed    int
	min      int
	gap      int
	gapSet   bool
	overflow bool
	// width is the widest cell measured; fitted the widest a bordered
	// grid's last column holds once fitted.
	width  int
	fitted int
}

// NewWidths are columns of their own.
func NewWidths() *Widths { return &Widths{} }

func (w *Widths) col(i int) *column {
	for len(w.cols) <= i {
		w.cols = append(w.cols, &column{})
	}
	return w.cols[i]
}

// Right aligns column i to its right.
func (w *Widths) Right(i int) *Widths { w.col(i).align = Right; return w }

// Fixed makes column i n columns wide, whatever its cells: a wider one
// pushes its own row's next cells (or moves them to the next line, with
// Overflow).
func (w *Widths) Fixed(i, n int) *Widths { w.col(i).fixed = n; return w }

// Min makes column i at least n columns wide.
func (w *Widths) Min(i, n int) *Widths { w.col(i).min = n; return w }

// Gap is the spaces after column i's cells.
func (w *Widths) Gap(i, n int) *Widths { c := w.col(i); c.gap, c.gapSet = n, true; return w }

// Overflow makes a cell wider than column i's fixed width end its line:
// the row's next cells go on the next line, at their column.
func (w *Widths) Overflow(i int) *Widths { w.col(i).overflow = true; return w }

// size is column i's width: fixed, else its widest cell, at least its
// minimum.
func (w *Widths) size(i int) int {
	c := w.col(i)
	if c.fixed > 0 {
		return c.fixed
	}
	return max(c.width, c.min)
}

// shown says whether column i holds anything in any row naming it.
func (w *Widths) shown(i int) bool { return w.size(i) > 0 }

func (w *Widths) gap(i int) int {
	if c := w.col(i); c.gapSet {
		return c.gap
	}
	return DefaultGap
}

// A Cell is one column of a row: text (one or more lines, plain or, in a
// Keep cell, already colored) or a grid of its own.
type Cell struct {
	Text string
	// Grid is a grid laid out in the cell (a sub-table), at the cell's
	// column, in the room left of the row's.
	Grid *Grid
	Fit  Fit
	// Hang is where a wrapped cell's later lines start, after its
	// prefix.
	Hang int
	// Prefix starts each of the cell's lines (a bar's "| "), counted in
	// its width.
	Prefix string
	// Paint colors each of the cell's lines: it gets the line without
	// its prefix, trimmed on the right, and returns the whole line, its
	// prefix included. Widths are measured before it.
	Paint func(string) string
	// Span leaves the cell out of its column's width: it runs free (a
	// title, a group's heading, the last cell of a row of cells), and
	// the cells after it follow at their gap, in no column.
	Span bool
}

// A Row is cells in columns.
type Row struct {
	Cells []Cell
	// Widths are the columns the row shares; the grid's own when nil.
	Widths *Widths
	// Indent is written before the first cell and counted in its width,
	// so rows at different indents share the columns after it.
	Indent int
	// Break puts a blank line before the row (a rule line in a box),
	// unless it is the grid's first.
	Break bool
	// Underline puts a line of - under the row, as wide as its text.
	Underline bool
	// Header marks a header row: a box draws a line under it.
	Header bool
	// Paint colors each of the row's lines whole, from its first column,
	// after its right is trimmed (a warning's color over its key too);
	// a row has it or cell paints, never both.
	Paint func(string) string
}

// A Grid is rows laid out in columns.
type Grid struct {
	Rows []Row
	// Widths are the columns of the rows that name none.
	Widths *Widths
	// Width is the room the grid's lines have, borders included: 0 for
	// none, nothing then wrapped or moved.
	Width int
	// MinRoom is the least room a WrapWords cell wraps into; with less,
	// it stays whole.
	MinRoom int
	// Border draws the grid in a box.
	Border Border
}

// Lines are the grid's lines, laid out.
func (g *Grid) Lines() []string {
	prepare(g)
	room := -1
	if g.Width > 0 {
		room = g.Width
	}
	return g.render(room)
}

// String is the grid's lines, each ended by a newline.
func (g *Grid) String() string {
	lines := g.Lines()
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

func (g *Grid) widths(r Row) *Widths {
	if r.Widths != nil {
		return r.Widths
	}
	if g.Widths == nil {
		g.Widths = NewWidths()
	}
	return g.Widths
}

// prepare measures every grid of the tree under g, the deepest first:
// text cells everywhere, then each nested grid at its natural width into
// its parent's column.
func prepare(g *Grid) {
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
	walk(g, 0)
	for _, gs := range byDepth {
		for _, g := range gs {
			for _, r := range g.Rows {
				w := g.widths(r)
				for k, c := range r.Cells {
					if c.Span || c.Grid != nil {
						continue
					}
					col := w.col(k)
					col.width = max(col.width, cellWidth(c, indentOf(r, k)))
				}
			}
		}
	}
	for d := len(byDepth) - 1; d >= 0; d-- {
		for _, g := range byDepth[d] {
			for _, r := range g.Rows {
				w := g.widths(r)
				for k, c := range r.Cells {
					if c.Span || c.Grid == nil {
						continue
					}
					col := w.col(k)
					col.width = max(col.width, indentOf(r, k)+linesWidth(c.Grid.render(-1)))
				}
			}
		}
	}
	for _, gs := range byDepth {
		for _, g := range gs {
			if g.Border != NoBorder {
				g.fitBorders()
			}
		}
	}
}

func indentOf(r Row, k int) int {
	if k == 0 {
		return r.Indent
	}
	return 0
}

// cellWidth is c's natural width, its lines unwrapped, its prefix and
// an indent before it.
func cellWidth(c Cell, indent int) int {
	n := 0
	for _, l := range strings.Split(c.Text, "\n") {
		n = max(n, Width(l))
	}
	return indent + Width(c.Prefix) + n
}

func linesWidth(lines []string) int {
	n := 0
	for _, l := range lines {
		n = max(n, Width(l))
	}
	return n
}

func empty(c Cell) bool { return c.Text == "" && c.Grid == nil && c.Prefix == "" }

// render lays out g's rows in room columns from its origin (-1 for
// none).
func (g *Grid) render(room int) []string {
	if g.Border != NoBorder {
		return g.renderBox(room)
	}
	var out []string
	for i, r := range g.Rows {
		if r.Break && i > 0 {
			out = append(out, "")
		}
		lines := g.renderRow(r, room)
		if r.Paint != nil {
			for j := range lines {
				lines[j] = r.Paint(lines[j])
			}
		}
		out = append(out, lines...)
		if r.Underline && len(lines) > 0 {
			plain := Strip(lines[0])
			text := strings.TrimLeft(plain, " ")
			out = append(out, plain[:len(plain)-len(text)]+strings.Repeat("-", Width(text)))
		}
	}
	return out
}

// placed is one cell of a row as it is written: its lines, unpainted
// and painted, at a column.
type placed struct {
	at    int
	lines []string
	// pad is the width the cell is padded to on its lines (0: none).
	pad   int
	right bool
}

// renderRow lays out one row without borders, each line trimmed on its
// right.
func (g *Grid) renderRow(r Row, room int) []string {
	w := g.widths(r)
	last := len(r.Cells) - 1
	for last > 0 && empty(r.Cells[last]) {
		last--
	}
	if last < 0 {
		return []string{""}
	}
	shownCell := func(k int) bool {
		c := r.Cells[k]
		return k == last || c.Span || w.shown(k) || c.Grid != nil
	}
	// The row's own last cell says how it fits, an empty one too.
	move := r.Cells[len(r.Cells)-1].Fit == MoveLast && room >= 0 && last > 0
	// own is the width of cells from..to as they are, their gaps between.
	own := func(from, to int) int {
		n, any := 0, false
		for k := from; k <= to; k++ {
			if k == to || (shownCell(k) && !r.Cells[k].Span) {
				n += cellWidth(r.Cells[k], indentOf(r, k)) + w.gap(k)
				any = true
			}
		}
		if !any {
			return 0
		}
		return n - w.gap(to)
	}
	moved := move && own(0, last) > room
	end := last
	if moved {
		end = last - 1
	}

	var cells []placed
	var lines [][]placed // a new line after an overflowing cell
	pos := 0
	spanned := false
	for k := 0; k <= end; k++ {
		c := r.Cells[k]
		if !shownCell(k) {
			continue
		}
		indent := indentOf(r, k)
		var text []string
		if k == last {
			text = g.fitCell(c, pos+indent, room)
		} else {
			text = cellLines(c, room, pos+indent, g)
		}
		for j := range text {
			text[j] = strings.Repeat(" ", indent) + text[j]
		}
		p := placed{at: pos, lines: text}
		width := linesWidth(text)
		// Every cell but the last is padded to its column; in a row that
		// may move its last, only while the row still fits with it padded
		// (its next cells counted as they are).
		padded := false
		if k < last && !c.Span && !spanned {
			padded = true
			if move {
				rest := 0
				if k < end {
					rest = own(k+1, end) + w.gap(k)
				}
				padded = pos+w.size(k)+rest <= room
			}
		}
		// A right aligned cell is padded on its left wherever it sits, its
		// row's last too.
		right := w.col(k).align == Right && !c.Span && !spanned
		if padded || right {
			p.pad = w.size(k)
			p.right = right
			width = max(width, w.size(k))
		}
		if c.Span {
			spanned = true
		}
		cells = append(cells, p)
		pos += width + w.gap(k)
		// A cell wider than its fixed column ends its line: the next
		// cells go on the next line, at their columns.
		if col := w.col(k); col.fixed > 0 && col.overflow && linesWidth(text) > col.fixed && k < end {
			cells[len(cells)-1].pad = 0
			lines = append(lines, cells)
			cells = nil
			pos = 0
			for j := 0; j <= k; j++ {
				if shownCell(j) {
					pos += w.size(j) + w.gap(j)
				}
			}
		}
	}
	lines = append(lines, cells)
	var out []string
	for _, cs := range lines {
		out = append(out, compose(cs)...)
	}
	if moved {
		out = append(out, g.fitCell(r.Cells[last], 0, room)...)
	}
	return out
}

// compose writes placed cells as lines, each trimmed on its right.
func compose(cs []placed) []string {
	height := 0
	for _, c := range cs {
		height = max(height, len(c.lines))
	}
	out := make([]string, height)
	for i := range out {
		var b strings.Builder
		col := 0
		for _, c := range cs {
			if c.at > col {
				b.WriteString(strings.Repeat(" ", c.at-col))
				col = c.at
			}
			s := ""
			if i < len(c.lines) {
				s = c.lines[i]
			}
			switch {
			case c.pad > 0 && c.right:
				s = padLeft(s, c.pad)
			case c.pad > 0:
				s = Pad(s, c.pad)
			}
			b.WriteString(s)
			col += Width(s)
		}
		out[i] = strings.TrimRight(b.String(), " ")
	}
	return out
}

// cellLines are a cell's lines as it is written, not fitted: its text's
// lines or its grid's, each with its prefix and painted.
func cellLines(c Cell, room, start int, g *Grid) []string {
	var text []string
	if c.Grid != nil {
		sub := -1
		if room >= 0 {
			sub = room - start
		}
		return c.Grid.render(sub)
	}
	text = strings.Split(c.Text, "\n")
	return finish(c, text)
}

// fitCell is a row's last cell, fitted to the room from start.
func (g *Grid) fitCell(c Cell, start, room int) []string {
	if c.Grid != nil {
		return cellLines(c, room, start, g)
	}
	if c.Fit != WrapWords || room < 0 {
		return finish(c, strings.Split(c.Text, "\n"))
	}
	var text []string
	for _, line := range strings.Split(c.Text, "\n") {
		body := strings.TrimLeft(line, " ")
		lead := len(line) - len(body)
		hang := max(c.Hang, lead)
		pw := Width(c.Prefix)
		r := room - start - pw - hang
		if r < max(1, g.MinRoom) || start+pw+Width(line) <= room {
			text = append(text, line)
			continue
		}
		parts := Wrap(line, r)
		for i := 1; i < len(parts); i++ {
			parts[i] = strings.Repeat(" ", hang) + parts[i]
		}
		text = append(text, parts...)
	}
	return finish(c, text)
}

// finish puts a cell's prefix on its lines and paints them.
func finish(c Cell, text []string) []string {
	out := make([]string, len(text))
	for i, t := range text {
		t = strings.TrimRight(t, " ")
		if c.Paint != nil {
			out[i] = c.Paint(t)
		} else {
			out[i] = c.Prefix + t
		}
	}
	return out
}

// Cells are a row's cells of plain text, one per string.
func Cells(texts ...string) []Cell {
	cs := make([]Cell, len(texts))
	for i, t := range texts {
		cs[i] = Cell{Text: t}
	}
	return cs
}
