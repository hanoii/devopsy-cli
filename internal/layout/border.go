package layout

import (
	"slices"
	"strings"
)

// Border is how a grid is drawn.
type Border int

const (
	// NoBorder lays the grid out in columns of spaces.
	NoBorder Border = iota
	// Box draws it in a box of light lines with rounded corners: a line
	// between columns, under a header row, at each row's Break, and where
	// the box goes from a row across it to columns or back.
	Box
)

// The box's pieces: a cell sits between "│ " and " │".
const (
	boxSide    = "│"
	boxLine    = "─"
	boxPadding = 1
	// sideWidth is the columns a side or a column line takes (its bytes
	// are three).
	sideWidth = 1
)

// firstHeld is the index of r's first cell that holds anything,
// len(r.Cells) for none.
func firstHeld(r Row) int {
	for k, c := range r.Cells {
		if !empty(c) {
			return k
		}
	}
	return len(r.Cells)
}

// SpanRow says whether r's first cell that holds anything is a span: the
// row runs free of the columns (a heading), across a box.
func SpanRow(r Row) bool {
	k := firstHeld(r)
	return k < len(r.Cells) && r.Cells[k].Span
}

// spanAt is the index of r's first span cell, len(r.Cells) for none:
// the cells before it are in columns, those from it on in none.
func spanAt(r Row) int {
	for k, c := range r.Cells {
		if c.Span {
			return k
		}
	}
	return len(r.Cells)
}

// runSpan is the index of r's first span cell that holds anything,
// len(r.Cells) for none: where a box runs to its side from, an empty span
// cell drawing nothing, as an empty cell does.
func runSpan(r Row) int {
	for k, c := range r.Cells {
		if c.Span && !empty(c) {
			return k
		}
	}
	return len(r.Cells)
}

// lastColumn is the last column a row of g draws: a cell it holds before
// its span, or the column a span starts in; -1 for none. Whether a row's
// prose runs to the side (runsFrom) and which column a box fits last go by
// it alike.
func (g *Grid) lastColumn() int {
	n := -1
	for _, r := range g.Rows {
		if SpanRow(r) {
			n = max(n, firstHeld(r))
			continue
		}
		sp := runSpan(r)
		if sp < len(r.Cells) {
			n = max(n, sp)
		}
		for k := min(sp, len(r.Cells)) - 1; k >= 0; k-- {
			if !empty(r.Cells[k]) {
				n = max(n, k)
				break
			}
		}
	}
	return n
}

// boxColumns are the columns boxes sharing a Widths draw: every one any of
// them holds, and their last.
type boxColumns struct {
	cols    []int
	lastCol int
}

// heldColumns are the columns g's rows hold anything in before they run
// to the side, and each a row runs from, with lastCol the box's last.
func (g *Grid) heldColumns(lastCol int) []int {
	n := 0
	for _, r := range g.Rows {
		n = max(n, min(runsFrom(r, lastCol)+1, len(r.Cells)))
	}
	var out []int
	for k := range n {
		for _, r := range g.Rows {
			rf := runsFrom(r, lastCol)
			if k < len(r.Cells) && (k < rf && !empty(r.Cells[k]) || k == rf) {
				out = append(out, k)
				break
			}
		}
	}
	return out
}

// runsFrom is the cell of r a box runs from to its side, with no column
// line across it: a span row's first cell that holds anything, else its
// first span cell that holds anything, else its last cell before the
// box's last column when that is prose (WrapWords), which so wraps in the
// room to the side and never in a column the cells after it hold, or the
// row's one cell short of that column (a heading); len(r.Cells) for none.
// A box's widths then hang on no fit of a column but its last, and are
// measured in one pass.
func runsFrom(r Row, lastCol int) int {
	if SpanRow(r) {
		return firstHeld(r)
	}
	sp := runSpan(r)
	last := lastShown(r)
	if last < 0 || last >= sp || last >= lastCol || empty(r.Cells[last]) {
		return sp
	}
	// Prose and a grid run to the side, laid out in the room to it, which
	// so never hangs on the columns after them; so does a row of one cell
	// that stops short of the box's last column (a heading); a row of
	// several cells keeps them in their columns, its alignment and lines
	// with them.
	if lastCell(r, last).Fit == WrapWords || r.Cells[last].Grid != nil || firstHeld(r) == last && len(r.Cells)-1 < lastCol {
		return last
	}
	return sp
}

