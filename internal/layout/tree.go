package layout

import "strings"

// A tree's guides (Row.Level), each a level wide.
const (
	guideWidth = 3
	guideFork  = "├─ "
	guideLast  = "└─ "
	guideDown  = "│  "
	guideNone  = "   "
)

// runsDown says whether a guide at level runs down from row p on: a node
// at level comes before any at a lower one (its parent's next sibling, a
// root), continuing rows passed by.
func (g *Grid) runsDown(p, level int) bool {
	for _, r := range g.Rows[min(p, len(g.Rows)):] {
		if r.Continues {
			continue
		}
		if r.Level < level {
			return false
		}
		if r.Level == level {
			return true
		}
	}
	return false
}

// depth is the deepest level of g's rows.
func (g *Grid) depth() int {
	n := 0
	for _, r := range g.Rows {
		n = max(n, r.Level)
	}
	return n
}

// guides are the guides at the start of a line, at indent: a guide at
// each level up to upTo that runs down from row p on, branch's own (a
// node's first line) a fork, or its last child's corner; "" when none is
// drawn.
func (g *Grid) guides(p, indent, upTo, branch int) string {
	var b strings.Builder
	for l := 1; l <= upTo; l++ {
		down := g.runsDown(p, l)
		switch {
		case l == branch && down:
			b.WriteString(guideFork)
		case l == branch:
			b.WriteString(guideLast)
		case down:
			b.WriteString(guideDown)
		default:
			b.WriteString(guideNone)
		}
	}
	s := strings.TrimRight(b.String(), " ")
	if s == "" {
		return ""
	}
	return strings.Repeat(" ", indent) + s
}

// rowGuide is what line j of row i starts with: a node's first line its
// branch, the guides above it running by; any other line, a continuing
// row's too, the guides running on below it, the line to its first child
// included.
func (g *Grid) rowGuide(i, j int) string {
	r := g.Rows[i]
	if j == 0 && !r.Continues {
		return g.guides(i+1, r.Indent, r.Level, r.Level)
	}
	return g.guides(i+1, r.Indent, g.depth(), 0)
}

// gapGuide is what a line before row i starts with (a blank line, a box's
// rule): the guides running down to it or past it.
func (g *Grid) gapGuide(i int) string {
	return g.guides(i, g.Rows[i].Indent, g.depth(), 0)
}

// guided splits line where a guide goes: the guide, and what follows its
// columns, when the line holds nothing there and the guide fits in most
// columns (its first column's width); else none and the line whole, so a
// guide never covers a cell nor widens its column.
func guided(line, guide string, most int) (string, string) {
	n := Width(guide)
	if n == 0 || n > most {
		return "", line
	}
	head := line[:min(n, len(line))]
	if strings.Trim(head, " ") != "" {
		return "", line
	}
	return guide, line[len(head):]
}

// firstWidth is how wide row r's first column is drawn without borders:
// its column's width, or its own when it is wider (or in none).
func (g *Grid) firstWidth(r Row) int {
	return max(g.widths(r).size(0), indentOf(r, 0))
}
