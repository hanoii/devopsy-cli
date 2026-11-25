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
// run of rows aligns across the lines between them. Its rows sit at one
// depth of the tree: shared by a grid and a grid in one of its own
// cells, a column can hold no layout, each width moving the other.
type Widths struct {
	cols []*column
	// box is the columns the boxes naming these draw, as fitBoxes took
	// them for all of them at once.
	box *boxColumns
}

type column struct {
	align    Align
	fixed    int
	min      int
	gap      int
	gapSet   bool
	overflow bool
	// text is the widest text cell measured, nest the widest grid in a
	// cell; fitted what a box's cells take once laid out as drawn.
	text   int
	nest   int
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
// Overflow); in a box it grows to its widest cell, so the box's side
// holds.
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
	return max(c.text, c.nest, c.min)
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
	// column, in the room left of the row's. In a box's cell it draws no
	// box of its own.
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
	// the cells after it follow at their gap, in no column; in a box it
	// runs from its column to the box's side.
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
	// Underline puts a line of - under the row, as wide as its text; a
	// box draws none.
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

	// What prepare works out: fit is the room the grid is laid out in,
	// its parent's room less its cell's start, at most its own Width, -1
	// for none; natural how wide it draws in that room; inBox that its
	// cell is a box's, where it draws no box of its own.
	fit     int
	natural int
	inBox   bool
	// boxRoom is the room a box is fitted and drawn in: the tightest
	// among the boxes sharing its columns (fitBoxes).
	boxRoom int
}