// across says whether r runs across the box from its first cell that
// holds anything, with nothing in a column before it (a span row, a row
// of prose alone).
func across(r Row, lastCol int) bool {
	k := firstHeld(r)
	return k < len(r.Cells) && runsFrom(r, lastCol) <= k
}

// boxShape is what a box draws: its columns' Widths, the last column its
// rows hold, the cells it draws as columns (by their index in a row), and
// how wide it draws each.
type boxShape struct {
	w       *Widths
	lastCol int
	cols    []int
	widths  []int
}

// shapeOf is g's columns as a box draws them: the columns its rows hold
// (heldColumns), or, once fitted, those every box sharing its Widths
// holds, so they draw one set; each as wide as fitBoxes fitted it, else as
// measured.
func (g *Grid) shapeOf() boxShape {
	var s boxShape
	for _, r := range g.Rows {
		s.w = g.widths(r)
		break
	}
	if s.w == nil {
		s.w = g.widths(Row{})
	}
	if b := s.w.box; b != nil {
		s.lastCol, s.cols = b.lastCol, b.cols
	} else {
		s.lastCol = g.lastColumn()
		s.cols = g.heldColumns(s.lastCol)
	}
	for _, k := range s.cols {
		c := s.w.col(k)
		width := s.w.size(k)
		if c.fitted > 0 {
			width = max(c.min, c.fixed, c.fitted)
		}
		s.widths = append(s.widths, width)
	}
	return s
}

// drawn are r's cells a box draws in columns, by the index of the column
// each is in: those before the row runs to the side.
func (s boxShape) drawn(r Row) map[int]int {
	out := map[int]int{}
	rf := runsFrom(r, s.lastCol)
	for i, k := range s.cols {
		if k < rf && k < len(r.Cells) {
			out[i] = k
		}
	}
	return out
}

// runsIn is the box's column r runs to the side from, len(s.cols) for a
// row that does not.
func (s boxShape) runsIn(r Row) int {
	if rf := runsFrom(r, s.lastCol); rf < len(r.Cells) {
		return s.column(rf)
	}
	return len(s.cols)
}

// start is where the box's column i starts, from its left side.
func (s boxShape) start(i int) int {
	at := sideWidth + boxPadding
	for j := range i {
		at += s.widths[j] + 2*boxPadding + sideWidth
	}
	return at
}

// inner is the width between a box's sides.
func (s boxShape) inner() int {
	if len(s.cols) == 0 {
		return 2 * boxPadding
	}
	return s.start(len(s.cols)) - boxPadding - 2*sideWidth
}

// column is the box's column cell k is drawn in: the first drawn at k
// or after, len(s.cols) when none is.
func (s boxShape) column(k int) int {
	for i, c := range s.cols {
		if c >= k {
			return i
		}
	}
	return len(s.cols)
}

// insideRoom is the room a box's lines leave its cells, its right side
// taken off (cells' starts count its left).
func insideRoom(room int) int {
	if room < 0 {
		return -1
	}
	return room - boxPadding - sideWidth
}

// boxLaid is a row of a box laid out: one list of lines per column it
// draws, then the lines of what runs to the side from the column it
// starts in (nil for none).
type boxLaid struct {
	cols   [][]string
	run    []string
	runsIn int
}

