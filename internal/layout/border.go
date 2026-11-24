package layout

import "strings"

// Border is how a grid is drawn.
type Border int

const (
	// NoBorder lays the grid out in columns of spaces.
	NoBorder Border = iota
	// Box draws it in a box of light lines with rounded corners: a line
	// between columns, under a header row and at each row's Break.
	Box
)

// The box's pieces: a cell sits between "│ " and " │".
const (
	boxSide    = "│"
	boxLine    = "─"
	boxPadding = 1
)

// boxColumns are the columns a box draws, its rows' widest count of
// cells but for its span rows, and the widths it draws them at.
func (g *Grid) boxColumns(room int) (w *Widths, n int, widths []int) {
	for _, r := range g.Rows {
		if w == nil {
			w = g.widths(r)
		}
		if !spanRow(r) {
			n = max(n, len(r.Cells))
		}
	}
	if w == nil {
		w = g.widths(Row{})
	}
	widths = make([]int, n)
	for k := range n {
		c := w.col(k)
		widths[k] = max(w.size(k), c.fitted)
		if k == n-1 && room >= 0 && c.fitted > 0 {
			widths[k] = max(c.min, c.fitted)
		}
	}
	return w, n, widths
}

// spanRow says whether every cell of r that holds anything spans: the
// row runs across the box, with no column lines.
func spanRow(r Row) bool {
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

// fitBorders measures what a box's cells take once fitted: every cell of
// a row that is no span row, spans too, and its last cell fitted to the
// room inside the box, into its column, so boxes sharing columns draw
// them at one width.
func (g *Grid) fitBorders() {
	room := -1
	if g.Width > 0 {
		room = g.Width
	}
	w, n, _ := g.boxColumns(-1)
	for _, r := range g.Rows {
		if spanRow(r) {
			continue
		}
		start := 1 + boxPadding
		for k := 0; k < n-1; k++ {
			start += w.size(k) + 2*boxPadding + 1
		}
		for k, c := range r.Cells {
			col := w.col(k)
			switch {
			case k == len(r.Cells)-1 && k == n-1:
				lines := g.fitCell(c, start+indentOf(r, k), insideRoom(room))
				if c.Grid != nil {
					lines = cellLines(c, insideRoom(room), start, g)
				}
				col.fitted = max(col.fitted, indentOf(r, k)+linesWidth(lines))
			case c.Grid != nil:
				col.fitted = max(col.fitted, indentOf(r, k)+linesWidth(c.Grid.render(-1)))
			default:
				col.fitted = max(col.fitted, cellWidth(c, indentOf(r, k)))
			}
		}
	}
}

// insideRoom is the room a box's lines leave its cells, its right side
// taken off (cells' starts count its left).
func insideRoom(room int) int {
	if room < 0 {
		return -1
	}
	return room - boxPadding - 1
}

// renderBox draws g in a box.
func (g *Grid) renderBox(room int) []string {
	_, n, widths := g.boxColumns(room)
	inner := 0
	for _, cw := range widths {
		inner += cw + 2*boxPadding
	}
	inner += max(0, n-1)
	// A span row wider than the columns widens the last.
	spans := 0
	for _, r := range g.Rows {
		if spanRow(r) {
			spans = max(spans, Width(spanText(r)))
		}
	}
	if extra := spans + 2*boxPadding - inner; extra > 0 && n > 0 {
		widths[n-1] += extra
		inner += extra
	}
	if n == 0 {
		inner = spans + 2*boxPadding
	}
	rule := func(left, mid, right string, above, below bool) string {
		if n == 0 || (!above && !below) {
			return left + strings.Repeat(boxLine, inner) + right
		}
		junction := mid
		switch {
		case above && !below:
			junction = "┴"
		case below && !above:
			junction = "┬"
		}
		parts := make([]string, n)
		for k, cw := range widths {
			parts[k] = strings.Repeat(boxLine, cw+2*boxPadding)
		}
		return left + strings.Join(parts, junction) + right
	}
	var out []string
	var prev *Row
	for i := range g.Rows {
		r := g.Rows[i]
		cols := !spanRow(r)
		switch {
		case prev == nil:
			out = append(out, rule("╭", "┬", "╮", false, cols))
		case r.Break || prev.Header:
			out = append(out, rule("├", "┼", "┤", !spanRow(*prev), cols))
		}
		out = append(out, g.boxRow(r, widths, inner, room)...)
		prev = &g.Rows[i]
	}
	if prev != nil {
		out = append(out, rule("╰", "┴", "╯", !spanRow(*prev), false))
	}
	return out
}

// spanText is a span row's cells as one line, at their gaps.
func spanText(r Row) string {
	var parts []string
	for _, c := range r.Cells {
		if !empty(c) {
			parts = append(parts, c.Prefix+c.Text)
		}
	}
	return strings.Repeat(" ", r.Indent) + strings.Join(parts, strings.Repeat(" ", DefaultGap))
}

// boxRow is one row of a box: its cells padded to their columns between
// the box's lines, or a span row across them.
func (g *Grid) boxRow(r Row, widths []int, inner, room int) []string {
	pad := strings.Repeat(" ", boxPadding)
	paint := func(s string) string {
		if r.Paint != nil && s != "" {
			return r.Paint(s)
		}
		return s
	}
	if spanRow(r) {
		var line string
		for _, c := range r.Cells {
			if !empty(c) {
				line = finish(c, []string{c.Text})[0]
				break
			}
		}
		if text := spanText(r); len(r.Cells) > 1 || r.Indent > 0 {
			line = text
		}
		return []string{boxSide + pad + Pad(paint(line), inner-2*boxPadding) + pad + boxSide}
	}
	cells := make([][]string, len(widths))
	start := 1 + boxPadding
	height := 1
	for k := range widths {
		if k >= len(r.Cells) {
			continue
		}
		c := r.Cells[k]
		indent := strings.Repeat(" ", indentOf(r, k))
		var lines []string
		switch {
		case k == len(r.Cells)-1:
			lines = g.fitCell(c, start+indentOf(r, k), insideRoom(room))
		default:
			lines = cellLines(c, insideRoom(room), start+indentOf(r, k), g)
		}
		for j := range lines {
			lines[j] = indent + lines[j]
		}
		cells[k] = lines
		height = max(height, len(lines))
		start += widths[k] + 2*boxPadding + 1
	}
	out := make([]string, height)
	for i := range out {
		parts := make([]string, len(widths))
		for k, cw := range widths {
			s := ""
			if i < len(cells[k]) {
				s = paint(cells[k][i])
			}
			if g.widths(r).col(k).align == Right {
				parts[k] = padLeft(s, cw)
			} else {
				parts[k] = Pad(s, cw)
			}
		}
		out[i] = boxSide + pad + strings.Join(parts, pad+boxSide+pad) + pad + boxSide
	}
	return out
}