// Lines are the grid's lines, laid out.
func (g *Grid) Lines() []string {
	prepare(g)
	return g.render(g.fit)
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

// boxed says whether g draws a box: a box in a box's cell draws none.
func (g *Grid) boxed() bool { return g.Border != NoBorder && !g.inBox }

// prepare measures every grid of the tree under g before any is drawn:
// the text cells everywhere; then passes (tree.pass) until one changes
// nothing, each giving every grid its room from where its row places it,
// every nested grid's width as it draws there, the deepest first, into
// its parent's column, and the boxes fitted to their rooms (fitBoxes),
// every grid measured again as it then draws, so each pass's state holds
// by itself. The bound only guards a tree no pass settles (a Widths shared
// across depths).
func prepare(root *Grid) *tree {
	t := newTree(root)
	// A render measures afresh: what an earlier one measured goes.
	t.all(func(g *Grid) {
		g.fit, g.natural, g.boxRoom = -1, 0, -1
		for _, r := range g.Rows {
			w := g.widths(r)
			w.box = nil
			for _, c := range w.cols {
				c.text, c.nest, c.fitted = 0, 0, 0
			}
		}
	})
	t.all(func(g *Grid) {
		for _, r := range g.Rows {
			w := g.widths(r)
			for k, c := range measured(r) {
				if c.Grid == nil {
					col := w.col(k)
					col.text = max(col.text, cellWidth(c, indentOf(r, k)))
				}
			}
		}
	})
	root.fit = roomOf(root.Width, -1, 0)
	for range len(t.byDepth) + 3 {
		before := t.state()
		t.pass()
		if equal(before, t.state()) {
			break
		}
	}
	return t
}

// tree is a grid and every grid in its cells, by depth, and its boxes.
type tree struct {
	byDepth [][]*Grid
	boxes   []*Grid
}

func newTree(root *Grid) *tree {
	t := &tree{}
	var walk func(g *Grid, d int, inBox bool)
	walk = func(g *Grid, d int, inBox bool) {
		for len(t.byDepth) <= d {
			t.byDepth = append(t.byDepth, nil)
		}
		g.inBox = inBox
		t.byDepth[d] = append(t.byDepth[d], g)
		if g.boxed() {
			t.boxes = append(t.boxes, g)
		}
		for _, r := range g.Rows {
			for _, c := range r.Cells {
				if c.Grid != nil {
					walk(c.Grid, d+1, inBox || g.boxed())
				}
			}
		}
	}
	walk(root, 0, false)
	return t
}

func (t *tree) all(f func(g *Grid)) {
	for _, gs := range t.byDepth {
		for _, g := range gs {
			f(g)
		}
	}
}

// pass is one pass of prepare's: rooms, natural widths, boxes fitted and
// every grid measured as it then draws.
func (t *tree) pass() {
	for _, gs := range t.byDepth {
		for _, g := range gs {
			g.roomsBelow()
		}
	}
	t.naturals()
	if len(t.boxes) > 0 {
		fitBoxes(t.boxes)
		t.naturals()
	}
}

// naturals measures each nested grid as it draws in its room, the
// deepest first, into its parent's column (nest).
func (t *tree) naturals() {
	for d := len(t.byDepth) - 1; d >= 1; d-- {
		for _, g := range t.byDepth[d] {
			g.natural = linesWidth(g.render(g.fit))
		}
		t.all(func(g *Grid) {
			for _, r := range g.Rows {
				for _, c := range g.widths(r).cols {
					c.nest = 0
				}
			}
		})
		for e := len(t.byDepth) - 1; e >= 0; e-- {
			for _, g := range t.byDepth[e] {
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
}

// state is every grid's room, natural width and box room, and every
// column's fit and nested width, to tell when a pass changed nothing.
func (t *tree) state() []int {
	var out []int
	t.all(func(g *Grid) {
		out = append(out, g.fit, g.natural, g.boxRoom)
		for _, r := range g.Rows {
			for _, c := range g.widths(r).cols {
				out = append(out, c.fitted, c.nest)
			}
		}
	})
	return out
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// measured are the cells of r its columns measure, by their index: those
// before its first span cell (spanAt), which runs free with every cell
// after it.
func measured(r Row) map[int]Cell {
	out := map[int]Cell{}
	for k := range spanAt(r) {
		out[k] = r.Cells[k]
	}
	return out
}

// lastCell is r's last cell that holds anything, its fit the row's: its
// own, else the row's positional last cell's (an empty last cell says how
// the row fits too).
func lastCell(r Row, last int) Cell {
	c := r.Cells[last]
	if p := r.Cells[len(r.Cells)-1]; c.Fit == Keep {
		c.Fit = p.Fit
	}
	return c
}

// alignLine is a cell's line padded to width, on its left when right; a
// grid's lines (block, its widest) padded to it first, so the grid moves
// as one and keeps its own columns.
func alignLine(s string, width int, right bool, block int) string {
	if !right {
		return Pad(s, width)
	}
	if block > 0 {
		s = Pad(s, block)
	}
	return padLeft(s, width)
}

// roomOf is the room a grid has in a cell that starts at start, of a
// parent with room (-1 for none), at most the grid's own width when it
// has one, and never less than none left.
func roomOf(own, room, start int) int {
	sub := -1
	if room >= 0 {
		sub = max(0, room-start)
	}
	if own > 0 && (sub < 0 || own < sub) {
		sub = own
	}
	return sub
}

// roomsBelow gives each grid in g's cells its room (fit): where its row
// places it (place), as it is drawn, or in a box's cell where the box
// lays it (cells).
func (g *Grid) roomsBelow() {
	if g.boxed() {
		// Where cells lays it, in the room the box is drawn in.
		s := g.shapeOf()
		room := g.boxRoom
		if room < 0 {
			room = g.fit
		}
		for _, r := range g.Rows {
			last := lastShown(r)
			for i, k := range s.drawn(r) {
				if c := r.Cells[k]; c.Grid != nil {
					// Where cells lays it: a row's last in what the columns
					// after it leave.
					inside := insideRoom(room)
					if k == last {
						inside = s.lastRoom(inside, i)
					}
					c.Grid.fit = roomOf(c.Grid.Width, inside, s.start(i)+indentOf(r, k))
				}
			}
			// A grid the row runs to the side from, from its column.
			if rf := runsFrom(r, s.lastCol); rf < len(r.Cells) && r.Cells[rf].Grid != nil && !r.Cells[rf].Span {
				c := r.Cells[rf]
				c.Grid.fit = roomOf(c.Grid.Width, insideRoom(room), s.start(s.runsIn(r))+indentOf(r, rf))
			}
		}
		return
	}
	for _, r := range g.Rows {
		p := g.place(r, g.fit, 0)
		for _, at := range p.cells {
			if c := r.Cells[at.k]; c.Grid != nil {
				c.Grid.fit = roomOf(c.Grid.Width, g.fit, at.at+indentOf(r, at.k))
			}
		}
		if p.moved >= 0 {
			if c := r.Cells[p.moved]; c.Grid != nil {
				c.Grid.fit = roomOf(c.Grid.Width, g.fit, 0)
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
// an indent before it; a grid's, as prepare measured it.
func cellWidth(c Cell, indent int) int {
	if c.Grid != nil {
		return indent + c.Grid.natural
	}
	n := 0
	for _, l := range textLines(c) {
		n = max(n, Width(l))
	}
	return indent + Width(c.Prefix) + n
}

// textLines are a text cell's lines, a tab in them one space: a
// terminal's tab stops are not the grid's columns.
func textLines(c Cell) []string {
	return strings.Split(strings.ReplaceAll(c.Text, "\t", " "), "\n")
}

func linesWidth(lines []string) int {
	n := 0
	for _, l := range lines {
		n = max(n, Width(l))
	}
	return n
}

func empty(c Cell) bool { return c.Text == "" && c.Grid == nil && c.Prefix == "" }

// lastShown is the index of r's last cell that holds anything, its
// trailing empty cells left (0 when none holds anything, -1 with no
// cells).
func lastShown(r Row) int {
	last := len(r.Cells) - 1
	for last > 0 && empty(r.Cells[last]) {
		last--
	}
	return last
}

// render lays out g's rows in room columns from its origin (-1 for
// none).
func (g *Grid) render(room int) []string {
	if g.boxed() {
		return g.renderBox()
	}
	var out []string
	for i, r := range g.Rows {
		if r.Break && i > 0 {
			out = append(out, "")
		}
		lines := g.renderRow(r, room)
		under := ""
		if r.Underline && len(lines) > 0 {
			// Under every line of the row, as wide as the widest, at the
			// row's indent.
			n := 0
			for _, l := range lines {
				n = max(n, Width(strings.TrimLeft(Strip(l), " ")))
			}
			if n > 0 {
				under = strings.Repeat(" ", r.Indent) + strings.Repeat("-", n)
			}
		}
		if r.Paint != nil {
			for j := range lines {
				if lines[j] != "" {
					lines[j] = r.Paint(lines[j])
				}
			}
		}
		out = append(out, lines...)
		if under != "" {
			out = append(out, under)
		}
	}
	return out
}

// slot is one cell of a row as its row places it: the cell, its column
// from the grid's origin, the width it is padded to (0: none, right for
// on its left), the line of the row it is on (a cell wider than its fixed
// column with Overflow ends one), and whether it is the row's last,
// fitted to the room.
type slot struct {
	k, at, pad int
	right      bool
	line       int
	last       bool
}

// rowPlace is where a row's cells go: the cells it shows, and the cell
// its row moves to a line of its own (MoveLast), -1 for none.
type rowPlace struct {
	cells []slot
	moved int
}

// place lays out where r's cells go in room (-1 for none), from cell
// from on, at the grid's origin, by their widths as measured: what
// renderRow draws and roomsBelow gives each grid in a cell its room by,
// so the two never differ. A box lays out what runs from a span cell to
// its side from that cell, each cell keeping its own column's settings.
func (g *Grid) place(r Row, room, from int) rowPlace {
	p := rowPlace{moved: -1}
	w := g.widths(r)
	last := lastShown(r)
	if last < from {
		return p
	}
	// Shown: a cell in a column something holds, a grid, the row's last,
	// a span cell holding anything, and any cell after one, in no column.
	shown := make([]bool, len(r.Cells))
	spanned := false
	for k, c := range r.Cells {
		shown[k] = k == last || c.Grid != nil || (spanned || c.Span) && !empty(c) || !spanned && w.shown(k)
		spanned = spanned || c.Span
	}
	// The row's own last cell says how it fits, an empty one too.
	move := r.Cells[len(r.Cells)-1].Fit == MoveLast && room >= 0 && last > from
	// inColumn: before the row's first span cell, padded to its column.
	inColumn := make([]bool, len(r.Cells))
	for k := range spanAt(r) {
		inColumn[k] = true
	}
	// own is the width of shown cells from..to as they are, a right
	// aligned one at its column's width as it is padded wherever it sits,
	// their gaps between.
	own := func(from, to int) int {
		n, any := 0, false
		for k := from; k <= to; k++ {
			if shown[k] {
				width := cellWidth(r.Cells[k], indentOf(r, k))
				if inColumn[k] && w.col(k).align == Right {
					width = max(width, w.size(k))
				}
				n += width + w.gap(k)
				any = true
			}
		}
		if !any {
			return 0
		}
		return n - w.gap(to)
	}
	// A cell in its column wider than the column's fixed width, with
	// Overflow, ends its line: the next line starts at the column after it,
	// and the row's last cell fits or not on the line it is on.
	ends := func(k int) bool {
		col := w.col(k)
		return shown[k] && inColumn[k] && col.fixed > 0 && col.overflow && cellWidth(r.Cells[k], indentOf(r, k)) > col.fixed
	}
	after := func(k int) int {
		at := 0
		for j := from; j <= k; j++ {
			if shown[j] {
				at += w.size(j) + w.gap(j)
			}
		}
		return at
	}
	lineFrom, lineAt := from, 0
	for k := from; k < last; k++ {
		if ends(k) {
			lineFrom, lineAt = k+1, after(k)
		}
	}
	end := last
	if move && lineAt+own(lineFrom, last) > room {
		p.moved, end = last, last-1
	}
	// lineEnd is the last cell on the line cell k is on, before the next
	// that ends a line.
	lineEnd := func(k int) int {
		for j := k; j < end; j++ {
			if ends(j) {
				return j
			}
		}
		return end
	}
	pos, line := 0, 0
	for k := from; k <= end; k++ {
		c := r.Cells[k]
		if !shown[k] {
			continue
		}
		s := slot{k: k, at: pos, line: line, last: k == last}
		width := cellWidth(c, indentOf(r, k))
		// Every cell but the last is padded to its column; in a row that
		// may move its last, only while its line still fits with it padded
		// (the next cells on it counted as they are). A right aligned cell
		// is padded on its left wherever it sits, its row's last too.
		padded := k < last && inColumn[k]
		if padded && move {
			rest := 0
			if e := lineEnd(k); k < e {
				rest = own(k+1, e) + w.gap(k)
			}
			padded = pos+w.size(k)+rest <= room
		}
		right := inColumn[k] && w.col(k).align == Right
		if padded || right {
			s.pad, s.right = w.size(k), right
			width = max(width, w.size(k))
		}
		p.cells = append(p.cells, s)
		pos += width + w.gap(k)
		// A cell wider than its fixed column ends its line: the next
		// cells go on the next line, at their columns.
		if ends(k) && k < end {
			p.cells[len(p.cells)-1].pad = 0
			line++
			pos = after(k)
		}
	}
	return p
}

// renderRow lays out one row without borders where place puts its
// cells, each line trimmed on its right.
func (g *Grid) renderRow(r Row, room int) []string { return g.renderFrom(r, room, 0) }

// renderFrom lays out r from cell from on, as renderRow does.
func (g *Grid) renderFrom(r Row, room, from int) []string {
	p := g.place(r, room, from)
	if len(p.cells) == 0 && p.moved < 0 {
		return []string{""}
	}
	var lines [][]placed
	for _, s := range p.cells {
		c := r.Cells[s.k]
		indent := indentOf(r, s.k)
		var text []string
		if s.last {
			text = g.fitCell(lastCell(r, s.k), s.at+indent, room)
		} else {
			text = cellLines(c, room, s.at+indent)
		}
		for len(lines) <= s.line {
			lines = append(lines, nil)
		}
		lines[s.line] = append(lines[s.line], placed{at: s.at, lines: indented(text, indent), pad: s.pad, right: s.right, block: c.Grid != nil})
	}
	var out []string
	for _, cs := range lines {
		out = append(out, compose(cs)...)
	}
	if p.moved >= 0 {
		out = append(out, g.fitCell(r.Cells[p.moved], 0, room)...)
	}
	return out
}

// placed is one cell of a row as it is written: its lines at a column,
// padded to a width; a grid's (block) aligned as one, its own columns
// kept.
type placed struct {
	at    int
	lines []string
	pad   int
	right bool
	block bool
}

// indented is lines with n spaces before each (a row's indent).
func indented(lines []string, n int) []string {
	if n == 0 {
		return lines
	}
	for j := range lines {
		lines[j] = strings.Repeat(" ", n) + lines[j]
	}
	return lines
}

// compose writes placed cells as lines, each trimmed on its right.
func compose(cs []placed) []string {
	height := 0
	for _, c := range cs {
		height = max(height, len(c.lines))
	}
	out := make([]string, height)
	block := make([]int, len(cs))
	for j, c := range cs {
		if c.block {
			block[j] = linesWidth(c.lines)
		}
	}
	for i := range out {
		var b strings.Builder
		col := 0
		for j, c := range cs {
			if c.at > col {
				b.WriteString(strings.Repeat(" ", c.at-col))
				col = c.at
			}
			s := ""
			if i < len(c.lines) {
				s = c.lines[i]
			}
			if c.pad > 0 {
				s = alignLine(s, c.pad, c.right, block[j])
			}
			b.WriteString(s)
			col += Width(s)
		}
		out[i] = strings.TrimRight(b.String(), " ")
	}
	return out
}

// cellLines are a cell's lines as it is written, not fitted: its text's
// lines or its grid's, in the room its cell leaves it (roomOf), each with
// its prefix and painted.
func cellLines(c Cell, room, start int) []string {
	if c.Grid != nil {
		return c.Grid.render(roomOf(c.Grid.Width, room, start))
	}
	return finish(c, textLines(c))
}

// fitCell is a row's last cell, fitted to the room from start.
func (g *Grid) fitCell(c Cell, start, room int) []string {
	if c.Grid != nil {
		return cellLines(c, room, start)
	}
	if c.Fit != WrapWords || room < 0 {
		return finish(c, textLines(c))
	}
	var text []string
	for _, line := range textLines(c) {
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