// cells lays out a row of a box in room: each column's cell after the
// box's side and the columns before it, the box's last column's prose
// fitted to the room left, and what the row runs from (runsFrom), as
// without borders, from its column to the side. Measuring a box
// (fitBoxes) and drawing it (boxRow) lay a row out here alike.
func (g *Grid) cells(r Row, s boxShape, room int) boxLaid {
	inside := insideRoom(room)
	rf := runsFrom(r, s.lastCol)
	last := lastShown(r)
	l := boxLaid{cols: make([][]string, len(s.cols)), runsIn: s.runsIn(r)}
	for i, k := range s.drawn(r) {
		c := r.Cells[k]
		indent := indentOf(r, k)
		if k == last {
			l.cols[i] = indented(g.fitCell(lastCell(r, k), s.start(i)+indent, inside), indent)
		} else {
			l.cols[i] = indented(cellLines(c, inside, s.start(i)+indent), indent)
		}
	}
	if rf >= len(r.Cells) {
		return l
	}
	start := s.start(l.runsIn)
	if c := r.Cells[rf]; !c.Span && rf == last {
		// Prose before the last column, or a row's one cell, to the side.
		indent := indentOf(r, rf)
		l.run = indented(g.fitCell(lastCell(r, rf), start+indent, inside), indent)
		return l
	}
	run := -1
	if inside >= 0 {
		run = max(0, inside-start)
	}
	l.run = g.renderFrom(r, run, rf)
	return l
}

// fitBoxes fits boxes to their rooms, in one pass: boxes sharing columns
// fit them to the tightest room among them and are drawn in it
// (boxRoom); every column but a box's last is as wide as its cells as
// they are, and the last as wide as its own cells fitted to the room left
// after the others, and as what runs to the side needs.
func fitBoxes(boxes []*Grid) {
	// The columns of boxes sharing a Widths: their last the last of any,
	// their set every one any holds.
	groups := map[*Widths][]*Grid{}
	var order []*Widths
	for _, g := range boxes {
		w := g.shapeOf().w
		w.box = nil
		if groups[w] == nil {
			order = append(order, w)
		}
		groups[w] = append(groups[w], g)
	}
	for _, w := range order {
		last := -1
		for _, g := range groups[w] {
			last = max(last, g.lastColumn())
		}
		held := map[int]bool{}
		var cols []int
		for _, g := range groups[w] {
			for _, k := range g.heldColumns(last) {
				if !held[k] {
					held[k] = true
					cols = append(cols, k)
				}
			}
		}
		slices.Sort(cols)
		w.box = &boxColumns{cols: cols, lastCol: last}
	}
	room := map[*Widths]int{}
	shapes := map[*Grid]boxShape{}
	for _, g := range boxes {
		s := g.shapeOf()
		shapes[g] = s
		if r, ok := room[s.w]; !ok || (g.fit >= 0 && (r < 0 || g.fit < r)) {
			room[s.w] = g.fit
		}
	}
	// The columns but the last, as their cells take: they wrap nothing.
	natural := map[*column]int{}
	for _, g := range boxes {
		g.boxRoom = room[shapes[g].w]
		s := shapes[g]
		for _, r := range g.Rows {
			last := lastShown(r)
			for i, k := range s.drawn(r) {
				if i == len(s.cols)-1 && k == last {
					continue
				}
				col := s.w.col(k)
				natural[col] = max(natural[col], cellWidth(r.Cells[k], indentOf(r, k)))
			}
		}
	}
	for _, g := range boxes {
		s := shapes[g]
		for i, k := range s.cols {
			if col := s.w.col(k); i < len(s.cols)-1 {
				col.fitted = max(natural[col], col.min, col.fixed, 1)
			}
		}
	}
	// The last column: its own cells fitted after the others, and what
	// runs to the side; what wraps (prose, a grid laid out in its room) and
	// what cannot (a command, kept whole) apart, as MaxWidth holds back the
	// second alone.
	need, keep := map[*column]int{}, map[*column]int{}
	for _, g := range boxes {
		s := g.shapeOf()
		if len(s.cols) == 0 {
			continue
		}
		lastCol := s.w.col(s.cols[len(s.cols)-1])
		from := s.start(len(s.cols)-1) - sideWidth - boxPadding
		need[lastCol] = max(need[lastCol], natural[lastCol])
		for _, r := range g.Rows {
			l := g.cells(r, s, g.boxRoom)
			n := linesWidth(l.cols[len(s.cols)-1])
			if k := s.cols[len(s.cols)-1]; k < len(r.Cells) && kept(r, k) {
				keep[lastCol] = max(keep[lastCol], n)
			} else {
				need[lastCol] = max(need[lastCol], n)
			}
			if l.run != nil {
				at := s.start(l.runsIn) - sideWidth - boxPadding
				n := at + linesWidth(l.run) - from
				if kept(r, runsFrom(r, s.lastCol)) {
					keep[lastCol] = max(keep[lastCol], n)
				} else {
					need[lastCol] = max(need[lastCol], n)
				}
			}
		}
	}
	// A box widens for a line it cannot wrap up to its MaxWidth, boxes
	// sharing columns up to the least of theirs: a longer line runs past
	// the side.
	most := map[*column]int{}
	for _, g := range boxes {
		s := g.shapeOf()
		if len(s.cols) == 0 || g.MaxWidth <= 0 {
			continue
		}
		lastCol := s.w.col(s.cols[len(s.cols)-1])
		// Never less than the room the box wraps in.
		at := max(g.MaxWidth, g.Width) - s.start(len(s.cols)-1) - boxPadding - sideWidth
		if m, ok := most[lastCol]; !ok || at < m {
			most[lastCol] = at
		}
	}
	for col := range keep {
		if _, ok := need[col]; !ok {
			need[col] = 0
		}
	}
	for col, n := range need {
		// Held back to the room MaxWidth leaves the column, never below
		// what its prose takes; with none left (the columns before it pass
		// MaxWidth already), nothing is held back: the box is wider anyway.
		k := keep[col]
		if m, ok := most[col]; ok && m > 0 {
			k = min(k, max(m, n))
		}
		col.fitted = max(n, k, col.min, col.fixed, 1)
	}
}

