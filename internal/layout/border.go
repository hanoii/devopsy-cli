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

// boxShape is what a box draws: its columns' Widths, the cells it draws as
// columns (by their index in a row), and how wide it draws each.
type boxShape struct {
	w      *Widths
	cols   []int
	widths []int
}

// shapeOf is g's columns as a box draws them: every column a row that is
// no span row holds anything in, a column empty in all of them left out
// as without borders.
func (g *Grid) shapeOf() boxShape {
	var s boxShape
	n := 0
	for _, r := range g.Rows {
		if s.w == nil {
			s.w = g.widths(r)
		}
		if SpanRow(r) {
			continue
		}
		n = max(n, len(r.Cells))
	}
	if s.w == nil {
		s.w = g.widths(Row{})
	}
	for k := range n {
		held := s.w.shown(k) || s.w.col(k).fitted > 0
		for _, r := range g.Rows {
			if !SpanRow(r) && k < len(r.Cells) && !empty(r.Cells[k]) {
				held = true
			}
		}
		if held {
			// Once measured as drawn (fitBoxes), a column is as wide as its
			// cells laid out take, a wrapped one's narrower than its text.
			c := s.w.col(k)
			width := s.w.size(k)
			if c.fitted > 0 && c.fixed == 0 {
				width = max(c.min, c.fitted)
			}
			s.cols = append(s.cols, k)
			s.widths = append(s.widths, width)
		}
	}
	return s
}

// inner is the width between a box's sides.
func (s boxShape) inner() int {
	n := max(0, len(s.cols)-1)
	for _, w := range s.widths {
		n += w + 2*boxPadding
	}
	return n
}

// cells lays out the cells of r a box draws, one list of lines per column,
// each column's cell starting after the box's side and the columns before
// it, a row's last cell fitted to what the room leaves it inside the box.
// Measuring a box (fitBoxes) and drawing it (boxRow) lay a row out here
// alike.
func (g *Grid) cells(r Row, s boxShape, room int) [][]string {
	last := len(r.Cells) - 1
	for last > 0 && empty(r.Cells[last]) {
		last--
	}
	inside := -1
	if room >= 0 {
		inside = room - boxPadding - sideWidth
	}
	out := make([][]string, len(s.cols))
	start := sideWidth + boxPadding
	for i, k := range s.cols {
		if k < len(r.Cells) {
			c := r.Cells[k]
			indent := indentOf(r, k)
			var lines []string
			if k == last {
				// A row's last cell before the box's last column leaves the
				// columns after it their room.
				room := inside
				if room >= 0 {
					for j := i + 1; j < len(s.cols); j++ {
						room -= s.widths[j] + 2*boxPadding + sideWidth
					}
				}
				lines = g.fitCell(c, start+indent, room)
			} else {
				lines = cellLines(c, inside, start+indent)
			}
			for j := range lines {
				lines[j] = strings.Repeat(" ", indent) + lines[j]
			}
			out[i] = lines
		}
		start += s.widths[i] + 2*boxPadding + sideWidth
	}
	return out
}

// fitBoxes measures what boxes' cells take once laid out as they are
// drawn, into their columns, so boxes sharing columns draw them at one
// width: first every row's cells but its last, then its last fitted to
// the room they leave it, then each span row, which widens the last column
// it runs across.
func fitBoxes(boxes []*Grid) {
	for _, g := range boxes {
		s := g.shapeOf()
		for _, r := range g.Rows {
			if SpanRow(r) {
				continue
			}
			lines := g.cells(r, s, -1)
			last := len(r.Cells) - 1
			for i, k := range s.cols {
				if k < last {
					col := s.w.col(k)
					col.fitted = max(col.fitted, linesWidth(lines[i]))
				}
			}
		}
	}
	for _, g := range boxes {
		s := g.shapeOf()
		for _, r := range g.Rows {
			if SpanRow(r) {
				continue
			}
			lines := g.cells(r, s, g.fit)
			for i, k := range s.cols {
				col := s.w.col(k)
				col.fitted = max(col.fitted, linesWidth(lines[i]))
			}
		}
	}
	for _, g := range boxes {
		s := g.shapeOf()
		if len(s.cols) == 0 {
			continue
		}
		for _, r := range g.Rows {
			if !SpanRow(r) {
				continue
			}
			if extra := linesWidth(g.renderRow(r, -1)) + 2*boxPadding - s.inner(); extra > 0 {
				col := s.w.col(s.cols[len(s.cols)-1])
				col.fitted = max(col.fitted, s.widths[len(s.widths)-1]+extra)
				s = g.shapeOf()
			}
		}
	}
}

// renderBox draws g in a box.
func (g *Grid) renderBox(room int) []string {
	s := g.shapeOf()
	inner := s.inner()
	if len(s.cols) == 0 {
		for _, r := range g.Rows {
			inner = max(inner, linesWidth(g.renderRow(r, -1))+2*boxPadding)
		}
	}
	rule := func(left, mid, right string, above, below bool) string {
		if len(s.cols) == 0 || (!above && !below) {
			return left + strings.Repeat(boxLine, inner) + right
		}
		junction := mid
		switch {
		case above && !below:
			junction = "┴"
		case below && !above:
			junction = "┬"
		}
		parts := make([]string, len(s.widths))
		for i, w := range s.widths {
			parts[i] = strings.Repeat(boxLine, w+2*boxPadding)
		}
		return left + strings.Join(parts, junction) + right
	}
	var out []string
	var prev *Row
	for i := range g.Rows {
		r := g.Rows[i]
		cols := !SpanRow(r)
		switch {
		case prev == nil:
			out = append(out, rule("╭", "┬", "╮", false, cols))
		case r.Break || prev.Header || !SpanRow(*prev) != cols:
			// A rule at a break, under a header, and where the box goes
			// from a span row to columns or back, so its lines join.
			out = append(out, rule("├", "┼", "┤", !SpanRow(*prev), cols))
		}
		out = append(out, g.boxRow(r, s, inner, room)...)
		prev = &g.Rows[i]
	}
	if prev != nil {
		out = append(out, rule("╰", "┴", "╯", !SpanRow(*prev), false))
	}
	return out
}

// boxRow is one row of a box: its cells padded to their columns between
// the box's lines, or a span row across them, laid out as without
// borders.
func (g *Grid) boxRow(r Row, s boxShape, inner, room int) []string {
	pad := strings.Repeat(" ", boxPadding)
	paint := func(line string) string {
		if r.Paint != nil && line != "" {
			return r.Paint(line)
		}
		return line
	}
	if SpanRow(r) {
		var out []string
		for _, line := range g.renderRow(r, -1) {
			out = append(out, boxSide+pad+Pad(paint(line), inner-2*boxPadding)+pad+boxSide)
		}
		return out
	}
	cells := g.cells(r, s, room)
	height := 1
	for _, lines := range cells {
		height = max(height, len(lines))
	}
	out := make([]string, height)
	for j := range out {
		parts := make([]string, len(s.cols))
		for i, k := range s.cols {
			line := ""
			if j < len(cells[i]) {
				line = paint(cells[i][j])
			}
			if s.w.col(k).align == Right {
				parts[i] = padLeft(line, s.widths[i])
			} else {
				parts[i] = Pad(line, s.widths[i])
			}
		}
		out[j] = boxSide + pad + strings.Join(parts, pad+boxSide+pad) + pad + boxSide
	}
	return out
}
