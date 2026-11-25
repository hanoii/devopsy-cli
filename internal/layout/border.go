package layout

import "strings"

// Border is how a grid is drawn.
type Border int

const (
	// NoBorder lays the grid out in columns of spaces.
	NoBorder Border = iota
	// Box draws it in a box of light lines with rounded corners: a line
	// between columns, under a header row, at each row's Break, and where
	// the box goes from a span row to columns or back.
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

// SpanRow says whether every cell of r that holds anything spans, and one
// does: the row runs free of the columns (a heading), across a box.
func SpanRow(r Row) bool {
	any := false
	for _, c := range r.Cells {
		if empty(c) {
			continue
		}
		if !c.Span {
			return false
		}
		any = true
	}
	return any
}

// spanAt is the index of r's first span cell, len(r.Cells) for none: in a
// box the row's columns stop there, and what follows runs to the side.
func spanAt(r Row) int {
	for k, c := range r.Cells {
		if c.Span {
			return k
		}
	}
	return len(r.Cells)
}

// boxShape is what a box draws: its columns' Widths, the cells it draws as
// columns (by their index in a row), and how wide it draws each.
type boxShape struct {
	w      *Widths
	cols   []int
	widths []int
}

// shapeOf is g's columns as a box draws them: every column a row holds
// anything in before its span cell, a column empty in all of them left
// out as without borders; each as wide as its cells take once laid out as
// drawn (fitBoxes), a fixed one at least its width.
func (g *Grid) shapeOf() boxShape {
	var s boxShape
	n := 0
	for _, r := range g.Rows {
		if s.w == nil {
			s.w = g.widths(r)
		}
		if !SpanRow(r) {
			n = max(n, spanAt(r))
		}
	}
	if s.w == nil {
		s.w = g.widths(Row{})
	}
	for k := range n {
		c := s.w.col(k)
		held := s.w.shown(k) || c.fitted > 0
		for _, r := range g.Rows {
			if !SpanRow(r) && k < spanAt(r) && !empty(r.Cells[k]) {
				held = true
			}
		}
		if !held {
			continue
		}
		width := s.w.size(k)
		if c.fitted > 0 {
			width = max(c.min, c.fixed, c.fitted)
		}
		s.cols = append(s.cols, k)
		s.widths = append(s.widths, width)
	}
	return s
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

// insideRoom is the room a box's lines leave its cells, its right side
// taken off (cells' starts count its left).
func insideRoom(room int) int {
	if room < 0 {
		return -1
	}
	return room - boxPadding - sideWidth
}

// column is the box's column a row's span cell at k starts in: the first
// drawn at k or after, len(s.cols) when none is.
func (s boxShape) column(k int) int {
	for i, c := range s.cols {
		if c >= k {
			return i
		}
	}
	return len(s.cols)
}

// boxLaid is a row of a box laid out: one list of lines per column it
// draws, then the lines of what runs from its span cell to the side, at
// the column it starts in (nil for none).
type boxLaid struct {
	cols   [][]string
	span   []string
	spanIn int
}

// cells lays out a row of a box: each column's cell after the box's side
// and the columns before it, a row's last cell fitted to what the room
// leaves it inside the box (the columns after it taken off), and its span
// cell on, as without borders, from the column it starts in to the side.
// Measuring a box (fitBoxes) and drawing it (boxRow) lay a row out here
// alike.
func (g *Grid) cells(r Row, s boxShape, room int) boxLaid {
	inside := insideRoom(room)
	last := lastShown(r)
	sp := spanAt(r)
	if SpanRow(r) {
		// A span row runs from its first cell that holds anything.
		sp = 0
		for sp < len(r.Cells) && empty(r.Cells[sp]) {
			sp++
		}
	}
	l := boxLaid{cols: make([][]string, len(s.cols)), spanIn: s.column(sp)}
	for i, k := range s.cols {
		if k >= sp || k >= len(r.Cells) {
			continue
		}
		c := r.Cells[k]
		indent := indentOf(r, k)
		var lines []string
		if k == last {
			room := inside
			if room >= 0 {
				for j := i + 1; j < len(s.cols); j++ {
					room -= s.widths[j] + 2*boxPadding + sideWidth
				}
			}
			lines = g.fitCell(c, s.start(i)+indent, room)
		} else {
			lines = cellLines(c, inside, s.start(i)+indent)
		}
		for j := range lines {
			lines[j] = strings.Repeat(" ", indent) + lines[j]
		}
		l.cols[i] = lines
	}
	if sp < len(r.Cells) {
		rest := Row{Cells: r.Cells[sp:]}
		if sp == 0 {
			rest.Indent = r.Indent
		}
		room := inside
		if room >= 0 {
			room = max(0, inside-s.start(l.spanIn))
		}
		l.span = g.renderRow(rest, room)
	}
	return l
}

// fitBoxes measures what boxes' cells take laid out as they are drawn
// (cells), into their columns: boxes sharing columns draw them at one
// width, fitted to the tightest room among them; a row's last cell fits
// what the columns around it leave, so the layout is taken again until no
// column's width moves (a few passes at most); then each span part widens
// the last column it runs across when the columns leave it too little.
func fitBoxes(boxes []*Grid) {
	room := map[*Widths]int{}
	for _, g := range boxes {
		w := g.shapeOf().w
		if r, ok := room[w]; !ok || (g.fit >= 0 && (r < 0 || g.fit < r)) {
			room[w] = g.fit
		}
	}
	for range 6 {
		fitted := map[*column]int{}
		for _, g := range boxes {
			s := g.shapeOf()
			for _, r := range g.Rows {
				l := g.cells(r, s, room[s.w])
				for i, k := range s.cols {
					col := s.w.col(k)
					fitted[col] = max(fitted[col], linesWidth(l.cols[i]))
				}
			}
		}
		moved := false
		for _, g := range boxes {
			for _, k := range g.shapeOf().cols {
				col := g.shapeOf().w.col(k)
				if col.fitted != fitted[col] {
					col.fitted, moved = fitted[col], true
				}
			}
		}
		if !moved {
			break
		}
	}
	for _, g := range boxes {
		for _, r := range g.Rows {
			s := g.shapeOf()
			if len(s.cols) == 0 {
				continue
			}
			l := g.cells(r, s, room[s.w])
			if l.span == nil {
				continue
			}
			need := s.start(l.spanIn) - sideWidth - boxPadding + linesWidth(l.span)
			if extra := need - (s.inner() - 2*boxPadding); extra > 0 {
				col := s.w.col(s.cols[len(s.cols)-1])
				col.fitted = max(col.fitted, s.widths[len(s.widths)-1]+extra)
			}
		}
	}
}

// lines says, for a row of a box, whether a column line stands after each
// of its columns but the last: none in a span row, and in a row with a
// span cell after cells of its own, up to that cell's column and none
// across it.
func (s boxShape) lines(r *Row) []bool {
	out := make([]bool, max(0, len(s.cols)-1))
	if r == nil || SpanRow(*r) {
		return out
	}
	in := len(s.cols)
	if sp := spanAt(*r); sp < len(r.Cells) {
		in = s.column(sp)
	}
	for i := range out {
		out[i] = i+1 <= in
	}
	return out
}

// renderBox draws g in a box.
func (g *Grid) renderBox(room int) []string {
	s := g.shapeOf()
	inner := s.inner()
	if len(s.cols) == 0 {
		for _, r := range g.Rows {
			inner = max(inner, linesWidth(g.cells(r, s, room).span)+2*boxPadding)
		}
	}
	// A rule: a junction at each column line, joining those of the row
	// above and below it.
	rule := func(left, right string, above, below *Row) string {
		if len(s.cols) == 0 {
			return left + strings.Repeat(boxLine, inner) + right
		}
		up, down := s.lines(above), s.lines(below)
		var b strings.Builder
		b.WriteString(left)
		for i, w := range s.widths {
			b.WriteString(strings.Repeat(boxLine, w+2*boxPadding))
			if i == len(s.widths)-1 {
				break
			}
			switch {
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
			out = append(out, rule("╭", "╮", nil, r))
		case r.Break || prev.Header || SpanRow(*prev) != SpanRow(*r):
			// A rule at a break, under a header, and where the box goes
			// from a span row to columns or back, so its lines join.
			out = append(out, rule("├", "┤", prev, r))
		}
		out = append(out, g.boxRow(*r, s, inner, room)...)
		prev = r
	}
	if prev != nil {
		out = append(out, rule("╰", "╯", prev, nil))
	}
	return out
}

// boxRow is one row of a box: its cells padded to their columns between
// the box's lines, then what runs from its span cell to the side.
func (g *Grid) boxRow(r Row, s boxShape, inner, room int) []string {
	pad := strings.Repeat(" ", boxPadding)
	paint := func(line string) string {
		if r.Paint != nil && line != "" {
			return r.Paint(line)
		}
		return line
	}
	l := g.cells(r, s, room)
	height := max(1, len(l.span))
	for _, lines := range l.cols {
		height = max(height, len(lines))
	}
	upTo := len(s.cols)
	if l.span != nil {
		upTo = l.spanIn
	}
	lined := s.lines(&r)
	out := make([]string, height)
	for j := range out {
		var b strings.Builder
		b.WriteString(boxSide + pad)
		used := 0
		for i := range upTo {
			line := ""
			if j < len(l.cols[i]) {
				line = paint(l.cols[i][j])
			}
			if s.w.col(s.cols[i]).align == Right {
				line = padLeft(line, s.widths[i])
			} else {
				line = Pad(line, s.widths[i])
			}
			b.WriteString(line)
			used += s.widths[i]
			if i < len(s.cols)-1 || l.span != nil {
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
		content := inner - 2*boxPadding
		if l.span != nil {
			line := ""
			if j < len(l.span) {
				line = paint(l.span[j])
			}
			b.WriteString(Pad(line, content-used))
		} else if len(s.cols) == 0 {
			b.WriteString(strings.Repeat(" ", content))
		}
		b.WriteString(pad + boxSide)
		out[j] = b.String()
	}
	return out
}