// kept says whether r's cell k is kept whole wherever it is drawn: text
// that does not wrap, which a box's MaxWidth lets run past its side.
func kept(r Row, k int) bool {
	if k >= len(r.Cells) {
		return false
	}
	c := r.Cells[k]
	if k == lastShown(r) {
		c = lastCell(r, k)
	}
	return c.Grid == nil && c.Fit != WrapWords
}

// lines says, for a row of a box, whether a column line stands after each
// of its columns but the last: every one in a row of columns, up to the
// column it runs from in one that runs to the side after cells of its
// own, none in one across the box.
func (s boxShape) lines(r *Row) []bool {
	out := make([]bool, max(0, len(s.cols)-1))
	if r == nil || across(*r, s.lastCol) {
		return out
	}
	in := s.runsIn(*r)
	for i := range out {
		out[i] = i+1 <= in
	}
	return out
}

// renderBox draws g in a box, in the room it was fitted to (boxRoom).
func (g *Grid) renderBox() []string {
	s := g.shapeOf()
	inner := s.inner()
	// A rule: a junction at each column line, joining those of the row
	// above and below it, its first open columns left blank between the
	// lines that go on through it, a tree's guides in the first (guide).
	rule := func(left, right string, above, below *Row, open int, guide string) string {
		if len(s.cols) == 0 {
			return left + strings.Repeat(boxLine, inner) + right
		}
		up, down := s.lines(above), s.lines(below)
		var b strings.Builder
		if open > 0 {
			left = boxSide
		}
		b.WriteString(left)
		for i, w := range s.widths {
			switch {
			case i == 0 && i < open && s.cols[0] == 0 && guide != "" && Width(guide) <= w:
				b.WriteString(strings.Repeat(" ", boxPadding) + Pad(guide, w) + strings.Repeat(" ", boxPadding))
			case i < open:
				b.WriteString(strings.Repeat(" ", w+2*boxPadding))
			default:
				b.WriteString(strings.Repeat(boxLine, w+2*boxPadding))
			}
			if i == len(s.widths)-1 {
				break
			}
			switch {
			case i < open-1 && (up[i] || down[i]):
				b.WriteString(boxSide)
			case i < open-1:
				b.WriteString(" ")
			case i == open-1 && up[i] && down[i]:
				b.WriteString("├")
			case i == open-1 && up[i]:
				b.WriteString("└")
			case i == open-1 && down[i]:
				b.WriteString("┌")
			case i == open-1:
				b.WriteString("╶")
			case up[i] && down[i]:
				b.WriteString("┼")
			case up[i]:
				b.WriteString("┴")
			case down[i]:
				b.WriteString("┬")
			default:
				b.WriteString(boxLine)
			}
		}
		b.WriteString(right)
		return b.String()
	}
	var out []string
	var prev *Row
	for i := range g.Rows {
		r := &g.Rows[i]
		switch {
		case prev == nil:
			out = append(out, rule("╭", "╮", nil, r, 0, ""))
		case prev.Header || across(*prev, s.lastCol) != across(*r, s.lastCol):
			// A rule under a header, and where the box goes from a row
			// across it to columns or back, so its lines join: whole.
			out = append(out, rule("├", "┤", prev, r, 0, ""))
		case r.Break && (len(s.cols) == 0 || r.Open < len(s.cols)):
			// A rule at a break, its open columns left blank; with all of
			// them open, none.
			out = append(out, rule("├", "┤", prev, r, max(0, r.Open), g.gapGuide(i)))
		}
		out = append(out, g.boxRow(i, s, inner)...)
		prev = r
	}
	if prev != nil {
		out = append(out, rule("╰", "╯", prev, nil, 0, ""))
	}
	return out
}

// boxRow is row at of a box: its cells padded to their columns between
// the box's lines, then what runs to the side, a tree's guides before
// its first cell.
func (g *Grid) boxRow(at int, s boxShape, inner int) []string {
	r := g.Rows[at]
	pad := strings.Repeat(" ", boxPadding)
	paint := func(line string) string {
		if r.Paint != nil && line != "" {
			return r.Paint(line)
		}
		return line
	}
	// first paints line j of what starts at the row's origin, its guide
	// before the paint, in most columns (the first column's, or the box's
	// inside for what runs from it to the side).
	first := func(line string, j, most int) string {
		guide, rest := guided(line, g.rowGuide(at, j), most)
		return guide + paint(rest)
	}
	l := g.cells(r, s, g.boxRoom)
	height := max(1, len(l.run))
	for _, lines := range l.cols {
		height = max(height, len(lines))
	}
	upTo := len(s.cols)
	if l.run != nil {
		upTo = l.runsIn
	}
	lined := s.lines(&r)
	content := inner - 2*boxPadding
	out := make([]string, height)
lines:
	for j := range out {
		var b strings.Builder
		b.WriteString(boxSide + pad)
		used := 0
		for i := range upTo {
			line := ""
			if j < len(l.cols[i]) {
				line = l.cols[i][j]
			}
			if i == 0 && s.cols[0] == 0 {
				line = first(line, j, s.widths[0])
			} else {
				line = paint(line)
			}
			block := 0
			if k := s.cols[i]; k < len(r.Cells) && r.Cells[k].Grid != nil {
				block = min(linesWidth(l.cols[i]), s.widths[i])
			}
			if i == len(s.cols)-1 && l.run == nil && Width(line) > s.widths[i] {
				// Past the box's MaxWidth: the line runs out past the side.
				b.WriteString(line)
				out[j] = b.String()
				continue lines
			}
			line = alignLine(line, s.widths[i], s.w.col(s.cols[i]).align == Right, block)
			b.WriteString(line)
			used += s.widths[i]
			if i < len(s.cols)-1 || l.run != nil {
				// The column's line where the row has one (lines), else its
				// room in spaces, so what follows starts at its column.
				side := strings.Repeat(" ", sideWidth)
				if i < len(lined) && lined[i] {
					side = boxSide
				}
				b.WriteString(pad + side + pad)
				used += 2*boxPadding + sideWidth
			}
		}
		if l.run != nil {
			line := ""
			if j < len(l.run) {
				line = l.run[j]
			}
			if upTo == 0 && runsFrom(r, s.lastCol) == 0 {
				line = first(line, j, content)
			} else {
				line = paint(line)
			}
			if Width(line) > content-used {
				// Past the box's MaxWidth: the line runs out past the side.
				b.WriteString(line)
				out[j] = b.String()
				continue
			}
			b.WriteString(Pad(line, content-used))
		} else if upTo == 0 {
			b.WriteString(strings.Repeat(" ", content))
		}
		b.WriteString(pad + boxSide)
		out[j] = b.String()
	}
	return out
}
